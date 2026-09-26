// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"crypto/tls"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/stretchr/testify/require"
)

// newDTLSForkTestSession builds one side of a DTLS-SRTP session without ICE,
// on the two socket layout, and closes it when the test ends. The role is left
// to the offer/answer exchange, as a dialog leaves it.
func newDTLSForkTestSession(t *testing.T, cert tls.Certificate) *MediaSession {
	t.Helper()
	s := &MediaSession{
		Codecs:    []Codec{CodecAudioUlaw},
		Mode:      sdp.ModeSendrecv,
		SecureRTP: SecureRTPModeDTLS,
		DTLSConf:  DTLSConfig{Certificates: []tls.Certificate{cert}},
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
	}
	require.NoError(t, s.Init())
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// finalizeBoth runs Finalize on both sessions at once, since each side of a
// handshake waits for the other, and fails the test when either fails or does
// not return in time.
func finalizeBoth(t *testing.T, a, b *MediaSession) {
	t.Helper()
	errCh := make(chan error, 2)
	go func() { errCh <- a.Finalize() }()
	go func() { errCh <- b.Finalize() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err, "DTLS negotiation failed")
		case <-time.After(20 * time.Second):
			t.Fatal("DTLS negotiation did not complete")
		}
	}
}

// negotiateDTLS runs an offer/answer exchange from offerer to answerer, the
// way a dialog runs it, and returns the offer as the answerer received it and
// the answer. mutateOffer, when not nil, rewrites the offer on its way. It does
// not finalize.
func negotiateDTLS(t *testing.T, offerer, answerer *MediaSession, mutateOffer func(string) string) (offer, answer []byte) {
	t.Helper()
	offer = offerer.LocalSDP()
	if mutateOffer != nil {
		offer = []byte(mutateOffer(string(offer)))
	}
	answerer.RemoteSDPIsAnswer = false
	require.NoError(t, answerer.RemoteSDP(offer))
	answer = answerer.LocalSDP()
	offerer.RemoteSDPIsAnswer = true
	require.NoError(t, offerer.RemoteSDP(answer))
	return offer, answer
}

// newEstablishedDTLSPair negotiates and keys a DTLS-SRTP call without ICE.
func newEstablishedDTLSPair(t *testing.T, mutateOffer func(string) string) (offerer, answerer *MediaSession, offer, answer []byte) {
	t.Helper()
	offerer = newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer = newDTLSForkTestSession(t, testdata.ServerCertificate())
	offer, answer = negotiateDTLS(t, offerer, answerer, mutateOffer)
	finalizeBoth(t, offerer, answerer)
	return offerer, answerer, offer, answer
}

// rebindFork moves a fork to sockets of its own on the same address, as an
// offerer that wants a new association does (RFC 8842 section 5.1).
func rebindFork(t *testing.T, fork *MediaSession) {
	t.Helper()
	fork.Laddr = net.UDPAddr{IP: fork.Laddr.IP}
	require.NoError(t, fork.Init())
	t.Cleanup(func() { _ = fork.Close() })
}

// replaceSDPLine replaces the line of body that starts with prefix, and
// requires there is one.
func replaceSDPLine(t *testing.T, body string, prefix string, line string) string {
	t.Helper()
	old := sdpLine(t, []byte(body), prefix)
	return strings.Replace(body, old, line, 1)
}

// addSDPAttribute appends an attribute line to body.
func addSDPAttribute(body string, attr string) string {
	return body + attr + "\r\n"
}

