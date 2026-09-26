// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newDialer(ua *sipgo.UserAgent) *Diago {
	return NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))
}

func newDiagoClientTest(ua *sipgo.UserAgent, onRequest func(req *sip.Request) *sip.Response) *Diago {
	// Create client transaction request
	cTxReq := &clientTxRequester{
		onRequest: onRequest,
	}

	client, _ := sipgo.NewClient(ua)
	client.TxRequester = cTxReq
	return NewDiago(ua, WithClient(client))
}

func dialogEcho(sess DialogSession) error {
	audioR, err := sess.Media().AudioReader()
	if err != nil {
		return err
	}

	audioW, err := sess.Media().AudioWriter()
	if err != nil {
		return err
	}

	_, err = media.Copy(audioR, audioW)
	if err != nil {
		return err
	}
	return nil
}

func TestIntegrationDialogClient(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Create transaction users, as many as needed.
	ua, _ := sipgo.NewUA(
		sipgo.WithUserAgent("inbound"),
	)
	defer ua.Close()

	dg := NewDiago(ua)

	err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
		// t.Log("Call received", d.InviteRequest)
		// Add some routing
		if d.ToUser() == "alice" {
			d.Trying()
			d.Ringing()
			d.Answer()

			dialogEcho(d)
			<-d.Context().Done()
			return
		}

		if d.ToUser() == "hanguper" {
			d.Trying()
			d.Answer()
			d.Hangup(d.Context())
			return
		}

		d.Respond(sip.StatusForbidden, "Forbidden", nil)

		<-d.Context().Done()
	})
	require.NoError(t, err)

	t.Run("HanguperClientNoServe", func(t *testing.T) {
		// We want to confirm that diago can receive BYE without Binding to IP, which will reflect Contact Header
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		// Has no listener just UAC. Contact will hold empheral port
		phone := newDialer(ua)
		// Hanguped
		dialog, err := phone.Invite(context.TODO(), sip.Uri{User: "hanguper", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.NoError(t, err)
		<-dialog.Context().Done()
	})

	t.Run("HanguperClientWithServe", func(t *testing.T) {
		// We want to confirm that diago can receive BYE on Binded IP
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		phone := newDialer(ua)
		// listening but stil with empheral port
		err := phone.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
		require.NoError(t, err)

		ports := phone.server.TransportLayer().ListenPorts("udp")
		require.Len(t, ports, 1)
		// Hanguped
		dialog, err := phone.Invite(context.TODO(), sip.Uri{User: "hanguper", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.NoError(t, err)
		<-dialog.Context().Done()
		assert.Equal(t, dialog.InviteRequest.Via().Port, dialog.InviteRequest.Contact().Address.Port)
	})

	t.Run("Dialer", func(t *testing.T) {
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		phone := newDialer(ua)
		// Start listener in order to reuse UDP listener
		err := phone.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
		require.NoError(t, err)

		phone.server.TransportLayer().ListenPorts("udp")

		// Forbiddden
		_, err = phone.Invite(context.TODO(), sip.Uri{User: "noroute", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.Error(t, err)

		// Hanguped
		dialog, err := phone.Invite(context.TODO(), sip.Uri{User: "hanguper", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.NoError(t, err)
		<-dialog.Context().Done()

		// Answered call
		dialog, err = phone.Invite(context.TODO(), sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.NoError(t, err)
		defer dialog.Close()

		// Confirm media traveling
		audioR, err := dialog.AudioReader()
		require.NoError(t, err)

		audioW, err := dialog.AudioWriter()
		require.NoError(t, err)

		writeN, _ := audioW.Write([]byte("my audio"))
		readN, _ := audioR.Read(make([]byte, 100))
		assert.Equal(t, writeN, readN, "media echo failed")
		dialog.Hangup(ctx)
	})
}

func TestIntegrationDialogClientCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	port := 15000 + rand.IntN(999)
	dg := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  port,
		},
	))

	dg.ServeBackground(ctx, func(d *DialogServerSession) {
		ctx := d.Context()
		d.Trying()
		d.Ringing()

		<-ctx.Done()
	})

	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := newDialer(ua)
		dg.ServeBackground(context.TODO(), func(d *DialogServerSession) {})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, err := dg.Invite(ctx, sip.Uri{User: "test", Host: "127.0.0.1", Port: port}, InviteOptions{
			OnResponse: func(res *sip.Response) error {
				if res.StatusCode == sip.StatusRinging {
					cancel()
					// return context.Canceled
				}
				return nil
			},
		})
		require.ErrorIs(t, err, context.Canceled)
	}

}

func TestIntegrationDialogClientEarlyMedia(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15060,
			},
		))

		authServer := NewDigestServer()
		log := asyncLog(t)
		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			log("Call received")

			err := authServer.AuthorizeDialog(d, DigestAuth{
				Username: "test",
				Password: "test123",
				Realm:    "",
				Expire:   10 * time.Second,
			})
			if err != nil {
				log("Failed to authorize", "error", err)
				return
			}

			d.Trying()
			if err := d.ProgressMedia(); err != nil {
				log("Failed to progress media", err)
				return
			}

			// Write frame
			w, _ := d.AudioWriter()
			if _, err := w.Write(bytes.Repeat([]byte{0, 100}, 80)); err != nil {
				log("Failed to write frame", err)
				return
			}

			if err := d.Answer(); err != nil {
				log("Failed to answer", err)
				return
			}
			return
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := newDialer(ua)
	err := dg.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
	require.NoError(t, err)

	dialog, err := dg.NewDialog(sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15060}, NewDialogOptions{})
	require.NoError(t, err)
	defer dialog.Close()

	err = dialog.Invite(ctx, InviteClientOptions{
		EarlyMediaDetect: true,
		Username:         "test",
		Password:         "test123",
	})
	require.ErrorIs(t, err, ErrClientEarlyMedia)

	// Now we should be able to read media
	r, err := dialog.AudioReader()
	require.NoError(t, err)

	// Read early media in background
	var earlyMediaBuf []byte
	doneEarly := make(chan struct{})
	go func() {
		defer close(doneEarly)
		earlyMediaBuf, _ = media.ReadAll(r, 160)
	}()

	err = dialog.WaitAnswer(ctx, sipgo.AnswerOptions{})
	require.NoError(t, err)
	dialog.Ack(ctx)

	<-dialog.Context().Done()
	<-doneEarly
	assert.Len(t, earlyMediaBuf, 160) // 1 frame
}

