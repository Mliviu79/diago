// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"

	"github.com/emiago/sipgo"
)

// InviteWebrtcLeavePending is InviteWebrtc, with its options and results, and
// differs in what the end of ctx does before the INVITE has its final
// response: it sends no CANCEL and returns with the INVITE pending, with an
// error that wraps sipgo.ErrInvitePending and context.Cause(ctx). The INVITE
// is then the caller's to end: Cancel withdraws it on a context that outlives
// ctx, such as the context of the owner of the call the INVITE attempts, so
// the wait of the CANCEL ends with that context (RFC 3261 section 9.1).
//
// The media made for the INVITE is closed when it returns an error other than
// ErrClientEarlyMedia, as InviteWebrtc closes it, and so also when the INVITE
// is left pending: a 2xx that then answers the INVITE regardless, which Cancel
// returns, has no media, and the caller sends Ack and Bye.
//
// With EarlyMediaDetect it returns the media with ErrClientEarlyMedia as
// InviteWebrtc does, and WaitAnswerWebrtcLeavePending goes on waiting for the
// answer.
func (d *DialogClientSession) InviteWebrtcLeavePending(ctx context.Context, opts InviteWebrtcOptions) (*DialogWebrtc, error) {
	return d.inviteWebrtcWith(ctx, opts, d.DialogClientSession.WaitAnswerLeavePending)
}

// WaitAnswerWebrtcLeavePending is WaitAnswerWebrtc, and differs only in what
// the end of ctx does before the INVITE has its final response, as
// InviteWebrtcLeavePending describes: the INVITE is left pending for Cancel,
// and the error wraps sipgo.ErrInvitePending and context.Cause(ctx). It goes
// on with the INVITE that InviteWebrtcLeavePending returned
// ErrClientEarlyMedia for, and med is the media returned with it, which stays
// as it is when the wait returns an error. On success it sends the ACK, with
// the answer of the 183 standing as WaitAnswerWebrtc describes.
func (d *DialogClientSession) WaitAnswerWebrtcLeavePending(ctx context.Context, med *DialogWebrtc, opts sipgo.AnswerOptions) error {
	if med == nil || med.MediaSession() == nil {
		return errors.New("dialog WebRTC media is not initialized")
	}
	if err := d.DialogClientSession.WaitAnswerLeavePending(ctx, opts); err != nil {
		return err
	}
	if err := d.applyEarlyWebrtcFinalAnswer(ctx, med); err != nil {
		// The 2xx is acknowledged and the call it set up ended.
		return errors.Join(err, d.ackAndBye(ctx))
	}
	return d.Ack(ctx)
}
