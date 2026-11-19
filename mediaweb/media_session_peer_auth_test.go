// SPDX-License-Identifier: MPL-2.0

package mediaweb

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"fmt"
	"hash"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/pion/ice/v4"
	webrtcsdp "github.com/pion/sdp/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fingerprintLine matches one a=fingerprint line of an SDP, its line end
// included.
var fingerprintLine = regexp.MustCompile(`(?m)^a=fingerprint:.*\r\n`)

// newPeerAuthSession returns a session that gathered its candidates on the
// loopback interface and presents certificate.
func newPeerAuthSession(ctx context.Context, t *testing.T, certificate tls.Certificate) *MediaSessionWebrtc {
	t.Helper()
	m := &MediaSessionWebrtc{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		Mode:   "sendrecv",
	}
	require.NoError(t, m.Init(ctx, MediaSessionWebrtcConfig{
		NetworkTypes:    []ice.NetworkType{ice.NetworkTypeUDP4},
		IncludeLoopback: true,
		InterfaceFilter: testLoopbackInterface,
		DTLS:            media.DTLSConfig{Certificates: []tls.Certificate{certificate}},
	}))
	t.Cleanup(func() { assert.NoError(t, m.Close()) })
	return m
}

// peerAuthOffer returns the offer of a new session presenting certificate.
func peerAuthOffer(ctx context.Context, t *testing.T, certificate tls.Certificate) string {
	t.Helper()
	offer, err := newPeerAuthSession(ctx, t, certificate).LocalSDP(ctx, false)
	require.NoError(t, err)
	require.Len(t, fingerprintLine.FindAllString(string(offer), -1), 1)
	return string(offer)
}

// fingerprintOf returns the a=fingerprint line of certificate under the hash
// function named name: the digest in upper case hex separated by colons.
func fingerprintOf(name string, h hash.Hash, certificate tls.Certificate) string {
	h.Write(certificate.Certificate[0])
	sum := h.Sum(nil)
	octets := make([]string, len(sum))
	for i, b := range sum {
		octets[i] = fmt.Sprintf("%02X", b)
	}
	return "a=fingerprint:" + name + " " + strings.Join(octets, ":") + "\r\n"
}

// withAudioFingerprints returns body with the a=fingerprint lines of its audio
// section replaced by lines, at the place of the first one.
func withAudioFingerprints(body string, lines ...string) string {
	first := true
	return fingerprintLine.ReplaceAllStringFunc(body, func(string) string {
		if !first {
			return ""
		}
		first = false
		return strings.Join(lines, "")
	})
}

// withSessionFingerprints returns body with lines added at its session level,
// ahead of the audio section.
func withSessionFingerprints(body string, lines ...string) string {
	return strings.Replace(body, "m=audio ", strings.Join(lines, "")+"m=audio ", 1)
}

