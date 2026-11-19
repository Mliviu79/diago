// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"slices"
	"time"

	"github.com/emiago/sipgo/sip"
)

// referAttempt is one REFER sent on the dialog, the refer subscription it
// created, and what has been observed for it. Every field but unobserved is
// guarded by sessionState.referMu.
//
// The attempt lives as long as its subscription (RFC 6665 section 4.1.3; RFC
// 3515 section 2.4.4), and we end it ourselves when its deadline passes. The
// deadline is 64×T1 from the REFER's 2xx until a NOTIFY arrives. The first NOTIFY
// that does not end the subscription replaces it with that NOTIFY's duration,
// counted from its arrival (see referSubscriptionBound), at most
// referSubscriptionMaxBound. Each later NOTIFY may only bring the deadline
// earlier (RFC 6665 section 4.2.2), so the subscription ends at most 64×T1 plus
// referSubscriptionMaxBound after the 2xx. The attempt is registered before its
// REFER is sent and retired when it ends. A BYE on the dialog's INVITE usage does
// not end it (RFC 5057 section 5.5), so the close of the dialog's media leaves it
// registered. An attempt nobody observes has no deadline, see unobserved.
type referAttempt struct {
	// unobserved is true for the attempt of a REFER nobody waits on: one Refer
	// or ReferOptions sent while the dialog had no OnNotify callback. It holds
	// the REFER's subscription so the NOTIFYs of that REFER are told from the
	// NOTIFYs of none, and its final NOTIFY hangs the call up, which is what
	// those two calls do with a transfer outcome nobody takes. They do so
	// whenever that outcome comes, so such an attempt has no deadline: it ends
	// on the NOTIFY that ends its subscription and on nothing else. Set before
	// the attempt is registered and not changed after.
	unobserved bool
	// cseq is the CSeq of the sent REFER. RFC 3515 makes the NOTIFY's Event id
	// the CSeq of the REFER that created the subscription, so this is what an
	// id-carrying NOTIFY is matched against.
	cseq uint32
	// cseqKnown is false until setReferAttemptCSeq records the sent REFER's CSeq.
	// Until then an id-carrying NOTIFY cannot be told apart from this attempt's.
	cseqKnown bool
	// registered is true while the attempt is in referAttempts.
	registered bool
	// end is how the attempt ended, zero while it is open.
	end ReferEnd
	// notifies are the NOTIFYs recorded while the attempt was open, at most
	// maxReferNotifies, and dropped counts the ones not kept.
	notifies []ReferNotify
	dropped  int
	// notified is true once a NOTIFY was recorded, so the deadline passing ends
	// the attempt as ReferEndSubscriptionExpired rather than ReferEndNoNotify.
	notified bool
	// deadline is the instant the subscription ends on our side. It is 64×T1 after
	// the REFER's 2xx until a NOTIFY arrives; the first NOTIFY that does not end
	// the subscription sets it to that NOTIFY's duration from its arrival, capped
	// at referSubscriptionMaxBound; a later NOTIFY can only move it earlier. Zero
	// until the subscription begins.
	deadline time.Time
	// bound is the timer that ends the attempt at deadline. Nil until the
	// subscription begins; stopped when the attempt ends. boundArmed counts the
	// times it was armed, so a timer that fired as it was being replaced ends
	// nothing.
	bound      *time.Timer
	boundArmed uint64
	// done is closed when the attempt is retired: ended on a NOTIFY, after the
	// 200 to it has been sent, or at its deadline.
	done chan struct{}
}

// beginReferAttempt registers a new REFER attempt as the most recent one and
// returns it. Called before the REFER is sent so a NOTIFY that races ahead of
// the REFER's response is recorded rather than lost.
func (s *sessionState) beginReferAttempt() *referAttempt {
	return s.registerReferAttempt(&referAttempt{})
}

// beginUnobservedReferAttempt registers, as beginReferAttempt does, the attempt
// of a REFER nobody waits on, see referAttempt.unobserved. Nothing finishes
// such an attempt and no deadline ends it: it is retired on the NOTIFY that
// ends its subscription, or dropped when its REFER was not accepted.
func (s *sessionState) beginUnobservedReferAttempt() *referAttempt {
	return s.registerReferAttempt(&referAttempt{unobserved: true})
}

// registerReferAttempt registers a, a new attempt, as the most recent one and
// returns it.
func (s *sessionState) registerReferAttempt(a *referAttempt) *referAttempt {
	a.registered = true
	a.done = make(chan struct{})
	s.referMu.Lock()
	s.referAttempts = append(s.referAttempts, a)
	s.referLatest = a
	s.referMu.Unlock()
	return a
}

// setReferAttemptCSeq records the sent REFER's CSeq on the dialog's high-water
// mark and on a, whatever a's state: a NOTIFY that raced ahead of the REFER's
// response may already have ended and retired a, and the CSeq still belongs to
// what a observed. Routing reads registered attempts only, so a retired one's
// CSeq takes no NOTIFY.
func (s *sessionState) setReferAttemptCSeq(a *referAttempt, cseq uint32) {
	s.referMu.Lock()
	if cseq > s.referHighestCSeq {
		s.referHighestCSeq = cseq
	}
	a.cseq = cseq
	a.cseqKnown = true
	s.referMu.Unlock()
}

