// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	mrand "math/rand/v2"
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

	dialogCallbacks
	sessionState

	mediaConfig MediaConfig
	closed      atomic.Uint32
}

func (d *DialogClientSession) Close() error {
	if !d.closed.CompareAndSwap(0, 1) {
		return nil
	}
	e1 := d.closeCallbacks()
	e2 := d.leaveCache()
	e3 := d.DialogClientSession.Close()
	return errors.Join(e1, e2, e3)
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
	return d.remoteContact(d.InviteResponse.Contact())
}

func (d *DialogClientSession) remoteContactUnsafe() *sip.ContactHeader {
	return d.remoteContact(d.InviteResponse.Contact())
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

	// RTPNAT is media.MediaSession.RTPNAT, set before media starts
	// Check media.RTPNAT... options
	RTPNAT int
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
func (d *DialogClientSession) Invite(ctx context.Context, opts InviteClientOptions) (*DialogMedia, error) {
	conf := d.mediaConfig
	conf.rtpNAT = opts.RTPNAT

	med := &DialogMedia{}
	if err := med.initMediaSessionFromConf(ctx, conf); err != nil {
		return nil, err
	}
	med.registerDialogCallbacks(&d.dialogCallbacks)

	// NOTE: this can be racy
	d.Dialog.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			med.Close()
		}

		if s == sip.DialogStateConfirmed {
			// Do some finalize on ACK?
		}
	})

	if err := d.invite(ctx, med, opts); err != nil {
		if errors.Is(err, ErrClientEarlyMedia) {
			return med, err
		}
		med.Close()
		return nil, err
	}

	return med, nil
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
			if err := sess.RemoteSDP(body, false); err != nil {
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

	// This only gets called after session established
	med.onMediaUpdate = opts.OnMediaUpdate
	d.dialogCallbacks.mu.Lock()
	d.onReferDialog = opts.OnRefer
	d.dialogCallbacks.mu.Unlock()

	if err := d.doInvite(ctx, sess.LocalSDP(false)); err != nil {
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

func (d *DialogClientSession) doInvite(ctx context.Context, sd []byte) error {
	inviteReq := d.InviteRequest

	// We have to manually do INVITE
	dialogCli := d.UA
	inviteReq.AppendHeader(&dialogCli.ContactHDR)
	inviteReq.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	inviteReq.SetBody(sd)

	// We allow changing full from header, but we need to make sure it is correctly set
	if fromHDR := inviteReq.From(); fromHDR != nil {
		fromHDR.Params.Add("tag", sip.GenerateTagN(16))
	}

	// Build here request
	client := d.UA.Client
	if err := sipgo.ClientRequestBuild(client, inviteReq); err != nil {
		return err
	}

	// This only gets called after session established
	// d.onMediaUpdate = opts.OnMediaUpdate
	err := d.DialogClientSession.Invite(ctx, func(c *sipgo.Client, req *sip.Request) error {
		// Do nothing
		return nil
	})
	if err != nil {
		// sess.Close()
		return err
	}
	return nil
}

// WaitAnswer waits dialog on answer. It should only be used if you have error Invite but still want to continue
// ex. ErrClientEarlyMedia was returned but you want to proceed with answering
func (d *DialogClientSession) WaitAnswer(ctx context.Context, med *DialogMedia, opts sipgo.AnswerOptions) error {
	if med == nil {
		return fmt.Errorf("dialog media is not initialized")
	}
	return d.waitAnswer(ctx, med, opts)
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

		if err := sess.RemoteSDP(remoteSDP, true); err != nil {
			return err
		}

		if err := sess.Finalize(); err != nil {
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

	// Apply SDP on existing (Early) media if it exists
	if err := med.checkEarlyMedia(remoteSDP); err != errNoRTPSession {
		return err
	}

	if err := sess.RemoteSDP(remoteSDP, true); err != nil {
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

	if err := d.ack(ctx, recipient, nil); err != nil {
		return err
	}

	mediaHanshaker := d.mediaHanshakerLocked()
	if mediaHanshaker == nil {
		return nil
	}
	return mediaHanshaker.onFinalize(ctx)
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
	mediaHanshaker := d.mediaHanshakerLocked()
	if mediaHanshaker == nil {
		return fmt.Errorf("dialog media is not initialized")
	}
	// One re-INVITE at a time: ours waits for one of the peer's to end, and
	// one of the peer's arriving meanwhile is answered 491.
	endUpdate, err := d.beginOwnMediaUpdate(ctx, &d.Dialog)
	if err != nil {
		return err
	}
	defer endUpdate()
	sdp, err := mediaHanshaker.onLocalSDP(ctx, false, "")
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			d.dialogCallbacks.abortMedia()
		}
	}()
	contact := d.RemoteContact()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.InviteRequest.Contact())
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(sdp)

	res, err := d.reInviteDo(ctx, req)
	if err != nil {
		return err
	}
	if err := mediaHanshaker.onRemoteSDP(ctx, res.Body(), true); err != nil {
		return err
	}
	d.setRemoteContact(res.Contact())
	committed = true
	return nil
}

func (d *DialogClientSession) reInviteDo(ctx context.Context, req *sip.Request) (*sip.Response, error) {

	for {
		res, err := d.Do(ctx, req.Clone())
		if err != nil {
			return nil, err
		}

		if !res.IsSuccess() {
			// https://datatracker.ietf.org/doc/html/rfc3261#section-14.1
			// If a UAC receives a 491 response to a re-INVITE, it SHOULD start a
			//    timer with a value T chosen as follows:
			//       1. If the UAC is the owner of the Call-ID of the dialog ID
			//          (meaning it generated the value), T has a randomly chosen value
			//          between 2.1 and 4 seconds in units of 10 ms.

			//       2. If the UAC is not the owner of the Call-ID of the dialog ID, T
			//          has a randomly chosen value of between 0 and 2 seconds in units
			//          of 10 ms.

			if res.StatusCode == sip.StatusRequestPending {
				select {
				case <-time.After(time.Duration(2000+mrand.IntN(200)*10) * time.Millisecond):
					continue
				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}

			return nil, sipgo.ErrDialogResponse{
				Res: res,
			}
		}

		// Now do ACK on new Contact
		if err := d.ack(ctx, res.Contact().Address, nil); err != nil {
			return res, err
		}

		return res, nil
	}
}

// reInvites withs empty SDP are way to keep alive or do some post media update after receiving offer on 2xx
func (d *DialogClientSession) reInviteKeepAlive(ctx context.Context) error {
	// NOTE: we do not change original invite request
	if d.mediaHanshakerLocked() == nil {
		return fmt.Errorf("dialog media is not initialized")
	}
	contact := d.RemoteContact()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.InviteRequest.Contact())

	res, err := d.reInviteDo(ctx, req)
	if err != nil {
		return err
	}

	// Save new remote target contact
	d.setRemoteContact(res.Contact())

	return nil
}

// Refer tries todo refer (blind transfer) on call. For more control use ReferOptions
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
	OnNotify func(statusCode int)
}

