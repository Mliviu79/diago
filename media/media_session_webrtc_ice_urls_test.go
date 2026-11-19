// SPDX-License-Identifier: MPL-2.0

package media

import (
	"crypto/tls"
	"testing"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUsableICEURLs pins that the URLs the ICE agent can take are returned in
// their order, and that an entry it cannot take is left out without taking the
// others with it.
func TestUsableICEURLs(t *testing.T) {
	for _, tc := range []struct {
		name string
		urls []string
		want []string
	}{
		{
			name: "every_URL_usable",
			urls: []string{"stun:stun.example.org:3478", "stun:stun.example.org", "turn:turn.example.org:3478?transport=udp", "turns:turn.example.org"},
			want: []string{"stun:stun.example.org:3478", "stun:stun.example.org", "turn:turn.example.org:3478?transport=udp", "turns:turn.example.org"},
		},
		{
			name: "no_scheme",
			urls: []string{"stun:a.example.org:3478", "stun.example.org:3478", "stun:b.example.org:3478"},
			want: []string{"stun:a.example.org:3478", "stun:b.example.org:3478"},
		},
		{
			name: "scheme_of_another_protocol",
			urls: []string{"http://stun.example.org", "stun:a.example.org:3478"},
			want: []string{"stun:a.example.org:3478"},
		},
		{
			name: "port_that_is_no_number",
			urls: []string{"stun:a.example.org:3478", "stun:stun.example.org:port"},
			want: []string{"stun:a.example.org:3478"},
		},
		{
			name: "no_host",
			urls: []string{"stun:", "turn:turn.example.org"},
			want: []string{"turn:turn.example.org"},
		},
		{
			name: "none_usable",
			urls: []string{"stun.example.org:3478", ""},
		},
		{
			name: "no_URL",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, UsableICEURLs(tc.urls))
		})
	}
}

// TestWebrtcInitWithUsableICEURLs pins what the helper is for: Init refuses a
// configuration for one URL it cannot parse, and takes the same list once it
// has been through UsableICEURLs, the session then gathering its host
// candidates.
func TestWebrtcInitWithUsableICEURLs(t *testing.T) {
	urls := []string{"stun.example.org:3478"}
	conf := func(iceURLs []string) MediaSessionWebrtcConfig {
		return MediaSessionWebrtcConfig{
			ICEURLs:         iceURLs,
			DTLS:            DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
			IPFamilies:      []ICEIPFamily{ICEIPFamilyIPv4},
			IncludeLoopback: true,
			InterfaceFilter: testLoopbackInterface,
		}
	}

	t.Run("as_written", func(t *testing.T) {
		m := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}}
		err := m.Init(t.Context(), conf(urls))
		require.ErrorIs(t, err, ErrICEGathering)
	})

	t.Run("through_UsableICEURLs", func(t *testing.T) {
		ctx := t.Context()
		m := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}}
		require.NoError(t, m.Init(ctx, conf(UsableICEURLs(urls))))
		closeWebrtcWithTest(t, m)

		offer, err := m.LocalSDP(ctx, false)
		require.NoError(t, err)
		assert.NotEmpty(t, sdpLines(offer, "a=candidate:"), "the offer\n%s", offer)
	})
}
