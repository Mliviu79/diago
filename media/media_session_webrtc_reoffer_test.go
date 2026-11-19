// SPDX-License-Identifier: MPL-2.0

package media

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/dtls/v3"
	"github.com/pion/ice/v4"
	"github.com/pion/srtp/v3"
	"github.com/stretchr/testify/require"
)

// negotiateWebrtc runs an offer/answer exchange from offerer to answerer, the
// way a dialog runs the first one of a call, and returns the offer as the
// answerer received it and the answer. mutateOffer, when not nil, rewrites the
// offer on its way. It does not finalize.
func negotiateWebrtc(t *testing.T, offerer, answerer *MediaSessionWebrtc, mutateOffer func(string) string) (offer, answer []byte) {
	t.Helper()
	ctx := t.Context()
	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	if mutateOffer != nil {
		offer = []byte(mutateOffer(string(offer)))
	}
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	answer, err = answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))
	return offer, answer
}

// newEstablishedWebrtcPair negotiates an offerer and an answerer on the
// loopback interface through the ICE connectivity checks, the DTLS handshake
// and SRTP keying. mutateOffer, when not nil, rewrites the offer on its way to
// the answerer. It returns both sessions, the offer as the answerer received
// it, and the answer.
func newEstablishedWebrtcPair(t *testing.T, mutateOffer func(string) string) (offerer, answerer *MediaSessionWebrtc, offer, answer []byte) {
	t.Helper()
	ctx := t.Context()
	offerer = newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer = newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
	offer, answer = negotiateWebrtc(t, offerer, answerer, mutateOffer)
	requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE and DTLS negotiation")
	return offerer, answerer, offer, answer
}

// closeWebrtcWithTest closes m when the test ends, and fails the test when
// the session does not close cleanly.
func closeWebrtcWithTest(t *testing.T, m *MediaSessionWebrtc) {
	t.Helper()
	t.Cleanup(func() {
		if err := m.Close(); err != nil {
			t.Errorf("Close() = %v, want nil", err)
		}
	})
}

// finalizeWebrtcWithin runs Finalize with a bound, so a session that wrongly
// starts connectivity checks or a handshake fails instead of hanging.
func finalizeWebrtcWithin(t *testing.T, m *MediaSessionWebrtc, d time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- m.Finalize(t.Context()) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(d):
		t.Fatalf("Finalize did not return within %s", d)
	}
}

// exchangeWebrtcReOffer carries a re-offer from one negotiated session to the
// other, whose answer it hands back and returns, and commits the exchange on
// both, as the 2xx and the ACK of a re-INVITE do.
func exchangeWebrtcReOffer(t *testing.T, from, to *MediaSessionWebrtc, reoffer []byte) (reanswer []byte) {
	t.Helper()
	ctx := t.Context()
	require.NoError(t, to.RemoteSDP(ctx, reoffer, false))
	reanswer, err := to.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, from.RemoteSDP(ctx, reanswer, true))
	finalizeWebrtcWithin(t, from, 5*time.Second)
	finalizeWebrtcWithin(t, to, 5*time.Second)
	return reanswer
}

// webrtcTransport is what a negotiated session runs its media on: the ICE
// agent and the pair it selected, the DTLS association, and the SRTP and SRTCP
// contexts keyed from it.
type webrtcTransport struct {
	agent       *ice.Agent
	iceConn     *ice.Conn
	mux         *webRTCPacketMux
	dtlsConn    *dtls.Conn
	localSRTP   *srtp.Context
	remoteSRTP  *srtp.Context
	localSRTCP  *srtp.Context
	remoteSRTCP *srtp.Context
}

// webrtcTransportOf returns the transport m runs on now.
func webrtcTransportOf(m *MediaSessionWebrtc) webrtcTransport {
	m.mu.Lock()
	defer m.mu.Unlock()
	return webrtcTransport{
		agent:       m.agent,
		iceConn:     m.iceConn,
		mux:         m.mux,
		dtlsConn:    m.dtlsConn,
		localSRTP:   m.localCtxSRTP,
		remoteSRTP:  m.remoteCtxSRTP,
		localSRTCP:  m.localCtxSRTCP,
		remoteSRTCP: m.remoteCtxSRTCP,
	}
}

