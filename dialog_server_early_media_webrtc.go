// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/ice/v4"
)

// ProgressMediaWebrtcOptions configures the WebRTC early media of
// ProgressMediaWebrtc.
type ProgressMediaWebrtcOptions struct {
	// Codecs that will be used, the media config's when empty.
	Codecs []media.Codec

	WebrtcConfig media.MediaSessionWebrtcConfig
	// OnICEStateChange observes ICE transport state. It must not block.
	OnICEStateChange func(ice.ConnectionState)

	// KeyTimeout bounds how long ProgressMediaWebrtc waits for the caller to
	// key the early media, by completing ICE and the DTLS handshake with us,
	// before it returns ErrEarlyMediaNotKeyed. Zero means five seconds, for
	// the reasons defaultEarlyMediaKeyTimeout gives. It bounds the wait only:
	// the negotiation goes on, for the answer to complete after the 200.
	KeyTimeout time.Duration
}

// ProgressMediaWebrtc is ProgressMedia for a WebRTC offer: it sends 183
// Session Progress with our answer to the offer in the INVITE, on ctx, and
// returns the early media of the call.
//
// It returns once the caller has keyed the early media, which then carries
// audio, or ErrEarlyMediaNotKeyed, with the media, when the caller has not
// within ProgressMediaWebrtcOptions.KeyTimeout. That media has no transport
// set up, so no audio reader or writer, until the answer has completed the
// negotiation ProgressMediaWebrtc started. The negotiation runs on with the
// dialog meanwhile.
//
// The call stays answerable: AnswerWebrtcContext, or AnswerWebrtc, answers it
// with this media and returns it. The 200 repeats the answer of the 183 byte
// for byte (RFC 3261 section 13.2.1), the reading of the ACK completes a
// negotiation left running, bounded by the call and by
// media.ICEConnectTimeout plus media.DTLSHandshakeTimeout, and a negotiation
// that has failed by the time of the answer declines the call 488. Any other
// error comes without media.
//
// The media is the session's once the 183 is sent: the session's Close and
// the peer's BYE close it.
func (d *DialogServerSession) ProgressMediaWebrtc(ctx context.Context, opts ProgressMediaWebrtcOptions) (*DialogWebrtc, error) {
	offer := d.InviteRequest.Body()
	if offer == nil {
		return nil, errNoOfferInInvite
	}
	// The media is the dialog's, not the 183's, so its socket is bound on
	// the dialog's ctx.
	med, err := newDialogWebrtc(d.Context(), d.mediaConf, opts.Codecs, opts.WebrtcConfig, opts.OnICEStateChange)
	if err != nil {
		return nil, err
	}
	answer, err := answerToOffer(ctx, med, offer)
	if err != nil {
		// Not registered with the session yet, so nothing else closes it.
		return nil, errors.Join(err, med.Close())
	}
	contentType := sip.NewHeader("Content-Type", "application/sdp")
	if err := d.DialogServerSession.Respond(ctx, sip.StatusSessionInProgress, "Session Progress", answer, contentType); err != nil {
		return nil, errors.Join(err, med.Close())
	}

	med.registerDialogCallbacks(&d.dialogCallbacks)
	d.dialogCallbacks.mu.Lock()
	d.onClose = append(d.onClose, med.Close)
	d.dialogCallbacks.mu.Unlock()

	// The early media carries nothing before the caller has keyed it.
	if err := d.keyEarlyMedia(med, answer, opts.KeyTimeout); err != nil {
		if errors.Is(err, ErrEarlyMediaNotKeyed) {
			return med, err
		}
		return nil, err
	}
	return med, nil
}

// answerEarlyWebrtcWithin answers, within ctx, a call that has the early
// media keyer, as AnswerWebrtcContext describes, with the REFER callback of
// opts, and returns that media. Early media that is not WebRTC media is
// refused with errEarlyMediaOtherType, and nothing is sent.
func (d *DialogServerSession) answerEarlyWebrtcWithin(ctx context.Context, keyer mediaEarlyKeyer, early earlyAnswer, opts AnswerWebrtcOptions) (*DialogWebrtc, error) {
	med, ok := keyer.(*DialogWebrtc)
	if !ok {
		return nil, errEarlyMediaOtherType
	}
	d.dialogCallbacks.mu.Lock()
	d.onReferDialog = opts.OnRefer
	d.dialogCallbacks.mu.Unlock()
	if err := d.answerEarlyMediaWithin(ctx, med, early); err != nil {
		return nil, err
	}
	return med, nil
}
