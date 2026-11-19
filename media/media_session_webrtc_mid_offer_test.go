// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pionAnswer applies offer to peer and returns the answer of peer with its
// candidates gathered.
func pionAnswer(ctx context.Context, t *testing.T, peer *webrtc.PeerConnection, offer []byte) []byte {
	t.Helper()
	require.NoError(t, peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: string(offer)}),
		"the offer\n%s", offer)
	answer, err := peer.CreateAnswer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(peer)
	require.NoError(t, peer.SetLocalDescription(answer))
	select {
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	case <-gathered:
	}
	return []byte(peer.LocalDescription().SDP)
}

// TestMediaSessionWebrtcOffersTheMIDExtensionToAPionPeer offers a call to a
// Pion PeerConnection, as a browser stack answering a session. The offer has
// its audio stream in a BUNDLE group, so it maps the MID header extension (RFC
// 8843 section 9.1). A peer that knows the extension keeps the mapping in its
// answer and is sent the tag in every packet; one that does not leaves the
// line out (RFC 8285 section 7) and is sent the packets as they are. Either
// way Pion places the audio.
func TestMediaSessionWebrtcOffersTheMIDExtensionToAPionPeer(t *testing.T) {
	for _, tc := range []struct {
		name string
		// extensions are the header extensions the peer knows.
		extensions []string
		// wantKept is whether the answer keeps our mapping.
		wantKept bool
	}{
		{name: "a_peer_that_knows_the_extension", extensions: []string{testMIDExtmap}, wantKept: true},
		{name: "a_peer_that_does_not"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			peer := newPionPeer(t, tc.extensions...)
			localTrack, err := webrtc.NewTrackLocalStaticRTP(
				webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "browser")
			require.NoError(t, err)
			_, err = peer.AddTrack(localTrack)
			require.NoError(t, err)
			remote := make(chan *webrtc.TrackRemote, 1)
			peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { remote <- track })

			session := newPionAnswerer(ctx, t)
			offer, err := session.LocalSDP(ctx, false)
			require.NoError(t, err)
			require.Equal(t, []string{"a=group:BUNDLE 0"}, sdpLines(offer, "a=group:"), "the offer\n%s", offer)
			require.Equal(t, []string{ownMIDExtmap}, audioSectionLines(t, offer, "a=extmap:"), "the offer\n%s", offer)

			answer := pionAnswer(ctx, t, peer, offer)
			var wantExtmap []string
			if tc.wantKept {
				wantExtmap = []string{ownMIDExtmap}
			}
			require.Equal(t, wantExtmap, sdpLines(answer, "a=extmap:"), "the answer of the peer\n%s", answer)
			require.NoError(t, session.RemoteSDP(ctx, answer, true), "the answer\n%s", answer)

			rtpSession := NewRTPSessionWebrtc(session)
			packetWriter := NewRTPPacketWriter(rtpSession, session.Codec())
			require.NoError(t, session.Finalize(ctx))

			sent := []byte("audio-to-the-peer")
			_, err = packetWriter.WriteSamples(sent, CodecAudioUlaw.SampleTimestamp(), true, CodecAudioUlaw.PayloadType)
			require.NoError(t, err)
			select {
			case <-ctx.Done():
				t.Fatalf("Pion raised no track for our audio: %v\nthe offer\n%s\nthe answer\n%s", context.Cause(ctx), offer, answer)
			case track := <-remote:
				pkt, _, readErr := track.ReadRTP()
				require.NoError(t, readErr)
				assert.Equal(t, sent, pkt.Payload)
				if tc.wantKept {
					assert.Equal(t, []byte("0"), pkt.GetExtension(1), "the tag of the packet Pion received")
				} else {
					assert.False(t, pkt.Header.Extension, "the packet Pion received has no header extension")
				}
			}
		})
	}
}

// ownMIDExtmap is the a=extmap line of the first offer of a session.
const ownMIDExtmap = "a=extmap:1 " + testMIDExtmap

// readWebrtcPacket sends a packet numbered seq from one negotiated session to
// the other and returns it as it was received.
func readWebrtcPacket(t *testing.T, from, to *MediaSessionWebrtc, seq uint16) rtp.Packet {
	t.Helper()
	pkt := dtlsTestPacket(seq)
	require.NoError(t, from.WriteRTP(pkt))
	require.False(t, pkt.Header.Extension, "the caller's packet is left as it is")
	require.NoError(t, to.StopRTP(1, 5*time.Second))
	got := rtp.Packet{}
	_, err := to.ReadRTP(make([]byte, RTPBufSize), &got)
	require.NoError(t, err, "the media did not arrive and decrypt")
	require.NoError(t, to.StartRTP(1))
	require.Equal(t, seq, got.SequenceNumber)
	return got
}

