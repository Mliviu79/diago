// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"crypto/tls"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// webrtcAudioPort matches the start of the audio m= line of an SDP, up to and
// including its port.
var webrtcAudioPort = regexp.MustCompile(`(?m)^m=audio \d+ `)

// newCodecsWebrtcSession returns a WebRTC session that gathered its candidates
// on the loopback interface, presents certificate and runs on codecs.
func newCodecsWebrtcSession(ctx context.Context, t *testing.T, certificate tls.Certificate, codecs ...Codec) *MediaSessionWebrtc {
	t.Helper()
	m := &MediaSessionWebrtc{
		Codecs: codecs,
		Mode:   sdp.ModeSendrecv,
	}
	require.NoError(t, m.Init(ctx, MediaSessionWebrtcConfig{
		DTLS:            DTLSConfig{Certificates: []tls.Certificate{certificate}},
		IPFamilies:      []ICEIPFamily{ICEIPFamilyIPv4},
		IncludeLoopback: true,
		InterfaceFilter: testLoopbackInterface,
	}))
	closeWebrtcWithTest(t, m)
	return m
}

// TestWebrtcRemoteSDPClassifiesNegotiationFailures obtains ErrNoCommonMedia
// and ErrNoCommonCodec through RemoteSDP of a WebRTC session, in an offer and
// in an answer. A description the session cannot meet is the peer's to change
// and is answered 488, so it has to be told apart from a description that is
// malformed, and the classes from each other: a caller that names the failure
// matches them one after the other.
func TestWebrtcRemoteSDPClassifiesNegotiationFailures(t *testing.T) {
	classes := []error{ErrNoCommonMedia, ErrNoCommonCodec, ErrNoCommonCrypto}

	for _, tc := range []struct {
		name string
		// codecs are those of the session the body is applied to.
		codecs []Codec
		// body returns the description, made from offer, a PCMU offer of
		// another session.
		body func(t *testing.T, offer string) []byte
		// want is the class of the refusal, or nil for a refusal of no class.
		want error
	}{
		{
			name:   "no_common_codec",
			codecs: []Codec{CodecAudioAlaw},
			body:   func(_ *testing.T, offer string) []byte { return []byte(offer) },
			want:   ErrNoCommonCodec,
		},
		{
			name:   "transport_outside_the_RTP_profiles",
			codecs: []Codec{CodecAudioUlaw},
			body: func(t *testing.T, offer string) []byte {
				return sdpWithProto(t, []byte(offer), "UDP/DTLS/SCTP")
			},
			want: ErrNoCommonMedia,
		},
		{
			name:   "no_audio_stream",
			codecs: []Codec{CodecAudioUlaw},
			body: func(t *testing.T, offer string) []byte {
				session, _, found := strings.Cut(offer, "\r\nm=")
				require.True(t, found, "the SDP has no m= line:\n%s", offer)
				return []byte(session + "\r\n" + webrtcStreamsVideo + "\r\n")
			},
			want: ErrNoCommonMedia,
		},
		{
			// Port 0 is a stream that must not be used in an offer and a
			// rejected one in an answer (RFC 3264 sections 5.1 and 6).
			name:   "audio_stream_on_port_0",
			codecs: []Codec{CodecAudioUlaw},
			body: func(t *testing.T, offer string) []byte {
				require.Len(t, webrtcAudioPort.FindAllString(offer, -1), 1, "the audio m= line of:\n%s", offer)
				return []byte(webrtcAudioPort.ReplaceAllString(offer, "m=audio 0 "))
			},
			want: ErrNoCommonMedia,
		},
		{
			// A description that breaks a rule of the transport is malformed,
			// which no change of ours or of the peer's capabilities mends.
			name:   "malformed_and_of_no_class",
			codecs: []Codec{CodecAudioUlaw},
			body: func(t *testing.T, offer string) []byte {
				require.Equal(t, 1, strings.Count(offer, "a=rtcp-mux\r\n"), "the a=rtcp-mux line of:\n%s", offer)
				return []byte(strings.Replace(offer, "a=rtcp-mux\r\n", "", 1))
			},
		},
	} {
		for _, offered := range []bool{false, true} {
			kind := "offer"
			if offered {
				kind = "answer"
			}
			t.Run(tc.name+"_in_"+kind, func(t *testing.T) {
				ctx := t.Context()
				offer := webrtcOffer(ctx, t, testdata.ClientCertificate())
				m := newCodecsWebrtcSession(ctx, t, testdata.ServerCertificate(), tc.codecs...)

				err := m.RemoteSDP(ctx, tc.body(t, offer), offered)

				require.Error(t, err)
				for _, class := range classes {
					if tc.want != nil && errors.Is(class, tc.want) {
						assert.ErrorIs(t, err, class)
						continue
					}
					assert.NotErrorIs(t, err, class)
				}
			})
		}
	}
}
