// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// webrtcInvites are the forms of a WebRTC INVITE that set the media of the
// call up once it is answered: InviteWebrtc, and InviteWebrtcLeaveAck
// followed by Ack.
var webrtcInvites = []struct {
	name   string
	invite func(ctx context.Context, d *DialogClientSession, opts InviteWebrtcOptions) (*DialogWebrtc, error)
}{
	{name: "InviteWebrtc", invite: func(ctx context.Context, d *DialogClientSession, opts InviteWebrtcOptions) (*DialogWebrtc, error) {
		return d.InviteWebrtc(ctx, opts)
	}},
	{name: "InviteWebrtcLeaveAck", invite: func(ctx context.Context, d *DialogClientSession, opts InviteWebrtcOptions) (*DialogWebrtc, error) {
		med, err := d.InviteWebrtcLeaveAck(ctx, opts)
		if err != nil {
			return nil, err
		}
		return med, d.Ack(ctx)
	}},
}

// earlyAnswerWebrtcCallee answers the call of callee with a 183 carrying the
// answer of a transport of its own, then a 200 carrying final(offer), and
// keys that transport, which it hands to setUp. It returns once the call or
// the serving ends.
func earlyAnswerWebrtcCallee(serveCtx context.Context, callee *DialogServerSession, final func(ctx context.Context, offer []byte) ([]byte, error), setUp chan<- *webrtcHandlePeer) error {
	ctx := callee.Context()
	conf, err := prepareWebrtcConfig(loopbackWebrtcConfig())
	if err != nil {
		return err
	}
	early := &media.MediaSessionWebrtc{Codecs: []media.Codec{media.CodecAudioUlaw}}
	if err := early.Init(ctx, conf); err != nil {
		return err
	}
	defer func() { _ = early.Close() }()
	offer := callee.InviteRequest.Body()
	if err := early.RemoteSDP(ctx, offer, false); err != nil {
		return err
	}
	earlyAnswer, err := early.LocalSDP(ctx, true)
	if err != nil {
		return err
	}
	finalAnswer, err := final(ctx, offer)
	if err != nil {
		return err
	}
	contentType := sip.NewHeader("Content-Type", "application/sdp")
	if err := callee.DialogServerSession.Respond(ctx, sip.StatusSessionInProgress, "Session Progress", earlyAnswer, contentType); err != nil {
		return err
	}
	if finalAnswer == nil {
		err = callee.DialogServerSession.Respond(ctx, sip.StatusOK, "OK", nil)
	} else {
		err = callee.DialogServerSession.Respond(ctx, sip.StatusOK, "OK", finalAnswer, contentType)
	}
	if err != nil {
		return err
	}
	if err := early.Finalize(ctx); err != nil {
		return err
	}
	setUp <- &webrtcHandlePeer{sess: early, laddr: early.Laddr}
	// The handler ends with its run as well as with its dialog: a dialog whose
	// BYE never arrived would otherwise hold serving past the test.
	select {
	case <-serveCtx.Done():
	case <-ctx.Done():
	}
	return nil
}

// runEarlyAnswerWebrtcCall places a WebRTC call without EarlyMediaDetect, in
// each form of webrtcInvites, to a callee that answers in a 183 and sends the
// 200 with final(offer), and requires the call to run on the transport of the
// 183, with audio both ways.
func runEarlyAnswerWebrtcCall(t *testing.T, final func(ctx context.Context, offer []byte) ([]byte, error)) {
	for _, tc := range webrtcInvites {
		t.Run(tc.name, func(t *testing.T) {
			calleeSetUp := make(chan *webrtcHandlePeer, 1)
			calleeEnded := make(chan error, 1)
			d := newWebrtcCallPair(t, func(serveCtx context.Context, callee *DialogServerSession) {
				calleeEnded <- earlyAnswerWebrtcCallee(serveCtx, callee, final, calleeSetUp)
			})

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			med, err := tc.invite(ctx, d, InviteWebrtcOptions{WebrtcConfig: loopbackWebrtcConfig()})
			require.NoError(t, err, "the call did not run on the answer of the 183")
			var peer *webrtcHandlePeer
			select {
			case peer = <-calleeSetUp:
			case err := <-calleeEnded:
				t.Fatalf("the callee's transport was not keyed: %v", err)
			case <-time.After(20 * time.Second):
				t.Fatal("the callee's transport was not keyed")
			}
			requireWebrtcAudioBothWays(t, med, peer)

			require.NoError(t, d.Hangup(t.Context()))
			select {
			case err := <-calleeEnded:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("the callee did not return")
			}
		})
	}
}

