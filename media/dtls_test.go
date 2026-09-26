// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"fmt"
	"hash"
	"log/slog"
	"net"
	"os"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/dtls/v3"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lentPacketConn hands a socket to a DTLS stack without handing it the socket's
// lifetime: Close is a no-op, and the socket is closed by whoever bound it. It
// is deliberately not dtlsKeyExchangeConn, which is what these tests exercise.
type lentPacketConn struct{ net.PacketConn }

func (lentPacketConn) Close() error { return nil }

// dtlsHandshakeOverTransport runs a real DTLS handshake between transport and a
// peer socket, and returns the local conn. The teardown paths under test are
// pion's own, so they have to be reached through a completed handshake rather
// than a hand built Conn.
//
// The peer's DTLS client owns peer: its teardown, which a close_notify from the
// local side triggers, closes it. A caller that still uses the socket after
// that passes it as a lentPacketConn.
func dtlsHandshakeOverTransport(t *testing.T, transport net.PacketConn, peer net.PacketConn, peerAddr net.Addr) *dtls.Conn {
	t.Helper()

	server, err := dtlsServer(transport, peerAddr, []tls.Certificate{testdata.ServerCertificate()})
	require.NoError(t, err)

	client, err := dtlsClient(peer, transport.LocalAddr(), []tls.Certificate{testdata.ClientCertificate()}, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })

	serverErr := make(chan error, 1)
	go func() { serverErr <- server.Handshake() }()
	require.NoError(t, client.Handshake())
	require.NoError(t, <-serverErr)

	return server
}

// TestDTLSTeardownLeavesSessionTransportOpen asserts the DTLS stack cannot take
// the media transport down with it.
//
// RFC 5764 section 5.1.2 multiplexes DTLS onto the socket that carries media,
// but the DTLS stack still treats that socket as its own: every teardown path
// ends in nextConn.Close(). Under ICE the socket is one stream of a mux, whose
// Close tears down the nominated pair, so a fatal alert or a peer hangup would
// take RTP and RTCP with it and silence a call that is otherwise healthy.
func TestDTLSTeardownLeavesSessionTransportOpen(t *testing.T) {
	local, remote := udpConnPair(t)
	peer := remote.(*udpPeer).UDPConn

	s := newICEBindSession(t)
	s.iceMux = newICEMux(local, remote.LocalAddr())
	t.Cleanup(func() { _ = s.iceMux.Close() })
	s.rtpConn = s.iceMux.rtp

	// The peer's DTLS client answers the close_notify below by closing its conn,
	// and media is sent from that same socket afterwards, so it is only lent.
	server := dtlsHandshakeOverTransport(t, s.dtlsTransport(), lentPacketConn{peer}, remote.LocalAddr())

	// close_notify is the gentlest of the teardowns that reach nextConn.Close();
	// a fatal alert arrives at the same place via close(false).
	require.NoError(t, server.Close())

	// The nominated pair must still carry media.
	rtpPkt := []byte{0x80, 8, 0x00, 0x01, 0xbe, 0xef}
	_, err := peer.WriteTo(rtpPkt, local.LocalAddr())
	require.NoError(t, err)
	assert.Equal(t, rtpPkt, readWithTimeout(t, s.iceMux.rtp),
		"RTP must survive DTLS teardown: the DTLS stack does not own the transport")
}

func TestDTLSSetup(t *testing.T) {
	clientAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 15333}
	serverAddr := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 15444}
	slog.SetLogLoggerLevel(slog.LevelDebug)

	listener, err := net.ListenUDP("udp", serverAddr)
	require.NoError(t, err)
	defer listener.Close()

	serverConn, err := dtlsServer(listener, clientAddr, []tls.Certificate{testdata.ServerCertificate()})
	require.NoError(t, err)
	defer serverConn.Close()

	listenerClient, err := net.ListenUDP("udp", clientAddr)
	if err != nil {
		panic(err)
	}
	defer listenerClient.Close()

	clientConn, err := dtlsClient(listenerClient, serverAddr, []tls.Certificate{testdata.ClientCertificate()}, "")
	require.NoError(t, err)
	defer clientConn.Close()

	serverErr := make(chan error)
	go func() {
		serverErr <- serverConn.Handshake()
	}()
	err = clientConn.Handshake()
	require.NoError(t, err)
	require.NoError(t, <-serverErr)
}

