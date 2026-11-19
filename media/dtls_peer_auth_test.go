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
func TestDTLSVerifyConnectionFingerprint(t *testing.T) {
	peer := testdata.ClientCertificate()
	leaf := peer.Certificate[0]
	state := &dtls.State{PeerCertificates: peer.Certificate}

	matching := testCertificateFingerprint(sha256.New(), leaf)
	other := testCertificateFingerprint(sha256.New(), testdata.ServerCertificate().Certificate[0])

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
	newSess := func(cert tls.Certificate) *MediaSession {
		t.Helper()
		s := &MediaSession{
			Codecs:    []Codec{CodecAudioUlaw},
			Mode:      sdp.ModeSendrecv,
			SecureRTP: SecureRTPModeDTLS,
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

	offer := string(newSess(testdata.ClientCertificate()).LocalSDP(false))
	fingerprintLine := regexp.MustCompile(`(?m)^a=fingerprint:.*\r\n`)
	require.Regexp(t, fingerprintLine, offer)

	tests := []struct {
		name    string
		offer   string
		wantErr bool
	}{
		{name: "fingerprint present", offer: offer},
		{name: "fingerprint missing", offer: fingerprintLine.ReplaceAllString(offer, ""), wantErr: true},
		{name: "only an unsupported hash", offer: fingerprintLine.ReplaceAllString(offer, "a=fingerprint:md5 00:11:22:33\r\n"), wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			answerer := newSess(testdata.ServerCertificate())
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
