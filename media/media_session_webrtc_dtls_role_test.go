// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"crypto/tls"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSetupRoleWebrtcSession returns a WebRTC session that gathered its
// candidates on the loopback interface, presents certificate and lets
// setupRole, when it is not nil, choose its a=setup value.
func newSetupRoleWebrtcSession(ctx context.Context, t *testing.T, certificate tls.Certificate, setupRole func(offer bool) string) *MediaSessionWebrtc {
	t.Helper()
	m := &MediaSessionWebrtc{
		Codecs: []Codec{CodecAudioUlaw},
		Mode:   sdp.ModeSendrecv,
	}
	require.NoError(t, m.Init(ctx, MediaSessionWebrtcConfig{
		DTLS: DTLSConfig{
			Certificates: []tls.Certificate{certificate},
			SDPSetupRole: setupRole,
		},
		IPFamilies:      []ICEIPFamily{ICEIPFamilyIPv4},
		IncludeLoopback: true,
		InterfaceFilter: testLoopbackInterface,
	}))
	t.Cleanup(func() { _ = m.Close() })
	return m
}

// webrtcSetupRoleConfig returns the configuration of a session whose
// SDPSetupRole returns value for every exchange, or sets none when value is
// empty.
func webrtcSetupRoleConfig(value string) MediaSessionWebrtcConfig {
	if value == "" {
		return MediaSessionWebrtcConfig{}
	}
	return MediaSessionWebrtcConfig{DTLS: DTLSConfig{
		SDPSetupRole: func(bool) string { return value },
	}}
}

// withSetupLine returns body with its a=setup line carrying value.
func withSetupLine(t *testing.T, body []byte, value string) []byte {
	t.Helper()
	old := sdpSetupLine(t, body)
	return []byte(strings.Replace(string(body), old+"\r\n", "a=setup:"+value+"\r\n", 1))
}

// requireWebrtcMedia runs Finalize on both sessions at once, requires ICE and
// the DTLS handshake to complete on both, and then requires an RTP packet to
// arrive and decrypt in each direction. The handshake completes only when one
// session sent the ClientHello and the other waited for it, and a packet
// decrypts only when each took the half of the exported keys that belongs to
// the side it played.
func requireWebrtcMedia(ctx context.Context, t *testing.T, offerer, answerer *MediaSessionWebrtc) {
	t.Helper()
	// Each side blocks until the other answers, so both run at once.
	offererErr := make(chan error, 1)
	answererErr := make(chan error, 1)
	go func() { offererErr <- offerer.Finalize(ctx) }()
	go func() { answererErr <- answerer.Finalize(ctx) }()
	gotAnswerer, gotOfferer := <-answererErr, <-offererErr
	require.NoError(t, gotAnswerer, "the answerer's negotiation")
	require.NoError(t, gotOfferer, "the offerer's negotiation")

	for i, dir := range []struct {
		name     string
		from, to *MediaSessionWebrtc
	}{
		{name: "offerer to answerer", from: offerer, to: answerer},
		{name: "answerer to offerer", from: answerer, to: offerer},
	} {
		payload := []byte{0xd5, 0xd5, 0xd5, byte(i)}
		pkt := &rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    CodecAudioUlaw.PayloadType,
				SequenceNumber: 1,
				Timestamp:      160,
				SSRC:           0xdeadbeef + uint32(i),
			},
			Payload: payload,
		}
		require.NoError(t, dir.from.WriteRTP(pkt), dir.name)
		require.NoError(t, dir.to.StopRTP(1, 5*time.Second), dir.name)
		got := rtp.Packet{}
		_, err := dir.to.ReadRTP(make([]byte, RTPBufSize), &got)
		require.NoError(t, err, "%s: media must arrive and decrypt", dir.name)
		assert.Equal(t, payload, got.Payload, dir.name)
	}
}

// TestDTLSWebrtcSetupIsComplementaryToTheOffer drives the RFC 4145 section 4.1
// role table on a WebRTC session. active initiates, passive accepts, so an
// answer must be the complement of the offer or both endpoints take the same
// side.
func TestDTLSWebrtcSetupIsComplementaryToTheOffer(t *testing.T) {
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
			m := &MediaSessionWebrtc{}
			got, err := m.dtlsSetupAgainst(tt.remote, false)
			require.NoError(t, err)
			if got != tt.want {
				t.Errorf("dtlsSetupAgainst(%q, false) = %q, want %q\n%s", tt.remote, got, tt.want, tt.why)
			}
		})
	}
}

