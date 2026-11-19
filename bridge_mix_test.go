// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestBridgeMixStopKeepsStoppingState checks that a mix loop stopped by
// mixStopWait leaves the bridge in the stopping state until the stopping caller
// has resumed. A caller that found the bridge idle earlier would start a mix and
// add to mixWG while the stopping caller is still waiting on it.
func TestBridgeMixStopKeepsStoppingState(t *testing.T) {
	t.Run("StoppedLoop", func(t *testing.T) {
		b := NewBridgeMix()
		// A mix loop is running
		b.mu.Lock()
		b.mixWG.Add(1)
		b.stateWriteUnsafe(1)
		b.mu.Unlock()

		stopped := make(chan error, 1)
		go func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			stopped <- b.mixStopWait()
		}()
		require.Eventually(t, func() bool { return b.stateRead() == 2 }, time.Second, time.Millisecond)

		// The loop exits while the stopping caller is still waiting on mixWG
		b.mixEnded()
		assert.Equal(t, 2, b.stateRead(), "bridge must stay stopping until the stopping caller resumes")

		b.mixWG.Done()
		select {
		case err := <-stopped:
			require.NoError(t, err)
		case <-time.After(time.Second):
			t.Fatal("mixStopWait did not return after the loop exited")
		}
		assert.Equal(t, 0, b.stateRead())
	})

	t.Run("LoopEndedOnItsOwn", func(t *testing.T) {
		b := NewBridgeMix()
		b.mu.Lock()
		b.stateWriteUnsafe(1)
		b.mu.Unlock()

		b.mixEnded()
		assert.Equal(t, 0, b.stateRead())
	})
}

// bridgeTestWriter records every frame the bridge writes to a dialog, and
// returns at once.
type bridgeTestWriter struct {
	frames chan []byte
}

func newBridgeTestWriter() *bridgeTestWriter {
	return &bridgeTestWriter{frames: make(chan []byte, 1024)}
}

func (w *bridgeTestWriter) Write(b []byte) (int, error) {
	select {
	case w.frames <- slices.Clone(b):
	default:
	}
	return len(b), nil
}

// stopBridgeMix stops the bridge's mix and waits, bounded, until it has.
func stopBridgeMix(t *testing.T, b *BridgeMix) {
	t.Helper()
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		b.mu.Lock()
		defer b.mu.Unlock()
		_ = b.mixStopWait()
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("bridge mix did not stop")
	}
}

// TestBridgeMixRTPControlErrors checks what a join or leave does when starting
// or stopping reads on a dialog in the bridge fails. A hung-up dialog, whose
// connection the BYE has already closed, leaves without an error. Any other
// failure is returned: a join is refused, a leave still takes the dialog out,
// and the dialogs left in the bridge keep being mixed.
func TestBridgeMixRTPControlErrors(t *testing.T) {
	errDeadline := errors.New("read deadline refused")

	t.Run("HungUpDialogLeaves", func(t *testing.T) {
		b := NewBridgeMix()
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
		hungUp := newBridgeTestDialog(t, "hungup", media.CodecAudioUlaw)
		for _, d := range []*bridgeTestDialog{a, c, hungUp} {
			require.NoError(t, b.AddDialogMedia(d.media))
		}
		t.Cleanup(func() { stopBridgeMix(t, b) })

		// A BYE closes the dialog's media before its handler leaves the bridge
		require.NoError(t, hungUp.conn.Close())
		require.NoError(t, b.RemoveDialogMedia(hungUp.media))
		assert.Equal(t, []*DialogMedia{a.media, c.media}, b.DialogMediaList())
		assert.Equal(t, 1, b.stateRead(), "the dialogs left must be mixed")
	})

	t.Run("JoinRefused", func(t *testing.T) {
		b := NewBridgeMix()
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		failing := newBridgeTestDialog(t, "failing", media.CodecAudioUlaw)
		failing.conn.deadlineErr = errDeadline
		for _, d := range []*bridgeTestDialog{a, failing} {
			require.NoError(t, b.AddDialogMedia(d.media))
		}
		t.Cleanup(func() { stopBridgeMix(t, b) })

		err := b.AddDialogMedia(newBridgeTestDialog(t, "c", media.CodecAudioUlaw).media)
		require.ErrorIs(t, err, errDeadline)
		assert.Equal(t, []*DialogMedia{a.media, failing.media}, b.DialogMediaList(), "a refused join must not add the dialog")
		assert.Equal(t, 1, b.stateRead(), "the dialogs in the bridge must keep being mixed")
	})

	t.Run("LeaveReportsError", func(t *testing.T) {
		b := NewBridgeMix()
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
		failing := newBridgeTestDialog(t, "failing", media.CodecAudioUlaw)
		failing.conn.deadlineErr = errDeadline
		for _, d := range []*bridgeTestDialog{a, c, failing} {
			require.NoError(t, b.AddDialogMedia(d.media))
		}
		t.Cleanup(func() { stopBridgeMix(t, b) })

		err := b.RemoveDialogMedia(a.media)
		require.ErrorIs(t, err, errDeadline)
		assert.Equal(t, []*DialogMedia{c.media, failing.media}, b.DialogMediaList(), "a leave must take the dialog out")
		assert.Equal(t, 1, b.stateRead(), "the dialogs left must be mixed")
	})
}

