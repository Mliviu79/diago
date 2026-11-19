// SPDX-License-Identifier: MPL-2.0

package media

import (
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	webrtcsdp "github.com/pion/sdp/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The parts of the bodies TestUnmarshalWebRTCDescription reads.
const (
	unreadTestSession = "v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\n"
	unreadTestAudio   = "m=audio 9 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\na=mid:0\r\n"
	unreadTestVideo   = "m=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:1\r\na=rtpmap:96 VP8/90000\r\n"
	unreadTestImage   = "m=image 30004 udptl t38\r\na=T38FaxVersion:0\r\n"
)

// TestUnmarshalWebRTCDescription pins what RemoteSDP parses of a body: the
// whole of one its SDP parser reads, and one it refuses without the sections
// whose m= line the parser does not read, which are returned as unread with
// what an answer repeats of them. A body the parser refuses for anything else
// stays refused.
func TestUnmarshalWebRTCDescription(t *testing.T) {
	image := sdp.MediaDescription{MediaType: "image", Port: 30004, Proto: "udptl", Formats: []string{"t38"}}
	for _, tc := range []struct {
		name string
		body string
		// wantMedia are the media types of the sections parsed, in order.
		wantMedia  []string
		wantUnread unreadStreams
		wantErr    bool
	}{
		{
			name:      "a_body_the_parser_reads",
			body:      unreadTestSession + unreadTestAudio + unreadTestVideo,
			wantMedia: []string{"audio", "video"},
		},
		{
			name:       "a_T.38_stream_between_two_others",
			body:       unreadTestSession + unreadTestAudio + unreadTestImage + unreadTestVideo,
			wantMedia:  []string{"audio", "video"},
			wantUnread: unreadStreams{{index: 1, media: image}},
		},
		{
			name:       "an_identified_T.38_stream_first",
			body:       unreadTestSession + unreadTestImage + "a=mid:fax\r\n" + unreadTestAudio,
			wantMedia:  []string{"audio"},
			wantUnread: unreadStreams{{index: 0, media: image, tag: "fax"}},
		},
		{
			name:      "several_streams_the_parser_does_not_read",
			body:      unreadTestSession + unreadTestImage + unreadTestAudio + "m=video 9/2 RTP/AVP/FOO 96 97\r\na=mid:odd\r\na=rtpmap:96 VP8/90000\r\n" + unreadTestImage,
			wantMedia: []string{"audio"},
			wantUnread: unreadStreams{
				{index: 0, media: image},
				{
					index:       2,
					media:       sdp.MediaDescription{MediaType: "video", Port: 9, PortNumbers: 2, Proto: "RTP/AVP/FOO", Formats: []string{"96", "97"}},
					tag:         "odd",
					formatLines: []string{"a=rtpmap:96 VP8/90000"},
				},
				{index: 3, media: image},
			},
		},
		{
			name:       "a_T.38_stream_alone",
			body:       unreadTestSession + unreadTestImage,
			wantUnread: unreadStreams{{index: 0, media: image}},
		},
		{
			name:       "a_tag_outside_the_token_grammar",
			body:       unreadTestSession + unreadTestAudio + unreadTestImage + "a=mid:a b\r\n",
			wantMedia:  []string{"audio"},
			wantUnread: unreadStreams{{index: 1, media: image}},
		},
		{
			name:    "a_body_whose_last_line_is_not_ended",
			body:    unreadTestSession + unreadTestAudio + strings.TrimSuffix(unreadTestImage, "\r\n"),
			wantErr: true,
		},
		{
			name:    "a_T.38_stream_and_a_line_the_parser_refuses",
			body:    unreadTestSession + "m=audio 9 UDP/TLS/RTP/SAVPF 0\r\nc=PSTN E164 +1\r\n" + unreadTestImage,
			wantErr: true,
		},
		{
			name:    "a_truncated_m=_line",
			body:    unreadTestSession + unreadTestAudio + "m=image 9\r\n",
			wantErr: true,
		},
		{
			name:    "a_refused_body_without_such_a_stream",
			body:    unreadTestSession + "m=audio 9 UDP/TLS/RTP/SAVPF 0\r\nc=PSTN E164 +1\r\n",
			wantErr: true,
		},
		{
			name:    "no_SDP",
			body:    "not an SDP\r\n",
			wantErr: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc, unread, err := unmarshalWebRTCDescription([]byte(tc.body))
			if tc.wantErr {
				require.Error(t, err)
				assert.Empty(t, unread)
				return
			}
			require.NoError(t, err)
			var media []string
			for _, md := range desc.MediaDescriptions {
				media = append(media, md.MediaName.Media)
			}
			assert.Equal(t, tc.wantMedia, media, "the sections parsed")
			assert.Equal(t, tc.wantUnread, unread, "the streams left unread")

			if len(tc.wantUnread) == 0 {
				var direct webrtcsdp.SessionDescription
				require.NoError(t, direct.Unmarshal([]byte(tc.body)))
				assert.Equal(t, direct, desc, "a body the parser reads is parsed as it is")
			}
		})
	}
}

