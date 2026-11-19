// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testRTCPMuxOnly = "a=rtcp-mux-only"

// withRTCPMuxOnly returns body, an SDP with one media section that has
// a=rtcp-mux, with a=rtcp-mux-only behind that line.
func withRTCPMuxOnly(t *testing.T, body []byte) []byte {
	t.Helper()
	require.Equal(t, 1, strings.Count(string(body), "a=rtcp-mux\r\n"), "the SDP\n%s", body)
	return []byte(strings.Replace(string(body), "a=rtcp-mux\r\n", "a=rtcp-mux\r\n"+testRTCPMuxOnly+"\r\n", 1))
}

// TestMediaSessionWebrtcAnswerRepeatsRTCPMuxOnly pins which SDPs of a session
// carry a=rtcp-mux-only.
//
// RFC 8843 section 9.3.1.2: "if the "m=" section that is selected as the
// offerer-tagged "m=" section contained an SDP 'rtcp-mux-only' attribute, the
// answerer MUST include an SDP 'rtcp-mux-only' attribute in the
// answerer-tagged "m=" section." The audio stream is the one bundled section
// the session accepts, so the answer to an offer that lists it in a BUNDLE
// group and gives it the line repeats the line there, beside the a=rtcp-mux
// line every SDP of ours has.
//
// RFC 8858 section 4.3: "The answerer MUST NOT associate an SDP
// 'rtcp-mux-only' attribute with an SDP media description ("m=" line) in the
// answer." So an audio stream offered outside a BUNDLE group is answered
// without it, and so is one whose group is not performed or whose a=group
// line is ignored (RFC 5888 section 6). A stream the session rejects carries
// neither line, and the line of such a stream says nothing of the audio one.
//
// An offer of ours has none: RFC 8843 section 9.3.1.1 has the offerer "MAY
// include" it.
func TestMediaSessionWebrtcAnswerRepeatsRTCPMuxOnly(t *testing.T) {
	const bundled = "a=group:BUNDLE audio"
	videoWith := func(tag string, lines ...string) string {
		return webrtcTaggedSection("video", tag, append([]string{"a=rtcp-mux"}, lines...)...)
	}
	for _, tc := range []struct {
		name string
		// group is the a=group line of the offer, whose audio stream has
		// the tag audio.
		group string
		// audioMuxOnly gives the audio section of the offer the line, and
		// session writes it at the session level.
		audioMuxOnly, session bool
		after                 []string
		// want is whether the answer carries the line in its audio section.
		want bool
	}{
		{name: "a_bundled_audio_stream_with_the_line", group: bundled, audioMuxOnly: true, want: true},
		{name: "a_bundled_audio_stream_without_it", group: bundled},
		{
			name:         "beside_a_bundled_stream_we_reject",
			group:        "a=group:BUNDLE audio v",
			audioMuxOnly: true,
			after:        []string{videoWith("v", testRTCPMuxOnly)},
			want:         true,
		},
		{
			// The line of a section we reject is not the audio stream's.
			name:  "on_a_bundled_stream_we_reject_alone",
			group: "a=group:BUNDLE audio v",
			after: []string{videoWith("v", testRTCPMuxOnly)},
		},
		{name: "an_audio_stream_in_no_group", audioMuxOnly: true},
		{name: "an_audio_stream_outside_the_group", group: "a=group:BUNDLE v", audioMuxOnly: true, after: []string{videoWith("v")}},
		{name: "a_group_of_other_semantics", group: "a=group:LS audio v", audioMuxOnly: true, after: []string{videoWith("v")}},
		{
			// RFC 5888 section 6: no grouping beside a stream without a
			// tag, so the audio stream is not a bundled one.
			name:         "a_group_beside_a_stream_without_a_tag",
			group:        bundled,
			audioMuxOnly: true,
			after:        []string{videoWith("")},
		},
		{
			// RFC 5888 section 6: a line that lists a tag of no m= line is
			// ignored, so the audio stream is not a bundled one.
			name:         "a_group_that_lists_a_tag_of_no_stream",
			group:        "a=group:BUNDLE audio v",
			audioMuxOnly: true,
		},
		{name: "written_at_the_session_level", group: bundled, session: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			alone, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			require.Empty(t, sdpLines(alone, testRTCPMuxOnly), "an offer of ours before any of the peer's\n%s", alone)
			if tc.audioMuxOnly {
				alone = withRTCPMuxOnly(t, alone)
			}
			if tc.session {
				alone = []byte(strings.Replace(string(alone), "t=0 0\r\n", "t=0 0\r\n"+testRTCPMuxOnly+"\r\n", 1))
			}
			offer := webrtcTaggedOffer(t, alone, tc.group, "audio", nil, tc.after)

			require.NoError(t, answerer.RemoteSDP(ctx, offer, false), "applying the offer\n%s", offer)
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			var want []string
			if tc.want {
				want = []string{testRTCPMuxOnly}
			}
			assert.Equal(t, want, sdpLines(answer, testRTCPMuxOnly), "the answer\n%s", answer)
			assert.Equal(t, want, audioSectionLines(t, answer, testRTCPMuxOnly), "the audio section of the answer\n%s", answer)
			assert.Contains(t, audioSectionLines(t, answer, "a=rtcp-mux"), "a=rtcp-mux", "the answer accepts the multiplexing\n%s", answer)

			ours, err := answerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			assert.Empty(t, sdpLines(ours, testRTCPMuxOnly), "our offer after the exchange\n%s", ours)
		})
	}
}

