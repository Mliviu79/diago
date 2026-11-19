// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"regexp"
	"strings"
	"testing"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webrtcAudioFormats matches the format list of the audio m= line of an SDP.
var webrtcAudioFormats = regexp.MustCompile(`(?m)^(m=audio \d+ \S+) [0-9 ]+\r$`)

// TestDialogWebrtcReInviteWithOnlyTelephoneEventInCommon pins that a peer's
// re-INVITE whose only format in common with a call that runs on PCMU is
// telephone-event is refused with ErrNoCommonCodec, which the session answers
// 488 with a Warning, and that the call stays on the media it runs on. Taken,
// such an offer left the media with the zero codec, and installing it panicked
// the packet writer.
func TestDialogWebrtcReInviteWithOnlyTelephoneEventInCommon(t *testing.T) {
	offers := map[string]func(t *testing.T, p *webrtcHandlePeer) []byte{
		// A new ICE generation, which the dialog takes on a new transport.
		"ice_restart": func(t *testing.T, p *webrtcHandlePeer) []byte {
			fork, err := p.sess.Fork(t.Context())
			require.NoError(t, err)
			t.Cleanup(func() { assert.NoError(t, fork.Close()) })
			fork.Codecs = []media.Codec{media.CodecAudioAlaw, media.CodecTelephoneEvent8000}
			offer, err := fork.LocalSDP(t.Context(), false)
			require.NoError(t, err)
			return offer
		},
		// The same ICE generation, which the dialog takes on the transport in
		// place.
		"in_place": func(t *testing.T, p *webrtcHandlePeer) []byte {
			offer, err := p.sess.ReOffer("")
			require.NoError(t, err)
			body := string(offer)
			require.Len(t, webrtcAudioFormats.FindAllString(body, -1), 1, "the audio m= line of:\n%s", body)
			body = webrtcAudioFormats.ReplaceAllString(body, "$1 8 101\r")
			require.Equal(t, 1, strings.Count(body, "a=rtpmap:0 pcmu/8000\r\n"), "the PCMU rtpmap of:\n%s", body)
			return []byte(strings.Replace(body, "a=rtpmap:0 pcmu/8000\r\n", "a=rtpmap:8 pcma/8000\r\n", 1))
		},
	}
	for name, offerOf := range offers {
		t.Run(name, func(t *testing.T) {
			d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000})
			before := d.MediaSession()

			err := d.onRemoteSDP(t.Context(), offerOf(t, p), false)

			require.ErrorIs(t, err, media.ErrNoCommonCodec)
			res, ok := notAcceptableHere(sip.NewRequest(sip.INVITE, sip.Uri{Host: "192.0.2.10", Port: 5080}), err)
			require.True(t, ok)
			assert.Equal(t, sip.StatusNotAcceptableHere, res.StatusCode)
			assert.Equal(t, []string{`305 192.0.2.10:5080 "Incompatible media format"`}, headerValues(res, "Warning"))
			assert.Same(t, before, d.MediaSession())
			assert.True(t, strings.EqualFold(media.CodecAudioUlaw.Name, d.MediaSession().Codec().Name), "the call runs on %v", d.MediaSession().Codec())
		})
	}
}

// headerValues returns the values of every header of res named name.
func headerValues(res *sip.Response, name string) []string {
	var values []string
	for _, h := range res.GetHeaders(name) {
		values = append(values, h.Value())
	}
	return values
}

// TestDialogWebrtcReInviteOfferCodecs pins the codecs of a re-INVITE of ours
// on a DialogWebrtc, which moves the call to a new ICE generation: every
// configured codec, as DialogMedia offers, and with
// MediaConfig.ReInviteNegotiatedCodecs only those the call runs on, except for
// an offer that changes the direction, which offers every configured codec
// either way.
func TestDialogWebrtcReInviteOfferCodecs(t *testing.T) {
	for _, tc := range []struct {
		name       string
		negotiated bool
		mode       string
		wantPCMA   bool
	}{
		{name: "configured", wantPCMA: true},
		{name: "negotiated", negotiated: true},
		{name: "negotiated_hold", negotiated: true, mode: "sendonly", wantPCMA: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := newWebrtcHandleCallWith(t,
				[]media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000},
				[]media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000})
			d.mu.Lock()
			d.offerNegotiatedCodecs = tc.negotiated
			d.mu.Unlock()

			offer, err := d.onLocalSDP(t.Context(), false, tc.mode)
			require.NoError(t, err)
			t.Cleanup(d.abortPendingMediaSession)

			formats := webrtcAudioFormats.FindStringSubmatch(string(offer))
			require.NotNil(t, formats, "the audio m= line of:\n%s", offer)
			assert.Equal(t, tc.wantPCMA, strings.Contains(string(offer), "a=rtpmap:8 pcma/8000\r\n"), "PCMA in the offer:\n%s", offer)
			assert.Contains(t, string(offer), "a=rtpmap:0 pcmu/8000\r\n")
		})
	}
}