// requireSameWebrtcTransport requires m to run on the very transport it ran on
// when before was taken: the same agent, pair, DTLS association and SRTP
// contexts, none of them built again.
func requireSameWebrtcTransport(t *testing.T, before webrtcTransport, m *MediaSessionWebrtc) {
	t.Helper()
	now := webrtcTransportOf(m)
	require.Same(t, before.agent, now.agent, "the ICE agent")
	require.Same(t, before.iceConn, now.iceConn, "the selected pair")
	require.Same(t, before.mux, now.mux, "the packet mux")
	require.Same(t, before.dtlsConn, now.dtlsConn, "the DTLS association")
	require.Same(t, before.localSRTP, now.localSRTP, "the local SRTP context")
	require.Same(t, before.remoteSRTP, now.remoteSRTP, "the remote SRTP context")
	require.Same(t, before.localSRTCP, now.localSRTCP, "the local SRTCP context")
	require.Same(t, before.remoteSRTCP, now.remoteSRTCP, "the remote SRTCP context")
}

// webrtcDirection returns the direction m runs in and the one it asks for.
func webrtcDirection(m *MediaSessionWebrtc) (mode, preference string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.Mode, m.modePreference
}

// sdpLines returns every line of body that starts with prefix.
func sdpLines(body []byte, prefix string) []string {
	var lines []string
	for l := range strings.SplitSeq(string(body), "\r\n") {
		if strings.HasPrefix(l, prefix) {
			lines = append(lines, l)
		}
	}
	return lines
}

// sdpOriginOf returns the session id, the session version and the rest of the
// o= line of body.
func sdpOriginOf(t *testing.T, body []byte) (id, version int64, rest string) {
	t.Helper()
	fields := strings.SplitN(sdpLine(t, body, "o="), " ", 4)
	require.Len(t, fields, 4, "the o= line of:\n%s", body)
	id, err := strconv.ParseInt(fields[1], 10, 64)
	require.NoError(t, err)
	version, err = strconv.ParseInt(fields[2], 10, 64)
	require.NoError(t, err)
	return id, version, fields[0] + " " + fields[3]
}

// TestICEWebrtcReOfferKeepsPair is the re-INVITE one side sends on an
// established ICE + DTLS call, and the other receives with its credentials
// unchanged. Neither side may touch its ICE agent, start a handshake or build
// SRTP contexts: both keep the nominated pair and the DTLS association, and
// carry media both ways with the keys already in use.
func TestICEWebrtcReOfferKeepsPair(t *testing.T) {
	ctx := t.Context()
	offerer, answerer, _, _ := newEstablishedWebrtcPair(t, nil)
	offererBefore, answererBefore := webrtcTransportOf(offerer), webrtcTransportOf(answerer)

	reoffer, err := offerer.ReOffer("")
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, reoffer, false))
	require.False(t, answerer.FinalizePending(), "the answerer has no negotiation to run")
	reanswer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	finalizeWebrtcWithin(t, answerer, 5*time.Second)
	requireSameWebrtcTransport(t, answererBefore, answerer)

	// The offerer has not read the answer yet. Had the answerer keyed itself
	// anew, nothing would decrypt between it and a peer that still holds the
	// keys of the association, in either direction.
	requireWebrtcSRTPBetween(t, answerer, offerer, 11)
	requireWebrtcSRTPBetween(t, offerer, answerer, 12)

	require.NoError(t, offerer.RemoteSDP(ctx, reanswer, true))
	require.False(t, offerer.FinalizePending(), "the offerer has no negotiation to run")
	finalizeWebrtcWithin(t, offerer, 5*time.Second)
	requireSameWebrtcTransport(t, offererBefore, offerer)

	requireWebrtcSRTPBetween(t, offerer, answerer, 13)
	requireWebrtcSRTPBetween(t, answerer, offerer, 14)
}

