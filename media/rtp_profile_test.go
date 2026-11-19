// SPDX-License-Identifier: MPL-2.0

package media

import (
	"crypto/tls"
	"net"
	"strings"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin the media profile rules to the RFCs, deliberately NOT to any
// one peer. The regression that motivated them was an Asterisk chan_sip webphone
// offering RTP/SAVPF with a=fingerprint -- DTLS-SRTP under the pre-RFC-5764
// profile name -- being rejected as "unsupported media description protocol".
// The fix is not an arm for that peer: it is parsing the profile's axes and
// taking the keying mechanism from the attributes, which is what the specs say
// and which classifies every peer, including ones we have never seen.

// TestParseRTPProfileDecomposesAxes drives the full product of the axes rather
// than the profiles we happen to meet. S = SRTP (RFC 3711), F = AVPF feedback
// (RFC 4585), UDP/TLS/ = DTLS-keyed (RFC 5764) -- independent axes, so all eight
// combinations are well-formed inputs and must classify, not be enumerated.
func TestParseRTPProfileDecomposesAxes(t *testing.T) {
	tests := []struct {
		proto        string
		wantSecure   bool
		wantFeedback bool
		wantDTLS     bool
	}{
		{"RTP/AVP", false, false, false},
		{"RTP/AVPF", false, true, false},
		{"RTP/SAVP", true, false, false},
		// The regression: DTLS-SRTP signalled under the plain profile name. It is
		// "secure, with feedback"; whether it is DTLS-keyed is NOT readable here,
		// it comes from a=fingerprint.
		{"RTP/SAVPF", true, true, false},
		{"UDP/TLS/RTP/SAVP", true, false, true},
		{"UDP/TLS/RTP/SAVPF", true, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.proto, func(t *testing.T) {
			got, ok := parseRTPProfile(tt.proto)
			if !ok {
				t.Fatalf("parseRTPProfile(%q) rejected a well-formed RTP profile", tt.proto)
			}
			if got.secure != tt.wantSecure {
				t.Errorf("secure = %v, want %v", got.secure, tt.wantSecure)
			}
			if got.feedback != tt.wantFeedback {
				t.Errorf("feedback = %v, want %v", got.feedback, tt.wantFeedback)
			}
			if got.dtls != tt.wantDTLS {
				t.Errorf("dtls = %v, want %v", got.dtls, tt.wantDTLS)
			}
		})
	}
}

// TestParseRTPProfileRejectsNonRTPAndIncoherent pins the boundary. Rejecting is
// correct for protocols this stack cannot answer; it must not become a shrug
// that accepts anything.
func TestParseRTPProfileRejectsNonRTPAndIncoherent(t *testing.T) {
	for _, proto := range []string{
		"",
		"udp",
		"TCP/MSRP",
		"RTP/FOO",
		// DTLS keying over an unsecured profile is not a thing a peer may offer:
		// the whole point of the UDP/TLS/ prefix is keying SRTP.
		"UDP/TLS/RTP/AVP",
	} {
		if _, ok := parseRTPProfile(proto); ok {
			t.Errorf("parseRTPProfile(%q) = accepted; want rejected", proto)
		}
	}
}

// TestParseRTPProfileIsLiberalInCasing pins tolerance of casing and whitespace.
// SDP tokens are defined uppercase (RFC 4566), but dropping a call over a peer's
// casing is a worse outcome than accepting it, and accepting costs nothing.
func TestParseRTPProfileIsLiberalInCasing(t *testing.T) {
	for _, proto := range []string{"rtp/savpf", "  RTP/SAVPF  ", "Rtp/SavpF"} {
		got, ok := parseRTPProfile(proto)
		if !ok {
			t.Fatalf("parseRTPProfile(%q) rejected; casing must not decide interop", proto)
		}
		if !got.secure || !got.feedback {
			t.Errorf("parseRTPProfile(%q) = %+v; want secure+feedback", proto, got)
		}
	}
}

// TestSDPFingerprintIsAuthoritativeForKeying pins the rule that actually fixes
// the regression: the KEYING mechanism comes from the attributes, never from the
// profile name. a=fingerprint means the keys arrive over DTLS (RFC 5763 s5,
// RFC 4572 s5), whatever profile name carried the offer.
func TestSDPFingerprintIsAuthoritativeForKeying(t *testing.T) {
	t.Run("present", func(t *testing.T) {
		attrs := []string{
			"rtcp-mux",
			"setup:actpass",
			"fingerprint:SHA-256 AA:0F:BA:F8:18:6D:6A:BD:0E:3B:98:FE:12:CA:59:D1",
		}
		if !sdpHasFingerprint(attrs) {
			t.Error("sdpHasFingerprint() = false with a=fingerprint present; " +
				"the offer would be misread as SDES and rejected for having no a=crypto")
		}
	})

	t.Run("absent", func(t *testing.T) {
		attrs := []string{"rtcp-mux", "crypto:1 AES_CM_128_HMAC_SHA1_80 inline:abc"}
		if sdpHasFingerprint(attrs) {
			t.Error("sdpHasFingerprint() = true with only a=crypto; an SDES offer must not be read as DTLS")
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		if !sdpHasFingerprint([]string{"FINGERPRINT:SHA-256 AA:0F"}) {
			t.Error("sdpHasFingerprint() is case sensitive; attribute casing must not decide keying")
		}
	})
}

// TestSecureRequestMeansSDESNotJustSecure pins the distinction between secured
// and keyed inline. RemoteSDP asks "must the keys be inline in this SDP?" --
// true only for a secured profile keyed by SDES. A DTLS-keyed offer is equally
// secure but carries no a=crypto, so demanding one rejects a valid offer. This
// is the exact shape of the bug: RTP/SAVPF + fingerprint classified as SDES ->
// no a=crypto -> ErrNoCommonCrypto.
func TestSecureRequestMeansSDESNotJustSecure(t *testing.T) {
	tests := []struct {
		name           string
		proto          string
		attrs          []string
		wantSDESNeeded bool
	}{
		{
			name:           "plain RTP/AVP needs no keys",
			proto:          "RTP/AVP",
			attrs:          []string{"rtcp-mux"},
			wantSDESNeeded: false,
		},
		{
			name:           "RTP/SAVP with crypto is SDES",
			proto:          "RTP/SAVP",
			attrs:          []string{"crypto:1 AES_CM_128_HMAC_SHA1_80 inline:abc"},
			wantSDESNeeded: true,
		},
		{
			name:           "RTP/SAVPF with fingerprint is DTLS, not SDES",
			proto:          "RTP/SAVPF",
			attrs:          []string{"setup:actpass", "fingerprint:SHA-256 AA:0F"},
			wantSDESNeeded: false,
		},
		{
			name:           "UDP/TLS/RTP/SAVPF is DTLS by profile",
			proto:          "UDP/TLS/RTP/SAVPF",
			attrs:          []string{"setup:actpass", "fingerprint:SHA-256 AA:0F"},
			wantSDESNeeded: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prof, ok := parseRTPProfile(tt.proto)
			if !ok {
				t.Fatalf("parseRTPProfile(%q) rejected", tt.proto)
			}
			// The classification RemoteSDP adds to its own.
			got := prof.requiresInlineKeys(tt.attrs)
			if got != tt.wantSDESNeeded {
				t.Errorf("SDES-required = %v, want %v (proto=%q)", got, tt.wantSDESNeeded, tt.proto)
			}
		})
	}
}

// TestForkCarriesSecurityMode pins that a re-negotiation inherits the session's
// security. Dropping SecureRTP made every bodied re-INVITE on a DTLS-SRTP call
// answer plain RTP/AVP with no fingerprint.
func TestForkCarriesSecurityMode(t *testing.T) {
	for _, mode := range []int{0, 1, 2} {
		s := &MediaSession{SecureRTP: mode, SRTPAlg: 7}
		got := s.Fork()
		if got.SecureRTP != mode {
			t.Errorf("Fork().SecureRTP = %d, want %d; a fork that downgrades answers a re-INVITE in the clear", got.SecureRTP, mode)
		}
		if got.SRTPAlg != 7 {
			t.Errorf("Fork().SRTPAlg = %d, want 7; the algorithm is a property of the session, not of one exchange", got.SRTPAlg)
		}
	}
}

// newProfileSession returns a session of the SecureRTP mode secure on the
// loopback. A DTLS-SRTP session gets cert and bound sockets, which its
// handshake is built on; the others describe a fixed port and bind nothing.
func newProfileSession(t *testing.T, secure int, cert func() tls.Certificate) *MediaSession {
	t.Helper()
	if secure == SecureRTPModeDTLS {
		return newDTLSRoleSession(t, cert())
	}
	s := &MediaSession{
		Codecs:    []Codec{CodecAudioUlaw},
		Mode:      sdp.ModeSendrecv,
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 4000},
		SecureRTP: secure,
	}
	if secure == SecureRTPModeSDES {
		s.SRTPAlg = SRTPProfileAes128CmHmacSha1_80
	}
	return s
}

