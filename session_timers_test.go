// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
)

// u32 returns a pointer to the given uint32, for building offered header values.
func u32(v uint32) *uint32 { return &v }

// --- sessionTimer.run ---------------------------------------------------------

// runClock runs session-timer time a thousand times faster: a 90 second
// interval lasts 90ms and a refresh is due every 45ms.
const runClock = time.Millisecond

// startTimer runs a session timer in the given state on its own goroutine and
// returns what it did: each call of refresh, which answers with the outcomes
// in order and then with refreshGoesOn, each call of expire, and a channel
// closed when run returns.
func startTimer(t *testing.T, ctx context.Context, state sessionTimerState, outcomes ...refreshOutcome) (*sessionTimer, <-chan struct{}, <-chan struct{}, <-chan struct{}) {
	t.Helper()
	timer := newSessionTimer(SessionTimerPolicy{Enabled: true, clock: runClock}, state)
	refreshes := make(chan struct{}, 64)
	expires := make(chan struct{}, 64)
	done := make(chan struct{})
	var mu sync.Mutex
	go func() {
		defer close(done)
		timer.run(ctx, func(context.Context) refreshOutcome {
			refreshes <- struct{}{}
			mu.Lock()
			defer mu.Unlock()
			if len(outcomes) == 0 {
				return refreshGoesOn
			}
			o := outcomes[0]
			outcomes = outcomes[1:]
			return o
		}, func(context.Context) {
			expires <- struct{}{}
		})
	}()
	return timer, refreshes, expires, done
}

func awaitSignal(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(within):
		t.Fatalf("%s did not happen within %s", what, within)
	}
}

func expectNoSignal(t *testing.T, ch <-chan struct{}, within time.Duration, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s happened", what)
	case <-time.After(within):
	}
}

// peerRefresh is a re-INVITE of the peer's carrying Session-Expires se.
func peerRefresh(se string) *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "dg", Host: "127.0.0.1"})
	req.AppendHeader(sip.NewHeader("Supported", "timer"))
	req.AppendHeader(sip.NewHeader("Session-Expires", se))
	return req
}

// TestSessionTimer_RefreshesAtTheCadenceUntilCancelled verifies that while we
// refresh, run refreshes every half interval for as long as each refresh goes
// on, and returns once its ctx is cancelled, the end of the dialog.
func TestSessionTimer_RefreshesAtTheCadenceUntilCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	_, refreshes, expires, done := startTimer(t, ctx, sessionTimerState{active: true, expires: 90 * time.Second, weRefresh: true})
	for range 3 {
		awaitSignal(t, refreshes, time.Second, "a refresh")
	}
	cancel()
	awaitSignal(t, done, time.Second, "run returning on cancel")
	expectNoSignal(t, expires, 0, "an expiry while we refresh")
}

// TestSessionTimer_AStoppedRefreshWaitsForAChange verifies that a refresh that
// stopped refreshing sends no further refresh until the state changes, which
// resumes refreshing. There is no retry counter: RFC 4028 section 10 leaves
// the session to the peer's own timer.
func TestSessionTimer_AStoppedRefreshWaitsForAChange(t *testing.T) {
	timer, refreshes, _, _ := startTimer(t, t.Context(), sessionTimerState{active: true, expires: 90 * time.Second, weRefresh: true}, refreshStopped)
	awaitSignal(t, refreshes, time.Second, "the first refresh")
	expectNoSignal(t, refreshes, 150*time.Millisecond, "a refresh after refreshing stopped")
	timer.answerPeer(peerRefresh("90;refresher=uas"))
	awaitSignal(t, refreshes, time.Second, "a refresh after the peer's refresh named us")
}

// TestSessionTimer_AnEndedRefreshReturns verifies that run returns once a
// refresh ended the call.
func TestSessionTimer_AnEndedRefreshReturns(t *testing.T) {
	_, refreshes, _, done := startTimer(t, t.Context(), sessionTimerState{active: true, expires: 90 * time.Second, weRefresh: true}, refreshEnded)
	awaitSignal(t, refreshes, time.Second, "the refresh")
	awaitSignal(t, done, time.Second, "run returning after the call ended")
}

