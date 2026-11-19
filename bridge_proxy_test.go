// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// bridgeTestDialog is the media of an answered call for bridge tests. It
// reads RTP on a real socket, which the test sends to from peer.
type bridgeTestDialog struct {
	id    string
	media *DialogMedia
	conn  *bridgeTestConn
	peer  *net.UDPConn
}

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

	return &bridgeTestDialog{
		id: id,
		media: &DialogMedia{
			mediaSession:    sess,
			RTPPacketReader: media.NewRTPPacketReader(sess, codec),
			RTPPacketWriter: media.NewRTPPacketWriter(sess, codec),
		},
		conn: conn,
		peer: peer,
	}
}

// hangUp ends the dialog's media as the end of its call does: its socket is
// closed.
func (d *bridgeTestDialog) hangUp(t *testing.T) {
	t.Helper()
	require.NoError(t, d.conn.Close())
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

// recvRTP waits, bounded, for the next RTP packet the dialog sends its peer and
// returns its payload.
func (d *bridgeTestDialog) recvRTP(t *testing.T) []byte {
	t.Helper()
	require.NoError(t, d.peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, media.RTPBufSize)
	n, _, err := d.peer.ReadFrom(buf)
	require.NoError(t, err, "the dialog sent its peer nothing")
	pkt := rtp.Packet{}
	require.NoError(t, pkt.Unmarshal(buf[:n]))
	return pkt.Payload
}

// readFrame reads the dialog's next frame through its audio reader and fails
// the test unless it is frame.
func (d *bridgeTestDialog) readFrame(t *testing.T, frame []byte) {
	t.Helper()
	r, err := d.media.AudioReader()
	require.NoError(t, err)
	require.NoError(t, d.conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, media.RTPBufSize)
	n, err := r.Read(buf)
	require.NoError(t, err, "%s did not get its frame", d.id)
	assert.Equal(t, frame, buf[:n])
}

// sendDTMF sends the dialog one RFC 4733 telephone-event packet from its peer.
func (d *bridgeTestDialog) sendDTMF(t *testing.T, seq uint16, ts uint32, marker bool, ev media.DTMFEvent) {
	t.Helper()
	pkt := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    media.CodecTelephoneEvent8000.PayloadType,
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           1,
		},
		Payload: media.DTMFEncode(ev),
	}
	data, err := pkt.Marshal()
	require.NoError(t, err)
	_, err = d.peer.WriteTo(data, d.conn.LocalAddr())
	require.NoError(t, err)
}

// renegotiate moves the dialog's audio to codec, as a re-INVITE that settles on
// another codec does: under the media's lock its media session is replaced by
// a fork of it on that codec.
func (d *bridgeTestDialog) renegotiate(codec media.Codec) {
	d.media.mu.Lock()
	defer d.media.mu.Unlock()
	sess := d.media.mediaSession.Fork()
	sess.Codecs = []media.Codec{codec}
	d.media.mediaSession = sess
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

// SetDeadline sets both deadlines, the read one as SetReadDeadline does.
func (c *bridgeTestConn) SetDeadline(t time.Time) error {
	return errors.Join(c.SetReadDeadline(t), c.UDPConn.SetWriteDeadline(t))
}

// newAnsweredBridgeTestDialog returns media that answered, over loopback and
// the way an answer sets it up, an offer of PCMU from peer. The media can also
// take PCMA, and follows a re-INVITE as the media of a call does.
func newAnsweredBridgeTestDialog(t *testing.T, id string) (d *bridgeTestDialog, peer *net.UDPConn) {
	t.Helper()
	loopback := net.IPv4(127, 0, 0, 1)
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback})
	require.NoError(t, err)
	t.Cleanup(func() { peer.Close() })

	sess := &media.MediaSession{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		Laddr:  net.UDPAddr{IP: loopback},
		Mode:   sdp.ModeSendrecv,
	}
	require.NoError(t, sess.Init())
	offer := sdp.GenerateForAudio(loopback, loopback, peer.LocalAddr().(*net.UDPAddr).Port, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ULAW})
	require.NoError(t, sess.RemoteSDP(offer, false))
	return &bridgeTestDialog{id: id, media: newICEDialogMedia(t, sess)}, peer
}

