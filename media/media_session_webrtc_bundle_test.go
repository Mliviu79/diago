// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"crypto/tls"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/pion/ice/v4"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webrtcTaggedSection returns a media section a browser stack offers beside
// the audio one, identified by tag, or by nothing when tag is empty, with
// lines of its own behind the tag.
func webrtcTaggedSection(media, tag string, lines ...string) string {
	section := []string{"m=" + media + " 9 UDP/TLS/RTP/SAVPF 96", "c=IN IP4 0.0.0.0"}
	if tag != "" {
		section = append(section, "a=mid:"+tag)
	}
	section = append(section, lines...)
	return strings.Join(append(section, "a=rtpmap:96 VP8/90000"), "\r\n")
}

// webrtcTaggedOffer returns alone, an offer of ours with its audio stream
// alone, as a peer identifying its streams would send it: group replaces the
// a=group line and audioTag the tag of the audio stream, an empty value
// removing the line, and before and after are the media sections around the
// audio one.
func webrtcTaggedOffer(t *testing.T, alone []byte, group, audioTag string, before, after []string) []byte {
	t.Helper()
	replace := func(body, old, value string) string {
		require.Equal(t, 1, strings.Count(body, old), "%q in the offer\n%s", old, body)
		if value != "" {
			value += "\r\n"
		}
		return strings.Replace(body, old, value, 1)
	}
	body := replace(string(alone), "a=group:BUNDLE 0\r\n", group)
	if audioTag != "" {
		audioTag = "a=mid:" + audioTag
	}
	body = replace(body, "a=mid:0\r\n", audioTag)
	return withStreamsAround(t, []byte(body), before, after)
}