func TestIntegrationDialogClientReinvite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15060,
			},
		))
		log := asyncLog(t)
		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			log("Call received")
			d.AnswerOptions(AnswerOptions{OnMediaUpdate: func(d *DialogMedia) {

			}})
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := newDialer(ua)
	err := dg.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
	require.NoError(t, err)

	dialog, err := dg.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15060}, InviteOptions{})
	require.NoError(t, err)

	err = dialog.ReInvite(ctx)
	require.NoError(t, err)

	dialog.Hangup(ctx)
}

func TestIntegrationDialogClientReinviteKeepAlive(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15066,
			},
		))
		log := asyncLog(t)
		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			log("Call received")
			d.AnswerOptions(AnswerOptions{OnMediaUpdate: func(d *DialogMedia) {

			}})
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := newDialer(ua)
	err := dg.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
	require.NoError(t, err)

	dialog, err := dg.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15066}, InviteOptions{})
	require.NoError(t, err)

	// Update now with full media
	err = dialog.reInviteKeepAlive(ctx)
	require.NoError(t, err)

	dialog.Hangup(ctx)
}

func TestIntegrationDialogClientReinviteMedia(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	beep, _ := audio.BeepLoadPCM(media.CodecAudioUlaw)
	numPkts := len(beep) / media.CodecAudioUlaw.Samples16()

	t.Log("Size beep", len(beep), numPkts)
	audioReceived := make(chan []byte, 1)
	// The handler runs on a server goroutine, so the result of its re-INVITE
	// is handed to the test rather than asserted there.
	reinvited := make(chan error, 1)
	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15079,
			},
		))
		digServer := NewDigestServer()
		log := asyncLog(t)
		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			log("New INVITE")
			if err := digServer.AuthorizeDialog(d, DigestAuth{
				Username: "test",
				Password: "test",
			}); err != nil {
				return
			}

			d.AnswerOptions(AnswerOptions{OnMediaUpdate: func(d *DialogMedia) {
				// fmt.Println("Server media update", d)
			}})

			// ar, _ := d.AudioReader()
			ar := d.RTPPacketReader
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				defer cancel()
				beepEncoded, _ := media.ReadAll(ar, 160)
				audioReceived <- beepEncoded
			}()

			time.Sleep(60 * time.Millisecond)
			var err error
			ms := d.MediaSession().Fork()
			ms.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 39999}
			err = ms.Init() // This will start new listener
			if err != nil {
				reinvited <- fmt.Errorf("new media session: %w", err)
				return
			}

			err = d.reInviteMediaSession(ctx, ms)
			reinvited <- err
			if err != nil {
				return
			}

			// beepEncoded, _ := media.ReadAll(ar, 160)
			// audioReceived <- beepEncoded
			<-ctx.Done()
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := newDialer(ua)
	// err := dg.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
	// require.NoError(t, err)
	dialog, err := dg.NewDialog(sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15079}, NewDialogOptions{})
	require.NoError(t, err)
	err = dialog.Invite(ctx, InviteClientOptions{
		OnMediaUpdate: func(d *DialogMedia) {
			fmt.Println("Media update", d)
		},
		Username: "test",
		Password: "test",
	})
	require.NoError(t, err)
	err = dialog.Ack(ctx)
	require.NoError(t, err)

	require.NoError(t, err)
	pb, _ := dialog.PlaybackCreate()
	_, err = pb.Play(bytes.NewBuffer(beep), "audio/pcm")
	require.NoError(t, err)

	// The server's re-INVITE completes before the hangup, which it would
	// otherwise race on a loaded machine.
	select {
	case err := <-reinvited:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the server's re-INVITE did not complete")
	}
	err = dialog.Hangup(ctx)
	require.NoError(t, err)
	var remoteAudio []byte
	select {
	case remoteAudio = <-audioReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("the server never finished reading the call audio")
	}

	// 1 packet will not be consumed due to update of RTP packets
	assert.GreaterOrEqual(t, len(remoteAudio)/160, numPkts-1)
}

