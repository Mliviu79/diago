// SPDX-License-Identifier: MPL-2.0

package media

import (
	"crypto/tls"
	"net"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newDTLSRoleSession returns a DTLS-SRTP session on the loopback configured
// with nothing but a certificate, the way a dialog builds one.
func newDTLSRoleSession(t *testing.T, cert tls.Certificate) *MediaSession {
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

// requireDTLSHandshake runs Finalize on both sessions at once and requires
// both handshakes to complete.
func requireDTLSHandshake(t *testing.T, offerer, answerer *MediaSession) {
	t.Helper()
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
}

// sdpSetupLine returns the a=setup line of body.
func sdpSetupLine(t *testing.T, body []byte) string {
	t.Helper()
	line := regexp.MustCompile(`(?m)^a=setup:[^\r\n]*`).Find(body)
	require.NotNil(t, line, "the SDP has no a=setup line")
	return string(line)
}

// TestAnsweringOfferDistinguishesRoles pins what tells an applied offer from
// the answer to an offer of ours, which is what the role of a session in the
// exchange RemoteSDP last took part in is read from.
func TestAnsweringOfferDistinguishesRoles(t *testing.T) {
	t.Run("offering:_nothing_applied_yet", func(t *testing.T) {
		s := &MediaSession{}
		if s.answeringOffer() {
			t.Error("answeringOffer() = true before any remote SDP; there is no offer to mirror")
		}
	})

	t.Run("answering:_an_offer_was_applied", func(t *testing.T) {
		s := &MediaSession{remoteProto: "RTP/SAVPF"}
		s.remoteSDPIsAnswer = false
		if !s.answeringOffer() {
			t.Error("answeringOffer() = false after applying an offer; the answer would not mirror it")
		}
	})

	t.Run("offerer_reading_the_answer_must_not_mirror", func(t *testing.T) {
		// We offered, the peer answered. remoteProto is set, but we are not the
		// answerer -- our own next offer stays ours to choose.
		s := &MediaSession{remoteProto: "RTP/AVP"}
		s.remoteSDPIsAnswer = true
		if s.answeringOffer() {
			t.Error("answeringOffer() = true while reading an answer to our own offer")
		}
	})
}

// TestLocalDTLSSetupIsComplementaryToTheOffer drives the RFC 4145 section 4.1
// role table. active initiates, passive accepts, so an answer must be the
// complement of the offer or both endpoints take the same side.
func TestLocalDTLSSetupIsComplementaryToTheOffer(t *testing.T) {
	tests := []struct {
		name   string
		remote string
		want   string
		why    string
	}{
		{
			name:   "peer_initiates_so_we_accept",
			remote: "active",
			want:   "passive",
			why:    "RFC 4145 s4.1: active initiates, so the complement accepts",
		},
		{
			name:   "peer_accepts_so_we_initiate",
			remote: "passive",
			want:   "active",
			why:    "RFC 4145 s4.1: passive accepts, so the complement initiates",
		},
		{
			// Asterisk offers actpass. Answering passive, from a known remote
			// address, while acting as the client has both endpoints send a
			// ClientHello, and the peer aborts the handshake with
			// UnexpectedMessage.
			name:   "peer_defers_the_choice_so_we_take_it",
			remote: "actpass",
			want:   "active",
			why:    "RFC 5763 s5: setup:active is RECOMMENDED; passive stalls the handshake until the answer lands",
		},
		{
			name:   "the_value_is_read_in_any_case",
			remote: "ACTIVE",
			want:   "passive",
			why:    "the peer's value is compared without regard to case",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Raddr is set, which says nothing about the role.
			s := &MediaSession{
				SecureRTP: SecureRTPModeDTLS,
				Raddr:     net.UDPAddr{IP: net.ParseIP("194.102.34.49"), Port: 19132},
			}
			s.dtlsRemoteSetup = tt.remote
			if got := s.localDTLSSetup(true); got != tt.want {
				t.Errorf("localDTLSSetup(true) = %q, want %q (offer was %q)\n%s", got, tt.want, tt.remote, tt.why)
			}
		})
	}
}

// TestOffererAdvertisesActpass pins RFC 5763 s5: "The endpoint that is the
// offerer MUST use the setup attribute value of setup:actpass". An offer has
// nothing to be complementary to, and the peer must be left free to choose.
func TestOffererAdvertisesActpass(t *testing.T) {
	s := &MediaSession{SecureRTP: SecureRTPModeDTLS}
	if got := s.localDTLSSetup(false); got != "actpass" {
		t.Errorf("localDTLSSetup(false) = %q with no offer applied, want \"actpass\"", got)
	}

	// A known remote address means we are dialling someone, not that we are
	// answering them.
	s.Raddr = net.UDPAddr{IP: net.ParseIP("194.102.34.49"), Port: 19132}
	if got := s.localDTLSSetup(false); got != "actpass" {
		t.Errorf("localDTLSSetup(false) = %q, want \"actpass\"; a known remote address is not an offer to answer", got)
	}

	// Nor does a body applied earlier make an offer an answer: every offer,
	// a subsequent one included, is actpass (RFC 8842 section 5.5).
	for _, remote := range []string{"active", "passive", "actpass"} {
		s.remoteProto = "UDP/TLS/RTP/SAVP"
		s.dtlsRemoteSetup = remote
		if got := s.localDTLSSetup(false); got != "actpass" {
			t.Errorf("localDTLSSetup(false) = %q after a body of a=setup:%s, want \"actpass\"", got, remote)
		}
	}
}

// TestAdvertisedRoleAndActedRoleAgree states the invariant directly: whatever
// a=setup we put in the SDP commits us to a DTLS role, and the role we play
// must be that one. Advertising passive while sending the ClientHello is the
// contradiction that produces Alert Fatal: UnexpectedMessage.
func TestAdvertisedRoleAndActedRoleAgree(t *testing.T) {
	check := func(t *testing.T, remote, advertised string, actsAsClient bool) {
		t.Helper()
		switch advertised {
		case "active":
			if !actsAsClient {
				t.Errorf("remote=%q: advertised active but does not send the ClientHello; the peer waits forever", remote)
			}
		case "passive":
			if actsAsClient {
				t.Errorf("remote=%q: advertised passive but sends the ClientHello; both endpoints are clients", remote)
			}
		case "actpass":
			// Still the peer's choice, so we must not preempt it: RFC 5763 s5 has
			// the offerer ready to receive a ClientHello before the answer.
			if actsAsClient {
				t.Errorf("remote=%q: advertised actpass but already acts as client before the peer chose", remote)
			}
		default:
			t.Errorf("remote=%q: advertised %q, which is not an RFC 4145 setup value", remote, advertised)
		}
	}

	t.Run("answering", func(t *testing.T) {
		for _, remote := range []string{"active", "passive", "actpass", ""} {
			s := &MediaSession{
				SecureRTP:   SecureRTPModeDTLS,
				remoteProto: "UDP/TLS/RTP/SAVP",
				Raddr:       net.UDPAddr{IP: net.ParseIP("194.102.34.49"), Port: 19132},
			}
			s.dtlsRemoteSetup = remote
			check(t, remote, s.localDTLSSetup(true), s.dtlsActsAsClient())
		}
	})

	t.Run("offering", func(t *testing.T) {
		// Before the answer the offerer has advertised actpass and plays no
		// side yet. The answer then gives it one, and a description of the
		// session it makes afterwards carries that one.
		s := &MediaSession{
			SecureRTP: SecureRTPModeDTLS,
			Raddr:     net.UDPAddr{IP: net.ParseIP("194.102.34.49"), Port: 19132},
		}
		check(t, "", s.localDTLSSetup(false), s.dtlsActsAsClient())

		for _, remote := range []string{"active", "passive"} {
			s.remoteProto = "UDP/TLS/RTP/SAVP"
			s.remoteSDPIsAnswer = true
			s.dtlsRemoteSetup = remote
			check(t, remote, s.localDTLSSetup(true), s.dtlsActsAsClient())
		}
	})
}

// TestSDPSetupRoleOverrideStillWins pins that the explicit choice is not
// swallowed by the derivation where the choice is ours. It is public API and a
// caller that reaches for it means it.
func TestSDPSetupRoleOverrideStillWins(t *testing.T) {
	s := &MediaSession{
		SecureRTP: SecureRTPModeDTLS,
		DTLSConf: DTLSConfig{
			SDPSetupRole: func(bool) string { return "passive" },
		},
	}
	s.dtlsRemoteSetup = "actpass" // would otherwise derive active
	if got := s.localDTLSSetup(true); got != "passive" {
		t.Errorf("localDTLSSetup(true) = %q, want %q from the explicit override", got, "passive")
	}
	if s.dtlsActsAsClient() {
		t.Error("override says passive but the session acts as client; the override must govern both")
	}
}

// TestSDPSetupRoleYieldsToThePeersSide pins where SDPSetupRole is not asked: a
// peer that said active or passive has taken a side, and RFC 4145 section 4.1
// leaves us the other one. Taking the same side has both endpoints wait for a
// ClientHello, or both send one.
func TestSDPSetupRoleYieldsToThePeersSide(t *testing.T) {
	for _, tc := range []struct {
		name     string
		remote   string
		isAnswer bool
		override string
		want     string
	}{
		{name: "answering_active_with_active", remote: "active", override: "active", want: "passive"},
		{name: "answering_passive_with_passive", remote: "passive", override: "passive", want: "active"},
		{name: "answering_passive_with_actpass", remote: "passive", override: "actpass", want: "active"},
		// The offer RFC 5763 section 5 requires, answered passive: the offerer
		// is the one left to send the ClientHello.
		{name: "offered_actpass_answered_passive", remote: "passive", isAnswer: true, override: "actpass", want: "active"},
		{name: "offered_actpass_answered_active", remote: "active", isAnswer: true, override: "actpass", want: "passive"},
		{name: "offered_active_answered_passive", remote: "passive", isAnswer: true, override: "active", want: "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &MediaSession{
				SecureRTP:   SecureRTPModeDTLS,
				remoteProto: "UDP/TLS/RTP/SAVP",
				DTLSConf: DTLSConfig{
					SDPSetupRole: func(bool) string { return tc.override },
				},
			}
			s.remoteSDPIsAnswer = tc.isAnswer
			s.dtlsRemoteSetup = tc.remote
			assert.Equal(t, tc.want, s.localDTLSSetup(true), "the value describing the session")
			assert.Equal(t, tc.want == "active", s.dtlsActsAsClient(), "the role played")
		})
	}
}

