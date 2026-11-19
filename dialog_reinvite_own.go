// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// errReInviteAnswerWithoutSDP is why a re-INVITE of ours answered 2xx with no
// SDP left the media session as it was.
var errReInviteAnswerWithoutSDP = errors.New("re-INVITE answered 2xx without SDP")

// ownReInvite is a re-INVITE a session sends for itself: what ReInvite, Hold
// and Unhold share on either side of a call. It reaches the media of the call
// through handshaker and the session's media callbacks only.
type ownReInvite struct {
	state     *sessionState
	callbacks *dialogCallbacks
	dialog    *sipgo.Dialog

	handshaker mediaHanshaker
	// mode is the direction the offer asks for, empty for the one in place.
	mode string
	// inPlace asks the media for the offer of the session in place, which
	// renegotiates nothing, whatever it is configured to offer in a
	// re-INVITE of ours otherwise. A session refresh sets it: RFC 4028
	// section 7.4 has the offer of a re-INVITE that refreshes the session
	// indicate that the session has not changed.
	inPlace bool
	// callIDOwner tells whether we generated the dialog's Call-ID, as the
	// caller does. It sets the wait after a 491, see reInviteRetryWait.
	callIDOwner bool
	// send sends one re-INVITE carrying offer and headers to the remote
	// target and acknowledges its 2xx. A final response other than 2xx is
	// returned together with sipgo.ErrDialogResponse.
	send func(ctx context.Context, offer []byte, headers []sip.Header) (*sip.Response, error)
	// hangup ends a call whose media the answer left unusable.
	hangup func() error
}

// run sends the re-INVITE and has the media take the answer in its 2xx. Each
// attempt waits first, bounded by ctx, for a re-INVITE in progress to end, and
// marks ours in progress, see beginOwnMediaUpdate. A 491 is retried after the
// wait RFC 3261 section 14.1 gives, with no re-INVITE of ours in progress
// meanwhile: a re-INVITE the peer retries inside that wait is then handled,
// not refused 491 again, so two endpoints re-INVITing each other at once do
// not keep refusing each other. Each attempt has the media make a fresh
// offer, of the session in place by then.
//
// While the session timer runs, each attempt carries its headers and the 2xx
// to it sets the timer (RFC 4028 sections 7.1, 7.2 and 7.4), whatever becomes
// of the answer that 2xx carries.
//
// It returns errMediaClosed when the dialog has ended or the media is closed,
// errReInviteAnswerWithoutSDP for a 2xx without SDP, sipgo.ErrDialogResponse
// for any other final response than 2xx or 491, and the cause of ctx when ctx
// ends first. Whenever the answer is not taken, the media drops the offer it
// staged and goes on as it was. An error wrapping errMediaUpdateAfterAnswer
// means the answer was taken and the media it asked for could not be keyed:
// the call has been ended with a BYE.
func (r ownReInvite) run(ctx context.Context) error {
	for {
		refused, err := r.attempt(ctx)
		if !refused {
			return err
		}
		if err := reInviteRetryWait(ctx, r.callIDOwner); err != nil {
			return err
		}
	}
}

// attempt sends the re-INVITE once, marked in progress from before its offer
// is made until its answer is with the media. It reports a 491, for the
// caller to retry.
func (r ownReInvite) attempt(ctx context.Context) (refused bool, err error) {
	endUpdate, err := r.state.beginOwnMediaUpdate(ctx, r.dialog)
	if err != nil {
		return false, err
	}
	defer endUpdate()

	if r.inPlace {
		// No description is handed over to answer: the offer asked for next
		// is the one of the session as it runs, as it is for a re-INVITE of
		// the peer's that carries no offer.
		if err := r.handshaker.onRemoteSDP(ctx, nil, false); err != nil {
			return false, err
		}
	}
	offer, err := r.handshaker.onLocalSDP(ctx, false, r.mode)
	if err != nil {
		return false, err
	}
	timer := r.state.timer.Load()
	var (
		timerHeaders []sip.Header
		weRefresh    bool
	)
	if timer != nil {
		timerHeaders, weRefresh = timer.requestHeaders()
	}
	res, err := r.send(ctx, offer, timerHeaders)
	if err == nil {
		if timer != nil {
			timer.answered(res, weRefresh)
		}
		err = r.applyAnswer(ctx, res)
	}
	if err != nil {
		// Dropped while the re-INVITE is still marked in progress, so the
		// next exchange, the peer's or ours, finds nothing staged.
		r.callbacks.abortMedia()
	}
	if res != nil && res.StatusCode == sip.StatusRequestPending {
		return true, nil
	}
	return false, err
}

