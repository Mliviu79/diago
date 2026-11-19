// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"os"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webrtcBridgeCodecs are the codecs of a WebRTC party of the bridge tests and
// of its peer: PCMU, as the bridgeTestDialog parties run on, and
// telephone-event, so the party can be sent DTMF.
var webrtcBridgeCodecs = []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000}

// sendWebrtcKey has the peer of a WebRTC party press the key 1.
func sendWebrtcKey(t *testing.T, p *webrtcHandlePeer) {
	t.Helper()
	pt := media.CodecTelephoneEvent8000.PayloadType
	p.send(t, pt, true, 8000, media.DTMFEncode(media.DTMFEvent{Event: 1, Volume: 10, Duration: 160}))
	p.send(t, pt, false, 8000, media.DTMFEncode(media.DTMFEvent{Event: 1, EndOfEvent: true, Volume: 10, Duration: 320}))
}

// requireWebrtcKey reads what a WebRTC party sends its peer until a
// telephone-event packet arrives, bounded, and requires it to carry the key 1.
func requireWebrtcKey(t *testing.T, p *webrtcHandlePeer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	require.NoError(t, p.sess.StopRTP(1, time.Until(deadline)))
	defer func() { require.NoError(t, p.sess.StartRTP(1)) }()
	buf := make([]byte, media.RTPBufSize)
	pkt := rtp.Packet{}
	for pkt.PayloadType != media.CodecTelephoneEvent8000.PayloadType {
		_, err := p.sess.ReadRTP(buf, &pkt)
		require.NoError(t, err, "the WebRTC party was sent no key")
	}
	ev := media.DTMFEvent{}
	require.NoError(t, media.DTMFDecode(pkt.Payload, &ev))
	assert.EqualValues(t, 1, ev.Event)
}

// requireMediaKey reads what a DialogMedia party sends its peer until a
// telephone-event packet arrives, bounded, and requires it to carry the key 1.
func requireMediaKey(t *testing.T, d *bridgeTestDialog) {
	t.Helper()
	require.NoError(t, d.peer.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, media.RTPBufSize)
	pkt := rtp.Packet{}
	for pkt.PayloadType != media.CodecTelephoneEvent8000.PayloadType {
		n, _, err := d.peer.ReadFrom(buf)
		require.NoError(t, err, "the other party was sent no key")
		require.NoError(t, pkt.Unmarshal(buf[:n]))
	}
	ev := media.DTMFEvent{}
	require.NoError(t, media.DTMFDecode(pkt.Payload, &ev))
	assert.EqualValues(t, 1, ev.Event)
}

// requireWebrtcSilent requires the WebRTC party to send its peer nothing for a
// moment.
func requireWebrtcSilent(t *testing.T, p *webrtcHandlePeer) {
	t.Helper()
	require.NoError(t, p.sess.StopRTP(1, 200*time.Millisecond))
	defer func() { require.NoError(t, p.sess.StartRTP(1)) }()
	_, err := p.sess.ReadRTP(make([]byte, media.RTPBufSize), &rtp.Packet{})
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the key was sent back to the party it was pressed on")
}

// TestBridgeWebrtcDTMFPassSendsKeysToTheOtherParty checks that a proxy
// passing DTMF sends a key pressed on one party to the other party as a
// telephone-event, whichever media type each party has, and none back to the
// party it was pressed on.
func TestBridgeWebrtcDTMFPassSendsKeysToTheOtherParty(t *testing.T) {
	newBridge := func(t *testing.T, add ...func(b *Bridge) error) {
		t.Helper()
		b := NewBridge()
		b.WaitDialogsNum = 3 // The proxy is started by hand
		b.DTMFpass = true
		for _, f := range add {
			require.NoError(t, f(&b))
		}
		stop, err := proxyMediaControl(t, &b)
		require.NoError(t, err)
		t.Cleanup(func() { require.NoError(t, stop()) })
	}

	t.Run("MediaToWebrtc", func(t *testing.T) {
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		w, p := newWebrtcHandleCall(t, webrtcBridgeCodecs)
		newBridge(t,
			func(b *Bridge) error { return b.AddDialogMedia(a.media) },
			func(b *Bridge) error { return b.AddDialogWebrtc(w) },
		)

		a.sendDTMF(t, 1, 160, true, media.DTMFEvent{Event: 1, Volume: 10, Duration: 160})
		a.sendDTMF(t, 2, 160, false, media.DTMFEvent{Event: 1, EndOfEvent: true, Volume: 10, Duration: 320})
		requireWebrtcKey(t, p)
	})

	t.Run("WebrtcToMedia", func(t *testing.T) {
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		w, p := newWebrtcHandleCall(t, webrtcBridgeCodecs)
		newBridge(t,
			func(b *Bridge) error { return b.AddDialogWebrtc(w) },
			func(b *Bridge) error { return b.AddDialogMedia(a.media) },
		)

		sendWebrtcKey(t, p)
		requireMediaKey(t, a)
		requireWebrtcSilent(t, p)
	})

	t.Run("WebrtcToWebrtc", func(t *testing.T) {
		w1, p1 := newWebrtcHandleCall(t, webrtcBridgeCodecs)
		w2, p2 := newWebrtcHandleCall(t, webrtcBridgeCodecs)
		newBridge(t,
			func(b *Bridge) error { return b.AddDialogWebrtc(w1) },
			func(b *Bridge) error { return b.AddDialogWebrtc(w2) },
		)

		sendWebrtcKey(t, p1)
		requireWebrtcKey(t, p2)
		requireWebrtcSilent(t, p1)
	})
}