// TestSDPSetupRoleIsToldWhetherWeOffer pins what the SDPSetupRole override is
// passed: whether this endpoint is the offerer of the exchange, decided as the
// derivation without the override decides it. A known remote address says
// nothing about that, since an offerer knows whom it is calling and an offerer
// reading the answer knows the answerer's address too.
func TestSDPSetupRoleIsToldWhetherWeOffer(t *testing.T) {
	raddr := net.UDPAddr{IP: net.ParseIP("194.102.34.49"), Port: 19132}
	offer := func(s *MediaSession) { s.localDTLSSetup(false) }
	answer := func(s *MediaSession) { s.localDTLSSetup(true) }
	// What RemoteSDP asks once it has read the body.
	play := func(s *MediaSession) { s.dtlsActsAsClient() }
	tests := []struct {
		name string
		s    *MediaSession
		// isAnswer is the offered argument of the RemoteSDP that applied a
		// body to s.
		isAnswer bool
		consult  func(*MediaSession)
		want     bool
	}{
		{name: "making_an_offer", s: &MediaSession{}, consult: offer, want: true},
		{name: "making_an_offer_to_a_known_address", s: &MediaSession{Raddr: raddr}, consult: offer, want: true},
		{name: "reading_an_offer", s: &MediaSession{remoteProto: "UDP/TLS/RTP/SAVP", Raddr: raddr}, consult: play, want: false},
		{name: "answering_an_offer", s: &MediaSession{remoteProto: "UDP/TLS/RTP/SAVP", Raddr: raddr}, consult: answer, want: false},
		{name: "reading_the_answer_to_our_offer", s: &MediaSession{remoteProto: "UDP/TLS/RTP/SAVP", Raddr: raddr}, isAnswer: true, consult: play, want: true},
		{name: "explicit_answerer", s: &MediaSession{DTLSRole: DTLSEndpointRoleAnswerer}, consult: offer, want: false},
		{name: "explicit_offerer", s: &MediaSession{DTLSRole: DTLSEndpointRoleOfferer, remoteProto: "UDP/TLS/RTP/SAVP", Raddr: raddr}, consult: answer, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			tt.s.SecureRTP = SecureRTPModeDTLS
			tt.s.remoteSDPIsAnswer = tt.isAnswer
			tt.s.DTLSConf.SDPSetupRole = func(offer bool) string {
				called = true
				if offer != tt.want {
					t.Errorf("SDPSetupRole(offer=%t), want offer=%t", offer, tt.want)
				}
				return "actpass"
			}
			tt.consult(tt.s)
			if !called {
				t.Fatal("SDPSetupRole was not consulted")
			}
		})
	}
}