// TestMediaSessionWebrtcRTCPMuxOnlyFollowsEachOffer carries a call on an offer
// whose bundled audio stream has a=rtcp-mux-only, and then modifies the
// session. The line of an answer is that of the offer it answers alone: a
// re-offer of the peer without it is answered without it, and one with it
// with it. The offers of the session itself never have it, and the media
// runs through every exchange.
func TestMediaSessionWebrtcRTCPMuxOnlyFollowsEachOffer(t *testing.T) {
	muxOnly := []string{testRTCPMuxOnly}
	offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, func(offer string) string {
		return string(withRTCPMuxOnly(t, []byte(offer)))
	})
	require.Equal(t, []string{"a=group:BUNDLE 0"}, sdpLines(offer, "a=group:"), "the offer\n%s", offer)
	require.Equal(t, muxOnly, audioSectionLines(t, answer, testRTCPMuxOnly), "the answer\n%s", answer)
	requireWebrtcSRTPBetween(t, offerer, answerer, 1)
	requireWebrtcSRTPBetween(t, answerer, offerer, 2)

	// An offer of the session that answered, and the answer of its peer to
	// an offer without the line.
	reoffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	assert.Empty(t, sdpLines(reoffer, testRTCPMuxOnly), "our re-offer\n%s", reoffer)
	reanswer := exchangeWebrtcReOffer(t, answerer, offerer, reoffer)
	assert.Empty(t, sdpLines(reanswer, testRTCPMuxOnly), "the answer to an offer without the line\n%s", reanswer)

	// The peer offers again, with the line and then without it.
	peerOffer, err := offerer.ReOffer("")
	require.NoError(t, err)
	peerAnswer := exchangeWebrtcReOffer(t, offerer, answerer, withRTCPMuxOnly(t, peerOffer))
	assert.Equal(t, muxOnly, audioSectionLines(t, peerAnswer, testRTCPMuxOnly), "the answer to a re-offer with the line\n%s", peerAnswer)
	ours, err := answerer.ReOffer("")
	require.NoError(t, err)
	answerer.Rollback()
	assert.Empty(t, sdpLines(ours, testRTCPMuxOnly), "our offer after that exchange\n%s", ours)

	peerOffer, err = offerer.ReOffer("")
	require.NoError(t, err)
	peerAnswer = exchangeWebrtcReOffer(t, offerer, answerer, peerOffer)
	assert.Empty(t, sdpLines(peerAnswer, testRTCPMuxOnly), "the answer to a re-offer without the line\n%s", peerAnswer)

	requireWebrtcSRTPBetween(t, offerer, answerer, 3)
	requireWebrtcSRTPBetween(t, answerer, offerer, 4)
}

// TestMediaSessionWebrtcAnswersAPionPeerWithRTCPMuxOnly answers the offer of
// a Pion PeerConnection whose bundled audio stream has a=rtcp-mux-only, as an
// offerer that gathers no candidate for RTCP alone writes it. Pion takes the
// answer that repeats the line, and the audio runs both ways over SRTP.
func TestMediaSessionWebrtcAnswersAPionPeerWithRTCPMuxOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	peer := newPionPeer(t)
	localTrack, err := webrtc.NewTrackLocalStaticRTP(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "browser")
	require.NoError(t, err)
	_, err = peer.AddTrack(localTrack)
	require.NoError(t, err)
	remote := make(chan *webrtc.TrackRemote, 1)
	peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { remote <- track })
	offer := withRTCPMuxOnly(t, pionOffer(ctx, t, peer))
	require.Len(t, sdpLines(offer, "a=group:BUNDLE "), 1, "the offer\n%s", offer)

	session := newPionAnswerer(ctx, t)
	require.NoError(t, session.RemoteSDP(ctx, offer, false), "the offer\n%s", offer)
	answer, err := session.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.Equal(t, []string{testRTCPMuxOnly}, audioSectionLines(t, answer, testRTCPMuxOnly), "the answer\n%s", answer)
	require.NoError(t, peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}),
		"the answer\n%s", answer)

	rtpSession := NewRTPSessionWebrtc(session)
	packetWriter := NewRTPPacketWriter(rtpSession, session.Codec())
	require.NoError(t, session.Finalize(ctx))

	sent := []byte("audio-to-the-peer")
	_, err = packetWriter.WriteSamples(sent, CodecAudioUlaw.SampleTimestamp(), true, CodecAudioUlaw.PayloadType)
	require.NoError(t, err)
	select {
	case <-ctx.Done():
		t.Fatalf("Pion raised no track for our audio: %v\nthe answer\n%s", context.Cause(ctx), answer)
	case track := <-remote:
		pkt, _, readErr := track.ReadRTP()
		require.NoError(t, readErr)
		assert.Equal(t, sent, pkt.Payload)
	}

	received := []byte("audio-from-the-peer")
	sourceCtx, stopSource := context.WithCancel(ctx)
	source := pionAudioSource(sourceCtx, localTrack, received)
	defer func() {
		stopSource()
		if err := <-source; err != nil {
			t.Errorf("the Pion audio source: %v", err)
		}
	}()
	require.NoError(t, session.StopRTP(1, 5*time.Second))
	got := rtp.Packet{}
	_, err = session.ReadRTP(make([]byte, RTPBufSize), &got)
	require.NoError(t, err, "the media of the peer did not arrive and decrypt")
	require.NoError(t, session.StartRTP(1))
	assert.Equal(t, received, got.Payload)
}
