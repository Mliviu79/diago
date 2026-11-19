// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationDialogServerEarlyMedia(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dialer *Diago
	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15020,
			},
		))

		// Run listener to accepte reinvites, but it should not receive any request
		err := serveBackground(t, dg, ctx, nil)
		require.NoError(t, err)

		dialer = dg
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  15010,
		},
	))

	log := asyncLog(t)
	waitDialog := make(chan *DialogServerSession, 1)
	err := serveBackground(t, dg, ctx, func(_ context.Context, d *DialogServerSession) {
		log("Call received")
		waitDialog <- d
		<-d.Context().Done()
	})
	require.NoError(t, err)

	allResponses := []sip.Response{}
	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		dialog, med, err := dialer.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15010}, InviteOptions{
			OnResponse: func(res *sip.Response) error {
				log("Received resp", res.StatusCode)
				allResponses = append(allResponses, *res.Clone())
				return nil
			},
		})
		if err != nil {
			log("Failed to dial", err)
			return
		}
		defer dialog.Close()
		defer med.Close()
		<-dialog.Context().Done()
		log("Dialog done")
	}()

	d := <-waitDialog

	earlyMed, err := d.ProgressMedia(t.Context(), ProgressMediaOptions{})
	require.NoError(t, err)
	defer earlyMed.Close()

	// It is valid to also send 180
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, d.Ringing(t.Context()))

	// We can play some file ringtone
	playback, err := earlyMed.PlaybackCreate()
	require.NoError(t, err)
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)

	// We can now answer
	require.NoError(t, d.AnswerEarlyMedia(earlyMed, AnswerOptions{}))

	playback, err = earlyMed.PlaybackCreate()
	require.NoError(t, err)
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	d.Hangup(context.TODO())

	wg.Wait()
	require.Len(t, allResponses, 3)
	assert.Equal(t, 183, allResponses[0].StatusCode)
	assert.Equal(t, 180, allResponses[1].StatusCode)
	assert.Equal(t, 200, allResponses[2].StatusCode)
}

func TestIntegrationDialogServerReinvite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The calling side runs on its own goroutine, so its Invite error is
	// handed to the test rather than asserted there.
	inviteErr := make(chan error, 1)
	log := asyncLog(t)
	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("server"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15070,
			},
		))

		// Run listener to accepte reinvites, but it should not receive any request
		err := serveBackground(t, dg, ctx, nil)
		require.NoError(t, err)

		go func() {
			dialog, _, err := dg.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15060}, InviteOptions{})
			inviteErr <- err
			if err != nil {
				return
			}
			<-dialog.Context().Done()
			log("Dialog done")
		}()
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  15060,
		},
	))

	waitDialog := make(chan *DialogServerSession, 1)
	err := serveBackground(t, dg, ctx, func(_ context.Context, d *DialogServerSession) {
		log("Call received")
		waitDialog <- d
		<-d.Context().Done()
	})
	require.NoError(t, err)
	var d *DialogServerSession
	select {
	case d = <-waitDialog:
	case err := <-inviteErr:
		t.Fatalf("the calling side's Invite returned before the call arrived: %v", err)
	}

	_, err = d.Answer(AnswerOptions{})
	require.NoError(t, err)
	select {
	case err := <-inviteErr:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the calling side's Invite did not return after the answer")
	}
	err = d.ReInvite(d.Context())
	require.NoError(t, err)

	d.Hangup(context.TODO())
}