// sendRTPFrom sends the dialog's media one RTP packet of payload type pt from
// peer.
func (d *bridgeTestDialog) sendRTPFrom(t *testing.T, peer *net.UDPConn, pt uint8, seq uint16, ts uint32, payload []byte) {
	t.Helper()
	pkt := rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: pt, SequenceNumber: seq, Timestamp: ts, SSRC: 1},
		Payload: payload,
	}
	data, err := pkt.Marshal()
	require.NoError(t, err)
	laddr := d.media.MediaSession().Laddr
	_, err = peer.WriteTo(data, &laddr)
	require.NoError(t, err)
}

// reInviteAlaw has the dialog's peer move it to PCMA with a re-INVITE, which
// the media takes as its session hands it one: the offer is applied, answered
// and committed.
func (d *bridgeTestDialog) reInviteAlaw(t *testing.T, peer *net.UDPConn) {
	t.Helper()
	offer := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1),
		peer.LocalAddr().(*net.UDPAddr).Port, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW})
	ctx := t.Context()
	require.NoError(t, d.media.onRemoteSDP(ctx, offer, false))
	_, err := d.media.onLocalSDP(ctx, true, "")
	require.NoError(t, err)
	require.NoError(t, d.media.onFinalize(ctx))
	p := MediaProps{}
	d.media.audioWriterProps(&p)
	require.Equal(t, media.CodecAudioAlaw, p.Codec)
}

// bridgedMedias returns the dialog medias of the parties in the bridge, in
// the order they joined, and nil for an empty bridge.
func bridgedMedias(b *Bridge) []*DialogMedia {
	var medias []*DialogMedia
	for _, m := range b.GetDialogs() {
		var dm *DialogMedia
		if d, ok := m.source.dialog.(*DialogMedia); ok {
			dm = d
		}
		medias = append(medias, dm)
	}
	return medias
}

// TestBridgeProxyMediaProxiesEveryFrame checks that ProxyMedia, which starts
// the proxy by hand, passes on every frame each way until the media ends, and
// not only the first.
func TestBridgeProxyMediaProxiesEveryFrame(t *testing.T) {
	for _, dtmfPass := range []bool{false, true} {
		t.Run(fmt.Sprintf("DTMFpass=%t", dtmfPass), func(t *testing.T) {
			b := NewBridge()
			b.WaitDialogsNum = 3 // The proxy is started by hand
			b.DTMFpass = dtmfPass
			a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
			c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
			require.NoError(t, b.AddDialogMedia(a.media))
			require.NoError(t, b.AddDialogMedia(c.media))

			proxied := make(chan error, 1)
			go func() { proxied <- b.ProxyMedia(t.Context()) }()

			for i := range 3 {
				frame := bytes.Repeat([]byte{byte(0x20 + i)}, 160)
				a.sendRTP(t, uint16(i), uint32(i)*160, frame)
				assert.Equal(t, frame, c.recvRTP(t), "frame %d from a", i)
				c.sendRTP(t, uint16(i), uint32(i)*160, frame)
				assert.Equal(t, frame, a.recvRTP(t), "frame %d from c", i)
			}

			// Ending the media ends the proxy
			require.NoError(t, a.conn.Close())
			require.NoError(t, c.conn.Close())
			select {
			case <-proxied:
			case <-time.After(5 * time.Second):
				t.Fatal("the proxy did not end with the media")
			}
		})
	}
}