// sdpAudioProto returns the transport of the audio m= line of body.
func sdpAudioProto(t *testing.T, body []byte) string {
	t.Helper()
	sd := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(body, &sd))
	md, err := sd.MediaDescription("audio")
	require.NoError(t, err)
	return md.Proto
}

// sdpWithProto returns body with the transport of its audio m= line replaced
// by proto.
func sdpWithProto(t *testing.T, body []byte, proto string) []byte {
	t.Helper()
	old := " " + sdpAudioProto(t, body) + " "
	require.Equal(t, 1, strings.Count(string(body), old), "the transport is not unique in the body")
	return []byte(strings.Replace(string(body), old, " "+proto+" ", 1))
}

// sdpWithoutAttribute returns body without its a= lines of the attribute name.
func sdpWithoutAttribute(body []byte, name string) []byte {
	var kept []string
	for line := range strings.SplitSeq(string(body), "\r\n") {
		if !strings.HasPrefix(line, "a="+name+":") {
			kept = append(kept, line)
		}
	}
	return []byte(strings.Join(kept, "\r\n"))
}

// TestAnswerRepeatsTheOfferedProfile drives every profile of the family
// through RemoteSDP and LocalSDP: each is accepted by the session that can key
// it, and the answer carries the transport the offer came under, as the peer
// wrote it, with the keying the session runs. Offer/answer leaves the
// transport to the offerer (RFC 3264 section 8.3.1, RFC 5124 section 3.3.1),
// so an answer under another name is an answer to another offer.
func TestAnswerRepeatsTheOfferedProfile(t *testing.T) {
	const (
		keyingNone        = ""
		keyingCrypto      = "a=crypto:"
		keyingFingerprint = "a=fingerprint:"
	)
	for _, tc := range []struct {
		name   string
		secure int
		// proto replaces the transport of the offer a session of the same
		// mode makes, which carries that mode's keying attributes.
		proto string
		// strip names a keying attribute taken out of the offer.
		strip      string
		wantKeying string
	}{
		{name: "plain_AVP", secure: SecureRTPModeNone, proto: "RTP/AVP", wantKeying: keyingNone},
		{name: "plain_AVPF", secure: SecureRTPModeNone, proto: "RTP/AVPF", wantKeying: keyingNone},
		{name: "plain_AVP_in_lower_case", secure: SecureRTPModeNone, proto: "rtp/avp", wantKeying: keyingNone},
		{name: "SDES_SAVP", secure: SecureRTPModeSDES, proto: "RTP/SAVP", wantKeying: keyingCrypto},
		{name: "SDES_SAVPF", secure: SecureRTPModeSDES, proto: "RTP/SAVPF", wantKeying: keyingCrypto},
		// SRTP offered under the unsecured name (RFC 8643 section 3.1) is
		// accepted by answering with the keying attribute (section 3.2), under
		// the name it was offered with.
		{name: "SDES_keys_under_AVP", secure: SecureRTPModeSDES, proto: "RTP/AVP", wantKeying: keyingCrypto},
		{name: "SDES_keys_under_AVPF", secure: SecureRTPModeSDES, proto: "RTP/AVPF", wantKeying: keyingCrypto},
		{name: "SDES_session_offered_plain_AVP", secure: SecureRTPModeSDES, proto: "RTP/AVP", strip: "crypto", wantKeying: keyingNone},
		{name: "DTLS_SAVP", secure: SecureRTPModeDTLS, proto: "UDP/TLS/RTP/SAVP", wantKeying: keyingFingerprint},
		{name: "DTLS_SAVPF", secure: SecureRTPModeDTLS, proto: "UDP/TLS/RTP/SAVPF", wantKeying: keyingFingerprint},
		// DTLS-SRTP under the names without the UDP/TLS/ prefix: the
		// fingerprint says how the keys arrive.
		{name: "DTLS_under_plain_SAVPF", secure: SecureRTPModeDTLS, proto: "RTP/SAVPF", wantKeying: keyingFingerprint},
		{name: "DTLS_under_plain_SAVP", secure: SecureRTPModeDTLS, proto: "RTP/SAVP", wantKeying: keyingFingerprint},
		{name: "DTLS_SAVPF_in_lower_case", secure: SecureRTPModeDTLS, proto: "udp/tls/rtp/savpf", wantKeying: keyingFingerprint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offerer := newProfileSession(t, tc.secure, testdata.ClientCertificate)
			answerer := newProfileSession(t, tc.secure, testdata.ServerCertificate)

			offer := sdpWithProto(t, offerer.LocalSDP(false), tc.proto)
			if tc.strip != "" {
				offer = sdpWithoutAttribute(offer, tc.strip)
			}
			require.NoError(t, answerer.RemoteSDP(offer, false))

			answer := answerer.LocalSDP(true)
			assert.Equal(t, tc.proto, sdpAudioProto(t, answer), "the answer's transport")
			for _, keying := range []string{keyingCrypto, keyingFingerprint} {
				assert.Equal(t, keying == tc.wantKeying, strings.Contains(string(answer), keying),
					"%s in the answer", keying)
			}
		})
	}
}

