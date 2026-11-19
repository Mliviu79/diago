// SPDX-License-Identifier: MPL-2.0

package media

import (
	"crypto/tls"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newOwnOfferWebrtcSession returns a WebRTC session that gathered its
// candidates on the loopback interface, presents certificate and writes its
// own first offer in the form withoutBundle chooses. It is closed when the
// test ends.
func newOwnOfferWebrtcSession(t *testing.T, certificate tls.Certificate, withoutBundle bool) *MediaSessionWebrtc {
	t.Helper()
	m := &MediaSessionWebrtc{
		Codecs: []Codec{CodecAudioUlaw},
		Mode:   sdp.ModeSendrecv,
	}
	require.NoError(t, m.Init(t.Context(), MediaSessionWebrtcConfig{
		DTLS:               DTLSConfig{Certificates: []tls.Certificate{certificate}},
		IPFamilies:         []ICEIPFamily{ICEIPFamilyIPv4},
		IncludeLoopback:    true,
		InterfaceFilter:    testLoopbackInterface,
		OfferWithoutBundle: withoutBundle,
	}))
	closeWebrtcWithTest(t, m)
	return m
}

// requireStreamIdentification requires body to identify its streams with the
// a=group lines group and, for each m= line in order, the tag of tags, "" for
// a stream without an a=mid line. It also requires that no a=mid line stands
// anywhere else in the body.
func requireStreamIdentification(t *testing.T, group, tags []string, body []byte, what string) {
	t.Helper()
	require.Equal(t, group, sdpLines(body, "a=group:"), "the a=group lines of %s\n%s", what, body)
	require.Equal(t, tags, streamTagsOf(body), "the tags of %s\n%s", what, body)
	identified := 0
	for _, tag := range tags {
		if tag != "" {
			identified++
		}
	}
	require.Len(t, sdpLines(body, "a=mid:"), identified, "the a=mid lines of %s\n%s", what, body)
}

// TestMediaSessionWebrtcOwnFirstOfferForm pins the two forms of the first
// offer a session makes before any offer of its peer, and that the SDPs which
// follow it keep the form.
//
// The zero value of MediaSessionWebrtcConfig.OfferWithoutBundle offers the
// audio stream under the tag 0 in a BUNDLE group of its own. Set, the offer
// carries neither an a=group line nor an a=mid line. Both are an offerer's to
// write: the group is in the offer of an offerer that "wants to negotiate a
// given BUNDLE group" (RFC 8843 section 7.2).
//
// The peer answers each in kind, whatever its own configuration: the group
// only where the offer had it (RFC 8843 section 7.3) and the a=mid line "If
// and only if present in the offer" (RFC 8829 section 5.3.1). Every later SDP
// of either side, a re-offer on the transport in use, its answer and the offer
// of an ICE restart, is identified as that first exchange was.
func TestMediaSessionWebrtcOwnFirstOfferForm(t *testing.T) {
	for _, tc := range []struct {
		name string
		// withoutBundle is the OfferWithoutBundle of the offerer, and
		// peerWithoutBundle the one of the session that answers it.
		withoutBundle, peerWithoutBundle bool
		wantGroup, wantTags              []string
	}{
		{
			name:      "a_BUNDLE_group_of_the_audio_stream_by_default",
			wantGroup: []string{"a=group:BUNDLE 0"},
			wantTags:  []string{"0"},
		},
		{
			name:          "no_group_and_no_tag_when_chosen",
			withoutBundle: true,
			wantTags:      []string{""},
		},
		{
			// The field chooses the form of an offer of ours alone: a
			// session that has it set answers a bundled offer in kind.
			name:              "a_bundled_offer_answered_by_a_session_that_offers_without",
			peerWithoutBundle: true,
			wantGroup:         []string{"a=group:BUNDLE 0"},
			wantTags:          []string{"0"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newOwnOfferWebrtcSession(t, testdata.ClientCertificate(), tc.withoutBundle)
			answerer := newOwnOfferWebrtcSession(t, testdata.ServerCertificate(), tc.peerWithoutBundle)

			offer, answer := negotiateWebrtc(t, offerer, answerer, nil)
			requireStreamIdentification(t, tc.wantGroup, tc.wantTags, offer, "our first offer")
			requireStreamIdentification(t, tc.wantGroup, tc.wantTags, answer, "the answer to it")
			requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE and DTLS negotiation")
			requireWebrtcSRTPBetween(t, offerer, answerer, 1)
			requireWebrtcSRTPBetween(t, answerer, offerer, 2)

			// The offerer has taken no offer of the peer: its later offers
			// are written in the form of its first.
			reoffer, err := offerer.ReOffer("")
			require.NoError(t, err)
			requireStreamIdentification(t, tc.wantGroup, tc.wantTags, reoffer, "our re-offer")
			reanswer := exchangeWebrtcReOffer(t, offerer, answerer, reoffer)
			requireStreamIdentification(t, tc.wantGroup, tc.wantTags, reanswer, "the answer to our re-offer")

			fork, err := offerer.Fork(ctx)
			require.NoError(t, err)
			closeWebrtcWithTest(t, fork)
			forkOffer, err := fork.LocalSDP(ctx, false)
			require.NoError(t, err)
			requireStreamIdentification(t, tc.wantGroup, tc.wantTags, forkOffer, "the offer of a fork")

			// The answerer follows the offer it committed to, in its own
			// offers too, whatever form its configuration gives a first
			// offer.
			peerOffer, err := answerer.ReOffer("")
			require.NoError(t, err)
			requireStreamIdentification(t, tc.wantGroup, tc.wantTags, peerOffer, "the re-offer of the answerer")
			peerAnswer := exchangeWebrtcReOffer(t, answerer, offerer, peerOffer)
			requireStreamIdentification(t, tc.wantGroup, tc.wantTags, peerAnswer, "the answer to the re-offer of the answerer")

			peerFork, err := answerer.Fork(ctx)
			require.NoError(t, err)
			closeWebrtcWithTest(t, peerFork)
			peerForkOffer, err := peerFork.LocalSDP(ctx, false)
			require.NoError(t, err)
			requireStreamIdentification(t, tc.wantGroup, tc.wantTags, peerForkOffer, "the offer of a fork of the answerer")

			requireWebrtcSRTPBetween(t, offerer, answerer, 3)
			requireWebrtcSRTPBetween(t, answerer, offerer, 4)
		})
	}
}

// TestMediaSessionWebrtcFollowsAnOfferWithoutTags pins what becomes of a
// session's own form once its peer offers: the answer to an offer with no
// a=mid and no a=group line carries neither (RFC 8829 section 5.3.1, RFC 8843
// section 7.3), whatever form the session's own offers had until then, and so
// do the SDPs of ours that follow once the exchange is committed. An exchange
// that fails leaves the session writing the form it had.
func TestMediaSessionWebrtcFollowsAnOfferWithoutTags(t *testing.T) {
	ctx := t.Context()
	ownGroup, ownTags := []string{"a=group:BUNDLE 0"}, []string{"0"}
	noTags := []string{""}

	offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, nil)
	requireStreamIdentification(t, ownGroup, ownTags, offer, "our first offer")
	requireStreamIdentification(t, ownGroup, ownTags, answer, "the answer to it")

	// The peer re-offers on the transport in use without identifying its
	// stream.
	peerOffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	answerer.Rollback()
	untagged := webrtcTaggedOffer(t, peerOffer, "", "", nil, nil)
	requireStreamIdentification(t, nil, noTags, untagged, "the offer of the peer")

	require.NoError(t, offerer.RemoteSDP(ctx, untagged, false))
	staged, err := offerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	requireStreamIdentification(t, nil, noTags, staged, "the answer to the offer without tags")
	offerer.Rollback()
	ours, err := offerer.ReOffer("")
	require.NoError(t, err)
	offerer.Rollback()
	requireStreamIdentification(t, ownGroup, ownTags, ours, "our offer after the exchange failed")

	require.NoError(t, offerer.RemoteSDP(ctx, untagged, false))
	committed, err := offerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	requireStreamIdentification(t, nil, noTags, committed, "the answer to the offer without tags")
	finalizeWebrtcWithin(t, offerer, 5*time.Second)

	ours, err = offerer.ReOffer("")
	require.NoError(t, err)
	offerer.Rollback()
	requireStreamIdentification(t, nil, noTags, ours, "our offer after the exchange")

	fork, err := offerer.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, fork)
	forkOffer, err := fork.LocalSDP(ctx, false)
	require.NoError(t, err)
	requireStreamIdentification(t, nil, noTags, forkOffer, "the offer of a fork after the exchange")

	requireWebrtcSRTPBetween(t, offerer, answerer, 1)
	requireWebrtcSRTPBetween(t, answerer, offerer, 2)
}

// TestOwnOfferStreams pins the stream list a session writes its SDPs around
// before any offer of its peer, in both forms: it is the list of no offer and
// it rejects nothing. The bundled form maps the header extension of the tag
// under the identifier 1 (RFC 8843 section 9.1), and the form without a
// group maps none.
func TestOwnOfferStreams(t *testing.T) {
	bundled := ownOfferStreams(false)
	assert.Equal(t, []string{"a=group:BUNDLE 0"}, bundled.bundleGroupLines())
	assert.Equal(t, []string{"a=mid:0", "a=extmap:1 " + testMIDExtmap}, bundled.bundledAudioLines())
	assert.Equal(t, midExtension{id: 1, tag: []byte("0")}, bundled.bundle.mid)

	plain := ownOfferStreams(true)
	assert.Empty(t, plain.bundleGroupLines())
	assert.Empty(t, plain.bundledAudioLines())
	assert.Zero(t, plain.bundle.mid.id)

	for _, streams := range []offeredStreams{bundled, plain} {
		assert.False(t, streams.ofAnOffer())
		before, after := streams.rejected()
		assert.Empty(t, before)
		assert.Empty(t, after)
	}
}