// TestBridgeProxyMediaEndsWithContext checks that the end of its context
// stops a proxy started by hand while both parties are silent, without an
// error, and leaves their reads usable.
func TestBridgeProxyMediaEndsWithContext(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 3 // The proxy is started by hand
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
	require.NoError(t, b.AddDialogMedia(a.media))
	require.NoError(t, b.AddDialogMedia(c.media))

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	proxied := make(chan error, 1)
	go func() { proxied <- b.ProxyMedia(ctx) }()
	frame := bytes.Repeat([]byte{0x20}, 160)
	a.sendRTP(t, 1, 0, frame)
	require.Equal(t, frame, c.recvRTP(t), "the proxy does not run")

	// Both dialogs are silent
	cancel()
	select {
	case err := <-proxied:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("the end of its context did not end the proxy")
	}
	next := bytes.Repeat([]byte{0x21}, 160)
	for _, d := range []*bridgeTestDialog{a, c} {
		d.sendRTP(t, 2, 160, next)
		d.readFrame(t, next)
	}

	t.Run("TooFewDialogs", func(t *testing.T) {
		b := NewBridge()
		b.WaitDialogsNum = 3
		require.NoError(t, b.AddDialogMedia(newBridgeTestDialog(t, "a", media.CodecAudioUlaw).media))
		var err error
		require.NotPanics(t, func() { err = b.ProxyMedia(t.Context()) })
		require.Error(t, err)
	})
}

// proxyMediaControl calls ProxyMediaControl and waits, bounded, for it to return.
func proxyMediaControl(t *testing.T, b *Bridge) (func() error, error) {
	t.Helper()
	type control struct {
		stop func() error
		err  error
	}
	started := make(chan control, 1)
	go func() {
		stop, err := b.ProxyMediaControl(t.Context())
		started <- control{stop, err}
	}()
	select {
	case ctl := <-started:
		return ctl.stop, ctl.err
	case <-time.After(2 * time.Second):
		t.Fatal("ProxyMediaControl did not return while its proxy runs")
		return nil, nil
	}
}

// TestBridgeProxyMediaControl checks that ProxyMediaControl returns once its
// proxy runs in the background, and that the stop it returns ends the proxy
// while both dialogs are silent and leaves their reads usable. It is refused
// with fewer than two dialogs, and with the proxy the bridge starts itself
// already running, which its stop would not stop.
func TestBridgeProxyMediaControl(t *testing.T) {
	for _, dtmfPass := range []bool{false, true} {
		t.Run(fmt.Sprintf("DTMFpass=%t", dtmfPass), func(t *testing.T) {
			b := NewBridge()
			b.WaitDialogsNum = 3 // The proxy is started by hand
			b.DTMFpass = dtmfPass
			a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
			c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
			require.NoError(t, b.AddDialogMedia(a.media))
			require.NoError(t, b.AddDialogMedia(c.media))

			stop, err := proxyMediaControl(t, &b)
			require.NoError(t, err)

			frame := bytes.Repeat([]byte{0x20}, 160)
			a.sendRTP(t, 1, 0, frame)
			require.Equal(t, frame, c.recvRTP(t), "the proxy does not run")

			// Both dialogs are silent
			stopped := make(chan error, 1)
			go func() { stopped <- stop() }()
			select {
			case err := <-stopped:
				require.NoError(t, err)
			case <-time.After(2 * time.Second):
				t.Fatal("the stop did not end the proxy")
			}
			// A stop made again returns what the proxy ended with
			require.NoError(t, stop())

			// The dialog's next frame goes to its own reader, which the stop
			// left no deadline on
			a.sendRTP(t, 2, 160, frame)
			r, err := a.media.AudioReader()
			require.NoError(t, err)
			read := make(chan error, 1)
			go func() {
				buf := make([]byte, media.RTPBufSize)
				n, err := r.Read(buf)
				if err == nil && !bytes.Equal(frame, buf[:n]) {
					err = fmt.Errorf("read %x", buf[:n])
				}
				read <- err
			}()
			select {
			case err := <-read:
				require.NoError(t, err, "the dialog's reads must work once the proxy has stopped")
			case <-time.After(2 * time.Second):
				t.Fatal("the dialog's reader did not get its frame")
			}
		})
	}

	t.Run("TooFewDialogs", func(t *testing.T) {
		b := NewBridge()
		b.WaitDialogsNum = 3
		require.NoError(t, b.AddDialogMedia(newBridgeTestDialog(t, "a", media.CodecAudioUlaw).media))
		_, err := proxyMediaControl(t, &b)
		require.Error(t, err)
	})

	t.Run("ProxyAlreadyRunning", func(t *testing.T) {
		b := NewBridge()
		require.NoError(t, b.AddDialogMedia(newBridgeTestDialog(t, "a", media.CodecAudioUlaw).media))
		require.NoError(t, b.AddDialogMedia(newBridgeTestDialog(t, "c", media.CodecAudioUlaw).media))
		t.Cleanup(func() { _ = stopProxyMedia(t, &b) })
		_, err := proxyMediaControl(t, &b)
		require.Error(t, err)
	})
}

