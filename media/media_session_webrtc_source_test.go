// SPDX-License-Identifier: MPL-2.0

package media

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	webrtcsdp "github.com/pion/sdp/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sourceLine returns the a=ssrc line that declares source.
func sourceLine(source *webrtcSource) string {
	return "a=ssrc:" + strconv.FormatUint(uint64(source.ssrc), 10) + " cname:" + source.cname
}

// audioSectionLines returns the lines of the audio section of body that begin
// with prefix: those between its m=audio line and the next m= line.
func audioSectionLines(t *testing.T, body []byte, prefix string) []string {
	t.Helper()
	_, section, found := strings.Cut(string(body), "\r\nm=audio ")
	require.True(t, found, "the SDP has no audio section:\n%s", body)
	section, _, _ = strings.Cut(section, "\r\nm=")
	return sdpLines([]byte(section), prefix)
}

// TestMediaSessionWebrtcDeclaresItsSource pins the source an SDP of more than
// one m= section declares, and that the session sends as it.
//
// RFC 8843 section 9.2: "An offerer and answerer can inform each other which
// SSRC values they will use for an RTP stream by using the SDP 'ssrc'
// attribute [RFC5576]". RFC 5576 section 4.1 has the declaration carry the
// CNAME of the source, and section 6.1 has that be "the CNAME value that the
// media sender will place in its RTCP SDES packets". So the a=ssrc line of the
// audio section names the SSRC of the packets the session's writer sends and
// the CNAME of the reports made over the session, in the answer and in every
// later SDP of ours, an ICE restart included.
func TestMediaSessionWebrtcDeclaresItsSource(t *testing.T) {
	ctx := t.Context()
	offerer, answerer, offer, answer := newEstablishedWebrtcPair(t, func(offer string) string {
		return string(withStreamsAround(t, []byte(offer), []string{webrtcStreamsVideo}, nil))
	})
	source := answerer.sharedSource()
	require.NotZero(t, source.ssrc)
	require.NotEmpty(t, source.cname)
	want := []string{sourceLine(source)}
	assert.Empty(t, sdpLines(offer, "a=ssrc:"), "an offer of one m= section declares none\n%s", offer)
	assert.Equal(t, want, sdpLines(answer, "a=ssrc:"), "the answer\n%s", answer)
	assert.Equal(t, want, audioSectionLines(t, answer, "a=ssrc:"), "the audio section of the answer\n%s", answer)

	// The writer of the session's packets sends as the declared source, and
	// the reports of the session are that source's.
	rtpSession := NewRTPSessionWebrtc(answerer)
	writer := NewRTPPacketWriter(rtpSession, answerer.Codec())
	assert.Equal(t, source.ssrc, writer.SSRC, "the SSRC of the writer")
	assert.Equal(t, source.ssrc, rtpSession.reportSSRC, "the SSRC of the reports")
	assert.Equal(t, rtcp.NewCNAMESourceDescription(0, source.cname), rtpSession.sourceDescription, "the source description of the reports")

	payload := []byte("declared-audio")
	_, err := writer.WriteSamples(payload, CodecAudioUlaw.SampleTimestamp(), true, CodecAudioUlaw.PayloadType)
	require.NoError(t, err)
	require.NoError(t, offerer.StopRTP(1, 5*time.Second))
	got := rtp.Packet{}
	_, err = offerer.ReadRTP(make([]byte, RTPBufSize), &got)
	require.NoError(t, err, "the media did not arrive and decrypt")
	require.NoError(t, offerer.StartRTP(1))
	assert.Equal(t, payload, got.Payload)
	assert.Equal(t, source.ssrc, got.SSRC, "the SSRC of the packet the peer read")

	// A further writer numbers its packets apart, so it is a source of its
	// own.
	assert.NotEqual(t, source.ssrc, NewRTPPacketWriter(rtpSession, answerer.Codec()).SSRC, "the SSRC of a second writer")

	// Our later SDPs describe the same source.
	reoffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	assert.Equal(t, want, audioSectionLines(t, reoffer, "a=ssrc:"), "our offer on the session\n%s", reoffer)
	exchangeWebrtcReOffer(t, answerer, offerer, reoffer)
	own, err := answerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, want, audioSectionLines(t, own, "a=ssrc:"), "an offer of the session\n%s", own)

	// An ICE restart modifies the same session: the writer is kept across
	// it, so the new generation declares the source it sends.
	fork, err := answerer.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, fork)
	forkOffer, err := fork.LocalSDP(ctx, false)
	require.NoError(t, err)
	assert.Equal(t, want, audioSectionLines(t, forkOffer, "a=ssrc:"), "an offer of its fork\n%s", forkOffer)
	forked := rtpSession.Fork(fork)
	assert.Equal(t, source.ssrc, forked.reportSSRC, "the SSRC of the reports after the restart")
	assert.Equal(t, rtcp.NewCNAMESourceDescription(0, source.cname), forked.sourceDescription, "the source description after the restart")
	assert.NotEqual(t, source.ssrc, NewRTPPacketWriter(forked, fork.Codec()).SSRC, "the SSRC of a second writer, made over the fork")
}

