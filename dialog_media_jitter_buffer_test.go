// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// jitterDialog is a dialog's media answered over loopback the way an answer
// sets it up, with a jitter buffer on its audio reader, a consumer reading
// that reader, and a peer socket sending it RTP.
type jitterDialog struct {
	d      *DialogMedia
	jitter *media.RTPJitterBuffer
	// offerer made the offer the media answered, and its SDP is what a
	// re-INVITE from the peer carries again.
	offerer *media.MediaSession
	peer    *net.UDPConn
	// payloadType is the payload type of the packets the peer sends: PCMU
	// until a test negotiates another.
	payloadType uint8
	// reads carries the sequence number of every packet the consumer read, and
	// the error that ended it.
	reads <-chan jitterDialogRead
}

type jitterDialogRead struct {
	seq uint16
	err error
}

// newAnsweredDialogMedia returns a dialog's media that answered an offer over
// loopback, the way an answer sets it up, and the session that made the offer.
func newAnsweredDialogMedia(t *testing.T) (*DialogMedia, *media.MediaSession) {
	t.Helper()
	ours := newMediaSessionForTest(t)
	offerer := newMediaSessionForTest(t)
	require.NoError(t, ours.RemoteSDP(offerer.LocalSDP(false), false))

	rtpSess := media.NewRTPSession(ours)
	d := &DialogMedia{}
	d.initRTPSessionUnsafe(ours, rtpSess)
	d.onCloseUnsafe(rtpSess.Close)
	require.NoError(t, rtpSess.MonitorBackground())
	return d, offerer
}

func newJitterDialog(t *testing.T) *jitterDialog {
	t.Helper()
	d, offerer := newAnsweredDialogMedia(t)
	return newJitterDialogOn(t, d, offerer)
}

// newJitterDialogOn returns the jitterDialog of d, media that answered the
// offer of offerer.
func newJitterDialogOn(t *testing.T, d *DialogMedia, offerer *media.MediaSession) *jitterDialog {
	t.Helper()
	ar, err := d.AudioReader(WithAudioReaderJitterBuffer(media.RTPJitterBufferOptions{
		DelayPackets: 1,
		MaxPackets:   4,
	}))
	require.NoError(t, err)
	jitter, ok := d.RTPPacketReader.Reader().(*media.RTPJitterBuffer)
	require.True(t, ok, "the audio reader reads no jitter buffer")

	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })

	return &jitterDialog{
		d:       d,
		jitter:  jitter,
		offerer: offerer,
		peer:    peer,
		reads:   consumeDialogAudio(t, d, ar),
	}
}

// consumeDialogAudio reads ar, the audio reader of d, until a read fails, and
// returns where the sequence number of every packet read arrives, and the
// error that ended the reads. The test's end closes d, which ends them, and
// waits for the consumer.
func consumeDialogAudio(t *testing.T, d *DialogMedia, ar io.Reader) <-chan jitterDialogRead {
	t.Helper()
	reads := make(chan jitterDialogRead, 1)
	stop := make(chan struct{})
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		buf := make([]byte, media.RTPBufSize)
		for {
			var r jitterDialogRead
			if _, r.err = ar.Read(buf); r.err == nil {
				r.seq = d.RTPPacketReader.PacketHeader.SequenceNumber
			}
			select {
			case reads <- r:
			case <-stop:
				return
			}
			if r.err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		_ = d.Close()
		select {
		case <-consumerDone:
		case <-time.After(5 * time.Second):
			t.Error("the audio reader did not return after Close")
		}
	})
	return reads
}

// jitterTestPacket is the packet seq of a stream of payloadType, 160 bytes of
// audio at timestamp, as the peer of a jitterDialog sends it.
func jitterTestPacket(t *testing.T, payloadType uint8, seq uint16, timestamp uint32) []byte {
	t.Helper()
	raw, err := (&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    payloadType,
			SequenceNumber: seq,
			Timestamp:      timestamp,
			SSRC:           1234,
		},
		Payload: make([]byte, 160),
	}).Marshal()
	require.NoError(t, err)
	return raw
}

// sendAndRead sends the packet seq, 20 ms of PCMU, to addr and waits for the
// consumer to read it, so each packet reaches a buffer that has played the one
// before.
func (jd *jitterDialog) sendAndRead(t *testing.T, addr net.UDPAddr, seq uint16) {
	t.Helper()
	jd.sendAndReadTimestamp(t, addr, seq, uint32(seq)*160)
}

