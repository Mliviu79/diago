// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/require"
)

// newWebrtcJitterCall returns a WebRTC call whose dialog reads its audio
// through a jitter buffer, the audio reader, and the buffer.
func newWebrtcJitterCall(t *testing.T) (*DialogWebrtc, *webrtcHandlePeer, io.Reader, *media.RTPJitterBuffer) {
	t.Helper()
	d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000})
	ar, err := d.AudioReader(WithAudioReaderWebrtcJitterBuffer(media.RTPJitterBufferOptions{
		DelayPackets: 1,
		MaxPackets:   4,
	}))
	require.NoError(t, err)
	jitter, ok := d.RTPPacketReader.Reader().(*media.RTPJitterBuffer)
	require.True(t, ok, "the audio reader reads no jitter buffer")
	return d, p, ar, jitter
}

// sendAndReadThrough has the peer send frames of audio and reads each through
// r.
func sendAndReadThrough(t *testing.T, p *webrtcHandlePeer, r io.Reader, frames int) {
	t.Helper()
	for range frames {
		p.sendAudio(t)
		_, err := readWithin(t, r, 5*time.Second)
		require.NoError(t, err)
	}
}

// TestDialogWebrtcJitterBufferFollowsMediaUpdates pins that the jitter buffer
// of a WebRTC call keeps reading the audio across a media update: a new
// transport and a peer's offer applied in place. The buffer stays in front of
// the packet reader, and what the peer sends after the update is read
// through it.
func TestDialogWebrtcJitterBufferFollowsMediaUpdates(t *testing.T) {
	updates := map[string]func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer){
		"NewTransport": func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer) { p.reinviteNewTransport(t, d, nil) },
		"InPlace":      func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer) { p.reinviteInPlace(t, d) },
	}
	for name, update := range updates {
		t.Run(name, func(t *testing.T) {
			d, p, ar, jitter := newWebrtcJitterCall(t)
			sendAndReadThrough(t, p, ar, 3)

			update(t, d, p)
			released := jitter.Statistics().PacketsReleased
			sendAndReadThrough(t, p, ar, 5)
			require.True(t, d.RTPPacketReader.Reader() == media.RTPReader(jitter), "the media update took the jitter buffer away")
			require.GreaterOrEqual(t, jitter.Statistics().PacketsReleased, released+4, "packets read past the jitter buffer after the update")
		})
	}
}

// TestDialogWebrtcJitterBufferSetUpOnce pins that a WebRTC call takes one
// jitter buffer: a second one is refused and the first stays.
func TestDialogWebrtcJitterBufferSetUpOnce(t *testing.T) {
	d, _, _, jitter := newWebrtcJitterCall(t)
	_, err := d.AudioReader(WithAudioReaderWebrtcJitterBuffer(media.RTPJitterBufferOptions{}))
	require.Error(t, err)
	require.True(t, d.RTPPacketReader.Reader() == media.RTPReader(jitter), "the first jitter buffer was replaced")
}

// TestDialogWebrtcJitterBufferBeforeSetup pins ErrNoMediaSetup for a jitter
// buffer asked for before the media is set up.
func TestDialogWebrtcJitterBufferBeforeSetup(t *testing.T) {
	d := &DialogWebrtc{}
	_, err := d.AudioReader(WithAudioReaderWebrtcJitterBuffer(media.RTPJitterBufferOptions{}))
	require.ErrorIs(t, err, ErrNoMediaSetup)
}

// TestDialogWebrtcJitterBufferKeepsReadStop pins that a stop of the reads set
// before the jitter buffer is put in is still in force after it.
func TestDialogWebrtcJitterBufferKeepsReadStop(t *testing.T) {
	d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000})
	require.NoError(t, d.StopRTP(1, 0))
	ar, err := d.AudioReader(WithAudioReaderWebrtcJitterBuffer(media.RTPJitterBufferOptions{
		DelayPackets: 1,
		MaxPackets:   4,
	}))
	require.NoError(t, err)

	p.sendAudio(t)
	_, err = readWithin(t, ar, 5*time.Second)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
}

// TestDialogWebrtcCloseJoinsJitterBuffer pins that Close of a WebRTC call
// returns only after the jitter buffer's read loop has stopped reading the
// dialog's session. Closing the transport fails the loop's read; the upstream
// here holds it between the two.
func TestDialogWebrtcCloseJoinsJitterBuffer(t *testing.T) {
	d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000})
	upstream := &holdingRTPReader{
		inner:   d.RTPPacketReader.Reader(),
		held:    make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	d.RTPPacketReader.UpdateReader(upstream)
	// UpdateReader ends a read waiting on the session it replaces with a read
	// deadline, which upstream would read under.
	require.NoError(t, d.StartRTP(1, 0))
	ar, err := d.AudioReader(WithAudioReaderWebrtcJitterBuffer(media.RTPJitterBufferOptions{
		DelayPackets: 1,
		MaxPackets:   4,
	}))
	require.NoError(t, err)
	jitter, ok := d.RTPPacketReader.Reader().(*media.RTPJitterBuffer)
	require.True(t, ok)

	release := sync.OnceFunc(func() { close(upstream.release) })
	closed := make(chan error, 1)
	closeDone := make(chan struct{})
	t.Cleanup(func() {
		release()
		select {
		case <-closeDone:
		case <-time.After(5 * time.Second):
			t.Error("Close did not return")
		}
	})

	// One packet read through the buffer starts its read loop, which then
	// waits for the next one.
	sendAndReadThrough(t, p, ar, 1)

	go func() {
		defer close(closeDone)
		closed <- d.Close()
	}()
	select {
	case <-upstream.held:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the transport did not fail the read loop's read")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while the jitter buffer's read loop was still reading", err)
	case <-time.After(100 * time.Millisecond):
	}

	release()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return once the read loop could stop")
	}
	select {
	case <-jitter.Done():
	default:
		t.Fatal("Close returned before the read loop stopped")
	}
}