// TestMediaSessionWebrtcDeclaresNoSourceInOneSection pins that an SDP with one
// m= section declares no source, whose receiver has no section to choose
// between, and that the writer made over such a session still sends as the
// source the session would declare: an offer of the peer that adds a stream
// later is answered with the SSRC the writer has had from the start.
func TestMediaSessionWebrtcDeclaresNoSourceInOneSection(t *testing.T) {
	ctx := t.Context()
	_, answerer, offer, answer := newEstablishedWebrtcPair(t, nil)
	assert.Empty(t, sdpLines(offer, "a=ssrc:"), "the offer\n%s", offer)
	assert.Empty(t, sdpLines(answer, "a=ssrc:"), "the answer\n%s", answer)
	reoffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	answerer.Rollback()
	assert.Empty(t, sdpLines(reoffer, "a=ssrc:"), "our offer on the session\n%s", reoffer)

	writer := NewRTPPacketWriter(NewRTPSessionWebrtc(answerer), answerer.Codec())

	added := []byte(string(offer) + webrtcStreamsVideo + "\r\n")
	require.NoError(t, answerer.RemoteSDP(ctx, added, false))
	reanswer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	source := answerer.sharedSource()
	assert.Equal(t, []string{sourceLine(source)}, audioSectionLines(t, reanswer, "a=ssrc:"), "the answer to the offer adding a stream\n%s", reanswer)
	assert.Equal(t, source.ssrc, writer.SSRC, "the SSRC of the writer made before it")
}

// TestMediaSessionWebrtcDeclaresNoSourceOfThePeer pins RFC 5576 section 8:
// "The SSRC IDs for sources described by an SDP answer MUST be distinct from
// the SSRC IDs for sources of that media stream in the offer." An offer that
// declares our SSRC for a source of its audio stream is answered without the
// declaration; one that declares it for another stream, or declares another
// SSRC, is answered with it.
func TestMediaSessionWebrtcDeclaresNoSourceOfThePeer(t *testing.T) {
	for _, tc := range []struct {
		name string
		// audio and video are the lines the offer adds to the two sections,
		// given the SSRC of the answerer.
		audio, video func(ssrc uint32) string
		wantDeclared bool
	}{
		{
			name:  "our_SSRC_in_the_audio_section",
			audio: func(ssrc uint32) string { return "a=ssrc:" + strconv.FormatUint(uint64(ssrc), 10) + " cname:peer\r\n" },
		},
		{
			name: "our_SSRC_among_others_in_the_audio_section",
			audio: func(ssrc uint32) string {
				return "a=ssrc:" + strconv.FormatUint(uint64(ssrc^1), 10) + " cname:peer\r\n" +
					"a=ssrc:" + strconv.FormatUint(uint64(ssrc), 10) + " cname:peer\r\n"
			},
		},
		{
			name: "another_SSRC_in_the_audio_section",
			audio: func(ssrc uint32) string {
				return "a=ssrc:" + strconv.FormatUint(uint64(ssrc^1), 10) + " cname:peer\r\n"
			},
			wantDeclared: true,
		},
		{
			name:         "our_SSRC_in_the_video_section",
			video:        func(ssrc uint32) string { return "\r\na=ssrc:" + strconv.FormatUint(uint64(ssrc), 10) + " cname:peer" },
			wantDeclared: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			source := answerer.sharedSource()
			alone, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			video := webrtcStreamsVideo
			if tc.audio != nil {
				alone = []byte(string(alone) + tc.audio(source.ssrc))
			}
			if tc.video != nil {
				video += tc.video(source.ssrc)
			}
			offer := withStreamsAround(t, alone, nil, []string{video})

			require.NoError(t, answerer.RemoteSDP(ctx, offer, false), "the offer\n%s", offer)
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
			var want []string
			if tc.wantDeclared {
				want = []string{sourceLine(source)}
			}
			assert.Equal(t, want, sdpLines(answer, "a=ssrc:"), "the answer\n%s", answer)
		})
	}
}