// TestDTLSEndpointRole pins the offer/answer role of a session. Without an
// explicit DTLSRole the role follows the SDP applied so far. A known remote
// address says nothing about it: an offerer that has read the answer knows the
// answerer's address.
func TestDTLSEndpointRole(t *testing.T) {
	t.Run("follows_the_exchange", func(t *testing.T) {
		offerer := newDTLSRoleSession(t, testdata.ClientCertificate())
		answerer := newDTLSRoleSession(t, testdata.ServerCertificate())

		assert.Equal(t, DTLSEndpointRoleOfferer, offerer.dtlsEndpointRole(),
			"no SDP applied yet means we are offering")

		require.NoError(t, answerer.RemoteSDP(offerer.LocalSDP(false), false))
		assert.Equal(t, DTLSEndpointRoleAnswerer, answerer.dtlsEndpointRole(),
			"an applied offer makes us the answerer")

		require.NoError(t, offerer.RemoteSDP(answerer.LocalSDP(true), true))
		require.NotNil(t, offerer.Raddr.IP)
		assert.Equal(t, DTLSEndpointRoleOfferer, offerer.dtlsEndpointRole(),
			"the answer to our offer leaves us the offerer, although the remote address is now known")
	})

	t.Run("explicit_role_wins", func(t *testing.T) {
		s := &MediaSession{DTLSRole: DTLSEndpointRoleOfferer}
		s.Raddr = net.UDPAddr{IP: net.IPv4(10, 0, 0, 1), Port: 5000}
		assert.Equal(t, DTLSEndpointRoleOfferer, s.dtlsEndpointRole())

		s = &MediaSession{DTLSRole: DTLSEndpointRoleAnswerer}
		assert.Equal(t, DTLSEndpointRoleAnswerer, s.dtlsEndpointRole())
	})
}