// TestSessionTimer_ExitsOnContextCancel verifies run returns without a refresh
// or an expiry when its ctx is cancelled before it starts, and while it waits
// on either side.
func TestSessionTimer_ExitsOnContextCancel(t *testing.T) {
	for _, weRefresh := range []bool{true, false} {
		t.Run(fmt.Sprintf("we refresh %v, cancelled before the start", weRefresh), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			_, refreshes, expires, done := startTimer(t, ctx, sessionTimerState{active: true, expires: 90 * time.Second, weRefresh: weRefresh})
			awaitSignal(t, done, time.Second, "run returning")
			expectNoSignal(t, refreshes, 0, "a refresh")
			expectNoSignal(t, expires, 0, "an expiry")
		})
		t.Run(fmt.Sprintf("we refresh %v, cancelled while waiting", weRefresh), func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			_, refreshes, expires, done := startTimer(t, ctx, sessionTimerState{active: true, expires: 1800 * time.Second, weRefresh: weRefresh})
			cancel()
			awaitSignal(t, done, time.Second, "run returning")
			expectNoSignal(t, refreshes, 0, "a refresh")
			expectNoSignal(t, expires, 0, "an expiry")
		})
	}
}

// TestSessionTimer_ExpiresOnceWhenThePeerStopsRefreshing verifies that while
// the peer refreshes, run ends the call once, the session interval plus the
// grace after the last change, and returns.
func TestSessionTimer_ExpiresOnceWhenThePeerStopsRefreshing(t *testing.T) {
	_, refreshes, expires, done := startTimer(t, t.Context(), sessionTimerState{active: true, expires: 90 * time.Second})
	awaitSignal(t, expires, time.Second, "the expiry")
	awaitSignal(t, done, time.Second, "run returning after the expiry")
	expectNoSignal(t, expires, 0, "a second expiry")
	expectNoSignal(t, refreshes, 0, "a refresh while the peer refreshes")
}

// TestSessionTimer_APeerRefreshResetsTheExpiry verifies that each 2xx to a
// peer's refresh restarts the wait for the next, so a peer that keeps
// refreshing is never cut, and one that stops is, an interval plus the grace
// after its last refresh.
func TestSessionTimer_APeerRefreshResetsTheExpiry(t *testing.T) {
	timer, _, expires, done := startTimer(t, t.Context(), sessionTimerState{active: true, expires: 90 * time.Second})
	tick := time.NewTicker(30 * time.Millisecond)
	defer tick.Stop()
	for range 10 {
		select {
		case <-expires:
			t.Fatal("the session expired while the peer kept refreshing")
		case <-tick.C:
			timer.answerPeer(peerRefresh("90;refresher=uac"))
		}
	}
	awaitSignal(t, expires, time.Second, "the expiry after the peer stopped")
	awaitSignal(t, done, time.Second, "run returning after the expiry")
}

// TestSessionTimer_ARefresherChangeMovesTheRefreshing verifies that the 2xx
// to a peer's refresh naming us the refresher has run refresh where it
// watched, and the 2xx to our refresh naming the peer has it watch where it
// refreshed; one goroutine does either, never both.
func TestSessionTimer_ARefresherChangeMovesTheRefreshing(t *testing.T) {
	timer, refreshes, expires, _ := startTimer(t, t.Context(), sessionTimerState{active: true, expires: 90 * time.Second})
	timer.answerPeer(peerRefresh("90;refresher=uas"))
	awaitSignal(t, refreshes, time.Second, "our refresh")
	expectNoSignal(t, expires, 0, "an expiry once we refresh")

	res := sip.NewResponseFromRequest(peerRefresh("90"), sip.StatusOK, "OK", nil)
	res.AppendHeader(sip.NewHeader("Session-Expires", "90;refresher=uas"))
	timer.answered(res, true)
	awaitSignal(t, expires, time.Second, "the expiry once the peer refreshes")
}

