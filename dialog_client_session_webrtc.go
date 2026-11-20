// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/ice/v4"
)

// InviteWebrtcOptions configures a SIP call using diago's direct ICE +
// DTLS-SRTP media stack. A temporary DTLS certificate is generated when the
// configuration does not provide one.
type InviteWebrtcOptions struct {
	OnResponse func(*sip.Response) error
	OnRefer    OnReferDialogFunc
	Username   string
	Password   string
	Headers    []sip.Header
	// Stop after WebRTC media is established from a 183 Session Progress
	// response. InviteWebrtc returns the usable media together with
	// ErrClientEarlyMedia; call WaitAnswerWebrtc to finish the call.
	EarlyMediaDetect bool
	// EarlyMediaKeyTimeout bounds how long, with EarlyMediaDetect, the call
	// waits for the callee to key the early media of a 183, by completing ICE
	// and the DTLS handshake, before it takes the call as having none. Zero
	// means five seconds. It bounds the wait only: the negotiation goes on,
	// and the ACK completes it.
	EarlyMediaKeyTimeout time.Duration

	WebrtcConfig media.MediaSessionWebrtcConfig
	// OnICEStateChange observes ICE transport state. It must not block.
	OnICEStateChange func(ice.ConnectionState)
}

// InviteWebrtc sends an SDP offer, completes ICE/DTLS-SRTP negotiation and
// returns encoded audio RTP access for the established SIP dialog.
//
// When EarlyMediaDetect is enabled and a 183 response contains SDP, the method
// establishes the WebRTC transport and returns it with ErrClientEarlyMedia.
// The caller can use the returned media immediately, then call
// WaitAnswerWebrtc to wait for the final response and ACK it.
//
// The callee is given EarlyMediaKeyTimeout to key the early media. Early
// media it has not keyed by then is not reported: InviteWebrtc goes on to the
// 2xx, the answer of the 183 stands and no later session description is
// applied (RFC 3261 section 13.2.1), and once the ACK is sent it completes the
// negotiation of the 183, bounded by ctx, media.ICEConnectTimeout and
// media.DTLSHandshakeTimeout. A 2xx of another early dialog than the 183, one
// with another To tag, ends that negotiation and has the media take its own
// dialog's answer instead, as WaitAnswerWebrtc describes.
//
// Without EarlyMediaDetect the answer is likewise the first session
// description of the 2xx's dialog: one a provisional response with the 2xx's
// To tag carried, when one did, and the 2xx's otherwise.
func (d *DialogClientSession) InviteWebrtc(ctx context.Context, opts InviteWebrtcOptions) (*DialogWebrtc, error) {
	return d.inviteWebrtcWith(ctx, opts, d.DialogClientSession.WaitAnswer)
}