// TestDTLSDefaultConfigOffererCompletesHandshake negotiates DTLS-SRTP between two
// sessions configured with nothing but a certificate, the way a dialog builds
// them. The offerer advertises actpass and the answerer takes active, as RFC
// 5763 section 5 recommends, so the offerer is the DTLS server. It checks the
// client's certificate against the answer's a=fingerprint, so it has to ask the
// client for one: a server that never sends a CertificateRequest gets no
// certificate and fails every handshake.
func TestDTLSDefaultConfigOffererCompletesHandshake(t *testing.T) {
	offerer := newDTLSRoleSession(t, testdata.ClientCertificate())
	answerer := newDTLSRoleSession(t, testdata.ServerCertificate())

	offer := offerer.LocalSDP(false)
	require.Contains(t, string(offer), "a=setup:actpass")
	require.NoError(t, answerer.RemoteSDP(offer, false))
	answer := answerer.LocalSDP(true)
	require.Contains(t, string(answer), "a=setup:active")
	require.NoError(t, offerer.RemoteSDP(answer, true))

	requireDTLSHandshake(t, offerer, answerer)

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

// TestDTLSSetupRoleChoicesCompleteHandshake runs the exchange for every value
// SDPSetupRole can choose on either side and requires the handshake to
// complete with the values the SDP carried: the side that advertised active
// sends the ClientHello, whichever of the two it is.
func TestDTLSSetupRoleChoicesCompleteHandshake(t *testing.T) {
	for _, tc := range []struct {
		name string
		// offer and answer are what SDPSetupRole returns on each side, empty
		// for a side that does not set it.
		offer, answer         string
		wantOffer, wantAnswer string
	}{
		{name: "defaults", wantOffer: "actpass", wantAnswer: "active"},
		{name: "answerer_chooses_passive", answer: "passive", wantOffer: "actpass", wantAnswer: "passive"},
		{name: "answerer_chooses_active", answer: "active", wantOffer: "actpass", wantAnswer: "active"},
		{name: "offerer_chooses_actpass,_answered_passive", offer: "actpass", answer: "passive", wantOffer: "actpass", wantAnswer: "passive"},
		{name: "offerer_chooses_active", offer: "active", wantOffer: "active", wantAnswer: "passive"},
		{name: "offerer_chooses_passive", offer: "passive", wantOffer: "passive", wantAnswer: "active"},
		{name: "both_choose_active", offer: "active", answer: "active", wantOffer: "active", wantAnswer: "passive"},
		{name: "both_choose_passive", offer: "passive", answer: "passive", wantOffer: "passive", wantAnswer: "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			offerer := newDTLSRoleSession(t, testdata.ClientCertificate())
			answerer := newDTLSRoleSession(t, testdata.ServerCertificate())
			if tc.offer != "" {
				offerer.DTLSConf.SDPSetupRole = func(offer bool) string {
					assert.True(t, offer, "the offerer was told it answers")
					return tc.offer
				}
			}
			if tc.answer != "" {
				answerer.DTLSConf.SDPSetupRole = func(offer bool) string {
					assert.False(t, offer, "the answerer was told it offers")
					return tc.answer
				}
			}

			offer := offerer.LocalSDP(false)
			require.Equal(t, "a=setup:"+tc.wantOffer, sdpSetupLine(t, offer))
			require.NoError(t, answerer.RemoteSDP(offer, false))
			answer := answerer.LocalSDP(true)
			require.Equal(t, "a=setup:"+tc.wantAnswer, sdpSetupLine(t, answer))
			require.NoError(t, offerer.RemoteSDP(answer, true))

			requireDTLSHandshake(t, offerer, answerer)
		})
	}
}

