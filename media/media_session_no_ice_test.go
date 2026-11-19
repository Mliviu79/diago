// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newBindSession builds a session far enough to exercise the bind step only.
// It deliberately does not run Init: these are bind side invariants.
func newBindSession(t *testing.T, secureRTP int) *MediaSession {
	t.Helper()
	s := &MediaSession{
		Codecs:    []Codec{CodecAudioUlaw},
		Mode:      sdp.ModeSendrecv,
		SecureRTP: secureRTP,
	}
	s.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
	return s
}

// requireUDPAddrOf returns the UDP address conn is bound to.
func requireUDPAddrOf(t *testing.T, conn net.PacketConn) *net.UDPAddr {
	t.Helper()
	require.NotNil(t, conn)
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	require.True(t, ok, "LocalAddr() = %T, want *net.UDPAddr", conn.LocalAddr())
	return addr
}

// TestNonICEBindsTwoSockets guards the established layout: without ICE the
// session still binds RTP and RTCP on adjacent ports.
func TestNonICEBindsTwoSockets(t *testing.T) {
	s := newBindSession(t, SecureRTPModeNone)

	require.NoError(t, s.createListeners(&s.Laddr))
	t.Cleanup(func() { _ = s.Close() })

	rtpAddr := requireUDPAddrOf(t, s.rtpConn)
	rtcpAddr := requireUDPAddrOf(t, s.rtcpConn)
	assert.Equal(t, rtpAddr.Port+1, rtcpAddr.Port, "RTCP stays on the RTP port + 1")
	assert.Equal(t, rtpAddr.Port, s.Laddr.Port, "the bound port is resolved onto Laddr")
}

// TestICEDisabledWithoutDTLS pins that the security mode of a session of this
// type never changes its socket layout. ICE runs on a WebRTC media session
// only, so a plain RTP, an SDES and a DTLS session each keep their RTP and
// their RTCP socket: nothing in their SDP would carry candidates.
func TestICEDisabledWithoutDTLS(t *testing.T) {
	for _, mode := range []int{SecureRTPModeNone, SecureRTPModeSDES, SecureRTPModeDTLS} {
		s := newBindSession(t, mode)

		require.NoError(t, s.createListeners(&s.Laddr))
		assert.NotNil(t, s.rtpConn, "SecureRTP=%d keeps its RTP socket", mode)
		assert.NotNil(t, s.rtcpConn, "SecureRTP=%d keeps its RTCP socket", mode)
		require.NoError(t, s.Close())
	}
}

// TestGenerateSDPWithoutICEHasNoICEAttrs pins that a plain session does not
// leak ICE or rtcp-mux attributes into its offer. A UDP trunk neither
// understands them nor wants the extra datagram size.
func TestGenerateSDPWithoutICEHasNoICEAttrs(t *testing.T) {
	out := string(generateSDPForAudio(1, 1, "RTP/AVP",
		net.IPv4(192, 0, 2, 1), net.IPv4(192, 0, 2, 1), 10000,
		sdp.ModeSendrecv, []Codec{CodecAudioUlaw}, sdesInline{}, nil))

	assert.NotContains(t, out, "a=ice-ufrag")
	assert.NotContains(t, out, "a=ice-pwd")
	assert.NotContains(t, out, "a=candidate")
	assert.NotContains(t, out, "a=rtcp-mux")
}