// TestMediaSessionWebrtcHandshakeChecksPeerFingerprints asserts the handshake
// verifies the peer's certificate against the fingerprints RFC 8122 gives it
// for the audio, and against no other its SDP carries. They are:
//
//   - every a=fingerprint line that applies, not the first one alone. Section
//     5.1: "Multiple SDP fingerprint attributes can be associated with an 'm'
//     line", for one certificate under several hash functions or for several
//     certificates;
//   - of those, the ones under the strongest hash function offered. Section
//     5.1: "An endpoint MUST select the set of fingerprints that use its most
//     preferred hash function (out of those offered by the peer) and verify
//     that each certificate used matches one fingerprint out of that set. If
//     a certificate does not match any such fingerprint, the endpoint MUST NOT
//     establish the TLS connection." A certificate that matches only under a
//     weaker hash function beside it fails;
//   - those of the audio section, and those of the session level only when
//     the section has none. Section 5: "If it is a session-level attribute,
//     it applies to all TLS sessions for which no media-level fingerprint
//     attribute is defined." A certificate that matches only a session-level
//     fingerprint beside the section's own fails.
//
// A peer that offers sha-1 alone is verified under it, and MD5 is not among
// the hash functions to choose from, since section 5 forbids verifying with
// it.
func TestMediaSessionWebrtcHandshakeChecksPeerFingerprints(t *testing.T) {
	peer := testdata.ClientCertificate()
	// The answerer's own certificate is the one the offerer does not present.
	other := testdata.ServerCertificate()

	sha1Line := fingerprintOf("sha-1", sha1.New(), peer)
	sha256Line := fingerprintOf("sha-256", sha256.New(), peer)
	sha384Line := fingerprintOf("sha-384", sha512.New384(), peer)
	otherSHA1Line := fingerprintOf("sha-1", sha1.New(), other)
	otherSHA256Line := fingerprintOf("sha-256", sha256.New(), other)
	otherSHA512Line := fingerprintOf("sha-512", sha512.New(), other)
	otherMD5Line := "a=fingerprint:md5 00:11:22:33:44:55:66:77:88:99:AA:BB:CC:DD:EE:FF\r\n"

	tests := []struct {
		name    string
		session []string
		audio   []string
		wantErr bool
	}{
		{name: "SHA256", audio: []string{sha256Line}},
		{name: "SHA1Alone", audio: []string{sha1Line}},
		{name: "SHA224", audio: []string{fingerprintOf("sha-224", sha256.New224(), peer)}},
		{name: "SHA384", audio: []string{sha384Line}},
		{name: "SHA512UpperCaseName", audio: []string{fingerprintOf("SHA-512", sha512.New(), peer)}},
		{name: "AnotherCertificate", audio: []string{otherSHA256Line}, wantErr: true},

		{name: "FirstOfTwoUnderOneHash", audio: []string{sha256Line, otherSHA256Line}},
		{name: "SecondOfTwoUnderOneHash", audio: []string{otherSHA256Line, sha256Line}},
		{name: "SecondOfTwoUnderStrongerHash", audio: []string{otherSHA256Line, sha384Line}},
		{name: "EveryHashOfTheCertificate", audio: []string{sha1Line, sha256Line, sha384Line}},
		{name: "NoneOfSeveral", audio: []string{otherSHA256Line, otherSHA512Line}, wantErr: true},

		{name: "StrongerHashBesideWeakerOfOther", audio: []string{otherSHA1Line, sha256Line}},
		{name: "WeakerHashOnlyStrongerOffered", audio: []string{sha1Line, otherSHA256Line}, wantErr: true},
		{name: "WeakerHashOnlyStrongerOfferedFirst", audio: []string{otherSHA256Line, sha1Line}, wantErr: true},
		{name: "SHA256OnlySHA512Offered", audio: []string{sha256Line, otherSHA512Line}, wantErr: true},
		{name: "MD5Only", audio: []string{otherMD5Line}, wantErr: true},
		{name: "MD5IsNotPreferred", audio: []string{otherMD5Line, sha1Line}},

		{name: "SessionLevelAlone", session: []string{sha256Line}},
		{name: "SessionLevelSecondOfTwo", session: []string{otherSHA256Line, sha256Line}},
		{name: "SessionLevelWeakerHashOnly", session: []string{sha1Line, otherSHA256Line}, wantErr: true},
		{name: "SessionLevelBesideOtherOnAudio", session: []string{sha256Line}, audio: []string{otherSHA256Line}, wantErr: true},
		{name: "AudioBesideOtherOnSession", session: []string{otherSHA256Line}, audio: []string{sha256Line}},
		// The stronger hash function is offered for other media only, so the
		// audio's own sha-1 is the strongest that applies to it.
		{name: "StrongerHashOnSessionAudioHasItsOwn", session: []string{otherSHA512Line}, audio: []string{sha1Line}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			offerer := newPeerAuthSession(ctx, t, peer)
			answerer := newPeerAuthSession(ctx, t, other)

			offer, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			rewritten := withSessionFingerprints(withAudioFingerprints(string(offer), tc.audio...), tc.session...)
			require.NoError(t, answerer.RemoteSDP(ctx, []byte(rewritten), false))
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			require.NoError(t, offerer.RemoteSDP(ctx, answer, true))

			// Each side blocks until the other answers, so both run at once.
			offererErr := make(chan error, 1)
			answererErr := make(chan error, 1)
			go func() { offererErr <- offerer.Finalize(ctx) }()
			go func() { answererErr <- answerer.Finalize(ctx) }()
			gotAnswerer, gotOfferer := <-answererErr, <-offererErr

			if tc.wantErr {
				assert.ErrorContains(t, gotAnswerer, "does not match any SDP fingerprint")
				assert.Error(t, gotOfferer, "the peer must see the handshake fail too")
				return
			}
			assert.NoError(t, gotAnswerer)
			assert.NoError(t, gotOfferer)
		})
	}
}

