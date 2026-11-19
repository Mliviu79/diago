// SPDX-License-Identifier: MPL-2.0

package media

import (
	"crypto/tls"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// iceLiteTestConfig returns the configuration of one side of a WebRTC session
// on the loopback interface, with a lite agent when lite is set.
func iceLiteTestConfig(lite bool, cert tls.Certificate) MediaSessionWebrtcConfig {
	return MediaSessionWebrtcConfig{
		DTLS: DTLSConfig{
			Certificates:     []tls.Certificate{cert},
			ServerClientAuth: ServerClientAuthRequireCert,
		},
		ICE:             ICEConfig{Lite: lite},
		IPFamilies:      []ICEIPFamily{ICEIPFamilyIPv4},
		IncludeLoopback: true,
		InterfaceFilter: testLoopbackInterface,
	}
}

// newICELiteTestSession builds one side of a WebRTC session, with a lite
// agent when lite is set, and closes it when the test ends.
func newICELiteTestSession(t *testing.T, lite bool, cert tls.Certificate) *MediaSessionWebrtc {
	t.Helper()
	s := &MediaSessionWebrtc{
		Codecs: []Codec{CodecAudioUlaw},
		Mode:   sdp.ModeSendrecv,
	}
	require.NoError(t, s.Init(t.Context(), iceLiteTestConfig(lite, cert)))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// sdpSessionLevel returns the lines of body before its first m= line.
func sdpSessionLevel(body []byte) []string {
	var lines []string
	for l := range strings.SplitSeq(string(body), "\r\n") {
		if strings.HasPrefix(l, "m=") {
			break
		}
		lines = append(lines, l)
	}
	return lines
}

// requireICELiteLine requires body to carry a=ice-lite when lite is set, at
// session level and nowhere else, and not at all otherwise.
func requireICELiteLine(t *testing.T, lite bool, what string, body []byte) {
	t.Helper()
	assert.Equal(t, lite, slices.Contains(sdpSessionLevel(body), "a=ice-lite"), "%s:\n%s", what, body)
	wantLines := 0
	if lite {
		wantLines = 1
	}
	require.Equal(t, wantLines, strings.Count(string(body), "a=ice-lite"), "%s: a=ice-lite is session level only", what)
}

// iceControllingOf returns the role s takes in the connectivity checks of its
// ICE generation; offered says s made the offer.
func iceControllingOf(s *MediaSessionWebrtc, offered bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.iceControlling(offered)
}

// TestICELite negotiates an ICE lite agent against a full one, in both offer
// directions, and two lite agents, through connectivity checks, the DTLS
// handshake and SRTP. A lite agent gathers host candidates only and says so
// with a session level a=ice-lite (RFC 8839 section 4.2.1.4, section 5.3),
// which a full agent never writes. RFC 8445 section 6.1.1 makes the full agent
// controlling against a lite one whichever side offered, since the lite agent
// never sends checks, and the offerer controlling otherwise. A later offer on
// the same ICE generation says the same about the agent as the first
// (RFC 8839 section 4.4.1.1.1).
func TestICELite(t *testing.T) {
	cases := []struct {
		name                   string
		offererLite            bool
		answererLite           bool
		wantOffererControlling bool
	}{
		{name: "lite_answerer", answererLite: true, wantOffererControlling: true},
		{name: "lite_offerer", offererLite: true, wantOffererControlling: false},
		{name: "both_lite", offererLite: true, answererLite: true, wantOffererControlling: true},
		{name: "both_full", wantOffererControlling: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newICELiteTestSession(t, tc.offererLite, testdata.ClientCertificate())
			answerer := newICELiteTestSession(t, tc.answererLite, testdata.ServerCertificate())

			offer, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			requireICELiteLine(t, tc.offererLite, "offer", offer)
			require.NoError(t, answerer.RemoteSDP(ctx, offer, false))

			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			requireICELiteLine(t, tc.answererLite, "answer", answer)
			require.NoError(t, offerer.RemoteSDP(ctx, answer, true))

			require.Equal(t, tc.wantOffererControlling, iceControllingOf(offerer, true), "offerer role")
			require.Equal(t, !tc.wantOffererControlling, iceControllingOf(answerer, false), "answerer role")

			requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE + DTLS negotiation")

			requireWebrtcSRTPBetween(t, offerer, answerer, 71)
			requireWebrtcSRTPBetween(t, answerer, offerer, 72)

			reOffer, err := answerer.ReOffer("")
			require.NoError(t, err)
			requireICELiteLine(t, tc.answererLite, "re-offer", reOffer)
		})
	}
}

// TestICELiteGathersHostCandidatesOnly pins that a lite agent offers host
// candidates only, as RFC 8445 section 2.5 has it, also on the ICE generation
// a Fork starts, and refuses STUN and TURN servers with an error that says so
// instead of the ICE stack's refusal of them.
func TestICELiteGathersHostCandidatesOnly(t *testing.T) {
	ctx := t.Context()
	s := newICELiteTestSession(t, true, testdata.ServerCertificate())
	fork, err := s.Fork(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fork.Close() })

	for _, generation := range []struct {
		name string
		sess *MediaSessionWebrtc
	}{
		{name: "first generation", sess: s},
		{name: "fork", sess: fork},
	} {
		offer, err := generation.sess.LocalSDP(ctx, false)
		require.NoError(t, err, generation.name)
		requireICELiteLine(t, true, generation.name, offer)
		candidates := sdpLines(offer, "a=candidate:")
		require.NotEmpty(t, candidates, generation.name)
		for _, l := range candidates {
			assert.Contains(t, l, " typ host", "%s: a lite agent offers host candidates only", generation.name)
		}
	}

	for _, url := range []string{"stun:stun.example.com:3478", "turn:turn.example.com:3478?transport=udp"} {
		conf := iceLiteTestConfig(true, testdata.ServerCertificate())
		conf.ICEURLs = []string{url}
		refused := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}, Mode: sdp.ModeSendrecv}
		err := refused.Init(ctx, conf)
		require.ErrorIs(t, err, ErrICEGathering, url)
		require.ErrorIs(t, err, errICELiteWithURLs, url)
		require.Contains(t, err.Error(), "lite", url)
	}

	// A list of candidate types the caller wrote is not narrowed for it: one
	// that names more than host candidates is refused as well.
	conf := iceLiteTestConfig(true, testdata.ServerCertificate())
	conf.CandidateTypes = []ICECandidateType{ICECandidateHost, ICECandidateServerReflexive}
	refused := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}, Mode: sdp.ModeSendrecv}
	require.ErrorIs(t, refused.Init(ctx, conf), ErrICEGathering)

	conf.CandidateTypes = []ICECandidateType{ICECandidateHost}
	hostOnly := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}, Mode: sdp.ModeSendrecv}
	require.NoError(t, hostOnly.Init(ctx, conf))
	require.NoError(t, hostOnly.Close())
}