// TestBridgeConcurrentUse checks, under the race detector, that a Bridge can be
// used from several goroutines at once: medias joining it while its parties are
// read, and while its proxy is started by hand.
func TestBridgeConcurrentUse(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 3 // The proxy is started by hand
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)

	deadline := time.Now().Add(5 * time.Second)
	done := make(chan error, 3)
	go func() {
		var err error
		for _, d := range []*bridgeTestDialog{a, c} {
			err = errors.Join(err, b.AddDialogMedia(d.media))
		}
		done <- err
	}()
	go func() {
		for len(b.GetDialogs()) < 2 {
			if time.Now().After(deadline) {
				done <- errors.New("the dialogs did not join")
				return
			}
			runtime.Gosched()
		}
		done <- nil
	}()
	go func() {
		for {
			stop, err := b.ProxyMediaControl(t.Context())
			if err == nil {
				done <- stop()
				return
			}
			if time.Now().After(deadline) {
				done <- err
				return
			}
			runtime.Gosched()
		}
	}()
	for range 3 {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the bridge's users did not finish")
		}
	}
	assert.Equal(t, []*DialogMedia{a.media, c.media}, bridgedMedias(&b))
}

// bridgeErrorLog is a log handler that keeps the message of every record at
// Error or above.
type bridgeErrorLog struct {
	mu       sync.Mutex
	messages []string
}

func (h *bridgeErrorLog) Enabled(_ context.Context, level slog.Level) bool {
	return level >= slog.LevelError
}

func (h *bridgeErrorLog) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.messages = append(h.messages, r.Message)
	return nil
}

func (h *bridgeErrorLog) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *bridgeErrorLog) WithGroup(string) slog.Handler      { return h }

func (h *bridgeErrorLog) logged() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.messages)
}

// stopProxyMedia calls StopProxyMedia and waits, bounded, for it to return.
func stopProxyMedia(t *testing.T, b *Bridge) error {
	t.Helper()
	stopped := make(chan error, 1)
	go func() { stopped <- b.StopProxyMedia() }()
	select {
	case err := <-stopped:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("StopProxyMedia did not return")
		return nil
	}
}