// startReferSubscription begins a's subscription at its REFER's 2xx (RFC 6665
// section 8.3.1, which RFC 7647 section 5 applies to a REFER): Timer N, 64×T1,
// bounds the wait for its first NOTIFY (section 4.1.2.4). A NOTIFY that raced
// ahead of the 2xx has already begun it (RFC 3515 section 2.4.4), and its
// deadline stands.
func (s *sessionState) startReferSubscription(a *referAttempt) {
	s.referMu.Lock()
	defer s.referMu.Unlock()
	if a.end != 0 || a.bound != nil {
		return
	}
	s.armReferBoundLocked(a, time.Now().Add(64*sip.T1))
}

// armReferBoundLocked sets a's deadline to deadline and arms its bound to end
// the attempt then, replacing the bound it had. Called with referMu held, on an
// open attempt.
func (s *sessionState) armReferBoundLocked(a *referAttempt, deadline time.Time) {
	if a.bound != nil {
		a.bound.Stop()
	}
	a.boundArmed++
	armed := a.boundArmed
	a.deadline = deadline
	a.bound = time.AfterFunc(time.Until(deadline), func() { s.endReferAttemptOnItsBound(a, armed) })
}

// endReferAttemptOnItsBound ends a when its deadline passes with the attempt
// still open and the bound still the one armed as armed.
func (s *sessionState) endReferAttemptOnItsBound(a *referAttempt, armed uint64) {
	s.referMu.Lock()
	defer s.referMu.Unlock()
	if a.end != 0 || a.boundArmed != armed {
		return
	}
	s.endReferAttemptOnItsBoundLocked(a)
}

// endReferAttemptOnItsBoundLocked ends the open attempt a whose deadline has
// passed: as expired when a NOTIFY set the deadline, else as no NOTIFY came
// within Timer N. Called with referMu held.
func (s *sessionState) endReferAttemptOnItsBoundLocked(a *referAttempt) {
	if a.notified {
		a.end = ReferEndSubscriptionExpired
	} else {
		a.end = ReferEndNoNotify
	}
	s.retireReferAttemptLocked(a)
}

// recordReferNotify routes one parsed NOTIFY to the open REFER attempt it
// belongs to and records it there, and returns that attempt, or nil when none
// holds it. With an Event id that is the registered attempt whose known CSeq
// equals it, or else the most recent attempt while its CSeq is still unknown,
// provided no other attempt awaits its CSeq and the id is above every REFER
// CSeq the dialog has recorded; without an id it is the most recent attempt.
//
// A NOTIFY that does not end the attempt sets its deadline. The first one sets
// it to its duration from now, which replaces the 64×T1 wait. A later one sets
// it to the earlier of its own duration from now and the deadline already set,
// so a NOTIFY can shorten the subscription but never lengthen it (RFC 6665
// section 4.2.2). A NOTIFY that arrives after the deadline passed is refused, as
// one after the end is: the attempt ends as the deadline's timer would have ended
// it, and nil is returned. An unobserved attempt takes no deadline.
//
// A NOTIFY that ends the attempt sets its end here, so neither its deadline nor
// its caller can end it another way; the attempt is retired by
// retireEndedReferAttempt once the NOTIFY has been answered.
func (s *sessionState) recordReferNotify(n ReferNotify) *referAttempt {
	s.referMu.Lock()
	defer s.referMu.Unlock()

	a := s.referNotifyTargetLocked(n)
	if a == nil || a.end != 0 {
		return nil
	}
	now := time.Now()
	if !a.deadline.IsZero() && !now.Before(a.deadline) {
		// The deadline has passed but its timer has not ended the attempt yet.
		s.endReferAttemptOnItsBoundLocked(a)
		return nil
	}
	first := !a.notified
	a.recordLocked(n)
	if a.end == 0 && !a.unobserved {
		deadline := now.Add(referSubscriptionBound(n))
		if !first && a.deadline.Before(deadline) {
			deadline = a.deadline
		}
		s.armReferBoundLocked(a, deadline)
	}
	return a
}

// retireEndedReferAttempt retires a if the NOTIFY just answered ended it, which
// RFC 6665 section 4.4.1 places after that NOTIFY's 200.
func (s *sessionState) retireEndedReferAttempt(a *referAttempt) {
	s.referMu.Lock()
	defer s.referMu.Unlock()
	if a.end != 0 {
		s.retireReferAttemptLocked(a)
	}
}

// referNotifyExpected reports whether a NOTIFY carrying req's Event header has
// a REFER sent on this dialog to go to: onNotify, the OnNotify callback of
// ReferOptions, when the dialog has one, or an open attempt it is routed to.
func (s *sessionState) referNotifyExpected(req *sip.Request, onNotify func(statusCode int)) bool {
	if onNotify != nil {
		return true
	}

	var n ReferNotify
	n.EventID, n.HasEventID = parseReferNotifyEventID(req)
	s.referMu.Lock()
	defer s.referMu.Unlock()
	a := s.referNotifyTargetLocked(n)
	return a != nil && a.end == 0
}

