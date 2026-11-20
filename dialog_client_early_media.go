// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emiago/sipgo"
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
// carries is not applied, and a final that carries none is answered. The UAC
// "MUST ignore any session descriptions in subsequent responses to the
// initial INVITE" (RFC 3261 section 13.2.1), and one that differs from the
// first is taken as equal to it, the UAC not switching to it (RFC 6337
// section 3.1.1): the call keeps the codecs, the remote address and the keys
// of the 183, and the handshake the 183 started, when it was left running.
//
// That holds within one early dialog, and so when final has the To tag of
// the 183 (RFC 3261 section 12.1). A 183 without one created no early dialog,
// and the answer of a 2xx of another early dialog is that dialog's own, see
// dialogAnswer.
func (s *sessionState) earlyAnswerStands(final *sip.Response) bool {
	tag := toTag(final)
	if tag == "" {
		return false
	}
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	return s.earlyAnswerApplied && s.earlyAnswerTag == tag
}

// noteDialogAnswer records the session description res carries as the answer
// of the early dialog res belongs to, when res is a provisional response to
// our INVITE with a To tag and that dialog has none yet: the first session
// description the UAC receives is the answer (RFC 3261 section 13.2.1), and
// each early dialog, which a provisional response with a To tag creates
// (section 12.1), has its own. A provisional response without a To tag
// creates no dialog, and its description is no dialog's answer. A non-2xx
// final response ends every early dialog of the INVITE (section 12.3), see
// endEarlyDialogs.
func (s *sessionState) noteDialogAnswer(res *sip.Response) {
	if res.StatusCode >= 300 {
		s.endEarlyDialogs()
		return
	}
	if res.StatusCode <= sip.StatusTrying || res.StatusCode >= 200 {
		return
	}
	body := sessionDescription(res)
	tag := toTag(res)
	if body == nil || tag == "" {
		return
	}
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	if _, ok := s.earlyDialogAnswers[tag]; ok {
		return
	}
	if s.earlyDialogAnswers == nil {
		s.earlyDialogAnswers = make(map[string][]byte)
	}
	s.earlyDialogAnswers[tag] = bytes.Clone(body)
}

// endEarlyDialogs forgets every early dialog of our INVITE, at a non-2xx final
// response to it, which ends them all (RFC 3261 section 12.3): the answer each
// gave our offer, and which of them gave the answer applied to the call's
// media. An INVITE sent again, as after a digest challenge (section 22.2), is
// a new request, whose responses form new early dialogs: the first session
// description one of them carries is its answer (section 13.2.1), also when
// its To tag is that of an ended one.
//
// What the media took from the ended early dialogs is stopped by the media
// itself, see endEarlyMedia.
func (s *sessionState) endEarlyDialogs() {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	s.earlyDialogAnswers = nil
	s.earlyAnswerApplied = false
	s.earlyAnswerTag = ""
}

// dialogAnswer returns the answer to our offer of the dialog res, a response
// to our INVITE, belongs to: the first session description a provisional
// response of that early dialog carried, see noteDialogAnswer, and otherwise
// the body of res. A 2xx of an early dialog that has an answer is answered by
// it, whatever the 2xx carries (RFC 3261 section 13.2.1).
func (s *sessionState) dialogAnswer(res *sip.Response) []byte {
	if tag := toTag(res); tag != "" {
		s.answeringMu.Lock()
		answer, ok := s.earlyDialogAnswers[tag]
		s.answeringMu.Unlock()
		if ok {
			return answer
		}
	}
	return res.Body()
}

// sessionDescription returns the session description res carries, and nil
// when it carries none.
func sessionDescription(res *sip.Response) []byte {
	contentType := res.ContentType()
	if contentType == nil || contentType.Value() != "application/sdp" {
		return nil
	}
	if len(res.Body()) == 0 {
		return nil
	}
	return res.Body()
}

// awaitInviteAnswer waits for the final response to our INVITE with wait, as
// opts asks, and records the answer each early dialog gives our offer on the
// way, see noteDialogAnswer. At a non-2xx final response, which ends every
// early dialog, keyer, the media of the INVITE, stops the media it took from
// them before anything else is done with that response, the INVITE sent again
// after a challenge included: see endEarlyMedia.
//
// Media that fails to stop ends the wait at that response, which is returned
// as the *sipgo.ErrDialogResponse the wait returns for a non-2xx final,
// joined with the media's error: the error of OnResponse replaces the one the
// wait would have returned, and the caller still learns how the INVITE was
// answered. A challenge is then not answered, as the INVITE sent again would
// find the media of the ended dialogs still running.
func (d *DialogClientSession) awaitInviteAnswer(ctx context.Context, keyer mediaEarlyKeyer, opts sipgo.AnswerOptions, wait inviteAnswerWait) error {
	onResponse := opts.OnResponse
	opts.OnResponse = func(res *sip.Response) error {
		d.noteDialogAnswer(res)
		if res.StatusCode >= 300 {
			if err := d.endEarlyMedia(keyer); err != nil {
				return errors.Join(&sipgo.ErrDialogResponse{Res: res}, err)
			}
		}
		if onResponse == nil {
			return nil
		}
		return onResponse(res)
	}
	return wait(ctx, opts)
}

// endEarlyMedia has keyer, the media of our INVITE, stop what it took from
// the early dialogs a non-2xx final response ended (RFC 3261 section 12.3):
// nothing of an ended dialog's media runs after it. The INVITE sent again
// after a challenge carries the same offer, and a 183 to it is taken as early
// media again, as the first one was.
func (d *DialogClientSession) endEarlyMedia(keyer mediaEarlyKeyer) error {
	if err := keyer.endEarlyMedia(); err != nil {
		return fmt.Errorf("end the media of the ended early dialogs: %w", err)
	}
	// A handshake left running was ended with the rest.
	d.forgetEarlyKeyingLeft()
	return nil
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
