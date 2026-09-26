// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// fakeServerTransaction records the response handleMediaUpdate builds.
type fakeServerTransaction struct {
	res *sip.Response
}

func (t *fakeServerTransaction) Respond(res *sip.Response) error      { t.res = res; return nil }
func (t *fakeServerTransaction) Acks() <-chan *sip.Request            { return nil }
func (t *fakeServerTransaction) OnCancel(f sip.FnTxCancel) bool       { return false }
func (t *fakeServerTransaction) OnTerminate(f sip.FnTxTerminate) bool { return false }
func (t *fakeServerTransaction) Terminate()                           {}
func (t *fakeServerTransaction) Done() <-chan struct{}                { return nil }
func (t *fakeServerTransaction) Err() error                           { return nil }

func newReInvite(t *testing.T, body []byte) *sip.Request {
	t.Helper()
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "alice", Host: "127.0.0.1"})
	req.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "bob", Host: "127.0.0.2"}})
	if body != nil {
		req.SetBody(body)
	}
	return req
}

func newMediaSessionForTest(t *testing.T) *media.MediaSession {
	t.Helper()
	sess := &media.MediaSession{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0},
		Mode:   sdp.ModeSendrecv,
	}
	require.NoError(t, sess.Init())
	t.Cleanup(func() { sess.Close() })
	return sess
}

// fakePortAllocator hands out ports from a fixed list and records releases.
type fakePortAllocator struct {
	ports    []int
	next     int
	err      error
	released []int
}

func (a *fakePortAllocator) AllocateRTPPort() (int, error) {
	if a.err != nil {
		return 0, a.err
	}
	p := a.ports[a.next]
	a.next++
	return p, nil
}

func (a *fakePortAllocator) ReleaseRTPPort(port int) { a.released = append(a.released, port) }

// freeEvenPort returns an even port that is free together with its RTCP companion.
func freeEvenPort(t *testing.T, ip net.IP) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 0})
		require.NoError(t, err)
		port := c.LocalAddr().(*net.UDPAddr).Port
		c.Close()
		if port%2 != 0 {
			continue
		}
		// The RTCP companion must be free too, as MediaSession binds the pair.
		c2, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port + 1})
		if err != nil {
			continue
		}
		c2.Close()
		return port
	}
	t.Fatal("no free even port pair found")
	return 0
}

func TestInitMediaSessionRTPPortAllocator(t *testing.T) {
	bindIP := net.IPv4(127, 0, 0, 1)

	t.Run("BindsAllocatedPortAndReleasesOnClose", func(t *testing.T) {
		// A probed port can be taken by anything else before Init binds it, so a
		// lost race is retried rather than reported as a failure.
		var d *DialogMedia
		var alloc *fakePortAllocator
		var port int
		for attempt := 0; ; attempt++ {
			port = freeEvenPort(t, bindIP)
			alloc = &fakePortAllocator{ports: []int{port}}
			d = &DialogMedia{}
			err := d.initMediaSessionFromConf(MediaConfig{
				Codecs:           []media.Codec{media.CodecAudioUlaw},
				bindIP:           bindIP,
				RTPPortAllocator: alloc,
			})
			if err == nil {
				break
			}
			require.Less(t, attempt, 10, "could not bind a probed free port: %v", err)
		}
		// The allocated port is what actually got bound, not an OS ephemeral one.
		require.Equal(t, port, d.mediaSession.Laddr.Port)
		require.Empty(t, alloc.released, "port released before Close")

		require.NoError(t, d.Close())
		require.Equal(t, []int{port}, alloc.released)

		// Close is latched, so the allocator is never handed the port twice.
		require.NoError(t, d.Close())
		require.Equal(t, []int{port}, alloc.released)
	})

	// A failed Init never reaches Close, so the port has to go back right away
	// or it leaks out of the pool for the life of the process.
	t.Run("ReleasesWhenInitFails", func(t *testing.T) {
		alloc := &fakePortAllocator{ports: []int{0}}
		d := &DialogMedia{}
		err := d.initMediaSessionFromConf(MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw},
			// TEST-NET-3, assigned to no interface, so the bind always fails.
			bindIP:           net.IPv4(203, 0, 113, 1),
			RTPPortAllocator: alloc,
		})
		require.Error(t, err)
		require.Equal(t, []int{0}, alloc.released)
	})

	// An allocator with nothing left fails the call rather than silently falling
	// back to an unbounded ephemeral port.
	t.Run("AllocationErrorFailsInit", func(t *testing.T) {
		alloc := &fakePortAllocator{err: errors.New("pool exhausted")}
		d := &DialogMedia{}
		err := d.initMediaSessionFromConf(MediaConfig{
			Codecs:           []media.Codec{media.CodecAudioUlaw},
			bindIP:           bindIP,
			RTPPortAllocator: alloc,
		})
		require.ErrorContains(t, err, "pool exhausted")
		require.Nil(t, d.mediaSession)
		require.Empty(t, alloc.released, "nothing was allocated, so nothing to release")
	})

	// Nil allocator keeps the historical OS/globals behaviour.
	t.Run("NilAllocatorLeavesPortToOS", func(t *testing.T) {
		d := &DialogMedia{}
		require.NoError(t, d.initMediaSessionFromConf(MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw},
			bindIP: bindIP,
		}))
		defer d.Close()
		require.Nil(t, d.releaseRTPPort)
		require.NotZero(t, d.mediaSession.Laddr.Port)
	})
}

