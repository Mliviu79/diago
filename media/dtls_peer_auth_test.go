// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"fmt"
	"hash"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/dtls/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// dtlsServer and dtlsClient run a handshake outside SDP negotiation, where the
// peer has sent no fingerprint to check its certificate against, so they turn
// the fingerprint check off.
func dtlsServer(conn net.PacketConn, raddr net.Addr, certificates []tls.Certificate) (*dtls.Conn, error) {
	conf := DTLSConfig{
		Certificates: certificates,
	}
	libConf := conf.ToLibConf(nil)
	libConf.VerifyConnection = nil
	return dtls.Server(conn, raddr, libConf)
}

func dtlsClient(conn net.PacketConn, raddr net.Addr, certificates []tls.Certificate, serverName string) (*dtls.Conn, error) {
	// Client DTLS config
	conf := DTLSConfig{
		Certificates: certificates,
		ServerName:   serverName,
	}
	libConf := conf.ToLibConf(nil)
	libConf.VerifyConnection = nil
	return dtls.Client(conn, raddr, libConf)
}

// testCertificateFingerprint formats the digest of der the way a=fingerprint
// carries it, upper case hex separated by colons.
func testCertificateFingerprint(h hash.Hash, der []byte) string {
	h.Write(der)
	sum := h.Sum(nil)
	octets := make([]string, len(sum))
	for i, b := range sum {
		octets[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(octets, ":")
}

// TestDTLSVerifyConnectionFingerprint asserts the peer is accepted only when its
// certificate matches an a=fingerprint from its SDP (RFC 5763 section 5).
// Certificates here are self signed and nothing else authenticates them, so a
// certificate matching none of the fingerprints would let anyone on the media
// path take the DTLS association. Hash names are case insensitive (RFC 8122
// section 5) and browsers write them in lower case. Lower case hex is accepted
// too.
//
// Of several hash functions the certificate has to match under the strongest
// one offered. RFC 8122 section 5.1: "An endpoint MUST select the set of
// fingerprints that use its most preferred hash function (out of those offered
// by the peer) and verify that each certificate used matches one fingerprint
// out of that set." A match under a weaker one beside it does not count, a
// hash function fingerprints are not compared under is not one of those to
// choose from, and a peer that offers sha-1 alone is verified under it.
func TestDTLSVerifyConnectionFingerprint(t *testing.T) {
	peer := testdata.ClientCertificate()
	leaf := peer.Certificate[0]
	otherLeaf := testdata.ServerCertificate().Certificate[0]
	state := &dtls.State{PeerCertificates: peer.Certificate}

	matching := testCertificateFingerprint(sha256.New(), leaf)
	other := testCertificateFingerprint(sha256.New(), otherLeaf)
	matchingSHA1 := testCertificateFingerprint(sha1.New(), leaf)
	otherSHA1 := testCertificateFingerprint(sha1.New(), otherLeaf)
	matchingSHA384 := testCertificateFingerprint(sha512.New384(), leaf)
	otherSHA512 := testCertificateFingerprint(sha512.New(), otherLeaf)

	tests := []struct {
		name         string
		fingerprints []DTLSFingerprint
		wantErr      bool
	}{
		{name: "matching", fingerprints: []DTLSFingerprint{{Algorithm: "SHA-256", Value: matching}}},
		{name: "browser hash name", fingerprints: []DTLSFingerprint{{Algorithm: "sha-256", Value: matching}}},
		{name: "lower case hex", fingerprints: []DTLSFingerprint{{Algorithm: "SHA-256", Value: strings.ToLower(matching)}}},
		{name: "sha-1", fingerprints: []DTLSFingerprint{{Algorithm: "sha-1", Value: testCertificateFingerprint(sha1.New(), leaf)}}},
		{name: "sha-224", fingerprints: []DTLSFingerprint{{Algorithm: "sha-224", Value: testCertificateFingerprint(sha256.New224(), leaf)}}},
		{name: "sha-384", fingerprints: []DTLSFingerprint{{Algorithm: "sha-384", Value: testCertificateFingerprint(sha512.New384(), leaf)}}},
		{name: "sha-512", fingerprints: []DTLSFingerprint{{Algorithm: "sha-512", Value: testCertificateFingerprint(sha512.New(), leaf)}}},
		{name: "one of several", fingerprints: []DTLSFingerprint{{Algorithm: "sha-256", Value: other}, {Algorithm: "sha-256", Value: matching}}},
		{name: "another certificate", fingerprints: []DTLSFingerprint{{Algorithm: "sha-256", Value: other}}, wantErr: true},
		{name: "malformed", fingerprints: []DTLSFingerprint{{Algorithm: "sha-256", Value: "00:11"}}, wantErr: true},
		{name: "right digest under another hash name", fingerprints: []DTLSFingerprint{{Algorithm: "sha-1", Value: matching}}, wantErr: true},
		{name: "unsupported hash only", fingerprints: []DTLSFingerprint{{Algorithm: "md5", Value: matching}}, wantErr: true},
		{name: "StrongerHashBesideWeakerOfOther", fingerprints: []DTLSFingerprint{{Algorithm: "sha-1", Value: otherSHA1}, {Algorithm: "sha-256", Value: matching}}},
		{name: "WeakerHashOnlyStrongerOffered", fingerprints: []DTLSFingerprint{{Algorithm: "sha-1", Value: matchingSHA1}, {Algorithm: "sha-256", Value: other}}, wantErr: true},
		{name: "WeakerHashOnlyStrongerOfferedFirst", fingerprints: []DTLSFingerprint{{Algorithm: "SHA-256", Value: other}, {Algorithm: "SHA-1", Value: matchingSHA1}}, wantErr: true},
		{name: "SHA384OnlySHA512Offered", fingerprints: []DTLSFingerprint{{Algorithm: "sha-384", Value: matchingSHA384}, {Algorithm: "sha-512", Value: otherSHA512}}, wantErr: true},
		{name: "UnsupportedHashIsNotPreferred", fingerprints: []DTLSFingerprint{{Algorithm: "md5", Value: other}, {Algorithm: "sha-1", Value: matchingSHA1}}},
		{name: "EveryHashOfTheSameCertificate", fingerprints: []DTLSFingerprint{{Algorithm: "sha-1", Value: matchingSHA1}, {Algorithm: "sha-256", Value: matching}, {Algorithm: "sha-384", Value: matchingSHA384}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := dtlsVerifyConnection(state, tc.fingerprints)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)
		})
	}

	t.Run("no peer certificate", func(t *testing.T) {
		err := dtlsVerifyConnection(&dtls.State{}, []DTLSFingerprint{{Algorithm: "sha-256", Value: matching}})
		assert.Error(t, err)
	})
}

// TestDTLSPreferredFingerprintHash pins the order the hash function a
// certificate is verified under is chosen in: by strength, sha-512 first and
// sha-1 last, whatever the order and the case the peer wrote them in (RFC 8122
// section 5.1). MD5, MD2 and unknown names are never chosen, since
// fingerprints are not compared under them.
func TestDTLSPreferredFingerprintHash(t *testing.T) {
	tests := []struct {
		name    string
		offered []string
		want    string
	}{
		{name: "None", want: ""},
		{name: "SHA1Alone", offered: []string{"sha-1"}, want: "sha-1"},
		{name: "SHA224OverSHA1", offered: []string{"sha-1", "sha-224"}, want: "sha-224"},
		{name: "SHA256OverSHA224", offered: []string{"sha-256", "sha-224"}, want: "sha-256"},
		{name: "SHA384OverSHA256", offered: []string{"sha-256", "sha-384"}, want: "sha-384"},
		{name: "SHA512OverSHA384", offered: []string{"sha-384", "sha-512"}, want: "sha-512"},
		{name: "AllWeakestFirst", offered: []string{"sha-1", "sha-224", "sha-256", "sha-384", "sha-512"}, want: "sha-512"},
		{name: "UpperCase", offered: []string{"SHA-1", "SHA-256"}, want: "sha-256"},
		{name: "UnsupportedOnly", offered: []string{"md5", "md2", "sha3-256"}, want: ""},
		{name: "UnsupportedBesideSHA1", offered: []string{"md5", "sha-1", "sha3-512"}, want: "sha-1"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fingerprints := make([]DTLSFingerprint, len(tc.offered))
			for i, name := range tc.offered {
				fingerprints[i] = DTLSFingerprint{Algorithm: name, Value: "00"}
			}
			assert.Equal(t, tc.want, preferredDTLSFingerprintHash(fingerprints))
		})
	}
}

