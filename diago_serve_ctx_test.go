// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type serveRunKey struct{}

// serveRunValue is what the ctx of a ServeRequests run carries in these tests,
// so a ctx seen by a handler or a cache can be told from any other.
const serveRunValue = "serving run"

// ctxRecordingCache is the server dialog cache that records the ctx values it
// was called with.
type ctxRecordingCache struct {
	dialogCacheMap[*DialogServerSession]

	mu      sync.Mutex
	loads   []any
	deletes []any
}

func (c *ctxRecordingCache) DialogLoad(ctx context.Context, id string) (*DialogServerSession, error) {
	c.mu.Lock()
	c.loads = append(c.loads, ctx.Value(serveRunKey{}))
	c.mu.Unlock()
	return c.dialogCacheMap.DialogLoad(ctx, id)
}

func (c *ctxRecordingCache) DialogDelete(ctx context.Context, id string) error {
	c.mu.Lock()
	c.deletes = append(c.deletes, ctx.Value(serveRunKey{}))
	c.mu.Unlock()
	return c.dialogCacheMap.DialogDelete(ctx, id)
}

func (c *ctxRecordingCache) seen() (loads, deletes []any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]any(nil), c.loads...), append([]any(nil), c.deletes...)
}

// newServeCtxHarness returns a Diago served on a loopback socket, with a peer
// socket, whose request run, when run is true, carries serveRunValue.
func newServeCtxHarness(t *testing.T, run bool, cache DialogCache[*DialogServerSession], handler ServeDialogFunc) *rawInviteHarness {
	t.Helper()
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua)
	if cache != nil {
		dg.cache.server = cache
	}
	if handler != nil {
		dg.HandleFunc(handler)
	}

	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = dg.server.ServeUDP(server) }()

	if run {
		ctx, cancel := context.WithCancel(context.WithValue(t.Context(), serveRunKey{}, serveRunValue))
		ready := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = dg.ServeRequests(ctx, func() { close(ready) })
		}()
		t.Cleanup(func() {
			cancel()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Error("request serving did not end with the test")
			}
		})
		select {
		case <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("request serving did not start")
		}
	}

	h := &rawInviteHarness{log: &recordingLogHandler{}, done: make(chan struct{})}
	h.serverAddr = server.LocalAddr()
	h.serverPort = server.LocalAddr().(*net.UDPAddr).Port
	h.client, err = net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.client.Close() })
	h.clientPort = h.client.LocalAddr().(*net.UDPAddr).Port
	return h
}

// TestServeDialogFuncReceivesServeRequestsCtx pins that the dialog handler is
// given the ctx ServeRequests was given.
func TestServeDialogFuncReceivesServeRequestsCtx(t *testing.T) {
	got := make(chan any, 1)
	h := newServeCtxHarness(t, true, nil, func(ctx context.Context, d *DialogServerSession) {
		got <- ctx.Value(serveRunKey{})
	})

	invite := h.newInvite("serve-ctx", "", "")
	h.send(t, "INVITE", invite)
	res := readFinalResponse(t, h.client, 2*time.Second)
	h.send(t, "ACK", ackLines(invite, res))

	select {
	case v := <-got:
		assert.Equal(t, serveRunValue, v)
	case <-time.After(5 * time.Second):
		t.Fatal("the dialog handler did not run")
	}
}

// TestRequestPathsRunOnTheHandlerCtx pins that the cache lookups of a BYE and
// of a re-INVITE, and the INVITE's removal of its dialog, run on the ctx the
// request handler was given, not on a root of their own.
func TestRequestPathsRunOnTheHandlerCtx(t *testing.T) {
	cache := &ctxRecordingCache{}
	served := make(chan struct{}, 1)
	h := newServeCtxHarness(t, true, cache, func(context.Context, *DialogServerSession) {
		served <- struct{}{}
	})

	invite := h.newInvite("paths-invite", "", "")
	h.send(t, "INVITE", invite)
	res := readFinalResponse(t, h.client, 2*time.Second)
	h.send(t, "ACK", ackLines(invite, res))
	select {
	case <-served:
	case <-time.After(5 * time.Second):
		t.Fatal("the dialog handler did not run")
	}
	require.Eventually(t, func() bool {
		_, deletes := cache.seen()
		return len(deletes) == 1
	}, 5*time.Second, 10*time.Millisecond, "the INVITE did not remove its dialog")

	bye := h.newInvite("paths-bye", "", "nodialog")
	for i, line := range bye {
		if line == "CSeq: 1 INVITE" {
			bye[i] = "CSeq: 2 BYE"
		}
	}
	h.send(t, "BYE", bye)
	require.Equal(t, sip.StatusCallTransactionDoesNotExists, readFinalResponse(t, h.client, 2*time.Second).StatusCode)

	reInvite := h.newInvite("paths-reinvite", "", "nodialog")
	h.send(t, "INVITE", reInvite)
	require.Equal(t, sip.StatusCallTransactionDoesNotExists, readFinalResponse(t, h.client, 2*time.Second).StatusCode)

	loads, deletes := cache.seen()
	assert.Equal(t, []any{serveRunValue}, deletes, "the dialog removal ctx")
	require.NotEmpty(t, loads)
	for i, v := range loads {
		assert.Equal(t, serveRunValue, v, "cache lookup %d ctx", i)
	}
}

// TestServeWithoutServeRequestsAnswersInvite480 pins that serving the listeners
// alone takes no request to a handler: the transaction layer answers an INVITE
// 480 and the dialog handler does not run.
func TestServeWithoutServeRequestsAnswersInvite480(t *testing.T) {
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	ran := make(chan struct{}, 1)
	dg.HandleFunc(func(context.Context, *DialogServerSession) { ran <- struct{}{} })

	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- dg.ServeWithReady(ctx, func() { close(ready) }) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("serving did not end with the test")
		}
	})
	select {
	case <-ready:
	case err := <-done:
		t.Fatalf("serving ended before it was ready: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("serving was not ready")
	}

	tran, ok := dg.getTransport("udp")
	require.True(t, ok)
	client, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	h := &rawInviteHarness{client: client, serverPort: tran.BindPort, clientPort: client.LocalAddr().(*net.UDPAddr).Port}
	h.serverAddr = &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: tran.BindPort}

	invite := h.newInvite("no-run", "", "")
	h.send(t, "INVITE", invite)
	res := readFinalResponse(t, h.client, 2*time.Second)
	assert.Equal(t, sip.StatusTemporarilyUnavailable, res.StatusCode)
	select {
	case <-ran:
		t.Fatal("the dialog handler ran without a request run")
	default:
	}
}
