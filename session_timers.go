// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo/sip"
)

// RFC 4028 session-timer policy defaults.
const (
	// defaultSessionExpires is the session interval we advertise as a fallback
	// only when a timer-supporting peer offers no Session-Expires of its own.
	// SessionTimerPolicy.DefaultSE overrides it. It is never a value imposed on
	// a peer that offered one.
	defaultSessionExpires = 1800 * time.Second

	// defaultMinSE is the minimum acceptable session interval, the RFC 4028 §4
	// default of 90 seconds. An offered Session-Expires below this is reported
	// as below-floor so the answer path can reject with 422 and a Min-SE header.
	defaultMinSE = 90 * time.Second

	// defaultWatchdogGrace is the tolerance added to the negotiated interval
	// before the peer-refresh watchdog hangs up on a missed refresh. Used when
	// SessionTimerPolicy.WatchdogGrace is zero.
	defaultWatchdogGrace = 5 * time.Second

	// minRefreshInterval clamps the refresh cadence so a tiny offered
	// Session-Expires can never drive a sub-floor re-INVITE storm.
	minRefreshInterval = defaultMinSE / 2
)

// Refresher tokens as carried in the RFC 4028 refresher parameter.
const (
	refresherUAS = "uas"
	refresherUAC = "uac"
)

// SessionTimerPolicy is the RFC 4028 session-timer policy. It is stamped onto
// each dialog at construction via WithSessionTimers and drives negotiation, the
// refresh loop and the peer-refresh watchdog. The zero value is disabled and is
// safe to use.
type SessionTimerPolicy struct {
	// Enabled turns session-timer advertisement and negotiation on.
	Enabled bool

	// DefaultSE is the advertise-fallback session interval, used only when we
	// advertise to a timer-supporting peer that offered no Session-Expires. Zero
	// falls back to defaultSessionExpires. It is never imposed on a peer's own
	// offer.
	DefaultSE time.Duration

	// MinSE is our floor for an acceptable session interval. Zero falls back to
	// defaultMinSE. The effective floor is max(MinSE, the peer's offered Min-SE).
	MinSE time.Duration

	// PreferUASRefresher is a tiebreaker applied only when the peer omits a
	// refresher: true elects "uas", false stays neutral. When the peer offers a
	// refresher it is always honored and this never overrides it.
	PreferUASRefresher bool

	// WatchdogGrace is the tolerance added to the negotiated interval before the
	// peer-refresh watchdog hangs up on a missed refresh. Zero falls back to
	// defaultWatchdogGrace.
	WatchdogGrace time.Duration

	// clock is how long one second of session-timer time lasts on the wall:
	// every interval, grace and Retry-After the timer waits out is scaled by
	// it. Zero is a second. Only this package's tests set it, to run session
	// timers in milliseconds.
	clock time.Duration
}

// wall returns how long d, a session-timer duration, lasts on the wall.
func (p SessionTimerPolicy) wall(d time.Duration) time.Duration {
	if p.clock <= 0 {
		return d
	}
	return time.Duration(int64(d) / int64(time.Second) * int64(p.clock))
}

// sessionTimerDecision is the outcome of negotiate.
type sessionTimerDecision struct {
	// Negotiated is the agreed session interval. It is also the Min-SE value to
	// echo when rejecting a below-floor offer.
	Negotiated time.Duration

	// Interval is the refresh cadence: half the negotiated interval, clamped to
	// minRefreshInterval.
	Interval time.Duration

	// Refresher is the elected refresher token: "uas", "uac", or "" for neutral.
	Refresher string

	// BelowFloor is true when an offered Session-Expires was strictly below the
	// floor, signalling the answer path to reply 422.
	BelowFloor bool

	// WeRefresh is true when the elected refresher is our role: the caller runs
	// the refresh loop, otherwise it arms the peer-refresh watchdog.
	WeRefresh bool
}

// watchdogGrace resolves the peer-refresh watchdog grace from policy, falling
// back to defaultWatchdogGrace when the field is zero. It is the single source
// for the grace so call sites never pass a bare literal.
func watchdogGrace(policy SessionTimerPolicy) time.Duration {
	if policy.WatchdogGrace > 0 {
		return policy.WatchdogGrace
	}
	return defaultWatchdogGrace
}

