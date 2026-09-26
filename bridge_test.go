// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"encoding/binary"
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

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
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

	// The dialogs have sockets and contexts, as every answered dialog has: the
	// proxy watches their contexts and stops their reads with a deadline
	incoming := newBridgeTestDialog(t, "incoming", media.CodecAudioAlaw)
	incoming.media.audioReader = bytes.NewBuffer(make([]byte, 9999))
	incoming.media.audioWriter = bytes.NewBuffer(make([]byte, 0))
	outgoing := newBridgeTestDialog(t, "outgoing", media.CodecAudioAlaw)
	outgoing.media.audioReader = bytes.NewBuffer(make([]byte, 9999))
	outgoing.media.audioWriter = bytes.NewBuffer(make([]byte, 0))

	err := b.AddDialogSession(incoming)
	require.NoError(t, err)
	err = b.AddDialogSession(outgoing)
	require.NoError(t, err)

	err = b.proxyMedia(b.GetDialogs(), nil)
	require.ErrorIs(t, err, io.EOF)

	// Confirm all data is proxied
	assert.Equal(t, 9999, incoming.media.audioWriter.(*bytes.Buffer).Len())
	assert.Equal(t, 9999, outgoing.media.audioWriter.(*bytes.Buffer).Len())
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
			require.NoError(t, b.AddDialogSession(a))
			require.NoError(t, b.AddDialogSession(c))

			proxied := make(chan error, 1)
			go func() { proxied <- b.ProxyMedia() }()

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