// TestDTLSSRTPSessionWithoutICE negotiates a full DTLS-SRTP session on the two
// socket layout and asserts every media packet survives the handshake.
//
// RFC 5764 section 5.1.2 puts the handshake and SRTP on one socket, so once the
// keying material is exported the DTLS stack has to stop reading: its read loop
// and ReadRTP are otherwise two consumers of the same socket, and whatever the
// handshake loop takes is discarded as a malformed record per RFC 6347 section
// 4.1.2.7. The loss is silent, which is why it is asserted on a packet count
// rather than on a single packet.
func TestDTLSSRTPSessionWithoutICE(t *testing.T) {
	newSess := func(role DTLSEndpointRole, cert tls.Certificate) *MediaSession {
		t.Helper()
		s := &MediaSession{
			Codecs:    []Codec{CodecAudioUlaw},
			Mode:      sdp.ModeSendrecv,
			SecureRTP: SecureRTPModeDTLS,
			DTLSRole:  role,
			DTLSConf: DTLSConfig{
				Certificates: []tls.Certificate{cert},
				// RFC 5763 section 5: the a=fingerprint binds the certificate to
				// the signalling, so the server has to ask for the peer
				// certificate to have anything to verify it against.
				ServerClientAuth: ServerClientAuthRequireCert,
			},
		}
		s.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
		require.NoError(t, s.Init())
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	offerer := newSess(DTLSEndpointRoleOfferer, testdata.ClientCertificate())
	answerer := newSess(DTLSEndpointRoleAnswerer, testdata.ServerCertificate())

	offer := offerer.LocalSDP()
	require.Contains(t, string(offer), "a=fingerprint:")
	require.Contains(t, string(offer), "RTP/SAVP")
	require.NoError(t, answerer.RemoteSDP(offer))

	answer := answerer.LocalSDP()
	require.NoError(t, offerer.RemoteSDP(answer))

	// Each side blocks until the other answers, so both must run concurrently.
	errCh := make(chan error, 2)
	go func() { errCh <- answerer.Finalize() }()
	go func() { errCh <- offerer.Finalize() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err, "DTLS negotiation failed")
		case <-time.After(20 * time.Second):
			t.Fatal("DTLS negotiation did not complete")
		}
	}

	require.NotNil(t, offerer.localCtxSRTP, "offerer must have an SRTP send context")
	require.NotNil(t, answerer.remoteCtxSRTP, "answerer must have an SRTP receive context")

	// Nothing may consume media off the socket except ReadRTP.
	const packets = 20
	payload := []byte{0xd5, 0xd5, 0xd5, 0xd5, 0xd5, 0xd5, 0xd5, 0xd5}
	for i := 0; i < packets; i++ {
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    CodecAudioUlaw.PayloadType,
				SequenceNumber: uint16(1 + i),
				Timestamp:      uint32(160 * (1 + i)),
				SSRC:           0xdeadbeef,
			},
			Payload: payload,
		}
		require.NoError(t, offerer.WriteRTP(pkt))
	}

	buf := make([]byte, RTPBufSize)
	for i := 0; i < packets; i++ {
		require.NoError(t, answerer.rtpConn.SetReadDeadline(time.Now().Add(5*time.Second)))
		got := rtp.Packet{}
		_, err := answerer.ReadRTP(buf, &got)
		require.NoErrorf(t, err, "media packet %d did not arrive: the DTLS read loop is still on the socket", i+1)
		require.Equal(t, uint16(1+i), got.SequenceNumber, "packet %d was consumed by the DTLS read loop", i+1)
		require.Equal(t, payload, got.Payload, "payload must survive the SRTP round trip")
	}
}

// dtlsGoroutines counts goroutines parked anywhere in the DTLS stack. The count
// is used as a delta against a baseline rather than as an absolute, since the
// package's other tests leave conns draining.
func dtlsGoroutines() int {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]

	n := 0
	for _, g := range strings.Split(string(buf), "\n\n") {
		if strings.Contains(g, "pion/dtls") {
			n++
		}
	}
	return n
}

