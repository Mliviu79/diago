// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/emiago/sipgo/sip"
)

// errNoOfferInInvite refuses to answer, with an answer, an INVITE that
// carried no offer. AnswerLateContext answers such an INVITE.
var errNoOfferInInvite = errors.New("no sdp present in INVITE")

// AnswerContext is Answer without options, whose wait ends with ctx. It
// returns the media of the call once the ACK to its 2xx is read and the media
// started, with the reason they were not, or, when ctx ends first while the
// dialog lives, at that end with an error wrapping context.Cause(ctx). The
// dialog's end ends the answer's exchange too, which then returns why it was
// not confirmed. A ctx that has ended before the 2xx is sent leaves the INVITE
// unanswered.
//
// ctx bounds the wait and nothing else: the media runs until it is closed,
// which the session's Close and the peer's BYE do. The media of an answer
// that returned an error is closed by the session too.
//
// A 2xx once sent is the INVITE's final response, and its exchange goes on
// without the caller: the dialog retransmits it until its ACK is read or 64*T1
// has passed (RFC 3261 section 13.3.1.4). Nobody is then left to use the
// answer, so the dialog ends the call itself with a BYE, which goes out once
// the ACK has confirmed the dialog, or once the ACK wait has passed without
// one, and never before (RFC 3261 section 15). The dialog has ended once that
// BYE is handed to its transaction. Close waits for all of it.
//
// A call ProgressMedia gave early media is answered with that media, which is
// returned: the 2xx repeats the answer of the 183, and the DTLS handshake
// ProgressMedia left running is completed or the call declined 488, as
// AnswerEarlyMedia describes. A refusal waits for its ACK within ctx too.
func (d *DialogServerSession) AnswerContext(ctx context.Context) (*DialogMedia, error) {
	if med, early, ok := d.earlyMedia(); ok {
		return d.answerEarlyMediaWithin(ctx, med, early)
	}
	return d.answerOfferWithin(ctx, d.mediaConf, nil)
}

// AnswerOptions is AnswerContext with the options of Answer. A call that has
// early media keeps the codecs and RTPNAT that media was made with.
func (d *DialogServerSession) AnswerOptions(ctx context.Context, opt AnswerOptions) (*DialogMedia, error) {
	d.dialogCallbacks.mu.Lock()
	d.onReferDialog = opt.OnRefer
	d.dialogCallbacks.mu.Unlock()

	if med, early, ok := d.earlyMedia(); ok {
		med.setOnMediaUpdate(opt.OnMediaUpdate)
		return d.answerEarlyMediaWithin(ctx, med, early)
	}

	conf := d.mediaConf
	conf.update(opt.Codecs, opt.RTPNAT)
	return d.answerOfferWithin(ctx, conf, opt.OnMediaUpdate)
}

// AnswerLateContext is AnswerLate, whose wait ends with ctx, as AnswerContext
// describes: when ctx ends first it returns at that end, and the dialog reads
// the ACK, hands the media the answer it carries and then ends the call with a
// BYE, or ends it with a BYE once the ACK wait has passed without one.
func (d *DialogServerSession) AnswerLateContext(ctx context.Context) (*DialogMedia, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("answer: %w", context.Cause(ctx))
	}
	// RFC 4028: as AnswerContext, negotiate session timers before the 2xx. A
	// below-floor offer is refused 422 here, and respondSDP adds the timer
	// headers to the 2xx and starts the timer once it is sent.
	if err := d.prepareSessionTimers(ctx); err != nil {
		return nil, err
	}

	// The media is the dialog's, not the wait's, so its port is chosen on the
	// dialog's ctx.
	med, err := newDialogMedia(d.Context(), d.mediaConf, nil)
	if err != nil {
		return nil, err
	}
	offer, err := med.onLocalSDP(ctx, false, "")
	if err != nil {
		// Not registered with the session yet, so nothing else closes it.
		return nil, errors.Join(err, med.Close())
	}
	med.registerDialogCallbacks(&d.dialogCallbacks)

	// Our 2xx carries the offer, so the ACK must carry the answer, which
	// ReadAck hands to the media.
	d.expectAnswerInAck()
	if err := d.respondSDPWithin(ctx, offer); err != nil {
		return nil, err
	}
	return med, nil
}

// answerOfferWithin answers the INVITE's offer with media built from conf,
// and waits within ctx as AnswerContext describes. onMediaUpdate, when not
// nil, is told of each later update of that media.
func (d *DialogServerSession) answerOfferWithin(ctx context.Context, conf MediaConfig, onMediaUpdate func(*DialogMedia)) (*DialogMedia, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("answer: %w", context.Cause(ctx))
	}
	// RFC 4028: negotiate session timers before the 200 OK. A below-floor offer is
	// rejected here with 422 and aborts the answer. The timer headers are stashed
	// for respondSDP, which launches the loop or watchdog once the 200 OK is sent.
	if err := d.prepareSessionTimers(ctx); err != nil {
		return nil, err
	}
	offer := d.InviteRequest.Body()
	if offer == nil {
		return nil, errNoOfferInInvite
	}

	// The media is the dialog's, not the wait's, so its port is chosen on the
	// dialog's ctx.
	med, err := newDialogMedia(d.Context(), conf, onMediaUpdate)
	if err != nil {
		return nil, err
	}
	answer, err := answerToOffer(ctx, med, offer)
	if err != nil {
		// Not registered with the session yet, so nothing else closes it.
		return nil, errors.Join(err, med.Close())
	}
	med.registerDialogCallbacks(&d.dialogCallbacks)

	if err := d.respondSDPWithin(ctx, answer); err != nil {
		return nil, err
	}
	return med, nil
}

