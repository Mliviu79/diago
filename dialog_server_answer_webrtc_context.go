// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
)

// AnswerWebrtcContext is AnswerWebrtc whose wait ends with ctx, as
// AnswerContext describes for DialogMedia. It answers the WebRTC offer of the
// INVITE with a 200 and returns the media of the call once the ACK to that 200
// is read and the media completed: ICE connected, the DTLS handshake done and
// the SRTP keys derived. The reading of that ACK completes the media, on the
// dialog's ctx, so the wait for ICE and DTLS ends with the call. When ctx ends
// first while the dialog lives, it returns at that end with an error wrapping
// context.Cause(ctx), and the dialog completes the exchange and then ends the
// call with a BYE. A ctx that has ended before the 200 is sent leaves the
// INVITE unanswered.
//
// Session timers are negotiated as for AnswerContext (RFC 4028): an INVITE
// whose Session-Expires is below our Min-SE is refused 422.
//
// An ACK cannot be refused, so media the ACK leaves unusable, such as a
// handshake that fails or a fingerprint that does not match, ends the call
// with a BYE, and AnswerWebrtcContext returns why.
//
// ctx bounds the wait and nothing else: the media runs until it is closed,
// which the session's Close and the peer's BYE do. The media of an answer
// that returned an error is closed by the session too.
//
// A call ProgressMediaWebrtc gave early media is answered with that media,
// which is returned: the 200 repeats the answer of the 183, and the
// negotiation ProgressMediaWebrtc left running is completed by the reading of
// the ACK, or the call declined 488 when it has failed by then. The codecs and
// WebRTC configuration of opts then go unused: the media was made with those
// ProgressMediaWebrtc had. A call ProgressMedia gave early media is answered
// with AnswerContext, and AnswerWebrtcContext returns an error for it, sending
// nothing.
func (d *DialogServerSession) AnswerWebrtcContext(ctx context.Context, opts AnswerWebrtcOptions) (*DialogWebrtc, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("answer: %w", context.Cause(ctx))
	}
	if med, early, ok := d.earlyMedia(); ok {
		return d.answerEarlyWebrtcWithin(ctx, med, early, opts)
	}
	// RFC 4028: negotiate session timers before the 200 OK, as AnswerContext
	// does.
	if err := d.prepareSessionTimers(ctx); err != nil {
		return nil, err
	}
	offer := d.InviteRequest.Body()
	if offer == nil {
		return nil, errNoOfferInInvite
	}

	med, err := d.newAnswerWebrtc(opts)
	if err != nil {
		return nil, err
	}
	answer, err := answerToOffer(ctx, med, offer)
	if err != nil {
		// Not registered with the session yet, so nothing else closes it.
		return nil, errors.Join(err, med.Close())
	}
	d.registerAnswerWebrtc(med, opts)

	if err := d.respondSDPWithin(ctx, answer); err != nil {
		return nil, err
	}
	return med, nil
}

// AnswerWebrtcLateContext is AnswerWebrtcContext for an INVITE that carried
// no offer, as AnswerLateContext is for DialogMedia: its 200 carries our
// WebRTC offer, and the ACK must carry the answer (RFC 3261 section 13.2.1),
// which the reading of that ACK applies to the media before it completes it.
// An ACK without an answer ends the call with a BYE, and the answer returns
// errAckCarriesNoAnswer. When ctx ends first it returns at that end, as
// AnswerWebrtcContext does.
func (d *DialogServerSession) AnswerWebrtcLateContext(ctx context.Context, opts AnswerWebrtcOptions) (*DialogWebrtc, error) {
	if ctx.Err() != nil {
		return nil, fmt.Errorf("answer: %w", context.Cause(ctx))
	}
	// RFC 4028: as AnswerWebrtcContext, negotiate session timers before the
	// 2xx.
	if err := d.prepareSessionTimers(ctx); err != nil {
		return nil, err
	}

	med, err := d.newAnswerWebrtc(opts)
	if err != nil {
		return nil, err
	}
	offer, err := med.onLocalSDP(ctx, false, "")
	if err != nil {
		// Not registered with the session yet, so nothing else closes it.
		return nil, errors.Join(err, med.Close())
	}
	d.registerAnswerWebrtc(med, opts)

	// Our 2xx carries the offer, so the ACK must carry the answer, which
	// ReadAck hands to the media.
	d.expectAnswerInAck()
	if err := d.respondSDPWithin(ctx, offer); err != nil {
		return nil, err
	}
	return med, nil
}

// newAnswerWebrtc returns the WebRTC media an answer of the session's INVITE
// exchanges through, before its first exchange. The media is the dialog's,
// not the wait's, so its socket is bound on the dialog's ctx.
func (d *DialogServerSession) newAnswerWebrtc(opts AnswerWebrtcOptions) (*DialogWebrtc, error) {
	return newDialogWebrtc(d.Context(), d.mediaConf, opts.Codecs, opts.WebrtcConfig, opts.OnICEStateChange)
}

// registerAnswerWebrtc makes med the media of the session before the 2xx is
// sent, so the reading of the ACK reaches it and the session's Close and the
// peer's BYE close it, and installs the REFER callback of opts.
func (d *DialogServerSession) registerAnswerWebrtc(med *DialogWebrtc, opts AnswerWebrtcOptions) {
	med.registerDialogCallbacks(&d.dialogCallbacks)
	d.dialogCallbacks.mu.Lock()
	d.onReferDialog = opts.OnRefer
	d.onClose = append(d.onClose, med.Close)
	d.dialogCallbacks.mu.Unlock()
}
