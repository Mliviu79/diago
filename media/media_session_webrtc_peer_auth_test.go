// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webrtcFingerprintLine matches one a=fingerprint line of an SDP, its line end
// included.
var webrtcFingerprintLine = regexp.MustCompile(`(?m)^a=fingerprint:.*\r\n`)

// newPeerAuthWebrtcSession returns a WebRTC session that gathered its
// candidates on the loopback interface and presents certificates.
func newPeerAuthWebrtcSession(ctx context.Context, t *testing.T, certificates ...tls.Certificate) *MediaSessionWebrtc {
	t.Helper()
	m := &MediaSessionWebrtc{
		Codecs: []Codec{CodecAudioUlaw},
		Mode:   sdp.ModeSendrecv,
	}
	require.NoError(t, m.Init(ctx, MediaSessionWebrtcConfig{
		DTLS:            DTLSConfig{Certificates: certificates},
		IPFamilies:      []ICEIPFamily{ICEIPFamilyIPv4},
		IncludeLoopback: true,
		InterfaceFilter: testLoopbackInterface,
	}))
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// webrtcOffer returns the offer of a new session presenting certificate.
func webrtcOffer(ctx context.Context, t *testing.T, certificate tls.Certificate) string {
	t.Helper()
	offer, err := newPeerAuthWebrtcSession(ctx, t, certificate).LocalSDP(ctx, false)
	require.NoError(t, err)
	require.Len(t, webrtcFingerprintLine.FindAllString(string(offer), -1), 1)
	return string(offer)
}

// withFingerprintLines returns body with its a=fingerprint lines replaced by
// lines, at the place of the first one.
func withFingerprintLines(body string, lines ...string) string {
	first := true
	return webrtcFingerprintLine.ReplaceAllStringFunc(body, func(string) string {
		if !first {
			return ""
		}
		first = false
		return strings.Join(lines, "")
	})
}

// withSessionFingerprintLines returns body with lines added at its session
// level, ahead of the first media description.
func withSessionFingerprintLines(body string, lines ...string) string {
	return strings.Replace(body, "m=audio ", strings.Join(lines, "")+"m=audio ", 1)
}

// sha256FingerprintLine returns the a=fingerprint line of certificate under
// sha-256.
func sha256FingerprintLine(certificate tls.Certificate) string {
	return "a=fingerprint:sha-256 " + testCertificateFingerprint(sha256.New(), certificate.Certificate[0]) + "\r\n"
}

// TestDTLSWebrtcRemoteSDPRequiresFingerprint asserts a WebRTC description is
// refused while it is applied when it gives no a=fingerprint the peer's
// certificate can be checked against, and accepted with any that can: under
// each hash function fingerprints are compared with, on several lines, at the
// session level or on the audio media description. RFC 5763 section 5 makes
// the attribute mandatory: it is what binds the self-signed certificate to the
// signalling, so without one any certificate would complete the handshake. A
// refused description starts no connectivity checks.
func TestDTLSWebrtcRemoteSDPRequiresFingerprint(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	peer := testdata.ClientCertificate()
	offer := webrtcOffer(ctx, t, peer)
	sha256Line := sha256FingerprintLine(peer)
	sha384Line := "a=fingerprint:sha-384 " + testCertificateFingerprint(sha512.New384(), peer.Certificate[0]) + "\r\n"
	const md5Line = "a=fingerprint:md5 00:11:22:33\r\n"
	const noValueLine = "a=fingerprint:sha-256\r\n"

	tests := []struct {
		name    string
		offer   string
		wantErr bool
	}{
		{name: "Present", offer: offer},
		{name: "Missing", offer: withFingerprintLines(offer), wantErr: true},
		{name: "UnsupportedHashOnly", offer: withFingerprintLines(offer, md5Line), wantErr: true},
		{name: "AnotherHashFunction", offer: withFingerprintLines(offer, sha384Line)},
		{name: "UnsupportedHashBesideSupported", offer: withFingerprintLines(offer, md5Line, sha256Line)},
		{name: "TwoFingerprints", offer: withFingerprintLines(offer, sha256Line, sha384Line)},
		{name: "SessionLevel", offer: withSessionFingerprintLines(withFingerprintLines(offer), sha256Line)},
		{name: "SessionLevelAndMediaLevel", offer: withSessionFingerprintLines(offer, sha384Line)},
		{name: "UnsupportedOnMediaSupportedOnSession", offer: withSessionFingerprintLines(withFingerprintLines(offer, md5Line), sha256Line)},
		{name: "NoValue", offer: withFingerprintLines(offer, noValueLine), wantErr: true},
		{name: "NoValueBesideComplete", offer: withFingerprintLines(offer, sha256Line, noValueLine), wantErr: true},
		{
			name:    "OtherMediaDescriptionOnly",
			offer:   withFingerprintLines(offer) + "m=video 9 UDP/TLS/RTP/SAVPF 96\r\n" + sha256Line,
			wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			answerer := newPeerAuthWebrtcSession(ctx, t, testdata.ServerCertificate())
			err := answerer.RemoteSDP(ctx, []byte(tc.offer), false)

			answerer.mu.Lock()
			checksStarted := answerer.iceConn != nil
			answerer.mu.Unlock()

			if tc.wantErr {
				assert.ErrorContains(t, err, "fingerprint")
				assert.False(t, checksStarted, "a refused description must start no connectivity checks")
				return
			}
			assert.NoError(t, err)
			assert.True(t, checksStarted)
		})
	}
}