// TestBridgeProxyStopsWhenADialogEnds checks that the proxy the bridge starts
// stops once the media of either party is closed, as the end of its call
// closes it, and hands the other party back: nothing of the proxy is left
// reading it, StopProxyMedia returns once the proxy has stopped, and the
// party's next frame reaches its own reader. A proxy left reading the other
// party took that frame and failed to write it to the one that ended. A hangup
// is not an error, so nothing is logged at Error.
func TestBridgeProxyStopsWhenADialogEnds(t *testing.T) {
	for _, dtmfPass := range []bool{false, true} {
		for _, ending := range []string{"a", "c"} {
			t.Run(fmt.Sprintf("DTMFpass=%t/%sEnds", dtmfPass, ending), func(t *testing.T) {
				errorLog := &bridgeErrorLog{}
				b := Bridge{DTMFpass: dtmfPass}
				b.Init(slog.New(errorLog))
				a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
				c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
				require.NoError(t, b.AddDialogMedia(a.media))
				require.NoError(t, b.AddDialogMedia(c.media))
				frame := bytes.Repeat([]byte{0x20}, 160)
				a.sendRTP(t, 1, 0, frame)
				require.Equal(t, frame, c.recvRTP(t), "the proxy does not run")

				ended, left := a, c
				if ending == "c" {
					ended, left = c, a
				}
				require.Eventually(t, func() bool { return left.conn.reading.Load() == 1 }, 2*time.Second, time.Millisecond,
					"the proxy is not reading the dialog")
				ended.hangUp(t)
				require.Eventually(t, func() bool { return left.conn.reading.Load() == 0 }, 2*time.Second, time.Millisecond,
					"the proxy is still reading the dialog left")

				err := stopProxyMedia(t, &b)
				assert.True(t, proxyEndedWithMedia(err), "a hangup ended the proxy with %v", err)
				next := bytes.Repeat([]byte{0x21}, 160)
				left.sendRTP(t, 2, 160, next)
				left.readFrame(t, next)
				assert.Empty(t, errorLog.logged(), "a hangup was logged as an error")
			})
		}
	}

	t.Run("StopWhileBothGoOn", func(t *testing.T) {
		b := NewBridge()
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogMedia(a.media))
		require.NoError(t, b.AddDialogMedia(c.media))
		frame := bytes.Repeat([]byte{0x20}, 160)
		a.sendRTP(t, 1, 0, frame)
		require.Equal(t, frame, c.recvRTP(t), "the proxy does not run")

		// Both dialogs are silent
		require.NoError(t, stopProxyMedia(t, &b))
		next := bytes.Repeat([]byte{0x21}, 160)
		for _, d := range []*bridgeTestDialog{a, c} {
			d.sendRTP(t, 2, 160, next)
			d.readFrame(t, next)
		}
	})

	t.Run("NoProxy", func(t *testing.T) {
		b := NewBridge()
		require.NoError(t, b.AddDialogMedia(newBridgeTestDialog(t, "a", media.CodecAudioUlaw).media))
		require.NoError(t, stopProxyMedia(t, &b))
	})
}

// TestBridgeRefusedDialogStaysOut checks that media the bridge refuses is not
// left in it: a third party, and media that is not set up yet.
func TestBridgeRefusedDialogStaysOut(t *testing.T) {
	t.Run("ThirdParty", func(t *testing.T) {
		b := NewBridge()
		b.WaitDialogsNum = 3 // The proxy is started by hand
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogMedia(a.media))
		require.NoError(t, b.AddDialogMedia(c.media))

		require.Error(t, b.AddDialogMedia(newBridgeTestDialog(t, "x", media.CodecAudioUlaw).media))
		assert.Equal(t, []*DialogMedia{a.media, c.media}, bridgedMedias(&b))
	})

	t.Run("NotAnswered", func(t *testing.T) {
		b := NewBridge()
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogMedia(a.media))

		unanswered := newBridgeTestDialog(t, "unanswered", media.CodecAudioUlaw)
		unanswered.media.RTPPacketReader = nil
		require.Error(t, b.AddDialogMedia(unanswered.media))
		assert.Equal(t, []*DialogMedia{a.media}, bridgedMedias(&b))
	})

	// Audio handed over without a reader is refused at the join that would
	// start the proxy, and stays out.
	t.Run("AudioWithoutReader", func(t *testing.T) {
		b := NewBridge()
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogMedia(a.media))

		require.Error(t, b.AddAudioMedia(&BridgeAudioMedia{ReaderProps: MediaProps{Codec: media.CodecAudioUlaw}}))
		assert.Equal(t, []*DialogMedia{a.media}, bridgedMedias(&b))
	})
}

// TestBridgeRefusesDialogWithoutMedia checks that Bridge refuses media that
// has no media session with an error, whether it joins first or after another,
// and keeps the parties it has. The bridge reads a joining party's codec from
// its media session.
func TestBridgeRefusesDialogWithoutMedia(t *testing.T) {
	for _, first := range []bool{true, false} {
		t.Run(fmt.Sprintf("First=%t", first), func(t *testing.T) {
			b := NewBridge()
			a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
			var want []*DialogMedia
			if !first {
				require.NoError(t, b.AddDialogMedia(a.media))
				want = []*DialogMedia{a.media}
			}
			noMedia := newBridgeTestDialog(t, "nomedia", media.CodecAudioUlaw)
			noMedia.media.mediaSession = nil
			var err error
			require.NotPanics(t, func() { err = b.AddDialogMedia(noMedia.media) })
			require.ErrorIs(t, err, ErrNoMediaSetup)
			assert.Equal(t, want, bridgedMedias(&b))
		})
	}
}