func TestIntegrationDialogServerHoldUnholdKeepsAudio(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	newDiago := func(port int) *Diago {
		ua, _ := sipgo.NewUA()
		t.Cleanup(func() { ua.Close() })
		return NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: port}))
	}

	answered := make(chan *DialogServerSession, 1)
	uas := newDiago(15077)
	err := serveBackground(t, uas, ctx, func(_ context.Context, d *DialogServerSession) {
		med, err := d.Answer(AnswerOptions{})
		if err != nil {
			t.Error(err)
			return
		}
		defer med.Close()
		answered <- d
		med.Echo()
	})
	require.NoError(t, err)

	uac := newDiago(15067)
	require.NoError(t, serveBackground(t, uac, ctx, func(context.Context, *DialogServerSession) {}))
	dialog, med, err := uac.Invite(ctx, sip.Uri{User: "echo", Host: "127.0.0.1", Port: 15077}, InviteOptions{})
	require.NoError(t, err)
	defer med.Close()
	uasDialog := <-answered

	reader, err := med.AudioReader()
	require.NoError(t, err)
	writer, err := med.AudioWriter()
	require.NoError(t, err)
	echoed := make(chan []byte, 16)
	go func() {
		buf := make([]byte, 160)
		for {
			n, err := reader.Read(buf)
			if err != nil {
				return
			}
			if n > 0 {
				echoed <- bytes.Clone(buf[:n])
			}
		}
	}()
	// Audio flows as on a call, a frame every 20ms: one can be lost while a session swaps
	value := byte(0)
	echoes := func() {
		t.Helper()
		value++
		frame := bytes.Repeat([]byte{value}, 160)
		tick := time.NewTicker(20 * time.Millisecond)
		defer tick.Stop()
		timeout := time.After(3 * time.Second)
		for {
			_, err := writer.Write(frame)
			require.NoError(t, err)
			select {
			case got := <-echoed:
				if bytes.Equal(got, frame) {
					return
				}
			case <-tick.C:
			case <-timeout:
				t.Fatal("no echo")
			}
		}
	}
	echoes()

	// The held side sends nothing, so the holder's reader waits through both swaps
	require.NoError(t, uasDialog.Hold(ctx))
	require.NoError(t, uasDialog.Unhold(ctx))
	echoes()

	require.NoError(t, dialog.Hangup(ctx))
}

func TestIntegrationDialogServerPeerCodecPruneReinvite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("uas"))
	defer ua.Close()

	uas := NewDiago(ua, WithTransport(Transport{
		Transport:       "udp",
		BindHost:        "127.0.0.1",
		BindPort:        0,
		MediaExternalIP: net.IPv4(203, 0, 113, 10),
	}))
	type answerResult struct {
		dialog *DialogServerSession
		err    error
	}
	answerCh := make(chan answerResult, 1)
	err := serveBackground(t, uas, ctx, func(_ context.Context, d *DialogServerSession) {
		// This is the reported role: the peer sends the initial INVITE and
		// Diago answers it as the UAS. RTP NAT must not change the SIP flow.
		med, err := d.Answer(AnswerOptions{
			RTPNAT:        media.RTPNATSymetric,
			OnMediaUpdate: func(*DialogMedia) {},
		})
		if err != nil {
			answerCh <- answerResult{err: err}
			return
		}
		reader, err := med.AudioReader()
		answerCh <- answerResult{dialog: d, err: err}
		if err != nil {
			return
		}
		go func() {
			_, _ = reader.Read(make([]byte, 160))
		}()
		<-d.Context().Done()
	})
	require.NoError(t, err)
	require.NotZero(t, uas.transports[0].BindPort)

	peerUA, _ := sipgo.NewUA(sipgo.WithUserAgent("peer"))
	defer peerUA.Close()
	peer := newDialer(peerUA)
	err = serveBackground(t, peer, ctx, func(context.Context, *DialogServerSession) {})
	require.NoError(t, err)

	dialog, peerMedia, err := peer.Invite(ctx, sip.Uri{User: "service", Host: "127.0.0.1", Port: uas.transports[0].BindPort}, InviteOptions{})
	require.NoError(t, err)
	defer dialog.Close()
	answer := <-answerCh
	require.NoError(t, answer.err)
	require.NotNil(t, answer.dialog)
	require.Eventually(t, func() bool {
		return answer.dialog.LoadState() == sip.DialogStateConfirmed
	}, time.Second, 10*time.Millisecond)
	require.Contains(t, string(dialog.InviteRequest.Body()), " 0 8 101")

	// The initial peer offer contains PCMU, PCMA and telephone-event. The
	// post-answer offer intentionally prunes PCMA, matching the SBC behavior.
	prunedMedia := peerMedia.MediaSession().Fork()
	prunedMedia.Codecs = []media.Codec{
		media.CodecAudioUlaw,
		media.CodecTelephoneEvent8000,
	}
	prunedOffer := prunedMedia.LocalSDP(false)
	require.Contains(t, string(prunedOffer), " 0 101")
	require.NotContains(t, string(prunedOffer), " 0 8 101")
	reinvite := sip.NewRequest(sip.INVITE, dialog.RemoteContact().Address)
	reinvite.AppendHeader(dialog.InviteRequest.Contact())
	reinvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	reinvite.SetBody(prunedOffer)

	reinviteCtx, cancelReinvite := context.WithTimeout(ctx, 3*time.Second)
	defer cancelReinvite()
	res, err := dialog.Do(reinviteCtx, reinvite)
	require.NoError(t, err)
	require.Equal(t, sip.StatusOK, res.StatusCode)
	require.NotNil(t, res.Contact())
	contentType := res.ContentType()
	require.NotNil(t, contentType)
	require.Equal(t, "application/sdp", contentType.Value())
	require.NotEmpty(t, res.Body())
	require.Contains(t, string(res.Body()), "c=IN IP4 203.0.113.10")
	require.NotContains(t, string(res.Body()), "c=IN IP4 127.0.0.1")

	// Complete the re-INVITE transaction from the peer/UAC side.
	ack := sip.NewRequest(sip.ACK, res.Contact().Address)
	require.NoError(t, dialog.WriteRequest(ack))
	dialog.Hangup(ctx)
}