// TestBridgeMixWaitsForStopInProgress checks that a leave arriving while
// another caller is stopping the mix waits for that stop to finish. Otherwise
// it restarts reads on every dialog while the stopped mix is still reading
// them, and returns before that mix has stopped reading the dialog it removed.
func TestBridgeMixWaitsForStopInProgress(t *testing.T) {
	b := NewBridgeMix()
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
	t.Cleanup(func() { stopBridgeMix(t, b) })

	// A mix is running. The test stands in for its goroutines, which keep
	// reading until the test lets them stop.
	b.mu.Lock()
	b.medias = []bridgeMixMedia{a.media, c.media}
	b.mixWG.Add(1)
	b.stateWriteUnsafe(1)
	b.mu.Unlock()

	stopped := make(chan error, 1)
	go func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		stopped <- b.mixStopWait()
	}()
	require.Eventually(t, func() bool { return b.stateRead() == 2 }, time.Second, time.Millisecond)

	removed := make(chan error, 1)
	go func() { removed <- b.RemoveDialogMedia(a.media) }()
	assert.Never(t, func() bool { return len(removed) > 0 }, 100*time.Millisecond, time.Millisecond,
		"a leave must not return while the stopped mix is still reading its dialog")

	b.mixWG.Done()
	for _, done := range []chan error{stopped, removed} {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("stop or leave did not return after the mix stopped")
		}
	}
	assert.Equal(t, []*DialogMedia{c.media}, b.DialogMediaList())
}

// TestBridgeMixLeaveStopsReadersOfEndedLoop checks that a leave stops the
// readers of a mix whose loop ended on its own before it returns. The loop ends
// on its own once every dialog is dropped from the mix for its closed media, and
// a reader can then still be reading a dialog whose writes failed first.
func TestBridgeMixLeaveStopsReadersOfEndedLoop(t *testing.T) {
	b := NewBridgeMix()
	talker := newBridgeTestDialog(t, "talker", media.CodecAudioUlaw)
	hungUp := newBridgeTestDialog(t, "hungup", media.CodecAudioUlaw)
	// The talker's writes report its media closed while its reads go on
	talker.media.audioWriter = bridgeFailingWriter{err: net.ErrClosed}
	for _, d := range []*bridgeTestDialog{talker, hungUp} {
		require.NoError(t, b.AddDialogMedia(d.media))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	// The talker's frame is mixed and written to both dialogs. Both writes fail
	// for closed media, the hung-up dialog's on its closed connection, so both
	// are dropped and the loop ends.
	require.NoError(t, hungUp.conn.Close())
	talker.sendRTP(t, 1, 160, make([]byte, 160))
	require.Eventually(t, func() bool { return b.stateRead() == 0 }, 2*time.Second, time.Millisecond, "mix loop did not end")
	require.Eventually(t, func() bool { return talker.conn.reading.Load() == 1 }, 2*time.Second, time.Millisecond,
		"the talker's reader did not go back to reading")

	require.NoError(t, b.RemoveDialogMedia(talker.media))
	assert.Zero(t, talker.conn.reading.Load(), "a reader of the ended mix is still reading the dialog that left")
}

// TestBridgeMixRefusesCodecMismatch checks that a dialog whose audio differs
// from the bridge's in sample rate or frame duration is refused, since the mix
// neither resamples nor reframes, and that the dialogs already in the bridge
// keep being mixed with no reader of the refused mix left behind.
func TestBridgeMixRefusesCodecMismatch(t *testing.T) {
	ulaw30ms := media.CodecAudioUlaw
	ulaw30ms.SampleDur = 30 * time.Millisecond

	for _, tc := range []struct {
		name  string
		codec media.Codec
	}{
		{"SampleDur", ulaw30ms},
		{"SampleRate", media.CodecAudioOpus},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBridgeMix()
			a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
			require.NoError(t, b.AddDialogMedia(a.media))
			t.Cleanup(func() { stopBridgeMix(t, b) })

			err := b.AddDialogMedia(newBridgeTestDialog(t, "mismatch", tc.codec).media)
			require.ErrorContains(t, err, "Resampling or transcoding is not supported")
			assert.Equal(t, []*DialogMedia{a.media}, b.DialogMediaList(), "a refused join must not add the dialog")
			assert.Equal(t, 1, b.stateRead(), "the dialogs in the bridge must keep being mixed")

			require.Eventually(t, func() bool { return a.conn.reading.Load() >= 1 }, 2*time.Second, time.Millisecond,
				"the mix is not reading the dialog")
			assert.Never(t, func() bool { return a.conn.reading.Load() > 1 }, 100*time.Millisecond, time.Millisecond,
				"a reader of the refused mix is still reading the dialog")
		})
	}
}