// negotiate is a pure, role-agnostic RFC 4028 session-timer negotiation. Given a
// peer's offered Session-Expires, Min-SE and refresher plus our policy it floors
// the interval by Min-SE, falls back to the advertise-fallback when nothing is
// offered, halves for the refresh cadence, honors the peer's refresher (using
// PreferUASRefresher only as a tiebreaker when the peer omits one) and flags a
// below-floor offer for a 422 reply. ourRole only decides WeRefresh, so the same
// function serves the UAS answer path and a future UAC client path unchanged.
//
// All inputs are bounded uint32 deltas, so the duration arithmetic cannot
// overflow.
func negotiate(offeredSE *uint32, offeredMinSE *uint32, offeredRefresher string, policy SessionTimerPolicy, ourRole string) sessionTimerDecision {
	// Effective floor: our Min-SE, raised by the peer's when it offered a higher
	// one. Per RFC 4028 the floor is the max of both parties' minima, so the peer
	// can raise the floor but never lower ours.
	floor := policy.MinSE
	if floor <= 0 {
		floor = defaultMinSE
	}
	if offeredMinSE != nil {
		if peerMin := time.Duration(*offeredMinSE) * time.Second; peerMin > floor {
			floor = peerMin
		}
	}

	// Candidate interval: the peer's offer, or our advertise-fallback when the
	// peer is timer-supporting but offered no Session-Expires.
	var (
		candidate  time.Duration
		belowFloor bool
	)
	if offeredSE != nil {
		candidate = time.Duration(*offeredSE) * time.Second
		// A strictly below-floor offer is flagged for a 422. The negotiated value
		// still reports the floor, which is the Min-SE the answer path echoes.
		belowFloor = candidate < floor
	} else {
		candidate = policy.DefaultSE
		if candidate <= 0 {
			candidate = defaultSessionExpires
		}
	}

	// Clamp up to the floor so a below-floor offer never drives a sub-floor
	// cadence.
	negotiated := candidate
	if negotiated < floor {
		negotiated = floor
	}

	// Refresh at half the negotiated interval, clamped so no offer can storm the
	// signaling path.
	interval := refreshCadence(negotiated)

	// Honor the peer's offered refresher. Only tiebreak with PreferUASRefresher
	// when the peer omitted one; false stays neutral and never forces "uas".
	refresher := offeredRefresher
	if refresher == "" && policy.PreferUASRefresher {
		refresher = refresherUAS
	}

	return sessionTimerDecision{
		Negotiated: negotiated,
		Interval:   interval,
		Refresher:  refresher,
		BelowFloor: belowFloor,
		WeRefresh:  refresher != "" && refresher == ourRole,
	}
}

// sessionTimerOffer is a peer's parsed RFC 4028 offer.
type sessionTimerOffer struct {
	SE        *uint32
	MinSE     *uint32
	Refresher string
}

// parseSessionTimerOffer reads Session-Expires and Min-SE off a request or a
// response. sipgo
// carries no typed accessors for these headers, so they are read generically and
// parsed here. A malformed or out-of-range value is treated as absent rather
// than as an error: a timer header is never worth failing an otherwise valid
// INVITE over.
// ok is false when the peer sent no Session-Expires at all (timer-unaware).
func parseSessionTimerOffer(msg headerGetter) (offer sessionTimerOffer, ok bool) {
	h := msg.GetHeader("Session-Expires")
	if h == nil {
		// Compact form of Session-Expires per RFC 4028 §4.
		h = msg.GetHeader("x")
	}
	if h == nil {
		return sessionTimerOffer{}, false
	}

	value, params, _ := strings.Cut(h.Value(), ";")
	se, err := parseDeltaSeconds(value)
	if err != nil {
		return sessionTimerOffer{}, false
	}
	offer.SE = &se

	for _, p := range strings.Split(params, ";") {
		k, v, found := strings.Cut(p, "=")
		if !found {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(k), "refresher") {
			offer.Refresher = strings.ToLower(strings.TrimSpace(v))
		}
	}

	if minSE, ok := parseMinSE(msg); ok {
		offer.MinSE = &minSE
	}

	return offer, true
}

// parseMinSE reads the delta-seconds of msg's Min-SE header, which may itself
// carry generic params. ok is false when it has none or a malformed one.
func parseMinSE(msg headerGetter) (minSE uint32, ok bool) {
	m := msg.GetHeader("Min-SE")
	if m == nil {
		return 0, false
	}
	mv, _, _ := strings.Cut(m.Value(), ";")
	v, err := parseDeltaSeconds(mv)
	if err != nil {
		return 0, false
	}
	return v, true
}