func TestIntegrationDialogServerRefer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var dialer *Diago
	{
		ua, _ := sipgo.NewUA(sipgo.WithUserAgent("dialer"))
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15071,
				ID:        "udp",
			},
		))

		// Run listener to accepte reinvites, but it should not receive any request
		err := serveBackground(t, dg, ctx, nil)
		require.NoError(t, err)
		dialer = dg
	}

	// dialCall calls from its own goroutine. The Invite error is handed to the
	// test, and since the goroutine logs once its dialog ends, the test waits
	// for it before it ends.
	dialCall := func(t *testing.T) <-chan error {
		// Registered before the wait below, so the goroutine still logs while
		// the subtest waits for it.
		log := asyncLog(t)
		dialog, err := dialer.NewDialog(sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15070}, NewDialogOptions{})
		require.NoError(t, err)

		invited := make(chan error, 1)
		done := make(chan struct{})
		t.Cleanup(func() {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Error("the calling side's dialog did not end")
			}
		})
		go func() {
			defer close(done)
			_, err := dialog.Invite(ctx, InviteClientOptions{
				OnRefer: func(referDialog *DialogClientSession) error {
					// referDialog.
					if _, err := referDialog.Invite(ctx, InviteClientOptions{}); err != nil {
						return err
					}
					if err := referDialog.Ack(ctx); err != nil {
						return err
					}

					return referDialog.Hangup(ctx)
				},
			})
			invited <- err
			if err != nil {
				return
			}

			dialog.Ack(ctx)
			<-dialog.Context().Done()
			log("Dialog done")
		}()
		return invited
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

	log := asyncLog(t)
	waitDialog := make(chan *DialogServerSession, 1)
	err := serveBackground(t, dg, ctx, func(_ context.Context, d *DialogServerSession) {
		log("Call received")
		waitDialog <- d
		<-d.Context().Done()
	})
	require.NoError(t, err)

	// answerCall answers the call dialCall placed and checks its Invite.
	answerCall := func(t *testing.T, invited <-chan error) *DialogServerSession {
		t.Helper()
		var d *DialogServerSession
		select {
		case d = <-waitDialog:
		case err := <-invited:
			t.Fatalf("the calling side's Invite returned before the call arrived: %v", err)
		}
		t.Cleanup(func() { d.Hangup(ctx) })

		_, err := d.Answer(AnswerOptions{})
		require.NoError(t, err)
		select {
		case err := <-invited:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the calling side's Invite did not return after the answer")
		}
		return d
	}

	t.Run("Successfull", func(t *testing.T) {
		d := answerCall(t, dialCall(t))

		referState := make(chan int)
		err = d.ReferOptions(d.Context(), sip.Uri{Host: "127.0.0.1", Port: 15072}, ReferServerOptions{
			OnNotify: func(statusCode int) {
				referState <- statusCode
			},
		})
		require.NoError(t, err)

		assert.Equal(t, 100, <-referState)
		assert.Equal(t, 200, <-referState)
	})

	t.Run("UnreachableRefer", func(t *testing.T) {
		d := answerCall(t, dialCall(t))

		referState := make(chan int)
		err = d.ReferOptions(d.Context(), sip.Uri{User: "noanswer", Host: "127.0.0.1", Port: 15072}, ReferServerOptions{
			OnNotify: func(statusCode int) {
				referState <- statusCode
			},
		})
		require.NoError(t, err)

		assert.Equal(t, 100, <-referState)
		assert.Equal(t, sip.StatusTemporarilyUnavailable, <-referState)
	})

	t.Run("BusyRefer", func(t *testing.T) {
		d := answerCall(t, dialCall(t))

		referState := make(chan int)
		err = d.ReferOptions(d.Context(), sip.Uri{User: "busy", Host: "127.0.0.1", Port: 15072}, ReferServerOptions{
			OnNotify: func(statusCode int) {
				referState <- statusCode
			},
		})
		require.NoError(t, err)

		assert.Equal(t, 100, <-referState)
		assert.Equal(t, sip.StatusBusyHere, <-referState)
	})
}