// TestMediaSessionWebrtcOwnOfferMapsTheMIDExtension pins the header extension
// of the offers a session makes before any offer of its peer. RFC 8843 section
// 9.1: "The RTP MID header extension MUST be enabled, by including an SDP
// 'extmap' attribute [RFC8285], with a 'urn:ietf:params:rtp-hdrext:sdes:mid'
// URI value, in each bundled RTP-based "m=" section in every offer and
// answer." So the first offer in its default form, which has a BUNDLE group,
// maps the extension in its audio section, and so do the re-offer and the
// offer of an ICE restart that follow it. The peer answers with the mapping
// and both sides send the tag in their packets and in the source description
// of their reports (sections 9.2 and 15). An offer in the form without a
// group maps nothing and the packets are sent as they are.
func TestMediaSessionWebrtcOwnOfferMapsTheMIDExtension(t *testing.T) {
	for _, tc := range []struct {
		name          string
		withoutBundle bool
		wantExtmap    []string
	}{
		{name: "in_a_BUNDLE_group_by_default", wantExtmap: []string{ownMIDExtmap}},
		{name: "not_without_a_group", withoutBundle: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newOwnOfferWebrtcSession(t, testdata.ClientCertificate(), tc.withoutBundle)
			answerer := newOwnOfferWebrtcSession(t, testdata.ServerCertificate(), false)

			offer, answer := negotiateWebrtc(t, offerer, answerer, nil)
			require.Equal(t, tc.wantExtmap, sdpLines(offer, "a=extmap:"), "our first offer\n%s", offer)
			require.Equal(t, tc.wantExtmap, audioSectionLines(t, offer, "a=extmap:"), "the audio section of our first offer\n%s", offer)
			require.Equal(t, tc.wantExtmap, sdpLines(answer, "a=extmap:"), "the answer to it\n%s", answer)
			requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE and DTLS negotiation")

			requireTag := func(t *testing.T, from, to *MediaSessionWebrtc, seq uint16, what string) {
				t.Helper()
				got := readWebrtcPacket(t, from, to, seq)
				if tc.wantExtmap == nil {
					assert.False(t, got.Header.Extension, "%s has no header extension", what)
					return
				}
				assert.Equal(t, []byte("0"), got.GetExtension(1), "the tag of %s", what)
			}
			requireTag(t, offerer, answerer, 1, "a packet of the offerer")
			requireTag(t, answerer, offerer, 2, "a packet of the answerer")
			sd := rtcp.NewCNAMESourceDescription(0x5eed0000, "cname")
			for _, m := range []*MediaSessionWebrtc{offerer, answerer} {
				items := m.identifiedSource(sd).Chunks[0].Items
				if tc.wantExtmap == nil {
					assert.Len(t, items, 1, "the source description sent")
					continue
				}
				assert.Equal(t, rtcp.SourceDescriptionItem{Type: sdesMID, Text: "0"}, items[len(items)-1], "the source description sent")
			}

			reoffer, err := offerer.ReOffer("")
			require.NoError(t, err)
			assert.Equal(t, tc.wantExtmap, sdpLines(reoffer, "a=extmap:"), "our re-offer\n%s", reoffer)
			reanswer := exchangeWebrtcReOffer(t, offerer, answerer, reoffer)
			assert.Equal(t, tc.wantExtmap, sdpLines(reanswer, "a=extmap:"), "the answer to our re-offer\n%s", reanswer)
			requireTag(t, offerer, answerer, 3, "a packet of the offerer after its re-offer")

			fork, err := offerer.Fork(ctx)
			require.NoError(t, err)
			closeWebrtcWithTest(t, fork)
			forkOffer, err := fork.LocalSDP(ctx, false)
			require.NoError(t, err)
			assert.Equal(t, tc.wantExtmap, sdpLines(forkOffer, "a=extmap:"), "the offer of a fork\n%s", forkOffer)
		})
	}
}

