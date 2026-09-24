// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// newICEForkTestSession builds and initialises one side of an ICE + DTLS-SRTP
// session, as TestICEDTLSSRTPSession does, and closes it when the test ends.
func newICEForkTestSession(t *testing.T, ip net.IP, role DTLSEndpointRole, cert tls.Certificate) *MediaSession {
	t.Helper()
	s := &MediaSession{
		Codecs:    []Codec{CodecAudioUlaw},
		Mode:      sdp.ModeSendrecv,
		SecureRTP: SecureRTPModeDTLS,
		ICEConf:   &ICEConfig{},
		DTLSRole:  role,
		DTLSConf: DTLSConfig{
			Certificates:     []tls.Certificate{cert},
			ServerClientAuth: ServerClientAuthRequireCert,
		},
	}
	s.Laddr = net.UDPAddr{IP: ip, Port: 0}
	require.NoError(t, s.Init())
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// newEstablishedICEPair negotiates an offerer and an answerer through ICE
// connectivity checks, the DTLS handshake and SRTP keying. mutateOffer, when
// not nil, rewrites the offer on its way to the answerer. It returns both
// sessions, the offer as the answerer received it, and the answer.
func newEstablishedICEPair(t *testing.T, mutateOffer func(string) string) (offerer, answerer *MediaSession, offer, answer []byte) {
	t.Helper()
	ip := iceTestIP(t)

	offerer = newICEForkTestSession(t, ip, DTLSEndpointRoleOfferer, testdata.ClientCertificate())
	answerer = newICEForkTestSession(t, ip, DTLSEndpointRoleAnswerer, testdata.ServerCertificate())

	offer = offerer.LocalSDP()
	if mutateOffer != nil {
		offer = []byte(mutateOffer(string(offer)))
	}
	require.NoError(t, answerer.RemoteSDP(offer))
	answer = answerer.LocalSDP()
	offerer.RemoteSDPIsAnswer = true
	require.NoError(t, offerer.RemoteSDP(answer))

	errCh := make(chan error, 2)
	go func() { errCh <- answerer.Finalize() }()
	go func() { errCh <- offerer.Finalize() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err, "ICE + DTLS negotiation failed")
		case <-time.After(40 * time.Second):
			t.Fatal("ICE + DTLS negotiation did not complete")
		}
	}
	return offerer, answerer, offer, answer
}

// remoteSDPNoPanic applies body and turns a panic into an error whose text
// begins "panic: ", so a crash is reported as a failed assertion instead of
// taking the test binary down.
func remoteSDPNoPanic(s *MediaSession, body []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return s.RemoteSDP(body)
}

// requireNoPanic fails when err is a panic recovered by remoteSDPNoPanic.
func requireNoPanic(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		require.False(t, strings.HasPrefix(err.Error(), "panic: "), "RemoteSDP panicked: %v", err)
	}
}

// sdpLine returns the first line of body starting with prefix.
func sdpLine(t *testing.T, body []byte, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(string(body), "\r\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("SDP has no %q line:\n%s", prefix, body)
	return ""
}

// finalizeWithin runs Finalize with a bound, so a fork that wrongly starts
// connectivity checks or a handshake fails instead of hanging.
func finalizeWithin(t *testing.T, s *MediaSession, d time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- s.Finalize() }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(d):
		t.Fatalf("Finalize did not return within %s", d)
	}
}

// requireMediaDelivered writes one RTP packet on from and requires to to read
// and decrypt it with the payload intact.
func requireMediaDelivered(t *testing.T, from, to *MediaSession, seq uint16) {
	t.Helper()
	payload := []byte{0xd5, 0xd5, 0xd5, 0xd5, 0xd5, 0xd5, 0xd5, byte(seq)}
	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    CodecAudioUlaw.PayloadType,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 160,
			SSRC:           0xfeedf00d + uint32(seq),
		},
		Payload: payload,
	}
	require.NoError(t, from.WriteRTP(pkt))

	require.NoError(t, to.rtpConn.SetReadDeadline(time.Now().Add(5*time.Second)))
	buf := make([]byte, RTPBufSize)
	got := rtp.Packet{}
	_, err := to.ReadRTP(buf, &got)
	require.NoError(t, err, "media must arrive and decrypt over the existing pair")
	require.Equal(t, pkt.SSRC, got.SSRC)
	require.Equal(t, pkt.SequenceNumber, got.SequenceNumber)
	require.Equal(t, payload, got.Payload, "payload must survive the SRTP round trip")
}

// TestICEForkSameCredentialsKeepsPair is the re-INVITE a peer sends on an
// established ICE + DTLS call with its credentials unchanged. The fork must
// accept it without touching an ICE agent, keep the nominated pair and the
// DTLS association, and carry media both ways with the keys already in use.
func TestICEForkSameCredentialsKeepsPair(t *testing.T) {
	offerer, answerer, offer, _ := newEstablishedICEPair(t, nil)

	answerFork := answerer.Fork()
	requireNoError := func(err error) {
		t.Helper()
		requireNoPanic(t, err)
		require.NoError(t, err)
	}
	requireNoError(remoteSDPNoPanic(answerFork, offer))
	require.Same(t, answerer.iceMux, answerFork.iceMux, "the fork must keep the nominated pair")
	require.Equal(t, answerer.rtpConn, answerFork.rtpConn)
	finalizeWithin(t, answerFork, 5*time.Second)

	offerFork := offerer.Fork()
	require.True(t, offerFork.RemoteSDPIsAnswer)
	requireNoError(remoteSDPNoPanic(offerFork, answerFork.LocalSDP()))
	require.Same(t, offerer.iceMux, offerFork.iceMux)
	finalizeWithin(t, offerFork, 5*time.Second)

	requireMediaDelivered(t, offerFork, answerFork, 11)
	requireMediaDelivered(t, answerFork, offerFork, 12)

	// Two forks that had both lost their keys would round trip in plaintext.
	// A peer that never forked still holds the association's keys, so it only
	// reads what the fork really encrypted, and the fork only reads what it
	// really decrypts.
	require.NotNil(t, answerFork.localCtxSRTP)
	require.NotNil(t, answerFork.remoteCtxSRTP)
	requireMediaDelivered(t, answerFork, offerer, 13)
	requireMediaDelivered(t, offerer, answerFork, 14)
}