func TestIntegrationDialogServerPlayback(t *testing.T) {
	rtpBuf := newRTPWriterBuffer()
	med := &DialogMedia{
		mediaSession:    &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
		RTPPacketWriter: media.NewRTPPacketWriter(rtpBuf, media.CodecAudioUlaw),
	}
	// dialog := &DialogServerSession{
	// 	media: med,
	// }

	playback, err := med.PlaybackCreate()
	require.NoError(t, err)

	initTS := med.RTPPacketWriter.InitTimestamp()
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	diffTS := med.RTPPacketWriter.PacketHeader.Timestamp - initTS
	assert.Greater(t, diffTS, uint32(1000))

	time.Sleep(100 * time.Millisecond) // 4 frames
	initTS = med.RTPPacketWriter.InitTimestamp()
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	diffTS2 := med.RTPPacketWriter.PacketHeader.Timestamp - initTS
	t.Log(initTS, diffTS2)

	// Timestamp should be offset more than previous diff by Sleep
	assert.Greater(t, diffTS2, diffTS+5*media.CodecAudioUlaw.SampleTimestamp())
}

// TestDialogServerMediaAllocatesOnTheDialogCtx pins that each server entry point
// that sets up media makes its port allocation on the dialog's own ctx: live
// while the dialog is, and ended once the dialog ends.
func TestDialogServerMediaAllocatesOnTheDialogCtx(t *testing.T) {
	// confirmLate acknowledges a 2xx that carried our offer with a peer's
	// answer to it.
	confirmLate := func(t *testing.T, d *DialogServerSession, res *sip.Response) {
		peer := newMediaSessionForTest(t)
		require.NoError(t, peer.RemoteSDP(res.Body(), false))
		require.NoError(t, d.ReadAck(newLateAck(d, peer.LocalSDP(true)), newByeServerTx()))
	}
	for _, tc := range []struct {
		name string
		// late is an INVITE without an offer, whose 2xx carries ours.
		late bool
		run  func(d *DialogServerSession) error
		// complete is the caller's side, run once the first response is sent.
		// Nil where the entry point does not wait for an ACK.
		complete func(t *testing.T, d *DialogServerSession, res *sip.Response)
	}{
		{
			name: "ProgressMedia",
			run: func(d *DialogServerSession) error {
				_, err := d.ProgressMedia(t.Context(), ProgressMediaOptions{})
				return err
			},
		},
		{
			name: "ProgressMediaOptions",
			run: func(d *DialogServerSession) error {
				return d.ProgressMediaOptions(t.Context(), ProgressMediaOptions{})
			},
		},
		{
			name: "Answer",
			run: func(d *DialogServerSession) error {
				_, err := d.Answer(AnswerOptions{})
				return err
			},
			complete: func(t *testing.T, d *DialogServerSession, _ *sip.Response) { confirm(t, d) },
		},
		{
			name: "AnswerOptions",
			run: func(d *DialogServerSession) error {
				_, err := d.AnswerOptions(t.Context(), AnswerOptions{})
				return err
			},
			complete: func(t *testing.T, d *DialogServerSession, _ *sip.Response) { confirm(t, d) },
		},
		{
			name: "AnswerLate",
			late: true,
			run: func(d *DialogServerSession) error {
				_, err := d.AnswerLate()
				return err
			},
			complete: confirmLate,
		},
		{
			name: "AnswerLateContext",
			late: true,
			run: func(d *DialogServerSession) error {
				_, err := d.AnswerLateContext(t.Context())
				return err
			},
			complete: confirmLate,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				calls    int
				live     bool
				allocCtx context.Context
			)
			alloc := &funcPortAllocator{alloc: func(ctx context.Context) (int, error) {
				calls++
				live = ctx.Err() == nil
				allocCtx = ctx
				return 0, nil // 0 lets the OS pick, so the session still binds
			}}
			conf := MediaConfig{
				Codecs:           []media.Codec{media.CodecAudioUlaw},
				bindIP:           net.IPv4(127, 0, 0, 1),
				RTPPortAllocator: alloc,
			}

			var (
				d        *DialogServerSession
				inviteTx *firstResponseTx
			)
			if tc.late {
				d, inviteTx, _ = newLateOfferDialog(t, conf)
			} else {
				peer := newMediaSessionForTest(t)
				inviteTx = &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
				d = newTestDialogOver(t, inviteTx)
				d.InviteRequest.SetBody(peer.LocalSDP(false))
				d.mediaConf = conf
			}
			// The media an entry point makes is closed with the session it
			// registered with.
			t.Cleanup(func() { _ = d.Close() })

			// The entry point may wait for the caller's ACK of its first response,
			// so it runs on its own goroutine while the caller answers.
			entered := make(chan error, 1)
			go func() { entered <- tc.run(d) }()
			select {
			case res := <-inviteTx.first:
				if tc.complete != nil {
					tc.complete(t, d, res)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("no response was sent")
			}
			select {
			case err := <-entered:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatalf("%s did not return within 10s of its SIP exchange", tc.name)
			}

			require.Equal(t, 1, calls, "the allocator was consulted once")
			require.True(t, live, "the allocation was made on an ended ctx")
			require.NoError(t, allocCtx.Err(), "the dialog's ctx was ended while the dialog was live")

			require.NoError(t, d.ReadBye(t.Context(), newBye(t, d, d.InviteRequest.CSeq().SeqNo+1), newByeServerTx()))
			select {
			case <-allocCtx.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the dialog's ctx did not end with the dialog")
			}
		})
	}
}

