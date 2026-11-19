// SPDX-License-Identifier: MPL-2.0

package media

import (
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/require"
)

// TestDTLSWebrtcTLSID pins the a=tls-id procedures of RFC 8842 section 5 on
// both sides of a WebRTC exchange. Each DTLS association has a value of ours,
// written in every offer (sections 5.2 and 5.5) and in an answer to an offer
// that carries one (section 5.3), and never in an answer to an offer that
// does not. A session continuing its association writes the value it was
// negotiated with. A new association comes with a new ICE generation, and a
// new value (section 4), whether our offer starts the generation or the
// peer's does.
func TestDTLSWebrtcTLSID(t *testing.T) {
	ctx := t.Context()

	t.Run("InitialOfferAndAnswer", func(t *testing.T) {
		offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
		first, err := offerer.LocalSDP(ctx, false)
		require.NoError(t, err)
		offer, answer := negotiateWebrtc(t, offerer, answerer, nil)

		offerID, answerID := sdpTLSID(offer), sdpTLSID(answer)
		require.Regexp(t, tlsIDSyntax, offerID, "the initial offer must carry a tls-id (section 5.2)")
		require.Regexp(t, tlsIDSyntax, answerID, "the answer to an offer with a tls-id must carry one (section 5.3)")
		require.NotEqual(t, offerID, answerID)
		require.Equal(t, offerID, sdpTLSID(first), "an offer made again is the same offer")

		other := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		otherOffer, err := other.LocalSDP(ctx, false)
		require.NoError(t, err)
		require.NotEqual(t, offerID, sdpTLSID(otherOffer), "every session draws its own value")
	})

	t.Run("AnswerToAnOfferWithoutOne", func(t *testing.T) {
		_, answerer, _, answer := newEstablishedWebrtcPair(t, func(s string) string { return removeSDPLine(s, "a=tls-id:") })
		require.Empty(t, sdpTLSID(answer), "an answer to an offer without a tls-id must not carry one (section 5.3)")

		// A subsequent offer from that answerer continues an association that
		// has no tls-id of ours, so it still carries none.
		reoffer, err := answerer.ReOffer("")
		require.NoError(t, err)
		require.Empty(t, sdpTLSID(reoffer))
	})

	t.Run("AssociationContinues", func(t *testing.T) {
		offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, nil)

		reoffer, err := offerer.ReOffer("")
		require.NoError(t, err)
		require.Equal(t, sdpTLSID(offer), sdpTLSID(reoffer), "a subsequent offer continuing the association keeps its value (section 5.5)")

		require.NoError(t, answerer.RemoteSDP(ctx, reoffer, false))
		require.False(t, answerer.FinalizePending())
		reanswer, err := answerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		require.Equal(t, sdpTLSID(answer), sdpTLSID(reanswer), "an answer continuing the association keeps its value (section 5.3)")

		// A peer that leaves the attribute out of a later offer changes
		// nothing by it, and is answered without one.
		require.NoError(t, answerer.RemoteSDP(ctx, []byte(removeSDPLine(string(reoffer), "a=tls-id:")), false))
		reanswer, err = answerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		require.Empty(t, sdpTLSID(reanswer))
	})

	t.Run("NewAssociationWithANewICEGeneration", func(t *testing.T) {
		offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, nil)

		// The offerer starts a new generation, and with it a new association.
		offerFork, err := offerer.Fork(ctx)
		require.NoError(t, err)
		closeWebrtcWithTest(t, offerFork)
		restartOffer, err := offerFork.LocalSDP(ctx, false)
		require.NoError(t, err)
		newOfferID := sdpTLSID(restartOffer)
		require.Regexp(t, tlsIDSyntax, newOfferID)
		require.NotEqual(t, sdpTLSID(offer), newOfferID, "an offer asking for a new association must carry a new value (section 5.5)")

		// The answerer follows with a generation of its own.
		require.ErrorIs(t, answerer.RemoteSDP(ctx, restartOffer, false), ErrWebRTCICERestart)
		answerFork, err := answerer.Fork(ctx)
		require.NoError(t, err)
		closeWebrtcWithTest(t, answerFork)
		require.NoError(t, answerFork.RemoteSDP(ctx, restartOffer, false))
		restartAnswer, err := answerFork.LocalSDP(ctx, true)
		require.NoError(t, err)
		newAnswerID := sdpTLSID(restartAnswer)
		require.Regexp(t, tlsIDSyntax, newAnswerID)
		require.NotEqual(t, sdpTLSID(answer), newAnswerID, "the answer to a new association must carry a new value (section 5.3)")
		require.NoError(t, offerFork.RemoteSDP(ctx, restartAnswer, true))
		requireBothFinalize(t, offerFork, answerFork, 40*time.Second, "the new ICE generation")

		// The new association has the new values: both sides continue it
		// with them.
		reoffer, err := offerFork.ReOffer("")
		require.NoError(t, err)
		require.Equal(t, newOfferID, sdpTLSID(reoffer))
		reanswer := exchangeWebrtcReOffer(t, offerFork, answerFork, reoffer)
		require.Equal(t, newAnswerID, sdpTLSID(reanswer))
	})
}

// TestDTLSWebrtcSDPHasNoConnectionAttribute pins RFC 5763 section 5 on the
// WebRTC session: "The endpoint MUST NOT use the connection attribute defined
// in [RFC4145]." DTLS runs over UDP, where a=connection has no meaning, and
// RFC 8842 section 5.1 leaves it to the TCP and SCTP usages. Whether an
// association continues is told by the setup role, the fingerprints and the
// tls-id instead. It is checked on an offer, an answer and a subsequent offer,
// each of which still carries a=setup and a=fingerprint.
func TestDTLSWebrtcSDPHasNoConnectionAttribute(t *testing.T) {
	offerer, _, offer, answer := newEstablishedWebrtcPair(t, nil)
	reoffer, err := offerer.ReOffer("")
	require.NoError(t, err)

	bodies := map[string][]byte{
		"Offer":           offer,
		"Answer":          answer,
		"SubsequentOffer": reoffer,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			require.NotContains(t, string(body), "a=connection:")
			require.Contains(t, string(body), "a=setup:")
			require.Contains(t, string(body), "a=fingerprint:")
		})
	}
}
