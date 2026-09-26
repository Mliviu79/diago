// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
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
		err := dg.ServeBackground(ctx, nil)
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
	err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
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
		dialog, err := dialer.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15010}, InviteOptions{
			OnResponse: func(res *sip.Response) error {
				log("Received resp", res.StatusCode)
				// The server transaction sends 100 Trying on its own when the
				// handler has not answered within 200 ms (RFC 3261 section
				// 17.2.1), which a loaded machine can take. Only the responses
				// the session sends are asserted.
				if res.StatusCode == sip.StatusTrying {
					return nil
				}
				allResponses = append(allResponses, *res.Clone())
				return nil
			},
		})
		if err != nil {
			log("Failed to dial", err)
			return
		}
		defer dialog.Close()
		<-dialog.Context().Done()
		log("Dialog done")
	}()

	d := <-waitDialog

	err = d.ProgressMedia()
	require.NoError(t, err)

	// It is valid to also send 180
	time.Sleep(500 * time.Millisecond)
	require.NoError(t, d.Ringing())

	// We can play some file ringtone
	playback, err := d.PlaybackCreate()
	require.NoError(t, err)
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)

	// We can now answer
	err = d.Answer()
	require.NoError(t, err)

	// New playback is needed to follow new media session
	playback, err = d.PlaybackCreate()
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

// newProgressMediaDTLSDialog builds an inbound dialog over a fake transaction
// whose INVITE carries the DTLS-SRTP offer of peer, and whose media config is
// DTLS with a certificate of its own. The transaction hands every response it
// is given to the returned channel.
func newProgressMediaDTLSDialog(t *testing.T) (d *DialogServerSession, peer *media.MediaSession, inviteTx *respondedServerTx) {
	t.Helper()

	peer = &media.MediaSession{
		Codecs:    []media.Codec{media.CodecAudioUlaw},
		Mode:      sdp.ModeSendrecv,
		SecureRTP: media.SecureRTPModeDTLS,
		DTLSConf:  media.DTLSConfig{Certificates: []tls.Certificate{testdata.ClientCertificate()}},
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
	}
	require.NoError(t, peer.Init())
	t.Cleanup(func() { _ = peer.Close() })

	inviteTx = &respondedServerTx{byeServerTx: newByeServerTx(), responded: make(chan *sip.Response, 4)}
	d = newTestDialogOver(t, inviteTx)
	d.InviteRequest.SetBody(peer.LocalSDP())
	d.mediaConf = MediaConfig{
		Codecs:     []media.Codec{media.CodecAudioUlaw},
		DTLSConfig: &media.DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
		secureRTP:  media.SecureRTPModeDTLS,
		bindIP:     net.IPv4(127, 0, 0, 1),
	}
	t.Cleanup(func() { _ = d.DialogMedia.Close() })
	return d, peer, inviteTx
}

