// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAnsweringWebrtcMedia returns WebRTC dialog media that has taken the
// offer of peer and described its answer, as ProgressMediaWebrtc has it when
// its 183 is out, with that answer.
func newAnsweringWebrtcMedia(t *testing.T, peerOffer []byte) (*DialogWebrtc, []byte) {
	t.Helper()
	med, err := newDialogWebrtc(t.Context(), webrtcAnswerConfig(), nil, loopbackWebrtcConfig(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = med.Close() })
	answer, err := answerToOffer(t.Context(), med, peerOffer)
	require.NoError(t, err)
	return med, answer
}

// TestDialogWebrtcEarlyKeyingLeftRunning pins what WebRTC media does with a
// negotiation the peer has not completed within the wait: it reports it in
// progress and leaves it running, has no transport set up meanwhile, and sets
// it up, with its RTCP monitor, once onFinalize has waited for it.
func TestDialogWebrtcEarlyKeyingLeftRunning(t *testing.T) {
	peer, offer := newWebrtcPeerOffer(t)
	med, answer := newAnsweringWebrtcMedia(t, offer)

	require.ErrorIs(t, med.onFinalizeWithin(t.Context(), 100*time.Millisecond), errMediaKeyingInProgress)
	assert.Nil(t, med.MediaSession(), "media that is not keyed has no transport set up")
	assert.Nil(t, med.RTPSession())
	_, err := med.AudioReader()
	assert.ErrorIs(t, err, ErrNoMediaSetup)
	_, err = med.AudioWriter()
	assert.ErrorIs(t, err, ErrNoMediaSetup)
	// Asked again without waiting, it still runs.
	require.ErrorIs(t, med.onFinalizeWithin(t.Context(), 0), errMediaKeyingInProgress)

	// The peer takes part only now.
	require.NoError(t, peer.RemoteSDP(t.Context(), answer, true))
	peerDone := finalizeWebrtcPeer(t, peer)
	require.NoError(t, med.onFinalize(t.Context()))
	requireWebrtcPeerFinalized(t, peerDone)
	requireWebrtcMediaRunsWith(t, med, peer)

	// A second commit sets nothing up again.
	sess, rtpSess := med.MediaSession(), med.RTPSession()
	require.NoError(t, med.onFinalize(t.Context()))
	assert.Same(t, sess, med.MediaSession())
	assert.Same(t, rtpSess, med.RTPSession())
}

// TestDialogWebrtcEarlyKeyingKeyed pins that WebRTC media the peer keys
// within the wait is set up at once, carries audio, and that the commit
// following the answer and a later keying set nothing up again.
func TestDialogWebrtcEarlyKeyingKeyed(t *testing.T) {
	peer, offer := newWebrtcPeerOffer(t)
	med, answer := newAnsweringWebrtcMedia(t, offer)

	require.NoError(t, peer.RemoteSDP(t.Context(), answer, true))
	peerDone := finalizeWebrtcPeer(t, peer)
	require.NoError(t, med.onFinalizeWithin(t.Context(), 20*time.Second))
	requireWebrtcPeerFinalized(t, peerDone)
	requireWebrtcMediaRunsWith(t, med, peer)

	sess, rtpSess := med.MediaSession(), med.RTPSession()
	require.NoError(t, med.onFinalizeWithin(t.Context(), 0))
	require.NoError(t, med.onFinalize(t.Context()))
	assert.Same(t, sess, med.MediaSession())
	assert.Same(t, rtpSess, med.RTPSession())
}

// TestDialogWebrtcEarlyKeyingRefused pins the classes WebRTC media answers
// the keying with when it cannot run it.
func TestDialogWebrtcEarlyKeyingRefused(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		_, offer := newWebrtcPeerOffer(t)
		med, _ := newAnsweringWebrtcMedia(t, offer)
		require.NoError(t, med.Close())
		assert.ErrorIs(t, med.onFinalizeWithin(t.Context(), time.Minute), errMediaClosed)
	})

	t.Run("no_media_session", func(t *testing.T) {
		med := &DialogWebrtc{}
		assert.ErrorIs(t, med.onFinalizeWithin(t.Context(), time.Minute), errNoMediaSession)
	})

	// Closing the media ends the negotiation left running: the keying then
	// reports the media closed, and nothing is set up.
	t.Run("closed_while_left_running", func(t *testing.T) {
		_, offer := newWebrtcPeerOffer(t)
		med, _ := newAnsweringWebrtcMedia(t, offer)
		require.ErrorIs(t, med.onFinalizeWithin(t.Context(), 50*time.Millisecond), errMediaKeyingInProgress)
		require.NoError(t, med.Close())
		require.ErrorIs(t, med.onFinalizeWithin(t.Context(), 0), errMediaClosed)
		assert.Nil(t, med.MediaSession(), "closed media is not set up")
	})
}