// TestICEWebrtcReOfferCarriesICEAttributes asserts a re-offer on an
// established session still describes the pair it continues: the same local
// credentials and candidates, rtcp-mux, the unchanged certificate fingerprint
// and tls-id, with actpass as RFC 8842 section 5.5 asks of a subsequent offer.
func TestICEWebrtcReOfferCarriesICEAttributes(t *testing.T) {
	offerer, _, offer, _ := newEstablishedWebrtcPair(t, nil)

	reoffer, err := offerer.ReOffer("")
	require.NoError(t, err)
	for _, prefix := range []string{"a=ice-ufrag:", "a=ice-pwd:", "a=fingerprint:", "a=tls-id:", "m=audio ", "c="} {
		require.Equal(t, sdpLine(t, offer, prefix), sdpLine(t, reoffer, prefix))
	}
	candidates := sdpLines(offer, "a=candidate:")
	require.NotEmpty(t, candidates)
	require.Equal(t, candidates, sdpLines(reoffer, "a=candidate:"))
	require.Contains(t, string(reoffer), "a=rtcp-mux\r\n")
	require.Contains(t, string(reoffer), "a=setup:actpass\r\n")
	require.Contains(t, string(reoffer), "a=sendrecv\r\n")
}

// TestICEWebrtcAnswerKeepsEstablishedDTLSRole asserts that answering a
// subsequent actpass offer keeps the DTLS role the association was built with
// (RFC 8842 section 5.3). The pair is established with the answerer passive,
// which is the case a fresh derivation from actpass would get wrong.
func TestICEWebrtcAnswerKeepsEstablishedDTLSRole(t *testing.T) {
	ctx := t.Context()
	_, answerer, offer, answer := newEstablishedWebrtcPair(t, func(s string) string {
		return strings.Replace(s, "a=setup:actpass", "a=setup:active", 1)
	})
	require.Contains(t, string(answer), "a=setup:passive")

	subsequentOffer := strings.Replace(string(offer), "a=setup:active", "a=setup:actpass", 1)
	require.NoError(t, answerer.RemoteSDP(ctx, []byte(subsequentOffer), false))

	reanswer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.Contains(t, string(reanswer), "a=setup:passive")
	require.NotContains(t, string(reanswer), "a=setup:active")
}

// TestICEWebrtcNewAssociationNeedsRestart is a re-offer on an established ICE
// pair, with the credentials unchanged, that asks for a new DTLS association:
// it changes the fingerprint set, the roles or the tls-id (RFC 8842 section
// 3.1). A new association has to run over candidates the current one did not
// use (RFC 8842 section 6), which takes an ICE restart the offer does not
// make, so the offer is refused with ErrWebRTCICERestartRequired. Continuing
// the old association instead would leave the peer waiting for a handshake,
// with a certificate that no longer matches its fingerprint. The session goes
// on as it was.
func TestICEWebrtcNewAssociationNeedsRestart(t *testing.T) {
	ctx := t.Context()
	offerer, answerer, offer, _ := newEstablishedWebrtcPair(t, nil)
	before := webrtcTransportOf(answerer)

	ownLine := sha256FingerprintLine(testdata.ClientCertificate())
	otherLine := sha256FingerprintLine(testdata.ServerCertificate())
	cases := map[string]string{
		"PeerFingerprint":  withFingerprintLines(string(offer), otherLine),
		"AddedFingerprint": withFingerprintLines(string(offer), ownLine, otherLine),
		"Roles":            strings.Replace(string(offer), "a=setup:actpass", "a=setup:active", 1),
		"TLSID":            replaceSDPLine(t, string(offer), "a=tls-id:", "a=tls-id:"+strings.Repeat("n", 26)),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			require.NotEqual(t, string(offer), body)
			err := answerer.RemoteSDP(ctx, []byte(body), false)
			require.ErrorIs(t, err, ErrWebRTCICERestartRequired)
			require.NotErrorIs(t, err, ErrWebRTCICERestart, "the offer carries no new credentials to restart with")
			require.False(t, answerer.FinalizePending())
			finalizeWebrtcWithin(t, answerer, 5*time.Second)
			requireSameWebrtcTransport(t, before, answerer)
		})
	}

	// An answer is held to the same: the peer may not ask for a new
	// association in the answer to a re-offer that asked for none.
	t.Run("AnswerTLSID", func(t *testing.T) {
		reoffer, err := offerer.ReOffer("")
		require.NoError(t, err)
		require.NoError(t, answerer.RemoteSDP(ctx, reoffer, false))
		reanswer, err := answerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		answerer.Rollback()

		changed := replaceSDPLine(t, string(reanswer), "a=tls-id:", "a=tls-id:"+strings.Repeat("n", 26))
		require.ErrorIs(t, offerer.RemoteSDP(ctx, []byte(changed), true), ErrWebRTCICERestartRequired)
		offerer.Rollback()
	})

	requireWebrtcSRTPBetween(t, offerer, answerer, 21)
	requireWebrtcSRTPBetween(t, answerer, offerer, 22)
}

