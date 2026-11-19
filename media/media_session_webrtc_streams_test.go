// SPDX-License-Identifier: MPL-2.0

package media

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The media sections a browser stack offers beside the audio one. Each carries
// lines of its own that say nothing of the audio stream.
const (
	webrtcStreamsVideo = "m=video 9 UDP/TLS/RTP/SAVPF 96 97\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:1\r\n" +
		"a=rtcp-mux\r\n" +
		"a=sendrecv\r\n" +
		"a=rtpmap:96 VP8/90000\r\n" +
		"a=rtpmap:97 H264/90000"
	webrtcStreamsRejectedVideo = "m=video 0 UDP/TLS/RTP/SAVPF 96 97"

	// webrtcStreamsBundleOnlyVideo is a video stream as an offerer with the
	// max-bundle policy writes it: port 0 and a=bundle-only, which is not a
	// disabled stream in an offer (RFC 8843 section 6).
	webrtcStreamsBundleOnlyVideo = "m=video 0 UDP/TLS/RTP/SAVPF 96 97\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:1\r\n" +
		"a=bundle-only\r\n" +
		"a=rtcp-mux\r\n" +
		"a=rtpmap:96 VP8/90000\r\n" +
		"a=rtpmap:97 H264/90000"

	webrtcStreamsData = "m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:2\r\n" +
		"a=sctp-port:5000"
	webrtcStreamsRejectedData = "m=application 0 UDP/DTLS/SCTP webrtc-datachannel"

	webrtcStreamsText = "m=text 9 UDP/TLS/RTP/SAVPF 98\r\n" +
		"a=mid:3\r\n" +
		"a=rtpmap:98 t140/1000"
	webrtcStreamsRejectedText = "m=text 0 UDP/TLS/RTP/SAVPF 98"
)

// withStreamsAround returns body, an SDP with one m= line, with the media
// sections before written ahead of that line's section and the sections after
// written behind it.
func withStreamsAround(t *testing.T, body []byte, before, after []string) []byte {
	t.Helper()
	session, audio, found := strings.Cut(string(body), "\r\nm=")
	require.True(t, found, "the SDP has no m= line:\n%s", body)
	require.NotContains(t, audio, "\r\nm=", "the SDP has more than one m= line:\n%s", body)
	require.True(t, strings.HasSuffix(audio, "\r\n"), "the SDP does not end its last line:\n%s", body)

	var b strings.Builder
	b.WriteString(session + "\r\n")
	for _, s := range before {
		b.WriteString(s + "\r\n")
	}
	b.WriteString("m=" + audio)
	for _, s := range after {
		b.WriteString(s + "\r\n")
	}
	return []byte(b.String())
}

// withoutOriginLine returns lines without their o= line, whose version differs
// between two SDPs of one session.
func withoutOriginLine(lines []string) []string {
	return slices.DeleteFunc(slices.Clone(lines), func(l string) bool { return strings.HasPrefix(l, "o=") })
}

// webrtcAudioSectionAlone returns the lines of body, an SDP of a WebRTC
// session written around more than one stream, as they describe the audio
// stream alone: without the section of each rejected stream, which runs from
// an m= line on port 0 to the next m= line, and without the a=ssrc line of the
// audio section, which only an SDP of several m= sections carries. It requires
// that line to be there once.
func webrtcAudioSectionAlone(t *testing.T, body []byte) []string {
	t.Helper()
	var kept, sources []string
	rejected := false
	for _, l := range streamsSDPLines(body) {
		if strings.HasPrefix(l, "m=") {
			fields := strings.Fields(l)
			rejected = len(fields) > 1 && fields[1] == "0"
		}
		if rejected {
			continue
		}
		if strings.HasPrefix(l, "a=ssrc:") {
			sources = append(sources, l)
			continue
		}
		kept = append(kept, l)
	}
	require.Len(t, sources, 1, "the a=ssrc lines of the audio section of\n%s", body)
	return kept
}

