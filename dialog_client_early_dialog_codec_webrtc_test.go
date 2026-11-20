// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// withOnlyFormat returns the SDP body with its audio m= line left with the
// one payload type pt, and the rtpmap and fmtp lines of the others removed.
func withOnlyFormat(t *testing.T, body []byte, pt uint8) []byte {
	t.Helper()
	keep := strconv.Itoa(int(pt))
	var dropped []string
	lines := strings.Split(string(body), "\r\n")
	out := lines[:0:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "m=audio ") {
			fields := strings.Fields(l)
			require.GreaterOrEqual(t, len(fields), 4)
			for _, f := range fields[3:] {
				if f != keep {
					dropped = append(dropped, f)
				}
			}
			l = strings.Join(append(fields[:3:3], keep), " ")
		}
		drop := false
		for _, f := range dropped {
			if strings.HasPrefix(l, "a=rtpmap:"+f+" ") || strings.HasPrefix(l, "a=fmtp:"+f+" ") {
				drop = true
			}
		}
		if !drop {
			out = append(out, l)
		}
	}
	return []byte(strings.Join(out, "\r\n"))
}

// TestDialogClientWebrtcOtherEarlyDialogSameEndpointOtherCodec is a WebRTC
// call with EarlyMediaDetect whose 183 and 200 come from two early dialogs of
// one endpoint, with the same ICE credentials and fingerprint and another
// codec each: PCMU in the 183, PCMA in the 200. The negotiation the 183
// started is left running and ended for the 200, and the call runs on the
// codec of the 200's dialog.
func TestDialogClientWebrtcOtherEarlyDialogSameEndpointOtherCodec(t *testing.T) {
	codecs := []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw}
	for _, tc := range webrtcInvites {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()

			peer := newLoopbackWebrtcSession(t, codecs)
			acked := make(chan struct{})
			peerDone := make(chan error, 1)
			peerJoined := make(chan struct{})
			defer func() {
				cancel()
				<-peerJoined
			}()
			go func() {
				defer close(peerJoined)
				select {
				case <-acked:
					peerDone <- peer.Finalize(ctx)
				case <-ctx.Done():
					peerDone <- context.Cause(ctx)
				}
			}()
			d := newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
				answer := webrtcAnswerOf(t, peer, req)
				progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", withOnlyFormat(t, answer, media.CodecAudioUlaw.PayloadType))
				ok := earlyDialogResponse(req, sip.StatusOK, "OK", "b", withOnlyFormat(t, answer, media.CodecAudioAlaw.PayloadType))
				go func() {
					deliver(progress)
					deliver(ok)
				}()
			})
			d.mediaConfig.Codecs = codecs
			requester := &ackSignalRequester{next: d.UA.Client.TxRequester, acked: acked}
			d.UA.Client.TxRequester = requester

			med, err := tc.invite(ctx, d, InviteWebrtcOptions{
				EarlyMediaDetect:     true,
				EarlyMediaKeyTimeout: 300 * time.Millisecond,
				WebrtcConfig:         loopbackWebrtcConfig(),
			})
			require.NoError(t, err, "the 200's dialog was not keyed")
			require.NoError(t, awaitWebrtcResult(t, peerDone, "the callee's negotiation"))
			assert.Equal(t, media.CodecAudioAlaw.Name, med.MediaSession().Codec().Name, "the call does not run on the codec of the 200's dialog")

			_, err = med.RTPPacketWriter.Write(make([]byte, 160))
			require.NoError(t, err)
			pkt := (&webrtcHandlePeer{sess: peer, laddr: peer.Laddr}).recv(t)
			assert.Equal(t, media.CodecAudioAlaw.PayloadType, pkt.PayloadType, "the call sends the codec of the 183's dialog")
			require.NoError(t, d.Hangup(ctx))
		})
	}
}