// TestDTLSForkKeepsAssociation is a re-INVITE on an established DTLS-SRTP call
// without ICE that changes nothing RFC 8842 section 3.1 ties the association
// to. Each side forks, as a dialog does, and must carry on over the
// association it has: no handshake armed, the SRTP contexts kept, and the
// media encrypted both ways. Before, every such fork armed a handshake that no
// re-INVITE path runs, and held no keys, so the media after the re-INVITE went
// out and came in unencrypted.
func TestDTLSForkKeepsAssociation(t *testing.T) {
	cases := []struct {
		name string
		// mutateOffer rewrites the initial offer.
		mutateOffer func(string) string
		// reoffer is the subsequent offer, made by the offerer's side.
		reoffer func(offerer, offerFork *MediaSession) []byte
	}{
		{
			name: "re-offer from a fork",
			reoffer: func(_, offerFork *MediaSession) []byte {
				return offerFork.LocalSDP()
			},
		},
		{
			// diago's ReInvite offers the installed session's SDP, whose
			// a=setup is the role it plays rather than actpass.
			name: "re-offer from the established session",
			reoffer: func(offerer, _ *MediaSession) []byte {
				return offerer.LocalSDP()
			},
		},
		{
			// The association was built with the answerer passive, which a role
			// derived afresh from the actpass of the re-offer would change.
			name: "answerer passive",
			mutateOffer: func(s string) string {
				return strings.Replace(s, "a=setup:actpass", "a=setup:active", 1)
			},
			reoffer: func(_, offerFork *MediaSession) []byte {
				return offerFork.LocalSDP()
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offerer, answerer, _, answer := newEstablishedDTLSPair(t, tc.mutateOffer)
			establishedSetup := sdpLine(t, answer, "a=setup:")

			offerFork := offerer.Fork()
			answerFork := answerer.Fork()

			reoffer := tc.reoffer(offerer, offerFork)
			answerFork.RemoteSDPIsAnswer = false
			require.NoError(t, answerFork.RemoteSDP(reoffer))
			require.Nil(t, answerFork.onFinalize, "a re-offer continuing the association must not arm a handshake")
			require.NotNil(t, answerFork.localCtxSRTP, "the fork must keep the association's keys")
			require.NotNil(t, answerFork.remoteCtxSRTP, "the fork must keep the association's keys")
			require.Equal(t, answerer.Laddr, answerFork.Laddr, "the fork must stay on the association's transport")

			reanswer := answerFork.LocalSDP()
			require.Equal(t, establishedSetup, sdpLine(t, reanswer, "a=setup:"), "the answer must keep the negotiated role (RFC 8842 section 5.3)")

			offerFork.RemoteSDPIsAnswer = true
			require.NoError(t, offerFork.RemoteSDP(reanswer))
			require.Nil(t, offerFork.onFinalize, "an answer continuing the association must not arm a handshake")
			finalizeWithin(t, offerFork, 5*time.Second)
			finalizeWithin(t, answerFork, 5*time.Second)

			requireMediaDelivered(t, offerFork, answerFork, 21)
			requireMediaDelivered(t, answerFork, offerFork, 22)

			// Two forks that had both lost their keys would round trip in
			// plaintext. A peer that never forked still holds the association's
			// keys, so it only reads what the fork really encrypted, and the
			// fork only reads what it really decrypts.
			requireMediaDelivered(t, answerFork, offerer, 23)
			requireMediaDelivered(t, offerer, answerFork, 24)
		})
	}
}

// TestDTLSForkNewAssociation drives the offers RFC 8842 section 3.1 and
// section 4 tie to a new DTLS association, and the ones they do not. A fork
// answering one that does moves to sockets of its own, as section 5.1 has the
// answerer do, and arms the handshake; one answering any other keeps the
// association and its keys.
func TestDTLSForkNewAssociation(t *testing.T) {
	const tlsID = "a=tls-id:abc3de65cddef001be82"
	const otherTLSID = "a=tls-id:ffc3de65cddef001be99"

	cases := []struct {
		name        string
		mutateOffer func(string) string
		reoffer     func(t *testing.T, offer string) string
		wantNew     bool
	}{
		{
			name:    "unchanged",
			reoffer: func(t *testing.T, offer string) string { return offer },
		},
		{
			name: "roles",
			reoffer: func(t *testing.T, offer string) string {
				// The answerer took active against actpass; a peer now taking
				// active itself makes it passive.
				return replaceSDPLine(t, offer, "a=setup:", "a=setup:active")
			},
			wantNew: true,
		},
		{
			name: "peer fingerprint",
			reoffer: func(t *testing.T, offer string) string {
				fp, err := dtlsSHA256Fingerprint(testdata.ServerCertificate())
				require.NoError(t, err)
				return replaceSDPLine(t, offer, "a=fingerprint:", "a=fingerprint:SHA-256 "+fp)
			},
			wantNew: true,
		},
		{
			name: "peer fingerprint in another case",
			// Hash names and hex digits are case insensitive (RFC 8122
			// section 5), so this is the same fingerprint.
			reoffer: func(t *testing.T, offer string) string {
				line := sdpLine(t, []byte(offer), "a=fingerprint:")
				return strings.Replace(offer, line, strings.ToLower(line), 1)
			},
		},
		{
			name: "peer adds a fingerprint",
			reoffer: func(t *testing.T, offer string) string {
				fp, err := dtlsSHA256Fingerprint(testdata.ServerCertificate())
				require.NoError(t, err)
				return addSDPAttribute(offer, "a=fingerprint:SHA-256 "+fp)
			},
			wantNew: true,
		},
		{
			name:    "tls-id appears",
			reoffer: func(t *testing.T, offer string) string { return addSDPAttribute(offer, tlsID) },
			wantNew: true,
		},
		{
			name:        "tls-id changes",
			mutateOffer: func(s string) string { return addSDPAttribute(s, tlsID) },
			reoffer:     func(t *testing.T, offer string) string { return replaceSDPLine(t, offer, "a=tls-id:", otherTLSID) },
			wantNew:     true,
		},
		{
			// RFC 8842 section 4: an unchanged tls-id with unchanged
			// fingerprints reuses the association, wherever the peer moved.
			name:        "tls-id unchanged, peer transport changes",
			mutateOffer: func(s string) string { return addSDPAttribute(s, tlsID) },
			reoffer: func(t *testing.T, offer string) string {
				return replaceSDPLine(t, offer, "m=audio ", "m=audio 40000 UDP/TLS/RTP/SAVP 0")
			},
		},
		{
			// RFC 8842 section 4: a peer without tls-id asks for a new
			// association by changing its transport.
			name: "no tls-id, peer transport changes",
			reoffer: func(t *testing.T, offer string) string {
				return replaceSDPLine(t, offer, "m=audio ", "m=audio 40000 UDP/TLS/RTP/SAVP 0")
			},
			wantNew: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offerer, answerer, offer, _ := newEstablishedDTLSPair(t, tc.mutateOffer)

			fork := answerer.Fork()
			fork.RemoteSDPIsAnswer = false
			require.NoError(t, fork.RemoteSDP([]byte(tc.reoffer(t, string(offer)))))
			if !sameUDPAddr(fork.Laddr, answerer.Laddr) {
				t.Cleanup(func() { _ = fork.Close() })
			}

			if !tc.wantNew {
				require.False(t, fork.FinalizePending(), "the offer continues the association")
				require.Equal(t, answerer.Laddr, fork.Laddr)
				require.NotNil(t, fork.localCtxSRTP)
				require.NotNil(t, fork.remoteCtxSRTP)
				return
			}

			require.True(t, fork.FinalizePending(), "the offer asks for a new association, whose handshake must be armed")
			require.Nil(t, fork.localCtxSRTP, "the current association's keys are not the new one's")
			require.Nil(t, fork.remoteCtxSRTP, "the current association's keys are not the new one's")
			require.NotEqual(t, answerer.Laddr.Port, fork.Laddr.Port, "the answerer must move the new association to a transport of its own")
			require.Contains(t, string(fork.LocalSDP()), "m=audio "+strconv.Itoa(fork.Laddr.Port)+" ")

			// The current association is untouched, and still carries media.
			requireMediaDelivered(t, offerer, answerer, 31)
			requireMediaDelivered(t, answerer, offerer, 32)
		})
	}
}