// TestMediaSessionWebrtcAnswerKeepsTheOfferedTags pins how the SDPs of a
// session identify its streams once a peer's offer has.
//
// RFC 5888 section 9.1: "the "mid" value in the offer MUST be the same as the
// "mid" value in the answer", and "subsequent offers (e.g., in a re-INVITE)
// SHOULD use the same "mid" value for the already existing media streams".
// Section 9.2 and the examples of RFC 8843 section 18.5 keep the a=mid line of
// a refused stream. RFC 8843 section 7.3 lets the answer hold a BUNDLE group,
// and a stream in it, only where the offer had them, and section 7.3.3 keeps
// the tag of a rejected stream out of the group's list.
//
// RFC 5888 section 6: "If a session description contains at least one "m"
// line that has no "mid" identification, the application MUST NOT perform any
// grouping of media lines." An offer with such a line is answered without an
// a=group line, whatever its own lists, and each stream keeps the tag it has.
// The same section: ""a=group" lines that contain identification-tags that do
// not correspond to any "m" line within the session description MUST be
// ignored." An offer whose BUNDLE group lists such a tag beside that of the
// audio stream did not request a group we may answer with one (RFC 8843
// section 7.3), so it is answered without the line too.
//
// An offer that identifies no stream and asks for no group is answered with
// neither line. RFC 8829 section 5.3.1 has each m= section of an answer
// contain, "If and only if present in the offer, an "a=mid" line", and the
// same sentence of RFC 8843 section 7.3 that keeps a group the offer did not
// ask for out of the answer applies: "The answerer is only allowed to include
// a BUNDLE group in an initial BUNDLE answer if the offerer requested the
// BUNDLE group to be created in the corresponding initial BUNDLE offer".
func TestMediaSessionWebrtcAnswerKeepsTheOfferedTags(t *testing.T) {
	type row struct {
		name string
		// group and audioTag are the a=group line of the offer and the tag
		// of its audio stream, either empty for an offer without the line.
		group, audioTag string
		before, after   []string
		// wantGroup is the a=group lines of our SDPs.
		wantGroup []string
		// wantTags is the tag of each stream of our SDPs, in order.
		wantTags []string
	}
	rows := []row{
		{
			name:      "an_audio_stream_under_another_tag",
			group:     "a=group:BUNDLE audio",
			audioTag:  "audio",
			wantGroup: []string{"a=group:BUNDLE audio"},
			wantTags:  []string{"audio"},
		},
		{
			// The stream we reject has the tag 0 and leads the group: the
			// group of the answer names the audio stream's tag, not that one.
			name:      "a_video_stream_first_in_the_group",
			group:     "a=group:BUNDLE 0 1",
			audioTag:  "1",
			before:    []string{webrtcTaggedSection("video", "0")},
			wantGroup: []string{"a=group:BUNDLE 1"},
			wantTags:  []string{"0", "1"},
		},
		{
			name:      "streams_on_both_sides_of_the_audio",
			group:     "a=group:BUNDLE v a d",
			audioTag:  "a",
			before:    []string{webrtcTaggedSection("video", "v")},
			after:     []string{"m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:d\r\na=sctp-port:5000"},
			wantGroup: []string{"a=group:BUNDLE a"},
			wantTags:  []string{"v", "a", "d"},
		},
		{
			name:      "a_bundle-only_video_stream",
			group:     "a=group:BUNDLE a v",
			audioTag:  "a",
			after:     []string{"m=video 0 UDP/TLS/RTP/SAVPF 96\r\na=mid:v\r\na=bundle-only\r\na=rtpmap:96 VP8/90000"},
			wantGroup: []string{"a=group:BUNDLE a"},
			wantTags:  []string{"a", "v"},
		},
		{
			// RFC 8843 section 7.3: no BUNDLE group in the answer that the
			// offer did not ask for.
			name:     "tags_without_a_group",
			audioTag: "a",
			after:    []string{webrtcTaggedSection("video", "v")},
			wantTags: []string{"a", "v"},
		},
		{
			name:     "a_group_that_does_not_list_the_audio_stream",
			group:    "a=group:BUNDLE v t",
			audioTag: "a",
			after:    []string{webrtcTaggedSection("video", "v"), webrtcTaggedSection("text", "t")},
			wantTags: []string{"a", "v", "t"},
		},
		{
			// RFC 5888 section 9.2: a group of semantics we do not implement
			// is answered without its line, and the tags stay.
			name:     "a_group_of_other_semantics",
			group:    "a=group:LS a v",
			audioTag: "a",
			after:    []string{webrtcTaggedSection("video", "v")},
			wantTags: []string{"a", "v"},
		},
		{
			name:     "an_audio_stream_without_a_tag_beside_a_tagged_one",
			after:    []string{webrtcTaggedSection("video", "v")},
			wantTags: []string{"", "v"},
		},
		{
			// RFC 8829 section 5.3.1 and RFC 8843 section 7.3: neither an
			// a=mid line nor a BUNDLE group the offer did not have.
			name:     "no_tag_and_no_group",
			after:    []string{webrtcTaggedSection("video", "")},
			wantTags: []string{"", ""},
		},
		{
			name:     "one_audio_stream_with_no_tag_and_no_group",
			wantTags: []string{""},
		},
		{
			// A group lists streams by their tags, so one over streams
			// without any lists none of them.
			name:     "a_group_and_no_tag",
			group:    "a=group:BUNDLE 0",
			wantTags: []string{""},
		},
		{
			// RFC 5888 section 4: a tag is a token. A value that is not one
			// is not written back, and the stream counts as not identified:
			// no tag, and in no group.
			name:     "an_audio_tag_that_is_no_token",
			group:    "a=group:BUNDLE a b",
			audioTag: "a b",
			wantTags: []string{""},
		},
		{
			// RFC 5888 section 6: no grouping in a description that leaves
			// a stream without an a=mid line, also when the group lists the
			// audio stream and the stream without a tag is one we reject.
			name:     "a_group_beside_a_stream_without_a_tag",
			group:    "a=group:BUNDLE a",
			audioTag: "a",
			after:    []string{webrtcTaggedSection("video", "")},
			wantTags: []string{"a", ""},
		},
		{
			// A value that is no token is no tag, so the stream is one
			// without an a=mid line and the offer groups nothing.
			name:     "a_rejected_stream_whose_tag_is_no_token",
			group:    "a=group:BUNDLE a v",
			audioTag: "a",
			after:    []string{webrtcTaggedSection("video", "v w")},
			wantTags: []string{"a", ""},
		},
		{
			// The stream without a tag is one the SDP parser of the session
			// does not read.
			name:     "a_group_beside_a_T.38_stream_without_a_tag",
			group:    "a=group:BUNDLE a",
			audioTag: "a",
			after:    []string{"m=image 30004 udptl t38"},
			wantTags: []string{"a", ""},
		},
		{
			name:      "a_group_beside_an_identified_T.38_stream",
			group:     "a=group:BUNDLE a",
			audioTag:  "a",
			after:     []string{"m=image 30004 udptl t38\r\na=mid:fax"},
			wantGroup: []string{"a=group:BUNDLE a"},
			wantTags:  []string{"a", "fax"},
		},
		{
			// RFC 5888 section 6: an a=group line that lists a tag of no
			// m= line is ignored. This is the offer of two bundled streams
			// that reaches us without the section of the second.
			name:     "a_group_that_lists_a_tag_of_no_stream",
			group:    "a=group:BUNDLE a v",
			audioTag: "a",
			wantTags: []string{"a"},
		},
		{
			name:     "a_group_that_lists_a_tag_of_no_stream_beside_those_of_two",
			group:    "a=group:BUNDLE v a d",
			audioTag: "a",
			before:   []string{webrtcTaggedSection("video", "v")},
			wantTags: []string{"v", "a"},
		},
		{
			// The tag of a stream the SDP parser of the session does not
			// read is the tag of an m= line of the offer.
			name:      "a_group_that_lists_an_identified_T.38_stream",
			group:     "a=group:BUNDLE a fax",
			audioTag:  "a",
			after:     []string{"m=image 30004 udptl t38\r\na=mid:fax"},
			wantGroup: []string{"a=group:BUNDLE a"},
			wantTags:  []string{"a", "fax"},
		},
		{
			name:     "a_group_that_lists_a_tag_of_no_stream_beside_a_T.38_stream",
			group:    "a=group:BUNDLE a fax v",
			audioTag: "a",
			after:    []string{"m=image 30004 udptl t38\r\na=mid:fax"},
			wantTags: []string{"a", "fax"},
		},
		{
			// The line that lists a tag of no stream is ignored alone: the
			// group of the next line is the one the answer keeps.
			name:      "a_group_behind_a_line_that_lists_a_tag_of_no_stream",
			group:     "a=group:BUNDLE a x\r\na=group:BUNDLE a v",
			audioTag:  "a",
			after:     []string{webrtcTaggedSection("video", "v")},
			wantGroup: []string{"a=group:BUNDLE a"},
			wantTags:  []string{"a", "v"},
		},
		{
			// RFC 8843 section 5 keeps a stream in one BUNDLE group, so an
			// SDP of ours names the audio stream in one line alone.
			name:      "two_groups_that_list_the_audio_stream",
			group:     "a=group:BUNDLE a v\r\na=group:BUNDLE a t",
			audioTag:  "a",
			after:     []string{webrtcTaggedSection("video", "v"), webrtcTaggedSection("text", "t")},
			wantGroup: []string{"a=group:BUNDLE a"},
			wantTags:  []string{"a", "v", "t"},
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			alone, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			offer := webrtcTaggedOffer(t, alone, r.group, r.audioTag, r.before, r.after)

			require.NoError(t, answerer.RemoteSDP(ctx, offer, false), "applying the offer\n%s", offer)
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			assert.Equal(t, r.wantGroup, sdpLines(answer, "a=group:"), "the answer's group\n%s", answer)
			assert.Equal(t, r.wantTags, streamTagsOf(answer), "the answer's tags\n%s", answer)
			assert.Len(t, sdpLines(answer, "a=mid:"), len(slices.DeleteFunc(slices.Clone(r.wantTags), func(tag string) bool { return tag == "" })),
				"the answer's a=mid lines\n%s", answer)
			assert.Empty(t, sdpLines(answer, "a=bundle-only"), "the answer\n%s", answer)
			assert.Len(t, mediaLines(answer), len(mediaLines(offer)), "the answer's m= lines\n%s", answer)

			// An offer the session makes afterwards describes the same
			// streams under the same tags.
			ours, err := answerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			assert.Equal(t, r.wantGroup, sdpLines(ours, "a=group:"), "the group of our offer\n%s", ours)
			assert.Equal(t, r.wantTags, streamTagsOf(ours), "the tags of our offer\n%s", ours)
		})
	}
}

