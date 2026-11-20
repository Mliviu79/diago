// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ulawDecode(b []byte) []byte {
	dec := make([]byte, len(b)*2)
	n, _ := audio.DecodeUlawTo(dec, b)
	return dec[:n]
}

func ulawEncode(b []byte) []byte {
	dec := make([]byte, len(b)/2)
	n, _ := audio.EncodeUlawTo(dec, b)
	return dec[:n]
}
func TestBridgeProxy(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 99 // Do not start proxy

	incomingMed := &DialogMedia{
		mediaSession: &media.MediaSession{
			Codecs: []media.Codec{media.CodecAudioAlaw},
		},
		audioReader:     bytes.NewBuffer(make([]byte, 9999)),
		audioWriter:     bytes.NewBuffer(make([]byte, 0)),
		RTPPacketReader: media.NewRTPPacketReader(nil, media.CodecAudioAlaw),
		RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioAlaw),
	}
	outgoingMed := &DialogMedia{
		mediaSession: &media.MediaSession{
			Codecs: []media.Codec{media.CodecAudioAlaw},
		},
		audioReader:     bytes.NewBuffer(make([]byte, 9999)),
		audioWriter:     bytes.NewBuffer(make([]byte, 0)),
		RTPPacketReader: media.NewRTPPacketReader(nil, media.CodecAudioAlaw),
		RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioAlaw),
	}

	err := b.AddDialogMedia(incomingMed)
	require.NoError(t, err)
	err = b.AddDialogMedia(outgoingMed)
	require.NoError(t, err)

	err = b.proxyMediaChannels(context.Background(), b.dialogs[0], b.dialogs[1])
	require.ErrorIs(t, err, io.EOF)

	// Confirm all data is proxied
	assert.Equal(t, 9999, incomingMed.audioWriter.(*bytes.Buffer).Len())
	assert.Equal(t, 9999, outgoingMed.audioWriter.(*bytes.Buffer).Len())
}

func TestBridgeNoTranscodingAllowed(t *testing.T) {
	b := NewBridge()
	// b.waitDialogsNum = 99 // Do not start proxy

	incomingMed := &DialogMedia{
		mediaSession: &media.MediaSession{
			Codecs: []media.Codec{media.CodecAudioAlaw},
		},
		audioReader: bytes.NewBuffer(make([]byte, 0)),
		audioWriter: bytes.NewBuffer(make([]byte, 0)),
	}
	outgoingMed := &DialogMedia{
		mediaSession: &media.MediaSession{
			Codecs: []media.Codec{media.CodecAudioUlaw},
		},
		audioReader: bytes.NewBuffer(make([]byte, 0)),
		audioWriter: bytes.NewBuffer(make([]byte, 0)),
	}

	err := b.AddDialogMedia(incomingMed)
	require.NoError(t, err)
	err = b.AddDialogMedia(outgoingMed)
	require.Error(t, err)
}

func TestIntegrationBridging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Create transaction users, as many as needed.
	ua, _ := sipgo.NewUA(
		sipgo.WithUserAgent("inbound"),
	)
	defer ua.Close()
	tu := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  5090,
		},
	))

	log := asyncLog(t)
	err := serveBackground(t, tu, ctx, func(runCtx context.Context, in *DialogServerSession) {
		in.Trying(runCtx)
		in.Ringing(runCtx)
		med, err := in.Answer(AnswerOptions{})
		if err != nil {
			log("Answer failed", err)
			return
		}
		defer med.Close()

		inCtx := in.Context()
		ctx, cancel := context.WithTimeout(inCtx, 15*time.Second)
		defer cancel()

		// Wa want to bridge this call with originator
		bridge := NewBridge()
		// Add us in bridge
		if err := bridge.AddDialogMedia(med); err != nil {
			log("Adding dialog in bridge failed", err)
			return
		}

		out, _, err := tu.InviteBridge(ctx, sip.Uri{User: "test", Host: "127.0.0.200", Port: 5090}, &bridge, InviteOptions{})
		if err != nil {
			log("Dialing failed", err)
			return
		}

		outCtx := out.Context()
		defer func() {
			hctx, hcancel := context.WithTimeout(outCtx, 5*time.Second)
			out.Hangup(hctx)
			hcancel()
		}()

		// This is beauty, as you can even easily detect who hangups
		select {
		case <-inCtx.Done():
		case <-outCtx.Done():
		}

		// How to now do bridging
	})
	assert.NoError(t, err)

	// The bridged leg's handler runs on a server goroutine, so its errors are
	// handed to the test rather than asserted there.
	echoed := make(chan error, 1)
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.200",
				BindPort:  5090,
			},
		))

		err := serveBackground(t, dg, ctx, func(_ context.Context, d *DialogServerSession) {
			ctx := d.Context()
			med, err := d.Answer(AnswerOptions{})
			if err != nil {
				echoed <- fmt.Errorf("answer: %w", err)
				return
			}
			defer med.Close()

			// ms := d.mediaSession
			buf := make([]byte, media.RTPBufSize)
			r, _ := med.AudioReader()
			n, err := r.Read(buf)
			if err != nil {
				echoed <- fmt.Errorf("read: %w", err)
				return
			}

			w, _ := med.AudioWriter()
			if _, err := w.Write(buf[:n]); err != nil {
				echoed <- fmt.Errorf("write: %w", err)
				return
			}
			echoed <- nil

			<-ctx.Done()
		})
		require.NoError(t, err)
	}

	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := newDialer(ua)
		serveRequests(t, dg)
		dialog, med, err := dg.Invite(context.TODO(), sip.Uri{Host: "127.0.0.1", Port: 5090}, InviteOptions{})
		require.NoError(t, err)
		defer dialog.Close()
		defer med.Close()

		w, _ := med.AudioWriter()
		_, err = w.Write([]byte("1234"))
		require.NoError(t, err)

		buf := make([]byte, media.RTPBufSize)
		r, _ := med.AudioReader()
		r.Read(buf)
		require.NoError(t, err)

		t.Log("Hanguping")
		dialog.Hangup(ctx)
	}

	select {
	case err := <-echoed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the bridged leg did not echo the audio")
	}
}

