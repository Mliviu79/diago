// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// storeCountingCache is the server dialog cache that also counts the dialogs
// stored in it, so a test can tell that an INVITE made no dialog.
type storeCountingCache struct {
	dialogCacheMap[*DialogServerSession]

	mu     sync.Mutex
	stored int
}

func (c *storeCountingCache) DialogStore(ctx context.Context, id string, v *DialogServerSession) error {
	c.mu.Lock()
	c.stored++
	c.mu.Unlock()
	return c.dialogCacheMap.DialogStore(ctx, id, v)
}

func (c *storeCountingCache) storedCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.stored
}

// requireHarness is a raw loopback peer against one Diago with the given
// session-timer policy, counting the serve handler's calls and the dialogs
// stored.
type requireHarness struct {
	*rawInviteHarness
	cache  *storeCountingCache
	served chan struct{}

	mu     sync.Mutex
	serves int
}

func newRequireHarness(t *testing.T, timers SessionTimerPolicy) *requireHarness {
	t.Helper()
	h := &requireHarness{
		rawInviteHarness: &rawInviteHarness{log: &recordingLogHandler{}, done: make(chan struct{})},
		cache:            &storeCountingCache{},
		served:           make(chan struct{}, 1),
	}

	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithLogger(slog.New(h.log)), WithSessionTimers(timers))
	dg.cache.server = h.cache
	dg.HandleFunc(func(*DialogServerSession) {
		h.mu.Lock()
		h.serves++
		h.mu.Unlock()
		select {
		case h.served <- struct{}{}:
		default:
		}
	})

	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = dg.server.ServeUDP(server) }()
	h.serverAddr = server.LocalAddr()
	h.serverPort = server.LocalAddr().(*net.UDPAddr).Port

	h.client, err = net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.client.Close() })
	h.clientPort = h.client.LocalAddr().(*net.UDPAddr).Port
	return h
}

func (h *requireHarness) serveCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.serves
}

// TestServeRefusesAnInviteRequiringAnUnsupportedExtension checks RFC 3261
// section 8.2.2.3: a new INVITE whose Require names an option tag diago does
// not support is answered 420 Bad Extension with one Unsupported header listing
// those tags, before any dialog is made and without calling the serve handler.
// diago supports timer while its session-timer policy is enabled and nothing
// else. The check is the new INVITE's only: a re-INVITE is handled as before.
func TestServeRefusesAnInviteRequiringAnUnsupportedExtension(t *testing.T) {
	enabled := SessionTimerPolicy{Enabled: true}
	tests := []struct {
		name            string
		timers          SessionTimerPolicy
		require         []string
		toTag           string
		wantStatus      int
		wantReason      string
		wantUnsupported string
		wantServed      bool
	}{
		{name: "requires replaces", timers: enabled, require: []string{"replaces"}, wantStatus: sip.StatusBadExtension, wantReason: "Bad Extension", wantUnsupported: "replaces"},
		{name: "requires 100rel and replaces", timers: enabled, require: []string{"100rel, replaces"}, wantStatus: sip.StatusBadExtension, wantReason: "Bad Extension", wantUnsupported: "100rel, replaces"},
		{name: "requires 100rel and replaces in two headers", timers: enabled, require: []string{"100rel", "replaces"}, wantStatus: sip.StatusBadExtension, wantReason: "Bad Extension", wantUnsupported: "100rel, replaces"},
		{name: "requires timer and replaces", timers: enabled, require: []string{"timer, replaces"}, wantStatus: sip.StatusBadExtension, wantReason: "Bad Extension", wantUnsupported: "replaces"},
		{name: "requires timer with session timers enabled", timers: enabled, require: []string{"timer"}, wantStatus: sip.StatusTemporarilyUnavailable, wantServed: true},
		{name: "requires timer with session timers disabled", require: []string{"timer"}, wantStatus: sip.StatusBadExtension, wantReason: "Bad Extension", wantUnsupported: "timer"},
		{name: "requires nothing", timers: enabled, wantStatus: sip.StatusTemporarilyUnavailable, wantServed: true},
		{name: "re-INVITE requiring replaces", timers: enabled, require: []string{"replaces"}, toTag: "unknown", wantStatus: sip.StatusCallTransactionDoesNotExists, wantReason: "Call/Transaction Does Not Exist"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newRequireHarness(t, tc.timers)
			invite := h.newInvite(fmt.Sprintf("require-%d", i), "", tc.toTag)
			// Put the Require headers before Max-Forwards and Content-Length.
			tail := append([]string(nil), invite[len(invite)-2:]...)
			invite = invite[:len(invite)-2]
			for _, r := range tc.require {
				invite = append(invite, "Require: "+r)
			}
			invite = append(invite, tail...)

			h.send(t, "INVITE", invite)
			res := readFinalResponse(t, h.client, 2*time.Second)
			h.send(t, "ACK", ackLines(invite, res))

			assert.Equal(t, tc.wantStatus, res.StatusCode)
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, res.Reason)
			}
			unsupported := res.GetHeaders("Unsupported")
			if tc.wantUnsupported == "" {
				assert.Empty(t, unsupported, "an INVITE diago serves gets no Unsupported header")
			} else if assert.Len(t, unsupported, 1, "one Unsupported header lists every unsupported tag") {
				assert.Equal(t, tc.wantUnsupported, unsupported[0].Value())
			}

			if tc.wantServed {
				select {
				case <-h.served:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "the serve handler was not called")
				}
				assert.Equal(t, 1, h.serveCount(), "serve handler calls")
				assert.Equal(t, 1, h.cache.storedCount(), "dialogs stored")
				return
			}
			assert.Equal(t, 0, h.serveCount(), "the serve handler ran for an INVITE refused at arrival")
			assert.Equal(t, 0, h.cache.storedCount(), "a dialog was made for an INVITE refused at arrival")
		})
	}
}