// TestBridgeMixPollDeliversRealtimeAudio checks that a mix in poll mode passes
// on every frame of a stream arriving in real time. A stream's reader holds the
// frame it has read until the mix takes it, and the realtime reader drops a
// frame read more than two frame durations late, so the mix must take each
// frame within a frame duration, however fast its writers return.
func TestBridgeMixPollDeliversRealtimeAudio(t *testing.T) {
	testBridgeMixDeliversRealtimeAudio(t, true)
}

// TestBridgeMixDirectReadDeliversRealtimeAudio checks the same with Poll off,
// where the mix reads each stream itself with a short deadline, and that the
// frames it reads are mixed.
func TestBridgeMixDirectReadDeliversRealtimeAudio(t *testing.T) {
	testBridgeMixDeliversRealtimeAudio(t, false)
}

func testBridgeMixDeliversRealtimeAudio(t *testing.T, poll bool) {
	ulaw10ms := media.CodecAudioUlaw
	ulaw10ms.SampleDur = 10 * time.Millisecond

	for _, codec := range []media.Codec{media.CodecAudioUlaw, ulaw10ms} {
		t.Run(codec.SampleDur.String(), func(t *testing.T) {
			b := NewBridgeMix()
			b.Poll = poll
			talker := newBridgeTestDialog(t, "talker", codec)
			listener := newBridgeTestDialog(t, "listener", codec)
			talker.media.audioWriter = newBridgeTestWriter()
			heard := newBridgeTestWriter()
			listener.media.audioWriter = heard
			for _, d := range []*bridgeTestDialog{talker, listener} {
				require.NoError(t, b.AddDialogMedia(d.media))
			}
			t.Cleanup(func() { stopBridgeMix(t, b) })

			// Every byte of frame i is 0x20+i, which decodes and encodes back to
			// itself
			const frames = 25
			var sent []byte
			start := time.Now()
			for i := range frames {
				time.Sleep(time.Until(start.Add(time.Duration(i) * codec.SampleDur)))
				frame := byte(0x20 + i)
				payload := bytes.Repeat([]byte{frame}, int(codec.SampleTimestamp()))
				talker.sendRTP(t, uint16(i), uint32(i)*codec.SampleTimestamp(), payload)
				sent = append(sent, frame)
			}

			var got []byte
			timeout := time.After(2 * time.Second)
			for len(got) < frames {
				select {
				case frame := <-heard.frames:
					got = append(got, frame[0])
					continue
				case <-timeout:
				}
				break
			}
			assert.Equal(t, sent, got, "the listener must hear every frame the talker sent")
		})
	}
}

// waitHeard waits, bounded, until the writer records a frame whose first byte
// is frame, and fails the test if it does not.
func waitHeard(t *testing.T, w *bridgeTestWriter, frame byte) {
	t.Helper()
	timeout := time.After(2 * time.Second)
	for {
		select {
		case got := <-w.frames:
			if got[0] == frame {
				return
			}
		case <-timeout:
			t.Fatalf("frame %#x was not heard", frame)
		}
	}
}