// newProgressMediaWebrtcDialog builds an inbound dialog over a fake
// transaction whose INVITE carries offer, and whose media config is that of
// the WebRTC answer tests. The transaction hands every response it is given
// to the returned channel; requests the dialog sends go to the other.
func newProgressMediaWebrtcDialog(t *testing.T, offer []byte) (*DialogServerSession, *respondedServerTx, <-chan *sip.Request) {
	t.Helper()
	inviteTx := &respondedServerTx{byeServerTx: newByeServerTx(), responded: make(chan *sip.Response, 4)}
	d, sent := newRecordingServerDialog(t, inviteTx, offer, webrtcAnswerConfig())
	return d, inviteTx, sent
}

// progressWebrtcOptions are the options of ProgressMediaWebrtc in the tests,
// with keyTimeout.
func progressWebrtcOptions(keyTimeout time.Duration) ProgressMediaWebrtcOptions {
	return ProgressMediaWebrtcOptions{WebrtcConfig: loopbackWebrtcConfig(), KeyTimeout: keyTimeout}
}

// requireResponse requires that the transaction is given a response with
// status, and returns it.
func requireResponse(t *testing.T, inviteTx *respondedServerTx, status int) *sip.Response {
	t.Helper()
	select {
	case res := <-inviteTx.responded:
		require.Equal(t, status, res.StatusCode)
		return res
	case <-time.After(10 * time.Second):
		t.Fatalf("no %d was sent", status)
	}
	return nil
}

// TestDialogServerProgressMediaWebrtcUnkeyed pins the early media of a
// caller that does not key it: the 183 carries the answer, ProgressMediaWebrtc
// gives up after its KeyTimeout with the media, which has no transport set up,
// and the answer of a DialogMedia call is refused for it without a response.
func TestDialogServerProgressMediaWebrtcUnkeyed(t *testing.T) {
	_, offer := newWebrtcPeerOffer(t)
	d, inviteTx, _ := newProgressMediaWebrtcDialog(t, offer)

	start := time.Now()
	med, err := d.ProgressMediaWebrtc(t.Context(), progressWebrtcOptions(300*time.Millisecond))
	require.ErrorIs(t, err, ErrEarlyMediaNotKeyed)
	require.NotNil(t, med, "the media the answer takes is returned with the error")
	assert.Less(t, time.Since(start), 5*time.Second, "ProgressMediaWebrtc waited past its KeyTimeout")
	progress := requireResponse(t, inviteTx, sip.StatusSessionInProgress)
	assert.Contains(t, string(progress.Body()), "a=fingerprint:", "the 183 carries our answer")

	assert.Nil(t, med.MediaSession())
	_, err = med.AudioWriter()
	assert.ErrorIs(t, err, ErrNoMediaSetup)

	_, err = d.AnswerContext(t.Context())
	require.ErrorIs(t, err, errEarlyMediaOtherType)
	select {
	case res := <-inviteTx.responded:
		t.Fatalf("an answer refused for the type of its media sent %d", res.StatusCode)
	default:
	}
}