// TestMediaSessionWebrtcRemoteSDPFingerprintLines asserts which descriptions
// are refused for their a=fingerprint lines while they are applied: one with
// no line that applies to the audio, and one where a line that applies is not
// a hash function name followed by one value (RFC 8122 section 5:
// fingerprint-attribute = "fingerprint" ":" hash-func SP fingerprint). A
// session-level line is not read when the audio section has its own, since it
// does not apply to the audio. A refused description starts no connectivity
// checks.
func TestMediaSessionWebrtcRemoteSDPFingerprintLines(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	peer := testdata.ClientCertificate()
	offer := peerAuthOffer(ctx, t, peer)
	sha256Line := fingerprintOf("sha-256", sha256.New(), peer)
	sha384Line := fingerprintOf("sha-384", sha512.New384(), peer)
	const noValueLine = "a=fingerprint:sha-256\r\n"
	const twoValuesLine = "a=fingerprint:sha-256 00:11 22:33\r\n"
	const md5Line = "a=fingerprint:md5 00:11:22:33\r\n"

	tests := []struct {
		name    string
		offer   string
		wantErr string
	}{
		{name: "Present", offer: offer},
		{name: "TwoLines", offer: withAudioFingerprints(offer, sha256Line, sha384Line)},
		{name: "SessionLevel", offer: withSessionFingerprints(withAudioFingerprints(offer), sha256Line)},
		{name: "SessionLevelAndAudio", offer: withSessionFingerprints(offer, sha384Line)},
		// Nothing in this description can verify a certificate, which the
		// handshake finds out: see MD5Only above.
		{name: "MD5Only", offer: withAudioFingerprints(offer, md5Line)},
		{name: "Missing", offer: withAudioFingerprints(offer), wantErr: "no DTLS fingerprint"},
		{
			name:    "OtherMediaDescriptionOnly",
			offer:   withAudioFingerprints(offer) + "m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" + sha256Line,
			wantErr: "no DTLS fingerprint",
		},
		{name: "NoValue", offer: withAudioFingerprints(offer, noValueLine), wantErr: "invalid remote DTLS fingerprint"},
		{name: "TwoValues", offer: withAudioFingerprints(offer, twoValuesLine), wantErr: "invalid remote DTLS fingerprint"},
		{name: "NoValueAfterComplete", offer: withAudioFingerprints(offer, sha256Line, noValueLine), wantErr: "invalid remote DTLS fingerprint"},
		{name: "TwoValuesAfterComplete", offer: withAudioFingerprints(offer, sha256Line, twoValuesLine), wantErr: "invalid remote DTLS fingerprint"},
		{
			name:    "SessionLevelNoValueAfterComplete",
			offer:   withSessionFingerprints(withAudioFingerprints(offer), sha256Line, noValueLine),
			wantErr: "invalid remote DTLS fingerprint",
		},
		{name: "SessionLevelNoValueAudioHasItsOwn", offer: withSessionFingerprints(offer, noValueLine)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			answerer := newPeerAuthSession(ctx, t, testdata.ServerCertificate())
			err := answerer.RemoteSDP(ctx, []byte(tc.offer), false)

			answerer.mu.Lock()
			checksStarted := answerer.iceConn != nil
			answerer.mu.Unlock()

			if tc.wantErr != "" {
				assert.ErrorContains(t, err, tc.wantErr)
				assert.False(t, checksStarted, "a refused description must start no connectivity checks")
				return
			}
			assert.NoError(t, err)
			assert.True(t, checksStarted)
		})
	}
}

// TestRemoteDTLSFingerprints pins the fingerprints read for the audio: every
// line of its own section in the order given, and those of the session level
// only when the section has none (RFC 8122 section 5).
func TestRemoteDTLSFingerprints(t *testing.T) {
	attr := func(key, value string) webrtcsdp.Attribute {
		return webrtcsdp.Attribute{Key: key, Value: value}
	}
	session := []webrtcsdp.Attribute{
		attr("group", "BUNDLE 0"),
		attr("fingerprint", "sha-256 AA:BB"),
		attr("fingerprint", "sha-1 CC:DD"),
	}
	audio := []webrtcsdp.Attribute{
		attr("mid", "0"),
		attr("fingerprint", "sha-512 11:22"),
		attr("rtcp-mux", ""),
		attr("fingerprint", "SHA-256 33:44"),
	}
	sessionWant := []media.DTLSFingerprint{
		{Algorithm: "sha-256", Value: "AA:BB"},
		{Algorithm: "sha-1", Value: "CC:DD"},
	}
	audioWant := []media.DTLSFingerprint{
		{Algorithm: "sha-512", Value: "11:22"},
		{Algorithm: "SHA-256", Value: "33:44"},
	}

	tests := []struct {
		name    string
		session []webrtcsdp.Attribute
		audio   []webrtcsdp.Attribute
		want    []media.DTLSFingerprint
	}{
		{name: "AudioAlone", audio: audio, want: audioWant},
		{name: "SessionAlone", session: session, audio: []webrtcsdp.Attribute{attr("mid", "0")}, want: sessionWant},
		{name: "AudioBesideSession", session: session, audio: audio, want: audioWant},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := remoteDTLSFingerprints(tc.session, tc.audio)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