// TestMediaSessionWebrtcAnswerDecidesTheOfferedMIDExtension pins what the
// answer to an offer of ours makes of the header extension that offer mapped.
// RFC 8285 section 7: "An answerer that has no desire to receive the extension
// or does not understand the extension SHOULD remove it from the SDP answer",
// "the same identifier MUST be used for the same offered extension in the
// answer", and an answerer that wants to receive it and not to send it marks
// it "recvonly". So the session sends the tag when the answer holds its
// mapping under its identifier in a direction in which the peer receives, and
// sends its packets as they are otherwise.
func TestMediaSessionWebrtcAnswerDecidesTheOfferedMIDExtension(t *testing.T) {
	for _, tc := range []struct {
		name string
		// extmap is the a=extmap lines of the answer the offerer is given.
		extmap []string
		want   bool
	}{
		{name: "kept", extmap: []string{ownMIDExtmap}, want: true},
		{name: "kept_both_ways_in_words", extmap: []string{"a=extmap:1/sendrecv " + testMIDExtmap}, want: true},
		{name: "kept_for_the_peer_to_receive", extmap: []string{"a=extmap:1/recvonly " + testMIDExtmap}, want: true},
		{name: "kept_for_the_peer_to_send_alone", extmap: []string{"a=extmap:1/sendonly " + testMIDExtmap}},
		{name: "kept_inactive", extmap: []string{"a=extmap:1/inactive " + testMIDExtmap}},
		{name: "left_out"},
		{name: "under_another_identifier", extmap: []string{"a=extmap:2 " + testMIDExtmap}},
		{name: "another_extension_under_our_identifier", extmap: []string{"a=extmap:1 urn:ietf:params:rtp-hdrext:ssrc-audio-level"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			offer, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			require.Equal(t, []string{ownMIDExtmap}, sdpLines(answer, "a=extmap:"), "the answer\n%s", answer)
			answer = webrtcOfferWithExtmap(t, answer, nil, tc.extmap)
			require.NoError(t, offerer.RemoteSDP(ctx, answer, true), "the answer\n%s", answer)
			requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE and DTLS negotiation")

			got := readWebrtcPacket(t, offerer, answerer, 1)
			sd := rtcp.NewCNAMESourceDescription(0x5eed0000, "cname")
			if !tc.want {
				assert.False(t, got.Header.Extension, "the packet has no header extension")
				assert.Same(t, sd, offerer.identifiedSource(sd), "the source description is sent as it is")
			} else {
				assert.Equal(t, []byte("0"), got.GetExtension(1), "the tag of the packet")
				assert.Len(t, offerer.identifiedSource(sd).Chunks[0].Items, 2, "the source description carries the tag")
			}

			// Every offer of a bundled stream maps the extension, whatever
			// the answer before made of it.
			reoffer, err := offerer.ReOffer("")
			require.NoError(t, err)
			offerer.Rollback()
			assert.Equal(t, []string{ownMIDExtmap}, sdpLines(reoffer, "a=extmap:"), "our re-offer\n%s", reoffer)
		})
	}
}

// TestMediaSessionWebrtcReOfferAnswerDecidesTheMIDExtension follows the header
// extension through the re-offers of a negotiated session. What an answer
// makes of the mapping is the session's once Finalize commits the exchange,
// and an exchange that is rolled back leaves the packets as they were: tagged
// when the answer in force held the mapping, and as they are when it did not.
func TestMediaSessionWebrtcReOfferAnswerDecidesTheMIDExtension(t *testing.T) {
	ctx := t.Context()
	offerer, answerer, _, _ := newEstablishedWebrtcPair(t, nil)
	tagged := func(t *testing.T, seq uint16) bool {
		t.Helper()
		got := readWebrtcPacket(t, offerer, answerer, seq)
		if !got.Header.Extension {
			return false
		}
		require.Equal(t, []byte("0"), got.GetExtension(1), "the tag of packet %d", seq)
		return true
	}
	// answerReOffer has the peer answer a re-offer of the offerer, and hands
	// the offerer that answer with the a=extmap lines extmap.
	answerReOffer := func(t *testing.T, extmap []string) {
		t.Helper()
		reoffer, err := offerer.ReOffer("")
		require.NoError(t, err)
		require.Equal(t, []string{ownMIDExtmap}, sdpLines(reoffer, "a=extmap:"), "our re-offer\n%s", reoffer)
		require.NoError(t, answerer.RemoteSDP(ctx, reoffer, false))
		reanswer, err := answerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		finalizeWebrtcWithin(t, answerer, 5*time.Second)
		require.NoError(t, offerer.RemoteSDP(ctx, webrtcOfferWithExtmap(t, reanswer, nil, extmap), true))
	}
	require.True(t, tagged(t, 1), "the first answer held the mapping")

	// An answer that leaves the mapping out: staged, rolled back, and then
	// committed.
	answerReOffer(t, nil)
	assert.True(t, tagged(t, 2), "the exchange is not committed yet")
	offerer.Rollback()
	assert.True(t, tagged(t, 3), "the exchange was rolled back")
	answerReOffer(t, nil)
	finalizeWebrtcWithin(t, offerer, 5*time.Second)
	assert.False(t, tagged(t, 4), "the answer in force left the mapping out")

	// An answer that holds it again.
	answerReOffer(t, []string{ownMIDExtmap})
	assert.False(t, tagged(t, 5), "the exchange is not committed yet")
	offerer.Rollback()
	assert.False(t, tagged(t, 6), "the exchange was rolled back")
	answerReOffer(t, []string{ownMIDExtmap})
	finalizeWebrtcWithin(t, offerer, 5*time.Second)
	assert.True(t, tagged(t, 7), "the answer in force holds the mapping")

	// An offer of the peer that maps nothing takes the tag out of the
	// packets again: the answer cannot enable what the offer did not map.
	peerOffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	answerer.Rollback()
	require.NoError(t, offerer.RemoteSDP(ctx, sdpWithoutAttribute(peerOffer, "extmap"), false))
	staged, err := offerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Empty(t, sdpLines(staged, "a=extmap:"), "the answer to an offer that maps nothing\n%s", staged)
	finalizeWebrtcWithin(t, offerer, 5*time.Second)
	assert.False(t, tagged(t, 8), "the offer in force maps nothing")
	ours, err := offerer.ReOffer("")
	require.NoError(t, err)
	offerer.Rollback()
	assert.Equal(t, []string{ownMIDExtmap}, sdpLines(ours, "a=extmap:"), "our offer after it\n%s", ours)
}