// webrtcReOfferMediaLines returns the m= lines of an offer of m that modifies
// the negotiated session, and leaves nothing of that offer staged.
func webrtcReOfferMediaLines(t *testing.T, m *MediaSessionWebrtc) []string {
	t.Helper()
	offer, err := m.ReOffer("")
	require.NoError(t, err)
	m.Rollback()
	return mediaLines(offer)
}

// TestMediaSessionWebrtcAnswersEveryOfferedStream pins RFC 3264 section 6 on
// the WebRTC media type: "The answer MUST contain exactly the same number of
// "m=" lines as the offer", in its order, and "To reject an offered stream,
// the port number in the corresponding stream in the answer MUST be set to
// zero". The first audio stream we can carry carries the call, read with the
// session level and its own section alone, and every other stream is rejected
// with its m= line, the tag the offer gave it and the a=rtpmap lines of its
// formats alone: TestMediaSessionWebrtcRejectedStreamKeepsItsRTPMaps pins the
// lines of a rejected section. Section 8 keeps those lines in every later SDP
// of ours: "Deleted media streams from a previous SDP MUST NOT be removed in a
// new SDP".
//
// A stream rejected out of a BUNDLE group is written the same way, which is
// what RFC 8843 section 7.3.3 asks: a zero port and no a=bundle-only. The
// a=group:BUNDLE list of our SDP names the tag of its audio stream alone, and
// each stream keeps the tag of the offer (RFC 5888 section 9.1).
func TestMediaSessionWebrtcAnswersEveryOfferedStream(t *testing.T) {
	type row struct {
		name string
		// before and after are the media sections offered ahead of the audio
		// section and behind it.
		before, after []string
		// group, when set, is the session-level BUNDLE group of the offer.
		group string
		// wantBefore and wantAfter are the m= lines the answer has around
		// its audio line.
		wantBefore, wantAfter []string
	}
	rows := []row{
		{
			name:      "audio_then_video",
			after:     []string{webrtcStreamsVideo},
			wantAfter: []string{webrtcStreamsRejectedVideo},
		},
		{
			name:       "video_then_audio",
			before:     []string{webrtcStreamsVideo},
			wantBefore: []string{webrtcStreamsRejectedVideo},
		},
		{
			name:      "audio_and_a_data_channel",
			after:     []string{webrtcStreamsData},
			wantAfter: []string{webrtcStreamsRejectedData},
		},
		{
			name:       "streams_on_both_sides",
			before:     []string{webrtcStreamsVideo},
			after:      []string{webrtcStreamsData, webrtcStreamsText},
			wantBefore: []string{webrtcStreamsRejectedVideo},
			wantAfter:  []string{webrtcStreamsRejectedData, webrtcStreamsRejectedText},
		},
		{
			name:      "two_audio_streams",
			after:     []string{"m=audio 9 UDP/TLS/RTP/SAVPF 8\r\na=mid:1\r\na=rtpmap:8 PCMA/8000\r\na=sendrecv"},
			wantAfter: []string{"m=audio 0 UDP/TLS/RTP/SAVPF 8"},
		},
		{
			// An offered stream on port 0 "MUST NOT be used" (RFC 3264
			// section 5.1): the audio stream behind it carries the call.
			name:       "an_audio_stream_offered_on_port_0",
			before:     []string{"m=audio 0 UDP/TLS/RTP/SAVPF 8\r\na=rtpmap:8 PCMA/8000"},
			wantBefore: []string{"m=audio 0 UDP/TLS/RTP/SAVPF 8"},
		},
		{
			name:       "an_audio_stream_on_a_transport_we_do_not_carry",
			before:     []string{"m=audio 9 TCP/MSRP *\r\na=sendrecv"},
			wantBefore: []string{"m=audio 0 TCP/MSRP *"},
		},
		{
			// A payload type, a fingerprint, a role and ICE credentials are
			// those of the section they are written in (RFC 8866 section 6):
			// the video's say nothing of the audio stream, whose payload type
			// 0 stays PCMU and whose keying stays its own.
			name: "a_video_section_with_lines_of_its_own",
			before: []string{"m=video 9 UDP/TLS/RTP/SAVPF 0\r\n" +
				"a=rtpmap:0 H264/90000\r\n" +
				"a=setup:holdconn\r\n" +
				"a=fingerprint:sha-256 " + strings.TrimSuffix(strings.Repeat("00:", 32), ":") + "\r\n" +
				"a=ice-ufrag:video\r\n" +
				"a=ice-pwd:videovideovideovideovideo\r\n" +
				"a=inactive"},
			wantBefore: []string{"m=video 0 UDP/TLS/RTP/SAVPF 0"},
		},
		{
			name:      "a_BUNDLE_group_of_audio_and_video",
			after:     []string{webrtcStreamsVideo},
			group:     "a=group:BUNDLE 0 1",
			wantAfter: []string{webrtcStreamsRejectedVideo},
		},
		{
			name:      "a_BUNDLE_group_with_a_bundle-only_video",
			after:     []string{webrtcStreamsBundleOnlyVideo},
			group:     "a=group:BUNDLE 0 1",
			wantAfter: []string{webrtcStreamsRejectedVideo},
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)

			alone, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			// The offerer's audio stream is the group's first, tag 0.
			require.Equal(t, []string{"a=group:BUNDLE 0"}, sdpLines(alone, "a=group:"), "the offer\n%s", alone)
			require.Equal(t, []string{"a=mid:0"}, sdpLines(alone, "a=mid:"), "the offer\n%s", alone)
			offer := withStreamsAround(t, alone, r.before, r.after)
			if r.group != "" {
				offer = []byte(strings.Replace(string(offer), "a=group:BUNDLE 0\r\n", r.group+"\r\n", 1))
			}

			require.NoError(t, answerer.RemoteSDP(ctx, offer, false), "applying the offer\n%s", offer)
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			assert.Equal(t, []Codec{CodecAudioUlaw}, answerer.CommonCodecs(), "the audio codecs")

			// The audio stream offered alone, on the ICE generation in use.
			require.NoError(t, answerer.RemoteSDP(ctx, alone, false))
			aloneAnswer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			audio := mediaLines(aloneAnswer)
			require.Len(t, audio, 1, "the answer to the audio stream alone\n%s", aloneAnswer)
			require.NotEqual(t, 0, sdpAudioPort(t, aloneAnswer))

			want := slices.Concat(r.wantBefore, audio, r.wantAfter)
			assert.Equal(t, want, mediaLines(answer), "the answer's m= lines\n%s", answer)
			assert.Equal(t, withoutOriginLine(streamsSDPLines(aloneAnswer)), withoutOriginLine(webrtcAudioSectionAlone(t, answer)),
				"the audio stream is answered as it is when offered alone, apart from the source it declares")

			// RFC 8843 section 7.3.3: a rejected stream has no
			// a=bundle-only, and the group names the audio stream alone. RFC
			// 5888 section 9.1: each stream answers under the offer's tag.
			assert.Empty(t, sdpLines(answer, "a=bundle-only"), "the answer\n%s", answer)
			assert.Equal(t, []string{"a=group:BUNDLE 0"}, sdpLines(answer, "a=group:"), "the answer's group\n%s", answer)
			assert.Equal(t, streamTagsOf(offer), streamTagsOf(answer), "the answer's tags\n%s", answer)
		})
	}

	t.Run("video_alone", func(t *testing.T) {
		ctx := t.Context()
		offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
		alone, err := offerer.LocalSDP(ctx, false)
		require.NoError(t, err)
		session, _, found := strings.Cut(string(alone), "\r\nm=")
		require.True(t, found)

		err = answerer.RemoteSDP(ctx, []byte(session+"\r\n"+webrtcStreamsVideo+"\r\n"), false)
		require.ErrorIs(t, err, ErrNoCommonMedia)
	})

	t.Run("a_truncated_m=_line", func(t *testing.T) {
		ctx := t.Context()
		offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
		alone, err := offerer.LocalSDP(ctx, false)
		require.NoError(t, err)

		err = answerer.RemoteSDP(ctx, withStreamsAround(t, alone, nil, []string{"m=video 9"}), false)
		require.Error(t, err)
	})

	t.Run("one_audio_stream", func(t *testing.T) {
		ctx := t.Context()
		offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
		_, answer := negotiateWebrtc(t, offerer, answerer, nil)

		lines := streamsSDPLines(answer)
		assert.Len(t, mediaLines(answer), 1, "the answer\n%s", answer)
		assert.Equal(t, "a=msid:diago audio", lines[len(lines)-1], "nothing follows the audio section\n%s", answer)
	})

	t.Run("our_later_offer_keeps_every_stream", func(t *testing.T) {
		ctx := t.Context()
		offerer, answerer, _, answer := newEstablishedWebrtcPair(t, func(offer string) string {
			return string(withStreamsAround(t, []byte(offer), []string{webrtcStreamsVideo}, []string{webrtcStreamsData}))
		})
		audio := "m=audio " + strings.Fields(sdpLine(t, answer, "m=audio "))[1] + " UDP/TLS/RTP/SAVPF 0"
		want := []string{webrtcStreamsRejectedVideo, audio, webrtcStreamsRejectedData}
		require.Equal(t, want, mediaLines(answer), "the answer\n%s", answer)

		// An offer on the transport in use, as a refresh or a hold makes it.
		reoffer, err := answerer.ReOffer("")
		require.NoError(t, err)
		assert.Equal(t, want, mediaLines(reoffer), "our offer\n%s", reoffer)

		// The peer's answer to it keeps them too, and so does the offer after.
		reanswer := exchangeWebrtcReOffer(t, answerer, offerer, reoffer)
		assert.Len(t, mediaLines(reanswer), 3, "the answer to our offer\n%s", reanswer)
		assert.Equal(t, want, webrtcReOfferMediaLines(t, answerer), "the offer after")
		requireWebrtcSRTPBetween(t, answerer, offerer, 10)
		requireWebrtcSRTPBetween(t, offerer, answerer, 11)

		// An offer of the session itself.
		offer, err := answerer.LocalSDP(ctx, false)
		require.NoError(t, err)
		assert.Equal(t, want, mediaLines(offer), "an offer of the session\n%s", offer)

		// An offer that restarts ICE describes the same session from new
		// candidates.
		fork, err := answerer.Fork(ctx)
		require.NoError(t, err)
		closeWebrtcWithTest(t, fork)
		forkOffer, err := fork.LocalSDP(ctx, false)
		require.NoError(t, err)
		forkAudio := "m=audio " + strings.Fields(sdpLine(t, forkOffer, "m=audio "))[1] + " UDP/TLS/RTP/SAVPF 0"
		assert.Equal(t, []string{webrtcStreamsRejectedVideo, forkAudio, webrtcStreamsRejectedData}, mediaLines(forkOffer),
			"an offer of its fork\n%s", forkOffer)
	})

	t.Run("a_re-offer_of_the_peer_adding_a_stream", func(t *testing.T) {
		ctx := t.Context()
		offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, func(offer string) string {
			return string(withStreamsAround(t, []byte(offer), nil, []string{webrtcStreamsVideo}))
		})
		audio := sdpLine(t, answer, "m=audio ")
		before := []string{audio, webrtcStreamsRejectedVideo}
		after := []string{audio, webrtcStreamsRejectedVideo, webrtcStreamsRejectedText}
		require.Equal(t, before, mediaLines(answer), "the first answer\n%s", answer)
		reoffer := []byte(string(offer) + webrtcStreamsText + "\r\n")

		// The re-offer is answered stream by stream, and an exchange that
		// fails leaves our SDPs written around the offer before it.
		require.NoError(t, answerer.RemoteSDP(ctx, reoffer, false))
		reanswer, err := answerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		assert.Equal(t, after, mediaLines(reanswer), "the answer to the re-offer\n%s", reanswer)
		answerer.Rollback()
		ours, err := answerer.ReOffer("")
		require.NoError(t, err)
		assert.Equal(t, before, mediaLines(ours), "our offer after the exchange failed\n%s", ours)
		// An exchange of ours that is committed brings nothing of it back.
		exchangeWebrtcReOffer(t, answerer, offerer, ours)
		assert.Equal(t, before, webrtcReOfferMediaLines(t, answerer), "our offer after an exchange of ours")

		// Committed, it is the list our SDPs are written around.
		require.NoError(t, answerer.RemoteSDP(ctx, reoffer, false))
		reanswer, err = answerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		assert.Equal(t, after, mediaLines(reanswer), "the answer to the re-offer made again\n%s", reanswer)
		finalizeWebrtcWithin(t, answerer, 5*time.Second)
		assert.Equal(t, after, webrtcReOfferMediaLines(t, answerer), "our offer after the exchange")
	})

	t.Run("an_ICE_restart_of_the_peer_adding_a_stream", func(t *testing.T) {
		ctx := t.Context()
		_, answerer, _, _ := newEstablishedWebrtcPair(t, func(offer string) string {
			return string(withStreamsAround(t, []byte(offer), nil, []string{webrtcStreamsVideo}))
		})

		// A new generation of the peer: new credentials, which the session in
		// use refuses and a fork of it answers.
		restarted := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		restart, err := restarted.LocalSDP(ctx, false)
		require.NoError(t, err)
		restart = withStreamsAround(t, restart, nil, []string{webrtcStreamsVideo, webrtcStreamsText})
		require.ErrorIs(t, answerer.RemoteSDP(ctx, restart, false), ErrWebRTCICERestart)

		fork, err := answerer.Fork(ctx)
		require.NoError(t, err)
		closeWebrtcWithTest(t, fork)
		require.NoError(t, fork.RemoteSDP(ctx, restart, false))
		answer, err := fork.LocalSDP(ctx, true)
		require.NoError(t, err)
		assert.Equal(t, []string{sdpLine(t, answer, "m=audio "), webrtcStreamsRejectedVideo, webrtcStreamsRejectedText},
			mediaLines(answer), "the fork's answer\n%s", answer)
	})
}