// TestPeerRefreshWatchdog_GraceResolvesViaPolicy verifies the grace source: the
// policy field wins when set, otherwise the constant fallback applies.
func TestPeerRefreshWatchdog_GraceResolvesViaPolicy(t *testing.T) {
	if got := watchdogGrace(SessionTimerPolicy{}); got != defaultWatchdogGrace {
		t.Fatalf("watchdogGrace(zero) = %v, want %v", got, defaultWatchdogGrace)
	}
	if got := watchdogGrace(SessionTimerPolicy{WatchdogGrace: 3 * time.Second}); got != 3*time.Second {
		t.Fatalf("watchdogGrace(3s) = %v, want 3s", got)
	}
}

// --- negotiate ----------------------------------------------------------------

// TestSessionTimerNegotiate exercises the pure, role-agnostic RFC 4028
// negotiation: the Min-SE floor, the advertise-fallback when nothing is offered,
// the half refresh interval with anti-storm clamp, honoring the peer's
// refresher, the PreferUASRefresher tiebreaker only when the peer omits one, and
// the below-floor 422 signal.
func TestSessionTimerNegotiate(t *testing.T) {
	tests := []struct {
		name             string
		offeredSE        *uint32
		offeredMinSE     *uint32
		offeredRefresher string
		policy           SessionTimerPolicy
		ourRole          string
		want             sessionTimerDecision
	}{
		{
			name:    "none offered uses advertise-fallback, uas tiebreaker, we refresh",
			policy:  SessionTimerPolicy{PreferUASRefresher: true},
			ourRole: refresherUAS,
			want: sessionTimerDecision{
				Negotiated: defaultSessionExpires,
				Interval:   defaultSessionExpires / 2,
				Refresher:  refresherUAS,
				WeRefresh:  true,
			},
		},
		{
			name:             "peer refresher honored over our preference",
			offeredSE:        u32(1200),
			offeredRefresher: refresherUAC,
			policy:           SessionTimerPolicy{PreferUASRefresher: true},
			ourRole:          refresherUAS,
			want: sessionTimerDecision{
				Negotiated: 1200 * time.Second,
				Interval:   600 * time.Second,
				Refresher:  refresherUAC,
				WeRefresh:  false,
			},
		},
		{
			name:      "no peer refresher, PreferUAS true -> uas tiebreaker",
			offeredSE: u32(1800),
			policy:    SessionTimerPolicy{PreferUASRefresher: true},
			ourRole:   refresherUAS,
			want: sessionTimerDecision{
				Negotiated: 1800 * time.Second,
				Interval:   900 * time.Second,
				Refresher:  refresherUAS,
				WeRefresh:  true,
			},
		},
		{
			name:      "no peer refresher, PreferUAS false -> neutral, not forced uas",
			offeredSE: u32(1800),
			policy:    SessionTimerPolicy{PreferUASRefresher: false},
			ourRole:   refresherUAS,
			want: sessionTimerDecision{
				Negotiated: 1800 * time.Second,
				Interval:   900 * time.Second,
				Refresher:  "",
				WeRefresh:  false,
			},
		},
		{
			name:      "offer below floor -> below-floor 422 signal, negotiated reports the floor",
			offeredSE: u32(30),
			policy:    SessionTimerPolicy{PreferUASRefresher: true},
			ourRole:   refresherUAS,
			want: sessionTimerDecision{
				Negotiated: defaultMinSE,
				Interval:   defaultMinSE / 2,
				Refresher:  refresherUAS,
				BelowFloor: true,
				WeRefresh:  true,
			},
		},
		{
			name:         "peer Min-SE raises the floor above ours",
			offeredSE:    u32(1800),
			offeredMinSE: u32(120),
			policy:       SessionTimerPolicy{PreferUASRefresher: true},
			ourRole:      refresherUAS,
			want: sessionTimerDecision{
				Negotiated: 1800 * time.Second,
				Interval:   900 * time.Second,
				Refresher:  refresherUAS,
				WeRefresh:  true,
			},
		},
		{
			name:         "peer Min-SE above the offer raises the negotiated interval",
			offeredSE:    u32(100),
			offeredMinSE: u32(600),
			policy:       SessionTimerPolicy{PreferUASRefresher: true},
			ourRole:      refresherUAS,
			want: sessionTimerDecision{
				Negotiated: 600 * time.Second,
				Interval:   300 * time.Second,
				Refresher:  refresherUAS,
				BelowFloor: true,
				WeRefresh:  true,
			},
		},
		{
			name:    "configurable DefaultSE advertise-fallback is used, not the constant",
			policy:  SessionTimerPolicy{DefaultSE: 600 * time.Second, PreferUASRefresher: true},
			ourRole: refresherUAS,
			want: sessionTimerDecision{
				Negotiated: 600 * time.Second,
				Interval:   300 * time.Second,
				Refresher:  refresherUAS,
				WeRefresh:  true,
			},
		},
		{
			name:             "we are uac and peer elected uac -> we refresh",
			offeredSE:        u32(1800),
			offeredRefresher: refresherUAC,
			policy:           SessionTimerPolicy{},
			ourRole:          refresherUAC,
			want: sessionTimerDecision{
				Negotiated: 1800 * time.Second,
				Interval:   900 * time.Second,
				Refresher:  refresherUAC,
				WeRefresh:  true,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := negotiate(tt.offeredSE, tt.offeredMinSE, tt.offeredRefresher, tt.policy, tt.ourRole)
			if got != tt.want {
				t.Fatalf("negotiate() = %+v, want %+v", got, tt.want)
			}
			// Anti-storm: the refresh cadence is never sub-floor, even for a tiny
			// offer.
			if got.Interval < minRefreshInterval {
				t.Fatalf("Interval = %v, want >= minRefreshInterval %v", got.Interval, minRefreshInterval)
			}
		})
	}
}