func TestDialogClientInviteFailed(t *testing.T) {
	reqCh := make(chan *sip.Request, 1)
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		reqCh <- req
		return sip.NewResponseFromRequest(req, 500, "", nil)
	})

	// invite sends the INVITE and returns it once it is out. The call fails on
	// the 500, which is waited for before the subtest ends.
	invite := func(t *testing.T, opts InviteClientOptions) *sip.Request {
		t.Helper()
		dialog, err := dg.NewDialog(sip.Uri{User: "alice", Host: "localhost"}, NewDialogOptions{})
		require.NoError(t, err)
		t.Cleanup(func() { _ = dialog.Close() })

		invited := make(chan error, 1)
		go func() { invited <- dialog.Invite(context.Background(), opts) }()
		t.Cleanup(func() {
			select {
			case err := <-invited:
				assert.Error(t, err, "the INVITE was answered 500")
			case <-time.After(5 * time.Second):
				t.Error("the INVITE did not return")
			}
		})

		select {
		case req := <-reqCh:
			return req
		case <-time.After(5 * time.Second):
			require.FailNow(t, "the INVITE was never sent")
			return nil
		}
	}

	t.Run("WithCallerid", func(t *testing.T) {
		opts := InviteClientOptions{}
		opts.WithCaller("Test", "123456", "example.com")
		req := invite(t, opts)
		assert.Equal(t, "Test", req.From().DisplayName)
		assert.Equal(t, "123456", req.From().Address.User)
		assert.NotEmpty(t, req.From().Params.GetOr("tag", ""))
	})

	t.Run("WithAnonymous", func(t *testing.T) {
		opts := InviteClientOptions{}
		opts.WithAnonymousCaller()
		req := invite(t, opts)
		assert.Equal(t, "Anonymous", req.From().DisplayName)
		assert.Equal(t, "anonymous", req.From().Address.User)
		assert.NotEmpty(t, req.From().Params.GetOr("tag", ""))
	})
}

