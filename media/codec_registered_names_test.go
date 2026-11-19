// SPDX-License-Identifier: MPL-2.0

package media

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ExampleCodecsWithRegisteredNames() {
	codecs := CodecsWithRegisteredNames(CodecAudioUlaw, CodecAudioAlaw, CodecAudioG722, CodecAudioOpus, CodecTelephoneEvent8000)
	for _, codec := range codecs {
		fmt.Println(codec.Name)
	}
	// Output:
	// PCMU
	// PCMA
	// G722
	// opus
	// telephone-event
}

// rtpmapLines returns the a=rtpmap lines of body in the order it carries them.
func rtpmapLines(body []byte) []string {
	var lines []string
	for line := range strings.Lines(string(body)) {
		if strings.HasPrefix(line, "a=rtpmap:") {
			lines = append(lines, strings.TrimRight(line, "\r\n"))
		}
	}
	return lines
}

func TestCodecsWithRegisteredNames(t *testing.T) {
	// A codec that negotiation renumbered and that a peer described keeps its
	// number and its parameters: only the name is ours to spell.
	opus := CodecAudioOpus
	opus.Name = "OPUS"
	opus.PayloadType = 111
	opus.FMTP = "minptime=10;useinbandfec=1"
	telephoneEvent := CodecTelephoneEvent8000
	telephoneEvent.Name = "TELEPHONE-EVENT"
	speex := Codec{Name: "Speex", PayloadType: 97, SampleRate: 16000, NumChannels: 1}

	configured := []Codec{CodecAudioUlaw, CodecAudioAlaw, CodecAudioG722, CodecAudioG729, opus, telephoneEvent, speex}
	before := slices.Clone(configured)

	want := slices.Clone(configured)
	want[0].Name = "PCMU"
	want[1].Name = "PCMA"
	want[2].Name = "G722"
	want[4].Name = "opus"
	want[5].Name = "telephone-event"

	assert.Equal(t, want, CodecsWithRegisteredNames(configured...))
	assert.Equal(t, before, configured, "the caller's list must be left as it is")

	// A nil list means "not set" to MediaConfig and AnswerOptions, which then
	// keep their own codecs, so it has to come back nil.
	assert.Nil(t, CodecsWithRegisteredNames())
}

// The names in an offer and in an answer are those of the codecs the session
// is configured with, whatever spelling the peer used and also when the peer
// described a static format with no a=rtpmap line at all. The built-in codecs
// give the lower case names, CodecsWithRegisteredNames the registered ones.
func TestSDPEncodingNamesFollowConfiguredCodecs(t *testing.T) {
	builtIn := []Codec{CodecAudioUlaw, CodecAudioAlaw, CodecAudioG722, CodecTelephoneEvent8000}

	for _, tc := range []struct {
		name   string
		codecs []Codec
		want   []string
	}{
		{
			name:   "built_in",
			codecs: builtIn,
			want: []string{
				"a=rtpmap:0 pcmu/8000",
				"a=rtpmap:8 pcma/8000",
				"a=rtpmap:9 g722/8000",
				"a=rtpmap:101 telephone-event/8000",
			},
		},
		{
			name:   "registered",
			codecs: CodecsWithRegisteredNames(builtIn...),
			want: []string{
				"a=rtpmap:0 PCMU/8000",
				"a=rtpmap:8 PCMA/8000",
				"a=rtpmap:9 G722/8000",
				"a=rtpmap:101 telephone-event/8000",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("offer", func(t *testing.T) {
				m := negotiateSession(tc.codecs...)
				m.Mode = sdp.ModeSendrecv

				assert.Equal(t, tc.want, rtpmapLines(m.LocalSDP(false)))
			})

			for _, peer := range []struct {
				name    string
				rtpmaps []string
			}{
				{"answer_to_lower_case", []string{"a=rtpmap:0 pcmu/8000", "a=rtpmap:8 pcma/8000", "a=rtpmap:9 g722/8000"}},
				{"answer_to_registered", []string{"a=rtpmap:0 PCMU/8000", "a=rtpmap:8 PCMA/8000", "a=rtpmap:9 G722/8000"}},
				{"answer_to_no_rtpmap", nil},
			} {
				t.Run(peer.name, func(t *testing.T) {
					m := negotiateSession(tc.codecs...)
					m.Mode = sdp.ModeSendrecv

					offer := append([]string{
						"v=0",
						"o=- 1 1 IN IP4 198.51.100.7",
						"s=-",
						"c=IN IP4 198.51.100.7",
						"t=0 0",
						"m=audio 5004 RTP/AVP 0 8 9",
					}, peer.rtpmaps...)
					require.NoError(t, m.RemoteSDP(negotiateBody(offer...), false))

					// The peer offered no telephone-event, so the answer has none.
					assert.Equal(t, tc.want[:3], rtpmapLines(m.LocalSDP(true)))
				})
			}
		})
	}
}

// A WebRTC answer is written from the codecs its negotiation settles on, so
// these have to keep the configured names as well.
func TestCommonWebRTCCodecsKeepConfiguredNames(t *testing.T) {
	local := CodecsWithRegisteredNames(CodecAudioUlaw, CodecAudioAlaw)
	// What a peer's "m=audio ... 8 0" reads as: the built-in codecs.
	remote := []Codec{CodecAudioAlaw, CodecAudioUlaw}

	assert.Equal(t, []Codec{local[1], local[0]}, commonWebRTCCodecs(remote, local))
}
