// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// These tests pin ServeWithReady: it blocks like Serve, and it reports once,
// through ready, that every listener is up and every transport's client has
// been rebuilt to source from its listener. A caller that sends a request the
// moment ready runs therefore sends it from the listening socket.

// serveReadyBound bounds every wait here, so a regression fails instead of
// hanging the suite.
const serveReadyBound = 5 * time.Second

// unusedTCPPort is unusedUDPPort for TCP: a port the OS just handed out and
// then released, free again when the function returns.
func unusedTCPPort(t *testing.T) int {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a TCP port: %v", err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	if err := l.Close(); err != nil {
		t.Fatalf("releasing TCP port %d: %v", port, err)
	}
	return port
}

// portsHeld reports which of the two ports a fresh bind is refused on, which is
// the kernel's own answer to whether a listener holds them.
func portsHeld(udpPort, tcpPort int) (udpHeld, tcpHeld bool) {
	if c, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", udpPort)); err != nil {
		udpHeld = true
	} else {
		_ = c.Close()
	}
	if l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", tcpPort)); err != nil {
		tcpHeld = true
	} else {
		_ = l.Close()
	}
	return udpHeld, tcpHeld
}

// readyObservation is what the ready callback saw when it ran.
type readyObservation struct {
	client        *sipgo.Client
	udpHeld       bool
	tcpHeld       bool
	serveReturned bool
}

func TestServeWithReadyCallsReadyOnceAllListenersUp(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	udpPort := unusedUDPPort(t)
	tcpPort := unusedTCPPort(t)
	ua := newTestUA(t)
	dg := NewDiago(ua,
		WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: udpPort}),
		WithTransport(Transport{Transport: "tcp", BindHost: "127.0.0.1", BindPort: tcpPort}),
	)

	loadClient := dg.getClient(&dg.transports[0])
	if loadClient == nil {
		t.Fatal("udp transport has no client at load")
	}

	served := make(chan struct{})
	var readyCalls atomic.Int64
	observed := make(chan readyObservation, 1)
	ready := func() {
		if readyCalls.Add(1) != 1 {
			return
		}
		obs := readyObservation{client: dg.getClient(&dg.transports[0])}
		obs.udpHeld, obs.tcpHeld = portsHeld(udpPort, tcpPort)
		select {
		case <-served:
			obs.serveReturned = true
		default:
		}
		observed <- obs
	}

	var serveErr error
	go func() {
		defer close(served)
		serveErr = dg.ServeWithReady(ctx, nil, ready)
	}()

	var obs readyObservation
	select {
	case obs = <-observed:
	case <-served:
		t.Fatalf("ServeWithReady returned (%v) and ready not called", serveErr)
	case <-time.After(serveReadyBound):
		cancel()
		<-served
		t.Fatalf("ready not called within %v of serving", serveReadyBound)
	}

	if obs.serveReturned {
		t.Error("ServeWithReady had already returned when ready ran; it must block like Serve")
	}
	if !obs.udpHeld || !obs.tcpHeld {
		t.Errorf("ready ran with udp/%d held=%v and tcp/%d held=%v; it must wait for every listener",
			udpPort, obs.udpHeld, tcpPort, obs.tcpHeld)
	}
	if obs.client == loadClient {
		t.Errorf("ready ran before the udp transport's client was rebuilt on BindPort %d; "+
			"a request sent from ready would leave from another port", udpPort)
	}

	select {
	case <-served:
		t.Fatalf("ServeWithReady returned (%v) while its context was still live", serveErr)
	default:
	}

	cancel()
	select {
	case <-served:
	case <-time.After(serveReadyBound):
		t.Fatalf("ServeWithReady did not return within %v of its context ending", serveReadyBound)
	}

	if n := readyCalls.Load(); n != 1 {
		t.Fatalf("ready called %d times, want exactly once", n)
	}
}

