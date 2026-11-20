// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"time"

	"github.com/emiago/sipgo/sip"
)

// noteEarlyAnswerTaken records that the answer in res, a 183 to our INVITE,
// was applied to the call's media: it is the answer to our offer (RFC 3261
// section 13.2.1), and no later session description in a response of the
// same early dialog is.
func (s *sessionState) noteEarlyAnswerTaken(res *sip.Response) {
	tag := toTag(res)
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	s.earlyAnswerApplied = true
	s.earlyAnswerTag = tag
}

// earlyAnswerTaken reports whether the answer in a 183 to our INVITE was
// applied to the call's media.
func (s *sessionState) earlyAnswerTaken() bool {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	return s.earlyAnswerApplied
}

// earlyAnswerStands reports whether the answer a 183 gave the call's media
// stands at final, the 2xx to our INVITE, so that a session description final
// carries is not applied. The UAC "MUST ignore any session descriptions in
// subsequent responses to the initial INVITE" (RFC 3261 section 13.2.1), and
// one that differs from the first is taken as equal to it, the UAC not
// switching to it (RFC 6337 section 3.1.1): the call keeps the codecs, the
// remote address and the keys of the 183. A 2xx of another early dialog than
// that 183, one with another To tag, is applied to the media as before.
// While the handshake the 183 started was left running, the answer of the
// 183 stands whatever the 2xx.
func (s *sessionState) earlyAnswerStands(final *sip.Response) bool {
	tag := toTag(final)
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	if s.earlyUnkeyed {
		return true
	}
	return s.earlyAnswerApplied && s.earlyAnswerTag == tag
}

// toTag returns the tag of res's To header, empty when it has none.
func toTag(res *sip.Response) string {
	to := res.To()
	if to == nil {
		return ""
	}
	if tag, ok := to.Params.Get("tag"); ok {
		return tag
	}
	return ""
}

// keyEarlyAnswer has keyer, the media the answer in a 183 was just applied
// to, keyed by the callee, and reports whether it is: the call has early
// media then, and none when it reports false without an error.
//
// The callee may take part in the handshake only once the call is answered,
// as a callee that ignores early media does (RFC 3960), so the wait for the
// keys is bounded by wait, five seconds when it is zero, and the handshake is
// not ended with it: it runs until the call ends, and Ack completes it.
// Meanwhile the media sends nothing and has no audio set up, the answer of the
// 183 stands, and no later session description is applied.
//
// The wait also ends with ctx, which the wait for the INVITE's answer it runs
// in cannot see end while it runs, and the error is then ctx's cause.
func (d *DialogClientSession) keyEarlyAnswer(ctx context.Context, keyer mediaEarlyKeyer, wait time.Duration) (keyed bool, err error) {
	if wait <= 0 {
		wait = defaultEarlyMediaKeyTimeout
	}
	err = d.awaitEarlyKeys(ctx, keyer, wait)
	if errors.Is(err, errMediaKeyingInProgress) {
		d.noteEarlyKeyingLeft()
		return false, nil
	}
	return err == nil, err
}

// awaitEarlyKeys has keyer run the negotiation the answer in a 183 left it,
// and waits for it up to wait, or until ctx ends. The negotiation is bounded
// by the dialog only, and a wait that ends before it does returns
// errMediaKeyingInProgress and leaves it running.
func (d *DialogClientSession) awaitEarlyKeys(ctx context.Context, keyer mediaEarlyKeyer, wait time.Duration) error {
	// Checked in steps, as the keyer's own wait is not bounded by ctx: the
	// ctx it takes is the one the negotiation is left running under.
	const step = 50 * time.Millisecond
	deadline := time.Now().Add(wait)
	for {
		err := keyer.onFinalizeWithin(d.Context(), min(time.Until(deadline), step))
		if !errors.Is(err, errMediaKeyingInProgress) {
			return err
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if time.Until(deadline) <= 0 {
			return errMediaKeyingInProgress
		}
	}
}