// referNotifyTargetLocked returns the registered attempt n belongs to, or nil.
// Called with referMu held.
func (s *sessionState) referNotifyTargetLocked(n ReferNotify) *referAttempt {
	if n.HasEventID {
		awaitingCSeq := 0
		for _, a := range s.referAttempts {
			if !a.cseqKnown {
				awaitingCSeq++
				continue
			}
			if a.cseq == n.EventID {
				return a
			}
		}
		// The NOTIFY may have raced ahead of the REFER's response, while the
		// most recent attempt's CSeq is not known yet. In-dialog CSeqs only
		// grow, so an id at or below the highest REFER CSeq recorded belongs to
		// an earlier REFER, even one already retired. With a second attempt
		// also awaiting its CSeq the id may be that attempt's, so it is given
		// to neither.
		if latest := s.referLatest; latest != nil && latest.registered && !latest.cseqKnown &&
			awaitingCSeq == 1 && n.EventID > s.referHighestCSeq {
			return latest
		}
		return nil
	}
	if latest := s.referLatest; latest != nil && latest.registered {
		return latest
	}
	return nil
}

// recordLocked records n on an open attempt, marks the subscription begun, and
// ends the attempt when n is terminal. Past maxReferNotifies a non-terminal
// NOTIFY is only counted, and a terminal one takes the last slot so it is
// always last. Called with referMu held.
func (a *referAttempt) recordLocked(n ReferNotify) {
	switch {
	case len(a.notifies) < maxReferNotifies:
		a.notifies = append(a.notifies, n)
	case n.endsSubscription():
		a.notifies[len(a.notifies)-1] = n
		a.dropped++
	default:
		a.dropped++
	}
	a.notified = true

	switch {
	case n.isFinal():
		a.end = ReferEndFinalNotify
	case n.endsSubscription():
		a.end = ReferEndSubscriptionEnded
	}
}

// finishReferAttempt ends a with end unless a NOTIFY or its deadline already ended
// it, or with ReferEndRefused regardless, retires it, and returns what a
// observed. The caller giving the attempt up, ReferEndCancelled, retires it at
// once, so a NOTIFY for it after that is answered 481.
func (s *sessionState) finishReferAttempt(a *referAttempt, end ReferEnd) ReferObservation {
	s.referMu.Lock()
	defer s.referMu.Unlock()

	// A refused REFER created no subscription, so its response decides the end
	// even if a NOTIFY claiming an outcome raced ahead of it.
	if a.end == 0 || end == ReferEndRefused {
		a.end = end
	}
	s.retireReferAttemptLocked(a)
	return ReferObservation{
		End:             a.end,
		CSeq:            a.cseq,
		CSeqKnown:       a.cseqKnown,
		Notifies:        slices.Clone(a.notifies),
		NotifiesDropped: a.dropped,
	}
}

// dropReferAttempt retires a, whose REFER could not be carried. Dropping an
// attempt never affects another.
func (s *sessionState) dropReferAttempt(a *referAttempt) {
	s.referMu.Lock()
	s.retireReferAttemptLocked(a)
	s.referMu.Unlock()
}

// retireReferAttemptLocked stops a's bound, unregisters it, leaving referLatest
// as it is, and closes its done once. Called with referMu held.
func (s *sessionState) retireReferAttemptLocked(a *referAttempt) {
	if a.bound != nil {
		a.bound.Stop()
	}
	if !a.registered {
		return
	}
	a.registered = false
	s.referAttempts = slices.DeleteFunc(s.referAttempts, func(r *referAttempt) bool { return r == a })
	close(a.done)
}

// OnReferNotifyUnmatched sets f as the function told of a refer NOTIFY that no
// open REFER attempt of this dialog holds: one whose subscription has ended,
// one for a REFER its caller gave up, or one for no REFER sent here. diago
// answers such a NOTIFY 481 (RFC 6665 section 4.1.3) and never applies it; f
// then receives it with its sipfrag status, once per NOTIFY, on the SIP
// transaction goroutine and outside every lock, on a live dialog and on an
// ended one still held. f must not block. Each call replaces the function set
// before; nil, the default, reports nothing.
//
// A dialog that carries the OnNotify callback of ReferOptions hands that
// callback every refer NOTIFY, so none is unmatched on it.
//
// Both DialogServerSession and DialogClientSession have this method.
func (s *sessionState) OnReferNotifyUnmatched(f func(ReferNotify)) {
	s.referMu.Lock()
	s.onReferNotifyUnmatched = f
	s.referMu.Unlock()
}

// reportUnmatchedReferNotify hands n to the function OnReferNotifyUnmatched
// set, if any. Called with no lock held.
func (s *sessionState) reportUnmatchedReferNotify(n ReferNotify) {
	s.referMu.Lock()
	f := s.onReferNotifyUnmatched
	s.referMu.Unlock()
	if f != nil {
		f(n)
	}
}