// proxyMediaControl calls ProxyMediaControl and waits, bounded, for it to return.
func proxyMediaControl(t *testing.T, b *Bridge) (func() error, error) {
	t.Helper()
	type control struct {
		stop func() error
		err  error
	}
	started := make(chan control, 1)
	go func() {
		stop, err := b.ProxyMediaControl()
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
// while both dialogs are silent and leaves their reads usable. It is refused,
// as ProxyMedia is, with fewer than two dialogs or with the proxy that
// AddDialogSession starts already running, which its stop would not stop.
func TestBridgeProxyMediaControl(t *testing.T) {
	for _, dtmfPass := range []bool{false, true} {
		t.Run(fmt.Sprintf("DTMFpass=%t", dtmfPass), func(t *testing.T) {
			b := NewBridge()
			b.WaitDialogsNum = 3 // The proxy is started by hand
			b.DTMFpass = dtmfPass
			a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
			c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
			require.NoError(t, b.AddDialogSession(a))
			require.NoError(t, b.AddDialogSession(c))

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
		require.NoError(t, b.AddDialogSession(newBridgeTestDialog(t, "a", media.CodecAudioUlaw)))
		_, err := proxyMediaControl(t, &b)
		require.Error(t, err)
	})

	t.Run("ProxyAlreadyRunning", func(t *testing.T) {
		b := NewBridge()
		require.NoError(t, b.AddDialogSession(newBridgeTestDialog(t, "a", media.CodecAudioUlaw)))
		require.NoError(t, b.AddDialogSession(newBridgeTestDialog(t, "c", media.CodecAudioUlaw)))
		_, err := proxyMediaControl(t, &b)
		require.Error(t, err)
	})
}

// TestBridgeConcurrentUse checks, under the race detector, that a Bridge can be
// used from several goroutines at once: dialogs joining it while its dialogs
// and its originator are read, and while its proxy is started by hand.
func TestBridgeConcurrentUse(t *testing.T) {
	b := NewBridge()
	b.WaitDialogsNum = 3 // The proxy is started by hand
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
	// InviteBridge dials from the bridge's originator, with its From. The call
	// is refused, so it only reads the originator.
	for _, d := range []*bridgeTestDialog{a, c} {
		d.sipDialog.InviteRequest.AppendHeader(&sip.FromHeader{Address: sip.Uri{User: d.id, Host: "127.0.0.1"}, Params: sip.NewParams()})
	}
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, sip.StatusBusyHere, "Busy Here", nil)
	})

	deadline := time.Now().Add(5 * time.Second)
	done := make(chan error, 4)
	go func() {
		var err error
		for _, d := range []*bridgeTestDialog{a, c} {
			err = errors.Join(err, b.AddDialogSession(d))
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
			stop, err := b.ProxyMediaControl()
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
	go func() {
		_, err := dg.InviteBridge(context.Background(), sip.Uri{User: "x", Host: "127.0.0.1", Port: 5070}, &b, InviteOptions{})
		if err == nil {
			err = errors.New("a refused call was bridged")
		} else {
			err = nil
		}
		done <- err
	}()
	for range 4 {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the bridge's users did not finish")
		}
	}
	assert.Equal(t, []DialogSession{a, c}, b.GetDialogs())
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

// TestBridgeProxyStopsWhenADialogEnds checks that the proxy AddDialogSession
// starts stops once either dialog ends, as a BYE ends it and then closes its
// media, and hands the other dialog back: nothing of the proxy is left reading
// it, StopProxyMedia returns once the proxy has stopped, and the dialog's next
// frame reaches its own reader. A proxy left reading the other dialog took that
// frame and failed to write it to the dialog that ended. A hangup is not an
// error, so nothing is logged at Error.
func TestBridgeProxyStopsWhenADialogEnds(t *testing.T) {
	for _, dtmfPass := range []bool{false, true} {
		for _, ending := range []string{"a", "c"} {
			t.Run(fmt.Sprintf("DTMFpass=%t/%sEnds", dtmfPass, ending), func(t *testing.T) {
				errorLog := &bridgeErrorLog{}
				b := Bridge{DTMFpass: dtmfPass}
				b.Init(slog.New(errorLog))
				a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
				c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
				require.NoError(t, b.AddDialogSession(a))
				require.NoError(t, b.AddDialogSession(c))
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
				assert.True(t, err == nil || errors.Is(err, io.EOF), "a hangup ended the proxy with %v", err)
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
		require.NoError(t, b.AddDialogSession(a))
		require.NoError(t, b.AddDialogSession(c))
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
		require.NoError(t, b.AddDialogSession(newBridgeTestDialog(t, "a", media.CodecAudioUlaw)))
		require.NoError(t, stopProxyMedia(t, &b))
	})
}

// TestBridgeRefusedDialogStaysOut checks that a dialog the bridge refuses is
// not left in it: a third party, and a dialog with no media set up yet.
func TestBridgeRefusedDialogStaysOut(t *testing.T) {
	t.Run("ThirdParty", func(t *testing.T) {
		b := NewBridge()
		b.WaitDialogsNum = 3 // The proxy is started by hand
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogSession(a))
		require.NoError(t, b.AddDialogSession(c))

		require.Error(t, b.AddDialogSession(newBridgeTestDialog(t, "x", media.CodecAudioUlaw)))
		assert.Equal(t, []DialogSession{a, c}, b.GetDialogs())
	})

	t.Run("NotAnswered", func(t *testing.T) {
		b := NewBridge()
		a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogSession(a))

		unanswered := newBridgeTestDialog(t, "unanswered", media.CodecAudioUlaw)
		unanswered.media.RTPPacketReader = nil
		require.Error(t, b.AddDialogSession(unanswered))
		assert.Equal(t, []DialogSession{a}, b.GetDialogs())
	})
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
		require.NoError(t, b.AddDialogSession(d))
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
	err := serveBackground(t, tu, ctx, func(in *DialogServerSession) {
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
			log("Adding dialog in bridge failed", err)
			return
		}

		out, err := tu.InviteBridge(ctx, sip.Uri{User: "test", Host: "127.0.0.200", Port: 5090}, &bridge, InviteOptions{})
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

		err := serveBackground(t, dg, ctx, func(d *DialogServerSession) {
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
	// ctx is the dialog's context, which end ends
	ctx context.Context
	end context.CancelFunc
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
	ctx, end := context.WithCancel(context.Background())
	t.Cleanup(end)

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
		ctx:       ctx,
		end:       end,
	}
}

// hangUp ends the dialog as a BYE from its peer does: its context ends, and
// then its media is closed.
func (d *bridgeTestDialog) hangUp(t *testing.T) {
	t.Helper()
	d.end()
	require.NoError(t, d.conn.Close())
}

func (d *bridgeTestDialog) Id() string                       { return d.id }
func (d *bridgeTestDialog) Context() context.Context         { return d.ctx }
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

// renegotiate moves the dialog's audio to codec, as a re-INVITE that settles on
// another codec does: under the dialog's lock its media session is replaced by
// a fork of it on that codec.
func (d *bridgeTestDialog) renegotiate(codec media.Codec) {
	d.media.mu.Lock()
	defer d.media.mu.Unlock()
	sess := d.media.mediaSession.Fork()
	sess.Codecs = []media.Codec{codec}
	d.media.mediaSession = sess
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
				require.NoError(t, b.AddDialogSession(dialogs[id]))
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
				require.NoError(t, b.AddDialogSession(dialogs[tc.joins]), "a dialog that matches the mix must join")
			}
			if tc.leaves != "" {
				require.NoError(t, b.RemoveDialogSession(dialogs[tc.leaves]))
			}

			var want []DialogSession
			for _, id := range tc.want {
				want = append(want, dialogs[id])
			}
			assert.Equal(t, want, b.DialogSessionsList())
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
	require.NoError(t, b.AddDialogSession(a))
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
		require.NoError(t, b.AddDialogSession(c))
		require.NoError(t, b.RemoveDialogSession(c))
	}
}

// newAnsweredBridgeTestDialog returns a confirmed dialog whose media answered,
// over loopback and the way an answer sets it up, an offer of PCMU from peer.
// The dialog can also take PCMA, and handles a re-INVITE as the dialog does.
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
	require.NoError(t, sess.RemoteSDP(offer))

	rtpSess := media.NewRTPSession(sess)
	m := &DialogMedia{}
	m.initRTPSessionUnsafe(sess, rtpSess)
	m.onCloseUnsafe(rtpSess.Close)
	require.NoError(t, rtpSess.MonitorBackground())
	t.Cleanup(func() { _ = m.Close() })

	invite := sip.NewRequest(sip.INVITE, sip.Uri{User: id, Host: "127.0.0.1", Port: 5060})
	sipDialog := &sipgo.Dialog{ID: id, InviteRequest: invite}
	sipDialog.InitWithState(sip.DialogStateConfirmed)
	ctx, end := context.WithCancel(context.Background())
	t.Cleanup(end)
	return &bridgeTestDialog{id: id, media: m, sipDialog: sipDialog, ctx: ctx, end: end}, peer
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
		require.NoError(t, b.AddDialogSession(d))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	// Every byte of a PCMU frame is its value, which decodes and encodes back
	// to itself
	listener.sendRTP(t, 1, 0, bytes.Repeat([]byte{0x20}, 160))
	waitHeard(t, movedHeard, 0x20)

	// The peer moves the dialog to PCMA
	offer := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1),
		movedPeer.LocalAddr().(*net.UDPAddr).Port, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW})
	tx := &fakeServerTransaction{}
	contact := &sip.ContactHeader{Address: sip.Uri{User: "us", Host: "127.0.0.1"}}
	require.NoError(t, moved.media.handleMediaUpdate(context.Background(), newReInvite(t, offer), tx, contact))
	require.Equal(t, sip.StatusOK, tx.res.StatusCode)
	p := MediaProps{}
	moved.media.audioReaderProps(&p)
	require.Equal(t, media.CodecAudioAlaw, p.Codec)

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
		require.NoError(t, b.addDialogStream(d, &stream, media.CodecAudioUlaw))
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

