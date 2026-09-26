// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

var (
	ErrClientEarlyMedia = errors.New("Early media detected")
)

// DialogClientSession represents outbound channel
type DialogClientSession struct {
	*sipgo.DialogClientSession

	DialogMedia

	onReferDialog OnReferDialogFunc
	mediaConfig   MediaConfig

	// acking is set by Ack before it sends the ACK, and closed when Ack
	// returns: once the media it finalizes is keyed, or has failed. awaitAck
	// waits on it. Guarded by d.mu.
	acking chan struct{}

	closed atomic.Uint32
}

func (d *DialogClientSession) Close() error {
	if !d.closed.CompareAndSwap(0, 1) {
		return nil
	}
	e1 := d.DialogMedia.Close()
	e2 := d.DialogClientSession.Close()
	return errors.Join(e1, e2)
}

func (d *DialogClientSession) Id() string {
	return d.ID
}

func (d *DialogClientSession) Hangup(ctx context.Context) error {
	return d.Bye(ctx)
}

func (d *DialogClientSession) FromUser() string {
	return d.InviteRequest.From().Address.User
}

func (d *DialogClientSession) ToUser() string {
	return d.InviteRequest.To().Address.User
}

func (d *DialogClientSession) DialogSIP() *sipgo.Dialog {
	return &d.Dialog
}

func (d *DialogClientSession) RemoteContact() *sip.ContactHeader {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.remoteContactUnsafe()
}

func (d *DialogClientSession) remoteContactUnsafe() *sip.ContactHeader {
	if d.remoteContactTarget != nil {
		// Invite update can change contact
		return d.remoteContactTarget
	}
	return d.InviteResponse.Contact()
}

// InviteClientOptions is passed on dialog client Invite with extra control over dialog
type InviteClientOptions struct {
	Originator DialogSession
	OnResponse func(res *sip.Response) error
	// OnMediaUpdate called when media is changed.
	// NOTE: you should not block this call as it blocks response processing.
	OnMediaUpdate func(d *DialogMedia)
	// OnRefer is called on successfull REFER handling
	//
	// It creates new dialog (NewDialog) on which you need to call Invite() and Ack()
	// Any error from invite, ack or other processing should be returned for correct Notify handling
	//
	// NOTE: IT is SCOPED to handler and exiting handler will Close/Terminate this dialog!
	OnRefer OnReferDialogFunc
	// For digest authentication
	Username string
	Password string

	// Custom headers to pass. DO NOT SET THIS to nil
	Headers []sip.Header
	// Stop on early media. ErrClientEarlyMedia will be returned
	EarlyMediaDetect bool
}

// WithAnonymousCaller sets from user Anonymous per RFC
func (o *InviteClientOptions) WithAnonymousCaller() {
	o.Headers = append(o.Headers, &sip.FromHeader{
		DisplayName: "Anonymous",
		Address:     sip.Uri{User: "anonymous", Host: "anonymous.invalid"},
		Params:      sip.NewParams(),
	})
}

// WithCaller allows simpler way modifying caller
func (o *InviteClientOptions) WithCaller(displayName string, callerID string, host string) {
	o.Headers = append(o.Headers, &sip.FromHeader{
		DisplayName: displayName,
		Address:     sip.Uri{User: callerID, Host: host},
		Params:      sip.NewParams(),
	})
}

// Invite sends Invite request and establishes [early] media. Normally you need to call Ack after.
//
// Normal Answer with 200 OK (SDP)
// - You MUST call Ack() after to acknowledge session.
//
// Early Media Detect:
// - EarlyMediaDetect=true must be set as part of options otherwise it ignores early media
// - It RETURNS ErrClientEarlyMedia if remote answers with 183 Session in Progress
// - Media is negotiated and setuped
// - You need to call WaitAnswer() if you want to proceed with answering call
//
// Errors:
// - sipgo.ErrDialogResponse
// - ErrClientEarlyMedia
//
// NOTE: It updates internal invite request so NOT THREAD SAFE.
// If you pass originator it will use originator to set correct from header and avoid media transcoding
func (d *DialogClientSession) Invite(ctx context.Context, opts InviteClientOptions) error {
	if err := d.initMediaSessionFromConf(d.mediaConfig); err != nil {
		return err
	}
	return d.invite(ctx, &d.DialogMedia, opts)
}