// TestBridgeDTMFPassKeepsDialogAudio checks that a proxy passing DTMF leaves
// each dialog the audio reader and writer it had. Its DTMF interceptors, left
// on a dialog, still send a key read on that dialog to the other one once the
// proxy has stopped, and a send that fails, as it does once the other dialog has
// hung up, fails the read.
func TestBridgeDTMFPassKeepsDialogAudio(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 3 // The proxy is started by hand
	b.DTMFpass = true
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
	type dialogAudio struct {
		r io.Reader
		w io.Writer
	}
	joinedWith := map[*bridgeTestDialog]dialogAudio{}
	for _, d := range []*bridgeTestDialog{a, c} {
		r, err := d.media.AudioReader()
		require.NoError(t, err)
		w, err := d.media.AudioWriter()
		require.NoError(t, err)
		joinedWith[d] = dialogAudio{r, w}
		require.NoError(t, b.AddDialogMedia(d.media))
	}

	stop, err := proxyMediaControl(t, &b)
	require.NoError(t, err)
	frame := bytes.Repeat([]byte{0x20}, 160)
	a.sendRTP(t, 1, 0, frame)
	require.Equal(t, frame, c.recvRTP(t), "the proxy does not run")
	require.NoError(t, stop())

	for d, want := range joinedWith {
		r, err := d.media.AudioReader()
		require.NoError(t, err)
		w, err := d.media.AudioWriter()
		require.NoError(t, err)
		assert.Same(t, want.r, r, "%s must keep its audio reader", d.id)
		assert.Same(t, want.w, w, "%s must keep its audio writer", d.id)
	}

	// c hangs up, and a's caller presses a key
	require.NoError(t, c.conn.Close())
	r, err := a.media.AudioReader()
	require.NoError(t, err)
	a.sendDTMF(t, 2, 160, true, media.DTMFEvent{Event: 1, Volume: 10, Duration: 160})
	a.sendDTMF(t, 3, 160, false, media.DTMFEvent{Event: 1, EndOfEvent: true, Volume: 10, Duration: 320})
	require.NoError(t, a.conn.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, media.RTPBufSize)
	for range 2 {
		_, err := r.Read(buf)
		require.NoError(t, err, "a key pressed on a dialog the proxy has left must not fail its read")
	}
}