// TestDTLSRetiresItsGoroutinesAndLeavesTheSocket asserts a session hands the
// socket back and takes its goroutines with it.
//
// The DTLS stack parks a read loop and a handshake goroutine on the media
// socket. Both have to be retired once the keying material is exported, or
// every DTLS call leaks two goroutines parked on a socket for the life of the
// call, and the read loop keeps stealing SRTP the whole time. The socket itself
// must survive: SRTP is what it exists for.
func TestDTLSRetiresItsGoroutinesAndLeavesTheSocket(t *testing.T) {
	baseline := dtlsGoroutines()

	sock, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	defer sock.Close()

	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	// Closed explicitly below once it is no longer needed; this only covers an
	// early return.
	defer peer.Close()

	s := &MediaSession{}
	s.rtpConn = sock
	s.dtlsTr = s.dtlsTransport()

	s.dtlsConn, err = dtlsServer(s.dtlsTr, peer.LocalAddr(), []tls.Certificate{testdata.ServerCertificate()})
	require.NoError(t, err)

	client, err := dtlsClient(peer, sock.LocalAddr(), []tls.Certificate{testdata.ClientCertificate()}, "")
	require.NoError(t, err)

	serverErr := make(chan error, 1)
	go func() { serverErr <- s.dtlsConn.Handshake() }()
	require.NoError(t, client.Handshake())
	require.NoError(t, <-serverErr)

	require.NoError(t, s.retireDTLS())

	// The socket is MediaSession's: it must have survived, and nothing may be
	// left on it to intercept what arrives.
	rtpPkt := []byte{0x80, 8, 0x00, 0x01, 0xbe, 0xef}
	_, err = peer.WriteTo(rtpPkt, sock.LocalAddr())
	require.NoError(t, err)

	require.NoError(t, sock.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1500)
	n, _, err := sock.ReadFrom(buf)
	require.NoError(t, err, "the media socket must still read after DTLS retirement")
	require.Equal(t, rtpPkt, buf[:n], "the retired DTLS read loop took the packet")

	// The far end is scaffolding running a DTLS stack of its own, so it is
	// retired here to leave the session's goroutines as the only ones that could
	// stay above the baseline. Its socket is closed rather than its conn: a
	// close_notify would tear the session's read loop down for it and mask a
	// leak.
	require.NoError(t, peer.Close())

	// Close does not join the loops, so they retire on their own beat.
	require.Eventually(t, func() bool {
		return dtlsGoroutines() <= baseline
	}, 5*time.Second, 10*time.Millisecond,
		"DTLS goroutines outlived the handshake: they are still parked on the media socket")
}

// TestDTLSRetireIsInvisibleOnTheWire asserts retiring the association sends the
// peer nothing.
//
// pion emits a close_notify on Close. The peer's SRTP keys hang off its DTLS
// association, and RFC 5764 gives it no reason to expect ours to end while the
// call is up, so an alert would tell a WebRTC endpoint the transport is gone
// mid-call.
func TestDTLSRetireIsInvisibleOnTheWire(t *testing.T) {
	local, remote := udpConnPair(t)
	peer := remote.(*udpPeer).UDPConn

	s := &MediaSession{}
	s.iceMux = newICEMux(local, remote.LocalAddr())
	t.Cleanup(func() { _ = s.iceMux.Close() })
	s.rtpConn = s.iceMux.rtp
	s.dtlsTr = s.dtlsTransport()

	s.dtlsConn = dtlsHandshakeOverTransport(t, s.dtlsTr, peer, remote.LocalAddr())
	require.NoError(t, s.retireDTLS())

	// Nothing may follow the handshake on the wire.
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	buf := make([]byte, 1500)
	_, _, err := peer.ReadFrom(buf)
	require.Error(t, err, "retiring the DTLS association must not send the peer an alert")
	assert.True(t, os.IsTimeout(err), "expected no packet at all, got %v", err)
}