// TestDialogClientReInviteACKReadsMediaUnderLock asserts the ACK to a peer's
// re-INVITE reads the media session under the dialog lock. Handling another
// in-dialog offer swaps the session under that lock, so an unguarded read races
// it. The race only shows under -race. The dialog is a real one, since the
// ACK's handshake is bounded by its context.
func TestDialogClientReInviteACKReadsMediaUnderLock(t *testing.T) {
	d := newTestClientDialog(t, sip.DialogStateConfirmed)
	d.mediaSession = &media.MediaSession{}

	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		d.mu.Lock()
		d.mediaSession = &media.MediaSession{}
		d.mu.Unlock()
	}()

	ack := sip.NewRequest(sip.ACK, sip.Uri{User: "dg", Host: "127.0.0.1"})
	require.NoError(t, d.handleReInviteACK(ack, nil))
	<-swapped
}

// newTestClientDialog builds an outgoing dialog in the given state, without a
// transport.
func newTestClientDialog(t *testing.T, state sip.DialogState) *DialogClientSession {
	t.Helper()

	us := sip.Uri{User: "bob", Host: "127.0.0.2", Port: 5060}
	peer := sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}

	invite := sip.NewRequest(sip.INVITE, peer)
	invite.AppendHeader(&sip.ContactHeader{Address: us})
	fromParams := sip.NewParams()
	fromParams.Add("tag", "caller-tag")
	invite.AppendHeader(&sip.FromHeader{Address: us, Params: fromParams})
	invite.AppendHeader(&sip.ToHeader{Address: peer, Params: sip.NewParams()})
	invite.AppendHeader(sip.NewHeader("Call-ID", "client-reinvite-test-call-id"))
	invite.AppendHeader(&sip.CSeqHeader{SeqNo: 1, MethodName: sip.INVITE})

	d := &DialogClientSession{DialogClientSession: &sipgo.DialogClientSession{
		Dialog: sipgo.Dialog{InviteRequest: invite},
	}}
	d.InitWithState(state)
	return d
}

// newPeerReInvite builds a body-less re-INVITE the peer sends inside d.
func newPeerReInvite(d *DialogClientSession, seq uint32) *sip.Request {
	inv := d.InviteRequest
	toParams := sip.NewParams()
	toParams.Add("tag", "callee-tag")
	req := sip.NewRequest(sip.INVITE, inv.Contact().Address)
	req.AppendHeader(&sip.FromHeader{Address: inv.To().Address, Params: toParams})
	req.AppendHeader(&sip.ToHeader{Address: inv.From().Address, Params: inv.From().Params.Clone()})
	req.AppendHeader(sip.HeaderClone(inv.CallID()))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: sip.INVITE})
	req.AppendHeader(&sip.ContactHeader{Address: inv.To().Address})
	return req
}

// TestDialogClientReInviteEndedDialog pins how a peer's re-INVITE for an
// outgoing dialog that is no longer live is answered. After our BYE the dialog
// stays in the cache until it is closed, so the request still finds it. It
// matches no live dialog, and is answered 481 (RFC 3261 section 12.2.2)
// without reaching the media. A re-INVITE still pending when a BYE tears the
// media down gets 487 (RFC 3261 section 15.1.2) instead of an answer built on
// closed media.
func TestDialogClientReInviteEndedDialog(t *testing.T) {
	t.Run("ended", func(t *testing.T) {
		d := newTestClientDialog(t, sip.DialogStateEnded)
		newTestMediaSession(t, &d.DialogMedia)

		tx := newByeServerTx()
		require.NoError(t, d.handleReInvite(newPeerReInvite(d, 5), tx))
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
		assert.Nil(t, d.remoteContactTarget, "the re-INVITE reached the media")
	})

	t.Run("media closed", func(t *testing.T) {
		d := newTestClientDialog(t, sip.DialogStateConfirmed)
		newTestMediaSession(t, &d.DialogMedia)
		require.NoError(t, d.DialogMedia.Close())

		tx := newByeServerTx()
		require.NoError(t, d.handleReInvite(newPeerReInvite(d, 5), tx))
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusRequestTerminated, tx.responses[0].StatusCode)
	})
}

