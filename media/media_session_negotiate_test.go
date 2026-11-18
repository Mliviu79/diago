// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func negotiateBody(lines ...string) []byte {
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

func negotiateSession(codecs ...Codec) *MediaSession {
	return &MediaSession{
		Codecs: codecs,
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5004},
	}
}

// Port 0 means the stream is rejected (RFC 3264 section 6), and it is also what
// an unparseable port leaves behind. Latching it answered the call and then sat
// mute.
func TestRemoteSDPRejectsUnusableDestination(t *testing.T) {
	for _, tc := range []struct{ name, mline, cline string }{
		{"rejected stream", "m=audio 0 RTP/AVP 8", "c=IN IP4 198.51.100.7"},
		{"unparseable port", "m=audio notaport RTP/AVP 8", "c=IN IP4 198.51.100.7"},
		{"non IP address", "m=audio 5004 RTP/AVP 8", "c=IN FQDN host.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := negotiateSession(CodecAudioAlaw)
			err := m.RemoteSDP(negotiateBody(
				"v=0",
				"o=- 1 1 IN IP4 198.51.100.7",
				"s=-",
				tc.cline,
				"t=0 0",
				tc.mline,
				"a=rtpmap:8 PCMA/8000",
			), false)
			require.ErrorIs(t, err, ErrNoCommonMedia)
			assert.Zero(t, m.Raddr.Port, "an unusable destination must not be latched")
		})
	}
}

// A peer offering codecs we cannot meet is the peer's problem and should be
// classified as such (488), not counted as an internal error.
func TestRemoteSDPClassifiesNegotiationFailures(t *testing.T) {
	tests := []struct {
		name  string
		mline string
		want  error
	}{
		{"no common codec", "m=audio 5004 RTP/AVP 9", ErrNoCommonCodec},
		{"unsupported transport", "m=audio 5004 RTP/FOO 8", ErrNoCommonMedia},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := negotiateSession(CodecAudioAlaw)
			err := m.RemoteSDP(negotiateBody(
				"v=0",
				"o=- 1 1 IN IP4 198.51.100.7",
				"s=-",
				"c=IN IP4 198.51.100.7",
				"t=0 0",
				tc.mline,
			), false)
			require.ErrorIs(t, err, tc.want)
		})
	}
}
