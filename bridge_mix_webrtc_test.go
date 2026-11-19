// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBridgeMixWebrtcMixesWithDialogMedia checks that a WebRTC call's media is
// mixed with a DialogMedia's, both ways, and leaves the bridge again.
func TestBridgeMixWebrtcMixesWithDialogMedia(t *testing.T) {
	b := NewBridgeMix()
	// The peers send their frames as the test goes, not in real time
	b.RealtimeReader = false
	w, p := newWebrtcHandleCall(t, webrtcBridgeCodecs)
	listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
	webrtcHeard := newBridgeTestWriter()
	w.SetAudioWriter(webrtcHeard)
	listenerHeard := newBridgeTestWriter()
	listener.media.audioWriter = listenerHeard
	require.NoError(t, b.AddDialogWebrtc(w))
	require.NoError(t, b.AddDialogMedia(listener.media))
	t.Cleanup(func() { stopBridgeMix(t, b) })
	assert.Equal(t, []*DialogWebrtc{w}, b.DialogWebrtcList())
	assert.Equal(t, []*DialogMedia{listener.media}, b.DialogMediaList())

	// Every byte of a PCMU frame is its value, which decodes and encodes back
	// to itself
	p.send(t, 0, false, 160, bytes.Repeat([]byte{0x20}, 160))
	waitHeard(t, listenerHeard, 0x20)
	listener.sendRTP(t, 1, 0, bytes.Repeat([]byte{0x21}, 160))
	waitHeard(t, webrtcHeard, 0x21)

	require.NoError(t, b.RemoveDialogWebrtc(w))
	assert.Empty(t, b.DialogWebrtcList())
	assert.Equal(t, []*DialogMedia{listener.media}, b.DialogMediaList())
}

// TestBridgeMixWebrtcFollowsCodecChange checks that a re-INVITE moving a WebRTC
// call in the bridge from PCMU to PCMA, on a new transport, is followed at
// once: the mix decodes what the call sends as PCMA.
func TestBridgeMixWebrtcFollowsCodecChange(t *testing.T) {
	b := NewBridgeMix()
	b.RealtimeReader = false
	withAlaw := []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000}
	w, p := newWebrtcHandleCallWith(t, withAlaw, withAlaw)
	listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
	w.SetAudioWriter(newBridgeTestWriter())
	listenerHeard := newBridgeTestWriter()
	listener.media.audioWriter = listenerHeard
	require.NoError(t, b.AddDialogWebrtc(w))
	require.NoError(t, b.AddDialogMedia(listener.media))
	t.Cleanup(func() { stopBridgeMix(t, b) })

	p.send(t, 0, false, 160, bytes.Repeat([]byte{0x20}, 160))
	waitHeard(t, listenerHeard, 0x20)

	p.reinviteNewTransport(t, w, []media.Codec{media.CodecAudioAlaw, media.CodecTelephoneEvent8000})
	props, err := w.NegotiatedProps()
	require.NoError(t, err)
	require.Equal(t, media.CodecAudioAlaw.Name, props.Codec.Name)

	// What the call says in PCMA the listener hears in PCMU
	alawFrame := bytes.Repeat([]byte{0x55}, 160)
	pcm := make([]byte, 2*len(alawFrame))
	_, err = audio.DecodeAlawTo(pcm, alawFrame)
	require.NoError(t, err)
	want := ulawEncode(pcm)[0]
	require.NotEqual(t, ulawEncode(ulawDecode(alawFrame))[0], want, "the frame must tell PCMA from PCMU")
	p.send(t, media.CodecAudioAlaw.PayloadType, false, 320, alawFrame)
	waitHeard(t, listenerHeard, want)
}

// TestBridgeMixRefusesDialogWebrtcWithoutMedia checks that BridgeMix refuses a
// WebRTC call's media that has no media session with ErrNoMediaSetup.
func TestBridgeMixRefusesDialogWebrtcWithoutMedia(t *testing.T) {
	b := NewBridgeMix()
	var err error
	require.NotPanics(t, func() { err = b.AddDialogWebrtc(&DialogWebrtc{}) })
	require.ErrorIs(t, err, ErrNoMediaSetup)
	assert.Empty(t, b.DialogWebrtcList())
}

// bridgeDeadlineOnceWriter fails its first write on a write deadline and
// records the frames written after it.
type bridgeDeadlineOnceWriter struct {
	failed atomic.Bool
	heard  *bridgeTestWriter
}

func (w *bridgeDeadlineOnceWriter) Write(b []byte) (int, error) {
	if w.failed.CompareAndSwap(false, true) {
		return 0, os.ErrDeadlineExceeded
	}
	return w.heard.Write(b)
}

// TestBridgeMixWriteDeadline checks what a running mix does with a media whose
// write fails on a write deadline: by default the media is taken out of the
// running mix, and with WriteDeadlineLosesFrame it loses that frame alone and
// is written the next. Either way the others keep being mixed.
func TestBridgeMixWriteDeadline(t *testing.T) {
	for _, losesFrame := range []bool{false, true} {
		name := "Default"
		if losesFrame {
			name = "WriteDeadlineLosesFrame"
		}
		t.Run(name, func(t *testing.T) {
			b := NewBridgeMix()
			b.WriteDeadlineLosesFrame = losesFrame
			talker := newBridgeTestDialog(t, "talker", media.CodecAudioUlaw)
			failed := newBridgeTestDialog(t, "failed", media.CodecAudioUlaw)
			listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
			talker.media.audioWriter = newBridgeTestWriter()
			failedWriter := &bridgeDeadlineOnceWriter{heard: newBridgeTestWriter()}
			failed.media.audioWriter = failedWriter
			heard := newBridgeTestWriter()
			listener.media.audioWriter = heard
			for _, d := range []*bridgeTestDialog{talker, failed, listener} {
				require.NoError(t, b.AddDialogMedia(d.media))
			}
			t.Cleanup(func() { stopBridgeMix(t, b) })

			const frames = 5
			start := time.Now()
			for i := range frames {
				time.Sleep(time.Until(start.Add(time.Duration(i) * 20 * time.Millisecond)))
				talker.sendRTP(t, uint16(i), uint32(i)*160, bytes.Repeat([]byte{byte(0x20 + i)}, 160))
			}
			for i := range frames {
				waitHeard(t, heard, byte(0x20+i))
			}
			assert.Equal(t, 1, b.stateRead(), "the mix must keep running")
			require.True(t, failedWriter.failed.Load(), "the write deadline was never met")

			if losesFrame {
				waitHeard(t, failedWriter.heard, byte(0x20+frames-1))
				return
			}
			select {
			case got := <-failedWriter.heard.frames:
				t.Fatalf("the media taken out of the mix was written %#x", got[0])
			default:
			}
		})
	}
}