// inviteWebrtcWith is InviteWebrtc, waiting for the INVITE's answer with wait.
func (d *DialogClientSession) inviteWebrtcWith(ctx context.Context, opts InviteWebrtcOptions, wait inviteAnswerWait) (*DialogWebrtc, error) {
	conf, err := prepareWebrtcConfig(d.mediaConfig.webrtcConfig(opts.WebrtcConfig))
	if err != nil {
		return nil, err
	}
	sess := &media.MediaSessionWebrtc{Codecs: slices.Clone(d.mediaConfig.Codecs)}
	if err = sess.Init(ctx, conf, opts.OnICEStateChange); err != nil {
		return nil, err
	}
	med := &DialogWebrtc{dialogWebrtcState: dialogWebrtcState{offerNegotiatedCodecs: d.mediaConfig.ReInviteNegotiatedCodecs}}
	answered := false
	acked := false

	d.Dialog.OnState(func(state sip.DialogState) {
		if state == sip.DialogStateEnded {
			_ = med.Close()
		}
	})

	err = func() error {
		localSDP, err := sess.LocalSDP(ctx, false)
		if err != nil {
			return err
		}
		for _, header := range opts.Headers {
			if header == nil {
				return fmt.Errorf("invite header is nil")
			}
			d.InviteRequest.AppendHeader(header)
		}
		if err = d.doInvite(ctx, localSDP); err != nil {
			return err
		}
		answerOpts := sipgo.AnswerOptions{
			OnResponse: opts.OnResponse,
			Username:   opts.Username,
			Password:   opts.Password,
		}
		if opts.EarlyMediaDetect {
			onResponse := answerOpts.OnResponse
			answerOpts.OnResponse = func(res *sip.Response) error {
				if onResponse != nil {
					if err := onResponse(res); err != nil {
						return err
					}
				}
				if earlySessionDescription(res) == nil {
					return nil
				}
				// The answer of the 183 is applied and keyed through the
				// media, which takes the transport of the offer for it.
				if err := med.takeFirstExchange(sess); err != nil {
					return err
				}
				return d.takeEarlyWebrtcAnswer(ctx, med, res, opts.EarlyMediaKeyTimeout)
			}
		}
		if err = d.awaitInviteAnswer(ctx, med, answerOpts, wait); err != nil {
			return err
		}
		answered = true
		if d.earlyAnswerStands(d.InviteResponse) {
			// The answer of the 183 stands (RFC 3261 section 13.2.1), and the
			// negotiation it started is completed once the ACK is out.
			if err = d.Ack(ctx); err != nil {
				return err
			}
			acked = true
			return med.onFinalize(ctx)
		}
		// The answer of the 2xx's dialog, which its provisional responses
		// may have given (RFC 3261 section 13.2.1).
		remoteSDP := d.dialogAnswer(d.InviteResponse)
		if remoteSDP == nil {
			return fmt.Errorf("no SDP in response")
		}
		if d.earlyKeyingLeft() {
			// The 2xx of another early dialog than the 183 whose negotiation
			// was left running: the media ends that negotiation and takes
			// this dialog's answer, which it completes once the ACK is out.
			if err = med.onRemoteSDP(ctx, remoteSDP, true); err != nil {
				return err
			}
			if err = d.Ack(ctx); err != nil {
				return err
			}
			acked = true
			return med.onFinalize(ctx)
		}
		if err = sess.RemoteSDP(ctx, remoteSDP, true); err != nil {
			return err
		}
		if err = d.Ack(ctx); err != nil {
			return err
		}
		acked = true
		return finalizeWebrtcMedia(ctx, sess, med)
	}()
	if err != nil {
		if errors.Is(err, ErrClientEarlyMedia) && med.MediaSession() != nil {
			d.registerWebrtcDialogCallbacks(med, opts.OnRefer)
			return med, err
		}
		cleanupErr := sess.Close()
		if answered {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), webrtcFailureCleanupTimeout)
			defer cancel()
			if !acked {
				cleanupErr = errors.Join(cleanupErr, d.Ack(cleanupCtx))
			}
			cleanupErr = errors.Join(cleanupErr, d.Hangup(cleanupCtx))
		}
		return nil, errors.Join(err, cleanupErr)
	}

	d.registerWebrtcDialogCallbacks(med, opts.OnRefer)
	return med, nil
}

func finalizeWebrtcMedia(ctx context.Context, sess *media.MediaSessionWebrtc, med *DialogWebrtc) error {
	if err := sess.Finalize(ctx); err != nil {
		return err
	}
	rtpSess := media.NewRTPSessionWebrtc(sess)
	// Set up and started under the lock Close takes: a Close made by the
	// dialog's end meanwhile found no transport to close, and nothing may
	// start on closed media.
	if err := med.initMonitored(sess, rtpSess, rtpSess.MonitorBackground); err != nil {
		return errors.Join(err, rtpSess.Close())
	}
	return nil
}

func (d *DialogClientSession) registerWebrtcDialogCallbacks(med *DialogWebrtc, onRefer OnReferDialogFunc) {
	med.registerDialogCallbacks(&d.dialogCallbacks)
	d.dialogCallbacks.mu.Lock()
	d.onReferDialog = onRefer
	d.onClose = append(d.onClose, med.Close)
	d.dialogCallbacks.mu.Unlock()
}

// WaitAnswerWebrtc continues an InviteWebrtc call that returned
// ErrClientEarlyMedia. The early WebRTC transport remains active while this
// method waits for the final response; on success it sends the ACK.
//
// The answer of the 183 stands (RFC 3261 section 13.2.1): a session
// description in the 2xx is not applied, whether or not it differs from the
// 183's (RFC 6337 section 3.1.1), and med keeps the transport, the keys and
// the codecs the 183 set up, and a 2xx without one is answered. Only the 2xx
// of another early dialog, one with another To tag than the 183, has an
// answer applied to med in place: that dialog's own, the first session
// description its responses carried (RFC 3261 sections 12.1 and 13.2.1). One
// that cannot be applied is acknowledged and the call ended with a BYE, and
// the error returned.
func (d *DialogClientSession) WaitAnswerWebrtc(ctx context.Context, med *DialogWebrtc, opts sipgo.AnswerOptions) error {
	if med == nil || med.MediaSession() == nil {
		return fmt.Errorf("dialog WebRTC media is not initialized")
	}
	if err := d.awaitInviteAnswer(ctx, med, opts, d.DialogClientSession.WaitAnswer); err != nil {
		return err
	}
	if err := d.applyEarlyWebrtcFinalAnswer(ctx, med); err != nil {
		// The 2xx is acknowledged and the call it set up ended.
		return errors.Join(err, d.ackAndBye(ctx))
	}
	return d.Ack(ctx)
}