// TestDialogServerProgressMediaDTLS pins that early media over DTLS-SRTP is
// keyed before ProgressMedia returns. RFC 5763 section 6.2 has an answerer that
// wishes to provide early media take setup:active and establish the DTLS
// association at once, and RFC 3261 section 13.2.1 has the caller treat the
// answer in the 183 as the answer. Without the handshake the session has no
// SRTP keys, so the early media goes out unencrypted, and the caller, waiting
// as the DTLS server, never completes its own handshake.
func TestDialogServerProgressMediaDTLS(t *testing.T) {
	t.Run("keys the early media", func(t *testing.T) {
		d, peer, inviteTx := newProgressMediaDTLSDialog(t)

		progressed := make(chan error, 1)
		go func() { progressed <- d.ProgressMedia() }()

		var res *sip.Response
		select {
		case res = <-inviteTx.responded:
		case <-time.After(5 * time.Second):
			t.Fatal("no 183 was sent")
		}
		require.Equal(t, sip.StatusSessionInProgress, res.StatusCode)

		// The caller applies the answer in the 183 and runs its side of the
		// handshake, as the DTLS server the actpass offer left it.
		peer.RemoteSDPIsAnswer = true
		require.NoError(t, peer.RemoteSDP(res.Body()))
		peerDone := make(chan error, 1)
		go func() { peerDone <- peer.Finalize() }()

		for _, done := range []chan error{progressed, peerDone} {
			select {
			case err := <-done:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the early media DTLS handshake did not complete")
			}
		}

		payload := []byte{0xd5, 0xd5, 0xd5, 0xd5}
		require.NoError(t, d.MediaSession().WriteRTP(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    media.CodecAudioUlaw.PayloadType,
				SequenceNumber: 1,
				Timestamp:      160,
				SSRC:           0xdeadbeef,
			},
			Payload: payload,
		}))
		require.NoError(t, peer.StopRTP(1, 5*time.Second))
		got := rtp.Packet{}
		_, err := peer.ReadRTP(make([]byte, media.RTPBufSize), &got)
		require.NoError(t, err, "early media must arrive and decrypt")
		assert.Equal(t, payload, got.Payload)
	})

	// A caller that gives up before it keys the early media ends the wait
	// with the dialog, ahead of KeyTimeout.
	t.Run("gives up with the dialog", func(t *testing.T) {
		d, _, inviteTx := newProgressMediaDTLSDialog(t)

		progressed := make(chan error, 1)
		go func() { progressed <- d.ProgressMedia() }()

		select {
		case <-inviteTx.responded:
		case <-time.After(5 * time.Second):
			t.Fatal("no 183 was sent")
		}

		// The INVITE transaction ends before any final response, as a CANCEL
		// ends it, which ends the dialog.
		inviteTx.onTerminate("", nil)
		select {
		case <-d.Context().Done():
		case <-time.After(5 * time.Second):
			t.Fatal("the dialog did not end with its transaction")
		}

		select {
		case err := <-progressed:
			assert.Error(t, err, "early media that was never keyed is not a success")
		case <-time.After(5 * time.Second):
			t.Fatal("ProgressMedia outlived the dialog")
		}
	})
}

func TestIntegrationDialogServerReinvite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The calling side runs on its own goroutine. Its Invite error is handed
	// to the test, and since it logs once the hangup ends its dialog, the test
	// waits for it to end before it returns.
	inviteErr := make(chan error, 1)
	dialogDone := make(chan struct{})
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
		err := dg.ServeBackground(ctx, nil)
		require.NoError(t, err)

		go func() {
			defer close(dialogDone)
			dialog, err := dg.Invite(ctx, sip.Uri{User: "dialer", Host: "127.0.0.1", Port: 15060}, InviteOptions{})
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
	err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
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

	err = d.Answer()
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
	select {
	case <-dialogDone:
	case <-time.After(5 * time.Second):
		t.Fatal("calling side dialog did not end after hangup")
	}
}