// TestDialogServerAnswerWebrtcCompletesEarlyKeying is the answer to a WebRTC
// call whose caller ignored the 183, over a fake transaction:
// ProgressMediaWebrtc gave up on the keys, the 200 repeats the 183's answer,
// the caller takes part in ICE and DTLS only then, and the reading of the ACK
// waits for them. The answer returns the early media, set up and carrying
// audio.
func TestDialogServerAnswerWebrtcCompletesEarlyKeying(t *testing.T) {
	answers := []struct {
		name   string
		answer func(t *testing.T, d *DialogServerSession) (*DialogWebrtc, error)
	}{
		{name: "AnswerWebrtcContext", answer: func(t *testing.T, d *DialogServerSession) (*DialogWebrtc, error) {
			return d.AnswerWebrtcContext(t.Context(), webrtcAnswerOptions())
		}},
		{name: "AnswerWebrtc", answer: func(_ *testing.T, d *DialogServerSession) (*DialogWebrtc, error) {
			return d.AnswerWebrtc(webrtcAnswerOptions())
		}},
	}
	for _, tc := range answers {
		t.Run(tc.name, func(t *testing.T) {
			peer, offer := newWebrtcPeerOffer(t)
			d, inviteTx, sent := newProgressMediaWebrtcDialog(t, offer)

			early, err := d.ProgressMediaWebrtc(t.Context(), progressWebrtcOptions(100*time.Millisecond))
			require.ErrorIs(t, err, ErrEarlyMediaNotKeyed)
			progress := requireResponse(t, inviteTx, sip.StatusSessionInProgress)

			answered := make(chan webrtcContextAnswer, 1)
			go func() {
				med, err := tc.answer(t, d)
				answered <- webrtcContextAnswer{med: med, err: err}
			}()
			res := requireResponse(t, inviteTx, sip.StatusOK)
			require.Equal(t, string(progress.Body()), string(res.Body()), "the 200 must repeat the answer of the 183")

			// The caller acts on the answer only now, and acknowledges it.
			require.NoError(t, peer.RemoteSDP(t.Context(), res.Body(), true))
			peerDone := finalizeWebrtcPeer(t, peer)
			readAck(t, d)
			requireWebrtcPeerFinalized(t, peerDone)

			answer := awaitWebrtcAnswer(t, answered)
			require.NoError(t, answer.err)
			require.Same(t, early, answer.med, "the answer hands back the early media")
			requireWebrtcMediaRunsWith(t, early, peer)
			select {
			case req := <-sent:
				t.Errorf("the dialog sent %s on an answered call", req.Method)
			default:
			}
		})
	}
}

// TestDialogServerAnswerWebrtcAfterEarlyHandshakeFailed is an answer after
// ProgressMediaWebrtc returned ErrEarlyMediaNotKeyed, once the negotiation it
// left running has failed: the caller took part only after the wait, and
// presented a certificate that matches none of the fingerprints of its offer.
// The call has no media to answer with, so it is declined 488 (RFC 3261
// section 21.4.26).
func TestDialogServerAnswerWebrtcAfterEarlyHandshakeFailed(t *testing.T) {
	peer, offer := newWebrtcPeerOffer(t)
	_, otherOffer := newWebrtcPeerOffer(t)
	offer = withFingerprint(offer, sdpLineWithPrefix(t, otherOffer, "a=fingerprint:"))
	d, inviteTx, _ := newProgressMediaWebrtcDialog(t, offer)

	med, err := d.ProgressMediaWebrtc(t.Context(), progressWebrtcOptions(200*time.Millisecond))
	require.ErrorIs(t, err, ErrEarlyMediaNotKeyed)
	progress := requireResponse(t, inviteTx, sip.StatusSessionInProgress)

	require.NoError(t, peer.RemoteSDP(t.Context(), progress.Body(), true))
	peerCtx, cancelPeer := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelPeer()
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Finalize(peerCtx) }()
	// Our side fails the handshake on the certificate. The failure stays with
	// the media.
	err = med.onFinalizeWithin(t.Context(), 20*time.Second)
	require.Error(t, err)
	require.NotErrorIs(t, err, errMediaKeyingInProgress)

	answered := make(chan error, 1)
	go func() {
		_, err := d.AnswerWebrtcContext(t.Context(), webrtcAnswerOptions())
		answered <- err
	}()
	requireResponse(t, inviteTx, sip.StatusNotAcceptableHere)
	// The transaction ends, as the ACK to the 488 ends it.
	close(inviteTx.done)
	select {
	case err := <-answered:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the answer did not return")
	}
	cancelPeer()
	select {
	case <-peerDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the peer's handshake did not return")
	}
}