// TestICEWebrtcForkCompletesICERestart asserts that changed credentials, which
// is how a peer asks for an ICE restart, are reported with ErrWebRTCICERestart
// when both changed, and that a Fork then completes the restart: a new ICE
// generation takes the offer, and carries media both ways once its checks and
// its handshake are done. A change of one credential alone is no restart and
// is refused. No error spells a credential.
func TestICEWebrtcForkCompletesICERestart(t *testing.T) {
	ctx := t.Context()
	offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, nil)

	// The peer restarts ICE: it offers from a new generation of its own.
	peerFork, err := offerer.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, peerFork)
	restartOffer, err := peerFork.LocalSDP(ctx, false)
	require.NoError(t, err)

	oldUfragLine, oldPwdLine := sdpLine(t, offer, "a=ice-ufrag:"), sdpLine(t, offer, "a=ice-pwd:")
	newUfragLine, newPwdLine := sdpLine(t, restartOffer, "a=ice-ufrag:"), sdpLine(t, restartOffer, "a=ice-pwd:")
	require.NotEqual(t, oldUfragLine, newUfragLine)
	require.NotEqual(t, oldPwdLine, newPwdLine)
	secrets := []string{
		strings.TrimPrefix(oldUfragLine, "a=ice-ufrag:"), strings.TrimPrefix(oldPwdLine, "a=ice-pwd:"),
		strings.TrimPrefix(newUfragLine, "a=ice-ufrag:"), strings.TrimPrefix(newPwdLine, "a=ice-pwd:"),
	}
	requireNoCredential := func(t *testing.T, err error) {
		t.Helper()
		for _, secret := range secrets {
			require.NotContains(t, err.Error(), secret, "the error must not carry a credential")
		}
	}

	for name, body := range map[string]string{
		"PwdOnly":   strings.Replace(string(offer), oldPwdLine, newPwdLine, 1),
		"UfragOnly": strings.Replace(string(offer), oldUfragLine, newUfragLine, 1),
	} {
		t.Run(name, func(t *testing.T) {
			before := webrtcTransportOf(answerer)
			err := answerer.RemoteSDP(ctx, []byte(body), false)
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrWebRTCICERestart, "one changed credential is not a restart to complete")
			requireNoCredential(t, err)
			require.False(t, answerer.FinalizePending())
			requireSameWebrtcTransport(t, before, answerer)
		})
	}

	t.Run("UfragAndPwd", func(t *testing.T) {
		before := webrtcTransportOf(answerer)
		err := answerer.RemoteSDP(ctx, restartOffer, false)
		require.ErrorIs(t, err, ErrWebRTCICERestart)
		require.NotErrorIs(t, err, ErrWebRTCICERestartRequired)
		requireNoCredential(t, err)
		requireSameWebrtcTransport(t, before, answerer)

		fork, err := answerer.Fork(ctx)
		require.NoError(t, err)
		closeWebrtcWithTest(t, fork)
		require.NoError(t, fork.RemoteSDP(ctx, restartOffer, false))
		restartAnswer, err := fork.LocalSDP(ctx, true)
		require.NoError(t, err)
		require.NotEqual(t, sdpLine(t, answer, "a=ice-ufrag:"), sdpLine(t, restartAnswer, "a=ice-ufrag:"))
		require.NotEqual(t, sdpLine(t, answer, "a=ice-pwd:"), sdpLine(t, restartAnswer, "a=ice-pwd:"))
		require.NoError(t, peerFork.RemoteSDP(ctx, restartAnswer, true))
		requireBothFinalize(t, peerFork, fork, 40*time.Second, "the ICE restart")

		require.NotSame(t, before.iceConn, webrtcTransportOf(fork).iceConn, "the restart runs on a pair of its own")
		requireWebrtcSRTPBetween(t, peerFork, fork, 31)
		requireWebrtcSRTPBetween(t, fork, peerFork, 32)

		// The generation it replaces carried the call meanwhile, and does
		// until its owner closes it.
		requireWebrtcSRTPBetween(t, offerer, answerer, 33)
		requireWebrtcSRTPBetween(t, answerer, offerer, 34)
	})
}

