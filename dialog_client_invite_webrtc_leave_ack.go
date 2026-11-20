// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// errNoAnswerIn2xx is the error of an INVITE carrying our offer whose 2xx
// carries no answer: RFC 3261 section 13.2.1 has the answer in the first
// reliable non-failure response to an offer in the INVITE.
var errNoAnswerIn2xx = errors.New("no SDP in response")

// InviteWebrtcLeaveAck is InviteWebrtcLeavePending, with its options, and
// differs in what is left to the caller once the INVITE is answered, as for
// InviteLeavePending: it applies the answer of the 2xx to the media and
// returns that media with a nil error, without the ACK, and you MUST call Ack
// after. Ack sends the ACK and then completes the media within its ctx: ICE
// connected, the DTLS handshake done and the SRTP keys derived. Until Ack has
// returned nil the media carries no audio.
//
// The media is made the session's before the INVITE is sent, so Ack, and a
// re-INVITE of the peer once the ACK is out, reach it, and the session's
// Close and the peer's BYE close it.
//
// When ctx ends before the final response, it sends no CANCEL and returns
// with the INVITE pending, with an error that wraps sipgo.ErrInvitePending and
// context.Cause(ctx), and Cancel withdraws the INVITE on the owner's context,
// as InviteWebrtcLeavePending describes. Every error but ErrClientEarlyMedia
// comes with no media, the media made for the INVITE being closed, the
// pending INVITE's included.
//
// With EarlyMediaDetect, WebRTC early media the callee keyed within
// EarlyMediaKeyTimeout is returned with ErrClientEarlyMedia as InviteWebrtc
// returns it, and WaitAnswerWebrtcLeaveAck goes on waiting for the answer and
// applies the session description of the 2xx as WaitAnswerWebrtc does. Early
// media the callee has not keyed by then is not reported: the answer of the
// 183 stands (RFC 3261 section 13.2.1), the 2xx is returned with the media
// not set up, and Ack completes the negotiation of the 183.
//
// A 2xx whose answer is missing or cannot be applied is acknowledged and the
// call ended with a BYE (RFC 3261 section 13.2.2.4), and the error returned.
//
// NOTE: It updates internal invite request so NOT THREAD SAFE.
func (d *DialogClientSession) InviteWebrtcLeaveAck(ctx context.Context, opts InviteWebrtcOptions) (*DialogWebrtc, error) {
	med, err := newDialogWebrtc(ctx, d.mediaConfig, nil, opts.WebrtcConfig, opts.OnICEStateChange)
	if err != nil {
		return nil, err
	}
	d.registerWebrtcDialogCallbacks(med, opts.OnRefer)

	err = d.inviteWebrtcLeaveAck(ctx, med, opts)
	if err == nil || errors.Is(err, ErrClientEarlyMedia) {
		return med, err
	}
	if closeErr := med.Close(); closeErr != nil {
		return nil, errors.Join(err, closeErr)
	}
	return nil, err
}

// WaitAnswerWebrtcLeaveAck is WaitAnswerWebrtcLeavePending for media that
// InviteWebrtcLeaveAck returned with ErrClientEarlyMedia, and differs in
// leaving the ACK to the caller as InviteWebrtcLeaveAck does: it returns once
// the INVITE is answered, and you MUST call Ack after. When ctx ends before the
// final response it leaves the INVITE pending for Cancel, with an error that
// wraps sipgo.ErrInvitePending and context.Cause(ctx), and med stays as it
// is.
func (d *DialogClientSession) WaitAnswerWebrtcLeaveAck(ctx context.Context, med *DialogWebrtc, opts sipgo.AnswerOptions) error {
	if med == nil {
		return errors.New("dialog WebRTC media is not initialized")
	}
	return d.waitAnswerWebrtcLeaveAck(ctx, med, opts)
}

// inviteWebrtcLeaveAck sends the INVITE with the offer of med and waits for
// its answer, as InviteWebrtcLeaveAck describes.
func (d *DialogClientSession) inviteWebrtcLeaveAck(ctx context.Context, med *DialogWebrtc, opts InviteWebrtcOptions) error {
	offer, err := med.onLocalSDP(ctx, false, "")
	if err != nil {
		return err
	}
	for _, header := range opts.Headers {
		if header == nil {
			return errors.New("invite header is nil")
		}
		d.InviteRequest.AppendHeader(header)
	}
	if err := d.doInvite(ctx, offer); err != nil {
		return err
	}
	answerOpts := sipgo.AnswerOptions{
		OnResponse: opts.OnResponse,
		Username:   opts.Username,
		Password:   opts.Password,
	}
	if opts.EarlyMediaDetect {
		onResponse := opts.OnResponse
		answerOpts.OnResponse = func(res *sip.Response) error {
			if onResponse != nil {
				if err := onResponse(res); err != nil {
					return err
				}
			}
			return d.takeEarlyWebrtcAnswer(ctx, med, res, opts.EarlyMediaKeyTimeout)
		}
	}
	return d.waitAnswerWebrtcLeaveAck(ctx, med, answerOpts)
}

// waitAnswerWebrtcLeaveAck waits for the INVITE's final response, leaving it
// pending when ctx ends first, and applies the answer of a 2xx to med. After
// early media that answer is applied as WaitAnswerWebrtc applies it, and none
// is applied after a 183 whose negotiation was left running.
func (d *DialogClientSession) waitAnswerWebrtcLeaveAck(ctx context.Context, med *DialogWebrtc, opts sipgo.AnswerOptions) error {
	if err := d.DialogClientSession.WaitAnswerLeavePending(ctx, opts); err != nil {
		return err
	}
	// Without early media the 2xx carries the answer to the offer.
	err := errNoAnswerIn2xx
	answer := d.InviteResponse.Body()
	switch {
	case med.MediaSession() != nil || d.earlyKeyingLeft():
		err = d.applyEarlyWebrtcFinalAnswer(ctx, med)
	case answer != nil:
		err = med.onRemoteSDP(ctx, answer, true)
	}
	if err == nil {
		return nil
	}
	// The 2xx is acknowledged and the call it set up ended.
	return errors.Join(err, d.ackAndBye(ctx))
}