// TestMediaSessionWebrtcLaterSDPsKeepTheTags follows the tags of an offer
// through the life of a negotiated session: our re-offer, the offer of an ICE
// restart, and a re-offer of the peer that renames a stream, whose tags are
// the session's once the exchange is committed and not before.
func TestMediaSessionWebrtcLaterSDPsKeepTheTags(t *testing.T) {
	ctx := t.Context()
	identify := func(group, audioTag, videoTag string) func(string) string {
		return func(offer string) string {
			return string(webrtcTaggedOffer(t, []byte(offer), group, audioTag, []string{webrtcTaggedSection("video", videoTag)}, nil))
		}
	}
	offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, identify("a=group:BUNDLE 0 1", "1", "0"))
	group, tags := []string{"a=group:BUNDLE 1"}, []string{"0", "1"}
	require.Equal(t, group, sdpLines(answer, "a=group:"), "the answer\n%s", answer)
	require.Equal(t, tags, streamTagsOf(answer), "the answer\n%s", answer)
	requireWebrtcSRTPBetween(t, offerer, answerer, 1)
	requireWebrtcSRTPBetween(t, answerer, offerer, 2)

	// An offer on the transport in use, and the peer's answer to it.
	reoffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	assert.Equal(t, group, sdpLines(reoffer, "a=group:"), "our re-offer\n%s", reoffer)
	assert.Equal(t, tags, streamTagsOf(reoffer), "our re-offer\n%s", reoffer)
	reanswer := exchangeWebrtcReOffer(t, answerer, offerer, reoffer)
	assert.Equal(t, group, sdpLines(reanswer, "a=group:"), "the answer to our re-offer\n%s", reanswer)
	assert.Equal(t, tags, streamTagsOf(reanswer), "the answer to our re-offer\n%s", reanswer)

	// An offer that restarts ICE describes the same session.
	fork, err := answerer.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, fork)
	forkOffer, err := fork.LocalSDP(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, group, sdpLines(forkOffer, "a=group:"), "the offer of a fork\n%s", forkOffer)
	assert.Equal(t, tags, streamTagsOf(forkOffer), "the offer of a fork\n%s", forkOffer)

	// A re-offer of the peer under other tags is answered under them, and an
	// exchange that fails leaves the session under the ones it had.
	renamed := []byte(strings.NewReplacer(
		"a=group:BUNDLE 0 1\r\n", "a=group:BUNDLE video audio\r\n",
		"a=mid:0\r\n", "a=mid:video\r\n",
		"a=mid:1\r\n", "a=mid:audio\r\n",
	).Replace(string(offer)))
	renamedGroup, renamedTags := []string{"a=group:BUNDLE audio"}, []string{"video", "audio"}
	require.NoError(t, answerer.RemoteSDP(ctx, renamed, false))
	staged, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, renamedGroup, sdpLines(staged, "a=group:"), "the answer to the renaming offer\n%s", staged)
	assert.Equal(t, renamedTags, streamTagsOf(staged), "the answer to the renaming offer\n%s", staged)
	answerer.Rollback()
	ours, err := answerer.ReOffer("")
	require.NoError(t, err)
	answerer.Rollback()
	assert.Equal(t, group, sdpLines(ours, "a=group:"), "our offer after the exchange failed\n%s", ours)
	assert.Equal(t, tags, streamTagsOf(ours), "our offer after the exchange failed\n%s", ours)

	require.NoError(t, answerer.RemoteSDP(ctx, renamed, false))
	_, err = answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	finalizeWebrtcWithin(t, answerer, 5*time.Second)
	ours, err = answerer.ReOffer("")
	require.NoError(t, err)
	answerer.Rollback()
	assert.Equal(t, renamedGroup, sdpLines(ours, "a=group:"), "our offer after the exchange\n%s", ours)
	assert.Equal(t, renamedTags, streamTagsOf(ours), "our offer after the exchange\n%s", ours)
	requireWebrtcSRTPBetween(t, answerer, offerer, 3)
}

// TestWebRTCBundleGroupNeedsEveryStreamIdentified pins which descriptions
// group their audio stream. RFC 5888 section 6: "All of the "m" lines of a
// session description that uses "group" MUST be identified with a "mid"
// attribute whether they appear in the group line(s) or not. If a session
// description contains at least one "m" line that has no "mid"
// identification, the application MUST NOT perform any grouping of media
// lines." So a group is read only in a description whose every stream has an
// a=mid line with a tag for its value, the streams its SDP parser is not given
// included.
func TestWebRTCBundleGroupNeedsEveryStreamIdentified(t *testing.T) {
	const (
		session = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\na=group:BUNDLE 1 2\r\n"
		audio   = "m=audio 9 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\na=mid:1\r\n"
		video   = "m=video 9 UDP/TLS/RTP/SAVPF 96\r\n"
		image   = "m=image 30004 udptl t38\r\n"
	)
	group := []string{"1", "2"}
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{name: "every_stream_identified", body: session + audio + video + "a=mid:2\r\n", want: group},
		{name: "every_stream_identified_and_one_outside_the_group", body: session + audio + video + "a=mid:2\r\n" + video + "a=mid:3\r\n", want: group},
		{name: "an_unread_stream_identified", body: session + audio + video + "a=mid:2\r\n" + image + "a=mid:fax\r\n", want: group},
		{name: "a_listed_stream_without_a_tag", body: session + audio + video},
		{name: "a_stream_outside_the_group_without_a_tag", body: session + audio + video + "a=mid:2\r\n" + video},
		{name: "a_stream_whose_tag_is_no_token", body: session + audio + video + "a=mid:2\r\n" + video + "a=mid:3 4\r\n"},
		{name: "an_unread_stream_without_a_tag", body: session + audio + video + "a=mid:2\r\n" + image},
		{name: "an_unread_stream_whose_tag_is_no_token", body: session + audio + video + "a=mid:2\r\n" + image + "a=mid:f x\r\n"},
		{name: "the_audio_stream_without_a_tag", body: session + "m=audio 9 UDP/TLS/RTP/SAVPF 0\r\n" + video + "a=mid:2\r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc, unread, err := unmarshalWebRTCDescription([]byte(tc.body))
			require.NoError(t, err)
			audio := webRTCAudioStream(&desc)
			require.NotNil(t, audio)

			assert.Equal(t, tc.want, webRTCBundleGroup(&desc, unread, "1"), "the group of the audio stream")
			streams := webRTCOfferedStreams(&desc, unread, audio)
			assert.Equal(t, tc.want != nil, streams.bundle.grouped, "the audio stream is listed in a group")
			if tc.want == nil {
				assert.Empty(t, streams.bundleGroupLines(), "the a=group lines of our answer")
			}
		})
	}
}