// TestMediaSessionWebrtcRefusesAnOfferWithoutAnAudioStreamToCarry pins the
// offers no audio stream of which can carry the call: they are refused with
// ErrNoCommonMedia, and the session answers nothing.
func TestMediaSessionWebrtcRefusesAnOfferWithoutAnAudioStreamToCarry(t *testing.T) {
	for _, tc := range []struct {
		name string
		// rewrite makes the offer from one with a single audio stream.
		rewrite func(t *testing.T, alone []byte) []byte
	}{
		{
			name: "audio_on_port_0",
			rewrite: func(t *testing.T, alone []byte) []byte {
				old := sdpLine(t, alone, "m=audio ")
				fields := strings.Fields(old)
				fields[1] = "0"
				return []byte(strings.Replace(string(alone), old, strings.Join(fields, " "), 1))
			},
		},
		{
			name: "audio_on_a_transport_we_do_not_carry",
			rewrite: func(t *testing.T, alone []byte) []byte {
				return sdpWithProto(t, alone, "TCP/MSRP")
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			alone, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)

			offer := withStreamsAround(t, tc.rewrite(t, alone), nil, []string{webrtcStreamsVideo})
			require.ErrorIs(t, answerer.RemoteSDP(ctx, offer, false), ErrNoCommonMedia, "the offer\n%s", offer)
			_, err = answerer.LocalSDP(ctx, true)
			require.Error(t, err, "an answer to the refused offer")
		})
	}
}