// TestDialogClientWebrtcEarlyDialogAnswerWithoutDetect pins that, without
// EarlyMediaDetect, the session description of a 183 is the answer of its
// early dialog (RFC 3261 section 13.2.1): the 200 of that dialog carries the
// answer of another transport, with its own ICE credentials and fingerprint,
// which is not applied.
func TestDialogClientWebrtcEarlyDialogAnswerWithoutDetect(t *testing.T) {
	runEarlyAnswerWebrtcCall(t, func(ctx context.Context, offer []byte) ([]byte, error) {
		conf, err := prepareWebrtcConfig(loopbackWebrtcConfig())
		if err != nil {
			return nil, err
		}
		other := &media.MediaSessionWebrtc{Codecs: []media.Codec{media.CodecAudioAlaw}}
		if err := other.Init(ctx, conf); err != nil {
			return nil, err
		}
		// Only its description is used: closing the transport leaves it with
		// nothing to answer the caller's checks with.
		defer func() { _ = other.Close() }()
		if err := other.RemoteSDP(ctx, offer, false); err != nil {
			return nil, err
		}
		return other.LocalSDP(ctx, true)
	})
}

// TestDialogClientWebrtcEarlyDialogAnswerFinalWithoutSDP pins that, without
// EarlyMediaDetect, a 200 without a session description is answered when the
// 183 of its dialog carried one: that is the answer (RFC 3261 section 13.2.1,
// RFC 6337 section 3.1.1).
func TestDialogClientWebrtcEarlyDialogAnswerFinalWithoutSDP(t *testing.T) {
	runEarlyAnswerWebrtcCall(t, func(context.Context, []byte) ([]byte, error) {
		return nil, nil
	})
}

// TestDialogClientWebrtcOtherEarlyDialogEndsLeftNegotiation is a WebRTC call
// with EarlyMediaDetect whose 183 comes from a callee that never keys it, so
// the negotiation it started is left running, and whose 200 comes from
// another early dialog, with a transport of its own. That negotiation is
// ended and the answer of the 200's dialog applied to the transport of our
// offer, which can take it only with an ICE restart our offer did not ask
// for: the error says so, and the call is acknowledged and ended, well before
// the negotiation of the 183 could have timed out.
func TestDialogClientWebrtcOtherEarlyDialogEndsLeftNegotiation(t *testing.T) {
	for _, tc := range webrtcInvites {
		t.Run(tc.name, func(t *testing.T) {
			early := newLoopbackWebrtcSession(t, []media.Codec{media.CodecAudioUlaw})
			answering := newLoopbackWebrtcSession(t, []media.Codec{media.CodecAudioUlaw})
			answer := func(sess *media.MediaSessionWebrtc, req *sip.Request) []byte {
				if err := sess.RemoteSDP(t.Context(), req.Body(), false); err != nil {
					t.Errorf("the peer took no offer: %v", err)
					return nil
				}
				body, err := sess.LocalSDP(t.Context(), true)
				if err != nil {
					t.Errorf("the peer gave no answer: %v", err)
				}
				return body
			}
			d := newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
				progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", answer(early, req))
				ok := earlyDialogResponse(req, sip.StatusOK, "OK", "b", answer(answering, req))
				go func() {
					deliver(progress)
					deliver(ok)
				}()
			})

			ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
			defer cancel()
			start := time.Now()
			med, err := tc.invite(ctx, d, InviteWebrtcOptions{
				EarlyMediaDetect:     true,
				EarlyMediaKeyTimeout: 300 * time.Millisecond,
				WebrtcConfig:         loopbackWebrtcConfig(),
			})
			require.ErrorIs(t, err, media.ErrWebRTCICERestart)
			require.Nil(t, med)
			require.Less(t, time.Since(start), 10*time.Second, "the negotiation of the 183 was waited for")
			require.Equal(t, sip.DialogStateEnded, d.LoadState(), "the call of the 200 was not ended")
		})
	}
}