func (d *DialogClientSession) invite(ctx context.Context, med *DialogMedia, opts InviteClientOptions) error {
	sess := med.mediaSession
	inviteReq := d.InviteRequest
	originator := opts.Originator

	for _, h := range opts.Headers {
		inviteReq.AppendHeader(h)
	}

	if originator != nil {
		// In case originator then:
		// - check do we support this media formats by conf
		// - if we do, then filter and pass to dial endpoint filtered
		origInvite := originator.DialogSIP().InviteRequest
		if fromHDR := inviteReq.From(); fromHDR == nil {
			// From header should be preserved from originator
			fromHDROrig := origInvite.From()
			f := sip.FromHeader{
				DisplayName: fromHDROrig.DisplayName,
				Address:     *fromHDROrig.Address.Clone(),
				Params:      fromHDROrig.Params.Clone(),
			}
			inviteReq.AppendHeader(&f)
		}

		// Avoid transcoding if originator present
		// Check ContentType and body present
		contType := origInvite.ContentType()
		if body := origInvite.Body(); body != nil && (contType != nil && contType.Value() == "application/sdp") {
			// apply remote SDP
			if err := sess.RemoteSDP(body); err != nil {
				return fmt.Errorf("failed to apply originator sdp: %w", err)
			}
			// We do not want originator to be remote side, but we want to apply codec filtering
			sess.SetRemoteAddr(&net.UDPAddr{})

			// Now to totally remove transcoding a chance. Leave only one codec of different types
			audioCodec := media.Codec{}
			telEventCodec := media.Codec{}

			codecs := sess.CommonCodecs()
			if len(codecs) == 0 { // No negotiation yet happened
				codecs = sess.Codecs
			}

			for _, c := range codecs {
				// TODO refactor this
				if strings.HasPrefix(c.Name, "telephone-event") {
					if telEventCodec.SampleRate == 0 {
						telEventCodec = c
					}
					continue
				}

				if audioCodec.SampleRate == 0 {
					audioCodec = c
				}
			}

			// TODO: DO we need to be thread safe here?
			// In this case we want to rewrite what should be Offered in our SDP
			// NOTE: Generally this would require Session Fork, but for now we avoid this extra step.
			sessCodecs := sess.Codecs[:0]
			if audioCodec.SampleRate != 0 {
				sessCodecs = append(sessCodecs, audioCodec)
			}

			// TODO: should we only match telephone event with same sampling rate?
			if telEventCodec.SampleRate != 0 {
				sessCodecs = append(sessCodecs, telEventCodec)
			}

			if len(sessCodecs) == 0 {
				return fmt.Errorf("no codecs support found from originator")
			}
			sess.Codecs = sessCodecs
		}
	}

	dialogCli := d.UA
	inviteReq.AppendHeader(&dialogCli.ContactHDR)
	inviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteReq.SetBody(sess.LocalSDP())

	// We allow changing full from header, but we need to make sure it is correctly set
	// If users specify 'tag' parameter it is assumed that they know what they do
	if fromHDR := inviteReq.From(); fromHDR != nil && !fromHDR.Params.Has("tag") {
		fromHDR.Params.Add("tag", sip.GenerateTagN(16))
	}

	// Build here request
	client := d.UA.Client
	if err := sipgo.ClientRequestBuild(client, inviteReq); err != nil {
		return err
	}

	// This only gets called after session established
	med.onMediaUpdate = opts.OnMediaUpdate
	d.onReferDialog = opts.OnRefer
	// reuse UDP listener
	// Problem if listener is unspecified IP sipgo will not map this to listener
	// Code below only works if our bind host is specified
	// For now let SIPgo create 1 UDP connection and it will reuse it
	// via := inviteReq.Via()
	// if via.Host == "" {
	// }
	err := d.DialogClientSession.Invite(ctx, func(c *sipgo.Client, req *sip.Request) error {
		// Do nothing
		return nil
	})
	if err != nil {
		// sess.Close()
		return err
	}
	ansOpts := sipgo.AnswerOptions{
		Username:   opts.Username,
		Password:   opts.Password,
		OnResponse: opts.OnResponse,
	}

	if opts.EarlyMediaDetect {
		return d.waitAnswerEarly(ctx, med, ansOpts)
	}

	return d.waitAnswer(ctx, med, ansOpts)
}