func TestIntegrationDialogClientBadMediaNegotiation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	lock := sync.Mutex{}
	requests := []sip.Message{}
	responses := []sip.Message{}

	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()
		ua.TransportLayer().OnMessage(func(msg sip.Message) {
			lock.Lock()
			defer lock.Unlock()
			requests = append(requests, msg)
		})

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15060,
			},
		),
		)

		log := asyncLog(t)
		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			log("Call received")
			if err := d.Answer(); err != nil {
				log("Error on answer", err)
				return
			}
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	ua.TransportLayer().OnMessage(func(msg sip.Message) {
		// The server transaction sends 100 Trying whenever the answer takes
		// over 200 ms (RFC 3261 section 17.2.1), so only final responses count.
		if res, ok := msg.(*sip.Response); ok && res.IsProvisional() {
			return
		}
		lock.Lock()
		defer lock.Unlock()
		responses = append(responses, msg)
	})

	dg := newDialer(ua)
	err := dg.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
	require.NoError(t, err)

	// Media negotiaton should fail and call should be terminated
	_, err = dg.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15060}, InviteOptions{
		OnResponse: func(res *sip.Response) error {
			// Fake Bad SDP
			res.SetBody([]byte("Bad SDP"))
			return nil
		},
	})
	t.Log(err)
	require.Error(t, err)

	// The transport hands each message to the transaction layer before these
	// hooks record it, so Invite can return before the last one is recorded.
	require.Eventually(t, func() bool {
		lock.Lock()
		defer lock.Unlock()
		return len(requests) >= 3 && len(responses) >= 2
	}, 5*time.Second, 10*time.Millisecond)

	lock.Lock()
	defer lock.Unlock()
	require.Len(t, requests, 3)
	require.Len(t, responses, 2)

	// Termination of dialog should be this correct
	assert.EqualValues(t, "INVITE", requests[0].(*sip.Request).Method)
	assert.EqualValues(t, 200, responses[0].(*sip.Response).StatusCode)
	assert.EqualValues(t, "ACK", requests[1].(*sip.Request).Method)
	assert.EqualValues(t, "BYE", requests[2].(*sip.Request).Method)
	assert.EqualValues(t, 200, responses[1].(*sip.Response).StatusCode)
}