// parseDeltaSeconds parses an RFC 3261 delta-seconds value, the on-the-wire form
// of Session-Expires and Min-SE. It rejects anything that does not fit the
// unsigned 32-bit range the negotiation arithmetic assumes.
func parseDeltaSeconds(s string) (uint32, error) {
	v, err := strconv.ParseUint(strings.TrimSpace(s), 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(v), nil
}

// secondsOf renders a session-timer duration as integer delta-seconds, the
// on-the-wire form for Session-Expires and Min-SE header values.
func secondsOf(d time.Duration) string {
	return strconv.FormatInt(int64(d/time.Second), 10)
}

// refreshCadence is how long after a refresh the next one is sent for a
// session interval of expires: half of it (RFC 4028 section 10), clamped to
// minRefreshInterval so no interval can storm the signalling path.
func refreshCadence(expires time.Duration) time.Duration {
	return max(expires/2, minRefreshInterval)
}

// headerGetter is the header access requests and responses share.
type headerGetter interface {
	GetHeader(name string) sip.Header
	GetHeaders(name string) []sip.Header
}

// supportsTimer reports whether msg lists the timer option tag in a Supported
// header, or in its compact form k.
func supportsTimer(msg headerGetter) bool {
	for _, name := range []string{"Supported", "k"} {
		for _, h := range msg.GetHeaders(name) {
			for _, tag := range strings.Split(h.Value(), ",") {
				if strings.EqualFold(strings.TrimSpace(tag), "timer") {
					return true
				}
			}
		}
	}
	return false
}

// retryAfter returns the delta-seconds of res's Retry-After header, which may
// be followed by a comment and parameters (RFC 3261 section 20.33), and
// whether it has one.
func retryAfter(res *sip.Response) (time.Duration, bool) {
	h := res.GetHeader("Retry-After")
	if h == nil {
		return 0, false
	}
	value := h.Value()
	if i := strings.IndexAny(value, " \t;("); i >= 0 {
		value = value[:i]
	}
	v, err := parseDeltaSeconds(value)
	if err != nil {
		return 0, false
	}
	return time.Duration(v) * time.Second, true
}

// sessionTimerState is a dialog's RFC 4028 session-timer state.
type sessionTimerState struct {
	// active is true while the session has an interval: from the 2xx that
	// answered the INVITE until a 2xx to a re-INVITE of ours carries no
	// Session-Expires (RFC 4028 section 7.2).
	active bool
	// expires is the session interval the most recent 2xx named (RFC 4028
	// section 9).
	expires time.Duration
	// weRefresh is true when we are the refresher, false when the peer is.
	weRefresh bool
	// minSE is the largest Min-SE a request of the peer's or a 422 to a
	// refresh of ours carried, zero while none has.
	minSE time.Duration
}

// sessionTimer is a dialog's running RFC 4028 session timer. Its state changes
// only with a 2xx: one that answers a re-INVITE of ours (answered) and one we
// send to a re-INVITE of the peer's (answerPeer); a 422 raises its Min-SE. run
// refreshes the session while we are the refresher, and ends the dialog when
// the peer is and lets the session expire. Each change restarts run's wait, so
// the refreshing and the watch on the peer's refreshing are one goroutine and
// never run together.
type sessionTimer struct {
	policy SessionTimerPolicy
	// changed is signalled, without blocking, on every change of state that
	// moves run's wait.
	changed chan struct{}
	// done is closed when run returns.
	done chan struct{}

	mu    sync.Mutex
	state sessionTimerState
}

func newSessionTimer(policy SessionTimerPolicy, state sessionTimerState) *sessionTimer {
	return &sessionTimer{policy: policy, changed: make(chan struct{}, 1), done: make(chan struct{}), state: state}
}

// snapshot returns the state.
func (t *sessionTimer) snapshot() sessionTimerState {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.state
}

// setLocked replaces the state and signals run. Called with mu held.
func (t *sessionTimer) setLocked(state sessionTimerState) {
	t.state = state
	select {
	case t.changed <- struct{}{}:
	default:
	}
}

// requestHeaders returns the session-timer headers of a re-INVITE of ours,
// none while the timer is off, and whether they name us the refresher (RFC
// 4028 sections 7.1 and 7.4): Supported: timer; Session-Expires with the larger
// of the Min-SE seen and the current interval, and refresher=uac when we
// refresh or uas when the peer does; and Min-SE once one was seen.
func (t *sessionTimer) requestHeaders() ([]sip.Header, bool) {
	st := t.snapshot()
	if !st.active {
		return nil, false
	}
	refresher := refresherUAS
	if st.weRefresh {
		refresher = refresherUAC
	}
	hdrs := []sip.Header{
		sip.NewHeader("Supported", "timer"),
		sip.NewHeader("Session-Expires", secondsOf(max(st.minSE, st.expires))+";refresher="+refresher),
	}
	if st.minSE > 0 {
		hdrs = append(hdrs, sip.NewHeader("Min-SE", secondsOf(st.minSE)))
	}
	return hdrs, st.weRefresh
}

// answered sets the state from res, the 2xx to a re-INVITE of ours whose
// session-timer headers named us the refresher when weRefresh (RFC 4028
// section 7.2): its Session-Expires gives the interval and the refresher, the
// one we named when it names none; a 2xx without Session-Expires turns the
// timer off. A 2xx while the timer is off changes nothing: our re-INVITE
// asked for no timer.
func (t *sessionTimer) answered(res *sip.Response, weRefresh bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state
	if !st.active {
		return
	}
	offer, ok := parseSessionTimerOffer(res)
	if !ok {
		st.active = false
		t.setLocked(st)
		return
	}
	st.expires = time.Duration(*offer.SE) * time.Second
	switch offer.Refresher {
	case refresherUAC:
		st.weRefresh = true
	case refresherUAS:
		st.weRefresh = false
	default:
		st.weRefresh = weRefresh
	}
	t.setLocked(st)
}

// raiseMinSE records the Min-SE of a 422 to a refresh of ours, so the retry
// asks for at least it (RFC 4028 section 7.3).
func (t *sessionTimer) raiseMinSE(minSE time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state.minSE = max(t.state.minSE, minSE)
}

// answerPeer returns the session-timer headers of the 2xx to req, a re-INVITE
// of the peer's, none while the timer is off, and sets the state that 2xx
// names (RFC 4028 section 9): the interval req's Session-Expires carries, else
// the current one; the refresher req names, else the current one when req
// lists timer in Supported, else us; Supported: timer; and Require: timer when
// the peer is to refresh. It is called as the 2xx is sent, as the session
// interval is the one "in the most recent 2xx response".
func (t *sessionTimer) answerPeer(req *sip.Request) []sip.Header {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.state
	if !st.active {
		return nil
	}
	offer, ok := parseSessionTimerOffer(req)
	if ok {
		st.expires = time.Duration(*offer.SE) * time.Second
	}
	if minSE, ok := parseMinSE(req); ok {
		st.minSE = max(st.minSE, time.Duration(minSE)*time.Second)
	}
	switch {
	case offer.Refresher == refresherUAC:
		st.weRefresh = false
	case offer.Refresher == refresherUAS:
		st.weRefresh = true
	case !supportsTimer(req):
		// RFC 4028 section 9: a UAC that does not support the timer cannot
		// refresh, so we do.
		st.weRefresh = true
	}
	refresher := refresherUAS
	if !st.weRefresh {
		refresher = refresherUAC
	}
	hdrs := []sip.Header{
		sip.NewHeader("Session-Expires", secondsOf(st.expires)+";refresher="+refresher),
		sip.NewHeader("Supported", "timer"),
	}
	if refresher == refresherUAC {
		hdrs = append(hdrs, sip.NewHeader("Require", "timer"))
	}
	t.setLocked(st)
	return hdrs
}

// refreshOutcome is what a session refresh leaves the timer to do.
type refreshOutcome int

const (
	// refreshGoesOn: the next refresh is due a cadence later.
	refreshGoesOn refreshOutcome = iota
	// refreshStopped: no refresh is sent until the state changes, and the
	// peer's own timer decides (RFC 4028 section 10).
	refreshStopped
	// refreshEnded: the dialog is ending.
	refreshEnded
)

// run keeps the session timer until ctx ends. While we refresh it calls
// refresh a refresh cadence after the last change, and refresh says what
// follows; while the peer refreshes it calls expire once the session interval
// plus the policy's grace passes with no change, and returns. A change of
// state restarts the wait, and one after a stopped refresh resumes refreshing.
// It closes done when it returns.
func (t *sessionTimer) run(ctx context.Context, refresh func(context.Context) refreshOutcome, expire func(context.Context)) {
	defer close(t.done)
	refreshing := true
	for {
		select {
		case <-t.changed:
			refreshing = true
		default:
		}
		st := t.snapshot()

		var due time.Duration
		switch {
		case !st.active || (st.weRefresh && !refreshing):
			// Nothing is due until the state changes.
		case st.weRefresh:
			due = t.policy.wall(refreshCadence(st.expires))
		default:
			due = t.policy.wall(st.expires + watchdogGrace(t.policy))
		}

		switch t.wait(ctx, due) {
		case wakeDone:
			return
		case wakeChanged:
			refreshing = true
			continue
		case wakeDue:
		}

		if !st.weRefresh {
			expire(ctx)
			return
		}
		switch refresh(ctx) {
		case refreshGoesOn:
		case refreshStopped:
			refreshing = false
		case refreshEnded:
			return
		}
	}
}

// wakeReason is why wait returned.
type wakeReason int

const (
	wakeDue wakeReason = iota
	wakeChanged
	wakeDone
)

// wait waits until due has passed, the state changes or ctx ends. A zero due
// waits for the change or the end only.
func (t *sessionTimer) wait(ctx context.Context, due time.Duration) wakeReason {
	var fire <-chan time.Time
	if due > 0 {
		timer := time.NewTimer(due)
		defer timer.Stop()
		fire = timer.C
	}
	select {
	case <-ctx.Done():
		return wakeDone
	case <-t.changed:
		return wakeChanged
	case <-fire:
		return wakeDue
	}
}