// TestBridgeMixLeaveKeepsDialogReader checks that, with RealtimeReaderPerMix
// set, a dialog leaves the bridge with the audio reader it joined with. The
// mix reads a dialog through a realtime reader, which drops a frame that was
// waiting to be read for longer than it allows. Left on the dialog, it drops
// the frames of an application that reads at its own pace after the leave.
func TestBridgeMixLeaveKeepsDialogReader(t *testing.T) {
	b := NewBridgeMix()
	b.RealtimeReaderPerMix = true
	talker := newBridgeTestDialog(t, "talker", media.CodecAudioUlaw)
	listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
	talker.media.audioWriter = newBridgeTestWriter()
	heard := newBridgeTestWriter()
	listener.media.audioWriter = heard
	joinedWith, err := talker.media.AudioReader()
	require.NoError(t, err)
	for _, d := range []*bridgeTestDialog{talker, listener} {
		require.NoError(t, b.AddDialogMedia(d.media))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	// Every byte of a frame is its value, which decodes and encodes back to
	// itself
	talker.sendRTP(t, 1, 0, bytes.Repeat([]byte{0x20}, 160))
	waitHeard(t, heard, 0x20)
	require.NoError(t, b.RemoveDialogMedia(talker.media))

	leftWith, err := talker.media.AudioReader()
	require.NoError(t, err)
	assert.Same(t, joinedWith, leftWith, "the dialog must leave with the reader it joined with")

	// The application reads the next frame of the stream well over two frame
	// durations after the mix read the one before it
	time.Sleep(100 * time.Millisecond)
	talker.sendRTP(t, 2, 160, bytes.Repeat([]byte{0x21}, 160))
	require.NoError(t, talker.conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, media.RTPBufSize)
	n, err := leftWith.Read(buf)
	require.NoError(t, err, "the application must read the frame the peer sent")
	assert.Equal(t, bytes.Repeat([]byte{0x21}, 160), buf[:n])
}

// TestBridgeMixRealtimeReaderPerMix checks that, with RealtimeReaderPerMix
// set, a mix judges a dialog's frames late only against the frames it has read
// itself. A frame sent after the peer paused and resumed its RTP clock where
// it left off is heard in the mix that replaced the one before the pause.
func TestBridgeMixRealtimeReaderPerMix(t *testing.T) {
	b := NewBridgeMix()
	b.RealtimeReaderPerMix = true
	talker := newBridgeTestDialog(t, "talker", media.CodecAudioUlaw)
	listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
	talker.media.audioWriter = newBridgeTestWriter()
	heard := newBridgeTestWriter()
	listener.media.audioWriter = heard
	for _, d := range []*bridgeTestDialog{talker, listener} {
		require.NoError(t, b.AddDialogMedia(d.media))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	talker.sendRTP(t, 1, 0, bytes.Repeat([]byte{0x20}, 160))
	waitHeard(t, heard, 0x20)

	// A join replaces the mix while the talker pauses
	joining := newBridgeTestDialog(t, "joining", media.CodecAudioUlaw)
	joining.media.audioWriter = newBridgeTestWriter()
	require.NoError(t, b.AddDialogMedia(joining.media))
	time.Sleep(100 * time.Millisecond)

	talker.sendRTP(t, 2, 160, bytes.Repeat([]byte{0x21}, 160))
	waitHeard(t, heard, 0x21)
}

// bridgeFailingWriter fails every write with err.
type bridgeFailingWriter struct {
	err error
}

func (w bridgeFailingWriter) Write(b []byte) (int, error) {
	return 0, w.err
}

// TestBridgeMixKeepsMixingPastAFailedDialog checks that a dialog the mix can no
// longer write to does not stop the mix for the others. A dialog whose media a
// BYE has closed, before its handler leaves the bridge, is dropped from the mix;
// any other failed write loses that frame for that dialog alone.
func TestBridgeMixKeepsMixingPastAFailedDialog(t *testing.T) {
	t.Run("Poll", func(t *testing.T) { testBridgeMixKeepsMixingPastAFailedDialog(t, true) })
	t.Run("DirectRead", func(t *testing.T) { testBridgeMixKeepsMixingPastAFailedDialog(t, false) })
}

func testBridgeMixKeepsMixingPastAFailedDialog(t *testing.T, poll bool) {
	for _, tc := range []struct {
		name string
		// joined sets the dialog up to fail once it has joined
		joined func(t *testing.T, d *bridgeTestDialog)
		// writer, when set, is the dialog's audio writer from the start
		writer io.Writer
	}{
		{name: "MediaClosed", joined: func(t *testing.T, d *bridgeTestDialog) { require.NoError(t, d.conn.Close()) }},
		{name: "WriteError", writer: bridgeFailingWriter{err: errors.New("write refused")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBridgeMix()
			b.Poll = poll
			talker := newBridgeTestDialog(t, "talker", media.CodecAudioUlaw)
			failed := newBridgeTestDialog(t, "failed", media.CodecAudioUlaw)
			listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
			talker.media.audioWriter = newBridgeTestWriter()
			failed.media.audioWriter = tc.writer
			heard := newBridgeTestWriter()
			listener.media.audioWriter = heard
			// The failed dialog is written to before the listener in every round
			for _, d := range []*bridgeTestDialog{talker, failed, listener} {
				require.NoError(t, b.AddDialogMedia(d.media))
			}
			t.Cleanup(func() { stopBridgeMix(t, b) })

			if tc.joined != nil {
				tc.joined(t, failed)
			}
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
		})
	}
}

// TestBridgeMixDirectReadStopsWhileAudioFlows checks that a stop ends a mix
// that reads its streams directly, with Poll off, while every read finds a frame
// waiting. Such a mix never has a read time out on its own deadline, so it sees
// the stop only through the deadline the stop sets, which its own must never
// replace.
func TestBridgeMixDirectReadStopsWhileAudioFlows(t *testing.T) {
	b := NewBridgeMix()
	b.Poll = false
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
	a.media.audioWriter = newBridgeTestWriter()
	heard := newBridgeTestWriter()
	c.media.audioWriter = heard
	for _, d := range []*bridgeTestDialog{a, c} {
		require.NoError(t, b.AddDialogMedia(d.media))
	}

	// Both peers send four frames for every frame the mix reads, so a read
	// always finds one waiting
	feeding := make(chan struct{})
	fed := make(chan struct{})
	go func() {
		defer close(fed)
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for i := 0; ; i++ {
			select {
			case <-feeding:
				return
			case <-ticker.C:
			}
			for _, d := range []*bridgeTestDialog{a, c} {
				pkt := rtp.Packet{
					Header:  rtp.Header{Version: 2, SequenceNumber: uint16(i), Timestamp: uint32(i) * 160, SSRC: 1},
					Payload: bytes.Repeat([]byte{0x20}, 160),
				}
				data, _ := pkt.Marshal()
				_, _ = d.peer.WriteTo(data, d.conn.LocalAddr())
			}
		}
	}()
	var feedEnd sync.Once
	endFeed := func() {
		feedEnd.Do(func() { close(feeding) })
		<-fed
	}
	t.Cleanup(endFeed)

	select {
	case <-heard.frames:
	case <-time.After(2 * time.Second):
		t.Fatal("the mix wrote nothing")
	}

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		b.mu.Lock()
		defer b.mu.Unlock()
		_ = b.mixStopWait()
	}()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		// Ending the feed lets the reads drain the frames waiting and then time
		// out, which ends the mix and the stop
		endFeed()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
		}
		t.Fatal("the mix did not stop while audio was flowing")
	}
}