// TestDTLSWebrtcOffererAdvertisesActpass pins RFC 5763 s5: "The endpoint that
// is the offerer MUST use the setup attribute value of setup:actpass". An
// offer has nothing to be complementary to, and the peer must be left free to
// choose. A body applied earlier does not make an offer an answer: every
// offer, a subsequent one included, is actpass (RFC 8842 section 5.5).
func TestDTLSWebrtcOffererAdvertisesActpass(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	for _, remote := range []string{"", "active", "passive", "actpass"} {
		t.Run("after_"+remote, func(t *testing.T) {
			m := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			if remote != "" {
				peer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
				offer, err := peer.LocalSDP(ctx, false)
				require.NoError(t, err)
				require.NoError(t, m.RemoteSDP(ctx, withSetupLine(t, offer, remote), false))
			}
			offer, err := m.LocalSDP(ctx, false)
			require.NoError(t, err)
			assert.Equal(t, "a=setup:actpass", sdpSetupLine(t, offer))
		})
	}
}

// TestDTLSWebrtcSetupTakesOneSide states the invariant directly: whatever the
// peer's value, whichever of the two made the offer and whatever SDPSetupRole
// returns, a session that read the peer's value is on one side of the
// handshake, the client's or the server's. It is the value RemoteSDP builds
// the DTLS conn from and the value an answer carries, so the role advertised
// and the role played cannot disagree. Advertising passive while sending the
// ClientHello is the contradiction that produces Alert Fatal:
// UnexpectedMessage.
func TestDTLSWebrtcSetupTakesOneSide(t *testing.T) {
	for _, override := range []string{"", "active", "passive", "actpass", "holdconn"} {
		for _, remote := range []string{"active", "passive", "actpass"} {
			for _, offered := range []bool{false, true} {
				m := &MediaSessionWebrtc{Config: webrtcSetupRoleConfig(override)}
				got, err := m.dtlsSetupAgainst(remote, offered)
				require.NoError(t, err)
				assert.Contains(t, []string{"active", "passive"}, got,
					"remote=%q offered=%t override=%q", remote, offered, override)
			}
		}
	}
}

// TestDTLSWebrtcSetupRoleOverrideStillWins pins that the explicit choice is
// not swallowed by the derivation where the choice is ours. It is public API
// and a caller that reaches for it means it.
func TestDTLSWebrtcSetupRoleOverrideStillWins(t *testing.T) {
	m := &MediaSessionWebrtc{Config: webrtcSetupRoleConfig("passive")}
	// An offer of actpass would otherwise derive active.
	got, err := m.dtlsSetupAgainst("actpass", false)
	require.NoError(t, err)
	assert.Equal(t, "passive", got, "the override must govern the value written and the role played")

	// An offer is the other exchange that leaves a choice.
	m = &MediaSessionWebrtc{Config: webrtcSetupRoleConfig("active")}
	assert.Equal(t, "active", m.localDTLSSetup(false))
}

// TestDTLSWebrtcSetupRoleYieldsToThePeersSide pins where SDPSetupRole is not
// asked: a peer that said active or passive has taken a side, and RFC 4145
// section 4.1 leaves us the other one. Taking the same side has both endpoints
// wait for a ClientHello, or both send one.
func TestDTLSWebrtcSetupRoleYieldsToThePeersSide(t *testing.T) {
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
			asked := false
			m := &MediaSessionWebrtc{Config: MediaSessionWebrtcConfig{DTLS: DTLSConfig{
				SDPSetupRole: func(bool) string {
					asked = true
					return tc.override
				},
			}}}
			got, err := m.dtlsSetupAgainst(tc.remote, tc.isAnswer)
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
			assert.False(t, asked, "SDPSetupRole was asked although the peer took a side")
		})
	}
}