func (jd *jitterDialog) sendAndReadTimestamp(t *testing.T, addr net.UDPAddr, seq uint16, timestamp uint32) {
	t.Helper()
	_, err := jd.peer.WriteToUDP(jitterTestPacket(t, jd.payloadType, seq, timestamp), &addr)
	require.NoError(t, err)

	select {
	case r := <-jd.reads:
		require.NoError(t, r.err, "reading packet %d", seq)
		require.Equal(t, seq, r.seq)
	case <-time.After(5 * time.Second):
		t.Fatalf("packet %d was never read", seq)
	}
}

// TestDialogMediaJitterBufferFollowsReinvite pins that a jitter buffer on the
// audio reader keeps playing the call after a re-INVITE, reading the session
// the re-INVITE installs, with nothing left reading the one it replaced. A
// re-INVITE that only renegotiates forks the session onto the same socket, and
// one that moves our media rebinds it and closes the old socket.
func TestDialogMediaJitterBufferFollowsReinvite(t *testing.T) {
	t.Run("SameSocket", func(t *testing.T) {
		jd := newJitterDialog(t)
		addr := jd.d.MediaSession().Laddr
		for seq := uint16(0); seq < 10; seq++ {
			jd.sendAndRead(t, addr, seq)
		}

		// The peer's re-INVITE, the way a session hands it to the media: the
		// offer, our answer to it, and the commit once the answer is out.
		ctx := t.Context()
		require.NoError(t, jd.d.onRemoteSDP(ctx, jd.offerer.LocalSDP(false), false))
		answer, err := jd.d.onLocalSDP(ctx, true, "")
		require.NoError(t, err)
		require.NotEmpty(t, answer)
		require.NoError(t, jd.d.onFinalize(ctx))
		require.True(t, jd.d.RTPPacketReader.Reader() == media.RTPReader(jd.jitter), "the re-INVITE took the jitter buffer off the audio reader")

		// The fork reads the socket the peer already sends to. A read loop
		// left on the replaced session would take every other packet from it.
		for seq := uint16(10); seq < 50; seq++ {
			jd.sendAndRead(t, addr, seq)
		}
	})

	// A re-INVITE that moves the call to a codec the session it replaces did
	// not negotiate. A read left on the replaced session takes no packet of
	// that codec for its own, so it would go on reading there.
	t.Run("CodecChange", func(t *testing.T) {
		ours := &media.MediaSession{
			Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
			Mode:   sdp.ModeSendrecv,
		}
		require.NoError(t, ours.Init())
		t.Cleanup(func() { _ = ours.Close() })
		offerer := newMediaSessionForTest(t)
		require.NoError(t, ours.RemoteSDP(offerer.LocalSDP(false), false))
		rtpSess := media.NewRTPSession(ours)
		d := &DialogMedia{}
		d.initRTPSessionUnsafe(ours, rtpSess)
		require.NoError(t, rtpSess.MonitorBackground())

		jd := newJitterDialogOn(t, d, offerer)
		addr := jd.d.MediaSession().Laddr
		for seq := uint16(0); seq < 10; seq++ {
			jd.sendAndRead(t, addr, seq)
		}

		ctx := t.Context()
		offer := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), offerer.Laddr.Port, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW})
		require.NoError(t, jd.d.onRemoteSDP(ctx, offer, false))
		answer, err := jd.d.onLocalSDP(ctx, true, "")
		require.NoError(t, err)
		require.NotEmpty(t, answer)
		require.NoError(t, jd.d.onFinalize(ctx))
		require.Equal(t, media.CodecAudioAlaw.PayloadType, media.CodecAudioFromSession(jd.d.MediaSession()).PayloadType)

		jd.payloadType = media.CodecAudioAlaw.PayloadType
		for seq := uint16(10); seq < 50; seq++ {
			jd.sendAndRead(t, addr, seq)
		}
	})

	t.Run("NewSocket", func(t *testing.T) {
		jd := newJitterDialog(t)
		for seq := uint16(0); seq < 10; seq++ {
			jd.sendAndRead(t, jd.d.MediaSession().Laddr, seq)
		}

		// A re-INVITE whose answer moves our media to a new socket.
		ms := jd.d.MediaSession().Fork()
		ms.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
		require.NoError(t, ms.Init())
		t.Cleanup(func() { _ = ms.Close() })
		answerer := newMediaSessionForTest(t)
		require.NoError(t, answerer.RemoteSDP(ms.LocalSDP(false), false))
		require.NoError(t, ms.RemoteSDP(answerer.LocalSDP(true), true))

		jd.d.mu.Lock()
		err := jd.d.mediaUpdateUnsafe(ms)
		jd.d.mu.Unlock()
		require.NoError(t, err)
		require.True(t, jd.d.RTPPacketReader.Reader() == media.RTPReader(jd.jitter), "the re-INVITE took the jitter buffer off the audio reader")

		// The replaced session is closed under the read loop's read, which
		// must continue on the new socket rather than end the stream.
		for seq := uint16(10); seq < 50; seq++ {
			jd.sendAndRead(t, ms.Laddr, seq)
		}
	})
}