// requireLocalSDP asserts the response replays our published media. LocalSDP
// regenerates the o= session-version on every call, so the bytes can not be
// compared to a second LocalSDP call; the port and codec identify it instead.
func requireLocalSDP(t *testing.T, d *DialogMedia, res *sip.Response) {
	t.Helper()
	require.Equal(t, "application/sdp", res.ContentType().Value())
	require.Contains(t, string(res.Body()),
		fmt.Sprintf("m=audio %d RTP/AVP 0", d.mediaSession.Laddr.Port))
}

// An offer-less re-INVITE is a request for an offer (RFC 3261 14.2): it must be
// answered with the SDP we already published, not rejected.
func TestHandleMediaUpdateOfferless(t *testing.T) {
	contactHDR := &sip.ContactHeader{Address: sip.Uri{User: "us", Host: "127.0.0.1"}}

	t.Run("ReplaysLocalSDP", func(t *testing.T) {
		d := &DialogMedia{mediaSession: newMediaSessionForTest(t)}
		tx := &fakeServerTransaction{}

		require.NoError(t, d.handleMediaUpdate(newReInvite(t, nil), tx, contactHDR))
		require.Equal(t, sip.StatusOK, tx.res.StatusCode)
		requireLocalSDP(t, d, tx.res)
	})

	// Content-Length: 0 can reach us as an empty but non nil body, which a nil
	// check lets through into the SDP parser. It finds no m= line and the peer is
	// told its legal request was rejected.
	t.Run("EmptyNonNilBodyReplaysLocalSDP", func(t *testing.T) {
		d := &DialogMedia{mediaSession: newMediaSessionForTest(t)}
		tx := &fakeServerTransaction{}

		require.NoError(t, d.handleMediaUpdate(newReInvite(t, []byte{}), tx, contactHDR))
		require.Equal(t, sip.StatusOK, tx.res.StatusCode)
		requireLocalSDP(t, d, tx.res)
	})

	// A request for an offer can not be answered with an SDP we never built. The
	// bodied path already rejects this; the offer-less path must not instead
	// dereference the nil session.
	t.Run("NoMediaSessionIsRejected", func(t *testing.T) {
		d := &DialogMedia{}
		tx := &fakeServerTransaction{}

		require.NoError(t, d.handleMediaUpdate(newReInvite(t, nil), tx, contactHDR))
		require.Equal(t, sip.StatusRequestTerminated, tx.res.StatusCode)
	})

	// Control: a re-INVITE that does carry an offer must still be negotiated,
	// never swallowed by the offer-less branch.
	t.Run("BodiedOfferIsStillNegotiated", func(t *testing.T) {
		d := &DialogMedia{}
		tx := &fakeServerTransaction{}

		require.NoError(t, d.handleMediaUpdate(newReInvite(t, []byte("v=0\r\n")), tx, contactHDR))
		require.Equal(t, sip.StatusRequestTerminated, tx.res.StatusCode)
		require.Contains(t, tx.res.Reason, "no media session present")
	})
}

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
	// reads carries the sequence number of every packet the consumer read, and
	// the error that ended it.
	reads chan jitterDialogRead
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
	require.NoError(t, ours.RemoteSDP(offerer.LocalSDP()))

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

	jd := &jitterDialog{
		d:       d,
		jitter:  jitter,
		offerer: offerer,
		peer:    peer,
		reads:   make(chan jitterDialogRead, 1),
	}
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
			case jd.reads <- r:
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
	return jd
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
	raw, err := (&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    0,
			SequenceNumber: seq,
			Timestamp:      timestamp,
			SSRC:           1234,
		},
		Payload: make([]byte, 160),
	}).Marshal()
	require.NoError(t, err)
	_, err = jd.peer.WriteToUDP(raw, &addr)
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

		tx := &fakeServerTransaction{}
		contactHDR := &sip.ContactHeader{Address: sip.Uri{User: "us", Host: "127.0.0.1"}}
		require.NoError(t, jd.d.handleMediaUpdate(newReInvite(t, jd.offerer.LocalSDP()), tx, contactHDR))
		require.Equal(t, sip.StatusOK, tx.res.StatusCode)
		require.True(t, jd.d.RTPPacketReader.Reader() == media.RTPReader(jd.jitter), "the re-INVITE took the jitter buffer off the audio reader")

		// The fork reads the socket the peer already sends to. A read loop
		// left on the replaced session would take every other packet from it.
		for seq := uint16(10); seq < 50; seq++ {
			jd.sendAndRead(t, addr, seq)
		}
	})

	t.Run("NewSocket", func(t *testing.T) {
		jd := newJitterDialog(t)
		for seq := uint16(0); seq < 10; seq++ {
			jd.sendAndRead(t, jd.d.MediaSession().Laddr, seq)
		}

		// Our own re-INVITE moves the media to a new socket, as
		// reInviteMediaSession does.
		ms := jd.d.MediaSession().Fork()
		ms.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
		require.NoError(t, ms.Init())
		t.Cleanup(func() { _ = ms.Close() })
		answerer := newMediaSessionForTest(t)
		require.NoError(t, answerer.RemoteSDP(ms.LocalSDP()))
		ms.RemoteSDPIsAnswer = true
		require.NoError(t, ms.RemoteSDP(answerer.LocalSDP()))

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
