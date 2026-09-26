// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
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

	incoming := &DialogServerSession{
		DialogMedia: DialogMedia{
			mediaSession: &media.MediaSession{
				Codecs: []media.Codec{media.CodecAudioAlaw},
			},
			audioReader:     bytes.NewBuffer(make([]byte, 9999)),
			audioWriter:     bytes.NewBuffer(make([]byte, 0)),
			RTPPacketReader: media.NewRTPPacketReader(nil, media.CodecAudioAlaw),
			RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioAlaw),
		},
	}
	outgoing := &DialogClientSession{
		DialogMedia: DialogMedia{
			mediaSession: &media.MediaSession{
				Codecs: []media.Codec{media.CodecAudioAlaw},
			},
			audioReader:     bytes.NewBuffer(make([]byte, 9999)),
			audioWriter:     bytes.NewBuffer(make([]byte, 0)),
			RTPPacketReader: media.NewRTPPacketReader(nil, media.CodecAudioAlaw),
			RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioAlaw),
		},
	}

	err := b.AddDialogSession(incoming)
	require.NoError(t, err)
	err = b.AddDialogSession(outgoing)
	require.NoError(t, err)

	err = b.proxyMedia()
	require.ErrorIs(t, err, io.EOF)

	// Confirm all data is proxied
	assert.Equal(t, 9999, incoming.audioWriter.(*bytes.Buffer).Len())
	assert.Equal(t, 9999, outgoing.audioWriter.(*bytes.Buffer).Len())
}

