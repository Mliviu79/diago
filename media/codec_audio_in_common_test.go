// SPDX-License-Identifier: MPL-2.0

package media

import (
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoteSDPRefusesOnlyTelephoneEventInCommon pins that an offer whose only
// format in common with ours is telephone-event is refused as one with no
// common codec, and so answered 488: the events carry no audio, and the
// session would otherwise send the call's audio in a codec the peer never
// offered.
func TestRemoteSDPRefusesOnlyTelephoneEventInCommon(t *testing.T) {
	m := negotiateSession(CodecAudioUlaw, CodecTelephoneEvent8000)

	err := m.RemoteSDP(negotiateBody(
		"v=0",
		"o=- 1 1 IN IP4 198.51.100.7",
		"s=-",
		"c=IN IP4 198.51.100.7",
		"t=0 0",
		"m=audio 5004 RTP/AVP 8 101",
		"a=rtpmap:8 PCMA/8000",
		"a=rtpmap:101 telephone-event/8000",
	), false)

	require.ErrorIs(t, err, ErrNoCommonCodec)
}

// TestWebrtcRemoteSDPRefusesOnlyTelephoneEventInCommon pins the same for a
// WebRTC session, on its first offer and on a later one: a peer's ICE restart
// that offers PCMA and telephone-event to a call that runs on PCMU is refused
// as having no common codec, and the session keeps the codec it runs on,
// rather than being taken and leaving the session with the zero codec.
func TestWebrtcRemoteSDPRefusesOnlyTelephoneEventInCommon(t *testing.T) {
	t.Run("first_offer", func(t *testing.T) {
		ctx := t.Context()
		peer := newCodecsWebrtcSession(ctx, t, testdata.ClientCertificate(), CodecAudioAlaw, CodecTelephoneEvent8000)
		m := newCodecsWebrtcSession(ctx, t, testdata.ServerCertificate(), CodecAudioUlaw, CodecTelephoneEvent8000)
		offer, err := peer.LocalSDP(ctx, false)
		require.NoError(t, err)

		require.ErrorIs(t, m.RemoteSDP(ctx, offer, false), ErrNoCommonCodec)
	})

	t.Run("ice_restart", func(t *testing.T) {
		ctx := t.Context()
		peer := newCodecsWebrtcSession(ctx, t, testdata.ClientCertificate(), CodecAudioUlaw, CodecTelephoneEvent8000)
		m := newCodecsWebrtcSession(ctx, t, testdata.ServerCertificate(), CodecAudioUlaw, CodecTelephoneEvent8000)
		negotiateWebrtc(t, peer, m, nil)
		requireBothFinalize(t, peer, m, 40*time.Second, "ICE and DTLS negotiation")
		require.Equal(t, CodecAudioUlaw.Name, m.Codec().Name)

		restart, err := peer.Fork(ctx)
		require.NoError(t, err)
		closeWebrtcWithTest(t, restart)
		restart.Codecs = []Codec{CodecAudioAlaw, CodecTelephoneEvent8000}
		offer, err := restart.LocalSDP(ctx, false)
		require.NoError(t, err)

		require.ErrorIs(t, m.RemoteSDP(ctx, offer, false), ErrNoCommonCodec)
		assert.Equal(t, CodecAudioUlaw.Name, m.Codec().Name)
	})
}

// TestWebrtcForkStartsFromConfiguredCodecs pins that a fork of a WebRTC
// session, which offers a new ICE generation, starts from every codec the
// session was set up with, as MediaSession.Fork does, and not from those the
// last negotiation kept; and that it hands them on to its own forks.
func TestWebrtcForkStartsFromConfiguredCodecs(t *testing.T) {
	ctx := t.Context()
	configured := []Codec{CodecAudioUlaw, CodecAudioAlaw, CodecTelephoneEvent8000}
	peer := newCodecsWebrtcSession(ctx, t, testdata.ClientCertificate(), CodecAudioUlaw, CodecTelephoneEvent8000)
	m := newCodecsWebrtcSession(ctx, t, testdata.ServerCertificate(), slices.Clone(configured)...)
	negotiateWebrtc(t, peer, m, nil)
	requireBothFinalize(t, peer, m, 40*time.Second, "ICE and DTLS negotiation")
	require.Equal(t, []Codec{CodecAudioUlaw, CodecTelephoneEvent8000}, codecNames(m.CommonCodecs()), "the fixture negotiates PCMU and telephone-event")

	fork, err := m.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, fork)
	assert.Equal(t, configured, fork.Codecs)
	offer, err := fork.LocalSDP(ctx, false)
	require.NoError(t, err)
	assert.Len(t, sdpLines(offer, "a=rtpmap:8 "), 1, "PCMA in the fork's offer:\n%s", offer)

	// A fork whose Codecs a caller narrowed for its offer still hands every
	// configured codec on.
	fork.Codecs = []Codec{CodecAudioUlaw}
	again, err := fork.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, again)
	assert.Equal(t, configured, again.Codecs)
}

// codecNames returns codecs with only what identifies a format kept, the name
// and the clock rate filled in from the static codec of that name, so a list
// can be compared regardless of the payload types it was negotiated with.
func codecNames(codecs []Codec) []Codec {
	out := make([]Codec, 0, len(codecs))
	for _, c := range codecs {
		for _, known := range []Codec{CodecAudioUlaw, CodecAudioAlaw, CodecTelephoneEvent8000} {
			if strings.EqualFold(c.Name, known.Name) {
				out = append(out, known)
			}
		}
	}
	return out
}

// TestRTPPacketWriterUpdateWriterRefusesUnpaceableCodec pins that
// UpdateWriter refuses a codec with no sample duration or sample rate with
// ErrCodecNotPaceable, where resetting the clock with it panics, and that the
// writer keeps the writer and codec it had.
func TestRTPPacketWriterUpdateWriterRefusesUnpaceableCodec(t *testing.T) {
	session := fakeMediaSessionWriter(0, 1234, io.Discard)
	w := NewRTPPacketWriterSession(NewRTPSession(session))
	before, codecBefore := w.Writer(), w.codec

	for name, codec := range map[string]Codec{
		"zero_codec":       {},
		"no_sample_rate":   {Name: "PCMU", SampleDur: 20 * time.Millisecond},
		"no_sample_period": {Name: "PCMU", SampleRate: 8000},
	} {
		t.Run(name, func(t *testing.T) {
			var err error
			require.NotPanics(t, func() { err = w.UpdateWriter(NewRTPSession(session), codec) })
			require.ErrorIs(t, err, ErrCodecNotPaceable)
			assert.Same(t, before, w.Writer())
			assert.Equal(t, codecBefore, w.codec)
		})
	}

	next := NewRTPSession(session)
	require.NoError(t, w.UpdateWriter(next, CodecAudioUlaw))
	assert.Same(t, next, w.Writer())
	assert.Equal(t, CodecAudioUlaw, w.codec)
}