// TestDTLSMediaFingerprints pins which level's fingerprints apply to a media
// description. RFC 8122 section 5: "If it is a session-level attribute, it
// applies to all TLS sessions for which no media-level fingerprint attribute
// is defined." The media's own replace those of the session level and are
// never joined with them, whatever hash function either level uses.
func TestDTLSMediaFingerprints(t *testing.T) {
	session := []DTLSFingerprint{{Algorithm: "sha-256", Value: "AA"}, {Algorithm: "sha-512", Value: "BB"}}
	own := []DTLSFingerprint{{Algorithm: "sha-1", Value: "CC"}}

	assert.Equal(t, own, mediaDTLSFingerprints(session, own), "the media's own apply alone")
	assert.Equal(t, session, mediaDTLSFingerprints(session, nil), "the session level applies to media with none")
	assert.Equal(t, own, mediaDTLSFingerprints(nil, own))
	assert.Empty(t, mediaDTLSFingerprints(nil, nil))
}

// TestDTLSSessionHandshakeChecksPeerFingerprint asserts the handshake of a
// session without ICE verifies the peer's certificate against the fingerprints
// RFC 8122 gives it, and against no other its SDP carries:
//
//   - those of the strongest hash function offered (section 5.1), so a
//     certificate that matches only under a weaker one beside it fails;
//   - those of the audio section, and those of the session level only when
//     the section has none (section 5), so a certificate that matches only a
//     session-level fingerprint beside the section's own fails.
func TestDTLSSessionHandshakeChecksPeerFingerprint(t *testing.T) {
	peer := testdata.ClientCertificate()
	leaf := peer.Certificate[0]
	// The answerer's own certificate is the one the offerer does not present.
	otherLeaf := testdata.ServerCertificate().Certificate[0]
	sha256Line := sha256FingerprintLine(peer)
	sha1Line := "a=fingerprint:sha-1 " + testCertificateFingerprint(sha1.New(), leaf) + "\r\n"
	otherLine := sha256FingerprintLine(testdata.ServerCertificate())
	otherSHA1Line := "a=fingerprint:sha-1 " + testCertificateFingerprint(sha1.New(), otherLeaf) + "\r\n"

	tests := []struct {
		name    string
		rewrite func(offer string) string
		wantErr bool
	}{
		{name: "SHA1Alone", rewrite: func(offer string) string {
			return withFingerprintLines(offer, sha1Line)
		}},
		{name: "StrongerHashBesideWeakerOfOther", rewrite: func(offer string) string {
			return withFingerprintLines(offer, otherSHA1Line, sha256Line)
		}},
		{name: "OneOfTwoCertificatesUnderStrongestHash", rewrite: func(offer string) string {
			return withFingerprintLines(offer, otherLine, sha256Line)
		}},
		{name: "WeakerHashOnlyStrongerOffered", wantErr: true, rewrite: func(offer string) string {
			return withFingerprintLines(offer, sha1Line, otherLine)
		}},
		{name: "SessionLevelAlone", rewrite: func(offer string) string {
			return withSessionFingerprintLines(withFingerprintLines(offer), sha256Line)
		}},
		{name: "MediaLevelBesideOtherOnSession", rewrite: func(offer string) string {
			return withSessionFingerprintLines(offer, otherLine)
		}},
		{name: "SessionLevelBesideOtherOnMedia", wantErr: true, rewrite: func(offer string) string {
			return withSessionFingerprintLines(withFingerprintLines(offer, otherLine), sha256Line)
		}},
		{name: "StrongerHashOnSessionMediaHasItsOwn", rewrite: func(offer string) string {
			return withSessionFingerprintLines(withFingerprintLines(offer, sha1Line), otherLine)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			offerer := newDTLSForkTestSession(t, peer)
			answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
			negotiateDTLS(t, offerer, answerer, tc.rewrite)

			// Each side blocks until the other answers, so both run at once.
			offererErr := make(chan error, 1)
			answererErr := make(chan error, 1)
			go func() { offererErr <- offerer.FinalizeContext(ctx) }()
			go func() { answererErr <- answerer.FinalizeContext(ctx) }()
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

// TestDTLSHandshakeChecksPeerFingerprint asserts a real handshake enforces the
// SDP fingerprint: it completes against the certificate the SDP names and fails
// against any other.
func TestDTLSHandshakeChecksPeerFingerprint(t *testing.T) {
	serverFP, err := dtlsSHA256Fingerprint(testdata.ServerCertificate())
	require.NoError(t, err)
	otherFP, err := dtlsSHA256Fingerprint(testdata.ClientCertificate())
	require.NoError(t, err)

	tests := []struct {
		name        string
		fingerprint string
		wantErr     bool
	}{
		{name: "named certificate", fingerprint: serverFP},
		{name: "another certificate", fingerprint: otherFP, wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			serverSock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			require.NoError(t, err)
			clientSock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
			require.NoError(t, err)
			t.Cleanup(func() {
				_ = serverSock.Close()
				_ = clientSock.Close()
			})

			server, err := dtlsServer(serverSock, clientSock.LocalAddr(), []tls.Certificate{testdata.ServerCertificate()})
			require.NoError(t, err)
			t.Cleanup(func() { _ = server.Close() })

			conf := DTLSConfig{Certificates: []tls.Certificate{testdata.ClientCertificate()}}
			client, err := dtls.Client(clientSock, serverSock.LocalAddr(), conf.ToLibConf([]DTLSFingerprint{{Algorithm: "sha-256", Value: tc.fingerprint}}))
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			serverErr := make(chan error, 1)
			go func() { serverErr <- server.HandshakeContext(ctx) }()

			err = client.HandshakeContext(ctx)
			if tc.wantErr {
				assert.ErrorContains(t, err, "does not match any SDP fingerprint")
				assert.Error(t, <-serverErr, "the server must see the handshake fail too")
				return
			}
			require.NoError(t, err)
			require.NoError(t, <-serverErr)
		})
	}
}

// TestDTLSRemoteSDPRequiresFingerprint asserts DTLS media without an
// a=fingerprint the peer can be checked against is refused while the SDP is
// negotiated. RFC 5763 section 5 makes the attribute mandatory: it is what
// binds the self-signed certificate to the signalling, so without one any
// certificate would complete the handshake.
func TestDTLSRemoteSDPRequiresFingerprint(t *testing.T) {
	newSess := func(role DTLSEndpointRole, cert tls.Certificate) *MediaSession {
		t.Helper()
		s := &MediaSession{
			Codecs:    []Codec{CodecAudioUlaw},
			Mode:      sdp.ModeSendrecv,
			SecureRTP: SecureRTPModeDTLS,
			DTLSRole:  role,
			DTLSConf: DTLSConfig{
				Certificates:     []tls.Certificate{cert},
				ServerClientAuth: ServerClientAuthRequireCert,
			},
		}
		s.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
		require.NoError(t, s.Init())
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	offer := string(newSess(DTLSEndpointRoleOfferer, testdata.ClientCertificate()).LocalSDP(false))
	fingerprintLine := regexp.MustCompile(`(?m)^a=fingerprint:.*\r\n`)
	require.Regexp(t, fingerprintLine, offer)

	const md5Line = "a=fingerprint:md5 00:11:22:33\r\n"
	sha256Line := sha256FingerprintLine(testdata.ClientCertificate())

	tests := []struct {
		name    string
		offer   string
		wantErr bool
	}{
		{name: "fingerprint present", offer: offer},
		{name: "fingerprint missing", offer: fingerprintLine.ReplaceAllString(offer, ""), wantErr: true},
		{name: "only an unsupported hash", offer: fingerprintLine.ReplaceAllString(offer, md5Line), wantErr: true},
		{name: "SessionLevel", offer: withSessionFingerprintLines(fingerprintLine.ReplaceAllString(offer, ""), sha256Line)},
		{name: "SupportedOnMediaUnsupportedOnSession", offer: withSessionFingerprintLines(offer, md5Line)},
		// RFC 8122 section 5: the session-level fingerprint applies to media
		// with none of its own, so here nothing is left to check against.
		{name: "UnsupportedOnMediaSupportedOnSession", offer: withSessionFingerprintLines(fingerprintLine.ReplaceAllString(offer, md5Line), sha256Line), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			answerer := newSess(DTLSEndpointRoleAnswerer, testdata.ServerCertificate())
			err := answerer.RemoteSDP([]byte(tc.offer), false)
			if tc.wantErr {
				assert.ErrorContains(t, err, "fingerprint")
				return
			}
			assert.NoError(t, err)
		})
	}
}

// TestDTLSLibConfAlwaysChecksFingerprint asserts the DTLS configuration built
// for a negotiated session never skips the fingerprint check, not even with no
// fingerprints to check against. An empty list refuses every certificate.
func TestDTLSLibConfAlwaysChecksFingerprint(t *testing.T) {
	conf := DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}}
	libConf := conf.ToLibConf(nil)
	require.NotNil(t, libConf.VerifyConnection)

	state := &dtls.State{PeerCertificates: testdata.ClientCertificate().Certificate}
	assert.Error(t, libConf.VerifyConnection(state))
}

// TestDTLSLibConfRequiresClientCertificate pins what ServerClientAuth means for a
// session checking the peer against its SDP fingerprints: the server always
// requires the client's certificate, since without one there is nothing to
// check, and a stricter policy is kept.
func TestDTLSLibConfRequiresClientCertificate(t *testing.T) {
	tests := []struct {
		name string
		auth int
		want dtls.ClientAuthType
	}{
		{name: "default", auth: 0, want: dtls.RequireAnyClientCert},
		{name: "no cert", auth: ServerClientAuthNoCert, want: dtls.RequireAnyClientCert},
		{name: "require cert", auth: ServerClientAuthRequireCert, want: dtls.RequireAnyClientCert},
		{name: "stricter", auth: int(dtls.RequireAndVerifyClientCert), want: dtls.RequireAndVerifyClientCert},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			conf := DTLSConfig{
				Certificates:     []tls.Certificate{testdata.ServerCertificate()},
				ServerClientAuth: tc.auth,
			}
			assert.Equal(t, tc.want, conf.ToLibConf(nil).ClientAuth)
		})
	}
}