// TestBridgeRefusesDialogWithoutMedia checks that both bridges refuse a dialog
// that has no media session with an error, whether it joins first or after
// another, and keep the dialogs they have. Each bridge reads a joining
// dialog's codec from its media session, which dereferenced the missing
// session and panicked the join, or the next one.
func TestBridgeRefusesDialogWithoutMedia(t *testing.T) {
	newNoMedia := func(t *testing.T) *bridgeTestDialog {
		d := newBridgeTestDialog(t, "nomedia", media.CodecAudioUlaw)
		d.media.mediaSession = nil
		return d
	}

	for _, first := range []bool{true, false} {
		t.Run(fmt.Sprintf("Bridge/First=%t", first), func(t *testing.T) {
			b := NewBridge()
			a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
			var want []DialogSession
			if !first {
				require.NoError(t, b.AddDialogSession(a))
				want = []DialogSession{a}
			}
			var err error
			require.NotPanics(t, func() { err = b.AddDialogSession(newNoMedia(t)) })
			require.Error(t, err)
			assert.Equal(t, want, b.GetDialogs())
		})

		t.Run(fmt.Sprintf("BridgeMix/First=%t", first), func(t *testing.T) {
			b := NewBridgeMix()
			a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
			var want []DialogSession
			if !first {
				require.NoError(t, b.AddDialogSession(a))
				want = []DialogSession{a}
			}
			t.Cleanup(func() { stopBridgeMix(t, b) })
			var err error
			require.NotPanics(t, func() { err = b.AddDialogSession(newNoMedia(t)) })
			require.Error(t, err)
			assert.Equal(t, want, b.DialogSessionsList())
			if !first {
				assert.Equal(t, 1, b.stateRead(), "the dialog in the bridge must keep being mixed")
			}
		})
	}
}