// TestICEForkReofferCarriesICEAttributes asserts a re-offer from a fork of an
// established session still describes the pair it continues: the same local
// credentials and candidates, rtcp-mux and the unchanged certificate
// fingerprint, with actpass as RFC 8842 section 5.5 asks of a subsequent offer.
func TestICEForkReofferCarriesICEAttributes(t *testing.T) {
	offerer, _, offer, _ := newEstablishedICEPair(t, nil)

	reoffer := offerer.Fork().LocalSDP()
	require.Contains(t, string(reoffer), sdpLine(t, offer, "a=ice-ufrag:")+"\r\n")
	require.Contains(t, string(reoffer), sdpLine(t, offer, "a=ice-pwd:")+"\r\n")
	require.Contains(t, string(reoffer), "a=candidate:")
	require.Contains(t, string(reoffer), "a=rtcp-mux")
	require.Contains(t, string(reoffer), sdpLine(t, offer, "a=fingerprint:")+"\r\n")
	require.Contains(t, string(reoffer), "a=setup:actpass")
}

// TestICEForkAnswerKeepsEstablishedDTLSRole asserts that answering a
// subsequent actpass offer keeps the DTLS role the association was built with
// (RFC 8842 section 5.3). The pair is established with the answerer passive,
// which is the case a fresh derivation from actpass would get wrong.
func TestICEForkAnswerKeepsEstablishedDTLSRole(t *testing.T) {
	_, answerer, offer, answer := newEstablishedICEPair(t, func(s string) string {
		return strings.Replace(s, "a=setup:actpass", "a=setup:active", 1)
	})
	require.Contains(t, string(answer), "a=setup:passive")

	subsequentOffer := strings.Replace(string(offer), "a=setup:active", "a=setup:actpass", 1)
	fork := answerer.Fork()
	err := remoteSDPNoPanic(fork, []byte(subsequentOffer))
	requireNoPanic(t, err)
	require.NoError(t, err)

	reanswer := string(fork.LocalSDP())
	require.Contains(t, reanswer, "a=setup:passive")
	require.NotContains(t, reanswer, "a=setup:active")
}

// TestICEForkRefusesICERestart asserts that changed credentials, which is how
// a peer asks for an ICE restart, are refused with ErrICERestartUnsupported
// and never reach an agent. The error must not spell a credential.
func TestICEForkRefusesICERestart(t *testing.T) {
	_, answerer, offer, _ := newEstablishedICEPair(t, nil)

	oldUfragLine := sdpLine(t, offer, "a=ice-ufrag:")
	oldPwdLine := sdpLine(t, offer, "a=ice-pwd:")
	oldUfrag := strings.TrimPrefix(oldUfragLine, "a=ice-ufrag:")
	oldPwd := strings.TrimPrefix(oldPwdLine, "a=ice-pwd:")
	const newUfrag = "RstUfragZ9"
	const newPwd = "RestartPasswordValue0123456789"

	cases := map[string]string{
		"UfragAndPwd": strings.Replace(strings.Replace(string(offer),
			oldUfragLine, "a=ice-ufrag:"+newUfrag, 1),
			oldPwdLine, "a=ice-pwd:"+newPwd, 1),
		"PwdOnly": strings.Replace(string(offer), oldPwdLine, "a=ice-pwd:"+newPwd, 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			err := remoteSDPNoPanic(answerer.Fork(), []byte(body))
			requireNoPanic(t, err)
			require.True(t, errors.Is(err, ErrICERestartUnsupported), "want ErrICERestartUnsupported, got %v", err)
			for _, secret := range []string{newPwd, newUfrag, oldPwd, oldUfrag} {
				require.NotContains(t, err.Error(), secret, "the error must not carry a credential")
			}
		})
	}
}

// TestICEForkWithoutEstablishedPairFails asserts a fork of an ICE session that
// never completed connectivity checks has nothing to renegotiate over: it
// refuses the offer with a plain error, not the restart sentinel, and does not
// crash.
func TestICEForkWithoutEstablishedPairFails(t *testing.T) {
	ip := iceTestIP(t)
	local := newICEForkTestSession(t, ip, DTLSEndpointRoleAnswerer, testdata.ServerCertificate())
	peer := newICEForkTestSession(t, ip, DTLSEndpointRoleOfferer, testdata.ClientCertificate())

	fork := local.Fork()
	err := remoteSDPNoPanic(fork, peer.LocalSDP())
	require.Error(t, err)
	requireNoPanic(t, err)
	require.False(t, errors.Is(err, ErrICERestartUnsupported), "a pair-less fork is not a restart: %v", err)
	finalizeWithin(t, fork, 5*time.Second)
}
