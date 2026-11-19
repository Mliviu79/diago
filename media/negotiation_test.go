// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func portBody(port string) []byte {
	return negotiateBody(
		"v=0",
		"o=- 1 1 IN IP4 198.51.100.7",
		"s=-",
		"c=IN IP4 198.51.100.7",
		"t=0 0",
		"m=audio "+port+" RTP/AVP 8",
		"a=rtpmap:8 PCMA/8000",
	)
}

// A port of zero is refused whichever side sent it: in an offer the stream
// "MUST NOT be used" (RFC 3264 sections 5.1 and 8.2), in an answer it is
// rejected and no media or RTCP may be generated for it (section 6). A port
// above 65535 names no UDP port. Neither may become the destination of RTP, nor
// the port above it that of RTCP.
func TestRemoteSDPRefusesUnusablePortInOfferAndAnswer(t *testing.T) {
	for _, tc := range []struct {
		name    string
		port    string
		offered bool
	}{
		{name: "zero_in_offer", port: "0", offered: false},
		{name: "zero_in_answer", port: "0", offered: true},
		{name: "above_range_in_offer", port: "65536", offered: false},
		{name: "above_range_in_answer", port: "65536", offered: true},
		{name: "negative_in_offer", port: "-1", offered: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := negotiateSession(CodecAudioAlaw)

			err := m.RemoteSDP(portBody(tc.port), tc.offered)

			require.ErrorIs(t, err, ErrNoCommonMedia)
			assert.Equal(t, net.UDPAddr{}, m.Raddr, "an unusable port must not become the RTP destination")
			assert.Equal(t, net.UDPAddr{}, m.rtcpRaddr, "an unusable port must not become the RTCP destination")
		})
	}
}

// The refusal ends at the bounds of the UDP port range: the lowest and the
// highest port a datagram can be sent to are still taken as the destination.
func TestRemoteSDPTakesTheWholeUDPPortRange(t *testing.T) {
	for _, tc := range []struct {
		name string
		port string
		want int
	}{
		{name: "lowest", port: "1", want: 1},
		{name: "highest", port: "65535", want: 65535},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := negotiateSession(CodecAudioAlaw)

			require.NoError(t, m.RemoteSDP(portBody(tc.port), false))

			assert.Equal(t, net.UDPAddr{IP: net.IPv4(198, 51, 100, 7).To4(), Port: tc.want}, m.Raddr)
		})
	}
}