// TestWebRTCDeclaredSSRCs pins which SSRCs a media section is read as
// declaring: the decimal 32-bit identifier that opens each a=ssrc line (RFC
// 5576 section 4.1), once each.
func TestWebRTCDeclaredSSRCs(t *testing.T) {
	md := &webrtcsdp.MediaDescription{Attributes: []webrtcsdp.Attribute{
		{Key: "mid", Value: "0"},
		{Key: "ssrc", Value: "1234 cname:a"},
		{Key: "ssrc", Value: "1234 msid:s t"},
		{Key: "ssrc", Value: "4294967295 cname:b"},
		{Key: "ssrc", Value: "4294967296 cname:c"},
		{Key: "ssrc", Value: "five cname:d"},
		{Key: "ssrc", Value: ""},
		{Key: "ssrc", Value: "7"},
		{Key: "ssrc-group", Value: "FID 8 9"},
	}}
	assert.Equal(t, []uint32{1234, 4294967295, 7}, webRTCDeclaredSSRCs(md))
	assert.Empty(t, webRTCDeclaredSSRCs(&webrtcsdp.MediaDescription{}))
}

// TestNewRTPPacketWriterTakesTheDeclaredSourceOnce pins where a packet writer
// has its SSRC from: the source of the WebRTC media session under its writer,
// for the first writer made over that session or over an RTP session of it,
// and for no other. A writer over anything else draws its own, as before.
func TestNewRTPPacketWriterTakesTheDeclaredSourceOnce(t *testing.T) {
	t.Run("over_the_media_session", func(t *testing.T) {
		session := &MediaSessionWebrtc{}
		source := session.sharedSource()
		assert.Equal(t, source.ssrc, NewRTPPacketWriter(session, CodecAudioUlaw).SSRC)
		assert.NotEqual(t, source.ssrc, NewRTPPacketWriter(session, CodecAudioUlaw).SSRC, "a second writer")
		assert.NotEqual(t, source.ssrc, NewRTPPacketWriter(NewRTPSessionWebrtc(session), CodecAudioUlaw).SSRC, "a writer over its RTP session")
	})

	t.Run("over_an_RTP_session", func(t *testing.T) {
		session := &MediaSessionWebrtc{}
		rtpSession := NewRTPSessionWebrtc(session)
		source := session.sharedSource()
		assert.Equal(t, source.ssrc, NewRTPPacketWriter(rtpSession, CodecAudioUlaw).SSRC)
		assert.NotEqual(t, source.ssrc, NewRTPPacketWriter(NewRTPSessionWebrtc(session), CodecAudioUlaw).SSRC, "a writer over another RTP session of it")
	})

	t.Run("over_no_media_session", func(t *testing.T) {
		rtpSession := NewRTPSessionWebrtc(nil)
		assert.NotZero(t, rtpSession.reportSSRC, "the SSRC of the reports")
		require.Len(t, rtpSession.sourceDescription.Chunks, 1)
		require.Len(t, rtpSession.sourceDescription.Chunks[0].Items, 1)
		assert.NotEmpty(t, rtpSession.sourceDescription.Chunks[0].Items[0].Text, "the CNAME of the reports")
		for _, writer := range []RTPWriter{nil, rtpSession, (*RTPSessionWebrtc)(nil), (*MediaSessionWebrtc)(nil)} {
			require.NotNil(t, NewRTPPacketWriter(writer, CodecAudioUlaw))
		}
	})

	t.Run("sessions_apart", func(t *testing.T) {
		one, other := &MediaSessionWebrtc{}, &MediaSessionWebrtc{}
		assert.NotEqual(t, one.sharedSource().cname, other.sharedSource().cname, "the CNAMEs of two sessions")
	})
}