// TestOwnOfferMID pins the header extension an offer of ours maps for a
// stream list: none outside a BUNDLE group, the mapping of the list's offer
// when it has one, the identifier that offer gave the extension when its
// mapping was not one to repeat, and otherwise the lowest identifier that
// offer left free, in the one-byte form before the two-byte one.
func TestOwnOfferMID(t *testing.T) {
	list := func(grouped bool, tag string, mid midExtension, extensions mappedExtensions) offeredStreams {
		return offeredStreams{
			media:      []sdp.MediaDescription{{MediaType: "audio"}},
			tags:       streamTags([]string{tag}),
			bundle:     bundledAudio{grouped: grouped, mid: mid},
			extensions: extensions,
		}
	}
	var oneByte, all []uint8
	for id := 1; id <= maxExtensionID; id++ {
		if id <= maxOneByteExtensionID {
			oneByte = append(oneByte, uint8(id))
		}
		if id != reservedOneByteExtensionID {
			all = append(all, uint8(id))
		}
	}
	for _, tc := range []struct {
		name    string
		streams offeredStreams
		want    midExtension
	}{
		{name: "no_group", streams: list(false, "a", midExtension{}, mappedExtensions{})},
		{name: "nothing_mapped", streams: list(true, "a", midExtension{}, mappedExtensions{}), want: midExtension{id: 1, tag: []byte("a")}},
		{
			name:    "the_mapping_of_the_offer",
			streams: list(true, "a", midExtension{id: 7, tag: []byte("a")}, mappedExtensions{ids: []uint8{1, 7}, mid: 7}),
			want:    midExtension{id: 7, tag: []byte("a")},
		},
		{
			name:    "the_identifier_of_a_mapping_not_repeated",
			streams: list(true, "a", midExtension{}, mappedExtensions{ids: []uint8{1, 7}, mid: 7, midDirection: "sendonly"}),
			want:    midExtension{id: 7, tag: []byte("a")},
		},
		{
			name:    "the_lowest_identifier_left_free",
			streams: list(true, "a", midExtension{}, mappedExtensions{ids: []uint8{2, 1, 4}}),
			want:    midExtension{id: 3, tag: []byte("a")},
		},
		{
			name:    "past_the_one-byte_form_and_its_reserved_identifier",
			streams: list(true, "a", midExtension{}, mappedExtensions{ids: oneByte}),
			want:    midExtension{id: 16, tag: []byte("a")},
		},
		{name: "no_identifier_left", streams: list(true, "a", midExtension{}, mappedExtensions{ids: all})},
		{name: "a_tag_longer_than_an_element_holds", streams: list(true, strings.Repeat("t", maxMIDLen+1), midExtension{}, mappedExtensions{})},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.streams.ownOfferMID())
			assert.Equal(t, tc.want, tc.streams.inOwnOffer().bundle.mid, "the list an offer of ours is written around")
			assert.Equal(t, tc.want.extmapLines(), tc.streams.inOwnOffer().bundle.mid.extmapLines())
		})
	}
}
