// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/stretchr/testify/require"
)

// TestMediaSessionInitWithListenersLaddr pins that a session built over
// sockets the caller bound takes its local address from the RTP socket, which
// is what LocalSDP advertises on the m= line and where the peer sends RTP. The
// RTCP socket is at the next port up by convention (RFC 3550 section 11), so
// taking the address from it advertised a port that carries no RTP.
func TestMediaSessionInitWithListenersLaddr(t *testing.T) {
	lRTP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lRTP.Close() })
	lRTCP, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = lRTCP.Close() })

	rtpPort := lRTP.LocalAddr().(*net.UDPAddr).Port
	s := &MediaSession{Codecs: []Codec{CodecAudioUlaw}}
	s.InitWithListeners(lRTP, lRTCP, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40000})

	require.Equal(t, rtpPort, s.Laddr.Port)
	require.True(t, s.Laddr.IP.Equal(net.IPv4(127, 0, 0, 1)))

	sd := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(s.LocalSDP(false), &sd))
	md, err := sd.MediaDescription("audio")
	require.NoError(t, err)
	require.Equal(t, rtpPort, md.Port, "the SDP must advertise the RTP socket")
}
