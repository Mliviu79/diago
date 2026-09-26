// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
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
			err = d.Answer()
			require.NoError(t, err)

			// ms := d.mediaSession
			buf := make([]byte, media.RTPBufSize)
			r, _ := d.AudioReader()
			n, err := r.Read(buf)
			require.NoError(t, err)

			w, _ := d.AudioWriter()
			w.Write(buf[:n])
			require.NoError(t, err)

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
// reported as failed.
type bridgeTestConn struct {
	*net.UDPConn
	deadlineErr error
	reading     atomic.Int32
}

func (c *bridgeTestConn) ReadFrom(b []byte) (int, net.Addr, error) {
	c.reading.Add(1)
	defer c.reading.Add(-1)
	return c.UDPConn.ReadFrom(b)
}

func (c *bridgeTestConn) SetReadDeadline(t time.Time) error {
	if err := c.UDPConn.SetReadDeadline(t); err != nil {
		return err
	}
	return c.deadlineErr
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
// on its own when a write to a dialog fails, as it does once a BYE has closed
// that dialog's media, and its readers are then still reading the others.
func TestBridgeMixLeaveStopsReadersOfEndedLoop(t *testing.T) {
	b := NewBridgeMix()
	talker := newBridgeTestDialog(t, "talker", media.CodecAudioUlaw)
	hungUp := newBridgeTestDialog(t, "hungup", media.CodecAudioUlaw)
	for _, d := range []*bridgeTestDialog{talker, hungUp} {
		require.NoError(t, b.AddDialogSession(d))
	}
	t.Cleanup(func() { stopBridgeMix(t, b) })

	// The talker's frame is mixed and written to the hung-up dialog, whose
	// closed connection fails the write and ends the loop
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
		bridge.WaitDialogsNum = 2 // Do not start mixing until all 3 get joined, otherwise there will be no gurantee when something is mixed
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