// applyAnswer hands the media the answer in res, the 2xx to our re-INVITE,
// which has been acknowledged.
func (r ownReInvite) applyAnswer(ctx context.Context, res *sip.Response) error {
	// On a dialog that has ended nothing would close what the answer
	// installs.
	if r.dialog.LoadState() == sip.DialogStateEnded {
		return errMediaClosed
	}
	// The 2xx to a target refresh request replaces the remote target with
	// its Contact, if present (RFC 3261 section 12.2.1.2), whatever becomes
	// of the answer it carries.
	if contact := res.Contact(); contact != nil {
		r.callbacks.setRemoteContact(contact)
	}
	answer := res.Body()
	if len(answer) == 0 {
		return errReInviteAnswerWithoutSDP
	}
	// An answer that completes a new DTLS association has the media run its
	// handshake before it returns: within ctx, and within the call, since a
	// peer that has hung up never completes it.
	ctx, stop := endsWithDialog(ctx, r.dialog)
	defer stop()
	err := r.handshaker.onRemoteSDP(ctx, answer, true)
	if errors.Is(err, errMediaKeyingFailed) {
		// The answer stands, so the peer has moved to a session that has no
		// keys here.
		return errors.Join(fmt.Errorf("%w: %w", errMediaUpdateAfterAnswer, err), r.hangup())
	}
	return err
}

// reInviteRetryWait waits, bounded by ctx, before a re-INVITE of ours answered
// 491 is sent again. callIDOwner tells whether we generated the dialog's
// Call-ID, as the caller does. RFC 3261 section 14.1:
//
//	If a UAC receives a 491 response to a re-INVITE, it SHOULD start a
//	timer with a value T chosen as follows:
//	   1. If the UAC is the owner of the Call-ID of the dialog ID
//	      (meaning it generated the value), T has a randomly chosen value
//	      between 2.1 and 4 seconds in units of 10 ms.
//	   2. If the UAC is not the owner of the Call-ID of the dialog ID, T
//	      has a randomly chosen value of between 0 and 2 seconds in units
//	      of 10 ms.
func reInviteRetryWait(ctx context.Context, callIDOwner bool) error {
	wait := time.Duration(mrand.IntN(201)) * 10 * time.Millisecond
	if callIDOwner {
		wait = time.Duration(210+mrand.IntN(191)) * 10 * time.Millisecond
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return context.Cause(ctx)
	}
}

// reInviteSendOnce sends req, a re-INVITE, with do, and acknowledges its 2xx
// with ack at the Contact that 2xx carries. A final response other than 2xx
// is returned with sipgo.ErrDialogResponse. A 2xx without Contact is
// returned, not acknowledged, with an error wrapping
// sipgo.ErrDialogInviteNoContact: RFC 3261 section 12.1.1 has a 2xx to an
// INVITE carry one.
func reInviteSendOnce(
	ctx context.Context,
	req *sip.Request,
	do func(context.Context, *sip.Request) (*sip.Response, error),
	ack func(ctx context.Context, remoteTarget sip.Uri, body []byte) error,
) (*sip.Response, error) {
	res, err := do(ctx, req)
	if err != nil {
		return nil, err
	}
	if !res.IsSuccess() {
		return res, sipgo.ErrDialogResponse{
			Res: res,
		}
	}

	// Now do ACK on new Contact
	cont := res.Contact()
	if cont == nil {
		return res, fmt.Errorf("reinvite: 2xx without Contact: %w", sipgo.ErrDialogInviteNoContact)
	}
	if err := ack(ctx, cont.Address, nil); err != nil {
		return res, err
	}
	return res, nil
}

