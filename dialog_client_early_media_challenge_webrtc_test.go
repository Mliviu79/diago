// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// challengedWebrtcCaller returns an outgoing dialog whose first INVITE is
// answered by a 183 from early dialog a with the answer of peer, and, once
// challenged is closed, by a 401 challenge; the INVITE sent again with
// credentials is handed to resent. keyed, when not nil, is given the result
// of the peer's negotiation, which it runs once the 183 is out.
func challengedWebrtcCaller(t *testing.T, peer *media.MediaSessionWebrtc, keyed chan<- error, challenged <-chan struct{}, resent func(req *sip.Request, deliver func(*sip.Response))) *DialogClientSession {
	t.Helper()
	return newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
		if req.GetHeader("Authorization") != nil {
			resent(req, deliver)
			return
		}
		answer := webrtcAnswerOf(t, peer, req)
		progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", answer)
		challenge := earlyDialogResponse(req, sip.StatusUnauthorized, "Unauthorized", "a", nil)
		challenge.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="pbx", nonce="early-nonce", algorithm=MD5`))
		go func() {
			deliver(progress)
			if keyed != nil {
				keyed <- peer.Finalize(t.Context())
			}
			select {
			case <-challenged:
			case <-t.Context().Done():
				return
			}
			deliver(challenge)
		}()
	})
}

// webrtcAnswerOf has peer take the offer req carries and returns its answer.
func webrtcAnswerOf(t *testing.T, peer *media.MediaSessionWebrtc, req *sip.Request) []byte {
	t.Helper()
	if err := peer.RemoteSDP(t.Context(), req.Body(), false); err != nil {
		t.Errorf("the peer took no offer: %v", err)
		return nil
	}
	answer, err := peer.LocalSDP(t.Context(), true)
	if err != nil {
		t.Errorf("the peer gave no answer: %v", err)
	}
	return answer
}

// requireWebrtcNothingToPeer fails the test when p receives RTP from the
// dialog within the duration.
func requireWebrtcNothingToPeer(t *testing.T, p *webrtcHandlePeer, within time.Duration) {
	t.Helper()
	require.NoError(t, p.sess.StopRTP(1, within))
	pkt := rtp.Packet{}
	_, err := p.sess.ReadRTP(make([]byte, media.RTPBufSize), &pkt)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the media of the ended early dialog sent RTP to its peer")
	require.NoError(t, p.sess.StartRTP(1))
}

// awaitWebrtcResult returns what done is given, failing the test when it is
// given nothing in time.
func awaitWebrtcResult(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not complete", what)
		return nil
	}
}

// TestDialogClientWebrtcChallengeStopsKeyedEarlyMedia is a WebRTC call with
// EarlyMediaDetect whose 183 is keyed early media and whose INVITE is then
// challenged. The challenge ends the early dialog (RFC 3261 section 12.3) and
// its media stops: the writer handed out for it refuses, no RTP reaches the
// peer, and the media has no transport set up. The callee answers the INVITE
// sent again from the same transport, and the call carries audio both ways.
func TestDialogClientWebrtcChallengeStopsKeyedEarlyMedia(t *testing.T) {
	peer := newLoopbackWebrtcSession(t, []media.Codec{media.CodecAudioUlaw})
	handle := &webrtcHandlePeer{sess: peer, laddr: peer.Laddr}
	keyed := make(chan error, 1)
	challenged := make(chan struct{})
	resent, held := holdResends()
	d := challengedWebrtcCaller(t, peer, keyed, challenged, resent)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	med, err := d.InviteWebrtc(ctx, InviteWebrtcOptions{
		EarlyMediaDetect:     true,
		EarlyMediaKeyTimeout: 20 * time.Second,
		WebrtcConfig:         loopbackWebrtcConfig(),
	})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	require.NoError(t, awaitWebrtcResult(t, keyed, "the peer's negotiation"))
	requireWebrtcAudioToPeer(t, med, handle)
	earlyWriter := med.RTPPacketWriter

	close(challenged)
	answered := make(chan error, 1)
	go func() {
		answered <- d.WaitAnswerWebrtc(ctx, med, sipgo.AnswerOptions{Username: "caller", Password: "secret"})
	}()
	again := awaitResend(t, held)

	_, err = earlyWriter.Write(make([]byte, 160))
	require.ErrorIs(t, err, errEarlyMediaEnded, "the early media writer still writes")
	require.Nil(t, med.MediaSession(), "the transport of the ended early dialog carries media")
	require.Nil(t, med.RTPSession())
	requireWebrtcNothingToPeer(t, handle, 500*time.Millisecond)

	body := webrtcAnswerOf(t, peer, again.req)
	again.deliver(earlyDialogResponse(again.req, sip.StatusSessionInProgress, "Session Progress", "a", body))
	again.deliver(earlyDialogResponse(again.req, sip.StatusOK, "OK", "a", body))
	require.NoError(t, awaitWebrtcResult(t, answered, "the wait for the answer of the INVITE sent again"))
	require.NoError(t, peer.Finalize(ctx))
	requireWebrtcAudioBothWays(t, med, handle)
	require.NoError(t, d.Hangup(ctx))
}

// TestDialogClientWebrtcChallengeEndsLeftNegotiation is a WebRTC call with
// EarlyMediaDetect whose 183 comes from a callee that does not take part in
// the negotiation in time, so it is left running and the wait for the answer
// goes on. The challenge ends the early dialog and that negotiation with it.
// The 183 to the INVITE sent again is taken as early media as the first one
// was, keyed by the callee, and its 200 confirms the call, with audio both
// ways.
func TestDialogClientWebrtcChallengeEndsLeftNegotiation(t *testing.T) {
	peer := newLoopbackWebrtcSession(t, []media.Codec{media.CodecAudioUlaw})
	handle := &webrtcHandlePeer{sess: peer, laddr: peer.Laddr}
	challenged := make(chan struct{})
	resent, held := holdResends()
	d := challengedWebrtcCaller(t, peer, nil, challenged, resent)

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	invited := make(chan error, 1)
	var med *DialogWebrtc
	go func() {
		var err error
		med, err = d.InviteWebrtc(ctx, InviteWebrtcOptions{
			EarlyMediaDetect:     true,
			EarlyMediaKeyTimeout: 300 * time.Millisecond,
			WebrtcConfig:         loopbackWebrtcConfig(),
			Username:             "caller",
			Password:             "secret",
		})
		invited <- err
	}()
	require.Eventually(t, d.earlyKeyingLeft, 10*time.Second, 20*time.Millisecond, "the negotiation of the 183 was not left running")
	close(challenged)
	again := awaitResend(t, held)

	body := webrtcAnswerOf(t, peer, again.req)
	keyed := make(chan error, 1)
	go func() { keyed <- peer.Finalize(ctx) }()
	again.deliver(earlyDialogResponse(again.req, sip.StatusSessionInProgress, "Session Progress", "a", body))
	require.ErrorIs(t, awaitWebrtcResult(t, invited, "the INVITE sent again"), ErrClientEarlyMedia,
		"the 183 to the INVITE sent again was not taken as early media")
	require.NoError(t, awaitWebrtcResult(t, keyed, "the peer's negotiation"))
	requireWebrtcAudioToPeer(t, med, handle)

	answered := make(chan error, 1)
	go func() { answered <- d.WaitAnswerWebrtc(ctx, med, sipgo.AnswerOptions{}) }()
	again.deliver(earlyDialogResponse(again.req, sip.StatusOK, "OK", "a", body))
	require.NoError(t, awaitWebrtcResult(t, answered, "the wait for the 200"))
	requireWebrtcAudioBothWays(t, med, handle)
	require.NoError(t, d.Hangup(ctx))
}
