// SPDX-License-Identifier: MPL-2.0

package media

import (
	"testing"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestWebrtcNegotiatedTelephoneEvent pins that the telephone-event codec of a
// WebRTC call is found in the list negotiation settled on, at the number the
// offer gave it. telephone-event is a dynamic format, which each side numbers
// on its own (RFC 3551 section 3) and an answer repeats as offered (RFC 3264
// section 6.1), so the number of CodecTelephoneEvent8000 is the one of the
// call only when the offerer chose it too.
func TestWebrtcNegotiatedTelephoneEvent(t *testing.T) {
	t.Run("offered_at_another_number", func(t *testing.T) {
		ctx := t.Context()
		offerer := newCodecsWebrtcSession(ctx, t, testdata.ClientCertificate(), CodecAudioUlaw, telephoneEventAt(110))
		answerer := newCodecsWebrtcSession(ctx, t, testdata.ServerCertificate(), CodecAudioUlaw, CodecTelephoneEvent8000)

		_, answer := negotiateWebrtc(t, offerer, answerer, nil)

		assert.Equal(t, []string{"a=rtpmap:110 telephone-event/8000"}, sdpLines(answer, "a=rtpmap:110 "), "the answer\n%s", answer)
		assert.Empty(t, sdpLines(answer, "a=rtpmap:101 "), "the answer\n%s", answer)
		for name, m := range map[string]*MediaSessionWebrtc{"offerer": offerer, "answerer": answerer} {
			te, ok := CodecTelephoneEventFromList(m.CommonCodecs())
			require.True(t, ok, "the %s negotiated telephone-event", name)
			assert.EqualValues(t, 110, te.PayloadType, "the %s sends DTMF on the offered number", name)
			assert.EqualValues(t, 110, te.PayloadTypeRecv(), "the %s reads DTMF on the offered number", name)
		}
	})

	t.Run("not_offered", func(t *testing.T) {
		ctx := t.Context()
		offerer := newCodecsWebrtcSession(ctx, t, testdata.ClientCertificate(), CodecAudioUlaw)
		answerer := newCodecsWebrtcSession(ctx, t, testdata.ServerCertificate(), CodecAudioUlaw, CodecTelephoneEvent8000)

		negotiateWebrtc(t, offerer, answerer, nil)

		for name, m := range map[string]*MediaSessionWebrtc{"offerer": offerer, "answerer": answerer} {
			_, ok := CodecTelephoneEventFromList(m.CommonCodecs())
			assert.False(t, ok, "the %s negotiated no telephone-event", name)
		}
	})
}
