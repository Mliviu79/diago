// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// WithSessionTimers sets the RFC 4028 session-timer policy stamped onto each
// dialog at construction. The policy drives negotiation, the refresh loop and
// the peer-refresh watchdog. Without this option session timers stay disabled
// and diago neither advertises nor enforces them.
func WithSessionTimers(p SessionTimerPolicy) DiagoOption {
	return func(dg *Diago) {
		dg.sessionTimers = p
	}
}

// statusSessionIntervalTooSmall is the RFC 4028 §6 rejection for an offered
// Session-Expires below the Min-SE floor. sipgo carries no constant for it.
const statusSessionIntervalTooSmall = 422

// errSessionIntervalTooSmall aborts the answer when the offered Session-Expires
// is below the Min-SE floor. A 422 has already been sent to the peer.
var errSessionIntervalTooSmall = errors.New("session interval too small")

// timerAnswer is the negotiated RFC 4028 outcome for a single answer: the
// headers to append to the 200 OK and the session timer it starts. It is
// computed before RespondSDP and consumed once the 200 OK is sent.
type timerAnswer struct {
	headers   []sip.Header
	weRefresh bool
	expiry    time.Duration
	// minSE is the Min-SE the INVITE carried, zero when it carried none.
	minSE time.Duration
}

// buildTimerAnswer is the pure UAS answer-path decision for RFC 4028 session
// timers. Given the offered INVITE and our policy it returns the headers to add
// to the 200 OK (or the Min-SE header for a 422) and the response status. It
// keeps every SIP-transaction dependency out so the decision is unit testable
// without a PBX.
//
//   - timers disabled, or the peer offered no Session-Expires (timer-unaware) →
//     no headers, status 200, nil answer: a graceful no-op, never a rejection.
//   - offered Session-Expires below the floor → status 422 plus Min-SE, nil
//     answer, so no media, no loop and no watchdog.
//   - otherwise → Session-Expires with the honored refresher plus Supported:
//     timer, and Require: timer when the refresher is uac (RFC 4028 section 9:
//     the UAC must process the 2xx to know it refreshes), and an answer that
//     either refreshes or arms the watchdog.
func buildTimerAnswer(invite *sip.Request, policy SessionTimerPolicy) (ta *timerAnswer, hdrs []sip.Header, status int) {
	if !policy.Enabled {
		return nil, nil, sip.StatusOK
	}

	offer, ok := parseSessionTimerOffer(invite)
	if !ok {
		// Timer-unaware peer: answer normally with no timer headers, start nothing.
		return nil, nil, sip.StatusOK
	}

	dec := negotiate(offer.SE, offer.MinSE, offer.Refresher, policy, refresherUAS)

	if dec.BelowFloor {
		// RFC 4028 §6: reject below-floor and echo our Min-SE, establishing nothing.
		return nil, []sip.Header{sip.NewHeader("Min-SE", secondsOf(dec.Negotiated))}, statusSessionIntervalTooSmall
	}

	seValue := secondsOf(dec.Negotiated)
	if dec.Refresher != "" {
		seValue += ";refresher=" + dec.Refresher
	}
	hdrs = []sip.Header{
		sip.NewHeader("Session-Expires", seValue),
		sip.NewHeader("Supported", "timer"),
	}
	if dec.Refresher == refresherUAC {
		hdrs = append(hdrs, sip.NewHeader("Require", "timer"))
	}
	// We refresh when we are the elected refresher, otherwise the peer refreshes
	// and the timer ends a session the peer stops refreshing.
	ta = &timerAnswer{
		headers:   hdrs,
		weRefresh: dec.WeRefresh,
		expiry:    dec.Negotiated,
	}
	if offer.MinSE != nil {
		ta.minSE = time.Duration(*offer.MinSE) * time.Second
	}
	return ta, hdrs, sip.StatusOK
}

// setTimerAnswer keeps ta, the session timer an answer negotiated, for the
// 2xx of that answer.
func (s *sessionState) setTimerAnswer(ta *timerAnswer) {
	s.answeringMu.Lock()
	s.timerAnswer = ta
	s.answeringMu.Unlock()
}

// negotiatedTimerAnswer returns the session timer the answer negotiated, nil
// when it negotiated none.
func (s *sessionState) negotiatedTimerAnswer() *timerAnswer {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	return s.timerAnswer
}

// prepareSessionTimers negotiates RFC 4028 session timers for the answer. On a
// below-floor offer it sends 422 with Min-SE and returns
// errSessionIntervalTooSmall so the caller aborts before any media, loop or
// watchdog; the 422 waits for its ACK within ctx, see answerWithin. Otherwise
// it stashes the timer headers, which respondSDP appends to the 200 OK, along
// with the launch parameters. A timer-unaware peer or a disabled policy
// leaves the timer answer nil, a graceful no-op.
func (d *DialogServerSession) prepareSessionTimers(ctx context.Context) error {
	ta, hdrs, status := buildTimerAnswer(d.InviteRequest, d.sessionTimers)
	if status == statusSessionIntervalTooSmall {
		err := d.answerWithin(ctx, make(chan struct{}), func() error {
			return d.Respond(d.Context(), statusSessionIntervalTooSmall, "Session Interval Too Small", nil, hdrs...)
		})
		if err != nil {
			return err
		}
		return errSessionIntervalTooSmall
	}
	if ta == nil {
		return nil
	}
	d.setTimerAnswer(ta)
	return nil
}