// rebind moves the dialog's media to a session on a new socket, as a re-INVITE
// that rebinds the media does when its session is replaced: under the dialog's
// lock the packet reader moves to the new session and the session is replaced,
// and the replaced session's socket is then closed.
func (d *bridgeTestDialog) rebind(t *testing.T) {
	t.Helper()
	rtpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { rtpConn.Close() })
	conn := &bridgeTestConn{UDPConn: rtpConn}

	d.media.mu.Lock()
	sess := &media.MediaSession{Codecs: slices.Clone(d.media.mediaSession.Codecs)}
	sess.InitWithListeners(conn, conn, d.peer.LocalAddr().(*net.UDPAddr))
	d.media.RTPPacketReader.UpdateReader(sess)
	d.media.mediaSession = sess
	d.media.mu.Unlock()

	require.NoError(t, d.conn.Close())
	d.conn = conn
}

// TestBridgeMixDirectReadFollowsRebind checks that a mix reading its streams
// directly, with Poll off, reads a dialog with a deadline on its current media
// session. Once a re-INVITE has moved a dialog's media to a new socket, a
// deadline set on the replaced session leaves the read on the new socket without
// one, and the mix waits there until that dialog sends a frame. That holds for
// a read started after the move, and for one the move caught waiting, which the
// packet reader continues on the new socket.
func TestBridgeMixDirectReadFollowsRebind(t *testing.T) {
	t.Run("BetweenReads", func(t *testing.T) { testBridgeMixDirectReadFollowsRebind(t, false) })
	t.Run("DuringRead", func(t *testing.T) { testBridgeMixDirectReadFollowsRebind(t, true) })
}