// answerToOffer hands the media behind handshaker the offer of an INVITE and
// returns the answer the 2xx carries. The media commits the exchange when the
// ACK is read, see readInviteAck.
func answerToOffer(ctx context.Context, handshaker mediaHanshaker, offer []byte) ([]byte, error) {
	if err := handshaker.onRemoteSDP(ctx, offer, false); err != nil {
		return nil, err
	}
	return handshaker.onLocalSDP(ctx, true, "")
}

// respondSDPWithin answers the INVITE 200 with body and waits within ctx for
// the ACK, as answerWithin describes. The media registered with the session
// is committed by the reading of that ACK, before it confirms the dialog, so
// the answer is complete once the 2xx is acknowledged.
func (d *DialogServerSession) respondSDPWithin(ctx context.Context, body []byte) error {
	answering := d.markAnswering()
	return d.answerWithin(ctx, answering, func() error {
		return d.respondSDP(d.Context(), body)
	})
}

// answerWithin runs complete, the part of an answer from its final response
// on, a 2xx or a refusal, on a goroutine of the dialog's, and waits for it
// within ctx. answering is the answer's in-progress mark, which the goroutine
// closes once complete has returned, so in-dialog requests wait for the whole
// answer (awaitAnswer), and an ACK that leaves the call without usable media
// leaves the BYE to complete (readInviteAck). When ctx has ended already,
// complete does not run and no final response is sent.
//
// When ctx ends while complete runs and the dialog is live, answerWithin
// returns at once with its cause, and the goroutine goes on to complete's end
// and then ends the call (endAbandonedAnswer). When the dialog has ended by
// then, complete ends with it, and its result is returned. The goroutine is
// bounded by the ACK wait of 64*T1, what complete does once the ACK is read,
// and that BYE's Timer F; Close joins it.
//
// complete answers on the dialog's ctx, not on ctx: once the final response is
// being sent, its exchange belongs to the dialog and goes on without the
// caller, which ctx only stops waiting.
func (d *DialogServerSession) answerWithin(ctx context.Context, answering chan struct{}, complete func() error) error {
	if ctx.Err() != nil {
		close(answering)
		return fmt.Errorf("answer: %w", context.Cause(ctx))
	}

	var (
		// mu orders the goroutine's report against the caller's leaving, so
		// complete's result is either reported or acted on by the goroutine.
		mu        sync.Mutex
		abandoned bool
	)
	result := make(chan error, 1)
	d.answers.Go(func() {
		defer close(answering)
		err := complete()
		mu.Lock()
		if !abandoned {
			result <- err
			mu.Unlock()
			return
		}
		mu.Unlock()
		d.endAbandonedAnswer(err)
	})

	select {
	case err := <-result:
		return err
	case <-ctx.Done():
	}
	if d.Context().Err() != nil {
		// The dialog has ended, which ends complete's exchange too, as it ends
		// Answer's own ctx: complete's error says why the answer was not
		// confirmed.
		return <-result
	}
	mu.Lock()
	defer mu.Unlock()
	select {
	case err := <-result:
		return err
	default:
	}
	abandoned = true
	return fmt.Errorf("answer: %w", context.Cause(ctx))
}

// endAbandonedAnswer ends the call of an answer whose caller stopped waiting,
// once that answer's exchange is over, err being how it ended. A dialog the
// ACK, or the ACK wait passing without one, has confirmed is ended with a BYE,
// whose wait is bounded by its transaction's Timer F (RFC 3261 section
// 17.1.2.2). A dialog that ended meanwhile, on the peer's BYE, on a CANCEL
// that took the INVITE's transaction, on a refusal, or on the BYE that ends a
// call the ACK left without usable media, needs none.
func (d *DialogServerSession) endAbandonedAnswer(err error) {
	state := d.LoadState()
	if state < sip.DialogStateEstablished || state == sip.DialogStateEnded {
		return
	}
	d.logger().Debug("Ending the call of an answer nobody waits for", "call_id", d.callID(), "answer_error", err)
	ctx, cancel := context.WithTimeout(d.Context(), sip.Timer_F)
	defer cancel()
	if byeErr := d.Bye(ctx); byeErr != nil {
		d.logger().Warn("Ending the call of an answer nobody waits for failed", "call_id", d.callID(), "error", byeErr)
	}
}

// callID returns the Call-ID of the dialog's INVITE, empty when it has none.
func (d *DialogServerSession) callID() string {
	if h := d.InviteRequest.CallID(); h != nil {
		return h.Value()
	}
	return ""
}
