// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the media profile rules of a WebRTC session to the RFCs,
// deliberately not to any one peer. The offer that motivated them came from an
// Asterisk chan_sip webphone: RTP/SAVPF with a=fingerprint, DTLS-SRTP under
// the name RFC 5124 registers and not the one of RFC 5764. Matching the
// transport against a list of spellings refuses it as an unsupported
// transport. The profile is read by its axes instead, the keying is taken from
// the attributes, and the answer repeats the name the offer came under.

// TestWebrtcProfileAnswerRepeatsTheOfferedProfile drives every profile of the
// family RFC 8829 section 5.1.2 has a WebRTC endpoint accept through RemoteSDP
// and LocalSDP: each is accepted, the answer carries the transport the offer
// came under and the DTLS keying the session runs, and the exchange completes
// with media decrypting both ways, the offerer having read an answer under
// that name too. Reduced-size RTCP is confirmed in the answer under the
// feedback profiles only, which are the ones it is defined for (RFC 5506
// section 4).
func TestWebrtcProfileAnswerRepeatsTheOfferedProfile(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proto string
		// wantReducedSize says the answer confirms the a=rtcp-rsize the
		// offer carries.
		wantReducedSize bool
	}{
		{name: "DTLS_SAVPF", proto: "UDP/TLS/RTP/SAVPF", wantReducedSize: true},
		{name: "DTLS_SAVP", proto: "UDP/TLS/RTP/SAVP"},
		// DTLS-SRTP under the names without the UDP/TLS/ prefix: the
		// fingerprint says how the keys arrive.
		{name: "DTLS_under_plain_SAVPF", proto: "RTP/SAVPF", wantReducedSize: true},
		{name: "DTLS_under_plain_SAVP", proto: "RTP/SAVP"},
		// "The choice of SAVP or AVP has no effect; support for DTLS-SRTP is
		// determined by the presence of one or more a=fingerprint attributes".
		{name: "DTLS_under_AVPF", proto: "RTP/AVPF", wantReducedSize: true},
		{name: "DTLS_under_AVP", proto: "RTP/AVP"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)

			offer, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			require.Contains(t, string(offer), "a=rtcp-rsize\r\n")
			require.NoError(t, answerer.RemoteSDP(ctx, sdpWithProto(t, offer, tc.proto), false))

			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			assert.Equal(t, tc.proto, sdpAudioProto(t, answer), "the answer's transport")
			assert.Contains(t, string(answer), "a=fingerprint:", "the answer's keying")
			assert.Equal(t, "a=setup:active", sdpSetupLine(t, answer))
			assert.Equal(t, tc.wantReducedSize, strings.Contains(string(answer), "a=rtcp-rsize\r\n"),
				"a=rtcp-rsize in the answer")

			require.NoError(t, offerer.RemoteSDP(ctx, answer, true))
			requireWebrtcMedia(ctx, t, offerer, answerer)
		})
	}
}

// TestWebrtcProfileOfferKeepsOurOwnProfile pins that only an answer repeats the
// peer's transport. A session that answered an offer under RTP/SAVPF makes its
// own offers under the name RFC 8829 section 5.2.1 requires of an offer, from
// the session itself and from a fork of it, and so does a session that read
// the answer to its offer under another name.
func TestWebrtcProfileOfferKeepsOurOwnProfile(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)

	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.Equal(t, "UDP/TLS/RTP/SAVPF", sdpAudioProto(t, offer))
	require.NoError(t, answerer.RemoteSDP(ctx, sdpWithProto(t, offer, "RTP/SAVPF"), false))
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.Equal(t, "RTP/SAVPF", sdpAudioProto(t, answer))
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))

	reoffer, err := answerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, "UDP/TLS/RTP/SAVPF", sdpAudioProto(t, reoffer), "an offer of the session")

	fork, err := answerer.Fork(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fork.Close() })
	forkOffer, err := fork.LocalSDP(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, "UDP/TLS/RTP/SAVPF", sdpAudioProto(t, forkOffer), "an offer of its fork")

	reoffer, err = offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, "UDP/TLS/RTP/SAVPF", sdpAudioProto(t, reoffer), "an offer of the session that read the answer")
}

// TestWebrtcProfileSubsequentOfferIsAnsweredUnderItsOwnName pins which offer an
// answer repeats: the one it answers. A peer that offers the call again under
// another name of the family, on the ICE generation the session negotiated, is
// answered under that name, and a session started for an ICE restart answers
// under the name the restarting offer came under.
func TestWebrtcProfileSubsequentOfferIsAnsweredUnderItsOwnName(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)

	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, sdpWithProto(t, offer, "RTP/SAVPF"), false))
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.Equal(t, "RTP/SAVPF", sdpAudioProto(t, answer))

	// The same ICE generation, under the registered name this time.
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	answer, err = answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, "UDP/TLS/RTP/SAVPF", sdpAudioProto(t, answer), "the answer to the subsequent offer")

	// A refused offer leaves the name of the last accepted one.
	err = answerer.RemoteSDP(ctx, withSetupLine(t, sdpWithProto(t, offer, "RTP/SAVP"), "holdconn"), false)
	require.Error(t, err)
	answer, err = answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, "UDP/TLS/RTP/SAVPF", sdpAudioProto(t, answer), "the answer after a refused offer")

	// An ICE restart: the dialog answers it from a fork of the session.
	restarted := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	restart, err := restarted.LocalSDP(ctx, false)
	require.NoError(t, err)
	restart = sdpWithProto(t, restart, "RTP/SAVP")
	require.ErrorIs(t, answerer.RemoteSDP(ctx, restart, false), ErrWebRTCICERestart)
	fork, err := answerer.Fork(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fork.Close() })
	require.NoError(t, fork.RemoteSDP(ctx, restart, false))
	answer, err = fork.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, "RTP/SAVP", sdpAudioProto(t, answer), "the answer to the restarting offer")

	// The session the restart was refused on still answers under the name of
	// the offer it last accepted.
	answer, err = answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, "UDP/TLS/RTP/SAVPF", sdpAudioProto(t, answer), "the answer of the session the restart left")
}

// TestWebrtcProfileRemoteSDPRefusesOtherTransports pins the boundary. Refusing
// is correct for a transport this session cannot answer; the family must not
// become a shrug that accepts anything.
func TestWebrtcProfileRemoteSDPRefusesOtherTransports(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	body := []byte(webrtcOffer(ctx, t, testdata.ClientCertificate()))
	for _, tc := range []struct {
		name  string
		proto string
	}{
		// DTLS keying over an unsecured profile is not a thing a peer may
		// offer: the whole point of the UDP/TLS/ prefix is keying SRTP.
		{name: "DTLS_over_an_unsecured_profile", proto: "UDP/TLS/RTP/AVP"},
		// ICE gathers UDP candidates only.
		{name: "DTLS_over_TCP", proto: "TCP/DTLS/RTP/SAVPF"},
		{name: "data_channel", proto: "UDP/DTLS/SCTP"},
	} {
		for _, offered := range []bool{false, true} {
			kind := "offer"
			if offered {
				kind = "answer"
			}
			t.Run(tc.name+"_in_"+kind, func(t *testing.T) {
				m := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
				err := m.RemoteSDP(ctx, sdpWithProto(t, body, tc.proto), offered)
				require.ErrorContains(t, err, `unsupported WebRTC audio transport "`+tc.proto+`"`)
				require.ErrorIs(t, err, ErrNoCommonMedia)
			})
		}
	}
}