func TestServeWithReadyReturnsListenErrorWithoutReady(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	port := squatUDPPort(t)
	ua := newTestUA(t)
	dg := NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: port}))

	var readyCalls atomic.Int64
	served := make(chan error, 1)
	go func() {
		served <- dg.ServeWithReady(ctx, nil, func() { readyCalls.Add(1) })
	}()

	select {
	case err := <-served:
		if err == nil {
			t.Fatalf("ServeWithReady = nil although udp/%d is held by another socket", port)
		}
	case <-time.After(serveReadyBound):
		cancel()
		<-served
		t.Fatalf("ServeWithReady did not return within %v although its listener cannot bind", serveReadyBound)
	}

	if n := readyCalls.Load(); n != 0 {
		t.Fatalf("ready called %d times although the listener never came up", n)
	}
}

// TestServeEphemeralPortWhileRequestsReadTransports pins that a listener bound
// to an ephemeral port records the port on its transport without racing the
// request paths that read the transports, such as an outbound call made while
// Serve starts. The race only shows under -race. Once ready has run, a new
// dialog's Contact carries the port the listener bound.
func TestServeEphemeralPortWhileRequestsReadTransports(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ua := newTestUA(t)
	dg := NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	// Nobody listens on the discard port; the dialogs are never invited.
	unreachable := sip.Uri{User: "nobody", Host: "127.0.0.1", Port: 9}

	const readerCount = 4
	iterations := make([]atomic.Int64, readerCount)
	stop := make(chan struct{})
	readerErrs := make(chan error, readerCount)
	for readerIndex := range readerCount {
		go func() {
			readerErrs <- func() error {
				for {
					select {
					case <-stop:
						return nil
					default:
					}
					d, err := dg.NewDialog(unreachable, NewDialogOptions{})
					if err != nil {
						return err
					}
					_ = d.Close()
					iterations[readerIndex].Add(1)
				}
			}()
		}()
	}
	// stopReaders stops the readers and waits for them with a bound.
	stopReaders := func() {
		close(stop)
		for range readerCount {
			select {
			case err := <-readerErrs:
				if err != nil {
					t.Errorf("NewDialog while serving started: %v", err)
				}
			case <-time.After(serveReadyBound):
				t.Fatalf("the readers did not stop within %v", serveReadyBound)
			}
		}
	}
	// waitForIterations blocks until every reader has read past its floor.
	waitForIterations := func(floors []int64) bool {
		deadline := time.Now().Add(serveReadyBound)
		for readerIndex := range readerCount {
			for iterations[readerIndex].Load() <= floors[readerIndex] {
				if time.Now().After(deadline) {
					return false
				}
				time.Sleep(time.Millisecond)
			}
		}
		return true
	}

	if !waitForIterations(make([]int64, readerCount)) {
		stopReaders()
		t.Fatalf("the readers did not start within %v", serveReadyBound)
	}

	ready := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- dg.ServeWithReady(ctx, nil, func() { close(ready) })
	}()
	select {
	case <-ready:
	case err := <-served:
		stopReaders()
		t.Fatalf("ServeWithReady returned (%v) and ready not called", err)
	case <-time.After(serveReadyBound):
		stopReaders()
		t.Fatalf("ready not called within %v of serving", serveReadyBound)
	}
	readyFloors := make([]int64, readerCount)
	for readerIndex := range readerCount {
		readyFloors[readerIndex] = iterations[readerIndex].Load()
	}
	readersAdvanced := waitForIterations(readyFloors)
	stopReaders()
	if !readersAdvanced {
		t.Fatalf("the readers did not advance after ready within %v", serveReadyBound)
	}

	ports := ua.TransportLayer().ListenPorts("udp")
	if len(ports) != 1 {
		t.Fatalf("listening on udp ports %v, want one", ports)
	}
	d, err := dg.NewDialog(unreachable, NewDialogOptions{})
	if err != nil {
		t.Fatalf("NewDialog once serving: %v", err)
	}
	defer d.Close()
	if port := d.UA.ContactHDR.Address.Port; port != ports[0] {
		t.Fatalf("a new dialog's Contact port = %d, want the listener's %d", port, ports[0])
	}

	cancel()
	select {
	case <-served:
	case <-time.After(serveReadyBound):
		t.Fatalf("ServeWithReady did not return within %v of its context ending", serveReadyBound)
	}
}
