// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/emiago/diago/testdata"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/rtcp"
	"github.com/stretchr/testify/require"
)

// sentCountingConn counts the datagrams written to a media socket.
type sentCountingConn struct {
	net.PacketConn
	sent atomic.Int64
}

func (c *sentCountingConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.sent.Add(1)
	return c.PacketConn.WriteTo(p, addr)
}

// countSentMedia puts a counter in front of both sockets of s, so that a test
// can tell that a write was refused before anything left. The DTLS handshake
// keeps the socket RemoteSDP lent it, so its flights are not counted.
func countSentMedia(s *MediaSession) (rtpSent, rtcpSent *atomic.Int64) {
	rtpConn := &sentCountingConn{PacketConn: s.rtpConn}
	rtcpConn := &sentCountingConn{PacketConn: s.rtcpConn}
	s.rtpConn, s.rtcpConn = rtpConn, rtcpConn
	return &rtpConn.sent, &rtcpConn.sent
}

// requireNoMediaSent requires every media write of s to be refused with
// ErrDTLSNotKeyed before a datagram leaves either socket.
func requireNoMediaSent(t *testing.T, s *MediaSession, rtpSent, rtcpSent *atomic.Int64) {
	t.Helper()
	report := &rtcp.ReceiverReport{SSRC: 0x5eed0000}
	require.ErrorIs(t, s.WriteRTP(dtlsTestPacket(1)), ErrDTLSNotKeyed)
	require.ErrorIs(t, s.WriteRTCP(report), ErrDTLSNotKeyed)
	require.ErrorIs(t, s.WriteRTCPs([]rtcp.Packet{report}), ErrDTLSNotKeyed)
	require.Zero(t, rtpSent.Load(), "RTP left a session that has no keys")
	require.Zero(t, rtcpSent.Load(), "RTCP left a session that has no keys")
}

// TestDTLSUnkeyedSessionSendsNoMedia pins that a DTLS-SRTP session sends no
// media before its handshake has keyed it. Its media is SRTP, which "MUST NOT
// take place before the DTLS handshake completes" (RFC 5764 section 5.1), and
// a packet written without keys would carry the payload in the clear. WriteRTP,
// WriteRTCP and WriteRTCPs return ErrDTLSNotKeyed instead, on both sides of
// the exchange, and write once the handshake has completed.
func TestDTLSUnkeyedSessionSendsNoMedia(t *testing.T) {
	offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
	negotiateDTLS(t, offerer, answerer, nil)

	for name, s := range map[string]*MediaSession{"offerer": offerer, "answerer": answerer} {
		t.Run(name, func(t *testing.T) {
			rtpSent, rtcpSent := countSentMedia(s)
			requireNoMediaSent(t, s, rtpSent, rtcpSent)
		})
	}

	finalizeBoth(t, offerer, answerer)
	requireMediaDelivered(t, offerer, answerer, 81)
	requireMediaDelivered(t, answerer, offerer, 82)
	require.NoError(t, answerer.WriteRTCP(&rtcp.ReceiverReport{SSRC: 0x5eed0000}))
}

// TestDTLSForkNewAssociationSendsNoMediaBeforeItsHandshake pins that a fork
// which starts a new DTLS association sends nothing until its own handshake
// has keyed it, whoever holds it meanwhile: the keys of the association it was
// forked with are not the new one's, and it has no others yet. A fork put in
// place of the running session before its handshake would otherwise send the
// call's media unprotected from its new sockets. Once the handshake has run,
// the fork sends media the new peer decrypts, and the session it was forked
// from still carries the association it has.
func TestDTLSForkNewAssociationSendsNoMediaBeforeItsHandshake(t *testing.T) {
	offerer, answerer, _, _ := newEstablishedDTLSPair(t, nil)

	cert, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	newPeer := newDTLSForkTestSession(t, cert)

	fork := answerer.Fork()
	require.False(t, fork.awaitsDTLSKeys(), "a fork of a keyed session continues its association")
	negotiateDTLS(t, newPeer, fork, nil)
	t.Cleanup(func() { _ = fork.Close() })
	require.True(t, fork.FinalizePending())
	require.True(t, fork.awaitsDTLSKeys(), "a new association is keyed by its own handshake only")

	rtpSent, rtcpSent := countSentMedia(fork)
	requireNoMediaSent(t, fork, rtpSent, rtcpSent)

	finalizeBoth(t, newPeer, fork)
	requireMediaDelivered(t, fork, newPeer, 91)
	require.EqualValues(t, 1, rtpSent.Load())
	requireMediaDelivered(t, newPeer, fork, 92)
	requireMediaDelivered(t, offerer, answerer, 93)
	requireMediaDelivered(t, answerer, offerer, 94)
}

// TestDTLSRemoteSDPAgainKeepsKeyedSession pins what RemoteSDP does to a
// session its handshake has keyed already, when a body is applied to it again:
// the 2xx that repeats the answer of a provisional response is one. A body that
// continues the association arms no handshake and leaves the SRTP contexts the
// session reads and writes with in place: a second handshake would replace
// them under a reader, and the peer runs none. A body that asks for a new
// association on the transport of the current one is refused, and the session
// carries on as it was.
func TestDTLSRemoteSDPAgainKeepsKeyedSession(t *testing.T) {
	offerer, answerer, offer, answer := newEstablishedDTLSPair(t, nil)

	type keys struct {
		local  any
		remote any
		assoc  *dtlsAssociation
	}
	keysOf := func(s *MediaSession) keys {
		return keys{local: s.localCtxSRTP, remote: s.remoteCtxSRTP, assoc: s.dtlsAssoc}
	}
	requireUntouched := func(t *testing.T, s *MediaSession, want keys) {
		t.Helper()
		require.False(t, s.FinalizePending(), "no handshake may be armed on a session that is keyed")
		require.False(t, s.awaitsDTLSKeys())
		got := keysOf(s)
		require.Same(t, want.local, got.local)
		require.Same(t, want.remote, got.remote)
		require.Same(t, want.assoc, got.assoc)
	}

	t.Run("answer_again", func(t *testing.T) {
		want := keysOf(offerer)
		require.NoError(t, offerer.RemoteSDP(answer, true))
		requireUntouched(t, offerer, want)
		requireMediaDelivered(t, offerer, answerer, 101)
		requireMediaDelivered(t, answerer, offerer, 102)
	})

	t.Run("offer_again", func(t *testing.T) {
		want := keysOf(answerer)
		require.NoError(t, answerer.RemoteSDP(offer, false))
		requireUntouched(t, answerer, want)
		require.Equal(t, sdpLine(t, answer, "a=setup:"), sdpLine(t, answerer.LocalSDP(true), "a=setup:"))
		requireMediaDelivered(t, offerer, answerer, 103)
		requireMediaDelivered(t, answerer, offerer, 104)
	})

	t.Run("answer_asking_for_a_new_association", func(t *testing.T) {
		fp, err := dtlsSHA256Fingerprint(testdata.ClientCertificate())
		require.NoError(t, err)
		other := replaceSDPLine(t, string(answer), "a=fingerprint:", "a=fingerprint:SHA-256 "+fp)
		require.False(t, strings.EqualFold(other, string(answer)), "the answer must name another certificate")

		want := keysOf(offerer)
		require.Error(t, offerer.RemoteSDP([]byte(other), true))
		requireUntouched(t, offerer, want)
		requireMediaDelivered(t, offerer, answerer, 105)
		requireMediaDelivered(t, answerer, offerer, 106)
	})
}