func TestBridgeNoTranscodingAllowed(t *testing.T) {
	b := NewBridge()
	// b.waitDialogsNum = 99 // Do not start proxy

	incoming := &DialogServerSession{
		DialogMedia: DialogMedia{
			mediaSession: &media.MediaSession{
				Codecs: []media.Codec{media.CodecAudioAlaw},
			},
			// RTPPacketReader: media.NewRTPPacketReader(nil, media.CodecAudioAlaw),
			// RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioAlaw),
		},
	}
	outgoing := &DialogClientSession{
		DialogMedia: DialogMedia{
			mediaSession: &media.MediaSession{
				Codecs: []media.Codec{media.CodecAudioUlaw},
			},
			// RTPPacketReader: media.NewRTPPacketReader(nil, media.CodecAudioUlaw),
			// RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioUlaw),
		},
	}

	err := b.AddDialogSession(incoming)
	require.NoError(t, err)
	err = b.AddDialogSession(outgoing)
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

	err := tu.ServeBackground(ctx, func(in *DialogServerSession) {
		in.Trying()
		in.Ringing()
		in.Answer()

		inCtx := in.Context()
		ctx, cancel := context.WithTimeout(inCtx, 15*time.Second)
		defer cancel()

		// Wa want to bridge this call with originator
		bridge := NewBridge()
		// Add us in bridge
		if err := bridge.AddDialogSession(in); err != nil {
			t.Log("Adding dialog in bridge failed", err)
			return
		}

		out, err := tu.InviteBridge(ctx, sip.Uri{User: "test", Host: "127.0.0.200", Port: 5090}, &bridge, InviteOptions{})
		if err != nil {
			t.Log("Dialing failed", err)
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

		err := dg.ServeBackground(context.Background(), func(d *DialogServerSession) {
			ctx := d.Context()
			if err := d.Answer(); err != nil {
				echoed <- fmt.Errorf("answer: %w", err)
				return
			}

			// ms := d.mediaSession
			buf := make([]byte, media.RTPBufSize)
			r, _ := d.AudioReader()
			n, err := r.Read(buf)
			if err != nil {
				echoed <- fmt.Errorf("read: %w", err)
				return
			}

			w, _ := d.AudioWriter()
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
		dialog, err := dg.Invite(context.TODO(), sip.Uri{Host: "127.0.0.1", Port: 5090}, InviteOptions{})
		require.NoError(t, err)
		defer dialog.Close()

		w, _ := dialog.AudioWriter()
		_, err = w.Write([]byte("1234"))
		require.NoError(t, err)

		buf := make([]byte, media.RTPBufSize)
		r, _ := dialog.AudioReader()
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

// bridgeTestDialog is a confirmed dialog for BridgeMix tests. Its media reads
// RTP on a real socket, which the test sends to from peer.
type bridgeTestDialog struct {
	id        string
	media     *DialogMedia
	sipDialog *sipgo.Dialog
	conn      *bridgeTestConn
	peer      *net.UDPConn
}

var _ DialogSession = (*bridgeTestDialog)(nil)

func newBridgeTestDialog(t *testing.T, id string, codec media.Codec) *bridgeTestDialog {
	t.Helper()
	loopback := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	rtpConn, err := net.ListenUDP("udp", loopback)
	require.NoError(t, err)
	peer, err := net.ListenUDP("udp", loopback)
	require.NoError(t, err)
	t.Cleanup(func() {
		rtpConn.Close()
		peer.Close()
	})

	conn := &bridgeTestConn{UDPConn: rtpConn}
	sess := &media.MediaSession{Codecs: []media.Codec{codec}}
	sess.InitWithListeners(conn, conn, peer.LocalAddr().(*net.UDPAddr))

	invite := sip.NewRequest(sip.INVITE, sip.Uri{User: id, Host: "127.0.0.1", Port: 5060})
	sipDialog := &sipgo.Dialog{ID: id, InviteRequest: invite}
	sipDialog.InitWithState(sip.DialogStateConfirmed)

	return &bridgeTestDialog{
		id: id,
		media: &DialogMedia{
			mediaSession:    sess,
			RTPPacketReader: media.NewRTPPacketReader(sess, codec),
			RTPPacketWriter: media.NewRTPPacketWriter(sess, codec),
		},
		sipDialog: sipDialog,
		conn:      conn,
		peer:      peer,
	}
}

func (d *bridgeTestDialog) Id() string                       { return d.id }
func (d *bridgeTestDialog) Context() context.Context         { return context.Background() }
func (d *bridgeTestDialog) Hangup(ctx context.Context) error { return nil }
func (d *bridgeTestDialog) Media() *DialogMedia              { return d.media }
func (d *bridgeTestDialog) DialogSIP() *sipgo.Dialog         { return d.sipDialog }
func (d *bridgeTestDialog) Close() error                     { return nil }

func (d *bridgeTestDialog) Do(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	return nil, errors.New("bridge test dialog sends no requests")
}

// sendRTP sends the dialog one RTP packet from its peer.
func (d *bridgeTestDialog) sendRTP(t *testing.T, seq uint16, ts uint32, payload []byte) {
	t.Helper()
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, SequenceNumber: seq, Timestamp: ts, SSRC: 1},
		Payload: payload,
	}
	data, err := pkt.Marshal()
	require.NoError(t, err)
	_, err = d.peer.WriteTo(data, d.conn.LocalAddr())
	require.NoError(t, err)
}

// bridgeTestConn is a dialog's RTP socket. It counts the reads in progress on
// it. With deadlineErr set, every read deadline it is given is applied and then
// reported as failed. With beforeRead set, every read calls it first.
type bridgeTestConn struct {
	*net.UDPConn
	deadlineErr error
	reading     atomic.Int32
	beforeRead  func()
}

func (c *bridgeTestConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.reading.Add(1)
	defer c.reading.Add(-1)
	if c.beforeRead != nil {
		c.beforeRead()
	}
	return c.UDPConn.ReadFrom(b)
}

func (c *bridgeTestConn) SetReadDeadline(t time.Time) error {
	if err := c.UDPConn.SetReadDeadline(t); err != nil {
		return err
	}
	return c.deadlineErr
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
			require.NoError(t, b.AddDialogSession(d))
		}
		t.Cleanup(func() { stopBridgeMix(t, b) })

		// A BYE closes the dialog's media before its handler leaves the bridge
		require.NoError(t, hungUp.conn.Close())
		require.NoError(t, b.RemoveDialogSession(hungUp))
		assert.Equal(t, []DialogSession{a, c}, b.DialogSessionsList())
		assert.Equal(t, 1, b.stateRead(), "the dialogs left must be mixed")
	})

	t.Run("JoinRefused", func(t *testing.T) {
		b := NewBridgeMix()
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		failing := newBridgeTestDialog(t, "failing", media.CodecAudioUlaw)
		failing.conn.deadlineErr = errDeadline
		for _, d := range []*bridgeTestDialog{a, failing} {
			require.NoError(t, b.AddDialogSession(d))
		}
		t.Cleanup(func() { stopBridgeMix(t, b) })

		err := b.AddDialogSession(newBridgeTestDialog(t, "c", media.CodecAudioUlaw))
		require.ErrorIs(t, err, errDeadline)
		assert.Equal(t, []DialogSession{a, failing}, b.DialogSessionsList(), "a refused join must not add the dialog")
		assert.Equal(t, 1, b.stateRead(), "the dialogs in the bridge must keep being mixed")
	})

	t.Run("LeaveReportsError", func(t *testing.T) {
		b := NewBridgeMix()
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
		failing := newBridgeTestDialog(t, "failing", media.CodecAudioUlaw)
		failing.conn.deadlineErr = errDeadline
		for _, d := range []*bridgeTestDialog{a, c, failing} {
			require.NoError(t, b.AddDialogSession(d))
		}
		t.Cleanup(func() { stopBridgeMix(t, b) })

		err := b.RemoveDialogSession(a)
		require.ErrorIs(t, err, errDeadline)
		assert.Equal(t, []DialogSession{c, failing}, b.DialogSessionsList(), "a leave must take the dialog out")
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
	b.dialogs = []DialogSession{a, c}
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
	go func() { removed <- b.RemoveDialogSession(a) }()
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
	assert.Equal(t, []DialogSession{c}, b.DialogSessionsList())
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
		require.NoError(t, b.AddDialogSession(d))
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

	require.NoError(t, b.RemoveDialogSession(talker))
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
			require.NoError(t, b.AddDialogSession(a))
			t.Cleanup(func() { stopBridgeMix(t, b) })

			err := b.AddDialogSession(newBridgeTestDialog(t, "mismatch", tc.codec))
			require.ErrorContains(t, err, "Resampling or transcoding is not supported")
			assert.Equal(t, []DialogSession{a}, b.DialogSessionsList(), "a refused join must not add the dialog")
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
				require.NoError(t, b.AddDialogSession(d))
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

// TestBridgeMixLeaveKeepsDialogReader checks that a dialog leaves the bridge
// with the audio reader it joined with. The mix reads a dialog through a
// realtime reader, which drops a frame read more than two frame durations after
// its place in the stream the reader first read. Left on the dialog, it drops
// the frames of an application that reads at its own pace after the leave.
func TestBridgeMixLeaveKeepsDialogReader(t *testing.T) {
	b := NewBridgeMix()
	talker := newBridgeTestDialog(t, "talker", media.CodecAudioUlaw)
	listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
	talker.media.audioWriter = newBridgeTestWriter()
	heard := newBridgeTestWriter()
	listener.media.audioWriter = heard
	joinedWith, err := talker.media.AudioReader()
	require.NoError(t, err)
	for _, d := range []*bridgeTestDialog{talker, listener} {
		require.NoError(t, b.AddDialogSession(d))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	// Every byte of a frame is its value, which decodes and encodes back to
	// itself
	talker.sendRTP(t, 1, 0, bytes.Repeat([]byte{0x20}, 160))
	waitHeard(t, heard, 0x20)
	require.NoError(t, b.RemoveDialogSession(talker))

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

// TestBridgeMixRealtimeReaderPerMix checks that a mix judges a dialog's frames
// late only against the frames it has read itself. A frame that the previous
// mix would have dropped as late, because the peer paused and resumed its RTP
// clock where it left off, is heard in the mix that replaced it.
func TestBridgeMixRealtimeReaderPerMix(t *testing.T) {
	b := NewBridgeMix()
	talker := newBridgeTestDialog(t, "talker", media.CodecAudioUlaw)
	listener := newBridgeTestDialog(t, "listener", media.CodecAudioUlaw)
	talker.media.audioWriter = newBridgeTestWriter()
	heard := newBridgeTestWriter()
	listener.media.audioWriter = heard
	for _, d := range []*bridgeTestDialog{talker, listener} {
		require.NoError(t, b.AddDialogSession(d))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	talker.sendRTP(t, 1, 0, bytes.Repeat([]byte{0x20}, 160))
	waitHeard(t, heard, 0x20)

	// A join replaces the mix while the talker pauses
	joining := newBridgeTestDialog(t, "joining", media.CodecAudioUlaw)
	joining.media.audioWriter = newBridgeTestWriter()
	require.NoError(t, b.AddDialogSession(joining))
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
				require.NoError(t, b.AddDialogSession(d))
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
		require.NoError(t, b.AddDialogSession(d))
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
// that rebinds the media does in replaceRTPSessionUnsafe: under the dialog's
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
		require.NoError(t, b.AddDialogSession(d))
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
	dialogExit := make(chan string, 10)
	err := tu.ServeBackground(ctx, func(in *DialogServerSession) {
		defer func() { dialogExit <- in.ID }()
		bridge := currentBridge.Load()

		in.Trying()
		in.Ringing()
		in.Answer()

		// Add us in bridge
		t.Log("Adding into bridge", in.ID)
		if err := bridge.AddDialogSession(in); err != nil {
			t.Log("Adding dialog in bridge failed", err)
			return
		}
		defer func() {
			t.Log("Removing from bridge", in.ID)
			bridge.RemoveDialogSession(in)
		}()

		<-in.Context().Done()
	})
	assert.NoError(t, err)

	t.Run("BridgeAddRemove", func(t *testing.T) {
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := newDialer(ua)

		dialogs := make([]*DialogClientSession, 3)
		for i := range 3 {
			t.Log("Inviting", "i", i)
			dialog, err := dg.Invite(context.TODO(), sip.Uri{Host: "127.0.0.1", Port: 5090}, InviteOptions{})
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
			<-dialogExit
		}
		bridge := currentBridge.Load()
		assert.Equal(t, 0, len(bridge.dialogs))
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
		defer func() {
			for range 2 {
				<-dialogExit
			}
		}()

		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := newDialer(ua)

		// Make number of calls that will have audio mixed in bridge
		// wg := sync.WaitGroup{}
		bridge := NewBridgeMix()
		bridge.WaitDialogsNum = 2 // Do not start mixing until both dialogs get joined, otherwise there will be no guarantee when something is mixed
		currentBridge.Store(bridge)

		dialog1, err := dg.Invite(context.TODO(), sip.Uri{Host: "127.0.0.1", Port: 5090}, InviteOptions{})
		require.NoError(t, err)
		defer dialog1.Hangup(dialog1.Context())

		dialog2, err := dg.Invite(context.TODO(), sip.Uri{Host: "127.0.0.1", Port: 5090}, InviteOptions{})
		require.NoError(t, err)
		defer dialog2.Hangup(dialog2.Context())

		// Write sound on dialog 1 and make sure it is read on dialog2
		sound := []byte("123450")
		go func(dialog *DialogClientSession) {
			w, _ := dialog.AudioWriter()
			for i := 0; i < 10; i++ {
				w.Write(sound)
			}

		}(dialog1)

		r, _ := dialog2.AudioReader()
		dialog2.StopRTP(1, 1*time.Second)

		buf := make([]byte, media.RTPBufSize)
		for i := 0; i < 10; i++ {
			n, err := r.Read(buf)
			require.NoError(t, err)
			assert.Equal(t, sound, buf[:n])
			t.Log("Sound received", "buf", buf[:n])
		}

	})
}