// TestWebRTCBundleGroupListsStreamsOfTheDescriptionAlone pins which a=group
// lines of a description are groups of it. RFC 5888 section 6: ""a=group"
// lines that contain identification-tags that do not correspond to any "m"
// line within the session description MUST be ignored. The application acts
// as if the "a=group" line did not exist." So a line that lists the audio
// stream's tag beside a tag of no m= line groups nothing, the other a=group
// lines of the description stand, and the tag of a stream the SDP parser is
// not given is the tag of an m= line like the others.
//
// A line that stands is read as it is written. No sentence has a tag listed
// twice in it change the line, and of two BUNDLE groups that list the audio
// stream, which RFC 8843 section 5 keeps a description from having, the first
// is the group of the stream: our SDPs name it in one group alone.
func TestWebRTCBundleGroupListsStreamsOfTheDescriptionAlone(t *testing.T) {
	const (
		session = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"
		audio   = "m=audio 9 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\na=mid:1\r\n"
		video   = "m=video 9 UDP/TLS/RTP/SAVPF 96\r\n"
		image   = "m=image 30004 udptl t38\r\n"
	)
	groups := func(lists ...string) string {
		var lines string
		for _, list := range lists {
			lines += "a=group:" + list + "\r\n"
		}
		return lines
	}
	for _, tc := range []struct {
		name string
		body string
		want []string
	}{
		{
			name: "every_listed_tag_that_of_a_stream",
			body: session + groups("BUNDLE 1 2") + audio + video + "a=mid:2\r\n",
			want: []string{"1", "2"},
		},
		{
			name: "a_tag_of_no_stream_beside_the_audio_tag",
			body: session + groups("BUNDLE 1 2") + audio,
		},
		{
			name: "a_tag_of_no_stream_behind_those_of_two_streams",
			body: session + groups("BUNDLE 1 2 3") + audio + video + "a=mid:2\r\n",
		},
		{
			name: "a_tag_of_no_stream_ahead_of_the_audio_tag",
			body: session + groups("BUNDLE 0 1") + audio,
		},
		{
			// A tag is compared as it is written: RFC 8866 section 5 has a
			// value "case-significant unless a specific field defines
			// otherwise", and RFC 5888 defines nothing else for a tag.
			name: "a_tag_in_another_case_than_the_stream_has_it",
			body: session + groups("BUNDLE 1 V") + audio + video + "a=mid:v\r\n",
		},
		{
			// The value of that a=mid line is no token, so the stream has no
			// tag and the description no group at all.
			name: "a_listed_value_that_is_no_tag_of_its_stream",
			body: session + groups("BUNDLE 1 2") + audio + video + "a=mid:2 3\r\n",
		},
		{
			name: "a_stream_refused_with_port_0_is_a_stream",
			body: session + groups("BUNDLE 1 2") + audio + "m=video 0 UDP/TLS/RTP/SAVPF 96\r\na=mid:2\r\n",
			want: []string{"1", "2"},
		},
		{
			name: "the_tag_of_an_unread_stream",
			body: session + groups("BUNDLE 1 fax") + audio + image + "a=mid:fax\r\n",
			want: []string{"1", "fax"},
		},
		{
			name: "the_tag_of_an_unread_stream_ahead_of_the_audio_one",
			body: session + groups("BUNDLE fax 1") + image + "a=mid:fax\r\n" + audio,
			want: []string{"fax", "1"},
		},
		{
			name: "a_tag_of_no_stream_beside_that_of_an_unread_stream",
			body: session + groups("BUNDLE 1 fax 2") + audio + image + "a=mid:fax\r\n",
		},
		{
			name: "an_unread_stream_outside_a_group_that_lists_no_stream",
			body: session + groups("BUNDLE 1 2") + audio + image + "a=mid:fax\r\n",
		},
		{
			// The sentence is about the one line: the next stands.
			name: "a_group_behind_a_line_that_lists_no_stream",
			body: session + groups("BUNDLE 1 9", "BUNDLE 1 2") + audio + video + "a=mid:2\r\n",
			want: []string{"1", "2"},
		},
		{
			name: "a_group_ahead_of_a_line_that_lists_no_stream",
			body: session + groups("BUNDLE 1 2", "BUNDLE 1 9") + audio + video + "a=mid:2\r\n",
			want: []string{"1", "2"},
		},
		{
			name: "a_line_of_other_semantics_that_lists_no_stream",
			body: session + groups("LS 1 9", "BUNDLE 1 2") + audio + video + "a=mid:2\r\n",
			want: []string{"1", "2"},
		},
		{
			name: "the_line_of_another_group_that_lists_no_stream",
			body: session + groups("BUNDLE 2 9", "BUNDLE 1") + audio + video + "a=mid:2\r\n",
			want: []string{"1"},
		},
		{
			name: "every_line_listing_a_tag_of_no_stream",
			body: session + groups("BUNDLE 1 8", "BUNDLE 1 9") + audio,
		},
		{
			name: "a_tag_listed_twice",
			body: session + groups("BUNDLE 1 1") + audio,
			want: []string{"1", "1"},
		},
		{
			name: "two_BUNDLE_groups_that_list_the_audio_stream",
			body: session + groups("BUNDLE 1 2", "BUNDLE 1 3") + audio + video + "a=mid:2\r\n" + video + "a=mid:3\r\n",
			want: []string{"1", "2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc, unread, err := unmarshalWebRTCDescription([]byte(tc.body))
			require.NoError(t, err)
			audio := webRTCAudioStream(&desc)
			require.NotNil(t, audio)

			assert.Equal(t, tc.want, webRTCBundleGroup(&desc, unread, "1"), "the group of the audio stream")
			streams := webRTCOfferedStreams(&desc, unread, audio)
			assert.Equal(t, tc.want != nil, streams.bundle.grouped, "the audio stream is listed in a group")
			var wantLines []string
			if tc.want != nil {
				wantLines = []string{"a=group:BUNDLE 1"}
			}
			assert.Equal(t, wantLines, streams.bundleGroupLines(), "the a=group lines of our answer")
			assert.Equal(t, wantLines, streams.inOwnOffer().bundleGroupLines(), "the a=group lines of our offer afterwards")
		})
	}
}

