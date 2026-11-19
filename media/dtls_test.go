// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"crypto/tls"
	"log/slog"
	"net"
	"testing"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/require"
)

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

func TestDTLSFingerprint(t *testing.T) {
	fingerprint, err := dtlsSHA256Fingerprint(testdata.ClientCertificate())
	require.NoError(t, err)
	t.Log(fingerprint)

	fingerprint, err = dtlsSHA256Fingerprint(testdata.ServerCertificate())
	require.NoError(t, err)
	t.Log(fingerprint)
}

// TestDTLSSDPHasNoConnectionAttribute pins RFC 5763 section 5: "The endpoint
// MUST NOT use the connection attribute defined in [RFC4145]." DTLS runs over
// UDP, where a=connection has no meaning, and RFC 8842 section 5.1 leaves it to
// the TCP and SCTP usages. Whether an association continues is told by the
// setup role, the fingerprints and the transport instead. It is checked on an
// offer, an answer and a subsequent offer, each of which still carries a=setup
// and a=fingerprint.
func TestDTLSSDPHasNoConnectionAttribute(t *testing.T) {
	offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
	offer, answer := negotiateDTLS(t, offerer, answerer, nil)
	finalizeBoth(t, offerer, answerer)

	bodies := map[string][]byte{
		"offer":            offer,
		"answer":           answer,
		"subsequent offer": offerer.Fork().LocalSDP(false),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			require.NotContains(t, string(body), "a=connection:")
			require.Contains(t, string(body), "a=setup:")
			require.Contains(t, string(body), "a=fingerprint:")
		})
	}
}
