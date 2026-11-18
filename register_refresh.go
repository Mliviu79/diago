// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

// registerRefreshRatio is the fraction of the granted expiry at which the
// binding is refreshed. Refreshing at 3/4 of the grant leaves a quarter of the
// lifetime for a refresh to fail and be retried before the binding lapses.
const registerRefreshRatio = 0.75

// registerRetryMin is the shortest refresh interval, so an absent or nonsense
// grant cannot busy loop the REGISTER path.
const registerRetryMin = 30 * time.Second

const (
	// registerRetryBackoffInitial is the wait after the first failed refresh.
	// Short, because most refresh failures are a blip and the binding is still
	// good for the remaining quarter of its grant.
	registerRetryBackoffInitial = 1 * time.Second
	// registerRetryBackoffMax caps the doubling so a long registrar outage
	// settles into a steady probe rather than an ever widening one.
	registerRetryBackoffMax = 5 * time.Minute
)

// recordGrant stores the expiry the registrar granted on a successful REGISTER
// and rearms the expired latch, since the binding is demonstrably alive.
func (t *RegisterTransaction) recordGrant(granted time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expiry = granted
	t.grantedExpires = granted
	t.lastRegistered = time.Now()
	t.expiredEmitted = false
}

// setExpiry overrides only the refresh cadence, leaving the recorded grant intact.
func (t *RegisterTransaction) setExpiry(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.expiry = d
}

// getExpiry reads the current refresh cadence input.
func (t *RegisterTransaction) getExpiry() time.Duration {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.expiry
}

// isDigestChallenge reports whether a 401/407 is an actual challenge, meaning it
// carries the matching WWW-Authenticate / Proxy-Authenticate header, rather than
// a plain rejection.
//
// A registrar is free to refuse an account with a challenge-less 401. Running
// digest against an absent challenge makes sipgo return a bare "No
// WWW-Authenticate header present" that replaces the final response, so the
// caller sees neither the status code nor the disposition. Falling through
// instead yields a RegisterResponseError carrying the real response, which is
// what a status code classifier needs.
func isDigestChallenge(res *sip.Response) bool {
	switch res.StatusCode {
	case sip.StatusUnauthorized:
		return res.GetHeader("WWW-Authenticate") != nil
	case sip.StatusProxyAuthRequired:
		return res.GetHeader("Proxy-Authenticate") != nil
	default:
		return false
	}
}

// grantedExpiry resolves a 200 OK's granted lifetime, defaulting to requested.
//
// RFC 3261 section 10.3 lets the registrar answer with either a per-Contact
// expires param or a top level Expires header. The per-Contact param is the
// binding's own lifetime, so it wins. Only a positive grant counts: expires=0 is
// a de-registration echo, not a lifetime.
func grantedExpiry(res *sip.Response, requested time.Duration) time.Duration {
	if c := res.Contact(); c != nil {
		if v, ok := c.Params.Get("expires"); ok {
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
				return time.Duration(n) * time.Second
			}
		}
	}

	if h := res.GetHeader("Expires"); h != nil {
		if n, err := strconv.Atoi(strings.TrimSpace(h.Value())); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}

	return requested
}

// registerRefresher is the half of the refresh loop that talks to the network,
// split out so the tolerance contract below is testable without a registrar.
// RegisterTransaction is the only production implementation.
type registerRefresher interface {
	// refresh sends one REGISTER refresh.
	refresh(ctx context.Context) error
	// cadence reports the wait after a successful refresh, derived from the
	// expiry the registrar granted.
	cadence() time.Duration
	// expired reports whether the binding is provably gone, meaning the last
	// confirmed registration's granted lifetime has run out. It latches, so a
	// lapsed binding escalates once per outage.
	expired() bool
}

// registerRefreshLoop keeps a binding alive, tolerating transient refresh
// failures instead of surrendering the whole registration to the first one.
//
// It used to return on the first failed Qualify, so one 503 or one network blip
// tore the registration down even though the binding the registrar holds is
// still valid for the rest of its granted lifetime and the refresh would have
// succeeded seconds later. The failure arm now backs off exponentially and
// retries. Escalation is reserved for the case where retrying is provably
// pointless: expired() reports the granted lifetime has run out, so the
// registrar has certainly dropped us. Cancellation is a clean shutdown and is
// surfaced as the context error, never as a failure.
//
// The backoff bounds are parameters rather than the constants directly so the
// tolerance contract is testable at millisecond scale.
func registerRefreshLoop(ctx context.Context, retry, initialBackoff, maxBackoff time.Duration, r registerRefresher) error {
	timer := time.NewTimer(retry)
	defer timer.Stop()

	backoff := initialBackoff
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		}

		err := r.refresh(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if err != nil {
			if r.expired() {
				// The binding outlived its grant: no amount of further retrying
				// brings it back, so hand the error up.
				return err
			}
			timer.Reset(backoff)
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
			continue
		}

		backoff = initialBackoff
		timer.Reset(r.cadence())
	}
}

// refresh sends one REGISTER refresh, logging a failure that the loop will
// absorb so a transient outage is still visible.
func (t *RegisterTransaction) refresh(ctx context.Context) error {
	expiry := t.getExpiry()
	if err := t.Qualify(ctx); err != nil {
		t.log.Warn("Register refresh failed, will retry", "error", err, "expiry", expiry)
		return err
	}

	if next := t.getExpiry(); next != expiry {
		t.log.Info("Register expiry changed", "expiry_old", expiry, "expiry_new", next, "retry", t.calcRetry(next))
	}
	return nil
}

// cadence reports the post success wait, recomputed each time so a registrar
// that shortens the grant mid registration takes effect on the next refresh.
func (t *RegisterTransaction) cadence() time.Duration {
	return t.calcRetry(t.getExpiry())
}

// expired reports whether the last confirmed registration's granted lifetime has
// run out, latching so one outage escalates once. A transaction that never
// registered successfully has no binding to lose and never reports expired: the
// initial Register's own error already covers that case.
func (t *RegisterTransaction) expired() bool {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.expiredEmitted || t.lastRegistered.IsZero() || t.grantedExpires <= 0 {
		return false
	}
	if !time.Now().After(t.lastRegistered.Add(t.grantedExpires)) {
		return false
	}
	t.expiredEmitted = true
	return true
}
