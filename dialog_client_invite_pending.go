// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"

	"github.com/emiago/sipgo"
)

// inviteAnswerWait waits for the answer to the session's INVITE. It is one of
// the two waits of the embedded session: WaitAnswer, which CANCELs the INVITE
// when ctx ends before the final response, or WaitAnswerLeavePending, which
// leaves the INVITE pending then.
type inviteAnswerWait func(ctx context.Context, opts sipgo.AnswerOptions) error

// InviteLeavePending is Invite, with its options and results, and differs in
// what the end of ctx does before the INVITE has its final response: it sends
// no CANCEL and returns with the INVITE pending, with an error that wraps
// sipgo.ErrInvitePending and context.Cause(ctx). The INVITE is then the
// caller's to end: Cancel withdraws it on a context that outlives ctx, such as
// the context of the owner of the call the INVITE attempts, so the wait of the
// CANCEL ends with that context (RFC 3261 section 9.1).
//
// The offer of a pending INVITE stands, so the media made for it stays bound
// while the INVITE is pending. It is not returned: the session holds it, and
// Cancel closes it, as Close does.
//
// It returns the media with a nil error once the INVITE is answered, and you
// MUST call Ack after, as for Invite. With EarlyMediaDetect it returns the
// media with ErrClientEarlyMedia as Invite does, and WaitAnswerLeavePending
// goes on waiting for the answer. Every other error comes with no media: the
// media is closed, unless the error wraps sipgo.ErrInvitePending.
//
// The end of ctx ends the wait and nothing else: the media returned runs
// until it is closed, which the session's Close, the peer's BYE and Cancel do.
//
// NOTE: It updates internal invite request so NOT THREAD SAFE.
func (d *DialogClientSession) InviteLeavePending(ctx context.Context, opts InviteClientOptions) (*DialogMedia, error) {
	conf := d.mediaConfig
	conf.rtpNAT = opts.RTPNAT

	med, err := newDialogMedia(ctx, conf, nil)
	if err != nil {
		return nil, err
	}
	med.registerDialogCallbacks(&d.dialogCallbacks)

	err = d.inviteWith(ctx, med, opts, d.DialogClientSession.WaitAnswerLeavePending)
	if err == nil || errors.Is(err, ErrClientEarlyMedia) {
		return med, err
	}
	if errors.Is(err, sipgo.ErrInvitePending) {
		// The peer may still answer the offer, and sends media to the port it
		// names meanwhile, so the port is kept until the INVITE has ended.
		return nil, err
	}
	if closeErr := med.Close(); closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	return nil, err
}

// WaitAnswerLeavePending is WaitAnswer, and differs only in what the end of
// ctx does before the INVITE has its final response, as InviteLeavePending
// describes: the INVITE is left pending for Cancel, and the error wraps
// sipgo.ErrInvitePending and context.Cause(ctx). It goes on with the INVITE
// that InviteLeavePending returned ErrClientEarlyMedia for, and med is the
// media returned with it, which stays as it is when the wait returns an error.
func (d *DialogClientSession) WaitAnswerLeavePending(ctx context.Context, med *DialogMedia, opts sipgo.AnswerOptions) error {
	if med == nil {
		return errors.New("dialog media is not initialized")
	}
	return d.waitAnswerWith(ctx, med, opts, d.DialogClientSession.WaitAnswerLeavePending)
}

// Cancel withdraws the INVITE that InviteLeavePending, WaitAnswerLeavePending
// or one of their WebRTC forms left pending. It is the Cancel of the embedded
// session, with its results, on ctx, the context of whoever owns the call the
// INVITE attempts: it sends the CANCEL and waits for the INVITE's final
// response within ctx, as RFC 3261 section 9.1 has it. It must not be called
// while an invite or a wait for the answer is running.
//
// When it returns *sipgo.ErrAnsweredAfterCancel, a 2xx answered the INVITE
// regardless and the dialog is established. The answer the 2xx carries is not
// applied, and the caller sends Ack and then Bye, as RFC 3261 section 13.2.2.4
// requires for a 2xx whose call it does not go on with. The peer sends media
// to the port of the offer until that BYE, so the media stays bound and Close
// closes it.
//
// No other result leaves a dialog, so Cancel then closes the media the session
// holds: the media kept for the pending INVITE, and early media. An error of
// that close is joined to the result.
func (d *DialogClientSession) Cancel(ctx context.Context) error {
	err := d.DialogClientSession.Cancel(ctx)
	var answered *sipgo.ErrAnsweredAfterCancel
	if errors.As(err, &answered) {
		return err
	}
	if closeErr := d.closeCallbacks(); closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return err
}