// --- parseSessionTimerOffer ---------------------------------------------------

// TestParseSessionTimerOffer pins the header shim that stands in for the typed
// accessors sipgo does not provide: the delta and refresher param are read off
// Session-Expires, Min-SE is optional, the compact form "x" is accepted, and a
// malformed value is treated as absent rather than as an error.
func TestParseSessionTimerOffer(t *testing.T) {
	tests := []struct {
		name          string
		headers       [][2]string
		wantOK        bool
		wantSE        *uint32
		wantMinSE     *uint32
		wantRefresher string
	}{
		{
			name:          "delta with refresher param",
			headers:       [][2]string{{"Session-Expires", "1800;refresher=uac"}},
			wantOK:        true,
			wantSE:        u32(1800),
			wantRefresher: refresherUAC,
		},
		{
			name:    "bare delta, no params",
			headers: [][2]string{{"Session-Expires", "1800"}},
			wantOK:  true,
			wantSE:  u32(1800),
		},
		{
			name:          "refresher param is case insensitive",
			headers:       [][2]string{{"Session-Expires", "1800;REFRESHER=UAS"}},
			wantOK:        true,
			wantSE:        u32(1800),
			wantRefresher: refresherUAS,
		},
		{
			name:      "Min-SE is parsed alongside",
			headers:   [][2]string{{"Session-Expires", "1800"}, {"Min-SE", "120"}},
			wantOK:    true,
			wantSE:    u32(1800),
			wantMinSE: u32(120),
		},
		{
			name:          "compact form x is accepted",
			headers:       [][2]string{{"x", "1800;refresher=uas"}},
			wantOK:        true,
			wantSE:        u32(1800),
			wantRefresher: refresherUAS,
		},
		{
			name:    "no Session-Expires means timer-unaware peer",
			headers: nil,
			wantOK:  false,
		},
		{
			name:    "malformed delta is treated as absent, never an error",
			headers: [][2]string{{"Session-Expires", "not-a-number"}},
			wantOK:  false,
		},
		{
			name:    "out-of-range delta is treated as absent",
			headers: [][2]string{{"Session-Expires", "4294967296"}},
			wantOK:  false,
		},
		{
			name:      "malformed Min-SE is ignored, Session-Expires still stands",
			headers:   [][2]string{{"Session-Expires", "1800"}, {"Min-SE", "bogus"}},
			wantOK:    true,
			wantSE:    u32(1800),
			wantMinSE: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := sip.NewRequest(sip.INVITE, sip.Uri{User: "alice", Host: "example.com"})
			for _, h := range tt.headers {
				req.AppendHeader(sip.NewHeader(h[0], h[1]))
			}

			offer, ok := parseSessionTimerOffer(req)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if got, want := derefU32(offer.SE), derefU32(tt.wantSE); got != want {
				t.Fatalf("SE = %v, want %v", got, want)
			}
			if got, want := derefU32(offer.MinSE), derefU32(tt.wantMinSE); got != want {
				t.Fatalf("MinSE = %v, want %v", got, want)
			}
			if offer.Refresher != tt.wantRefresher {
				t.Fatalf("Refresher = %q, want %q", offer.Refresher, tt.wantRefresher)
			}
		})
	}
}