func TestIntegrationDialogServerPeerCodecPruneReinvite(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ua, _ := sipgo.NewUA(sipgo.WithUserAgent("uas"))
	defer ua.Close()

	uas := NewDiago(ua, WithTransport(Transport{
		Transport:       "udp",
		BindHost:        "127.0.0.1",
		BindPort:        15080,
		MediaExternalIP: net.IPv4(203, 0, 113, 10),
	}))
	// The handler runs on a server goroutine, so its errors are handed to the
	// test rather than asserted there.
	answered := make(chan error, 1)
	err := uas.ServeBackground(ctx, func(d *DialogServerSession) {
		// This is the reported role: the peer sends the initial INVITE and
		// Diago answers it as the UAS. RTP NAT must not change the SIP flow.
		err := d.AnswerOptions(AnswerOptions{
			RTPNAT:        media.RTPNATSymetric,
			OnMediaUpdate: func(*DialogMedia) {},
		})
		if err != nil {
			answered <- fmt.Errorf("answer: %w", err)
			return
		}
		reader, err := d.AudioReader()
		if err != nil {
			answered <- fmt.Errorf("audio reader: %w", err)
			return
		}
		answered <- nil
		go func() {
			_, _ = reader.Read(make([]byte, 160))
		}()
		<-d.Context().Done()
	})
	require.NoError(t, err)

	peerUA, _ := sipgo.NewUA(sipgo.WithUserAgent("peer"))
	defer peerUA.Close()
	peer := newDialer(peerUA)
	err = peer.ServeBackground(ctx, func(*DialogServerSession) {})
	require.NoError(t, err)

	dialog, err := peer.Invite(ctx, sip.Uri{User: "service", Host: "127.0.0.1", Port: 15080}, InviteOptions{})
	require.NoError(t, err)
	defer dialog.Close()
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not complete after the call was set up")
	}
	require.Contains(t, string(dialog.InviteRequest.Body()), " 0 8 101")

	// The initial peer offer contains PCMU, PCMA and telephone-event. The
	// post-answer offer intentionally prunes PCMA, matching the SBC behavior.
	prunedMedia := dialog.MediaSession().Fork()
	prunedMedia.Codecs = []media.Codec{
		media.CodecAudioUlaw,
		media.CodecTelephoneEvent8000,
	}
	prunedOffer := prunedMedia.LocalSDP()
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
		err := dg.ServeBackground(ctx, nil)
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
			err := dialog.Invite(ctx, InviteClientOptions{
				OnRefer: func(referDialog *DialogClientSession) error {
					// referDialog.
					if err := referDialog.Invite(ctx, InviteClientOptions{}); err != nil {
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

	log := asyncLog(t)
	waitDialog := make(chan *DialogServerSession, 1)
	err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
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

		require.NoError(t, d.Answer())
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
	dialog := &DialogServerSession{
		DialogMedia: DialogMedia{
			mediaSession:    &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
			RTPPacketWriter: media.NewRTPPacketWriter(rtpBuf, media.CodecAudioUlaw),
		},
	}

	playback, err := dialog.PlaybackCreate()
	require.NoError(t, err)

	initTS := dialog.RTPPacketWriter.InitTimestamp()
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	diffTS := dialog.RTPPacketWriter.PacketHeader.Timestamp - initTS
	assert.Greater(t, diffTS, uint32(1000))

	time.Sleep(100 * time.Millisecond) // 4 frames
	initTS = dialog.RTPPacketWriter.InitTimestamp()
	_, err = playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	diffTS2 := dialog.RTPPacketWriter.PacketHeader.Timestamp - initTS
	t.Log(initTS, diffTS2)

	// Timestamp should be offset more than previous diff by Sleep
	assert.Greater(t, diffTS2, diffTS+5*media.CodecAudioUlaw.SampleTimestamp())
}

// newTestMediaSession installs a media session on d, so a body-less re-INVITE
// has an SDP to be answered with.
func newTestMediaSession(t *testing.T, d *DialogMedia) {
	t.Helper()
	sess, err := media.NewMediaSession(net.IPv4(127, 0, 0, 1), 0)
	require.NoError(t, err)
	t.Cleanup(func() { sess.Close() })
	d.mu.Lock()
	d.mediaSession = sess
	d.mu.Unlock()
}

// newReInviteTestDialog builds a confirmed inbound dialog, without a transport,
// whose 2xx carries our Contact and whose media session is set up.
func newReInviteTestDialog(t *testing.T) *DialogServerSession {
	t.Helper()
	d, _ := newByeTestDialog(t)
	res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
	res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
	d.InviteResponse = res
	confirm(t, d)
	newTestMediaSession(t, &d.DialogMedia)
	return d
}

// TestDialogServerReInviteEndedDialog pins how a re-INVITE for a dialog that is
// no longer live is answered. An ended dialog stays in the cache until the call
// handler returns, so the request still finds it. It matches no live dialog,
// and is answered 481 (RFC 3261 section 12.2.2) without reaching the media. A
// re-INVITE still pending when a BYE tears the media down gets 487 (RFC 3261
// section 15.1.2) instead of an answer built on closed media.
func TestDialogServerReInviteEndedDialog(t *testing.T) {
	t.Run("ended", func(t *testing.T) {
		d := newReInviteTestDialog(t)
		require.NoError(t, d.ReadBye(newBye(t, d, d.InviteRequest.CSeq().SeqNo+1), newByeServerTx()))
		require.Equal(t, sip.DialogStateEnded, d.LoadState())

		tx := newByeServerTx()
		require.NoError(t, d.handleReInvite(newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+2), tx))
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
		assert.Nil(t, d.remoteContactTarget, "the re-INVITE reached the media")
	})

	t.Run("media closed", func(t *testing.T) {
		d := newReInviteTestDialog(t)
		require.NoError(t, d.DialogMedia.Close())

		tx := newByeServerTx()
		require.NoError(t, d.handleReInvite(newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1), tx))
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusRequestTerminated, tx.responses[0].StatusCode)
	})
}
