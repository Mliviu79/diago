// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"crypto/tls"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/dtls/v3"
	"github.com/pion/ice/v4"
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

// newMuxedWebrtcSession returns a WebRTC session whose packet mux runs over
// conn, standing in for the selected ICE pair, without an agent: the DTLS
// transport of a session is the DTLS stream of that mux and nothing else of
// the session takes part in lending and retiring it.
func newMuxedWebrtcSession(t *testing.T, conn net.Conn) *MediaSessionWebrtc {
	t.Helper()
	m := &MediaSessionWebrtc{}
	m.mux = newWebRTCPacketMux(conn)
	t.Cleanup(func() { _ = m.mux.Close() })
	return m
}

// lendDTLSTransport calls dtlsTransport as RemoteSDP does, under the session's
// lock.
func lendDTLSTransport(m *MediaSessionWebrtc) *dtlsKeyExchangeConn {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.dtlsTransport()
}

// TestDTLSTeardownLeavesSessionTransportOpen asserts the DTLS stack cannot take
// the media transport down with it.
//
// RFC 5764 section 5.1.2 multiplexes DTLS onto the transport that carries
// media, but the DTLS stack still treats that transport as its own: every
// teardown path ends in nextConn.Close(). A fatal alert or a peer hangup must
// not take RTP and RTCP with it and silence a call that is otherwise healthy.
func TestDTLSTeardownLeavesSessionTransportOpen(t *testing.T) {
	local, remote := udpConnPair(t)
	peer := remote.(*udpPeer).UDPConn

	m := newMuxedWebrtcSession(t, local)

	// The peer's DTLS client answers the close_notify below by closing its conn,
	// and media is sent from that same socket afterwards, so it is only lent.
	server := dtlsHandshakeOverTransport(t, lendDTLSTransport(m), lentPacketConn{peer}, remote.LocalAddr())

	// close_notify is the gentlest of the teardowns that reach nextConn.Close();
	// a fatal alert arrives at the same place via close(false).
	require.NoError(t, server.Close())

	// The selected pair must still carry media.
	rtpPkt := []byte{0x80, 8, 0x00, 0x01, 0xbe, 0xef}
	_, err := peer.WriteTo(rtpPkt, local.LocalAddr())
	require.NoError(t, err)
	assert.Equal(t, rtpPkt, readWithTimeout(t, m.mux.rtp),
		"RTP must survive DTLS teardown: the DTLS stack does not own the transport")
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

	offer := offerer.LocalSDP(false)
	require.Contains(t, string(offer), "a=fingerprint:")
	require.Contains(t, string(offer), "RTP/SAVP")
	require.NoError(t, answerer.RemoteSDP(offer, false))

	answer := answerer.LocalSDP(true)
	require.NoError(t, offerer.RemoteSDP(answer, true))

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
	tr := s.dtlsTransport()
	s.dtlsTr = tr

	s.dtlsConn, err = dtlsServer(tr, peer.LocalAddr(), []tls.Certificate{testdata.ServerCertificate()})
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

	m := newMuxedWebrtcSession(t, local)
	tr := lendDTLSTransport(m)

	m.dtlsConn = dtlsHandshakeOverTransport(t, tr, peer, remote.LocalAddr())
	require.NoError(t, m.retireDTLS(m.dtlsConn))

	// Nothing may follow the handshake on the wire.
	require.NoError(t, peer.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	buf := make([]byte, 1500)
	_, _, err := peer.ReadFrom(buf)
	require.Error(t, err, "retiring the DTLS association must not send the peer an alert")
	assert.True(t, os.IsTimeout(err), "expected no packet at all, got %v", err)
}

// TestDTLSTransportSelection asserts the DTLS handshake runs on the DTLS
// stream of the mux in a WebRTC session, and on the RTP socket in a session
// without ICE. Handing DTLS the RTP stream of the mux would starve the
// handshake, since records are routed away from it. Either transport is only
// lent to the DTLS stack, never surrendered.
func TestDTLSTransportSelection(t *testing.T) {
	newBoundSession := func(t *testing.T) *MediaSession {
		t.Helper()
		s := &MediaSession{
			Codecs:    []Codec{CodecAudioUlaw},
			Mode:      sdp.ModeSendrecv,
			SecureRTP: 2,
		}
		s.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0}
		require.NoError(t, s.createListeners(&s.Laddr))
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	t.Run("without ice uses rtp socket", func(t *testing.T) {
		s := newBoundSession(t)

		assert.Equal(t, s.rtpConn, s.dtlsTransport().PacketConn)
	})

	t.Run("with ice uses dtls stream of mux", func(t *testing.T) {
		local, _ := udpConnPair(t)
		m := newMuxedWebrtcSession(t, local)

		tr := lendDTLSTransport(m)
		assert.Equal(t, net.PacketConn(m.mux.dtls), tr.PacketConn)
		assert.NotEqual(t, net.PacketConn(m.mux.rtp), tr.PacketConn,
			"DTLS must not read the RTP stream, records are routed away from it")
	})

	// The socket belongs to MediaSession, so no DTLS teardown may close it.
	t.Run("close does not reach the session socket", func(t *testing.T) {
		s := newBoundSession(t)

		require.NoError(t, s.dtlsTransport().Close())

		_, err := s.rtpConn.WriteTo([]byte{0x80, 8}, &s.Laddr)
		assert.NoError(t, err, "RTP socket must still be usable after a DTLS close")
	})
}

// TestICEDTLSSRTPSession negotiates a full ICE + DTLS-SRTP session between two
// WebRTC sessions and asserts media flows and decrypts both ways once each
// side has retired its DTLS stack.
//
// Finalize keys SRTP from the handshake and then has no use for the DTLS
// association, so by the time it returns the stack's read loop and handshake
// goroutine must be gone, while the selected pair goes on carrying SRTP.
func TestICEDTLSSRTPSession(t *testing.T) {
	baseline := dtlsGoroutines()

	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Second)
	defer cancel()

	newSess := func(cert tls.Certificate) *MediaSessionWebrtc {
		t.Helper()
		m := &MediaSessionWebrtc{
			Codecs: []Codec{CodecAudioUlaw},
			Mode:   sdp.ModeSendrecv,
		}
		require.NoError(t, m.Init(ctx, MediaSessionWebrtcConfig{
			DTLS:            DTLSConfig{Certificates: []tls.Certificate{cert}},
			NetworkTypes:    []ice.NetworkType{ice.NetworkTypeUDP4},
			IncludeLoopback: true,
			InterfaceFilter: testLoopbackInterface,
		}))
		t.Cleanup(func() { _ = m.Close() })
		return m
	}

	offerer := newSess(testdata.ClientCertificate())
	answerer := newSess(testdata.ServerCertificate())

	// Offer/answer carries the ICE credentials and candidates both ways.
	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.Contains(t, string(offer), "a=ice-ufrag:")
	require.Contains(t, string(offer), "a=candidate:")
	require.Contains(t, string(offer), "a=rtcp-mux")
	require.Contains(t, string(offer), "UDP/TLS/RTP/SAVP")

	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.Contains(t, string(answer), "a=ice-ufrag:")
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))

	// Both sides must run connectivity checks and the handshake concurrently:
	// each blocks until the other answers.
	errCh := make(chan error, 2)
	go func() { errCh <- answerer.Finalize(ctx) }()
	go func() { errCh <- offerer.Finalize(ctx) }()
	for i := 0; i < 2; i++ {
		require.NoError(t, <-errCh, "ICE + DTLS negotiation failed")
	}

	// Both stacks are retired while the sessions are still up. Close does not
	// join the loops, so they retire on their own beat.
	require.Eventually(t, func() bool {
		return dtlsGoroutines() <= baseline
	}, 5*time.Second, 10*time.Millisecond,
		"DTLS goroutines outlived the handshake: they are still parked on the mux")

	// Media flows and decrypts over the pair, both ways.
	payload := []byte{0xd5, 0xd5, 0xd5, 0xd5, 0xd5, 0xd5, 0xd5, 0xd5}
	for _, dir := range []struct {
		name     string
		from, to *MediaSessionWebrtc
	}{
		{name: "offerer to answerer", from: offerer, to: answerer},
		{name: "answerer to offerer", from: answerer, to: offerer},
	} {
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
		require.NoError(t, dir.from.WriteRTP(pkt), dir.name)

		require.NoError(t, dir.to.StopRTP(1, 5*time.Second), dir.name)
		buf := make([]byte, RTPBufSize)
		got := rtp.Packet{}
		_, err := dir.to.ReadRTP(buf, &got)
		require.NoError(t, err, "%s: media must arrive and decrypt over the ICE pair", dir.name)

		assert.Equal(t, pkt.SSRC, got.SSRC, dir.name)
		assert.Equal(t, pkt.SequenceNumber, got.SequenceNumber, dir.name)
		assert.Equal(t, payload, got.Payload, "%s: payload must survive the SRTP round trip", dir.name)
	}
}