// TestBridgeDTMFPassSendsKeysToTheOtherParty checks that a proxy passing DTMF
// sends a key pressed on one party to the other party as a telephone-event,
// and none back to the party it was pressed on.
func TestBridgeDTMFPassSendsKeysToTheOtherParty(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 3 // The proxy is started by hand
	b.DTMFpass = true
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
	require.NoError(t, b.AddDialogMedia(a.media))
	require.NoError(t, b.AddDialogMedia(c.media))
	stop, err := proxyMediaControl(t, &b)
	require.NoError(t, err)
	defer func() { require.NoError(t, stop()) }()

	a.sendDTMF(t, 1, 160, true, media.DTMFEvent{Event: 1, Volume: 10, Duration: 160})
	a.sendDTMF(t, 2, 160, false, media.DTMFEvent{Event: 1, EndOfEvent: true, Volume: 10, Duration: 320})

	// The packets of the key are also passed on as they were read, so the
	// telephone-event the proxy sends follows them.
	require.NoError(t, c.peer.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, media.RTPBufSize)
	pkt := rtp.Packet{}
	for pkt.PayloadType != media.CodecTelephoneEvent8000.PayloadType {
		n, _, err := c.peer.ReadFrom(buf)
		require.NoError(t, err, "the other party was sent no key")
		require.NoError(t, pkt.Unmarshal(buf[:n]))
	}
	ev := media.DTMFEvent{}
	require.NoError(t, media.DTMFDecode(pkt.Payload, &ev))
	assert.EqualValues(t, 1, ev.Event)

	require.NoError(t, a.peer.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, _, err = a.peer.ReadFrom(buf)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the key was sent back to the party it was pressed on")
}

// TestBridgeProxyEndsOnCodecChange checks that a Bridge stops proxying once a
// re-INVITE moves one of its dialogs from PCMU to PCMA, and that the proxy
// ends with an error saying the bridge does not transcode. The proxy passes
// payload on as it is, so it would otherwise hand each side audio in the
// other's codec. The dialog left in PCMU is then its own again.
func TestBridgeProxyEndsOnCodecChange(t *testing.T) {
	t.Run("ProxyMedia", func(t *testing.T) {
		b := NewBridge()
		b.WaitDialogsNum = 3 // The proxy is started by hand
		moved, movedPeer := newAnsweredBridgeTestDialog(t, "moved")
		other := newBridgeTestDialog(t, "other", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogMedia(moved.media))
		require.NoError(t, b.AddDialogMedia(other.media))
		proxied := make(chan error, 1)
		go func() { proxied <- b.ProxyMedia(t.Context()) }()
		frame := bytes.Repeat([]byte{0x20}, 160)
		moved.sendRTPFrom(t, movedPeer, media.CodecAudioUlaw.PayloadType, 1, 0, frame)
		require.Equal(t, frame, other.recvRTP(t), "the proxy does not run")

		moved.reInviteAlaw(t, movedPeer)
		select {
		case err := <-proxied:
			require.ErrorIs(t, err, errBridgeNoTranscoding)
		case <-time.After(2 * time.Second):
			t.Fatal("the proxy went on between PCMA and PCMU")
		}
		next := bytes.Repeat([]byte{0x21}, 160)
		other.sendRTP(t, 2, 160, next)
		other.readFrame(t, next)
	})

	t.Run("AddDialogMedia", func(t *testing.T) {
		errorLog := &bridgeErrorLog{}
		b := Bridge{}
		b.Init(slog.New(errorLog))
		moved, movedPeer := newAnsweredBridgeTestDialog(t, "moved")
		other := newBridgeTestDialog(t, "other", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogMedia(moved.media))
		require.NoError(t, b.AddDialogMedia(other.media))
		frame := bytes.Repeat([]byte{0x20}, 160)
		moved.sendRTPFrom(t, movedPeer, media.CodecAudioUlaw.PayloadType, 1, 0, frame)
		require.Equal(t, frame, other.recvRTP(t), "the proxy does not run")

		moved.reInviteAlaw(t, movedPeer)
		require.Eventually(t, func() bool { return other.conn.reading.Load() == 0 }, 2*time.Second, time.Millisecond,
			"the proxy went on between PCMA and PCMU")
		require.ErrorIs(t, stopProxyMedia(t, &b), errBridgeNoTranscoding)
		assert.Equal(t, []string{"Proxy media stopped"}, errorLog.logged())
		next := bytes.Repeat([]byte{0x21}, 160)
		other.sendRTP(t, 2, 160, next)
		other.readFrame(t, next)
	})

	// A re-INVITE between the join and the start of the proxy is not missed
	t.Run("BeforeProxyStarts", func(t *testing.T) {
		b := NewBridge()
		b.WaitDialogsNum = 3 // The proxy is started by hand
		moved := newBridgeTestDialog(t, "moved", media.CodecAudioUlaw)
		other := newBridgeTestDialog(t, "other", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogMedia(moved.media))
		require.NoError(t, b.AddDialogMedia(other.media))
		moved.renegotiate(media.CodecAudioAlaw)

		proxied := make(chan error, 1)
		go func() { proxied <- b.ProxyMedia(t.Context()) }()
		select {
		case err := <-proxied:
			require.ErrorIs(t, err, errBridgeNoTranscoding)
		case <-time.After(2 * time.Second):
			t.Fatal("the proxy ran between PCMA and PCMU")
		}
	})
}