// WaitAnswer waits dialog on answer. It should only be used if you have error Invite but still want to continue
// ex. ErrClientEarlyMedia was returned but you want to proceed with answering
func (d *DialogClientSession) WaitAnswer(ctx context.Context, opts sipgo.AnswerOptions) error {
	return d.waitAnswer(ctx, &d.DialogMedia, opts)
}

func (d *DialogClientSession) waitAnswerEarly(ctx context.Context, med *DialogMedia, opts sipgo.AnswerOptions) error {
	sess := med.mediaSession
	onResps := opts.OnResponse

	// Add early media check
	opts.OnResponse = func(res *sip.Response) error {
		// https://datatracker.ietf.org/doc/html/rfc3261#section-8.1.3.2
		// 		UAC MUST treat any provisional response different than 100 that it
		//    does not recognize as 183 (Session Progress).
		// Check any existing
		if onResps != nil {
			if err := onResps(res); err != nil {
				return err
			}
		}

		// handle 183 Session Progress early media
		if res.StatusCode != sip.StatusSessionInProgress {
			return nil
		}

		if cont := res.ContentType(); cont == nil || cont.Value() != "application/sdp" {
			return nil
		}

		remoteSDP := res.Body()
		if remoteSDP == nil {
			return nil
		}

		// A response body answers the offer we sent on the INVITE.
		sess.RemoteSDPIsAnswer = true
		if err := sess.RemoteSDP(remoteSDP); err != nil {
			return err
		}

		// The wait for the answer cannot see the caller give up while the
		// handshake runs, so the handshake ends with ctx, and with the call.
		fctx, cancel := contextWithDialog(ctx, d.Context())
		err := sess.FinalizeContext(fctx)
		cancel()
		if err != nil {
			return err
		}

		rtpSess := media.NewRTPSession(sess)
		med.mu.Lock()
		med.initRTPSessionUnsafe(sess, rtpSess)
		med.onCloseUnsafe(func() error {
			return rtpSess.Close()
		})
		med.mu.Unlock()

		// Must be called after reader and writer setup due to race
		if err := rtpSess.MonitorBackground(); err != nil {
			return err
		}

		return ErrClientEarlyMedia
	}
	return d.waitAnswer(ctx, med, opts)
}

func (d *DialogClientSession) waitAnswer(ctx context.Context, med *DialogMedia, opts sipgo.AnswerOptions) error {
	if err := d.DialogClientSession.WaitAnswer(ctx, opts); err != nil {
		return err
	}

	remoteSDP := d.InviteResponse.Body()
	if remoteSDP == nil {
		return fmt.Errorf("no SDP in response")
	}

	if err := d.applyRemoteSDP(med, remoteSDP); err != nil {
		// Terminate call. Call must be ACK before doing BYE
		if err := d.Ack(ctx); err != nil {
			return errors.Join(err, d.Ack(ctx))
		}
		return errors.Join(err, d.Bye(ctx))
	}

	return nil
}

func (d *DialogClientSession) applyRemoteSDP(med *DialogMedia, remoteSDP []byte) error {
	sess := med.mediaSession

	// This body comes from a response, so it answers our offer. checkEarlyMedia
	// forks the session, and Fork carries the role.
	sess.RemoteSDPIsAnswer = true

	// Apply SDP on existing (Early) media if it exists
	if err := med.checkEarlyMedia(remoteSDP); err != errNoRTPSession {
		return err
	}

	if err := sess.RemoteSDP(remoteSDP); err != nil {
		return err
	}

	// Create RTP session. After this no media session configuration should be changed
	rtpSess := media.NewRTPSession(sess)
	med.mu.Lock()
	med.initRTPSessionUnsafe(sess, rtpSess)
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	med.mu.Unlock()

	// Must be called after reader and writer setup due to race
	return rtpSess.MonitorBackground()
}

