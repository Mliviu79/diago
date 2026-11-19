// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestRTPJitterBufferUpdateRTPSessionEndsRead pins that moving the buffer onto
// another RTP session ends the read its read loop has in progress on the
// session replaced, so the loop reads the new session from then on. Left to
// finish, that read returns only with a packet the replaced session takes for
// its own: it skips a payload type it did not negotiate and waits for the
// next packet, so after a move to another codec it never returned, and on a
// socket of its own it waited for a packet the peer no longer sends there.
// The deadline that ends the read must not end the reads that follow it on a
// socket both sessions share.
func TestRTPJitterBufferUpdateRTPSessionEndsRead(t *testing.T) {
	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	send := func(t *testing.T, sess *MediaSession, payloadType uint8, seq uint16) {
		t.Helper()
		pkt := rtpPacket(1234, seq)
		pkt.PayloadType = payloadType
		raw, err := pkt.Marshal()
		require.NoError(t, err)
		_, err = peer.WriteTo(raw, sess.rtpConn.LocalAddr())
		require.NoError(t, err)
	}
	newBuffer := func(sess *MediaSession) *RTPJitterBuffer {
		jb := NewRTPJitterBuffer(NewRTPSession(sess), time.Millisecond, RTPJitterBufferOptions{
			DelayPackets: 1,
			MaxPackets:   4,
		})
		t.Cleanup(func() { _ = jb.Close() })
		return jb
	}
	// alawFork is a fork of sess, on its socket, that takes PCMA where sess
	// takes PCMU.
	alawFork := func(sess *MediaSession) *MediaSession {
		fork := sess.Fork()
		fork.Codecs = []Codec{CodecAudioAlaw}
		return fork
	}

	t.Run("OtherPayloadType", func(t *testing.T) {
		sess := newDeadlineTestSession(t)
		jb := newBuffer(sess)
		send(t, sess, CodecAudioUlaw.PayloadType, 0)
		require.Equal(t, uint16(0), readJitterSeq(t, jb))

		fork := alawFork(sess)
		jb.UpdateRTPSession(NewRTPSession(fork))
		for seq := uint16(1); seq < 5; seq++ {
			send(t, fork, CodecAudioAlaw.PayloadType, seq)
			require.Equal(t, seq, readJitterSeq(t, jb))
		}
	})

	t.Run("OtherSocket", func(t *testing.T) {
		sess := newDeadlineTestSession(t)
		jb := newBuffer(sess)
		send(t, sess, CodecAudioUlaw.PayloadType, 0)
		require.Equal(t, uint16(0), readJitterSeq(t, jb))

		// The socket replaced stays open, and nothing is sent to it.
		next := newDeadlineTestSession(t)
		jb.UpdateRTPSession(NewRTPSession(next))
		for seq := uint16(1); seq < 5; seq++ {
			send(t, next, CodecAudioUlaw.PayloadType, seq)
			require.Equal(t, seq, readJitterSeq(t, jb))
		}
	})

	t.Run("BeforeTheFirstRead", func(t *testing.T) {
		// No read is in progress, so the deadline of the move meets the first
		// read of the session in place, on the socket the two share.
		sess := newDeadlineTestSession(t)
		jb := newBuffer(sess)
		fork := alawFork(sess)
		jb.UpdateRTPSession(NewRTPSession(fork))
		for seq := uint16(0); seq < 4; seq++ {
			send(t, fork, CodecAudioAlaw.PayloadType, seq)
			require.Equal(t, seq, readJitterSeq(t, jb))
		}
	})

	t.Run("KeepsStop", func(t *testing.T) {
		// A stop of the session's reads set before the move ends the read
		// that follows the move, as it would have ended one without it.
		sess := newDeadlineTestSession(t)
		jb := newBuffer(sess)
		require.NoError(t, sess.StopRTP(1, 0))
		fork := alawFork(sess)
		require.NoError(t, fork.CarryRTPDeadlines(sess))
		jb.UpdateRTPSession(NewRTPSession(fork))
		send(t, fork, CodecAudioAlaw.PayloadType, 0)

		r := awaitJitterRead(t, jb, readJitterAsync(jb, make([]byte, RTPBufSize)))
		require.ErrorIs(t, r.err, os.ErrDeadlineExceeded)
	})
}