// TestDTLSRemoteSDPRefusesUnknownSetup pins the a=setup values RemoteSDP
// takes: active, passive and actpass in any case, and nothing else. RFC 8842
// section 5.1 forbids holdconn for a DTLS association.
func TestDTLSRemoteSDPRefusesUnknownSetup(t *testing.T) {
	offer := string(newDTLSRoleSession(t, testdata.ClientCertificate()).LocalSDP(false))
	require.Contains(t, offer, "a=setup:actpass\r\n")

	for _, tc := range []struct {
		setup   string
		wantErr string
	}{
		{setup: "actpass"},
		{setup: "ActPass"},
		{setup: "active"},
		{setup: "passive"},
		{setup: "holdconn", wantErr: "unknown setup value"},
		{setup: "server", wantErr: "unknown setup value"},
	} {
		t.Run(tc.setup, func(t *testing.T) {
			answerer := newDTLSRoleSession(t, testdata.ServerCertificate())
			body := strings.Replace(offer, "a=setup:actpass\r\n", "a=setup:"+tc.setup+"\r\n", 1)
			err := answerer.RemoteSDP([]byte(body), false)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				assert.Empty(t, answerer.dtlsRemoteSetup, "a refused value was kept")
				return
			}
			require.NoError(t, err)
		})
	}
}

// TestDTLSRemoteSDPRefusesActpassAnswer pins that the answer to an offer of
// ours carrying a=setup:actpass is refused with ErrNoCommonCrypto before the
// session records it. RFC 5763 section 5 has the answerer "use either a setup
// attribute value of setup:active or setup:passive", and RFC 8842 section 5.3
// has it follow RFC 4145, whose answers to an offer carry no actpass. The
// answer with the side the answerer took is accepted, so the a=setup value is
// what it is refused for.
func TestDTLSRemoteSDPRefusesActpassAnswer(t *testing.T) {
	offerer := newDTLSRoleSession(t, testdata.ClientCertificate())
	answerer := newDTLSRoleSession(t, testdata.ServerCertificate())

	offer := offerer.LocalSDP(false)
	require.NoError(t, answerer.RemoteSDP(offer, false))
	answer := answerer.LocalSDP(true)
	require.Equal(t, "a=setup:active", sdpSetupLine(t, answer))

	for _, setup := range []string{"actpass", "ActPass"} {
		body := strings.Replace(string(answer), "a=setup:active", "a=setup:"+setup, 1)
		err := offerer.RemoteSDP([]byte(body), true)
		require.ErrorIs(t, err, ErrNoCommonCrypto, "a=setup:%s", setup)
		assert.Empty(t, offerer.dtlsRemoteSetup, "a refused value was kept")
	}

	require.NoError(t, offerer.RemoteSDP(answer, true))
	requireDTLSHandshake(t, offerer, answerer)
}
