// SPDX-License-Identifier: MPL-2.0

package media

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// closedRTPReader stands for the reader of a replaced session whose socket is
// closed while a read is blocked on it: each ReadRTP reports that it started,
// blocks until closed is closed, and then fails with net.ErrClosed.
type closedRTPReader struct {
	started chan struct{}
	closed  chan struct{}
}

func newClosedRTPReader() *closedRTPReader {
	return &closedRTPReader{started: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (r *closedRTPReader) ReadRTP([]byte, *rtp.Packet) (int, error) {
	r.started <- struct{}{}
	<-r.closed
	return 0, net.ErrClosed
}

func waitReadStarted(t *testing.T, r *closedRTPReader) {
	t.Helper()
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not move to the new reader")
	}
}

// TestRTPPacketReaderFollowsReplacedReaders pins that a Read blocked while its
// reader is replaced continues on the new one, however many replacements
// happen before a packet arrives. Two re-INVITEs that each move the media to a
// new socket close the old one under the read twice; the application must
// read on, not see the stream end.
func TestRTPPacketReaderFollowsReplacedReaders(t *testing.T) {
	first := newClosedRTPReader()
	second := newClosedRTPReader()
	payload := []byte{1, 2, 3, 4}
	last := &sliceRTPReader{packets: []rtp.Packet{{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 1, SSRC: 1234},
		Payload: payload,
	}}}

	r := NewRTPPacketReader(first, CodecAudioUlaw)
	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, RTPBufSize)
	read := make(chan readResult, 1)
	go func() {
		n, err := r.Read(buf)
		read <- readResult{n: n, err: err}
	}()

	waitReadStarted(t, first)
	r.UpdateReader(second)
	close(first.closed)

	waitReadStarted(t, second)
	r.UpdateReader(last)
	close(second.closed)

	select {
	case res := <-read:
		require.NoError(t, res.err, "a replaced reader must not end the stream")
		assert.Equal(t, payload, buf[:res.n])
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not return")
	}
}

// TestRTPPacketReaderUpdateLeavesSharedSocketReadable pins that replacing the
// reader with one on the same socket, while no read is blocked, leaves the
// next read working. A fork of a media session shares its socket, so the read
// deadline that ends a read waiting on the old reader is on the new reader's
// socket too, where no read was there to take it: the next read must return
// the packet and not that deadline.
func TestRTPPacketReaderUpdateLeavesSharedSocketReadable(t *testing.T) {
	sess := &MediaSession{
		Codecs: []Codec{CodecAudioUlaw},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
	}
	require.NoError(t, sess.createListeners(&sess.Laddr))
	t.Cleanup(func() { _ = sess.Close() })

	r := NewRTPPacketReader(sess, CodecAudioUlaw)
	r.UpdateReader(sess.Fork())

	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	payload := []byte{1, 2, 3, 4}
	raw, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 1, SSRC: 1234},
		Payload: payload,
	}).Marshal()
	require.NoError(t, err)
	_, err = peer.WriteTo(raw, sess.rtpConn.LocalAddr())
	require.NoError(t, err)

	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, RTPBufSize)
	read := make(chan readResult, 1)
	go func() {
		n, err := r.Read(buf)
		read <- readResult{n: n, err: err}
	}()
	select {
	case res := <-read:
		require.NoError(t, res.err)
		assert.Equal(t, payload, buf[:res.n])
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not return")
	}
}

// readStartPacketConn is an RTP socket that reports each read started on it,
// so a test replaces a session only once the packet reader's read has reached
// that session's socket.
type readStartPacketConn struct {
	net.PacketConn
	started chan struct{}
}

func (c *readStartPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	select {
	case c.started <- struct{}{}:
	default:
	}
	return c.PacketConn.ReadFrom(b)
}

// newReadStartTestSession returns a session on sockets of its own, and the
// RTP socket that reports the reads started on it.
func newReadStartTestSession(t *testing.T) (*MediaSession, *readStartPacketConn) {
	t.Helper()
	rtpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	rtcpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	conn := &readStartPacketConn{PacketConn: rtpConn, started: make(chan struct{}, 1)}
	s := &MediaSession{Codecs: []Codec{CodecAudioUlaw}}
	s.InitWithListeners(conn, rtcpConn, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000})
	t.Cleanup(func() {
		// The test closes the sessions it replaces itself.
		if err := s.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			t.Errorf("closing the session: %v", err)
		}
	})
	return s, conn
}

// TestRTPPacketReaderFollowsSessionsMovedToOtherSockets pins, on sessions and
// sockets as a dialog has them, that a Read waiting on a session goes on
// through two media updates that each move the media to sockets of their own
// and close the ones replaced. The read ends on a replaced socket twice, by
// the move and by the close, and neither may reach the application: it reads
// the packet that arrives on the last socket.
func TestRTPPacketReaderFollowsSessionsMovedToOtherSockets(t *testing.T) {
	waitReadOn := func(t *testing.T, conn *readStartPacketConn) {
		t.Helper()
		select {
		case <-conn.started:
		case <-time.After(5 * time.Second):
			t.Fatal("the read did not reach the session in place")
		}
	}
	// replace moves reader from cur to next the way a media update does: the
	// deadlines, then the reader, then the replaced sockets are closed.
	replace := func(t *testing.T, reader *RTPPacketReader, cur, next *MediaSession) {
		t.Helper()
		require.NoError(t, next.CarryRTPDeadlines(cur))
		reader.UpdateRTPSession(NewRTPSession(next))
		require.NoError(t, cur.Close())
	}

	first, firstConn := newReadStartTestSession(t)
	second, secondConn := newReadStartTestSession(t)
	last, lastConn := newReadStartTestSession(t)

	reader := NewRTPPacketReaderSession(NewRTPSession(first))
	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, RTPBufSize)
	read := make(chan readResult, 1)
	go func() {
		n, err := reader.Read(buf)
		read <- readResult{n: n, err: err}
	}()

	waitReadOn(t, firstConn)
	replace(t, reader, first, second)

	waitReadOn(t, secondConn)
	replace(t, reader, second, last)

	waitReadOn(t, lastConn)
	payload := []byte{1, 2, 3, 4}
	raw, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: CodecAudioUlaw.PayloadType, SequenceNumber: 1, SSRC: 1234},
		Payload: payload,
	}).Marshal()
	require.NoError(t, err)
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	_, err = peer.WriteTo(raw, last.rtpConn.LocalAddr())
	require.NoError(t, err)

	select {
	case res := <-read:
		require.NoError(t, res.err, "a replaced session must not end the stream")
		assert.Equal(t, payload, buf[:res.n])
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not return")
	}
}