// TestDTLSSessionNeedsCertificate asserts a DTLS session without a usable
// certificate is refused by Init, before it binds a socket. Its SDP would carry
// a=setup and no a=fingerprint, which RFC 5763 section 5 forbids and a
// conformant peer refuses, and as the DTLS server it would have nothing to
// present, so it could only fail once the call was already answered.
func TestDTLSSessionNeedsCertificate(t *testing.T) {
	tests := []struct {
		name         string
		secure       int
		certificates []tls.Certificate
		wantErr      bool
	}{
		{name: "dtls without certificate", secure: SecureRTPModeDTLS, wantErr: true},
		{name: "dtls with an empty certificate", secure: SecureRTPModeDTLS, certificates: []tls.Certificate{{}}, wantErr: true},
		{name: "dtls with certificate", secure: SecureRTPModeDTLS, certificates: []tls.Certificate{testdata.ServerCertificate()}},
		{name: "plain rtp without certificate", secure: SecureRTPModeNone},
		{name: "sdes without certificate", secure: SecureRTPModeSDES},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &MediaSession{
				Codecs:    []Codec{CodecAudioUlaw},
				Mode:      sdp.ModeSendrecv,
				SecureRTP: tc.secure,
				DTLSConf:  DTLSConfig{Certificates: tc.certificates},
			}
			s.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
			err := s.Init()
			t.Cleanup(func() { _ = s.Close() })
			if tc.wantErr {
				require.ErrorContains(t, err, "certificate")
				assert.Nil(t, s.rtpConn, "a refused session must not bind a socket")
				return
			}
			require.NoError(t, err)
		})
	}
}
