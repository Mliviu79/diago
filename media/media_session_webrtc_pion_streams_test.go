// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// pionAudioSource writes a PCMU packet of payload to track every 20 ms, as
// an audio source does, until ctx ends. Pion sends what is written to a track
// only once the transports are up and the sender is bound, and reports
// neither, so the source keeps writing as a call does. The returned channel
// carries the error that ended the source, nil for the end of ctx.
func pionAudioSource(ctx context.Context, track *webrtc.TrackLocalStaticRTP, payload []byte) <-chan error {
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for seq := uint16(1); ; seq++ {
			pkt := &rtp.Packet{
				Header: rtp.Header{
					Version:        2,
					PayloadType:    CodecAudioUlaw.PayloadType,
					SequenceNumber: seq,
					Timestamp:      uint32(seq) * CodecAudioUlaw.SampleTimestamp(),
				},
				Payload: payload,
			}
			if err := track.WriteRTP(pkt); err != nil {
				done <- err
				return
			}
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
			}
		}
	}()
	return done
}

// TestMediaSessionWebrtcCarriesAPionCallOfferedWithOtherStreams answers a Pion
// PeerConnection that offers a video stream beside its audio one, in both
// orders, or a data channel, and carries the audio both ways over SRTP.
//
// The answer has one m= line for each of the offer's (RFC 3264 section 6),
// every one but the audio rejected on port 0. Pion reads the formats of every
// m= line of an answer, the rejected ones included, and refuses a description
// that names a dynamic payload type without its a=rtpmap line; and it places
// an RTP stream of a description with more than one m= section only by a
// source the description declared. So the rejected section keeps the a=rtpmap
// lines of the offer, as in the examples of RFC 8843 section 18.5, and the
// audio section declares the source the session sends (RFC 5576, which RFC
// 8843 section 9.2 names for this).
func TestMediaSessionWebrtcCarriesAPionCallOfferedWithOtherStreams(t *testing.T) {
	// pionDataChannel stands for the m=application section of a data channel
	// among the streams a peer offers.
	const pionDataChannel webrtc.RTPCodecType = 0
	for _, tc := range []struct {
		name  string
		kinds []webrtc.RTPCodecType
	}{
		{name: "video_then_audio", kinds: []webrtc.RTPCodecType{webrtc.RTPCodecTypeVideo, webrtc.RTPCodecTypeAudio}},
		{name: "audio_then_video", kinds: []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, webrtc.RTPCodecTypeVideo}},
		{name: "audio_and_a_data_channel", kinds: []webrtc.RTPCodecType{webrtc.RTPCodecTypeAudio, pionDataChannel}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			peer := newPionPeer(t)
			localTrack, err := webrtc.NewTrackLocalStaticRTP(
				webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypePCMU, ClockRate: 8000, Channels: 1}, "audio", "browser")
			require.NoError(t, err)
			wantMedia := make([]string, len(tc.kinds))
			for i, kind := range tc.kinds {
				switch kind {
				case webrtc.RTPCodecTypeAudio:
					_, err = peer.AddTrack(localTrack)
					wantMedia[i] = "m=audio "
				case pionDataChannel:
					_, err = peer.CreateDataChannel("data", nil)
					wantMedia[i] = "m=application 0 "
				default:
					_, err = peer.AddTransceiverFromKind(kind)
					wantMedia[i] = "m=video 0 "
				}
				require.NoError(t, err)
			}
			remote := make(chan *webrtc.TrackRemote, 1)
			peer.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { remote <- track })
			offer := pionOffer(ctx, t, peer)
			require.Len(t, mediaLines(offer), len(tc.kinds), "the offer\n%s", offer)

			session := newPionAnswerer(ctx, t)
			require.NoError(t, session.RemoteSDP(ctx, offer, false), "the offer\n%s", offer)
			answer, err := session.LocalSDP(ctx, true)
			require.NoError(t, err)
			answered := mediaLines(answer)
			require.Len(t, answered, len(wantMedia), "the answer\n%s", answer)
			for i, want := range wantMedia {
				assert.Contains(t, answered[i], want, "stream %d of the answer\n%s", i, answer)
			}
			require.NoError(t, peer.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: string(answer)}),
				"the answer\n%s", answer)

			rtpSession := NewRTPSessionWebrtc(session)
			packetWriter := NewRTPPacketWriter(rtpSession, session.Codec())
			declared := audioSectionLines(t, answer, "a=ssrc:")
			require.Len(t, declared, 1, "the answer\n%s", answer)
			require.True(t, strings.HasPrefix(declared[0], "a=ssrc:"+strconv.FormatUint(uint64(packetWriter.SSRC), 10)+" "),
				"the answer declares the SSRC %d of the writer\n%s", packetWriter.SSRC, answer)
			require.NoError(t, session.Finalize(ctx))

			// Ours to Pion: the track is raised by the first packet Pion
			// places in the audio section.
			sent := []byte("audio-to-the-peer")
			_, err = packetWriter.WriteSamples(sent, CodecAudioUlaw.SampleTimestamp(), true, CodecAudioUlaw.PayloadType)
			require.NoError(t, err)
			select {
			case <-ctx.Done():
				t.Fatalf("Pion raised no track for our audio: %v\nthe answer\n%s", context.Cause(ctx), answer)
			case track := <-remote:
				assert.Equal(t, webrtc.RTPCodecTypeAudio, track.Kind())
				assert.Equal(t, webrtc.SSRC(packetWriter.SSRC), track.SSRC())
				pkt, _, readErr := track.ReadRTP()
				require.NoError(t, readErr)
				assert.Equal(t, sent, pkt.Payload)
			}

			// Pion to ours.
			received := []byte("audio-from-the-peer")
			sourceCtx, stopSource := context.WithCancel(ctx)
			source := pionAudioSource(sourceCtx, localTrack, received)
			defer func() {
				stopSource()
				if err := <-source; err != nil {
					t.Errorf("the Pion audio source: %v", err)
				}
			}()
			require.NoError(t, session.StopRTP(1, 5*time.Second))
			got := rtp.Packet{}
			_, err = session.ReadRTP(make([]byte, RTPBufSize), &got)
			require.NoError(t, err, "the media of the peer did not arrive and decrypt")
			require.NoError(t, session.StartRTP(1))
			assert.Equal(t, received, got.Payload)
		})
	}
}