// TestDialogMediaJitterBufferLearnsPeerPacketDuration pins that the jitter
// buffer on the audio reader plays the peer's packets at their own duration,
// which it learns from their RTP timestamps in the clock rate of the negotiated
// codec. The call negotiated PCMU at 20 ms, and the peer sends 10 ms packets.
func TestDialogMediaJitterBufferLearnsPeerPacketDuration(t *testing.T) {
	jd := newJitterDialog(t)
	require.Equal(t, 20*time.Millisecond, jd.jitter.Statistics().PacketDuration)
	addr := jd.d.MediaSession().Laddr
	for seq := uint16(0); seq < 4; seq++ {
		jd.sendAndReadTimestamp(t, addr, seq, uint32(seq)*80)
	}
	require.Equal(t, 10*time.Millisecond, jd.jitter.Statistics().PacketDuration)
}

// TestDialogMediaJitterBufferSetUpOnce pins that a second jitter buffer is
// refused. Stacked on the first it would read it, and a media update, which
// moves only the buffer reading the RTP session, would leave a read loop
// behind.
func TestDialogMediaJitterBufferSetUpOnce(t *testing.T) {
	jd := newJitterDialog(t)
	_, err := jd.d.AudioReader(WithAudioReaderJitterBuffer(media.RTPJitterBufferOptions{}))
	require.Error(t, err)
	require.True(t, jd.d.RTPPacketReader.Reader() == media.RTPReader(jd.jitter), "the first jitter buffer was replaced")
}

// TestDialogMediaJitterBufferBeforeSetup pins that the jitter buffer is
// refused with ErrNoMediaSetup on a dialog whose media is not set up, as every
// other option that works on the audio reader is.
func TestDialogMediaJitterBufferBeforeSetup(t *testing.T) {
	d := &DialogMedia{}
	_, err := d.AudioReader(WithAudioReaderJitterBuffer(media.RTPJitterBufferOptions{}))
	require.ErrorIs(t, err, ErrNoMediaSetup)
	require.ErrorIs(t, WithAudioReaderJitterBuffer(media.RTPJitterBufferOptions{})(d), ErrNoMediaSetup)
}

// TestDialogMediaJitterBufferTakesOverRead pins that a read of the audio
// reader waiting for a packet when the jitter buffer is put in carries on, and
// that what the peer sends from then on is read through the buffer. Putting
// the buffer in ends the waiting read with a read deadline, and the buffer
// reads the same socket: left on it, that deadline would end the buffer's
// reads as well.
func TestDialogMediaJitterBufferTakesOverRead(t *testing.T) {
	d, _ := newAnsweredDialogMedia(t)
	ar, err := d.AudioReader()
	require.NoError(t, err)
	reads := consumeDialogAudio(t, d, ar)

	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	addr := d.MediaSession().Laddr
	sendAndRead := func(seq uint16) {
		t.Helper()
		_, err := peer.WriteToUDP(jitterTestPacket(t, 0, seq, uint32(seq)*160), &addr)
		require.NoError(t, err)
		select {
		case r := <-reads:
			require.NoError(t, r.err, "reading packet %d", seq)
			require.Equal(t, seq, r.seq)
		case <-time.After(5 * time.Second):
			t.Fatalf("packet %d was never read", seq)
		}
	}

	// The consumer has read a packet and waits for the next one.
	sendAndRead(0)
	_, err = d.AudioReader(WithAudioReaderJitterBuffer(media.RTPJitterBufferOptions{
		DelayPackets: 1,
		MaxPackets:   4,
	}))
	require.NoError(t, err)
	jitter, ok := d.RTPPacketReader.Reader().(*media.RTPJitterBuffer)
	require.True(t, ok, "the audio reader reads no jitter buffer")

	for seq := uint16(1); seq < 10; seq++ {
		sendAndRead(seq)
	}
	// The read that waited may have taken one packet itself before the
	// buffer's reads began.
	require.GreaterOrEqual(t, jitter.Statistics().PacketsReleased, uint64(8), "packets read past the jitter buffer")
}