// webrtcBundledOffer returns alone, an offer of ours with its audio stream
// alone, as an endpoint that bundles from the start sends it: a video section
// first, the audio section behind it, and the candidates of the one transport
// listed in the video section alone. group is the a=group line, and
// credentials the ICE credential lines the video section has before its
// candidates.
func webrtcBundledOffer(t *testing.T, alone []byte, group string, credentials []string) []byte {
	t.Helper()
	candidates := sdpLines(alone, "a=candidate:")
	require.NotEmpty(t, candidates, "the offer\n%s", alone)
	audio := sdpWithoutAttribute(alone, "candidate")
	video := webrtcTaggedSection("video", "0", slices.Concat(credentials, candidates)...)
	return webrtcTaggedOffer(t, audio, group, "1", []string{video}, nil)
}

// iceCredentialLines returns the a=ice-ufrag and a=ice-pwd lines of body.
func iceCredentialLines(t *testing.T, body []byte) []string {
	t.Helper()
	return []string{sdpLine(t, body, "a=ice-ufrag:"), sdpLine(t, body, "a=ice-pwd:")}
}

// candidateLinesRead returns the a=candidate lines RemoteSDP reads the remote
// candidates of the audio stream of body from.
func candidateLinesRead(t *testing.T, body []byte) []string {
	t.Helper()
	desc, unread, err := unmarshalWebRTCDescription(body)
	require.NoError(t, err)
	audio := webRTCAudioStream(&desc)
	require.NotNil(t, audio, "the SDP has no audio stream\n%s", body)
	var lines []string
	for _, attr := range webRTCCandidateAttributes(&desc, unread, audio) {
		if attr.Key == "candidate" {
			lines = append(lines, "a=candidate:"+attr.Value)
		}
	}
	return lines
}

