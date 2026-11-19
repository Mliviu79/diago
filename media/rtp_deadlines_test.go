// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// newDeadlineTestSession returns a session on sockets from the OS rather than
// from Init, whose port range another test in this package narrows for good.
func newDeadlineTestSession(t *testing.T) *MediaSession {
	t.Helper()
	rtpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	rtcpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	s := &MediaSession{Codecs: []Codec{CodecAudioUlaw}}
	s.InitWithListeners(rtpConn, rtcpConn, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000})
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestMediaSessionCarryRTPDeadlines pins that a session replacing another
// takes over the RTP read and write deadlines StopRTP and StartRTP last set on
// it, and sets them on its own socket when it has one of its own, so a
// deadline that was to end a read or a write ends the one carried over to the
// new socket. A fork on the same socket takes them over as its own, for the
// session that later replaces it.
func TestMediaSessionCarryRTPDeadlines(t *testing.T) {
	requireDeadlinesPassed := func(t *testing.T, s *MediaSession) {
		t.Helper()
		_, _, err := s.rtpConn.ReadFrom(make([]byte, RTPBufSize))
		require.True(t, os.IsTimeout(err), "the read deadline was not carried: %v", err)
		_, err = s.rtpConn.WriteTo([]byte{0}, &s.Laddr)
		require.True(t, os.IsTimeout(err), "the write deadline was not carried: %v", err)
	}

	old := newDeadlineTestSession(t)
	require.NoError(t, old.StopRTP(1, 0))
	require.NoError(t, old.StopRTP(2, 0))

	moved := newDeadlineTestSession(t)
	require.NoError(t, moved.CarryRTPDeadlines(old))
	requireDeadlinesPassed(t, moved)

	// A re-INVITE that keeps the socket, and then one that moves it.
	fork := old.Fork()
	require.NoError(t, fork.CarryRTPDeadlines(old))
	next := newDeadlineTestSession(t)
	require.NoError(t, next.CarryRTPDeadlines(fork))
	requireDeadlinesPassed(t, next)

	// Cleared deadlines are carried as cleared.
	require.NoError(t, fork.StartRTP(0))
	cleared := newDeadlineTestSession(t)
	require.NoError(t, cleared.StopRTP(0, 0))
	require.NoError(t, cleared.CarryRTPDeadlines(fork))
	_, err := cleared.rtpConn.WriteTo([]byte{0}, &cleared.Laddr)
	require.NoError(t, err, "the cleared write deadline was not carried")
}

// TestRTPPacketReaderSwapKeepsStopDeadline pins that moving the packet reader
// to the session that replaces the one it reads leaves a read deadline a
// caller set with StopRTP in force. The move ends a waiting read with a
// deadline of its own and the read is taken up again on the new session. It
// was taken up with no deadline at all, so a caller that had stopped its reads
// found them waiting again, for a packet that may never come.
func TestRTPPacketReaderSwapKeepsStopDeadline(t *testing.T) {
	// replace moves reader to a session that replaces cur the way a media
	// update does: the deadlines first, then the reader.
	replace := func(t *testing.T, reader *RTPPacketReader, cur, next *MediaSession) {
		t.Helper()
		require.NoError(t, next.CarryRTPDeadlines(cur))
		reader.UpdateRTPSession(NewRTPSession(next))
	}
	read := func(reader *RTPPacketReader) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := reader.Read(make([]byte, RTPBufSize))
			done <- err
		}()
		return done
	}
	requireDeadline := func(t *testing.T, done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			require.ErrorIs(t, err, os.ErrDeadlineExceeded)
		case <-time.After(5 * time.Second):
			t.Fatal("the read went on without the deadline StopRTP set")
		}
	}

	replacements := map[string]func(t *testing.T, cur *MediaSession) *MediaSession{
		"SameSocket":  func(_ *testing.T, cur *MediaSession) *MediaSession { return cur.Fork() },
		"OtherSocket": func(t *testing.T, _ *MediaSession) *MediaSession { return newDeadlineTestSession(t) },
	}
	for name, replacement := range replacements {
		t.Run(name, func(t *testing.T) {
			t.Run("StoppedBeforeTheSwap", func(t *testing.T) {
				sess := newDeadlineTestSession(t)
				reader := NewRTPPacketReaderSession(NewRTPSession(sess))
				require.NoError(t, sess.StopRTP(1, 0))
				replace(t, reader, sess, replacement(t, sess))
				requireDeadline(t, read(reader))
			})

			t.Run("SwappedWhileWaiting", func(t *testing.T) {
				sess := newDeadlineTestSession(t)
				reader := NewRTPPacketReaderSession(NewRTPSession(sess))
				require.NoError(t, sess.StopRTP(1, 300*time.Millisecond))
				done := read(reader)
				replace(t, reader, sess, replacement(t, sess))
				requireDeadline(t, done)
			})

			t.Run("ClearedAfterTheSwap", func(t *testing.T) {
				sess := newDeadlineTestSession(t)
				reader := NewRTPPacketReaderSession(NewRTPSession(sess))
				require.NoError(t, sess.StopRTP(1, 0))
				next := replacement(t, sess)
				replace(t, reader, sess, next)
				requireDeadline(t, read(reader))

				// Cleared on the session in place, the reads go on.
				require.NoError(t, next.StartRTP(1))
				done := read(reader)
				raw, err := (&rtp.Packet{
					Header:  rtp.Header{Version: 2, PayloadType: CodecAudioUlaw.PayloadType, SequenceNumber: 1, SSRC: 1234},
					Payload: make([]byte, 160),
				}).Marshal()
				require.NoError(t, err)
				peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
				require.NoError(t, err)
				t.Cleanup(func() { _ = peer.Close() })
				_, err = peer.WriteTo(raw, next.rtpConn.LocalAddr())
				require.NoError(t, err)
				select {
				case err := <-done:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("the read did not go on once the deadline was cleared")
				}
			})
		})
	}
}