// TestDTLSForkRunsNewAssociation runs a new DTLS association through to media:
// once for the call moving to another peer, which is a new certificate at a
// new address, and once for the roles changing between the same two sessions.
// The fork answering moves to sockets of its own; the media of the current
// association keeps flowing until the dialog swaps the fork in.
func TestDTLSForkRunsNewAssociation(t *testing.T) {
	t.Run("new peer", func(t *testing.T) {
		offerer, answerer, _, _ := newEstablishedDTLSPair(t, nil)

		cert, err := selfsign.GenerateSelfSigned()
		require.NoError(t, err)
		newPeer := newDTLSForkTestSession(t, cert)

		fork := answerer.Fork()
		negotiateDTLS(t, newPeer, fork, nil)
		t.Cleanup(func() { _ = fork.Close() })
		require.True(t, fork.FinalizePending())
		finalizeBoth(t, newPeer, fork)

		requireMediaDelivered(t, newPeer, fork, 41)
		requireMediaDelivered(t, fork, newPeer, 42)
		requireMediaDelivered(t, offerer, answerer, 43)
		requireMediaDelivered(t, answerer, offerer, 44)
	})

	t.Run("roles", func(t *testing.T) {
		offerer, answerer, _, _ := newEstablishedDTLSPair(t, nil)

		// The offerer wants the new association, so it allocates a new
		// transport for its offer (RFC 8842 section 5.1).
		offerFork := offerer.Fork()
		rebindFork(t, offerFork)
		answerFork := answerer.Fork()
		negotiateDTLS(t, offerFork, answerFork, func(s string) string {
			return replaceSDPLine(t, s, "a=setup:", "a=setup:active")
		})
		t.Cleanup(func() { _ = answerFork.Close() })
		require.True(t, answerFork.FinalizePending())
		require.True(t, offerFork.FinalizePending())
		finalizeBoth(t, offerFork, answerFork)

		requireMediaDelivered(t, offerFork, answerFork, 51)
		requireMediaDelivered(t, answerFork, offerFork, 52)
	})
}

// TestDTLSForkRefusesNewAssociationOnSharedTransport is an answer to our own
// re-offer, made on the transport of the current association, that asks for a
// new one. The peer is already sending to that transport, where the reader of
// the current association would take the handshake's packets, so the answer
// is refused rather than a handshake armed that could not complete.
func TestDTLSForkRefusesNewAssociationOnSharedTransport(t *testing.T) {
	offerer, answerer, _, _ := newEstablishedDTLSPair(t, nil)

	offerFork := offerer.Fork()
	reoffer := offerFork.LocalSDP()

	// The peer answers with a new certificate.
	cert, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	answerFork := answerer.Fork()
	answerFork.DTLSConf.Certificates = []tls.Certificate{cert}
	answerFork.RemoteSDPIsAnswer = false
	require.NoError(t, answerFork.RemoteSDP(reoffer))
	t.Cleanup(func() { _ = answerFork.Close() })

	offerFork.RemoteSDPIsAnswer = true
	err = offerFork.RemoteSDP(answerFork.LocalSDP())
	require.Error(t, err)
	require.False(t, offerFork.FinalizePending())
	require.Equal(t, offerer.Laddr, offerFork.Laddr)

	requireMediaDelivered(t, offerer, answerer, 61)
}