// TestICEWebrtcReOfferNeedsNegotiatedSession asserts a session that has no
// negotiated pair, or is closed, has nothing to describe again: ReOffer
// returns an error and changes nothing, where a first offer is LocalSDP's to
// make.
func TestICEWebrtcReOfferNeedsNegotiatedSession(t *testing.T) {
	ctx := t.Context()

	t.Run("NoDescription", func(t *testing.T) {
		m := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		_, err := m.ReOffer(sdp.ModeSendonly)
		require.ErrorIs(t, err, errWebRTCSessionNotNegotiated)
		mode, preference := webrtcDirection(m)
		require.Equal(t, sdp.ModeSendrecv, mode)
		require.Empty(t, preference)
	})

	t.Run("NegotiationNotRun", func(t *testing.T) {
		offerer, answerer := newNegotiatedWebrtcPair(t, "")
		for _, m := range []*MediaSessionWebrtc{offerer, answerer} {
			_, err := m.ReOffer(sdp.ModeSendonly)
			require.ErrorIs(t, err, errWebRTCSessionNotNegotiated)
			mode, preference := webrtcDirection(m)
			require.Equal(t, sdp.ModeSendrecv, mode)
			require.Equal(t, sdp.ModeSendrecv, preference)
		}
	})

	t.Run("Closed", func(t *testing.T) {
		offerer, _, _, _ := newEstablishedWebrtcPair(t, nil)
		require.NoError(t, offerer.Close())
		_, err := offerer.ReOffer("")
		require.ErrorIs(t, err, errWebRTCSessionClosed)
	})
}