// TestRTPDeadlinesSharedOnOneSocket pins that sessions on one socket record
// its deadlines in one place once the deadlines were carried from one to the
// other. A caller that kept the session it was given sets and clears the
// socket's read deadline through it after a media update has replaced it. The
// packet reader, moved again by a later update, then puts back what that
// caller asked for last, and not what stood when the first update came.
func TestRTPDeadlinesSharedOnOneSocket(t *testing.T) {
	read := func(reader *RTPPacketReader) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := reader.Read(make([]byte, RTPBufSize))
			done <- err
		}()
		return done
	}
	within := func(t *testing.T, done <-chan error) error {
		t.Helper()
		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			t.Fatal("the read did not return")
			return nil
		}
	}
	// replace moves reader to a fork of cur, the way a media update that
	// keeps the socket does, and returns the fork.
	replace := func(t *testing.T, reader *RTPPacketReader, cur *MediaSession) *MediaSession {
		t.Helper()
		next := cur.Fork()
		require.NoError(t, next.CarryRTPDeadlines(cur))
		reader.UpdateRTPSession(NewRTPSession(next))
		return next
	}

	t.Run("SetThroughTheReplacedSession", func(t *testing.T) {
		kept := newDeadlineTestSession(t)
		reader := NewRTPPacketReaderSession(NewRTPSession(kept))
		replace(t, reader, kept)

		require.NoError(t, kept.StopRTP(1, 0))
		require.ErrorIs(t, within(t, read(reader)), os.ErrDeadlineExceeded)
	})

	t.Run("ClearedThroughTheReplacedSession", func(t *testing.T) {
		kept := newDeadlineTestSession(t)
		reader := NewRTPPacketReaderSession(NewRTPSession(kept))
		require.NoError(t, kept.StopRTP(1, 0))
		next := replace(t, reader, kept)
		require.ErrorIs(t, within(t, read(reader)), os.ErrDeadlineExceeded)

		require.NoError(t, kept.StartRTP(1))
		last := replace(t, reader, next)

		done := read(reader)
		raw, err := (&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: CodecAudioUlaw.PayloadType, SequenceNumber: 1, SSRC: 1234},
			Payload: make([]byte, 160),
		}).Marshal()
		require.NoError(t, err)
		peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		require.NoError(t, err)
		t.Cleanup(func() { _ = peer.Close() })
		_, err = peer.WriteTo(raw, last.rtpConn.LocalAddr())
		require.NoError(t, err)
		require.NoError(t, within(t, done), "the read was ended by a deadline that had been cleared")
	})
}