// Ack acknowledgeds media
// Before Ack normally you want to setup more stuff like bridging
func (d *DialogClientSession) Ack(ctx context.Context) error {
	inviteRequest := d.InviteRequest
	recipient := inviteRequest.Recipient
	if contact := d.InviteResponse.Contact(); contact != nil {
		recipient = contact.Address
	}

	// The session is taken under the lock before the ACK goes out. Once it is
	// out the peer may re-INVITE, and handling that swaps in a fork of this
	// session concurrently. The fork has nothing to finalize; this one does.
	// Such a re-INVITE waits for Ack to return, see awaitAck.
	d.mu.Lock()
	msess := d.mediaSession
	acking := make(chan struct{})
	d.acking = acking
	d.mu.Unlock()
	defer close(acking)

	if err := d.ack(ctx, recipient, nil); err != nil {
		return err
	}

	// NOTE it generally advisable todo this after successfull ACK:
	// Server may not even listen yet as it is waiting for ACK
	if msess != nil {
		// The handshake ends with ctx, and with the call.
		fctx, cancel := contextWithDialog(ctx, d.Context())
		defer cancel()
		if err := msess.FinalizeContext(fctx); err != nil {
			return err
		}
	}

	return nil
}

// contextWithDialog returns a context done when ctx or dialogCtx is done, for
// work a caller bounds with a context of its own that belongs to the call as
// well, and has to end when the call does.
func contextWithDialog(ctx context.Context, dialogCtx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(dialogCtx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// AckLate sends ACK with media. Use this in combination with late(delay) offer
// func (d *DialogClientSession) AckLate(ctx context.Context) error {
// 	return d.ack(ctx, d.mediaSession.LocalSDP())
// }

func (d *DialogClientSession) ack(ctx context.Context, remoteTarget sip.Uri, body []byte) error {
	// inviteRequest := d.InviteRequest
	// recipient := &inviteRequest.Recipient
	// if contact := d.InviteResponse.Contact(); contact != nil {
	// 	recipient = &contact.Address
	// }
	ackRequest := sip.NewRequest(
		sip.ACK,
		remoteTarget,
	)

	if body != nil {
		// This is delayed offer
		ackRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		ackRequest.SetBody(body)
	}

	if err := d.DialogClientSession.WriteAck(ctx, ackRequest); err != nil {
		return err
	}

	// Now dialog is established and can be add into store
	// if err := DialogsClientCache.DialogStore(ctx, d.ID, d); err != nil {
	// 	return err
	// }
	// d.OnClose(func() error {
	// 	return DialogsClientCache.DialogDelete(context.Background(), d.ID)
	// })
	return nil
}

// ReInvite sends new invite based on current media session
func (d *DialogClientSession) ReInvite(ctx context.Context) error {
	d.mu.Lock()
	sdp := d.mediaSession.LocalSDP()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.InviteRequest.Contact())
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(sdp)

	res, err := d.reInviteDo(ctx, req)
	if err != nil {
		return err
	}

	cont := res.Contact()
	if cont == nil {
		return fmt.Errorf("no contact header present")
	}

	ack := sip.NewRequest(sip.ACK, cont.Address)
	return d.WriteRequest(ack)
}

// reInviteDo sends req, a re-INVITE, until it is not answered 491, and
// acknowledges its 2xx. A final response other than 2xx is returned with
// sipgo.ErrDialogResponse.
func (d *DialogClientSession) reInviteDo(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	for {
		res, err := d.reInviteSend(ctx, req)
		if res == nil || res.StatusCode != sip.StatusRequestPending {
			return res, err
		}
		// We generated the Call-ID.
		if err := reInviteRetryWait(ctx, true); err != nil {
			return nil, err
		}
	}
}

// reInviteSend sends req, a re-INVITE, and acknowledges its 2xx. A final
// response other than 2xx is returned with sipgo.ErrDialogResponse.
func (d *DialogClientSession) reInviteSend(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	res, err := d.Do(ctx, req.Clone())
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
	if err := d.ack(ctx, cont.Address, nil); err != nil {
		return res, err
	}
	return res, nil
}

// reInviteMediaSession re-INVITEs with ms, a fork of the media session, and
// installs it with the answer.
func (d *DialogClientSession) reInviteMediaSession(ctx context.Context, ms *media.MediaSession) error {
	return d.reInviteMedia(ctx, func(*media.MediaSession) *media.MediaSession { return ms })
}

// reInviteMedia re-INVITEs with the fork of the installed media session that
// fork returns, and installs the fork with the answer. Each attempt waits
// first for a re-INVITE in progress to end, and marks ours in progress, see
// beginOwnMediaUpdate. A 491 is retried after the wait RFC 3261 section 14.1
// gives, with no re-INVITE of ours in progress meanwhile.
func (d *DialogClientSession) reInviteMedia(ctx context.Context, fork func(cur *media.MediaSession) *media.MediaSession) error {
	for {
		end, err := d.beginOwnMediaUpdate(ctx)
		if err != nil {
			return err
		}
		ms := fork(d.MediaSession())
		pending, err := d.reInviteMediaOnce(ctx, ms)
		end()
		if !pending {
			return err
		}
		// We generated the Call-ID.
		if err := reInviteRetryWait(ctx, true); err != nil {
			d.mu.Lock()
			derr := d.discardForkUnsafe(ms)
			d.mu.Unlock()
			return errors.Join(err, derr)
		}
	}
}

// reInviteMediaOnce sends one re-INVITE offering ms, and installs ms with the
// answer, unless the dialog media is closed or the dialog has ended by then.
// ms is discarded when it is not installed. It reports a 491, for the caller
// to retry.
func (d *DialogClientSession) reInviteMediaOnce(ctx context.Context, ms *media.MediaSession) (bool, error) {
	sdp := ms.LocalSDP()

	// NOTE: we do not change original invite request
	d.mu.Lock()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.InviteRequest.Contact())
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(sdp)

	res, err := d.reInviteSend(ctx, req)
	if res != nil && res.StatusCode == sip.StatusRequestPending {
		return true, nil
	}
	if err != nil {
		d.mu.Lock()
		derr := d.discardForkUnsafe(ms)
		d.mu.Unlock()
		return false, errors.Join(err, derr)
	}

	// Save new remote target contact and update media
	err = func() error {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.DialogMedia.closed || d.Context().Err() != nil {
			return errors.Join(errMediaClosed, d.discardForkUnsafe(ms))
		}
		d.remoteContactTarget = res.Contact()

		remoteSDP := res.Body()
		// The 200 OK answers the offer we sent on the re-INVITE.
		ms.RemoteSDPIsAnswer = true
		if err := ms.RemoteSDP(remoteSDP); err != nil {
			return errors.Join(fmt.Errorf("sdp update media remote SDP applying failed: %w", err), d.discardForkUnsafe(ms))
		}

		// The answer completes a new DTLS association, whose handshake runs
		// before ms carries any media.
		if ms.FinalizePending() {
			if err := d.finalizeAndReplaceUnsafe(d.Context(), ms); err != nil {
				if errors.Is(err, errMediaClosed) {
					return err
				}
				return fmt.Errorf("%w: %w", errMediaUpdateAfterAnswer, err)
			}
			return nil
		}
		if err := d.mediaUpdateUnsafe(ms); err != nil {
			return errors.Join(err, d.discardForkUnsafe(ms))
		}
		return nil
	}()
	if errors.Is(err, errMediaUpdateAfterAnswer) {
		return false, errors.Join(err, d.hangupNoMedia())
	}
	return false, err
}