// TestOfferKeepsOurOwnProfile pins that only an answer repeats the peer's
// transport. A session that answered DTLS-SRTP offered under RTP/SAVPF makes
// its own offers under the name RFC 5764 section 8 gives DTLS-SRTP, from the
// session itself and from a fork of it.
func TestOfferKeepsOurOwnProfile(t *testing.T) {
	offerer := newDTLSRoleSession(t, testdata.ClientCertificate())
	answerer := newDTLSRoleSession(t, testdata.ServerCertificate())

	require.NoError(t, answerer.RemoteSDP(sdpWithProto(t, offerer.LocalSDP(false), "RTP/SAVPF"), false))
	require.Equal(t, "RTP/SAVPF", sdpAudioProto(t, answerer.LocalSDP(true)))

	assert.Equal(t, "UDP/TLS/RTP/SAVP", sdpAudioProto(t, answerer.LocalSDP(false)), "an offer of the session")
	assert.Equal(t, "UDP/TLS/RTP/SAVP", sdpAudioProto(t, answerer.Fork().LocalSDP(false)), "an offer of its fork")
}

// TestForkOfDTLSSessionDescribesDTLS pins what a fork of a DTLS-SRTP session
// puts on the wire, which is what a re-INVITE on the call carries. A fork that
// lost the security mode offers and answers plain RTP/AVP with neither
// a=fingerprint nor a=setup.
func TestForkOfDTLSSessionDescribesDTLS(t *testing.T) {
	offerer := newDTLSRoleSession(t, testdata.ClientCertificate())
	answerer := newDTLSRoleSession(t, testdata.ServerCertificate())
	require.NoError(t, answerer.RemoteSDP(offerer.LocalSDP(false), false))
	require.NoError(t, offerer.RemoteSDP(answerer.LocalSDP(true), true))

	t.Run("offer", func(t *testing.T) {
		for name, s := range map[string]*MediaSession{"of_the_offerer": offerer, "of_the_answerer": answerer} {
			t.Run(name, func(t *testing.T) {
				offer := s.Fork().LocalSDP(false)
				assert.Equal(t, "UDP/TLS/RTP/SAVP", sdpAudioProto(t, offer))
				assert.Contains(t, string(offer), "a=fingerprint:SHA-256 ")
				assert.Equal(t, "a=setup:actpass", sdpSetupLine(t, offer))
			})
		}
	})

	t.Run("answer", func(t *testing.T) {
		for _, proto := range []string{"UDP/TLS/RTP/SAVP", "UDP/TLS/RTP/SAVPF", "RTP/SAVPF"} {
			t.Run(strings.ReplaceAll(proto, "/", "_"), func(t *testing.T) {
				fork := answerer.Fork()
				require.NoError(t, fork.RemoteSDP(sdpWithProto(t, offerer.Fork().LocalSDP(false), proto), false))
				answer := fork.LocalSDP(true)
				assert.Equal(t, proto, sdpAudioProto(t, answer))
				assert.Contains(t, string(answer), "a=fingerprint:SHA-256 ")
				assert.Equal(t, "a=setup:active", sdpSetupLine(t, answer))
			})
		}
	})
}