// TestDTLSWebrtcSetupRoleIsToldWhetherWeOffer pins what the SDPSetupRole
// override is passed: whether this endpoint is the offerer of the exchange,
// which the arguments of LocalSDP and RemoteSDP state.
func TestDTLSWebrtcSetupRoleIsToldWhetherWeOffer(t *testing.T) {
	tests := []struct {
		name    string
		consult func(t *testing.T, m *MediaSessionWebrtc)
		want    bool
	}{
		{name: "making_an_offer", want: true, consult: func(_ *testing.T, m *MediaSessionWebrtc) {
			m.localDTLSSetup(false)
		}},
		{name: "reading_an_offer", want: false, consult: func(t *testing.T, m *MediaSessionWebrtc) {
			_, err := m.dtlsSetupAgainst("actpass", false)
			require.NoError(t, err)
		}},
		{name: "reading_the_answer_to_our_offer", want: true, consult: func(t *testing.T, m *MediaSessionWebrtc) {
			_, err := m.dtlsSetupAgainst("actpass", true)
			require.NoError(t, err)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			m := &MediaSessionWebrtc{Config: MediaSessionWebrtcConfig{DTLS: DTLSConfig{
				SDPSetupRole: func(offer bool) string {
					called = true
					if offer != tt.want {
						t.Errorf("SDPSetupRole(offer=%t), want offer=%t", offer, tt.want)
					}
					return "actpass"
				},
			}}}
			tt.consult(t, m)
			if !called {
				t.Fatal("SDPSetupRole was not consulted")
			}
		})
	}
}

// TestDTLSWebrtcSetupRoleChoicesCompleteHandshake runs the exchange for every
// value SDPSetupRole can choose on either side and requires the handshake to
// complete, and media to decrypt both ways, with the values the SDP carried:
// the side that advertised active sends the ClientHello, whichever of the two
// it is. An offer of active is answered passive. An answer carries the side
// its session plays, so a choice of actpass for an answer is written passive:
// RFC 5763 section 5 has the answerer use active or passive.
func TestDTLSWebrtcSetupRoleChoicesCompleteHandshake(t *testing.T) {
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
		{name: "answerer_chooses_actpass", answer: "actpass", wantOffer: "actpass", wantAnswer: "passive"},
		{name: "offerer_chooses_actpass,_answered_passive", offer: "actpass", answer: "passive", wantOffer: "actpass", wantAnswer: "passive"},
		{name: "offerer_chooses_active", offer: "active", wantOffer: "active", wantAnswer: "passive"},
		{name: "offerer_chooses_passive", offer: "passive", wantOffer: "passive", wantAnswer: "active"},
		{name: "both_choose_active", offer: "active", answer: "active", wantOffer: "active", wantAnswer: "passive"},
		{name: "both_choose_passive", offer: "passive", answer: "passive", wantOffer: "passive", wantAnswer: "active"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			var offererRole, answererRole func(bool) string
			if tc.offer != "" {
				offererRole = func(offer bool) string {
					assert.True(t, offer, "the offerer was told it answers")
					return tc.offer
				}
			}
			if tc.answer != "" {
				answererRole = func(offer bool) string {
					assert.False(t, offer, "the answerer was told it offers")
					return tc.answer
				}
			}
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), offererRole)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), answererRole)

			offer, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			require.Equal(t, "a=setup:"+tc.wantOffer, sdpSetupLine(t, offer))
			require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			require.Equal(t, "a=setup:"+tc.wantAnswer, sdpSetupLine(t, answer))
			require.NoError(t, offerer.RemoteSDP(ctx, answer, true))

			requireWebrtcMedia(ctx, t, offerer, answerer)
		})
	}
}