// TestMediaSessionWebrtcTakesTheCandidatesOfItsBundle pins where the remote
// candidates of the audio stream are read: in its own section, and for an
// audio section without one that is in a BUNDLE group, in the sections of that
// group that carry its ICE credentials. An endpoint that bundles from the
// start gathers for one transport and lists its candidates in the first
// bundled section, which is not the audio one when a video stream is offered
// ahead of it. A section of another group, or with other credentials, is
// another transport and gives none: the offer is then refused for having no
// candidate, as an offer without any is. So is the offer whose group stands
// beside a stream without an a=mid line, which groups nothing, and the one
// whose group lists a tag of no m= line, which is ignored (RFC 5888 section
// 6).
func TestMediaSessionWebrtcTakesTheCandidatesOfItsBundle(t *testing.T) {
	t.Run("listed_in_the_first_bundled_section", func(t *testing.T) {
		offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, func(offer string) string {
			return string(webrtcBundledOffer(t, []byte(offer), "a=group:BUNDLE 0 1", iceCredentialLines(t, []byte(offer))))
		})
		require.Empty(t, sdpLines([]byte(strings.SplitN(string(offer), "m=audio", 2)[1]), "a=candidate:"),
			"the audio section of the offer lists no candidate\n%s", offer)
		assert.Equal(t, sdpLines(offer, "a=candidate:"), candidateLinesRead(t, offer), "the candidates read from the offer\n%s", offer)
		assert.Equal(t, []string{"a=group:BUNDLE 1"}, sdpLines(answer, "a=group:"), "the answer\n%s", answer)
		assert.Equal(t, []string{"0", "1"}, streamTagsOf(answer), "the answer\n%s", answer)
		requireWebrtcSRTPBetween(t, offerer, answerer, 1)
		requireWebrtcSRTPBetween(t, answerer, offerer, 2)
	})

	newPair := func(t *testing.T) (alone []byte, answerer *MediaSessionWebrtc) {
		t.Helper()
		ctx := t.Context()
		offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		answerer = newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
		alone, err := offerer.LocalSDP(ctx, false)
		require.NoError(t, err)
		return alone, answerer
	}

	t.Run("under_session-level_credentials", func(t *testing.T) {
		alone, answerer := newPair(t)
		// The credentials stand at the session level and no section repeats
		// them.
		credentials := strings.Join(iceCredentialLines(t, alone), "\r\n")
		offer := webrtcBundledOffer(t, sdpWithoutAttribute(sdpWithoutAttribute(alone, "ice-ufrag"), "ice-pwd"), "a=group:BUNDLE 0 1", nil)
		offer = []byte(strings.Replace(string(offer), "t=0 0\r\n", "t=0 0\r\n"+credentials+"\r\n", 1))

		assert.Equal(t, sdpLines(alone, "a=candidate:"), candidateLinesRead(t, offer), "the candidates read from the offer\n%s", offer)
		require.NoError(t, answerer.RemoteSDP(t.Context(), offer, false), "the offer\n%s", offer)
	})

	t.Run("of_a_group_that_lists_an_identified_T.38_stream", func(t *testing.T) {
		alone, answerer := newPair(t)
		// The tag of the stream the SDP parser does not read is that of an
		// m= line of the offer, so the group that lists it stands.
		offer := webrtcBundledOffer(t, alone, "a=group:BUNDLE 0 1 fax", iceCredentialLines(t, alone))
		offer = slices.Concat(offer, []byte("m=image 30004 udptl t38\r\na=mid:fax\r\n"))

		assert.Equal(t, sdpLines(alone, "a=candidate:"), candidateLinesRead(t, offer), "the candidates read from the offer\n%s", offer)
		require.NoError(t, answerer.RemoteSDP(t.Context(), offer, false), "the offer\n%s", offer)
		answer, err := answerer.LocalSDP(t.Context(), true)
		require.NoError(t, err)
		assert.Equal(t, []string{"a=group:BUNDLE 1"}, sdpLines(answer, "a=group:"), "the answer\n%s", answer)
		assert.Equal(t, []string{"0", "1", "fax"}, streamTagsOf(answer), "the answer\n%s", answer)
	})

	t.Run("its_own_candidates_alone_when_it_has_some", func(t *testing.T) {
		alone, answerer := newPair(t)
		video := webrtcTaggedSection("video", "0", slices.Concat(iceCredentialLines(t, alone),
			[]string{"a=candidate:1 1 udp 2130706431 192.0.2.99 40000 typ host"})...)
		offer := webrtcTaggedOffer(t, alone, "a=group:BUNDLE 0 1", "1", []string{video}, nil)

		assert.Equal(t, sdpLines(alone, "a=candidate:"), candidateLinesRead(t, offer), "the candidates read from the offer\n%s", offer)
		require.NoError(t, answerer.RemoteSDP(t.Context(), offer, false), "the offer\n%s", offer)
	})

	for _, tc := range []struct {
		name  string
		group string
		// credentials, when set, are the ICE credentials of the video
		// section, which otherwise repeats those of the audio section.
		credentials []string
		// behind, when set, is a further media section of the offer,
		// behind the audio one.
		behind string
	}{
		{
			name:        "not_of_a_section_with_other_credentials",
			group:       "a=group:BUNDLE 0 1",
			credentials: []string{"a=ice-ufrag:other", "a=ice-pwd:otherotherotherotherother"},
		},
		{
			name:  "not_of_a_section_outside_the_group",
			group: "a=group:BUNDLE 1",
		},
		{
			name:  "not_of_a_section_of_another_group",
			group: "a=group:BUNDLE 0\r\na=group:BUNDLE 1",
		},
		{
			name: "not_without_a_group",
		},
		{
			// RFC 5888 section 6: a description that leaves a stream
			// without an a=mid line groups none, so the group that lists
			// both sections is not one to take candidates from.
			name:   "not_of_a_group_beside_a_stream_without_a_tag",
			group:  "a=group:BUNDLE 0 1",
			behind: webrtcTaggedSection("text", ""),
		},
		{
			name:   "not_of_a_group_beside_a_T.38_stream_without_a_tag",
			group:  "a=group:BUNDLE 0 1",
			behind: "m=image 30004 udptl t38",
		},
		{
			// RFC 5888 section 6: an a=group line that lists a tag of no
			// m= line is ignored, so the two sections it also lists are
			// not bundled.
			name:  "not_of_a_group_that_lists_a_tag_of_no_stream",
			group: "a=group:BUNDLE 0 1 2",
		},
		{
			name:   "not_of_a_group_that_lists_a_tag_of_no_stream_beside_a_T.38_stream",
			group:  "a=group:BUNDLE 0 1 2",
			behind: "m=image 30004 udptl t38\r\na=mid:fax",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alone, answerer := newPair(t)
			credentials := tc.credentials
			if len(credentials) == 0 {
				credentials = iceCredentialLines(t, alone)
			}
			offer := webrtcBundledOffer(t, alone, tc.group, credentials)
			if tc.behind != "" {
				offer = slices.Concat(offer, []byte(tc.behind+"\r\n"))
			}

			assert.Empty(t, candidateLinesRead(t, offer), "the candidates read from the offer\n%s", offer)
			err := answerer.RemoteSDP(t.Context(), offer, false)
			require.ErrorIs(t, err, ErrICEConnection, "the offer\n%s", offer)
			_, err = answerer.LocalSDP(t.Context(), true)
			require.Error(t, err, "an answer to the refused offer")
		})
	}
}

// TestMediaSessionWebrtcCarriesACallWhoseGroupListsNoStream negotiates two
// sessions on an offer whose BUNDLE group lists the audio stream beside a tag
// of no m= line, as an offer of two bundled streams arrives when the section
// of the second was taken out of it on the way. RFC 5888 section 6 has that
// a=group line ignored, so the audio stream is offered in no group: the answer
// keeps its tag, has no a=group line and does not repeat the mapping of the
// MID header extension, which is enabled in a bundled section alone (RFC 8843
// section 9.1). The call is carried on the audio stream's own candidates, its
// packets are sent as they are, and an offer of ours afterwards describes the
// stream the same way.
func TestMediaSessionWebrtcCarriesACallWhoseGroupListsNoStream(t *testing.T) {
	offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, func(offer string) string {
		return string(webrtcTaggedOffer(t, []byte(offer), "a=group:BUNDLE audio video", "audio", nil, nil))
	})
	require.Equal(t, []string{"a=extmap:1 " + testMIDExtmap}, sdpLines(offer, "a=extmap:"), "the offer maps the extension\n%s", offer)
	assert.Empty(t, sdpLines(answer, "a=group:"), "the answer\n%s", answer)
	assert.Equal(t, []string{"audio"}, streamTagsOf(answer), "the answer\n%s", answer)
	assert.Empty(t, sdpLines(answer, "a=extmap:"), "the answer\n%s", answer)

	read := func(t *testing.T, from, to *MediaSessionWebrtc, seq uint16) {
		t.Helper()
		require.NoError(t, from.WriteRTP(dtlsTestPacket(seq)))
		require.NoError(t, to.StopRTP(1, 5*time.Second))
		got := rtp.Packet{}
		_, err := to.ReadRTP(make([]byte, RTPBufSize), &got)
		require.NoError(t, err, "the media did not arrive and decrypt")
		require.NoError(t, to.StartRTP(1))
		require.Equal(t, seq, got.SequenceNumber)
		assert.False(t, got.Header.Extension, "a stream in no group is sent without its tag")
	}
	read(t, answerer, offerer, 1)
	read(t, offerer, answerer, 2)

	reoffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	assert.Empty(t, sdpLines(reoffer, "a=group:"), "our re-offer\n%s", reoffer)
	assert.Equal(t, []string{"audio"}, streamTagsOf(reoffer), "our re-offer\n%s", reoffer)
	assert.Empty(t, sdpLines(reoffer, "a=extmap:"), "our re-offer\n%s", reoffer)
	reanswer := exchangeWebrtcReOffer(t, answerer, offerer, reoffer)
	assert.Empty(t, sdpLines(reanswer, "a=group:"), "the answer to our re-offer\n%s", reanswer)
	assert.Equal(t, []string{"audio"}, streamTagsOf(reanswer), "the answer to our re-offer\n%s", reanswer)
	read(t, answerer, offerer, 3)
	read(t, offerer, answerer, 4)
}