// TestDTLSWebrtcHandshakeMatchesAnyFingerprint asserts the handshake of a
// WebRTC session checks the peer's certificate against every fingerprint its
// SDP gave: it completes when the certificate matches one of them, under any
// of the hash functions of RFC 8122 section 5 but MD2 and MD5, on whichever
// line and level it stands, and it fails when the certificate matches none
// (RFC 5763 section 5).
func TestDTLSWebrtcHandshakeMatchesAnyFingerprint(t *testing.T) {
	peer := testdata.ClientCertificate()
	leaf := peer.Certificate[0]
	// The answerer's own certificate is the one the offerer does not present.
	otherLine := sha256FingerprintLine(testdata.ServerCertificate())
	otherSHA512Line := "a=fingerprint:sha-512 " + testCertificateFingerprint(sha512.New(), testdata.ServerCertificate().Certificate[0]) + "\r\n"
	sha256Line := sha256FingerprintLine(peer)
	sha384Line := "a=fingerprint:sha-384 " + testCertificateFingerprint(sha512.New384(), leaf) + "\r\n"

	tests := []struct {
		name    string
		rewrite func(offer string) string
		wantErr bool
	}{
		{name: "sha-1", rewrite: func(offer string) string {
			return withFingerprintLines(offer, "a=fingerprint:sha-1 "+testCertificateFingerprint(sha1.New(), leaf)+"\r\n")
		}},
		{name: "sha-224", rewrite: func(offer string) string {
			return withFingerprintLines(offer, "a=fingerprint:sha-224 "+testCertificateFingerprint(sha256.New224(), leaf)+"\r\n")
		}},
		{name: "sha-384", rewrite: func(offer string) string {
			return withFingerprintLines(offer, sha384Line)
		}},
		{name: "sha-512-upper-case", rewrite: func(offer string) string {
			return withFingerprintLines(offer, "a=fingerprint:SHA-512 "+testCertificateFingerprint(sha512.New(), leaf)+"\r\n")
		}},
		{name: "SecondOfTwo", rewrite: func(offer string) string {
			return withFingerprintLines(offer, otherLine, sha384Line)
		}},
		{name: "SessionLevelBesideOtherOnMedia", rewrite: func(offer string) string {
			return withSessionFingerprintLines(withFingerprintLines(offer, otherLine), sha256Line)
		}},
		{name: "MediaLevelBesideOtherOnSession", rewrite: func(offer string) string {
			return withSessionFingerprintLines(offer, otherLine)
		}},
		{name: "NoneOfSeveral", wantErr: true, rewrite: func(offer string) string {
			return withSessionFingerprintLines(withFingerprintLines(offer, otherLine), otherSHA512Line)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			offerer := newPeerAuthWebrtcSession(ctx, t, peer)
			answerer := newPeerAuthWebrtcSession(ctx, t, testdata.ServerCertificate())

			offer, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			require.NoError(t, answerer.RemoteSDP(ctx, []byte(tc.rewrite(string(offer))), false))
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

// TestDTLSWebrtcSubsequentSDPFingerprintSet asserts a description of the ICE
// generation a session already negotiated continues its DTLS association only
// with the fingerprint set that association was negotiated with. RFC 8842
// section 3.1 asks for a new association when a fingerprint is modified, added
// or removed, which this session builds only with a new ICE generation. Hash
// function names and hex digits are case insensitive (RFC 8122 section 5) and
// neither the order nor the level of the lines carries a meaning, so those do
// not change the set.
func TestDTLSWebrtcSubsequentSDPFingerprintSet(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	peer := testdata.ClientCertificate()
	sha256Line := sha256FingerprintLine(peer)
	sha384Line := "a=fingerprint:sha-384 " + testCertificateFingerprint(sha512.New384(), peer.Certificate[0]) + "\r\n"
	otherLine := sha256FingerprintLine(testdata.ServerCertificate())

	tests := []struct {
		name    string
		first   []string
		reoffer func(first string) string
		wantErr bool
	}{
		{name: "Unchanged", first: []string{sha256Line}, reoffer: func(first string) string { return first }},
		{name: "AnotherCase", first: []string{sha256Line}, reoffer: func(first string) string {
			return withFingerprintLines(first, "a=fingerprint:SHA-256 "+strings.ToLower(strings.TrimPrefix(sha256Line, "a=fingerprint:sha-256 ")))
		}},
		{name: "AnotherOrder", first: []string{sha256Line, sha384Line}, reoffer: func(first string) string {
			return withFingerprintLines(first, sha384Line, sha256Line)
		}},
		{name: "MovedToSessionLevel", first: []string{sha256Line}, reoffer: func(first string) string {
			return withSessionFingerprintLines(withFingerprintLines(first), sha256Line)
		}},
		{name: "AnotherCertificate", wantErr: true, first: []string{sha256Line}, reoffer: func(first string) string {
			return withFingerprintLines(first, otherLine)
		}},
		{name: "Added", wantErr: true, first: []string{sha256Line}, reoffer: func(first string) string {
			return withFingerprintLines(first, sha256Line, otherLine)
		}},
		{name: "Removed", wantErr: true, first: []string{sha256Line, sha384Line}, reoffer: func(first string) string {
			return withFingerprintLines(first, sha256Line)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			first := withFingerprintLines(webrtcOffer(ctx, t, peer), tc.first...)
			answerer := newPeerAuthWebrtcSession(ctx, t, testdata.ServerCertificate())
			require.NoError(t, answerer.RemoteSDP(ctx, []byte(first), false))

			err := answerer.RemoteSDP(ctx, []byte(tc.reoffer(first)), false)
			if tc.wantErr {
				assert.ErrorContains(t, err, "fingerprint changed")
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestDTLSWebrtcSessionNeedsCertificate asserts a WebRTC session without a
// usable certificate is refused by Init, before it creates an ICE agent. Its
// SDP could carry no a=fingerprint for that certificate, which RFC 5763
// section 5 requires, and it would have nothing to present in the handshake,
// so it could only fail once the call was already answered.
func TestDTLSWebrtcSessionNeedsCertificate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	tests := []struct {
		name         string
		certificates []tls.Certificate
		wantErr      bool
	}{
		{name: "NoCertificate", wantErr: true},
		{name: "EmptyCertificate", certificates: []tls.Certificate{{}}, wantErr: true},
		{name: "EmptyBesideUsable", certificates: []tls.Certificate{testdata.ServerCertificate(), {}}, wantErr: true},
		{name: "Certificate", certificates: []tls.Certificate{testdata.ServerCertificate()}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &MediaSessionWebrtc{
				Codecs: []Codec{CodecAudioUlaw},
				Mode:   sdp.ModeSendrecv,
			}
			err := m.Init(ctx, MediaSessionWebrtcConfig{
				DTLS:            DTLSConfig{Certificates: tc.certificates},
				IPFamilies:      []ICEIPFamily{ICEIPFamilyIPv4},
				IncludeLoopback: true,
				InterfaceFilter: testLoopbackInterface,
			})
			t.Cleanup(func() { _ = m.Close() })

			m.mu.Lock()
			agentCreated := m.agent != nil
			m.mu.Unlock()

			if tc.wantErr {
				require.ErrorContains(t, err, "certificate")
				assert.False(t, agentCreated, "a refused session must not create an ICE agent")
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestDTLSWebrtcLocalSDPFingerprintPerCertificate asserts the SDP of a WebRTC
// session carries the fingerprint of each certificate it is configured with,
// in an offer and in an answer. The DTLS stack presents whichever of them
// suits the handshake, and RFC 8122 section 5.1 has a fingerprint calculated
// for each certificate that may be used.
func TestDTLSWebrtcLocalSDPFingerprintPerCertificate(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	certificates := []tls.Certificate{testdata.ServerCertificate(), testdata.ClientCertificate()}
	want := []string{sha256FingerprintLine(certificates[0]), sha256FingerprintLine(certificates[1])}

	m := newPeerAuthWebrtcSession(ctx, t, certificates...)
	offer, err := m.LocalSDP(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, want, webrtcFingerprintLine.FindAllString(string(offer), -1))

	require.NoError(t, m.RemoteSDP(ctx, []byte(webrtcOffer(ctx, t, testdata.ClientCertificate())), false))
	answer, err := m.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, want, webrtcFingerprintLine.FindAllString(string(answer), -1))
}
