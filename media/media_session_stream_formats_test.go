// SPDX-License-Identifier: MPL-2.0

package media

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestOfferedFormatLines pins which a=rtpmap lines of an offered section a
// rejected stream keeps: those that follow the grammar of RFC 8866 section 6.6
// and map a format of the m= line, one for each format. A value is written
// back into our own SDP, so one outside the grammar is left out.
func TestOfferedFormatLines(t *testing.T) {
	for _, tc := range []struct {
		name    string
		formats []string
		rtpmaps []string
		want    []string
	}{
		{
			name:    "every_format_mapped",
			formats: []string{"96", "97"},
			rtpmaps: []string{"96 VP8/90000", "97 H264/90000"},
			want:    []string{"a=rtpmap:96 VP8/90000", "a=rtpmap:97 H264/90000"},
		},
		{
			name:    "in_the_order_of_the_offer",
			formats: []string{"96", "97"},
			rtpmaps: []string{"97 H264/90000", "96 VP8/90000"},
			want:    []string{"a=rtpmap:97 H264/90000", "a=rtpmap:96 VP8/90000"},
		},
		{
			name:    "an_encoding_with_parameters",
			formats: []string{"111"},
			rtpmaps: []string{"111 opus/48000/2"},
			want:    []string{"a=rtpmap:111 opus/48000/2"},
		},
		{
			name:    "a_static_payload_type_without_a_line",
			formats: []string{"0", "96"},
			rtpmaps: []string{"96 VP8/90000"},
			want:    []string{"a=rtpmap:96 VP8/90000"},
		},
		{
			name:    "a_payload_type_the_m=_line_does_not_list",
			formats: []string{"96"},
			rtpmaps: []string{"96 VP8/90000", "98 VP9/90000"},
			want:    []string{"a=rtpmap:96 VP8/90000"},
		},
		{
			name:    "a_payload_type_mapped_twice",
			formats: []string{"96"},
			rtpmaps: []string{"96 VP8/90000", "96 H264/90000"},
			want:    []string{"a=rtpmap:96 VP8/90000"},
		},
		{
			name:    "a_format_that_is_no_payload_type",
			formats: []string{"webrtc-datachannel"},
			rtpmaps: []string{"webrtc-datachannel x/1"},
		},
		{
			name:    "values_outside_the_grammar",
			formats: []string{"96", "97", "98", "99", "100", "101", "102", "200"},
			rtpmaps: []string{
				"96",
				"97 VP8",
				"98 VP8/fast",
				"99 VP8/90000/2/2",
				"100 VP8/90000\ra=candidate:1",
				"101 VP 8/90000",
				"102 /90000",
				"200 VP8/90000",
				"96  VP8/90000",
				"+96 VP8/90000",
				"96 VP8/90000/",
			},
		},
		{
			name: "no_format",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, offeredFormatLines(tc.formats, tc.rtpmaps))
		})
	}
}

// rejectedSections returns the lines of each rejected section of an SDP body
// in order: an m= line on port 0 and the lines up to the next m= line.
func rejectedSections(body []byte) [][]string {
	var sections [][]string
	rejected := false
	for _, l := range streamsSDPLines(body) {
		if strings.HasPrefix(l, "m=") {
			fields := strings.Fields(l)
			rejected = len(fields) > 1 && fields[1] == "0"
			if rejected {
				sections = append(sections, nil)
			}
		}
		if rejected {
			sections[len(sections)-1] = append(sections[len(sections)-1], l)
		}
	}
	return sections
}

// TestMediaSessionWebrtcRejectedStreamKeepsItsRTPMaps pins the lines of a
// stream a WebRTC session rejects: its m= line on port 0, its a=mid line and
// the a=rtpmap lines the offer gave the formats of its m= line, as the
// disabled section of the examples of RFC 8843 section 18.5 has them, and no
// other line of the offered section. RFC 3264 section 6 has the formats of a
// rejected stream ignored, and an endpoint that looks up every payload type it
// is given needs their a=rtpmap lines to read the answer at all. Our later
// SDPs keep the section as the answer had it (RFC 3264 section 8).
func TestMediaSessionWebrtcRejectedStreamKeepsItsRTPMaps(t *testing.T) {
	video := "m=video 9 UDP/TLS/RTP/SAVPF 96 97 98\r\n" +
		"c=IN IP4 0.0.0.0\r\n" +
		"a=mid:1\r\n" +
		"a=rtcp-mux\r\n" +
		"a=sendrecv\r\n" +
		"a=rtpmap:96 VP8/90000\r\n" +
		"a=rtcp-fb:96 nack pli\r\n" +
		"a=rtpmap:97 rtx/90000\r\n" +
		"a=fmtp:97 apt=96\r\n" +
		"a=rtpmap:98 H264/90000\r\n" +
		"a=rtpmap:99 VP9/90000\r\n" +
		"a=ssrc:1234 cname:video"
	wantVideo := []string{
		"m=video 0 UDP/TLS/RTP/SAVPF 96 97 98",
		"a=mid:1",
		"a=rtpmap:96 VP8/90000",
		"a=rtpmap:97 rtx/90000",
		"a=rtpmap:98 H264/90000",
	}
	untagged := "m=video 9 UDP/TLS/RTP/SAVPF 100\r\na=rtpmap:100 AV1/90000"
	wantUntagged := []string{"m=video 0 UDP/TLS/RTP/SAVPF 100", "a=rtpmap:100 AV1/90000"}
	wantData := []string{"m=application 0 UDP/DTLS/SCTP webrtc-datachannel", "a=mid:2"}
	want := [][]string{wantVideo, wantUntagged, wantData}

	ctx := t.Context()
	offerer, answerer, _, answer := newEstablishedWebrtcPair(t, func(offer string) string {
		return string(withStreamsAround(t, []byte(offer), []string{video}, []string{untagged, webrtcStreamsData}))
	})
	assert.Equal(t, want, rejectedSections(answer), "the answer\n%s", answer)

	reoffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	assert.Equal(t, want, rejectedSections(reoffer), "our offer on the session\n%s", reoffer)
	exchangeWebrtcReOffer(t, answerer, offerer, reoffer)

	fork, err := answerer.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, fork)
	forkOffer, err := fork.LocalSDP(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, want, rejectedSections(forkOffer), "an offer of its fork\n%s", forkOffer)

	// The streams of a MediaSession are rejected with the m= line and the tag
	// alone.
	plain := newStreamsSession()
	require.NoError(t, plain.RemoteSDP([]byte("v=0\r\no=- 1 1 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n"+
		"m=audio 4000 RTP/AVP 0\r\n"+video+"\r\n"), false))
	assert.Equal(t, [][]string{{"m=video 0 UDP/TLS/RTP/SAVPF 96 97 98", "a=mid:1"}}, rejectedSections(plain.LocalSDP(true)))
}
