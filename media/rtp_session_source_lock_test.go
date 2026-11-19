// SPDX-License-Identifier: MPL-2.0

package media

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/fakes"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRTPSessionForkSourceLock pins that a fork keeps the RTP source it learned
// only while it runs on the same socket toward the same peer. A re-INVITE that
// moves the peer forks the session toward the new address, and the peer's
// packets then come from there: the fork has to learn that source, and a lock
// carried over from the old one refuses every packet the peer sends.
func TestRTPSessionForkSourceLock(t *testing.T) {
	oldPeer := net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 4000}
	newPeer := net.UDPAddr{IP: net.IPv4(10, 0, 0, 2), Port: 5000}

	// readFrom hands the session one packet from source and reports whether
	// the session accepted it. A refused packet leaves the reader dry, so the
	// read then ends with io.EOF.
	readFrom := func(t *testing.T, rtpSess *RTPSession, conn *fakes.UDPConn, source net.UDPAddr, ssrc uint32, seq uint16) bool {
		t.Helper()
		raw, err := (&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: CodecAudioUlaw.PayloadType, SequenceNumber: seq, SSRC: ssrc},
			Payload: []byte{1, 2, 3},
		}).Marshal()
		require.NoError(t, err)
		conn.ExpectAddr(source)
		conn.Reader = bytes.NewReader(raw)

		_, err = rtpSess.ReadRTP(make([]byte, RTPBufSize), &rtp.Packet{})
		if errors.Is(err, io.EOF) {
			return false
		}
		require.NoError(t, err)
		return true
	}

	// lockedSession is a session that has learned oldPeer as its source.
	lockedSession := func(t *testing.T) (*RTPSession, *fakes.UDPConn) {
		t.Helper()
		conn := &fakes.UDPConn{}
		sess := &MediaSession{
			Codecs:  []Codec{CodecAudioUlaw},
			Mode:    sdp.ModeSendrecv,
			Laddr:   net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
			rtpConn: conn,
		}
		sess.SetRemoteAddr(&oldPeer)
		rtpSess := NewRTPSession(sess)
		rtpSess.sourceLock = true

		for seq := uint16(1); seq < 4; seq++ {
			require.False(t, readFrom(t, rtpSess, conn, oldPeer, 1111, seq), "packet %d is still learning", seq)
		}
		require.True(t, readFrom(t, rtpSess, conn, oldPeer, 1111, 4), "the fourth packet in sequence locks the source")
		return rtpSess, conn
	}

	t.Run("fork toward a moved peer learns its source", func(t *testing.T) {
		rtpSess, conn := lockedSession(t)
		candidate := rtpSess.Sess.Fork()
		candidate.SetRemoteAddr(&newPeer)
		fork := rtpSess.Fork(candidate)

		// The moved peer's stream has a sequence of its own.
		accepted := 0
		for seq := uint16(30000); seq < 30010; seq++ {
			if readFrom(t, fork, conn, newPeer, 2222, seq) {
				accepted++
			}
		}
		assert.Equal(t, 7, accepted, "the new source is learned from four packets in sequence and accepted after")
	})

	t.Run("fork toward the same peer keeps the lock", func(t *testing.T) {
		rtpSess, conn := lockedSession(t)
		candidate := rtpSess.Sess.Fork()
		candidate.SetRemoteAddr(&oldPeer)
		fork := rtpSess.Fork(candidate)

		assert.True(t, readFrom(t, fork, conn, oldPeer, 1111, 5), "the locked source is accepted at once")
		for seq := uint16(30000); seq < 30010; seq++ {
			assert.False(t, readFrom(t, fork, conn, newPeer, 2222, seq), "another source stays refused")
		}
	})
}

// TestRTPSourceLockInSequence pins what a session that has read nothing counts
// while it learns its source. It takes the source of the fourth packet,
// whatever the order of the four, and with RTPSourceLockInSequence only the
// source of the fourth packet in sequence with the three before it, a packet
// out of sequence starting the count again.
func TestRTPSourceLockInSequence(t *testing.T) {
	peer := net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 4000}
	// The second and third packets arrive in each other's place.
	arrivals := []uint16{10, 12, 11, 13, 14, 15, 16, 17}

	for _, tc := range []struct {
		name       string
		inSequence bool
		read       []bool
	}{
		{name: "any_four", inSequence: false, read: []bool{false, false, false, true, true, true, true, true}},
		{name: "four_in_sequence", inSequence: true, read: []bool{false, false, false, false, false, false, true, true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prev := RTPSourceLockInSequence
			RTPSourceLockInSequence = tc.inSequence
			t.Cleanup(func() { RTPSourceLockInSequence = prev })

			sess := &MediaSession{Codecs: []Codec{CodecAudioUlaw}, Mode: sdp.ModeSendrecv}
			sess.SetRemoteAddr(&peer)
			rtpSess := NewRTPSession(sess)

			var read []bool
			for _, seq := range arrivals {
				pkt := &rtp.Packet{Header: rtp.Header{Version: 2, SequenceNumber: seq, SSRC: 1111}}
				read = append(read, rtpSess.sourceLockProtection(pkt, &peer))
			}
			assert.Equal(t, tc.read, read)
		})
	}
}