// TestRemoteSDPClassifiesSecuredProfileWithoutKeys obtains ErrNoCommonCrypto
// through RemoteSDP: a description that must carry its SRTP keys inline and
// brings none this session can use is the peer's to fix, and is told apart
// from a transport outside the RTP profile family.
func TestRemoteSDPClassifiesSecuredProfileWithoutKeys(t *testing.T) {
	sdesOffer := func(t *testing.T) []byte {
		return newProfileSession(t, SecureRTPModeSDES, nil).LocalSDP(false)
	}
	for _, tc := range []struct {
		name string
		// session returns the session the body is applied to.
		session func(t *testing.T) *MediaSession
		body    func(t *testing.T) []byte
		want    error
	}{
		{
			name:    "SAVP_without_keys",
			session: func(t *testing.T) *MediaSession { return newProfileSession(t, SecureRTPModeSDES, nil) },
			body:    func(t *testing.T) []byte { return sdpWithoutAttribute(sdesOffer(t), "crypto") },
			want:    ErrNoCommonCrypto,
		},
		{
			name:    "SAVPF_without_keys",
			session: func(t *testing.T) *MediaSession { return newProfileSession(t, SecureRTPModeSDES, nil) },
			body: func(t *testing.T) []byte {
				return sdpWithoutAttribute(sdpWithProto(t, sdesOffer(t), "RTP/SAVPF"), "crypto")
			},
			want: ErrNoCommonCrypto,
		},
		{
			name:    "SAVP_keyed_for_a_session_without_SRTP",
			session: func(t *testing.T) *MediaSession { return newProfileSession(t, SecureRTPModeNone, nil) },
			body:    sdesOffer,
			want:    ErrNoCommonCrypto,
		},
		{
			// A session keyed by SDES keeps asking for the keys, whatever name
			// a later offer comes under.
			name: "keyed_SDES_session_offered_AVP_without_keys",
			session: func(t *testing.T) *MediaSession {
				s := newProfileSession(t, SecureRTPModeSDES, nil)
				require.NoError(t, s.RemoteSDP(sdesOffer(t), false))
				return s.Fork()
			},
			body: func(t *testing.T) []byte {
				return sdpWithoutAttribute(sdpWithProto(t, sdesOffer(t), "RTP/AVP"), "crypto")
			},
			want: ErrNoCommonCrypto,
		},
		{
			name:    "DTLS_prefix_over_an_unsecured_profile",
			session: func(t *testing.T) *MediaSession { return newProfileSession(t, SecureRTPModeNone, nil) },
			body: func(t *testing.T) []byte {
				return sdpWithProto(t, newProfileSession(t, SecureRTPModeNone, nil).LocalSDP(false), "UDP/TLS/RTP/AVP")
			},
			want: ErrNoCommonMedia,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.session(t).RemoteSDP(tc.body(t), false)
			require.ErrorIs(t, err, tc.want)
		})
	}
}
