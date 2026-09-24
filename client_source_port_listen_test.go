// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// These tests pin that once a transport's listener is up, the transport's client
// sources requests from the port that listener holds.
//
// The client built when the transport loads may not pin the configured port,
// because nothing holds it yet (see client_source_port_wiring_test.go). The
// listener's ready callback is what proves the port is held, so that callback
// replaces the client with one that sources from it. Without that, a UA serving
// on its configured port sends INVITE and REGISTER from a random port: the Via
// carries one port, the Contact another, and the peer's responses and in-dialog
// requests land on a port nobody reads.
//
// The replacement happens on the listener goroutine while request paths may be
// reading the client, so it must be race-free. Run this file under -race.

// unusedUDPPort returns a port the OS just handed out and then released. Unlike
// squatUDPPort, which keeps its port held for the whole test, the port here is
// free again when the function returns, so a Diago can bind it as its fixed
// BindPort without colliding with a literal port another suite uses.
func unusedUDPPort(t *testing.T) int {
	t.Helper()

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a UDP port: %v", err)
	}
	port := conn.LocalAddr().(*net.UDPAddr).Port
	if err := conn.Close(); err != nil {
		t.Fatalf("releasing UDP port %d: %v", port, err)
	}
	return port
}

// TestIntegrationClientSourcesFromBindPortOnceServing is the regression: a UA
// that serves on a fixed port sends its INVITE from that port, so the Via, the
// source address the peer sees and the Contact all agree.
func TestIntegrationClientSourcesFromBindPortOnceServing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	peerPort := unusedUDPPort(t)
	peerUA := newTestUA(t)
	peer := NewDiago(peerUA, WithTransport(Transport{
		Transport: "udp",
		BindHost:  "127.0.0.1",
		BindPort:  peerPort,
	}))

	receivedInvites := make(chan *sip.Request, 1)
	err := peer.ServeBackground(ctx, func(d *DialogServerSession) {
		select {
		case receivedInvites <- d.InviteRequest:
		default:
		}
		_ = d.Respond(sip.StatusBusyHere, "Busy Here", nil)
		<-d.Context().Done()
	})
	if err != nil {
		t.Fatalf("peer ServeBackground: %v", err)
	}

	dialerBindPort := unusedUDPPort(t)
	dialerUA := newTestUA(t)
	dialer := NewDiago(dialerUA, WithTransport(Transport{
		Transport: "udp",
		BindHost:  "127.0.0.1",
		BindPort:  dialerBindPort,
	}))
	if err := dialer.ServeBackground(ctx, nil); err != nil {
		t.Fatalf("dialer ServeBackground: %v", err)
	}

	inviteCtx, inviteCancel := context.WithTimeout(ctx, 5*time.Second)
	defer inviteCancel()
	// The peer answers 486, so an error is the expected outcome, not a failure.
	_, _ = dialer.Invite(inviteCtx, sip.Uri{User: "busy", Host: "127.0.0.1", Port: peerPort}, InviteOptions{})

	var invite *sip.Request
	select {
	case invite = <-receivedInvites:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer never received the INVITE")
	}

	viaPort := invite.Via().Port
	source := invite.Source()
	_, sourcePort, err := sip.ParseAddr(source)
	if err != nil {
		t.Fatalf("parsing INVITE source %q: %v", source, err)
	}

	if viaPort != dialerBindPort {
		t.Fatalf("INVITE left from port %d (Via %d, source %s), want BindPort %d: "+
			"once the listener holds BindPort the client must source from it",
			viaPort, viaPort, source, dialerBindPort)
	}
	if sourcePort != dialerBindPort {
		t.Fatalf("INVITE source port = %d (source %s), want BindPort %d: the Via "+
			"claims BindPort but the datagram came from elsewhere", sourcePort, source, dialerBindPort)
	}
	if contactPort := invite.Contact().Address.Port; contactPort != dialerBindPort {
		t.Fatalf("INVITE Contact port = %d, want BindPort %d", contactPort, dialerBindPort)
	}
}

// TestIntegrationClientSwapDuringServeStartup pins that the ready callback's
// client replacement is race-free against request paths reading the client, and
// that the replacement happens for a fixed BindPort at all.
func TestIntegrationClientSwapDuringServeStartup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	bindPort := unusedUDPPort(t)
	ua := newTestUA(t)
	dg := NewDiago(ua, WithTransport(Transport{
		Transport: "udp",
		BindHost:  "127.0.0.1",
		BindPort:  bindPort,
	}))

	loadClient := dg.getClient(&dg.transports[0])
	if loadClient == nil {
		t.Fatal("transport has no client at load")
	}

	// Nobody listens on the discard port; the readers never send anything.
	unreachable := sip.Uri{User: "nobody", Host: "127.0.0.1", Port: 9}

	const readerCount = 4
	iterations := make([]atomic.Int64, readerCount)
	stopReaders := make(chan struct{})
	var readers sync.WaitGroup

	readClient := func(readerIndex int) {
		defer readers.Done()
		for {
			select {
			case <-stopReaders:
				return
			default:
			}

			dialog, err := dg.NewDialog(unreachable, NewDialogOptions{})
			if err != nil {
				t.Errorf("reader %d: NewDialog: %v", readerIndex, err)
			} else {
				_ = dialog.Close()
			}

			if _, err := dg.RegisterTransaction(ctx, unreachable, RegisterOptions{}); err != nil {
				t.Errorf("reader %d: RegisterTransaction: %v", readerIndex, err)
			}

			if dg.getClient(&dg.transports[0]) == nil {
				t.Errorf("reader %d: transport client is nil", readerIndex)
			}

			iterations[readerIndex].Add(1)
		}
	}

	readers.Add(readerCount)
	for readerIndex := range readerCount {
		go readClient(readerIndex)
	}

	// joinReaders stops the readers and waits for them with a bound, so a stuck
	// reader fails the test instead of hanging it.
	joinReaders := func() {
		close(stopReaders)
		joined := make(chan struct{})
		go func() {
			readers.Wait()
			close(joined)
		}()
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			t.Fatal("readers did not stop within 5s")
		}
	}

	// waitForIterations blocks until every reader has passed its floor.
	waitForIterations := func(floors []int64) bool {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			allPassed := true
			for readerIndex := range readerCount {
				if iterations[readerIndex].Load() <= floors[readerIndex] {
					allPassed = false
					break
				}
			}
			if allPassed {
				return true
			}
			time.Sleep(time.Millisecond)
		}
		return false
	}

	zeroFloors := make([]int64, readerCount)
	if !waitForIterations(zeroFloors) {
		joinReaders()
		t.Fatal("readers did not start within 5s")
	}

	serveErr := dg.ServeBackground(ctx, nil)

	servedFloors := make([]int64, readerCount)
	for readerIndex := range readerCount {
		servedFloors[readerIndex] = iterations[readerIndex].Load()
	}
	readersAdvanced := waitForIterations(servedFloors)
	joinReaders()

	if serveErr != nil {
		t.Fatalf("ServeBackground: %v", serveErr)
	}
	if !readersAdvanced {
		t.Fatal("readers did not advance after ServeBackground within 5s")
	}

	servingClient := dg.getClient(&dg.transports[0])
	if servingClient == loadClient {
		t.Fatalf("transport client not rebuilt after the listener came up on BindPort %d: "+
			"the client built at load does not source from BindPort, so it must be "+
			"replaced once the listener holds it", bindPort)
	}
}