func (d *DialogClientSession) ReferOptions(ctx context.Context, referTo sip.Uri, opts ReferClientOptions) error {
	cont := d.RemoteContact()
	d.dialogCallbacks.mu.Lock()
	if opts.OnNotify != nil {
		d.onReferNotify = opts.OnNotify
	}
	d.dialogCallbacks.mu.Unlock()
	return dialogRefer(ctx, d, cont.Address, referTo, d.InviteResponse.Contact().Address, opts.Headers...)
}

func (d *DialogClientSession) handleReferNotify(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) {
	d.dialogCallbacks.mu.Lock()
	onReferNotify := d.onReferNotify
	d.dialogCallbacks.mu.Unlock()
	dialogRouteReferNotify(ctx, d, req, tx, onReferNotify)
}

func (d *DialogClientSession) handleRefer(ctx context.Context, dg *Diago, req *sip.Request, tx sip.ServerTransaction) {
	// A REFER asks to transfer this call, which an ended dialog no longer has.
	if res := dialogEndedResponse(d, req); res != nil {
		tx.Respond(ctx, res)
		return
	}

	d.dialogCallbacks.mu.Lock()
	onRefDialog := d.onReferDialog
	d.dialogCallbacks.mu.Unlock()
	if onRefDialog == nil {
		tx.Respond(ctx, sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
		return
	}

	dialogHandleRefer(ctx, d, dg, req, tx, onRefDialog)
}

func (d *DialogClientSession) handleReInvite(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) (err error) {
	d.requestMu.Lock()
	defer d.requestMu.Unlock()

	// After our BYE an ended dialog stays in the cache until it is closed, so
	// a re-INVITE can still find it. It is answered 481 without reaching the
	// media. A live one is answered through respondPeerReInvite, or 487 by a
	// BYE that ends the dialog first.
	if answered, err := d.beginPeerReInvite(ctx, &d.Dialog, &d.dialogCallbacks, req, tx); answered {
		return err
	}
	// Once a 2xx has been sent, its ACK is waited for, and only then is the
	// re-INVITE no longer in progress. A 2xx whose ACK does not come ends the
	// call (RFC 3261 section 13.3.1.4). The media commits the update when that
	// ACK is read, so a re-INVITE that ends without it has the media drop what
	// the offer left staged.
	var endUpdate func()
	offered := false
	defer func() {
		acked, ackErr := d.endPeerReInvite(tx)
		if offered && !acked {
			d.abortMedia()
		}
		if ackErr != nil {
			err = errors.Join(err, ackErr, d.hangupNoMedia())
		}
		if endUpdate != nil {
			endUpdate()
		}
	}()

	if err := d.ReadRequest(req, tx); err != nil {
		_, err := d.respondPeerReInvite(ctx, tx, sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request - "+err.Error(), nil))
		return err
	}

	// RFC 3261 section 14.2: a re-INVITE arriving while one of ours is in
	// progress is answered 491.
	var ok bool
	endUpdate, ok = d.beginPeerMediaUpdate()
	if !ok {
		_, err := d.respondPeerReInvite(ctx, tx, sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil))
		return err
	}

	mediaHanshaker := d.mediaHanshakerLocked()
	if mediaHanshaker == nil {
		_, err := d.respondPeerReInvite(ctx, tx, sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
		return err
	}

	offered = true
	if err := mediaHanshaker.onRemoteSDP(d.Context(), req.Body(), false); err != nil {
		return d.refusePeerReInvite(ctx, req, tx, err)
	}
	localSDP, err := mediaHanshaker.onLocalSDP(d.Context(), true, "")
	if err != nil {
		return d.refusePeerReInvite(ctx, req, tx, err)
	}
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", localSDP)
	res.AppendHeader(d.InviteRequest.Contact())
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	// The remote target moves to the re-INVITE's Contact as this 2xx is sent,
	// and stays where it was when the re-INVITE is refused or a BYE ends it.
	_, err = d.respondPeerReInvite(ctx, tx, res)
	return err
}

// handleReInviteACK reads the ACK to our 2xx to a re-INVITE of the peer's: it
// ends the retransmission of that 2xx, hands the media the answer the ACK
// carries, if any, and has it commit the update, which the handling of the
// re-INVITE waits for.
//
// An ACK cannot be refused, so media that the ACK leaves unusable ends the
// call with a BYE.
func (d *DialogClientSession) handleReInviteACK(req *sip.Request, tx sip.ServerTransaction) error {
	// A copy of an ACK already read, sent for a retransmission of our 2xx,
	// brings the media nothing more.
	ackHandled, first := d.ackPeerReInvite(req)
	if !first {
		return nil
	}
	defer ackHandled()

	mediaHanshaker := d.mediaHanshakerLocked()
	if mediaHanshaker == nil {
		return nil
	}
	var err error
	if body := req.Body(); body != nil {
		err = mediaHanshaker.onRemoteSDP(d.Context(), body, true)
	}
	if err == nil {
		err = mediaHanshaker.onFinalize(d.Context())
	}
	if err != nil {
		// Nothing was installed: the media drops what the exchange left
		// staged.
		d.abortMedia()
		return errors.Join(err, d.hangupNoMedia())
	}
	return nil
}

func (d *DialogClientSession) readSIPInfoDTMF(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) error {
	if res := dialogEndedResponse(d, req); res != nil {
		return tx.Respond(ctx, res)
	}
	// DTMF relay is not read yet, so an INFO on a live dialog is refused
	// whatever it carries, without a body and so without a Content-Type (RFC
	// 3261 section 20.15) too.
	return tx.Respond(ctx, sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
}

func (d *DialogClientSession) Hold(ctx context.Context) error {
	mediaHanshaker := d.mediaHanshakerLocked()
	if mediaHanshaker == nil {
		return fmt.Errorf("dialog media is not initialized")
	}
	if err := d.reInviteMode(ctx, mediaHanshaker, sdp.ModeSendonly); err != nil {
		return err
	}
	return nil
}

func (d *DialogClientSession) Unhold(ctx context.Context) error {
	mediaHanshaker := d.mediaHanshakerLocked()
	if mediaHanshaker == nil {
		return fmt.Errorf("dialog media is not initialized")
	}
	if err := d.reInviteMode(ctx, mediaHanshaker, sdp.ModeSendrecv); err != nil {
		return err
	}
	return nil
}

func (d *DialogClientSession) reInviteMode(
	ctx context.Context,
	mediaHanshaker mediaHanshaker,
	mode string,
) error {
	// One re-INVITE at a time: ours waits for one of the peer's to end, and
	// one of the peer's arriving meanwhile is answered 491.
	endUpdate, err := d.beginOwnMediaUpdate(ctx, &d.Dialog)
	if err != nil {
		return err
	}
	defer endUpdate()
	sdp, err := mediaHanshaker.onLocalSDP(ctx, false, mode)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			d.dialogCallbacks.abortMedia()
		}
	}()
	contact := d.RemoteContact()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(d.InviteRequest.Contact())
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(sdp)

	res, err := d.reInviteDo(ctx, req)
	if err != nil {
		return err
	}
	if err := mediaHanshaker.onRemoteSDP(ctx, res.Body(), true); err != nil {
		return err
	}
	d.setRemoteContact(res.Contact())
	committed = true
	return nil
}
