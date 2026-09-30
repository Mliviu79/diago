// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"strings"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// streamsOfferSession is the session level of every offer below. Its o= line
// fixes the session id and version our answer derives its own from, so an
// answer is the same bytes on every run.
var streamsOfferSession = []string{
	"v=0",
	"o=- 100 100 IN IP4 192.0.2.10",
	"s=-",
	"c=IN IP4 192.0.2.10",
	"t=0 0",
}

const (
	streamsAudio = "m=audio 30000 RTP/AVP 8 0 101\r\n" +
		"a=rtpmap:8 PCMA/8000\r\n" +
		"a=rtpmap:0 PCMU/8000\r\n" +
		"a=rtpmap:101 telephone-event/8000\r\n" +
		"a=fmtp:101 0-16\r\n" +
		"a=sendrecv"
	streamsVideo = "m=video 30002 RTP/AVP 96 97\r\n" +
		"a=rtpmap:96 H264/90000\r\n" +
		"a=rtpmap:97 VP8/90000\r\n" +
		"a=sendrecv"
	// streamsAnsweredAudio is the m= line answering streamsAudio.
	streamsAnsweredAudio = "m=audio 1234 RTP/AVP 8 0 101"
)

// streamsOffer builds an offer from its media sections, in order, behind
// streamsOfferSession, CRLF terminated as a peer sends it.
func streamsOffer(sections ...string) []byte {
	lines := append([]string{}, streamsOfferSession...)
	lines = append(lines, sections...)
	return []byte(strings.Join(lines, "\r\n") + "\r\n")
}

// newStreamsSession is the media session answering the offers below: plain
// RTP, with the codecs a call is set up with.
func newStreamsSession() *MediaSession {
	return &MediaSession{
		Codecs: []Codec{CodecAudioAlaw, CodecAudioUlaw, CodecAudioOpus, CodecTelephoneEvent8000},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		Mode:   sdp.ModeSendrecv,
	}
}

// sdpLines returns the lines of an SDP body.
func sdpLines(body []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(body), "\r\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return lines
}

// mediaLines returns the m= lines of an SDP body, in order.
func mediaLines(body []byte) []string {
	var m []string
	for _, l := range sdpLines(body) {
		if strings.HasPrefix(l, "m=") {
			m = append(m, l)
		}
	}
	return m
}

// withoutRejectedStreams returns the lines of an SDP body without each m= line
// whose port is 0, and requires that no line follows one before the next m=
// line: a rejected stream is answered with its m= line alone.
func withoutRejectedStreams(t *testing.T, body []byte) []string {
	t.Helper()
	var kept []string
	rejected := false
	for _, l := range sdpLines(body) {
		if strings.HasPrefix(l, "m=") {
			fields := strings.Fields(l)
			rejected = len(fields) > 1 && fields[1] == "0"
			if rejected {
				continue
			}
		} else if rejected {
			require.Failf(t, "a rejected stream carries a line", "line %q follows a rejected m= line in\n%s", l, body)
		}
		kept = append(kept, l)
	}
	return kept
}

// answerTo applies offer to a new session and returns the session and its
// answer.
func answerTo(t *testing.T, offer []byte) (*MediaSession, []byte) {
	t.Helper()
	m := newStreamsSession()
	require.NoError(t, m.RemoteSDP(offer), "applying the offer\n%s", offer)
	return m, m.LocalSDP()
}