// reInvites withs empty SDP are way to keep alive or do some post media update after receiving offer on 2xx
func (d *DialogClientSession) reInviteKeepAlive(ctx context.Context) error {
	// NOTE: we do not change original invite request
	d.mu.Lock()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.InviteRequest.Contact())

	res, err := d.reInviteDo(ctx, req)
	if err != nil {
		return err
	}

	// Save new remote target contact
	d.mu.Lock()
	d.remoteContactTarget = res.Contact()
	d.mu.Unlock()

	return nil
}

// Refer tries todo refer (blind transfer) on call. For more control use ReferOptions
//
// It blocks until the transfer outcome is known: the terminal sipfrag NOTIFY, or
// a 30s deadline. A transfer that did not succeed returns *ReferFailureError.
//
// NOTE: It is expected that after calling this you are hanguping call to send BYE
func (d *DialogClientSession) Refer(ctx context.Context, referTo sip.Uri, headers ...sip.Header) error {
	// cont := d.InviteRequest.Contact()
	// return dialogRefer(ctx, d, cont.Address, referTo, headers...)
	return d.ReferOptions(ctx, referTo, ReferClientOptions{
		Headers: headers,
	})
}

type ReferClientOptions struct {
	Headers []sip.Header
	// OnNotify sends notify status code.
	// If implemented you need to react on different status code.
	// Setting this returns from refer as soon as it is accepted, leaving the
	// transfer outcome to the callback.
	OnNotify func(statusCode int)
}