// TestIntegrationDialogWebrtcEarlyMediaKeyedByCaller is a WebRTC call whose
// caller takes the answer in the 183 and keys it at once, InviteWebrtc with
// EarlyMediaDetect against ProgressMediaWebrtc. ProgressMediaWebrtc returns
// once the early media is keyed, the early media carries audio both ways, the
// 200 repeats the 183's answer, which the caller applies in place, and the
// media after the answer still carries audio.
func TestIntegrationDialogWebrtcEarlyMediaKeyedByCaller(t *testing.T) {
	type progressResult struct {
		med *DialogWebrtc
		err error
	}
	progressed := make(chan progressResult, 1)
	answerNow := make(chan struct{})
	answered := make(chan webrtcContextAnswer, 1)
	d := newWebrtcCallPair(t, func(ctx context.Context, callee *DialogServerSession) {
		med, err := callee.ProgressMediaWebrtc(ctx, progressWebrtcOptions(10*time.Second))
		progressed <- progressResult{med: med, err: err}
		if err != nil {
			return
		}
		select {
		case <-answerNow:
		case <-callee.Context().Done():
			return
		}
		med, err = callee.AnswerWebrtcContext(ctx, webrtcAnswerOptions())
		answered <- webrtcContextAnswer{med: med, err: err}
		if err != nil {
			return
		}
		<-callee.Context().Done()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	callerMed, err := d.InviteWebrtc(ctx, InviteWebrtcOptions{EarlyMediaDetect: true, WebrtcConfig: loopbackWebrtcConfig()})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	require.NotNil(t, callerMed.MediaSession(), "the early media is set up")

	var calleeMed *DialogWebrtc
	select {
	case r := <-progressed:
		require.NoError(t, r.err, "early media the caller keyed must be reported keyed")
		calleeMed = r.med
	case <-time.After(20 * time.Second):
		t.Fatal("ProgressMediaWebrtc did not return")
	}
	requireWebrtcAudioBetween(t, callerMed, calleeMed)

	close(answerNow)
	require.NoError(t, d.WaitAnswerWebrtc(ctx, callerMed, sipgo.AnswerOptions{}))
	select {
	case r := <-answered:
		require.NoError(t, r.err)
		require.Same(t, calleeMed, r.med, "the answer hands back the early media")
	case <-time.After(20 * time.Second):
		t.Fatal("the callee did not answer")
	}
	requireWebrtcAudioBetween(t, callerMed, calleeMed)
	require.NoError(t, d.Hangup(t.Context()))
}

// TestIntegrationDialogWebrtcEarlyMediaIgnoredByCaller is a WebRTC call whose
// caller ignores the answer in the 183, as RFC 3960 lets it, and takes part in
// ICE and DTLS only after the 200: InviteWebrtc without EarlyMediaDetect.
// ProgressMediaWebrtc gives up on early media after its KeyTimeout. The call
// stays answerable: the answer sends the 200, and the negotiation
// ProgressMediaWebrtc started completes with the caller once the ACK is read.
func TestIntegrationDialogWebrtcEarlyMediaIgnoredByCaller(t *testing.T) {
	type progressResult struct {
		err  error
		took time.Duration
	}
	progressed := make(chan progressResult, 1)
	answered := make(chan webrtcContextAnswer, 1)
	d := newWebrtcCallPair(t, func(ctx context.Context, callee *DialogServerSession) {
		start := time.Now()
		_, err := callee.ProgressMediaWebrtc(ctx, progressWebrtcOptions(time.Second))
		progressed <- progressResult{err: err, took: time.Since(start)}
		med, err := callee.AnswerWebrtcContext(ctx, webrtcAnswerOptions())
		answered <- webrtcContextAnswer{med: med, err: err}
		if err != nil {
			return
		}
		<-callee.Context().Done()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	invited := make(chan webrtcContextAnswer, 1)
	go func() {
		med, err := d.InviteWebrtc(ctx, InviteWebrtcOptions{WebrtcConfig: loopbackWebrtcConfig()})
		invited <- webrtcContextAnswer{med: med, err: err}
	}()

	select {
	case r := <-progressed:
		require.ErrorIs(t, r.err, ErrEarlyMediaNotKeyed)
		require.Less(t, r.took, 5*time.Second, "ProgressMediaWebrtc waited past its KeyTimeout")
	case <-time.After(20 * time.Second):
		t.Fatal("ProgressMediaWebrtc did not give up on early media within its KeyTimeout")
	}
	var call webrtcContextAnswer
	select {
	case call = <-invited:
	case <-time.After(30 * time.Second):
		t.Fatal("the caller's InviteWebrtc did not return")
	}
	require.NoError(t, call.err)
	answer := awaitWebrtcAnswer(t, answered)
	require.NoError(t, answer.err, "the call must stay answerable")
	requireWebrtcAudioBetween(t, call.med, answer.med)
	require.NoError(t, d.Hangup(t.Context()))
}

// unkeyedWebrtcCallee answers a WebRTC INVITE as a callee that ignores early
// media does (RFC 3960): its answer goes out in a 183, a second 183 carries a
// session description the caller must not take, and only after the 200
// carries that answer again does it take part in ICE and DTLS. The second 183
// goes out once the caller has read the first, closing firstRead: sipgo hands
// the caller only the latest provisional response its wait has not read yet.
// It reports the error of its negotiation on ended.
func unkeyedWebrtcCallee(callee *DialogServerSession, firstRead <-chan struct{}, ended chan<- error) {
	ended <- func() (err error) {
		ctx := callee.Context()
		conf, err := prepareWebrtcConfig(loopbackWebrtcConfig())
		if err != nil {
			return err
		}
		sess := &media.MediaSessionWebrtc{Codecs: callee.mediaConf.Codecs}
		if err := sess.Init(ctx, conf); err != nil {
			return err
		}
		defer func() { err = errors.Join(err, sess.Close()) }()
		if err := sess.RemoteSDP(ctx, callee.InviteRequest.Body(), false); err != nil {
			return err
		}
		answer, err := sess.LocalSDP(ctx, true)
		if err != nil {
			return err
		}
		contentType := sip.NewHeader("Content-Type", "application/sdp")
		if err := callee.DialogServerSession.Respond(ctx, sip.StatusSessionInProgress, "Session Progress", answer, contentType); err != nil {
			return err
		}
		select {
		case <-firstRead:
		case <-ctx.Done():
			return context.Cause(ctx)
		}
		if err := callee.DialogServerSession.Respond(ctx, sip.StatusSessionInProgress, "Session Progress", []byte("v=0\r\n"), contentType); err != nil {
			return err
		}
		if err := callee.DialogServerSession.Respond(ctx, sip.StatusOK, "OK", answer, contentType); err != nil {
			return err
		}
		if err := sess.Finalize(ctx); err != nil {
			return err
		}
		<-ctx.Done()
		return nil
	}()
}

// TestDialogClientWebrtcEarlyMediaUnkeyedByCallee is a WebRTC call with
// EarlyMediaDetect whose callee sends its answer in a 183 but takes part in
// ICE and DTLS only after the 200. The wait for keys in the 183 is bounded by
// EarlyMediaKeyTimeout, so the INVITE goes on to the 200 without reporting
// early media, a later 183 is ignored, and the ACK completes the negotiation
// left running and sets the media up.
func TestDialogClientWebrtcEarlyMediaUnkeyedByCallee(t *testing.T) {
	invites := []struct {
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
			if med.MediaSession() != nil {
				return nil, errors.New("media of a 183 that was not keyed was set up before the ACK")
			}
			return med, d.Ack(ctx)
		}},
	}
	for _, tc := range invites {
		t.Run(tc.name, func(t *testing.T) {
			calleeEnded := make(chan error, 1)
			firstRead := make(chan struct{})
			d := newWebrtcCallPair(t, func(_ context.Context, callee *DialogServerSession) {
				unkeyedWebrtcCallee(callee, firstRead, calleeEnded)
			})
			var readOnce sync.Once
			onResponse := func(res *sip.Response) error {
				if res.StatusCode == sip.StatusSessionInProgress {
					readOnce.Do(func() { close(firstRead) })
				}
				return nil
			}

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()
			start := time.Now()
			med, err := tc.invite(ctx, d, InviteWebrtcOptions{
				OnResponse:           onResponse,
				EarlyMediaDetect:     true,
				EarlyMediaKeyTimeout: 300 * time.Millisecond,
				WebrtcConfig:         loopbackWebrtcConfig(),
			})
			require.NoError(t, err, "unkeyed early media is not reported, and the call is answered")
			assert.Less(t, time.Since(start), 20*time.Second)
			require.NotNil(t, med.MediaSession(), "the ACK set the media up")

			writer, err := med.AudioWriter()
			require.NoError(t, err)
			_, err = writer.Write(bytes.Repeat([]byte{0x31}, int(media.CodecAudioUlaw.SampleTimestamp())))
			require.NoError(t, err)
			require.NoError(t, d.Hangup(t.Context()))
			select {
			case err := <-calleeEnded:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("the callee's call did not end")
			}
		})
	}
}

// TestDialogClientWebrtcEarlyMediaFinalAnswerApplied pins that the session
// description of the 200 after WebRTC early media is applied to the running
// media: a 200 whose description asks for another DTLS association, which
// the transport in place cannot take without an ICE restart, is acknowledged,
// the call ended with a BYE, and WaitAnswerWebrtc returns why.
func TestDialogClientWebrtcEarlyMediaFinalAnswerApplied(t *testing.T) {
	_, otherOffer := newWebrtcPeerOffer(t)
	otherFingerprint := sdpLineWithPrefix(t, otherOffer, "a=fingerprint:")
	calleeSetUp := make(chan struct{})
	calleeEnded := make(chan error, 1)
	answerNow := make(chan struct{})
	byeRead := make(chan struct{})
	d := newWebrtcCallPair(t, func(_ context.Context, callee *DialogServerSession) {
		ctx := callee.Context()
		conf, err := prepareWebrtcConfig(loopbackWebrtcConfig())
		if err != nil {
			calleeEnded <- err
			return
		}
		sess := &media.MediaSessionWebrtc{Codecs: callee.mediaConf.Codecs}
		if err := sess.Init(ctx, conf); err != nil {
			calleeEnded <- err
			return
		}
		defer func() { _ = sess.Close() }()
		calleeEnded <- func() error {
			if err := sess.RemoteSDP(ctx, callee.InviteRequest.Body(), false); err != nil {
				return err
			}
			answer, err := sess.LocalSDP(ctx, true)
			if err != nil {
				return err
			}
			contentType := sip.NewHeader("Content-Type", "application/sdp")
			if err := callee.DialogServerSession.Respond(ctx, sip.StatusSessionInProgress, "Session Progress", answer, contentType); err != nil {
				return err
			}
			if err := sess.Finalize(ctx); err != nil {
				return err
			}
			close(calleeSetUp)
			select {
			case <-answerNow:
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			final := withFingerprint(answer, otherFingerprint)
			if err := callee.DialogServerSession.Respond(ctx, sip.StatusOK, "OK", final, contentType); err != nil {
				return err
			}
			<-ctx.Done()
			close(byeRead)
			return nil
		}()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	med, err := d.InviteWebrtc(ctx, InviteWebrtcOptions{EarlyMediaDetect: true, WebrtcConfig: loopbackWebrtcConfig()})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	select {
	case <-calleeSetUp:
	case err := <-calleeEnded:
		t.Fatalf("the callee's early media was not set up: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the callee's early media was not set up")
	}
	sess := med.MediaSession()
	require.NotNil(t, sess)

	close(answerNow)
	err = d.WaitAnswerWebrtc(ctx, med, sipgo.AnswerOptions{})
	require.ErrorIs(t, err, media.ErrWebRTCICERestartRequired)
	assert.Equal(t, sip.DialogStateEnded, d.LoadState(), "the call is ended")
	assert.Same(t, sess, med.MediaSession(), "an answer that cannot be applied leaves the media as it was")
	select {
	case <-byeRead:
	case <-time.After(10 * time.Second):
		t.Fatal("the callee's call did not end with the BYE")
	}
	select {
	case err := <-calleeEnded:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the callee did not return")
	}
}

// withFingerprint returns body with its first a=fingerprint line replaced by
// line.
func withFingerprint(body []byte, line string) []byte {
	lines := strings.Split(string(body), "\r\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "a=fingerprint:") {
			lines[i] = line
			break
		}
	}
	return []byte(strings.Join(lines, "\r\n"))
}

// TestDialogServerProgressMediaWebrtcRefusesWrites pins that early media the
// caller has not keyed sends nothing: its transport refuses RTP before the
// DTLS session is keyed.
func TestDialogServerProgressMediaWebrtcRefusesWrites(t *testing.T) {
	_, offer := newWebrtcPeerOffer(t)
	d, inviteTx, _ := newProgressMediaWebrtcDialog(t, offer)
	med, err := d.ProgressMediaWebrtc(t.Context(), progressWebrtcOptions(100*time.Millisecond))
	require.ErrorIs(t, err, ErrEarlyMediaNotKeyed)
	requireResponse(t, inviteTx, sip.StatusSessionInProgress)

	sess, ok := med.firstExchangeSession()
	require.True(t, ok, "the transport of the 183 is kept for the answer")
	require.ErrorIs(t, sess.WriteRTP(&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: media.CodecAudioUlaw.PayloadType, SequenceNumber: 1, Timestamp: 160, SSRC: 0x5eed},
		Payload: bytes.Repeat([]byte{0x5a}, 160),
	}), media.ErrDTLSNotKeyed)
}