// TestBridgeMixRefusesDuplicateDialog checks that a dialog already in the bridge
// is refused a second join with an error, and that the bridge keeps mixing the
// dialogs it has. The bridge tells its dialogs apart by ID, so a dialog in it
// twice was read by two streams at once, and an eviction by ID took both
// entries out together.
func TestBridgeMixRefusesDuplicateDialog(t *testing.T) {
	b := NewBridgeMix()
	a := newBridgeTestDialog(t, "a", media.CodecAudioUlaw)
	c := newBridgeTestDialog(t, "c", media.CodecAudioUlaw)
	for _, d := range []*bridgeTestDialog{a, c} {
		require.NoError(t, b.AddDialogSession(d))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	require.Error(t, b.AddDialogSession(a), "a dialog in the bridge joined again")
	require.Error(t, b.AddDialogSession(newBridgeTestDialog(t, "a", media.CodecAudioUlaw)), "a dialog with the ID of one in the bridge joined")
	assert.Equal(t, []DialogSession{a, c}, b.DialogSessionsList())
	assert.Equal(t, 1, b.stateRead(), "the dialogs in the bridge must keep being mixed")

	require.NoError(t, b.RemoveDialogSession(a))
	assert.Equal(t, []DialogSession{c}, b.DialogSessionsList())
}

// reInviteAlaw has the dialog's peer move it to PCMA with a re-INVITE, which
// the dialog handles as it handles one.
func (d *bridgeTestDialog) reInviteAlaw(t *testing.T, peer *net.UDPConn) {
	t.Helper()
	offer := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1),
		peer.LocalAddr().(*net.UDPAddr).Port, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW})
	tx := &fakeServerTransaction{}
	contact := &sip.ContactHeader{Address: sip.Uri{User: "us", Host: "127.0.0.1"}}
	require.NoError(t, d.media.handleMediaUpdate(context.Background(), newReInvite(t, offer), tx, contact))
	require.Equal(t, sip.StatusOK, tx.res.StatusCode)
	p := MediaProps{}
	d.media.audioWriterProps(&p)
	require.Equal(t, media.CodecAudioAlaw, p.Codec)
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
		require.NoError(t, b.AddDialogSession(moved))
		require.NoError(t, b.AddDialogSession(other))
		proxied := make(chan error, 1)
		go func() { proxied <- b.ProxyMedia() }()
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

	t.Run("AddDialogSession", func(t *testing.T) {
		errorLog := &bridgeErrorLog{}
		b := Bridge{}
		b.Init(slog.New(errorLog))
		moved, movedPeer := newAnsweredBridgeTestDialog(t, "moved")
		other := newBridgeTestDialog(t, "other", media.CodecAudioUlaw)
		require.NoError(t, b.AddDialogSession(moved))
		require.NoError(t, b.AddDialogSession(other))
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
		require.NoError(t, b.AddDialogSession(moved))
		require.NoError(t, b.AddDialogSession(other))
		moved.renegotiate(media.CodecAudioAlaw)

		proxied := make(chan error, 1)
		go func() { proxied <- b.ProxyMedia() }()
		select {
		case err := <-proxied:
			require.ErrorIs(t, err, errBridgeNoTranscoding)
		case <-time.After(2 * time.Second):
			t.Fatal("the proxy ran between PCMA and PCMU")
		}
	})
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
	err := serveBackground(t, tu, ctx, func(in *DialogServerSession) {
		var exitErr error
		defer func() { dialogExit <- exitErr }()
		bridge := currentBridge.Load()

		in.Trying()
		in.Ringing()
		in.Answer()

		// Add us in bridge
		log("Adding into bridge", in.ID)
		if err := bridge.AddDialogSession(in); err != nil {
			// A subtest hangs its calls up as soon as they are answered, so a
			// call can end before its handler joins it, which refuses the join
			if in.LoadState() != sip.DialogStateEnded {
				exitErr = fmt.Errorf("adding %s into bridge: %w", in.ID, err)
			}
			return
		}
		defer func() {
			log("Removing from bridge", in.ID)
			if err := bridge.RemoveDialogSession(in); err != nil {
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
			waitDialogExit(t)
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
				waitDialogExit(t)
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