// TestICEWebrtcReOfferDirection pins what a re-offer of ours does to the
// direction of a session. The direction it asks for is staged: media keeps the
// direction it has until the answer is committed, and a re-offer that fails
// leaves both the direction and the one we ask for as they were.
func TestICEWebrtcReOfferDirection(t *testing.T) {
	ctx := t.Context()

	requireDirection := func(t *testing.T, m *MediaSessionWebrtc, wantMode, wantPreference string) {
		t.Helper()
		mode, preference := webrtcDirection(m)
		require.Equal(t, wantMode, mode, "the direction")
		require.Equal(t, wantPreference, preference, "the direction asked for")
	}

	t.Run("HoldRolledBack", func(t *testing.T) {
		offerer, answerer, _, _ := newEstablishedWebrtcPair(t, nil)
		before := webrtcTransportOf(offerer)

		// The re-INVITE is refused, or never answered.
		hold, err := offerer.ReOffer(sdp.ModeSendonly)
		require.NoError(t, err)
		require.Contains(t, string(hold), "a=sendonly\r\n")
		mode, _ := webrtcDirection(offerer)
		require.Equal(t, sdp.ModeSendrecv, mode, "an offer changes no direction")
		offerer.Rollback()
		requireDirection(t, offerer, sdp.ModeSendrecv, sdp.ModeSendrecv)

		// The re-INVITE fails after its answer was read.
		hold, err = offerer.ReOffer(sdp.ModeSendonly)
		require.NoError(t, err)
		require.NoError(t, answerer.RemoteSDP(ctx, hold, false))
		held, err := answerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		require.Contains(t, string(held), "a=recvonly\r\n")
		answerer.Rollback()
		require.NoError(t, offerer.RemoteSDP(ctx, held, true))
		offerer.Rollback()
		requireDirection(t, offerer, sdp.ModeSendrecv, sdp.ModeSendrecv)
		requireDirection(t, answerer, sdp.ModeSendrecv, sdp.ModeSendrecv)

		// What the peer offers next is answered as before the hold.
		peerOffer, err := answerer.ReOffer("")
		require.NoError(t, err)
		reanswer := exchangeWebrtcReOffer(t, answerer, offerer, peerOffer)
		require.Contains(t, string(reanswer), "a=sendrecv\r\n")
		requireDirection(t, offerer, sdp.ModeSendrecv, sdp.ModeSendrecv)

		requireSameWebrtcTransport(t, before, offerer)
		requireWebrtcSRTPBetween(t, offerer, answerer, 41)
		requireWebrtcSRTPBetween(t, answerer, offerer, 42)
	})

	t.Run("HoldAndUnhold", func(t *testing.T) {
		offerer, answerer, _, _ := newEstablishedWebrtcPair(t, nil)
		before := webrtcTransportOf(offerer)

		hold, err := offerer.ReOffer(sdp.ModeSendonly)
		require.NoError(t, err)
		held := exchangeWebrtcReOffer(t, offerer, answerer, hold)
		require.Contains(t, string(held), "a=recvonly\r\n")
		requireDirection(t, offerer, sdp.ModeSendonly, sdp.ModeSendonly)
		requireDirection(t, answerer, sdp.ModeRecvonly, sdp.ModeSendrecv)
		requireWebrtcSRTPBetween(t, offerer, answerer, 51)

		// The hold is committed: an exchange that fails later puts nothing
		// of the time before it back.
		peerOffer, err := answerer.ReOffer("")
		require.NoError(t, err)
		require.Contains(t, string(peerOffer), "a=sendrecv\r\n")
		require.NoError(t, offerer.RemoteSDP(ctx, peerOffer, false))
		stillHeld, err := offerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		require.Contains(t, string(stillHeld), "a=sendonly\r\n")
		offerer.Rollback()
		answerer.Rollback()
		requireDirection(t, offerer, sdp.ModeSendonly, sdp.ModeSendonly)
		requireDirection(t, answerer, sdp.ModeRecvonly, sdp.ModeSendrecv)

		unhold, err := offerer.ReOffer(sdp.ModeSendrecv)
		require.NoError(t, err)
		require.Contains(t, string(unhold), "a=sendrecv\r\n")
		requireDirection(t, offerer, sdp.ModeSendonly, sdp.ModeSendrecv)
		resumed := exchangeWebrtcReOffer(t, offerer, answerer, unhold)
		require.Contains(t, string(resumed), "a=sendrecv\r\n")
		requireDirection(t, offerer, sdp.ModeSendrecv, sdp.ModeSendrecv)
		requireDirection(t, answerer, sdp.ModeSendrecv, sdp.ModeSendrecv)

		requireSameWebrtcTransport(t, before, offerer)
		requireWebrtcSRTPBetween(t, offerer, answerer, 52)
		requireWebrtcSRTPBetween(t, answerer, offerer, 53)
	})

	// A re-offer that names no direction, a session refresh, asks for the
	// direction we last asked for, not for the one the peer has narrowed the
	// call to: otherwise a refresh sent while the peer holds the call would
	// make the hold ours, and the peer could not take the call off it.
	t.Run("RefreshWhileHeldByThePeer", func(t *testing.T) {
		offerer, answerer, _, _ := newEstablishedWebrtcPair(t, nil)

		peerHold, err := answerer.ReOffer(sdp.ModeSendonly)
		require.NoError(t, err)
		exchangeWebrtcReOffer(t, answerer, offerer, peerHold)
		requireDirection(t, offerer, sdp.ModeRecvonly, sdp.ModeSendrecv)

		refresh, err := offerer.ReOffer("")
		require.NoError(t, err)
		require.Contains(t, string(refresh), "a=sendrecv\r\n")
		refreshed := exchangeWebrtcReOffer(t, offerer, answerer, refresh)
		require.Contains(t, string(refreshed), "a=sendonly\r\n")
		requireDirection(t, offerer, sdp.ModeRecvonly, sdp.ModeSendrecv)
		requireDirection(t, answerer, sdp.ModeSendonly, sdp.ModeSendonly)
		requireWebrtcSRTPBetween(t, answerer, offerer, 61)

		peerUnhold, err := answerer.ReOffer(sdp.ModeSendrecv)
		require.NoError(t, err)
		resumed := exchangeWebrtcReOffer(t, answerer, offerer, peerUnhold)
		require.Contains(t, string(resumed), "a=sendrecv\r\n")
		requireDirection(t, offerer, sdp.ModeSendrecv, sdp.ModeSendrecv)
		requireWebrtcSRTPBetween(t, offerer, answerer, 62)
		requireWebrtcSRTPBetween(t, answerer, offerer, 63)
	})
}