// launchSessionTimers starts the RFC 4028 timer the 2xx that answered the
// INVITE set, exactly once. Its goroutine runs on d.Context(), so the end of
// the dialog ends it: it refreshes the session while we are the refresher and
// ends the call when the peer is and lets the session expire.
func (d *DialogServerSession) launchSessionTimers(ta *timerAnswer) {
	t := newSessionTimer(d.sessionTimers, sessionTimerState{
		active:    true,
		expires:   ta.expiry,
		weRefresh: ta.weRefresh,
		minSE:     ta.minSE,
	})
	if !d.timer.CompareAndSwap(nil, t) {
		return
	}
	go t.run(d.Context(), func(ctx context.Context) refreshOutcome {
		return d.refreshSession(ctx, t)
	}, d.expireSession)
}

// expireSession ends the call whose peer let the session expire without a
// refresh (RFC 4028 section 10). Our BYE ends ctx, the dialog's context, once
// it is sent and its answer is awaited past that end, so a failure is logged
// whatever ctx's state; only ctx's cancellation, which means the dialog ended
// before our BYE went out, is not a failure of ours.
func (d *DialogServerSession) expireSession(ctx context.Context) {
	d.logger().Warn("The peer did not refresh the session, ending the call", "call_id", d.callID())
	if err := d.Hangup(ctx); err != nil && !errors.Is(err, context.Canceled) {
		d.logger().Warn("Ending the call whose session expired failed", "call_id", d.callID(), "error", err)
	}
}

// reInviteSessionInPlace sends the re-INVITE that refreshes the session: one
// of ours as ReInvite sends it, but for its offer, which is the media's of the
// session in place and so asks the peer to change nothing, whatever a plain
// ReInvite is configured to offer. RFC 4028 section 7.4 has a re-INVITE that
// refreshes the session carry an offer, and that offer indicate that the
// session has not changed.
func (d *DialogServerSession) reInviteSessionInPlace(ctx context.Context) error {
	handshaker := d.mediaHanshakerLocked()
	if handshaker == nil {
		return fmt.Errorf("dialog media is not initialized")
	}
	r := d.ownReInvite(handshaker, "", d.reInviteSend)
	r.inPlace = true
	return r.run(ctx)
}

// refreshSession sends one session refresh, a re-INVITE offering the media
// session as it is, and acts on its outcome as RFC 4028 section 10 sets out: a
// 408 or 481, or a transaction that timed out, ends the call with our BYE; a
// 422 is retried once asking for its Min-SE (section 7.3); a 500 or 503 with
// Retry-After is retried once after it; a 491 is retried by the re-INVITE
// itself; a 2xx without SDP keeps the running session and refreshing goes on;
// any other failure stops refreshing, which leaves the session to the peer's
// own timer. Every failure is logged.
func (d *DialogServerSession) refreshSession(ctx context.Context, t *sessionTimer) refreshOutcome {
	log := d.logger()
	retried := false
	for {
		err := d.reInviteSessionInPlace(ctx)
		if err == nil {
			return refreshGoesOn
		}
		if ctx.Err() != nil {
			return refreshEnded
		}
		if errors.Is(err, errReInviteAnswerWithoutSDP) {
			log.Warn("Session refresh answered without SDP, keeping the media session", "call_id", d.callID())
			return refreshGoesOn
		}
		if errors.Is(err, errMediaClosed) || errors.Is(err, errMediaUpdateAfterAnswer) {
			// The call is ending: the media is closed, the dialog has ended,
			// or the answer could not be keyed and the call was hung up.
			log.Warn("Session refresh failed, the call is ending", "call_id", d.callID(), "error", err)
			return refreshEnded
		}

		status := 0
		var resErr sipgo.ErrDialogResponse
		if errors.As(err, &resErr) && resErr.Res != nil {
			status = resErr.Res.StatusCode
		}
		switch {
		case status == sip.StatusRequestTimeout || status == sip.StatusCallTransactionDoesNotExists || errors.Is(err, sip.ErrTransactionTimeout):
			log.Warn("Session refresh failed, ending the call", "call_id", d.callID(), "status", status, "error", err)
			if herr := d.hangupNoMedia(); herr != nil {
				log.Warn("Ending the call after a failed session refresh failed", "call_id", d.callID(), "error", herr)
			}
			return refreshEnded
		case status == statusSessionIntervalTooSmall && !retried:
			minSE, ok := parseMinSE(resErr.Res)
			if !ok {
				break
			}
			retried = true
			t.raiseMinSE(time.Duration(minSE) * time.Second)
			log.Warn("Session refresh answered 422, retrying with its Min-SE", "call_id", d.callID(), "min_se", minSE)
			continue
		case (status == sip.StatusInternalServerError || status == sip.StatusServiceUnavailable) && !retried:
			after, ok := retryAfter(resErr.Res)
			if !ok {
				break
			}
			retried = true
			log.Warn("Session refresh failed, retrying after Retry-After", "call_id", d.callID(), "status", status, "retry_after", after)
			timer := time.NewTimer(d.sessionTimers.wall(after))
			select {
			case <-ctx.Done():
				timer.Stop()
				return refreshEnded
			case <-timer.C:
			}
			continue
		}
		log.Warn("Session refresh failed, refreshing stopped", "call_id", d.callID(), "status", status, "error", err)
		return refreshStopped
	}
}
