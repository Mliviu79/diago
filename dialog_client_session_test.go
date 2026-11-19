// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"errors"
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

func dialogEcho(med *DialogMedia) error {
	audioR, err := med.AudioReader()
	if err != nil {
		return err
	}

	audioW, err := med.AudioWriter()
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

	log := asyncLog(t)
	err := serveBackground(t, dg, ctx, func(ctx context.Context, d *DialogServerSession) {
		// t.Log("Call received", d.InviteRequest)
		// Add some routing
		if d.ToUser() == "alice" {
			d.Trying(ctx)
			d.Ringing(ctx)
			med, err := d.Answer(AnswerOptions{})
			if err != nil {
				log("Failed to answer", "error", err)
				return
			}
			defer med.Close()

			dialogEcho(med)
			<-d.Context().Done()
			return
		}

		if d.ToUser() == "hanguper" {
			d.Trying(ctx)
			_, _ = d.Answer(AnswerOptions{})
			d.Hangup(d.Context())
			return
		}

		d.Respond(ctx, sip.StatusForbidden, "Forbidden", nil)

		<-d.Context().Done()
	})
	require.NoError(t, err)

	t.Run("HanguperClientNoServe", func(t *testing.T) {
		// We want to confirm that diago can receive BYE without Binding to IP, which will reflect Contact Header
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		// Has no listener just UAC. Contact will hold empheral port
		phone := newDialer(ua)
		serveRequests(t, phone)
		// Hanguped
		dialog, med, err := phone.Invite(context.TODO(), sip.Uri{User: "hanguper", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.NoError(t, err)
		defer dialog.Close()
		defer med.Close()
		<-dialog.Context().Done()
	})

	t.Run("HanguperClientWithServe", func(t *testing.T) {
		// We want to confirm that diago can receive BYE on Binded IP
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		phone := newDialer(ua)
		// listening but stil with empheral port
		err := serveBackground(t, phone, t.Context(), func(_ context.Context, d *DialogServerSession) {})
		require.NoError(t, err)

		ports := phone.server.TransportLayer().ListenPorts("udp")
		require.Len(t, ports, 1)
		// Hanguped
		dialog, med, err := phone.Invite(context.TODO(), sip.Uri{User: "hanguper", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.NoError(t, err)
		defer dialog.Close()
		defer med.Close()
		<-dialog.Context().Done()
		assert.Equal(t, dialog.InviteRequest.Via().Port, dialog.InviteRequest.Contact().Address.Port)
	})

	t.Run("Dialer", func(t *testing.T) {
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		phone := newDialer(ua)
		// Start listener in order to reuse UDP listener
		err := serveBackground(t, phone, t.Context(), func(_ context.Context, d *DialogServerSession) {})
		require.NoError(t, err)

		phone.server.TransportLayer().ListenPorts("udp")

		// Forbiddden
		_, _, err = phone.Invite(context.TODO(), sip.Uri{User: "noroute", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.Error(t, err)

		// Hanguped
		dialog, med, err := phone.Invite(context.TODO(), sip.Uri{User: "hanguper", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.NoError(t, err)
		defer dialog.Close()
		defer med.Close()
		<-dialog.Context().Done()

		// Answered call
		dialog, med, err = phone.Invite(context.TODO(), sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}, InviteOptions{})
		require.NoError(t, err)
		defer dialog.Close()
		defer med.Close()

		// Confirm media traveling
		audioR, err := med.AudioReader()
		require.NoError(t, err)

		audioW, err := med.AudioWriter()
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

	serveBackground(t, dg, ctx, func(runCtx context.Context, d *DialogServerSession) {
		ctx := d.Context()
		d.Trying(runCtx)
		d.Ringing(runCtx)

		<-ctx.Done()
	})

	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := newDialer(ua)
		serveBackground(t, dg, t.Context(), func(_ context.Context, d *DialogServerSession) {})

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		_, _, err := dg.Invite(ctx, sip.Uri{User: "test", Host: "127.0.0.1", Port: port}, InviteOptions{
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
		err := serveBackground(t, dg, ctx, func(ctx context.Context, d *DialogServerSession) {
			log("Call received")

			err := authServer.AuthorizeDialog(ctx, d, DigestAuth{
				Username: "test",
				Password: "test123",
				Realm:    "",
				Expire:   10 * time.Second,
			})
			if err != nil {
				log("Failed to authorize", "error", err)
				return
			}

			d.Trying(ctx)
			med, err := d.ProgressMedia(ctx, ProgressMediaOptions{})
			if err != nil {
				log("Failed to progress media", err)
				return
			}
			defer med.Close()

			// Write frame
			w, _ := med.AudioWriter()
			if _, err := w.Write(bytes.Repeat([]byte{0, 100}, 80)); err != nil {
				log("Failed to write frame", err)
				return
			}

			if err := d.AnswerEarlyMedia(med, AnswerOptions{}); err != nil {
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
	err := serveBackground(t, dg, t.Context(), func(_ context.Context, d *DialogServerSession) {})
	require.NoError(t, err)

	dialog, err := dg.NewDialog(sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15060}, NewDialogOptions{})
	require.NoError(t, err)
	defer dialog.Close()

	med, err := dialog.Invite(ctx, InviteClientOptions{
		EarlyMediaDetect: true,
		Username:         "test",
		Password:         "test123",
	})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	defer med.Close()

	// Now we should be able to read media
	r, err := med.AudioReader()
	require.NoError(t, err)

	// Read early media in background
	var earlyMediaBuf []byte
	doneEarly := make(chan struct{})
	go func() {
		defer close(doneEarly)
		earlyMediaBuf, _ = media.ReadAll(r, 160)
	}()

	err = dialog.WaitAnswer(ctx, med, sipgo.AnswerOptions{})
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
		err := serveBackground(t, dg, ctx, func(_ context.Context, d *DialogServerSession) {
			log("Call received")
			d.Answer(AnswerOptions{OnMediaUpdate: func(d *DialogMedia) {

			}})
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := newDialer(ua)
	err := serveBackground(t, dg, t.Context(), func(_ context.Context, d *DialogServerSession) {})
	require.NoError(t, err)

	dialog, _, err := dg.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15060}, InviteOptions{})
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
		err := serveBackground(t, dg, ctx, func(_ context.Context, d *DialogServerSession) {
			log("Call received")
			d.Answer(AnswerOptions{OnMediaUpdate: func(d *DialogMedia) {

			}})
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := newDialer(ua)
	err := serveBackground(t, dg, t.Context(), func(_ context.Context, d *DialogServerSession) {})
	require.NoError(t, err)

	dialog, _, err := dg.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15066}, InviteOptions{})
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
		err := serveBackground(t, dg, ctx, func(runCtx context.Context, d *DialogServerSession) {
			log("New INVITE")
			if err := digServer.AuthorizeDialog(runCtx, d, DigestAuth{
				Username: "test",
				Password: "test",
			}); err != nil {
				return
			}

			med, err := d.Answer(AnswerOptions{OnMediaUpdate: func(d *DialogMedia) {
				// fmt.Println("Server media update", d)
			}})
			if err != nil {
				reinvited <- fmt.Errorf("answer: %w", err)
				return
			}
			defer med.Close()

			ar := med.RTPPacketReader
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				defer cancel()
				beepEncoded, _ := media.ReadAll(ar, 160)
				audioReceived <- beepEncoded
			}()

			time.Sleep(60 * time.Millisecond)
			ms := med.MediaSession().Fork()
			ms.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 39999}
			ms.ExternalIP = ms.Laddr.IP
			err = ms.Init() // This will start new listener
			if err != nil {
				reinvited <- fmt.Errorf("new media session: %w", err)
				return
			}

			defer ms.Close()

			// Stage the custom RTP transport on the media side and exchange only
			// SDP through the signaling dialog.
			med.mu.Lock()
			med.pendingMediaSession = ms
			med.mu.Unlock()
			remoteSDP, err := d.reInviteSDP(ctx, ms.LocalSDP(false))
			if err != nil {
				reinvited <- fmt.Errorf("re-INVITE: %w", err)
				return
			}
			err = med.onRemoteSDP(ctx, remoteSDP, true)
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
	err := serveBackground(t, dg, t.Context(), func(_ context.Context, d *DialogServerSession) {})
	require.NoError(t, err)
	dialog, err := dg.NewDialog(sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15079}, NewDialogOptions{})
	require.NoError(t, err)
	med, err := dialog.Invite(ctx, InviteClientOptions{
		OnMediaUpdate: func(d *DialogMedia) {
		},
		Username: "test",
		Password: "test",
	})
	require.NoError(t, err)
	defer med.Close()
	err = dialog.Ack(ctx)
	require.NoError(t, err)

	require.NoError(t, err)
	pb, _ := med.PlaybackCreate()
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
		go func() {
			_, err := dialog.Invite(context.Background(), opts)
			invited <- err
		}()
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
		err := serveBackground(t, dg, ctx, func(_ context.Context, d *DialogServerSession) {
			log("Call received")
			if _, err := d.Answer(AnswerOptions{}); err != nil {
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
		lock.Lock()
		defer lock.Unlock()
		if _, ok := msg.(*sip.Response); ok {
			responses = append(responses, msg)
		}
	})

	dg := newDialer(ua)
	err := serveBackground(t, dg, t.Context(), func(_ context.Context, d *DialogServerSession) {})
	require.NoError(t, err)

	// Media negotiaton should fail and call should be terminated
	_, _, err = dg.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15060}, InviteOptions{
		OnResponse: func(res *sip.Response) error {
			// Fake Bad SDP
			res.SetBody([]byte("Bad SDP"))
			return nil
		},
	})
	t.Log(err)
	require.Error(t, err)

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
		err := serveBackground(t, dg, ctx, func(_ context.Context, d *DialogServerSession) {
			log("Call received")
			d.Answer(AnswerOptions{
				OnRefer: func(referDialog *DialogClientSession) error {
					if _, err := referDialog.Invite(referDialog.Context(), InviteClientOptions{}); err != nil {
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
		err := serveBackground(t, dg, ctx, func(ctx context.Context, d *DialogServerSession) {
			log("Call INVITE due to REFER received")
			// waitReferDialog <- d
			switch d.ToUser() {
			case "busy":
				d.Respond(ctx, sip.StatusBusyHere, "Busy Here", nil)
				return
			case "noanswer":
				d.Ringing(ctx)
				return
			default:
				_, _ = d.Answer(AnswerOptions{})
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

	err := serveBackground(t, dg, ctx, nil)
	require.NoError(t, err)

	t.Run("Succesfull", func(t *testing.T) {
		d, _, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15071}, InviteOptions{})
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
		d, _, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15071}, InviteOptions{})
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
		d, _, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15071}, InviteOptions{})
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

// TestDialogClientInviteAllocatesOnTheInviteCtx pins that Invite makes its port
// allocation on the ctx it was called with. The allocator refuses, so Invite
// returns before it sends anything.
func TestDialogClientInviteAllocatesOnTheInviteCtx(t *testing.T) {
	type ctxKey struct{}
	ctx := context.WithValue(t.Context(), ctxKey{}, "invite")
	errRefused := errors.New("port allocator refused")
	var got any
	alloc := &funcPortAllocator{alloc: func(allocCtx context.Context) (int, error) {
		got = allocCtx.Value(ctxKey{})
		return 0, errRefused
	}}

	d := newTestClientDialog(t, sip.DialogStateEstablished)
	d.mediaConfig = MediaConfig{
		Codecs:           []media.Codec{media.CodecAudioUlaw},
		bindIP:           net.IPv4(127, 0, 0, 1),
		RTPPortAllocator: alloc,
	}

	_, err := d.Invite(ctx, InviteClientOptions{})
	require.ErrorIs(t, err, errRefused)
	require.Equal(t, "invite", got, "Invite did not hand the allocator its ctx")
}