func TestIntegrationDialogClientRefer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15071,
				ID:        "udp",
			},
		))

		log := asyncLog(t)
		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			log("Call received")
			d.AnswerOptions(AnswerOptions{
				OnRefer: func(referDialog *DialogClientSession) error {
					if err := referDialog.Invite(referDialog.Context(), InviteClientOptions{}); err != nil {
						return err
					}
					if err := referDialog.Ack(ctx); err != nil {
						return err
					}
					return referDialog.Hangup(referDialog.Context())
				},
			})
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	// UAS that accepts REFER
	// waitReferDialog := make(chan *DialogServerSession)
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15072,
			},
		))

		log := asyncLog(t)
		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			log("Call INVITE due to REFER received")
			// waitReferDialog <- d
			switch d.ToUser() {
			case "busy":
				d.Respond(sip.StatusBusyHere, "Busy Here", nil)
				return
			case "noanswer":
				d.Ringing()
				return
			default:
				d.Answer()
			}

			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  15070,
		},
	))

	err := dg.ServeBackground(ctx, nil)
	require.NoError(t, err)

	t.Run("Succesfull", func(t *testing.T) {
		d, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15071}, InviteOptions{})
		require.NoError(t, err)
		defer d.Close()
		defer d.Hangup(d.Context())

		referState := make(chan int)
		err = d.ReferOptions(d.Context(), sip.Uri{Host: "127.0.0.1", Port: 15072}, ReferClientOptions{
			OnNotify: func(statusCode int) {
				referState <- statusCode
			},
		})
		require.NoError(t, err)

		assert.Equal(t, 100, <-referState)
		assert.Equal(t, 200, <-referState)
	})

	t.Run("UnreachableRefer", func(t *testing.T) {
		d, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15071}, InviteOptions{})
		require.NoError(t, err)
		defer d.Close()
		defer d.Hangup(d.Context())

		referState := make(chan int)
		err = d.ReferOptions(d.Context(), sip.Uri{User: "noanswer", Host: "127.0.0.1", Port: 15072}, ReferClientOptions{
			OnNotify: func(statusCode int) {
				referState <- statusCode
			},
		})
		require.NoError(t, err)

		assert.Equal(t, 100, <-referState)
		assert.Equal(t, sip.StatusTemporarilyUnavailable, <-referState)
	})

	t.Run("BusyRefer", func(t *testing.T) {
		d, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15071}, InviteOptions{})
		require.NoError(t, err)
		defer d.Close()
		defer d.Hangup(d.Context())

		referState := make(chan int)
		err = d.ReferOptions(d.Context(), sip.Uri{User: "busy", Host: "127.0.0.1", Port: 15072}, ReferClientOptions{
			OnNotify: func(statusCode int) {
				referState <- statusCode
			},
		})
		require.NoError(t, err)

		assert.Equal(t, 100, <-referState)
		assert.Equal(t, sip.StatusBusyHere, <-referState)
	})
}

// heldReInvites is a client that answers every request 200 without a
// transport, the INVITE with peerSDP, and holds each INVITE after the first
// until the test releases it.
type heldReInvites struct {
	mu      sync.Mutex
	log     []string
	invites int
	// requested receives each held INVITE, and release lets it be answered.
	requested chan struct{}
	release   chan struct{}
}

func newHeldReInvites() *heldReInvites {
	return &heldReInvites{requested: make(chan struct{}, 4), release: make(chan struct{}, 4)}
}

func (h *heldReInvites) record(entry string) {
	h.mu.Lock()
	h.log = append(h.log, entry)
	h.mu.Unlock()
}

func (h *heldReInvites) entries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.log...)
}

func (h *heldReInvites) onRequest(req *sip.Request) *sip.Response {
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	if !req.IsInvite() {
		return res
	}
	h.mu.Lock()
	h.invites++
	held := h.invites > 1
	h.mu.Unlock()
	if held {
		h.record("our re-INVITE")
		h.requested <- struct{}{}
		select {
		case <-h.release:
		case <-time.After(10 * time.Second):
		}
	}
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.SetBody(peerSDP())
	return res
}

// newHeldReInviteDialog returns an outgoing call, invited, answered with
// peerSDP and acknowledged, whose later INVITEs h holds.
func newHeldReInviteDialog(t *testing.T) (*DialogClientSession, *heldReInvites) {
	t.Helper()
	h := newHeldReInvites()
	dg := testDiagoClient(t, h.onRequest)
	ctx := context.Background()
	d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.Invite(ctx, InviteClientOptions{}))
	require.NoError(t, d.Ack(ctx))
	return d, h
}