// reInviteSend sends req, a re-INVITE, once, and acknowledges its 2xx, as
// reInviteSendOnce describes.
func (d *DialogServerSession) reInviteSend(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	return reInviteSendOnce(ctx, req, d.Do, d.ack)
}

// reInviteSendAtTarget sends req as reInviteSend does, but for a 2xx without
// Contact, which it acknowledges at the remote target in place and returns
// without an error. RFC 3261 section 13.2.2.4 has every 2xx acknowledged,
// with an ACK built as any request within the dialog is, and section 12.2.1.2
// has a 2xx move the remote target only when it carries a Contact.
func (d *DialogServerSession) reInviteSendAtTarget(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	res, err := d.reInviteSend(ctx, req)
	if !errors.Is(err, sipgo.ErrDialogInviteNoContact) {
		return res, err
	}
	target := d.RemoteContact()
	if target == nil {
		return res, err
	}
	if err := d.ack(ctx, target.Address, nil); err != nil {
		return res, err
	}
	return res, nil
}

// ownReInvite returns the re-INVITE this side of an inbound call sends for
// itself: its offer is the one handshaker makes for mode, and each attempt
// goes to the remote target of that moment through send.
func (d *DialogServerSession) ownReInvite(
	handshaker mediaHanshaker,
	mode string,
	send func(context.Context, *sip.Request) (*sip.Response, error),
) ownReInvite {
	return ownReInvite{
		state:      &d.sessionState,
		callbacks:  &d.dialogCallbacks,
		dialog:     &d.Dialog,
		handshaker: handshaker,
		mode:       mode,
		// The caller generated the Call-ID.
		callIDOwner: false,
		hangup:      d.hangupNoMedia,
		send: func(ctx context.Context, offer []byte, headers []sip.Header) (*sip.Response, error) {
			// NOTE: we do not change original invite request
			req := sip.NewRequest(sip.INVITE, d.RemoteContact().Address)
			req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			for _, h := range headers {
				req.AppendHeader(h)
			}
			req.SetBody(offer)
			return send(ctx, req)
		},
	}
}

// reInviteSend sends req, a re-INVITE, once, and acknowledges its 2xx, as
// reInviteSendOnce describes.
func (d *DialogClientSession) reInviteSend(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	return reInviteSendOnce(ctx, req, d.Do, d.ack)
}

// ownReInvite returns the re-INVITE this side of an outbound call sends for
// itself: its offer is the one handshaker makes for mode, and each attempt
// goes to the remote target of that moment.
func (d *DialogClientSession) ownReInvite(handshaker mediaHanshaker, mode string) ownReInvite {
	return ownReInvite{
		state:      &d.sessionState,
		callbacks:  &d.dialogCallbacks,
		dialog:     &d.Dialog,
		handshaker: handshaker,
		mode:       mode,
		// We generated the Call-ID.
		callIDOwner: true,
		hangup:      d.hangupNoMedia,
		send: func(ctx context.Context, offer []byte, headers []sip.Header) (*sip.Response, error) {
			// NOTE: we do not change original invite request
			req := sip.NewRequest(sip.INVITE, d.RemoteContact().Address)
			req.AppendHeader(d.InviteRequest.Contact())
			req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			for _, h := range headers {
				req.AppendHeader(h)
			}
			req.SetBody(offer)
			return d.reInviteSend(ctx, req)
		},
	}
}