// newPionPeer returns a Pion PeerConnection on the loopback interface, as a
// browser stack offering to a session: with the audio codec of the call, a
// video codec, and the header extensions given for its audio streams. It is
// closed when the test ends.
func newPionPeer(t *testing.T, audioExtensions ...string) *webrtc.PeerConnection {
	t.Helper()
	mediaEngine := &webrtc.MediaEngine{}
	require.NoError(t, mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1},
		PayloadType:        0,
	}, webrtc.RTPCodecTypeAudio))
	require.NoError(t, mediaEngine.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000},
		PayloadType:        96,
	}, webrtc.RTPCodecTypeVideo))
	for _, uri := range audioExtensions {
		require.NoError(t, mediaEngine.RegisterHeaderExtension(webrtc.RTPHeaderExtensionCapability{URI: uri}, webrtc.RTPCodecTypeAudio))
	}
	settingEngine := webrtc.SettingEngine{}
	settingEngine.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})
	settingEngine.SetIncludeLoopbackCandidate(true)
	settingEngine.SetInterfaceFilter(testLoopbackInterface)
	peer, err := webrtc.NewAPI(webrtc.WithMediaEngine(mediaEngine), webrtc.WithSettingEngine(settingEngine)).
		NewPeerConnection(webrtc.Configuration{})
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := peer.Close(); err != nil {
			t.Errorf("closing the Pion peer: %v", err)
		}
	})
	return peer
}

// pionOffer returns the offer of peer with its candidates gathered.
func pionOffer(ctx context.Context, t *testing.T, peer *webrtc.PeerConnection) []byte {
	t.Helper()
	offer, err := peer.CreateOffer(nil)
	require.NoError(t, err)
	gathered := webrtc.GatheringCompletePromise(peer)
	require.NoError(t, peer.SetLocalDescription(offer))
	select {
	case <-ctx.Done():
		t.Fatal(context.Cause(ctx))
	case <-gathered:
	}
	return []byte(peer.LocalDescription().SDP)
}

// newPionAnswerer returns the session that answers a Pion peer, on the
// loopback interface. It is closed when the test ends.
func newPionAnswerer(ctx context.Context, t *testing.T) *MediaSessionWebrtc {
	t.Helper()
	session := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}, Mode: "sendrecv"}
	require.NoError(t, session.Init(ctx, MediaSessionWebrtcConfig{
		NetworkTypes:    []ice.NetworkType{ice.NetworkTypeUDP4},
		IncludeLoopback: true,
		InterfaceFilter: testLoopbackInterface,
		DTLS:            DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
	}))
	closeWebrtcWithTest(t, session)
	return session
}

// TestMediaSessionWebrtcAnswersAPionOfferOfVideoThenAudio applies the offer a
// Pion PeerConnection makes of a video stream ahead of an audio one. Pion
// bundles both from the start: it lists its candidates in the first section
// alone, the video one, and gives that section the group's first tag. The
// session answers on those candidates, with the audio stream under the tag
// Pion gave it and alone in the group.
func TestMediaSessionWebrtcAnswersAPionOfferOfVideoThenAudio(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	peer := newPionPeer(t)
	_, err := peer.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo)
	require.NoError(t, err)
	_, err = peer.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio)
	require.NoError(t, err)
	offer := pionOffer(ctx, t, peer)

	media := mediaLines(offer)
	require.Len(t, media, 2, "the offer\n%s", offer)
	require.True(t, strings.HasPrefix(media[0], "m=video "), "the offer\n%s", offer)
	require.True(t, strings.HasPrefix(media[1], "m=audio "), "the offer\n%s", offer)
	tags := streamTagsOf(offer)
	require.Equal(t, []string{"a=group:BUNDLE " + tags[0] + " " + tags[1]}, sdpLines(offer, "a=group:"), "the offer\n%s", offer)
	require.Empty(t, sdpLines([]byte(strings.SplitN(string(offer), "m=audio", 2)[1]), "a=candidate:"),
		"the audio section of the offer lists no candidate\n%s", offer)

	session := newPionAnswerer(ctx, t)
	require.NoError(t, session.RemoteSDP(ctx, offer, false), "the offer\n%s", offer)
	assert.Equal(t, sdpLines(offer, "a=candidate:"), candidateLinesRead(t, offer), "the candidates read from the offer")
	answer, err := session.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, []string{"a=group:BUNDLE " + tags[1]}, sdpLines(answer, "a=group:"), "the answer\n%s", answer)
	assert.Equal(t, tags, streamTagsOf(answer), "the answer\n%s", answer)
	answered := mediaLines(answer)
	require.Len(t, answered, 2, "the answer\n%s", answer)
	assert.True(t, strings.HasPrefix(answered[0], "m=video 0 "), "the answer\n%s", answer)
}