func TestIntegrationBridgingMix(t *testing.T) {
	// NOTE: There are more tests executed but outside repo
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Create transaction users, as many as needed.
	ua, _ := sipgo.NewUA(
		sipgo.WithUserAgent("inbound"),
	)
	defer ua.Close()
	tu := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  5090,
		},
	))

	// Subtests replace the bridge while the server keeps running. The handler
	// runs on a server goroutine, so it loads the bridge through an atomic
	// pointer that a subtest stores only once the bridge is fully configured.
	var currentBridge atomic.Pointer[BridgeMix]
	currentBridge.Store(NewBridgeMix())
	// Each handler hands the test the error of its join or of its leave
	dialogExit := make(chan error, 10)
	waitDialogExit := func(t *testing.T) {
		t.Helper()
		select {
		case err := <-dialogExit:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("a call handler did not return")
		}
	}
	log := asyncLog(t)
	err := serveBackground(t, tu, ctx, func(ctx context.Context, in *DialogServerSession) {
		var exitErr error
		defer func() { dialogExit <- exitErr }()
		bridge := currentBridge.Load()

		in.Trying(ctx)
		in.Ringing(ctx)
		med, err := in.Answer(AnswerOptions{})
		if err != nil {
			log("Answer failed", err)
			return
		}
		defer med.Close()

		// Add us in bridge
		log("Adding into bridge", in.ID)
		if err := bridge.AddDialogMedia(med); err != nil {
			// A subtest hangs its calls up as soon as they are answered, so a
			// call can end before its handler joins it, which refuses the join
			if in.LoadState() != sip.DialogStateEnded {
				exitErr = fmt.Errorf("adding %s into bridge: %w", in.ID, err)
			}
			return
		}
		defer func() {
			log("Removing from bridge", in.ID)
			if err := bridge.RemoveDialogMedia(med); err != nil {
				exitErr = fmt.Errorf("removing %s from bridge: %w", in.ID, err)
			}
		}()

		<-in.Context().Done()
	})
	assert.NoError(t, err)

	t.Run("BridgeAddRemove", func(t *testing.T) {
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := newDialer(ua)
		serveRequests(t, dg)

		dialogs := make([]*DialogClientSession, 3)
		for i := range 3 {
			t.Log("Inviting", "i", i)
			dialog, _, err := dg.Invite(context.TODO(), sip.Uri{Host: "127.0.0.1", Port: 5090}, InviteOptions{})
			require.NoError(t, err)
			dialogs[i] = dialog
		}
		// bridge.mu.Lock()
		// assert.Equal(t, 3, len(bridge.dialogs))
		// assert.Equal(t, 1, bridge.mixState)
		// bridge.mu.Unlock()

		for _, dialog := range dialogs {
			dialog.Hangup(ctx)
			dialog.Close()
		}

		for range len(dialogs) {
			waitDialogExit(t)
		}
		bridge := currentBridge.Load()
		assert.Equal(t, 0, len(bridge.medias))
		assert.EqualValues(t, 0, bridge.stateRead())
	})

	t.Run("CheckMixing", func(t *testing.T) {

		mixedBuf := make([]byte, 12)
		audio.PCMMix(mixedBuf, mixedBuf, ulawDecode([]byte("123450")))
		audio.PCMMix(mixedBuf, mixedBuf, ulawDecode([]byte("123451")))
		audio.PCMUnmix(mixedBuf, mixedBuf, ulawDecode([]byte("123451")))
		t.Log("Mixed", ulawEncode(mixedBuf), string(ulawEncode(mixedBuf)))
	})

	t.Run("SoundProxied", func(t *testing.T) {
		dialogsCreated := 0
		defer func() {
			for range dialogsCreated {
				waitDialogExit(t)
			}
		}()

		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := newDialer(ua)
		serveRequests(t, dg)

		// Make number of calls that will have audio mixed in bridge
		// wg := sync.WaitGroup{}
		bridge := NewBridgeMix()
		bridge.WaitDialogsNum = 2 // Do not start mixing until both dialogs get joined, otherwise there will be no guarantee when something is mixed
		currentBridge.Store(bridge)

		dialog1, med1, err := dg.Invite(context.TODO(), sip.Uri{Host: "127.0.0.1", Port: 5090}, InviteOptions{})
		require.NoError(t, err)
		dialogsCreated++
		defer dialog1.Hangup(t.Context())
		defer dialog1.Close()
		defer med1.Close()

		dialog2, med2, err := dg.Invite(context.TODO(), sip.Uri{Host: "127.0.0.1", Port: 5090}, InviteOptions{})
		require.NoError(t, err)
		dialogsCreated++
		defer dialog2.Hangup(t.Context())
		defer dialog2.Close()
		defer med2.Close()

		// Write sound on dialog 1 and make sure it is read on dialog2
		sound := []byte("123450")
		go func(dialog *DialogClientSession) {
			w, _ := med1.AudioWriter()
			for i := 0; i < 10; i++ {
				w.Write(sound)
			}

		}(dialog1)

		r, _ := med2.AudioReader()
		med2.StopRTP(1, 1*time.Second)

		buf := make([]byte, media.RTPBufSize)
		for i := 0; i < 10; i++ {
			n, err := r.Read(buf)
			require.NoError(t, err)
			assert.Equal(t, sound, buf[:n])
			t.Log("Sound received", "buf", buf[:n])
		}

	})
}