// derefU32 renders an optional uint32 as a comparable value, using -1 for nil so
// "absent" and zero stay distinguishable in test failures.
func derefU32(p *uint32) int64 {
	if p == nil {
		return -1
	}
	return int64(*p)
}

// --- buildTimerAnswer ---------------------------------------------------------

// inviteWithSE builds a minimal INVITE carrying the given Session-Expires value.
// An empty se models a timer-unaware peer that sent no Session-Expires.
func inviteWithSE(se string) *sip.Request {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "alice", Host: "example.com"})
	if se != "" {
		req.AppendHeader(sip.NewHeader("Session-Expires", se))
	}
	return req
}

// answerHeader returns the value of the first answer header matching name.
func answerHeader(hdrs []sip.Header, name string) (string, bool) {
	for _, h := range hdrs {
		if strings.EqualFold(h.Name(), name) {
			return h.Value(), true
		}
	}
	return "", false
}

// TestTimerAnswer pins the answer-path decision over the pure buildTimerAnswer
// helper: honor the peer's offered refresher, advertise Supported: timer and
// never Require: timer, reject below-floor with 422 plus Min-SE, no-op
// gracefully for a timer-unaware peer, and elect who refreshes.
func TestTimerAnswer(t *testing.T) {
	tests := []struct {
		name          string
		se            string
		policy        SessionTimerPolicy
		wantStatus    int
		wantSE        string // expected Session-Expires value, "" means none expected
		wantSupported bool
		wantMinSE     string // expected Min-SE value on a 422, "" means none
		wantAnswer    bool
		wantWeRefresh bool
	}{
		{
			// Peer offered refresher=uac: the answer honors uac rather than forcing
			// uas, we are not the refresher, so the peer refreshes and we watch.
			name:          "honor peer refresher=uac -> arm watchdog",
			se:            "1800;refresher=uac",
			policy:        SessionTimerPolicy{Enabled: true, PreferUASRefresher: true},
			wantStatus:    200,
			wantSE:        "1800;refresher=uac",
			wantSupported: true,
			wantAnswer:    true,
			wantWeRefresh: false,
		},
		{
			// Peer omitted the refresher: the PreferUASRefresher tiebreaker elects
			// uas, we are the refresher, so we refresh.
			name:          "omitted refresher, PreferUAS -> refresher=uas, start loop",
			se:            "1800",
			policy:        SessionTimerPolicy{Enabled: true, PreferUASRefresher: true},
			wantStatus:    200,
			wantSE:        "1800;refresher=uas",
			wantSupported: true,
			wantAnswer:    true,
			wantWeRefresh: true,
		},
		{
			// Below the 90s floor: 422 Session Interval Too Small plus Min-SE 90,
			// no loop and no watchdog.
			name:       "offer below floor -> 422 Min-SE, no loop/watchdog",
			se:         "30",
			policy:     SessionTimerPolicy{Enabled: true, PreferUASRefresher: true},
			wantStatus: 422,
			wantMinSE:  "90",
			wantAnswer: false,
		},
		{
			// Timer-unaware peer: graceful no-op with no timer headers, no loop and
			// no watchdog, answered normally and never rejected.
			name:          "no timer support -> graceful no-op",
			se:            "",
			policy:        SessionTimerPolicy{Enabled: true, PreferUASRefresher: true},
			wantStatus:    200,
			wantSE:        "",
			wantSupported: false,
			wantAnswer:    false,
		},
		{
			name:          "timers disabled -> no headers",
			se:            "1800",
			policy:        SessionTimerPolicy{Enabled: false, PreferUASRefresher: true},
			wantStatus:    200,
			wantSE:        "",
			wantSupported: false,
			wantAnswer:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ta, hdrs, status := buildTimerAnswer(inviteWithSE(tt.se), tt.policy)

			if status != tt.wantStatus {
				t.Fatalf("status = %d, want %d", status, tt.wantStatus)
			}
			if (ta != nil) != tt.wantAnswer {
				t.Fatalf("answer present = %v, want %v", ta != nil, tt.wantAnswer)
			}
			if ta != nil {
				if ta.weRefresh != tt.wantWeRefresh {
					t.Fatalf("weRefresh = %v, want %v", ta.weRefresh, tt.wantWeRefresh)
				}
			}

			// We advertise Supported: timer and never Require: timer, so a
			// timer-unaware peer is never rejected for lacking timer support.
			if _, ok := answerHeader(hdrs, "Require"); ok {
				t.Fatalf("answer must never carry Require: timer; headers=%v", hdrs)
			}

			if tt.wantSE != "" {
				gotSE, ok := answerHeader(hdrs, "Session-Expires")
				if !ok {
					t.Fatalf("missing Session-Expires header; headers=%v", hdrs)
				}
				if gotSE != tt.wantSE {
					t.Fatalf("Session-Expires = %q, want %q", gotSE, tt.wantSE)
				}
			} else if gotSE, ok := answerHeader(hdrs, "Session-Expires"); ok {
				t.Fatalf("unexpected Session-Expires = %q, want none", gotSE)
			}

			_, gotSupported := answerHeader(hdrs, "Supported")
			if gotSupported != tt.wantSupported {
				t.Fatalf("Supported present = %v, want %v; headers=%v", gotSupported, tt.wantSupported, hdrs)
			}
			if tt.wantSupported {
				if v, _ := answerHeader(hdrs, "Supported"); v != "timer" {
					t.Fatalf("Supported = %q, want \"timer\"", v)
				}
			}

			if tt.wantMinSE != "" {
				gotMinSE, ok := answerHeader(hdrs, "Min-SE")
				if !ok {
					t.Fatalf("missing Min-SE header on 422; headers=%v", hdrs)
				}
				if gotMinSE != tt.wantMinSE {
					t.Fatalf("Min-SE = %q, want %q", gotMinSE, tt.wantMinSE)
				}
			}
		})
	}
}

// TestWithSessionTimers verifies the option stamps the policy onto Diago, which
// is what the dialog construction path reads. Without the option timers stay
// disabled, preserving the existing behavior for callers that never opt in.
func TestWithSessionTimers(t *testing.T) {
	policy := SessionTimerPolicy{Enabled: true, DefaultSE: 600 * time.Second, PreferUASRefresher: true}

	dg := &Diago{}
	WithSessionTimers(policy)(dg)

	if dg.sessionTimers != policy {
		t.Fatalf("sessionTimers = %+v, want %+v", dg.sessionTimers, policy)
	}

	if (&Diago{}).sessionTimers.Enabled {
		t.Fatal("session timers must be disabled without the option")
	}
}