// FuzzWebRTCStreamIdentification reads the identification of the streams of an
// arbitrary SDP as RemoteSDP does and writes the lines our SDPs would carry
// for it. Whatever the body, the stream list has one stream for each m= line
// read, the sections its SDP parser was not given included, the group of our
// SDP lists the audio stream's tag alone, no line of ours carries a line
// break, and a packet takes the tag of a mapped extension and gives it back.
func FuzzWebRTCStreamIdentification(f *testing.F) {
	const session = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"
	const audio = "m=audio 9 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\na=ice-ufrag:u\r\na=ice-pwd:p\r\n"
	f.Add([]byte(session + audio))
	f.Add([]byte(session + "a=group:BUNDLE 0 1\r\n" +
		"m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:0\r\na=ice-ufrag:u\r\na=ice-pwd:p\r\na=candidate:1 1 udp 2130706431 127.0.0.1 40000 typ host\r\n" +
		audio + "a=mid:1\r\na=extmap:3 urn:ietf:params:rtp-hdrext:sdes:mid\r\n"))
	f.Add([]byte(session + "a=extmap:300/recvonly urn:ietf:params:rtp-hdrext:sdes:mid\r\na=group:BUNDLE\r\na=group:LS a\r\n" +
		audio + "a=mid:a\r\na=extmap:0 urn:ietf:params:rtp-hdrext:sdes:mid\r\nm=text 0 RTP/AVP 98\r\na=bundle-only\r\n"))
	f.Add([]byte(session + "a=group:BUNDLE " + strings.Repeat("t", 300) + "\r\n" + audio + "a=mid:" + strings.Repeat("t", 300) + "\r\na=extmap:15 urn:ietf:params:rtp-hdrext:sdes:mid\r\n"))
	f.Add([]byte(session + "m=image 30004 udptl t38\r\na=mid:fax\r\na=T38FaxVersion:0\r\n" +
		audio + "a=mid:1\r\na=ssrc:1234 cname:peer\r\n" +
		"m=video 9 RTP/AVP/FOO 96 97\r\na=rtpmap:96 VP8/90000\r\na=rtpmap:97 bad name/1\r\n"))
	f.Add([]byte(session + audio + "m=video 9 UDP/TLS/RTP/SAVPF 96 0\r\na=rtpmap:96 VP8/90000/\r\na=rtpmap:96 H264/90000\r\na=rtpmap:98 VP9/90000\r\n"))
	f.Add([]byte(session + "a=group:BUNDLE 1 2\r\n" + audio + "a=mid:1\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:2\r\nm=image 30004 udptl t38\r\n"))
	f.Add([]byte(session + "a=group:BUNDLE 1 9\r\na=group:BUNDLE 1 fax\r\n" + audio + "a=mid:1\r\nm=image 30004 udptl t38\r\na=mid:fax\r\n"))

	f.Fuzz(func(t *testing.T, body []byte) {
		desc, unread, err := unmarshalWebRTCDescription(body)
		if err != nil {
			return
		}
		md := webRTCAudioStream(&desc)
		if md == nil {
			return
		}
		streams := webRTCOfferedStreams(&desc, unread, md)
		webRTCCandidateAttributes(&desc, unread, md)
		require.Len(t, streams.media, len(desc.MediaDescriptions)+len(unread), "one stream for each m= line")
		require.Len(t, streams.formatLines, len(streams.media))
		require.Equal(t, webRTCMediaLine(md), streams.media[streams.audio], "the stream that carries the call")
		for _, stream := range unread {
			require.Equal(t, stream.media, streams.media[stream.index], "an unread stream stands where the body has it")
		}

		group := streams.bundleGroupLines()
		if len(group) > 0 {
			require.NotEmpty(t, streams.audioTag(), "a group is written for an audio stream the offer identified alone")
			require.Equal(t, []string{"a=group:BUNDLE " + streams.audioTag()}, group)
			require.Len(t, streams.tags, len(streams.media))
			require.NotContains(t, streams.tags, "", "a group is written for a body that identifies every stream alone")
			listed := webRTCBundleGroup(&desc, unread, streams.audioTag())
			require.Contains(t, listed, streams.audioTag(), "a group is written for an audio stream a group of the body lists alone")
			require.Subset(t, streams.tags, listed, "a group is written for a group of the body that lists its streams alone")
		}
		if streams.audioTag() == "" {
			require.Empty(t, streams.bundledAudioLines(), "an audio stream the offer gave no tag is written with none")
		}
		before, after := streams.rejected()
		for _, l := range slices.Concat(group, streams.bundledAudioLines(), before, after) {
			require.NotContains(t, l, "\n")
			require.NotContains(t, l, "\r")
		}

		// The extension an offer of ours maps: for a bundled stream alone,
		// under the identifier the body gave it or one it mapped to nothing.
		own := streams.inOwnOffer().bundle.mid
		if own.id != 0 {
			require.True(t, streams.bundle.grouped, "an offer of ours maps the extension for a bundled stream alone")
			require.Equal(t, streams.audioTag(), string(own.tag))
			require.NotEqual(t, uint8(reservedOneByteExtensionID), own.id)
			if streams.extensions.mid != 0 {
				require.Equal(t, streams.extensions.mid, own.id, "the identifier the body gave the extension")
			} else {
				require.NotContains(t, streams.extensions.ids, own.id, "an identifier the body mapped to nothing")
			}
			for _, l := range streams.inOwnOffer().bundledAudioLines() {
				require.NotContains(t, l, "\n")
				require.NotContains(t, l, "\r")
			}
		}

		mid := streams.bundle.mid
		if mid.id == 0 {
			return
		}
		require.True(t, streams.bundle.grouped, "an extension is in use for a bundled stream alone")
		require.Equal(t, mid, own, "an offer of ours repeats the mapping of the body")
		require.Equal(t, mid, mid.keptBy(streams.extensions), "the body read as an answer holds its own mapping")
		wire, err := mid.tagged(dtlsTestPacket(1)).Marshal()
		require.NoError(t, err)
		var got rtp.Packet
		require.NoError(t, got.Unmarshal(wire))
		require.Equal(t, streams.audioTag(), string(got.GetExtension(mid.id)))
	})
}