// TestDialogServerEarlyMediaFailureReleasesThePort pins that an early media
// setup that fails once its media is bound closes that media. Nothing else
// holds it then, and the port an allocator chose goes back only from that
// close.
func TestDialogServerEarlyMediaFailureReleasesThePort(t *testing.T) {
	errNotSent := errors.New("the 183 was not sent")
	offer := func(t *testing.T, codec media.Codec) []byte {
		t.Helper()
		peer, err := media.NewMediaSession(net.IPv4(127, 0, 0, 1), 0)
		require.NoError(t, err)
		t.Cleanup(func() { _ = peer.Close() })
		peer.Codecs = []media.Codec{codec}
		return peer.LocalSDP(false)
	}

	entries := []struct {
		name string
		run  func(ctx context.Context, d *DialogServerSession) error
	}{
		{
			name: "ProgressMedia",
			run: func(ctx context.Context, d *DialogServerSession) error {
				_, err := d.ProgressMedia(ctx, ProgressMediaOptions{})
				return err
			},
		},
		{
			name: "ProgressMediaOptions",
			run: func(ctx context.Context, d *DialogServerSession) error {
				return d.ProgressMediaOptions(ctx, ProgressMediaOptions{})
			},
		},
	}
	failures := []struct {
		name string
		// offer is the INVITE's body, nil for an INVITE that carries none.
		offer func(t *testing.T) []byte
		// respondErr is what sending the 183 fails with, if it fails.
		respondErr error
	}{
		{name: "NoOffer"},
		{name: "NoCommonCodec", offer: func(t *testing.T) []byte { return offer(t, media.CodecAudioAlaw) }},
		{
			name:       "ResponseNotSent",
			offer:      func(t *testing.T) []byte { return offer(t, media.CodecAudioUlaw) },
			respondErr: errNotSent,
		},
	}

	for _, entry := range entries {
		t.Run(entry.name, func(t *testing.T) {
			for _, failure := range failures {
				t.Run(failure.name, func(t *testing.T) {
					alloc := &funcPortAllocator{alloc: func(context.Context) (int, error) {
						return 0, nil // 0 lets the OS pick, so the session still binds
					}}
					inviteTx := newByeServerTx()
					inviteTx.respondErr = failure.respondErr
					d := newTestDialogOver(t, inviteTx)
					t.Cleanup(func() { _ = d.Close() })
					if failure.offer != nil {
						d.InviteRequest.SetBody(failure.offer(t))
					}
					d.mediaConf = MediaConfig{
						Codecs:           []media.Codec{media.CodecAudioUlaw},
						bindIP:           net.IPv4(127, 0, 0, 1),
						RTPPortAllocator: alloc,
					}

					err := entry.run(t.Context(), d)
					require.Error(t, err)
					if failure.respondErr != nil {
						require.ErrorIs(t, err, failure.respondErr)
					}
					require.Equal(t, []int{0}, alloc.released, "the failed setup did not hand its port back")
				})
			}
		})
	}
}
