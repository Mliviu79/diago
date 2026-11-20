// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"

	"github.com/emiago/sipgo/sip"
)

// reInviteOffer returns the offer req, a re-INVITE of the peer's, carries, and
// nil when it carries none and so asks for ours. Content-Length: 0 can reach a
// handler as an empty but non-nil body, so the length is checked rather than
// nilness.
func reInviteOffer(req *sip.Request) []byte {
	body := req.Body()
	if len(body) == 0 {
		return nil
	}
	return body
}

// applyAckAnswer hands the media the answer ack carries to the offer in our
// 2xx to a re-INVITE of the peer's that carried none, and has it commit the
// update. ack is the ACK to that 2xx. ctx is the dialog's: it bounds what the
// media does to commit.
//
// An ACK without an answer has the media drop the offer, and so has an ACK
// read once the media is closed: the call goes on as it was, or has ended,
// and neither is an error. An ACK cannot be refused, so an error means the
// answer could not be applied or the update committed. The media has then
// dropped the offer too, and the caller ends the call, as RFC 3261 section
// 14.2 has a UAC do with an offer it cannot accept.
func (d *dialogCallbacks) applyAckAnswer(ctx context.Context, ack *sip.Request) error {
	handshaker := d.mediaHanshakerLocked()
	answer := ackSessionDescription(ack)
	if handshaker == nil || len(answer) == 0 {
		d.abortMedia()
		return nil
	}
	err := handshaker.onRemoteSDP(ctx, answer, true)
	if err == nil {
		err = handshaker.onFinalize(ctx)
	}
	if err == nil {
		return nil
	}
	d.abortMedia()
	if errors.Is(err, errMediaClosed) {
		return nil
	}
	return err
}

// readReInviteAnswerAck reads req, the ACK to our 2xx to a re-INVITE of the
// peer's that carried no offer: it hands the media the answer, see
// applyAckAnswer, and ends the call with a BYE when the answer leaves it
// without usable media. The ACK is read before that BYE: the BYE that ends an
// answered call waits for the ACK to its 2xx (RFC 3261 section 15), which is
// the one this handler holds.
func (d *DialogServerSession) readReInviteAnswerAck(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) error {
	err := d.applyAckAnswer(d.Context(), req)
	ackErr := d.DialogServerSession.ReadAck(req, tx)
	if err != nil {
		return errors.Join(err, ackErr, d.hangupNoMedia(ctx))
	}
	return ackErr
}

// readReInviteAnswerAck reads req, the ACK to our 2xx to a re-INVITE of the
// peer's that carried no offer: it hands the media the answer, see
// applyAckAnswer, and ends the call with a BYE when the answer leaves it
// without usable media.
func (d *DialogClientSession) readReInviteAnswerAck(ctx context.Context, req *sip.Request) error {
	if err := d.applyAckAnswer(d.Context(), req); err != nil {
		return errors.Join(err, d.hangupNoMedia(ctx))
	}
	return nil
}