// TestDialogReInviteAnsweredAfterMediaClosed is a re-INVITE of ours, moving the
// media to sockets of its own, whose 2xx arrives after the dialog media was
// closed. Close has taken what it closes by then, so a fork installed after it
// would keep its sockets open for good. The fork is not installed and its
// sockets are released.
func TestDialogReInviteAnsweredAfterMediaClosed(t *testing.T) {
	d, h := newHeldReInviteDialog(t)
	fork := d.MediaSession().Fork()
	fork.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	require.NoError(t, fork.Init())
	sess := d.MediaSession()

	reinvited := make(chan error, 1)
	go func() { reinvited <- d.reInviteMediaSession(context.Background(), fork) }()
	select {
	case <-h.requested:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not sent")
	}
	require.NoError(t, d.DialogMedia.Close())
	h.release <- struct{}{}

	select {
	case err := <-reinvited:
		require.ErrorIs(t, err, errMediaClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE did not return")
	}
	require.True(t, sess == d.MediaSession(), "a fork was installed on closed media")
	reuse, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: fork.Laddr.Port})
	require.NoError(t, err, "the fork's socket must be released")
	require.NoError(t, reuse.Close())
}

// TestDialogReInviteWaitsForPeerMediaUpdate is a Hold made while a re-INVITE
// of the peer's is being handled, its media update callback still running.
// RFC 3261 section 14.1 has a UAC start no re-INVITE while another INVITE
// transaction is in progress in either direction, so our re-INVITE goes out
// once the peer's has been answered, and forks the session the peer's
// installed.
func TestDialogReInviteWaitsForPeerMediaUpdate(t *testing.T) {
	d, h := newHeldReInviteDialog(t)
	peerReInvite := newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1)
	peerReInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	peerReInvite.SetBody(peerSDP())

	inCallback := make(chan struct{})
	releaseCallback := make(chan struct{})
	d.mu.Lock()
	d.onMediaUpdate = func(*DialogMedia) {
		close(inCallback)
		select {
		case <-releaseCallback:
		case <-time.After(10 * time.Second):
		}
	}
	d.mu.Unlock()

	handled := make(chan error, 1)
	go func() {
		handled <- d.handleReInvite(peerReInvite, newAckedServerTx(&loggedPeerTx{byeServerTx: newByeServerTx(), h: h}, func() {
			_ = d.handleReInviteACK(newReInviteAck(peerReInvite, nil), newByeServerTx())
		}))
	}()
	select {
	case <-inCallback:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer's media update did not start")
	}

	held := make(chan error, 1)
	go func() { held <- d.Hold(context.Background()) }()
	// Our re-INVITE must not go out while the peer's is being handled.
	select {
	case <-h.requested:
	case <-time.After(500 * time.Millisecond):
	}
	close(releaseCallback)
	select {
	case err := <-handled:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the peer's re-INVITE was not handled")
	}
	h.release <- struct{}{}
	select {
	case <-h.requested:
	default:
	}
	select {
	case err := <-held:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Hold did not return")
	}
	require.Equal(t, []string{"peer's re-INVITE 200", "our re-INVITE"}, h.entries())
}

// TestDialogReInviteGlare is a re-INVITE of the peer's arriving while one of
// ours awaits its answer. RFC 3261 section 14.2 has it answered 491.
func TestDialogReInviteGlare(t *testing.T) {
	d, h := newHeldReInviteDialog(t)
	held := make(chan error, 1)
	go func() { held <- d.Hold(context.Background()) }()
	select {
	case <-h.requested:
	case <-time.After(5 * time.Second):
		t.Fatal("our re-INVITE was not sent")
	}

	peerReInvite := newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1)
	peerReInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	peerReInvite.SetBody(peerSDP())
	tx := newByeServerTx()
	require.NoError(t, d.handleReInvite(peerReInvite, tx))
	h.release <- struct{}{}
	select {
	case err := <-held:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Hold did not return")
	}
	require.Len(t, tx.responses, 1)
	require.Equal(t, sip.StatusRequestPending, tx.responses[0].StatusCode)
}

// loggedPeerTx is a byeServerTx that records the peer's re-INVITE being
// answered in the log of h.
type loggedPeerTx struct {
	*byeServerTx
	h *heldReInvites
}

func (tx *loggedPeerTx) Respond(res *sip.Response) error {
	tx.h.record(fmt.Sprintf("peer's re-INVITE %d", res.StatusCode))
	return nil
}