// TestDTLSWebrtcAnswerOfActpassIsGivenASide pins what an offerer does with an
// answer that repeats actpass instead of taking a side, which RFC 5763 section
// 5 does not let an answerer do: the choice is still ours, so the session
// takes active, or what SDPSetupRole chooses, and the exchange is not refused
// for it.
func TestDTLSWebrtcAnswerOfActpassIsGivenASide(t *testing.T) {
	for _, tc := range []struct {
		override string
		want     string
	}{
		{override: "", want: "active"},
		{override: "active", want: "active"},
		{override: "passive", want: "passive"},
		// What an offerer's SDPSetupRole usually returns: it leaves the
		// session the server.
		{override: "actpass", want: "passive"},
	} {
		t.Run("override_"+tc.override, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			var role func(bool) string
			if tc.override != "" {
				role = func(bool) string { return tc.override }
			}
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), role)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)

			offer, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			require.NoError(t, answerer.RemoteSDP(ctx, withSetupLine(t, offer, "actpass"), false))
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)

			require.NoError(t, offerer.RemoteSDP(ctx, withSetupLine(t, answer, "actpass"), true))
			offerer.mu.Lock()
			got := offerer.localSetup
			offerer.mu.Unlock()
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestDTLSWebrtcRemoteSDPRefusesUnknownSetup pins the a=setup values RemoteSDP
// takes, in an offer and in an answer: active, passive and actpass in any
// case, and nothing else. RFC 8842 section 5.3 has an answerer "able to
// receive initial and subsequent offers with other attribute values" than
// actpass, and section 5.1 forbids holdconn for a DTLS association.
func TestDTLSWebrtcRemoteSDPRefusesUnknownSetup(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()

	body := []byte(webrtcOffer(ctx, t, testdata.ClientCertificate()))
	for _, tc := range []struct {
		setup   string
		wantErr bool
	}{
		{setup: "actpass"},
		{setup: "ActPass"},
		{setup: "active"},
		{setup: "passive"},
		{setup: "holdconn", wantErr: true},
		{setup: "server", wantErr: true},
	} {
		for _, offered := range []bool{false, true} {
			kind := "offer"
			if offered {
				kind = "answer"
			}
			t.Run(tc.setup+"_in_"+kind, func(t *testing.T) {
				m := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
				err := m.RemoteSDP(ctx, withSetupLine(t, body, tc.setup), offered)
				if tc.wantErr {
					require.ErrorContains(t, err, `invalid DTLS setup role "`+tc.setup+`" in `+kind)
					m.mu.Lock()
					defer m.mu.Unlock()
					assert.Empty(t, m.localSetup, "a refused value gave the session a side")
					assert.Nil(t, m.dtlsConn, "a refused value built a DTLS conn")
					return
				}
				require.NoError(t, err)
			})
		}
	}
}

// TestDTLSWebrtcSubsequentSDPKeepsTheRole pins the side a session keeps for a
// description of the ICE generation it already negotiated, which continues
// the DTLS association: RFC 8842 section 5.3 has the answer to such an offer
// carry a value "that does not change the previously negotiated DTLS roles".
// A peer that says actpass again leaves the side as it is, without
// SDPSetupRole being asked for another one; a peer that states the side it
// has is taken at its word; a peer that takes the other side asks for an
// association this session builds only with a new ICE generation.
func TestDTLSWebrtcSubsequentSDPKeepsTheRole(t *testing.T) {
	// first is the a=setup value of the peer's first description, which
	// isAnswer says is an answer to our offer, and want the side it leaves us.
	for _, tc := range []struct {
		name     string
		isAnswer bool
		first    string
		want     string
	}{
		{name: "answerer_that_took_active", first: "actpass", want: "active"},
		{name: "answerer_left_passive", first: "active", want: "passive"},
		{name: "offerer_left_active", isAnswer: true, first: "passive", want: "active"},
		{name: "offerer_left_passive", isAnswer: true, first: "active", want: "passive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()

			// Once the association is up the choice would go the other way.
			choice := tc.want
			m := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), func(bool) string {
				return choice
			})
			if tc.isAnswer {
				_, err := m.LocalSDP(ctx, false)
				require.NoError(t, err)
			}
			peer := []byte(webrtcOffer(ctx, t, testdata.ClientCertificate()))
			require.NoError(t, m.RemoteSDP(ctx, withSetupLine(t, peer, tc.first), tc.isAnswer))
			m.mu.Lock()
			require.Equal(t, tc.want, m.localSetup)
			m.mu.Unlock()

			other := "active"
			if tc.want == "active" {
				other = "passive"
			}
			choice = other

			// The peer leaves the choice open again, in an offer and in an
			// answer to an offer of ours.
			for _, offered := range []bool{false, true} {
				require.NoError(t, m.RemoteSDP(ctx, withSetupLine(t, peer, "actpass"), offered), "offered=%t", offered)
				if !offered {
					answer, err := m.LocalSDP(ctx, true)
					require.NoError(t, err)
					assert.Equal(t, "a=setup:"+tc.want, sdpSetupLine(t, answer))
				}
			}
			// The peer states the side it has.
			require.NoError(t, m.RemoteSDP(ctx, withSetupLine(t, peer, other), false))
			// The peer takes ours.
			err := m.RemoteSDP(ctx, withSetupLine(t, peer, tc.want), false)
			require.ErrorContains(t, err, "remote DTLS role changed without an ICE restart")

			m.mu.Lock()
			defer m.mu.Unlock()
			assert.Equal(t, tc.want, m.localSetup)
		})
	}
}
