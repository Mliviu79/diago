// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"time"

	"github.com/emiago/sipgo/sip"
)

// errAckCarriesNoAnswer ends a call whose 2xx carried our offer when the ACK
// confirming the dialog brings no answer to it: RFC 3261 section 13.2.1 has the
// answer in that ACK.
var errAckCarriesNoAnswer = errors.New("the ACK to the 2xx carrying our offer carries no answer")

// markAnswering marks an answer in progress and returns the mark, which the
// answer closes when it returns. An answer sets it before it sends its 2xx, so
// awaitAnswer finds it once that 2xx is acknowledged.
func (s *sessionState) markAnswering() chan struct{} {
	answering := make(chan struct{})
	s.answeringMu.Lock()
	s.answering = answering
	s.answeringMu.Unlock()
	return answering
}

// expectAnswerInAck records that our 2xx carries the offer, so the ACK
// confirming the dialog must carry the answer. It is called before that 2xx
// is sent.
func (s *sessionState) expectAnswerInAck() {
	s.answeringMu.Lock()
	s.ackCarriesAnswer = true
	s.answeringMu.Unlock()
}

// answerExpectedInAck reports whether the ACK confirming the dialog must
// carry the answer to an offer in our 2xx.
func (s *sessionState) answerExpectedInAck() bool {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	return s.ackCarriesAnswer
}

// failAckAnswer records err as why the call's media could not be completed
// with the ACK to our 2xx, and reports whether an answer is still waiting for
// that ACK. Such an answer reports err and ends the call itself, see
// endOnAckAnswerError.
func (s *sessionState) failAckAnswer(err error) (answerWaiting bool) {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	s.ackAnswerErr = err
	if s.answering == nil {
		return false
	}
	select {
	case <-s.answering:
		return false
	default:
		return true
	}
}

// ackAnswerError returns what failAckAnswer recorded, nil when the ACK left
// the call with its media.
func (s *sessionState) ackAnswerError() error {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	return s.ackAnswerErr
}

// endOnAckAnswerError is called by an answer once its 2xx is acknowledged.
// When the ACK left the call without usable media, ReadAck left ending the
// call to this answer: it sends the BYE and returns why, joined with the
// BYE's own failure. Otherwise it returns nil.
func (d *DialogServerSession) endOnAckAnswerError() error {
	ackErr := d.ackAnswerError()
	if ackErr == nil {
		return nil
	}
	return errors.Join(ackErr, d.hangupNoMedia())
}

// acksInvite reports whether req, an ACK, acknowledges the 2xx to the INVITE
// that created the dialog. An ACK carries the sequence number of the INVITE it
// acknowledges (RFC 3261 section 13.2.2.4), so the ACK to a re-INVITE carries
// another.
func (d *DialogServerSession) acksInvite(req *sip.Request) bool {
	ack, invite := req.CSeq(), d.InviteRequest.CSeq()
	return ack != nil && invite != nil && ack.SeqNo == invite.SeqNo
}

// ackSessionDescription returns the session description req, an ACK, carries,
// and nil when it carries none: no body, or a body its Content-Type declares
// to be something else. A body without a Content-Type is taken as a session
// description.
func ackSessionDescription(req *sip.Request) []byte {
	body := req.Body()
	if body == nil {
		return nil
	}
	if contentType := req.ContentType(); contentType != nil && contentType.Value() != "application/sdp" {
		return nil
	}
	return body
}