// TestUnreadStreamsAmong pins where the streams the parser was not given
// stand in the stream list: each at the place the description has it, with
// the audio stream still the one the list names.
func TestUnreadStreamsAmong(t *testing.T) {
	line := func(mediaType string) sdp.MediaDescription {
		return sdp.MediaDescription{MediaType: mediaType, Proto: "x", Formats: []string{"0"}}
	}
	// list returns a stream list of the media types given, each stream with
	// its tag and the lines it keeps when it is rejected.
	list := func(audio int, tags []string, formatLines [][]string, mediaTypes ...string) offeredStreams {
		streams := offeredStreams{audio: audio, tags: tags, formatLines: formatLines}
		for _, mediaType := range mediaTypes {
			streams.media = append(streams.media, line(mediaType))
		}
		return streams
	}
	for _, tc := range []struct {
		name    string
		streams offeredStreams
		unread  unreadStreams
		want    offeredStreams
	}{
		{
			name:    "none",
			streams: list(1, []string{"v", "a"}, [][]string{{"a=rtpmap:96 x/1"}, nil}, "video", "audio"),
			want:    list(1, []string{"v", "a"}, [][]string{{"a=rtpmap:96 x/1"}, nil}, "video", "audio"),
		},
		{
			name:    "behind_the_audio_stream",
			streams: list(0, nil, [][]string{nil}, "audio"),
			unread:  unreadStreams{{index: 1, media: line("image")}},
			want:    list(0, nil, [][]string{nil, nil}, "audio", "image"),
		},
		{
			name:    "ahead_of_the_audio_stream",
			streams: list(0, []string{"a"}, [][]string{nil}, "audio"),
			unread:  unreadStreams{{index: 0, media: line("image"), tag: "fax"}},
			want:    list(1, []string{"fax", "a"}, [][]string{nil, nil}, "image", "audio"),
		},
		{
			name:    "the_only_stream_identified",
			streams: list(0, nil, [][]string{nil}, "audio"),
			unread:  unreadStreams{{index: 1, media: line("image"), tag: "fax"}},
			want:    list(0, []string{"", "fax"}, [][]string{nil, nil}, "audio", "image"),
		},
		{
			name:    "on_both_sides_and_between",
			streams: list(1, nil, [][]string{nil, nil, nil}, "video", "audio", "text"),
			unread: unreadStreams{
				{index: 0, media: line("image")},
				{index: 2, media: line("fax"), tag: "f", formatLines: []string{"a=rtpmap:96 x/1"}},
				{index: 5, media: line("last")},
			},
			want: list(3, []string{"", "", "f", "", "", ""}, [][]string{nil, nil, {"a=rtpmap:96 x/1"}, nil, nil, nil},
				"image", "video", "fax", "audio", "text", "last"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, tc.unread.among(tc.streams))
		})
	}
}

// TestMediaSessionWebrtcAnswersAnOfferAddingAT38Stream applies, on a
// negotiated session, an offer of the peer that adds a stream the SDP parser
// of the session does not read. The offer is answered with that stream
// rejected, and once the exchange is committed our own offers keep its line
// (RFC 3264 section 8).
func TestMediaSessionWebrtcAnswersAnOfferAddingAT38Stream(t *testing.T) {
	ctx := t.Context()
	offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, nil)
	audio := sdpLine(t, answer, "m=audio ")
	want := []string{audio, webrtcStreamsRejectedImage}
	reoffer := []byte(string(offer) + webrtcStreamsImage + "\r\n")

	require.NoError(t, answerer.RemoteSDP(ctx, reoffer, false), "the offer\n%s", reoffer)
	reanswer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	assert.Equal(t, want, mediaLines(reanswer), "the answer\n%s", reanswer)
	assert.Equal(t, [][]string{{webrtcStreamsRejectedImage}}, rejectedSections(reanswer), "the answer\n%s", reanswer)
	finalizeWebrtcWithin(t, answerer, 5*time.Second)

	ours, err := answerer.ReOffer("")
	require.NoError(t, err)
	assert.Equal(t, want, mediaLines(ours), "our offer after the exchange\n%s", ours)
	// The peer reads our offer, with the line its parser refuses, and answers
	// every stream of it.
	theirs := exchangeWebrtcReOffer(t, answerer, offerer, ours)
	assert.Len(t, mediaLines(theirs), 2, "the answer to our offer\n%s", theirs)
	requireWebrtcSRTPBetween(t, answerer, offerer, 20)
	requireWebrtcSRTPBetween(t, offerer, answerer, 21)
}