// ReferOptions sends a REFER. With OnNotify set it returns once the REFER is
// accepted and reports outcomes to the callback. Without one it blocks for the
// transfer outcome and returns *ReferFailureError if it did not succeed.
func (d *DialogClientSession) ReferOptions(ctx context.Context, referTo sip.Uri, opts ReferClientOptions) error {
	d.mu.Lock()
	cont := d.remoteContactUnsafe()
	if opts.OnNotify != nil {
		d.onReferNotify = opts.OnNotify
	}
	d.mu.Unlock()
	return dialogRefer(ctx, d, cont.Address, referTo, d.InviteResponse.Contact().Address, opts.OnNotify == nil, opts.Headers...)
}

// ReferAndObserve sends a blind REFER for referTo and returns what the far end
// sent back: the REFER's final response, the status line of each NOTIFY and how
// the wait ended. The wait for a final NOTIFY is bounded by opts.Deadline; a
// terminal NOTIFY that arrives after the wait goes to opts.OnLate. Neither this
// method nor any NOTIFY ends the dialog, so what follows the outcome is the
// caller's decision. Referred-By names this side: the Contact of the INVITE we
// sent. It installs no callback on the dialog, so it does not change what a
// later Refer or ReferOptions call reports.
func (d *DialogClientSession) ReferAndObserve(ctx context.Context, referTo sip.Uri, opts ReferObserveOptions) (ReferObservation, error) {
	d.mu.Lock()
	recipient := d.remoteContactUnsafe()
	ourContact := d.InviteRequest.Contact()
	d.mu.Unlock()
	if recipient == nil {
		return ReferObservation{}, fmt.Errorf("refer: dialog has no remote contact")
	}
	if ourContact == nil {
		return ReferObservation{}, fmt.Errorf("refer: dialog has no local contact")
	}
	obs, _, err := dialogReferObserve(ctx, d, recipient.Address, referTo, ourContact.Address, opts)
	return obs, err
}

func (d *DialogClientSession) handleReferNotify(req *sip.Request, tx sip.ServerTransaction) {
	if respondNotifyDialogEnded(d, req, tx) {
		return
	}
	dialogHandleReferNotify(d, req, tx)
}

func (d *DialogClientSession) handleRefer(dg *Diago, req *sip.Request, tx sip.ServerTransaction) {
	// A REFER asks to transfer this call, which an ended dialog no longer has.
	if ended, _ := respondDialogEnded(d, req, tx); ended {
		return
	}

	d.mu.Lock()
	onRefDialog := d.onReferDialog
	d.mu.Unlock()
	if onRefDialog == nil {
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
		return
	}

	dialogHandleRefer(d, dg, req, tx, onRefDialog)
}

// ReadBye reads the peer's BYE as the embedded session does. It does not wait
// for a re-INVITE being handled: a re-INVITE with no final response yet is
// answered 487 (RFC 3261 section 15.1.2), so none is answered 200 after the
// BYE that ended the dialog.
func (d *DialogClientSession) ReadBye(req *sip.Request, tx sip.ServerTransaction) error {
	d.answerMu.Lock()
	defer d.answerMu.Unlock()
	if err := d.DialogClientSession.ReadBye(req, tx); err != nil {
		return err
	}
	return d.terminatePeerReInviteLocked()
}