// readInviteAck reads req, an ACK to the 2xx that answered our INVITE, with
// handshaker the media of the call, nil when it has none yet.
//
// When the 2xx carried our offer, the ACK carries the answer (RFC 3261 section
// 13.2.1). It is applied to the media as the answer it is, and the media is
// then completed, which keys it where it needs keys. An ACK without a session
// description completes the media that the offer in the INVITE and the answer
// in our 2xx set up.
//
// An ACK cannot be refused, so an answer that cannot be applied, an answer
// missing from the ACK to the 2xx of AnswerLate, or media that cannot be
// keyed, ends the call with a BYE once the ACK has confirmed the dialog: RFC
// 3261 section 13.2.2.4 has a UAC do the same with an offer it cannot accept,
// and RFC 5763 section 5 has the media session torn down on a fingerprint
// mismatch. An answer waiting for this ACK sends the BYE and reports the
// error itself, so the application learns why and one BYE goes out.
func (d *DialogServerSession) readInviteAck(req *sip.Request, tx sip.ServerTransaction, handshaker mediaHanshaker) error {
	// Only the ACK confirming the dialog answers the offer in the 2xx to our
	// INVITE and completes the media. A copy of it read later, sent for a
	// retransmission of the 2xx, answers nothing more.
	if handshaker == nil || d.LoadState() != sip.DialogStateEstablished {
		return d.DialogServerSession.ReadAck(req, tx)
	}

	err := d.completeAnswer(handshaker, req)
	if err == nil {
		return d.DialogServerSession.ReadAck(req, tx)
	}

	// Nothing was installed: the media drops what the exchange left staged.
	d.abortMedia()
	answerWaiting := d.failAckAnswer(err)
	// The ACK is read before the call is ended: the BYE that ends an answered
	// call waits for the ACK to its 2xx (RFC 3261 section 15), which is the
	// one this handler holds.
	if ackErr := d.DialogServerSession.ReadAck(req, tx); ackErr != nil {
		return errors.Join(err, ackErr)
	}
	if answerWaiting {
		return err
	}
	return errors.Join(err, d.hangupNoMedia())
}

// completeAnswer hands the media what the ACK confirming the dialog brings
// and has it complete the session: the answer the ACK carries, applied as an
// answer, then the commit. It returns errAckCarriesNoAnswer when the ACK had
// to carry the answer and carries none.
func (d *DialogServerSession) completeAnswer(handshaker mediaHanshaker, req *sip.Request) error {
	answer := ackSessionDescription(req)
	if answer == nil {
		if d.answerExpectedInAck() {
			// Our 2xx carried the offer, and this ACK, the one place the
			// answer may be, has none: the session has no remote media.
			return errAckCarriesNoAnswer
		}
		return handshaker.onFinalize(d.Context())
	}
	if err := handshaker.onRemoteSDP(d.Context(), answer, true); err != nil {
		return err
	}
	// The media is keyed on the dialog's ctx, so the wait ends with the call.
	return handshaker.onFinalize(d.Context())
}

// awaitAnswer waits until our answer is complete before an in-dialog request is
// handled, and reports whether it is. Complete means the ACK to our 2xx is read
// and the answer has returned. A peer sends its ACK before any request that
// follows it, but sipgo hands each request to its handler on its own
// goroutine, so a later request can be handled before the ACK, or before the
// answer has started the media that a re-INVITE would replace. Reading the ACK
// is not enough either with a sipgo that tells the dialog's state observers of
// each transition on the goroutine that makes it, one observer after another,
// as v1.6.0 does: a BYE woken by the ACK's notification could end the dialog,
// and have that told to the answer, before the ACK's notification reached it,
// and the answer would then report the ACK missing. A sipgo that tells the
// observers of the transitions in the order they happen cannot reorder them
// so. Waiting restores the order the peer sent them in.
//
// The wait ends with tx, with ctx, or after two T1 intervals, which covers an
// ACK lost in transit and resent when our 2xx is first retransmitted.
func (d *DialogServerSession) awaitAnswer(ctx context.Context, tx sip.ServerTransaction) bool {
	timer := time.NewTimer(2 * sip.T1)
	defer timer.Stop()

	if d.LoadState() == sip.DialogStateEstablished {
		// The state is loaded again after the read is registered, so an ACK
		// read in between is not missed.
		stateCh := d.StateRead()
		for state := d.LoadState(); state == sip.DialogStateEstablished; {
			select {
			case state = <-stateCh:
			case <-tx.Done():
				return false
			case <-ctx.Done():
				return false
			case <-timer.C:
				return false
			}
		}
	}

	// Loaded only now: an answer sets it before its 2xx, so once the 2xx is
	// acknowledged it is in place.
	d.answeringMu.Lock()
	answering := d.answering
	d.answeringMu.Unlock()
	if answering == nil {
		return true
	}
	select {
	case <-answering:
		return true
	case <-tx.Done():
		return false
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// hangupNoMedia ends a call left without usable media by an answer that could
// not be applied or never came, or by media that could not be keyed. The
// offer/answer exchange was complete by then, so the peer has moved to the new
// session. RFC 5763 section 5 has the media session torn down at once on a
// fingerprint mismatch, and with a single audio stream that is the call.
func (d *DialogServerSession) hangupNoMedia() error {
	ctx, cancel := context.WithTimeout(d.Context(), 10*time.Second)
	defer cancel()
	return d.Hangup(ctx)
}