func TestDTLSFingerprint(t *testing.T) {
	fingerprint, err := dtlsSHA256Fingerprint(testdata.ClientCertificate())
	require.NoError(t, err)
	t.Log(fingerprint)

	fingerprint, err = dtlsSHA256Fingerprint(testdata.ServerCertificate())
	require.NoError(t, err)
	t.Log(fingerprint)
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
		fingerprints []sdpFingerprints
		wantErr      bool
	}{
		{name: "matching", fingerprints: []sdpFingerprints{{alg: "SHA-256", fingerprint: matching}}},
		{name: "browser hash name", fingerprints: []sdpFingerprints{{alg: "sha-256", fingerprint: matching}}},
		{name: "lower case hex", fingerprints: []sdpFingerprints{{alg: "SHA-256", fingerprint: strings.ToLower(matching)}}},
		{name: "sha-1", fingerprints: []sdpFingerprints{{alg: "sha-1", fingerprint: testCertificateFingerprint(sha1.New(), leaf)}}},
		{name: "sha-224", fingerprints: []sdpFingerprints{{alg: "sha-224", fingerprint: testCertificateFingerprint(sha256.New224(), leaf)}}},
		{name: "sha-384", fingerprints: []sdpFingerprints{{alg: "sha-384", fingerprint: testCertificateFingerprint(sha512.New384(), leaf)}}},
		{name: "sha-512", fingerprints: []sdpFingerprints{{alg: "sha-512", fingerprint: testCertificateFingerprint(sha512.New(), leaf)}}},
		{name: "one of several", fingerprints: []sdpFingerprints{{alg: "sha-256", fingerprint: other}, {alg: "sha-256", fingerprint: matching}}},
		{name: "another certificate", fingerprints: []sdpFingerprints{{alg: "sha-256", fingerprint: other}}, wantErr: true},
		{name: "malformed", fingerprints: []sdpFingerprints{{alg: "sha-256", fingerprint: "00:11"}}, wantErr: true},
		{name: "right digest under another hash name", fingerprints: []sdpFingerprints{{alg: "sha-1", fingerprint: matching}}, wantErr: true},
		{name: "unsupported hash only", fingerprints: []sdpFingerprints{{alg: "md5", fingerprint: matching}}, wantErr: true},
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
		err := dtlsVerifyConnection(&dtls.State{}, []sdpFingerprints{{alg: "sha-256", fingerprint: matching}})
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
			client, err := dtls.Client(clientSock, serverSock.LocalAddr(), conf.ToLibConf([]sdpFingerprints{{alg: "sha-256", fingerprint: tc.fingerprint}}))
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

	offer := string(newSess(DTLSEndpointRoleOfferer, testdata.ClientCertificate()).LocalSDP())
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
			answerer := newSess(DTLSEndpointRoleAnswerer, testdata.ServerCertificate())
			err := answerer.RemoteSDP([]byte(tc.offer))
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

// TestDTLSDefaultConfigOffererCompletesHandshake negotiates DTLS-SRTP between two
// sessions configured with nothing but a certificate, the way a dialog builds
// them. The offerer advertises actpass and the answerer takes active, as RFC
// 5763 section 5 recommends, so the offerer is the DTLS server. It checks the
// client's certificate against the answer's a=fingerprint, so it has to ask the
// client for one: a server that never sends a CertificateRequest gets no
// certificate and fails every handshake.
func TestDTLSDefaultConfigOffererCompletesHandshake(t *testing.T) {
	newSess := func(cert tls.Certificate) *MediaSession {
		t.Helper()
		s := &MediaSession{
			Codecs:    []Codec{CodecAudioUlaw},
			Mode:      sdp.ModeSendrecv,
			SecureRTP: SecureRTPModeDTLS,
			DTLSConf:  DTLSConfig{Certificates: []tls.Certificate{cert}},
		}
		s.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
		require.NoError(t, s.Init())
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	offerer := newSess(testdata.ClientCertificate())
	answerer := newSess(testdata.ServerCertificate())

	offer := offerer.LocalSDP()
	require.Contains(t, string(offer), "a=setup:actpass")
	require.NoError(t, answerer.RemoteSDP(offer))
	answer := answerer.LocalSDP()
	require.Contains(t, string(answer), "a=setup:active")
	// As applyRemoteSDP does for the answer to our own INVITE.
	offerer.RemoteSDPIsAnswer = true
	require.NoError(t, offerer.RemoteSDP(answer))

	errCh := make(chan error, 2)
	go func() { errCh <- answerer.Finalize() }()
	go func() { errCh <- offerer.Finalize() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err, "DTLS negotiation failed")
		case <-time.After(20 * time.Second):
			t.Fatal("DTLS negotiation did not complete")
		}
	}

	payload := []byte{0xd5, 0xd5, 0xd5, 0xd5}
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    CodecAudioUlaw.PayloadType,
			SequenceNumber: 1,
			Timestamp:      160,
			SSRC:           0xdeadbeef,
		},
		Payload: payload,
	}
	require.NoError(t, offerer.WriteRTP(pkt))
	require.NoError(t, answerer.rtpConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	got := rtp.Packet{}
	_, err := answerer.ReadRTP(make([]byte, RTPBufSize), &got)
	require.NoError(t, err, "media must arrive and decrypt")
	assert.Equal(t, payload, got.Payload)
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