// TestWebrtcSDPOriginIsStable pins the o= line of the SDPs one session sends
// in a call: RFC 3264 section 8 has it identical from one SDP to the next but
// for the version, which increments by one. An ICE restart modifies the same
// session, so the generation Fork creates goes on from the origin of the one
// it replaces.
func TestWebrtcSDPOriginIsStable(t *testing.T) {
	ctx := t.Context()
	offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, nil)

	id, version, rest := sdpOriginOf(t, offer)
	requireNext := func(t *testing.T, body []byte) {
		t.Helper()
		gotID, gotVersion, gotRest := sdpOriginOf(t, body)
		require.Equal(t, id, gotID, "the session id")
		require.Equal(t, version+1, gotVersion, "the session version")
		require.Equal(t, rest, gotRest)
		version = gotVersion
	}

	reoffer, err := offerer.ReOffer("")
	require.NoError(t, err)
	requireNext(t, reoffer)
	offerer.Rollback()

	hold, err := offerer.ReOffer(sdp.ModeSendonly)
	require.NoError(t, err)
	requireNext(t, hold)

	// The answerer numbers its own SDPs, the answers included.
	answerID, answerVersion, _ := sdpOriginOf(t, answer)
	require.NotEqual(t, id, answerID, "an answer is the SDP of another endpoint")
	held := exchangeWebrtcReOffer(t, offerer, answerer, hold)
	heldID, heldVersion, _ := sdpOriginOf(t, held)
	require.Equal(t, answerID, heldID)
	require.Equal(t, answerVersion+1, heldVersion)

	fork, err := offerer.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, fork)
	restart, err := fork.LocalSDP(ctx, false)
	require.NoError(t, err)
	gotID, gotVersion, _ := sdpOriginOf(t, restart)
	require.Equal(t, id, gotID, "an ICE restart keeps the session id")
	require.Equal(t, version+1, gotVersion)
	version = gotVersion

	// The generation in place and the one forked from it share the count, so
	// no version is used twice whichever of them offers next.
	unhold, err := offerer.ReOffer(sdp.ModeSendrecv)
	require.NoError(t, err)
	requireNext(t, unhold)
}