func testBridgeMixDirectReadFollowsRebind(t *testing.T, duringRead bool) {
	b := NewBridgeMix()
	b.Poll = false
	talker := newBridgeTestDialog(t, "talker", media.CodecAudioUlaw)
	moved := newBridgeTestDialog(t, "moved", media.CodecAudioUlaw)
	listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
	talker.media.audioWriter = newBridgeTestWriter()
	moved.media.audioWriter = newBridgeTestWriter()
	heard := newBridgeTestWriter()
	listener.media.audioWriter = heard

	// The first read of the moved dialog waits until the move has closed the
	// socket it reads
	reading := make(chan struct{})
	moveDone := make(chan struct{})
	if duringRead {
		var once sync.Once
		moved.conn.beforeRead = func() {
			once.Do(func() {
				close(reading)
				select {
				case <-moveDone:
				case <-time.After(5 * time.Second):
				}
			})
		}
	}
	for _, d := range []*bridgeTestDialog{talker, moved, listener} {
		require.NoError(t, b.AddDialogMedia(d.media))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	// The moved dialog stays silent
	if duringRead {
		select {
		case <-reading:
		case <-time.After(2 * time.Second):
			t.Fatal("the mix did not read the dialog")
		}
	}
	moved.rebind(t)
	close(moveDone)
	const frames = 5
	start := time.Now()
	for i := range frames {
		time.Sleep(time.Until(start.Add(time.Duration(i) * 20 * time.Millisecond)))
		talker.sendRTP(t, uint16(i), uint32(i)*160, bytes.Repeat([]byte{byte(0x20 + i)}, 160))
	}
	for i := range frames {
		waitHeard(t, heard, byte(0x20+i))
	}
}

// TestBridgeMixTakesOutRenegotiatedDialog checks what the next join or leave
// does once a re-INVITE has moved a dialog in the bridge to audio the mix can
// not mix with the others. The bridge keeps mixing at the codec of its last
// mix, the dialog that no longer matches it is taken out of the bridge, and the
// others are mixed. When every dialog has moved, the bridge follows its first
// dialog.
func TestBridgeMixTakesOutRenegotiatedDialog(t *testing.T) {
	ulaw30ms := media.CodecAudioUlaw
	ulaw30ms.SampleDur = 30 * time.Millisecond

	for _, tc := range []struct {
		name string
		// dialogs join in this order, all on 20 ms PCMU
		dialogs []string
		// moved are renegotiated to 30 ms PCMU
		moved []string
		// then either joins, on 20 ms PCMU unless every dialog moved, or leaves
		joins, leaves string
		want          []string
	}{
		{name: "Join", dialogs: []string{"a", "x"}, moved: []string{"x"}, joins: "c", want: []string{"a", "c"}},
		{name: "Leave", dialogs: []string{"a", "x", "c"}, moved: []string{"x"}, leaves: "a", want: []string{"c"}},
		{name: "FirstDialogMoved", dialogs: []string{"x", "a"}, moved: []string{"x"}, joins: "c", want: []string{"a", "c"}},
		{name: "EveryDialogMoved", dialogs: []string{"a", "x"}, moved: []string{"a", "x"}, joins: "c", want: []string{"a", "x", "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := NewBridgeMix()
			dialogs := map[string]*bridgeTestDialog{}
			for _, id := range tc.dialogs {
				dialogs[id] = newBridgeTestDialog(t, id, media.CodecAudioUlaw)
				require.NoError(t, b.AddDialogMedia(dialogs[id].media))
			}
			t.Cleanup(func() { stopBridgeMix(t, b) })

			for _, id := range tc.moved {
				dialogs[id].renegotiate(ulaw30ms)
			}
			if tc.joins != "" {
				codec := media.CodecAudioUlaw
				if len(tc.moved) == len(tc.dialogs) {
					codec = ulaw30ms
				}
				dialogs[tc.joins] = newBridgeTestDialog(t, tc.joins, codec)
				require.NoError(t, b.AddDialogMedia(dialogs[tc.joins].media), "a dialog that matches the mix must join")
			}
			if tc.leaves != "" {
				require.NoError(t, b.RemoveDialogMedia(dialogs[tc.leaves].media))
			}

			var want []*DialogMedia
			for _, id := range tc.want {
				want = append(want, dialogs[id].media)
			}
			assert.Equal(t, want, b.DialogMediaList())
			assert.Equal(t, 1, b.stateRead(), "the dialogs left must be mixed")
		})
	}
}

// TestBridgeMixJoinDuringReInvite checks, under the race detector, that joins
// and leaves read the media session of a dialog in the bridge under the
// dialog's lock, as a re-INVITE replaces that session under it.
func TestBridgeMixJoinDuringReInvite(t *testing.T) {
	b := NewBridgeMix()
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	require.NoError(t, b.AddDialogMedia(a.media))
	t.Cleanup(func() { stopBridgeMix(t, b) })

	reinviting := make(chan struct{})
	reinvited := make(chan struct{})
	go func() {
		defer close(reinvited)
		for {
			select {
			case <-reinviting:
				return
			default:
			}
			a.renegotiate(media.CodecAudioUlaw)
		}
	}()
	defer func() {
		close(reinviting)
		select {
		case <-reinvited:
		case <-time.After(5 * time.Second):
			t.Error("the re-INVITEs did not stop")
		}
	}()

	for i := range 20 {
		c := newBridgeTestDialog(t, fmt.Sprintf("c%d", i), media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogMedia(c.media))
		require.NoError(t, b.RemoveDialogMedia(c.media))
	}
}

// TestBridgeMixFollowsCodecChange checks that a re-INVITE moving a dialog in a
// running mix from PCMU to PCMA, which has the same sample rate and frame
// duration, is followed at once: the mix decodes what the dialog sends as PCMA
// and encodes what it hears as PCMA. A mix left decoding it as PCMU passes the
// PCMA bytes on to the others as they are, and sends it PCMU, garbling the audio
// both ways until the next join or leave.
func TestBridgeMixFollowsCodecChange(t *testing.T) {
	b := NewBridgeMix()
	// The peers send their frames as the test goes, not in real time
	b.RealtimeReader = false
	moved, movedPeer := newAnsweredBridgeTestDialog(t, "moved")
	listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
	movedHeard := newBridgeTestWriter()
	moved.media.audioWriter = movedHeard
	listenerHeard := newBridgeTestWriter()
	listener.media.audioWriter = listenerHeard
	for _, d := range []*bridgeTestDialog{moved, listener} {
		require.NoError(t, b.AddDialogMedia(d.media))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	// Every byte of a PCMU frame is its value, which decodes and encodes back
	// to itself
	listener.sendRTP(t, 1, 0, bytes.Repeat([]byte{0x20}, 160))
	waitHeard(t, movedHeard, 0x20)

	// The peer moves the dialog to PCMA
	moved.reInviteAlaw(t, movedPeer)

	// What the moved dialog says in PCMA the listener hears in PCMU
	alawFrame := bytes.Repeat([]byte{0x55}, 160)
	pcm := make([]byte, 2*len(alawFrame))
	_, err := audio.DecodeAlawTo(pcm, alawFrame)
	require.NoError(t, err)
	want := ulawEncode(pcm)[0]
	require.NotEqual(t, ulawEncode(ulawDecode(alawFrame))[0], want, "the frame must tell PCMA from PCMU")
	moved.sendRTPFrom(t, movedPeer, media.CodecAudioAlaw.PayloadType, 1, 0, alawFrame)
	waitHeard(t, listenerHeard, want)

	// What the listener says in PCMU the moved dialog hears in PCMA
	_, err = audio.EncodeAlawTo(alawFrame, ulawDecode(bytes.Repeat([]byte{0x21}, 160)))
	require.NoError(t, err)
	require.NotEqual(t, byte(0x21), alawFrame[0], "the frame must tell PCMA from PCMU")
	listener.sendRTP(t, 2, 160, bytes.Repeat([]byte{0x21}, 160))
	waitHeard(t, movedHeard, alawFrame[0])
}

// TestBridgeMixStreamTakesOneCodec checks that a mix stream decodes its dialog
// and encodes to it with one codec, read in one go. A re-INVITE that lands
// between reading the codec for the stream's decoder and reading it for its
// encoder otherwise leaves the stream decoding the dialog in one codec and
// encoding to it in another. The window is short, so the stream is set up
// many times while re-INVITEs move the dialog between PCMU and PCMA.
func TestBridgeMixStreamTakesOneCodec(t *testing.T) {
	b := NewBridgeMix()
	b.RealtimeReader = false
	d := newBridgeTestDialog(t, "d", media.CodecAudioUlaw)

	reinviting := make(chan struct{})
	reinvited := make(chan struct{})
	go func() {
		defer close(reinvited)
		for i := 0; ; i++ {
			select {
			case <-reinviting:
				return
			default:
			}
			if i%2 == 0 {
				d.renegotiate(media.CodecAudioAlaw)
			} else {
				d.renegotiate(media.CodecAudioUlaw)
			}
		}
	}()
	defer func() {
		close(reinviting)
		select {
		case <-reinvited:
		case <-time.After(5 * time.Second):
			t.Error("the re-INVITEs did not stop")
		}
	}()

	// A frame decoded and encoded again in one G.711 codec comes back as it
	// was, and in two it does not
	frame := bytes.Repeat([]byte{0x55}, 160)
	const joins = 2000
	mixed := 0
	for range joins {
		heard := newBridgeTestWriter()
		d.media.mu.Lock()
		d.media.audioReader = bytes.NewReader(frame)
		d.media.audioWriter = heard
		d.media.mu.Unlock()

		stream := bridgePCMStream{}
		require.NoError(t, b.addDialogStream(d.media, &stream, media.CodecAudioUlaw))
		pcm := make([]byte, media.RTPBufSize)
		n, err := stream.r.Read(pcm)
		require.NoError(t, err)
		_, err = stream.w.Write(pcm[:n])
		require.NoError(t, err)
		if !bytes.Equal(frame, <-heard.frames) {
			mixed++
		}
	}
	assert.Zero(t, mixed, "%d of %d streams decode and encode their dialog in two codecs", mixed, joins)
}

// bridgeScriptedReader answers each read with the next of its reads, and a nil
// read, or any read once they are used up, with the timeout a direct read of a
// silent dialog ends in.
type bridgeScriptedReader struct {
	reads [][]byte
}

func (r *bridgeScriptedReader) Read(b []byte) (int, error) {
	if len(r.reads) == 0 {
		return 0, os.ErrDeadlineExceeded
	}
	read := r.reads[0]
	r.reads = r.reads[1:]
	if read == nil {
		return 0, os.ErrDeadlineExceeded
	}
	return copy(b, read), nil
}

// bridgePCM returns samples 16-bit PCM samples of value v.
func bridgePCM(v int16, samples int) []byte {
	b := make([]byte, 2*samples)
	for i := 0; i < len(b); i += 2 {
		binary.LittleEndian.PutUint16(b[i:], uint16(v))
	}
	return b
}

// TestBridgeMixUnmixesShortRead checks what a stream hears in a round where it
// read less than the longest read of the round. It hears the round's mix
// without its own frame, which covers only the bytes it read. Past them it put
// nothing into the mix, so it hears the mix as it is, and never less what an
// earlier round left in its buffer: the mix it heard then.
func TestBridgeMixUnmixesShortRead(t *testing.T) {
	b := NewBridgeMix()
	b.Poll = false
	heard := map[string]*bridgeTestWriter{}
	newStream := func(id string, reads ...[]byte) *bridgePCMStream {
		heard[id] = newBridgeTestWriter()
		return &bridgePCMStream{
			r:     &bridgeScriptedReader{reads: reads},
			w:     heard[id],
			media: newBridgeTestDialog(t, id, media.CodecAudioUlaw).media,
			buf:   make([]byte, media.RTPBufSize),
		}
	}
	// Both streams read a whole frame in the first round. In the second the
	// short stream reads one of half that length.
	streams := []*bridgePCMStream{
		newStream("short", bridgePCM(1000, 80), bridgePCM(2000, 40)),
		newStream("talker", bridgePCM(500, 80), bridgePCM(300, 80)),
		newStream("listener"),
	}
	nextHeard := func(id string) []byte {
		t.Helper()
		select {
		case frame := <-heard[id].frames:
			return frame
		case <-time.After(2 * time.Second):
			t.Fatalf("%s heard nothing", id)
			return nil
		}
	}

	b.mu.Lock()
	b.stateWriteUnsafe(1)
	b.mu.Unlock()
	mixed := make(chan error, 1)
	go func() { mixed <- b.mixLoop(streams, false, 20*time.Millisecond) }()
	defer func() {
		b.mu.Lock()
		b.stateWriteUnsafe(2)
		b.mu.Unlock()
		select {
		case <-mixed:
		case <-time.After(5 * time.Second):
			t.Error("the mix did not stop")
		}
	}()

	assert.Equal(t, bridgePCM(500, 80), nextHeard("short"), "the stream must hear the talker")
	assert.Equal(t, bridgePCM(300, 80), nextHeard("short"), "the stream must hear the talker, and nothing of an earlier round")
}

// TestBridgeMixRefusesDuplicateDialog checks that a dialog already in the bridge
// is refused a second join with an error, and that the bridge keeps mixing the
// dialogs it has. A dialog in it twice was read by two streams at once.
func TestBridgeMixRefusesDuplicateDialog(t *testing.T) {
	b := NewBridgeMix()
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
	for _, d := range []*bridgeTestDialog{a, c} {
		require.NoError(t, b.AddDialogMedia(d.media))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	require.Error(t, b.AddDialogMedia(a.media), "a dialog in the bridge joined again")
	assert.Equal(t, []*DialogMedia{a.media, c.media}, b.DialogMediaList())
	assert.Equal(t, 1, b.stateRead(), "the dialogs in the bridge must keep being mixed")

	require.NoError(t, b.RemoveDialogMedia(a.media))
	assert.Equal(t, []*DialogMedia{c.media}, b.DialogMediaList())
}

// TestBridgeMixRealtimeReaderOnDialogByDefault checks that, without
// RealtimeReaderPerMix, the realtime reader a mix reads a dialog through is
// set on the dialog as its audio reader, is still there once the dialog has
// left, and is the one the next mix reads through.
func TestBridgeMixRealtimeReaderOnDialogByDefault(t *testing.T) {
	b := NewBridgeMix()
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
	for _, d := range []*bridgeTestDialog{a, c} {
		require.NoError(t, b.AddDialogMedia(d.media))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	joined, err := a.media.AudioReader()
	require.NoError(t, err)
	require.IsType(t, &media.RTPRealTimeReader{}, joined)

	require.NoError(t, b.RemoveDialogMedia(a.media))
	left, err := a.media.AudioReader()
	require.NoError(t, err)
	assert.Same(t, joined, left, "the dialog must leave with the realtime reader")

	require.NoError(t, b.AddDialogMedia(a.media))
	again, err := a.media.AudioReader()
	require.NoError(t, err)
	assert.Same(t, joined, again, "the next mix must read through the reader on the dialog")
}

// TestBridgeMixRefusesDialogWithoutMedia checks that BridgeMix refuses media
// that has no media session with an error, whether it joins first or after
// another, and keeps the dialogs it has. The mix reads a joining dialog's
// codec from its media session.
func TestBridgeMixRefusesDialogWithoutMedia(t *testing.T) {
	for _, first := range []bool{true, false} {
		t.Run(fmt.Sprintf("First=%t", first), func(t *testing.T) {
			b := NewBridgeMix()
			a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
			var want []*DialogMedia
			if !first {
				require.NoError(t, b.AddDialogMedia(a.media))
				want = []*DialogMedia{a.media}
			}
			t.Cleanup(func() { stopBridgeMix(t, b) })
			noMedia := newBridgeTestDialog(t, "nomedia", media.CodecAudioUlaw)
			noMedia.media.mediaSession = nil
			var err error
			require.NotPanics(t, func() { err = b.AddDialogMedia(noMedia.media) })
			require.ErrorIs(t, err, ErrNoMediaSetup)
			assert.Equal(t, want, b.DialogMediaList())
			if !first {
				assert.Equal(t, 1, b.stateRead(), "the dialog in the bridge must keep being mixed")
			}
		})
	}
}