// TestDialogMediaJitterBufferKeepsReadStop pins that a stop of the dialog's
// reads set before the jitter buffer is put in is still in force after: the
// read through the buffer ends with the stop instead of returning the packet
// that waits on the socket.
func TestDialogMediaJitterBufferKeepsReadStop(t *testing.T) {
	d, _ := newAnsweredDialogMedia(t)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.StopRTP(1, 0))

	ar, err := d.AudioReader(WithAudioReaderJitterBuffer(media.RTPJitterBufferOptions{
		DelayPackets: 1,
		MaxPackets:   4,
	}))
	require.NoError(t, err)

	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	addr := d.MediaSession().Laddr
	_, err = peer.WriteToUDP(jitterTestPacket(t, 0, 1, 160), &addr)
	require.NoError(t, err)

	_, err = readWithin(t, ar, 5*time.Second)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded)
}

// holdingRTPReader passes reads to inner, and holds a read that failed until
// release is closed, reporting on held that it does.
type holdingRTPReader struct {
	inner   media.RTPReader
	held    chan struct{}
	release chan struct{}
}

func (r *holdingRTPReader) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	n, err := r.inner.ReadRTP(buf, p)
	if err != nil {
		r.held <- struct{}{}
		<-r.release
	}
	return n, err
}

// TestDialogMediaCloseJoinsJitterBuffer pins that Close returns only after the
// jitter buffer's read loop has stopped reading the dialog's session. Closing
// the sockets fails the loop's read, and the loop ends once it sees that; the
// upstream here holds it between the two.
func TestDialogMediaCloseJoinsJitterBuffer(t *testing.T) {
	d, _ := newAnsweredDialogMedia(t)
	upstream := &holdingRTPReader{
		inner:   d.RTPPacketReader.Reader(),
		held:    make(chan struct{}, 1),
		release: make(chan struct{}),
	}
	d.RTPPacketReader.UpdateReader(upstream)
	// UpdateReader ends a read waiting on the session it replaces with a
	// read deadline. Nothing reads yet, and upstream reads that session, so
	// the deadline is cleared before it does.
	require.NoError(t, d.StartRTP(1, 0))
	ar, err := d.AudioReader(WithAudioReaderJitterBuffer(media.RTPJitterBufferOptions{
		DelayPackets: 1,
		MaxPackets:   4,
	}))
	require.NoError(t, err)
	jitter := d.RTPPacketReader.Reader().(*media.RTPJitterBuffer)

	release := sync.OnceFunc(func() { close(upstream.release) })
	closed := make(chan error, 1)
	var closeDone chan struct{}
	t.Cleanup(func() {
		release()
		if closeDone == nil {
			_ = d.Close()
			return
		}
		select {
		case <-closeDone:
		case <-time.After(5 * time.Second):
			t.Error("Close did not return")
		}
	})

	// One packet read through the buffer starts its read loop, which then
	// waits on the socket for the next one.
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	raw, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 1, SSRC: 1234},
		Payload: make([]byte, 160),
	}).Marshal()
	require.NoError(t, err)
	addr := d.MediaSession().Laddr
	_, err = peer.WriteToUDP(raw, &addr)
	require.NoError(t, err)
	read := make(chan error, 1)
	go func() {
		_, err := ar.Read(make([]byte, media.RTPBufSize))
		read <- err
	}()
	select {
	case err := <-read:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the packet was never read")
	}

	closeDone = make(chan struct{})
	go func() {
		defer close(closeDone)
		closed <- d.Close()
	}()
	select {
	case <-upstream.held:
	case <-time.After(5 * time.Second):
		t.Fatal("closing the sockets did not fail the read loop's read")
	}
	select {
	case err := <-closed:
		t.Fatalf("Close returned (%v) while the jitter buffer's read loop was still reading", err)
	case <-time.After(100 * time.Millisecond):
	}
	select {
	case <-jitter.Done():
		t.Fatal("the read loop stopped while its read was held")
	default:
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