// awaitAck waits until Ack has returned before a re-INVITE of the peer's is
// handled, and reports whether it has. The peer may re-INVITE as soon as our
// ACK reaches it, while Ack still runs the DTLS handshake of the media the
// ACK started. A fork made then has no association to continue and would arm
// a second handshake on the socket the first one runs on; once Ack has
// returned, the fork continues the association Ack keyed. The wait ends with
// tx, or after two T1 intervals, which covers the handshake's last flight; RFC
// 5407 section 3.1.4 lets a re-INVITE crossing the ACK be answered 491, which
// asks the peer to retry.
func (d *DialogClientSession) awaitAck(tx sip.ServerTransaction) bool {
	d.mu.Lock()
	acking := d.acking
	d.mu.Unlock()
	if acking == nil {
		return true
	}

	timer := time.NewTimer(2 * sip.T1)
	defer timer.Stop()
	select {
	case <-acking:
		return true
	case <-tx.Done():
		return false
	case <-timer.C:
		return false
	}
}

func (d *DialogClientSession) handleReInvite(req *sip.Request, tx sip.ServerTransaction) error {
	if !d.awaitAck(tx) {
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil))
	}

	d.requestMu.Lock()
	defer d.requestMu.Unlock()

	// A re-INVITE for an ended dialog is answered 481 without reaching the
	// media. A live one is answered through respondPeerReInvite, or 487 by a
	// BYE that ends the dialog first.
	if answered, err := d.beginPeerReInvite(&d.Dialog, req, tx); answered {
		return err
	}
	defer d.endPeerReInvite(tx)

	if err := d.ReadRequest(req, tx); err != nil {
		_, err := d.respondPeerReInvite(tx, sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request - "+err.Error(), nil))
		return err
	}

	// RFC 3261 section 14.2: a re-INVITE arriving while one of ours is in
	// progress is answered 491.
	endUpdate, ok := d.beginPeerMediaUpdate()
	if !ok {
		_, err := d.respondPeerReInvite(tx, sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil))
		return err
	}
	defer endUpdate()

	if err := d.handleMediaUpdate(d.Context(), req, tx, d.InviteRequest.Contact()); err != nil {
		if errors.Is(err, errMediaUpdateAfterAnswer) {
			return errors.Join(err, d.hangupNoMedia())
		}
		return err
	}
	return nil
}

// hangupNoMedia ends a call that a failed media update left without media.
// The offer/answer exchange was complete by then, so the peer has moved to the
// new session. When that was a new DTLS association whose handshake failed,
// RFC 5763 section 5 has the media session torn down at once, and with a single
// audio stream that is the call.
func (d *DialogClientSession) hangupNoMedia() error {
	ctx, cancel := context.WithTimeout(d.Context(), 10*time.Second)
	defer cancel()
	return d.Hangup(ctx)
}

// handleReInviteACK reads the ACK to our 2xx to a re-INVITE of the peer's.
// When the re-INVITE carried no offer, the 2xx carried ours and the ACK
// carries the answer (RFC 3261 section 14.2), which is applied to the fork
// that made the offer, and the fork is installed. An answer that cannot be
// applied or keyed ends the call, since an ACK cannot be refused.
func (d *DialogClientSession) handleReInviteACK(req *sip.Request, tx sip.ServerTransaction) error {
	reInvite, err := d.applyAckAnswer(d.Context(), req)
	if err != nil {
		return errors.Join(err, d.hangupNoMedia())
	}
	if !reInvite {
		return nil
	}

	// The app callback runs without the lock, to avoid deadlocks.
	d.mu.Lock()
	onMediaUpdate := d.onMediaUpdate
	d.mu.Unlock()
	if onMediaUpdate != nil {
		onMediaUpdate(d.Media())
	}
	return nil
}

func (d *DialogClientSession) readSIPInfoDTMF(req *sip.Request, tx sip.ServerTransaction) error {
	if ended, err := respondDialogEnded(d, req, tx); ended {
		return err
	}
	return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
}

func (d *DialogClientSession) Hold(ctx context.Context) error {
	return d.reInviteMedia(ctx, func(cur *media.MediaSession) *media.MediaSession {
		m := cur.Fork()
		m.Mode = sdp.ModeSendonly
		return m
	})
}

func (d *DialogClientSession) Unhold(ctx context.Context) error {
	return d.reInviteMedia(ctx, func(cur *media.MediaSession) *media.MediaSession {
		m := cur.Fork()
		m.Mode = sdp.ModeSendrecv
		return m
	})
}
