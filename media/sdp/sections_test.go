// SPDX-License-Identifier: MPL-2.0

package sdp

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestUnmarshalSectionsBindsLinesToTheirSection pins what a body kept by line
// type cannot give: each stream is described by the session level and its own
// section alone. A c= inside a media section governs that stream and no other
// (RFC 4566 section 5.7), a stream without one takes the session level c=, and
// an attribute of one stream is never read as another's.
func TestUnmarshalSectionsBindsLinesToTheirSection(t *testing.T) {
	sections, err := UnmarshalSections(offer(
		"v=0",
		"o=- 1 1 IN IP4 203.0.113.1",
		"s=-",
		"c=IN IP4 203.0.113.9",
		"t=0 0",
		"m=audio 5004 RTP/AVP 8",
		"c=IN IP4 198.51.100.7",
		"a=rtpmap:8 PCMA/8000",
		"m=video 5006/2 RTP/AVP 96",
		"c=IN IP4 192.0.2.44",
		"a=rtpmap:96 H264/90000",
		"m=text 5008 RTP/AVP 98",
		"a=rtpmap:98 t140/1000",
	))
	require.NoError(t, err)

	assert.Equal(t, []Line{
		{Type: "v", Value: "0"},
		{Type: "o", Value: "- 1 1 IN IP4 203.0.113.1"},
		{Type: "s", Value: "-"},
		{Type: "c", Value: "IN IP4 203.0.113.9"},
		{Type: "t", Value: "0 0"},
	}, sections.Session)

	type stream struct {
		media MediaDescription
		conn  string
		attrs []string
	}
	want := []stream{
		{
			media: MediaDescription{MediaType: "audio", Port: 5004, Proto: "RTP/AVP", Formats: []string{"8"}},
			conn:  "198.51.100.7",
			attrs: []string{"rtpmap:8 PCMA/8000"},
		},
		{
			media: MediaDescription{MediaType: "video", Port: 5006, PortNumbers: 2, Proto: "RTP/AVP", Formats: []string{"96"}},
			conn:  "192.0.2.44",
			attrs: []string{"rtpmap:96 H264/90000"},
		},
		{
			media: MediaDescription{MediaType: "text", Port: 5008, Proto: "RTP/AVP", Formats: []string{"98"}},
			conn:  "203.0.113.9",
			attrs: []string{"rtpmap:98 t140/1000"},
		},
	}

	got := make([]stream, len(sections.Media))
	for i, sec := range sections.Media {
		sd := sections.Description(i)
		ci, err := sd.ConnectionInformation()
		require.NoError(t, err, "stream %d", i)
		got[i] = stream{media: sec.Media, conn: ci.IP.String(), attrs: sd.Values("a")}

		// The description of one stream is a body of one m= line, which is
		// what the readers of a SessionDescription expect.
		md, err := sd.MediaDescription(sec.Media.MediaType)
		require.NoError(t, err, "stream %d", i)
		assert.Equal(t, sec.Media, md, "stream %d", i)
	}
	assert.Equal(t, want, got)
}

// TestUnmarshalSectionsRefusesUnreadableLine covers the two ways a line fails:
// one that is not type=value, as Unmarshal refuses it, and an m= line without
// its four fields (RFC 4566 section 5.14), which leaves a stream the answer
// could not repeat.
func TestUnmarshalSectionsRefusesUnreadableLine(t *testing.T) {
	bodies := map[string][]byte{
		"not_type_value":      offer("v=0", "nonsense"),
		"truncated_m":         offer("v=0", "m=audio 5004 RTP/AVP"),
		"second_m_truncated":  offer("v=0", "m=audio 5004 RTP/AVP 8", "m=video 5006 RTP/AVP"),
		"m_without_any_field": offer("m="),
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			_, err := UnmarshalSections(body)
			require.Error(t, err)
		})
	}
}