// TestMediaSessionAnswersEveryOfferedStream pins RFC 3264 section 6: "The
// answer MUST contain exactly the same number of "m=" lines as the offer", in
// its order, and "To reject an offered stream, the port number in the
// corresponding stream in the answer MUST be set to zero". The first audio
// stream we can carry carries the call and is read with the session level
// alone; every other stream is rejected. Section 8 keeps those lines in every
// later SDP of ours: "Deleted media streams from a previous SDP MUST NOT be
// removed in a new SDP".
func TestMediaSessionAnswersEveryOfferedStream(t *testing.T) {
	type row struct {
		name     string
		sections []string
		// audio is the section carrying the call: the answer to the offer
		// with every other section removed is the answer to this offer
		// without its rejected lines.
		audio      int
		wantMedia  []string
		wantRemote string
		wantCodecs []Codec
	}
	rows := []row{
		{
			name:       "audio then video",
			sections:   []string{streamsAudio, streamsVideo},
			audio:      0,
			wantMedia:  []string{streamsAnsweredAudio, "m=video 0 RTP/AVP 96 97"},
			wantRemote: "192.0.2.10:30000",
		},
		{
			name:       "video then audio",
			sections:   []string{streamsVideo, streamsAudio},
			audio:      1,
			wantMedia:  []string{"m=video 0 RTP/AVP 96 97", streamsAnsweredAudio},
			wantRemote: "192.0.2.10:30000",
		},
		{
			name:       "audio and a T.38 image stream",
			sections:   []string{streamsAudio, "m=image 30004 udptl t38\r\na=T38FaxVersion:0\r\na=T38FaxRateManagement:transferredTCF"},
			audio:      0,
			wantMedia:  []string{streamsAnsweredAudio, "m=image 0 udptl t38"},
			wantRemote: "192.0.2.10:30000",
		},
		{
			name:       "two audio streams",
			sections:   []string{streamsAudio, "m=audio 30006 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\na=sendrecv"},
			audio:      0,
			wantMedia:  []string{streamsAnsweredAudio, "m=audio 0 RTP/AVP 0"},
			wantRemote: "192.0.2.10:30000",
		},
		{
			// A payload type is scoped to its media section (RFC 4566 section
			// 6): the video's 96 is H.264 and says nothing about the audio's 96.
			name: "a video section reusing an audio payload type",
			sections: []string{
				"m=video 30002 RTP/AVP 96\r\na=rtpmap:96 H264/90000",
				"m=audio 30000 RTP/AVP 96 8\r\na=rtpmap:96 opus/48000/2\r\na=rtpmap:8 PCMA/8000",
			},
			audio:      1,
			wantMedia:  []string{"m=video 0 RTP/AVP 96", "m=audio 1234 RTP/AVP 96 8"},
			wantRemote: "192.0.2.10:30000",
			wantCodecs: []Codec{CodecAudioOpus, CodecAudioAlaw},
		},
		{
			// A c= inside a media section governs that stream (RFC 4566
			// section 5.7), and another section's c= governs nothing of ours.
			name: "a c= line in the audio section",
			sections: []string{
				"m=audio 30000 RTP/AVP 8\r\nc=IN IP4 198.51.100.7\r\na=rtpmap:8 PCMA/8000",
				"m=video 30002 RTP/AVP 96\r\nc=IN IP4 203.0.113.99\r\na=rtpmap:96 H264/90000",
			},
			audio:      0,
			wantMedia:  []string{"m=audio 1234 RTP/AVP 8", "m=video 0 RTP/AVP 96"},
			wantRemote: "198.51.100.7:30000",
		},
	}

	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			m, answer := answerTo(t, streamsOffer(r.sections...))

			assert.Equal(t, r.wantMedia, mediaLines(answer), "the answer's m= lines\n%s", answer)
			assert.Equal(t, r.wantRemote, m.Raddr.String(), "the RTP destination")
			if r.wantCodecs != nil {
				assert.Equal(t, r.wantCodecs, m.CommonCodecs(), "the audio codecs")
			}

			_, audioOnly := answerTo(t, streamsOffer(r.sections[r.audio]))
			assert.Equal(t, sdpLines(audioOnly), withoutRejectedStreams(t, answer),
				"the audio stream is answered as it is when offered alone")
		})
	}

	t.Run("video alone", func(t *testing.T) {
		m := newStreamsSession()
		err := m.RemoteSDP(streamsOffer(streamsVideo))
		require.ErrorIs(t, err, ErrNoCommonMedia)
	})

	t.Run("a truncated m= line", func(t *testing.T) {
		m := newStreamsSession()
		err := m.RemoteSDP(streamsOffer(streamsAudio, "m=video 30002 RTP/AVP"))
		require.Error(t, err)
	})

	t.Run("one audio stream", func(t *testing.T) {
		_, answer := answerTo(t, streamsOffer(streamsAudio))
		want := "v=0\r\n" +
			"o=- 100 102 IN IP4 127.0.0.1\r\n" +
			"s=Sip Go Media\r\n" +
			"c=IN IP4 127.0.0.1\r\n" +
			"t=0 0\r\n" +
			"m=audio 1234 RTP/AVP 8 0 101\r\n" +
			"a=rtpmap:8 PCMA/8000\r\n" +
			"a=rtpmap:0 PCMU/8000\r\n" +
			"a=rtpmap:101 telephone-event/8000\r\n" +
			"a=fmtp:101 0-16\r\n" +
			"a=ptime:20\r\n" +
			"a=maxptime:20\r\n" +
			"a=sendrecv\r\n"
		assert.Equal(t, want, string(answer))
	})

	t.Run("our later offer keeps every stream", func(t *testing.T) {
		m, _ := answerTo(t, streamsOffer(streamsAudio, streamsVideo))

		// As the dialog makes an offer of its own: a fork offering the codecs
		// the call runs on.
		fork := m.Fork()
		fork.Codecs = m.CommonCodecs()
		offer := fork.LocalSDP()
		assert.Equal(t, []string{streamsAnsweredAudio, "m=video 0 RTP/AVP 96 97"}, mediaLines(offer), "our offer\n%s", offer)

		// The answer to it keeps them too, and so does the offer after.
		fork.RemoteSDPIsAnswer = true
		require.NoError(t, fork.RemoteSDP([]byte(strings.Join([]string{
			"v=0", "o=- 100 101 IN IP4 192.0.2.10", "s=-", "c=IN IP4 192.0.2.10", "t=0 0",
			"m=audio 30000 RTP/AVP 8", "a=rtpmap:8 PCMA/8000", "a=sendrecv",
			"m=video 0 RTP/AVP 96 97",
		}, "\r\n")+"\r\n")))
		next := fork.Fork()
		next.Codecs = fork.CommonCodecs()
		assert.Equal(t, []string{"m=audio 1234 RTP/AVP 8", "m=video 0 RTP/AVP 96 97"}, mediaLines(next.LocalSDP()))
	})

	t.Run("a peer's re-offer adding a stream", func(t *testing.T) {
		m, _ := answerTo(t, streamsOffer(streamsAudio, streamsVideo))

		// As the dialog applies a peer's re-INVITE offer.
		fork := m.Fork()
		fork.RemoteSDPIsAnswer = false
		reoffer := strings.Join([]string{
			"v=0", "o=- 100 101 IN IP4 192.0.2.10", "s=-", "c=IN IP4 192.0.2.10", "t=0 0",
			streamsAudio,
			"m=video 0 RTP/AVP 96 97",
			"m=text 30008 RTP/AVP 98\r\na=rtpmap:98 t140/1000",
		}, "\r\n") + "\r\n"
		require.NoError(t, fork.RemoteSDP([]byte(reoffer)))

		answer := fork.LocalSDP()
		assert.Equal(t, []string{streamsAnsweredAudio, "m=video 0 RTP/AVP 96 97", "m=text 0 RTP/AVP 98"}, mediaLines(answer), "the answer\n%s", answer)
	})
}