// TestBridgeWebrtcDTMFPassKeepsDialogAudio checks that a proxy passing DTMF
// leaves a WebRTC party the audio reader and writer it had.
func TestBridgeWebrtcDTMFPassKeepsDialogAudio(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 3 // The proxy is started by hand
	b.DTMFpass = true
	w, _ := newWebrtcHandleCall(t, webrtcBridgeCodecs)
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	r, err := w.AudioReader()
	require.NoError(t, err)
	wr, err := w.AudioWriter()
	require.NoError(t, err)
	require.NoError(t, b.AddDialogWebrtc(w))
	require.NoError(t, b.AddDialogMedia(a.media))
	stop, err := proxyMediaControl(t, &b)
	require.NoError(t, err)
	require.NoError(t, stop())

	after, err := w.AudioReader()
	require.NoError(t, err)
	assert.Same(t, r, after, "the WebRTC party must keep its audio reader")
	afterW, err := w.AudioWriter()
	require.NoError(t, err)
	assert.Same(t, wr, afterW, "the WebRTC party must keep its audio writer")
}

// TestBridgeWebrtcProxyEndsOnCodecChange checks that a Bridge stops proxying
// once a re-INVITE moves its WebRTC party from PCMU to PCMA, with an error
// saying the bridge does not transcode.
func TestBridgeWebrtcProxyEndsOnCodecChange(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 3 // The proxy is started by hand
	withAlaw := []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000}
	w, p := newWebrtcHandleCallWith(t, withAlaw, withAlaw)
	other := newBridgeTestDialog(t, "other", media.CodecAudioUlaw)
	require.NoError(t, b.AddDialogWebrtc(w))
	require.NoError(t, b.AddDialogMedia(other.media))
	proxied := make(chan error, 1)
	go func() { proxied <- b.ProxyMedia(t.Context()) }()
	p.sendAudio(t)
	require.Len(t, other.recvRTP(t), 160, "the proxy does not run")

	p.reinviteNewTransport(t, w, []media.Codec{media.CodecAudioAlaw, media.CodecTelephoneEvent8000})
	select {
	case err := <-proxied:
		require.ErrorIs(t, err, errBridgeNoTranscoding)
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy went on between PCMA and PCMU")
	}
}

// TestBridgeWebrtcStopsSilentParty checks that the proxy the bridge starts
// between a WebRTC party and another stops while both are silent: the stop
// ends a read waiting on the WebRTC transport, and the party can be read
// again after.
func TestBridgeWebrtcStopsSilentParty(t *testing.T) {
	b := NewBridge()
	w, p := newWebrtcHandleCall(t, webrtcBridgeCodecs)
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	require.NoError(t, b.AddDialogWebrtc(w))
	require.NoError(t, b.AddDialogMedia(a.media))
	frame := bytes.Repeat([]byte{0x20}, 160)
	a.sendRTP(t, 1, 0, frame)
	require.Equal(t, frame, p.recv(t).Payload, "the proxy does not run")

	require.NoError(t, stopProxyMedia(t, &b))
	r, err := w.AudioReader()
	require.NoError(t, err)
	p.sendAudio(t)
	_, err = readWithin(t, r, 5*time.Second)
	require.NoError(t, err, "the WebRTC party can not be read once the proxy stopped")
}

// TestBridgeRefusesDialogWebrtcWithoutMedia checks that Bridge refuses a
// WebRTC party that has no media session with ErrNoMediaSetup and keeps the
// parties it has.
func TestBridgeRefusesDialogWebrtcWithoutMedia(t *testing.T) {
	b := NewBridge()
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	require.NoError(t, b.AddDialogMedia(a.media))
	var err error
	require.NotPanics(t, func() { err = b.AddDialogWebrtc(&DialogWebrtc{}) })
	require.ErrorIs(t, err, ErrNoMediaSetup)
	assert.Equal(t, []*DialogMedia{a.media}, bridgedMedias(&b))
}
