// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWebrtcPeer returns a WebRTC media session on loopback, offering PCMU,
// as the peer of a call under test. It is closed with the test.
func newWebrtcPeer(t *testing.T) *media.MediaSessionWebrtc {
	t.Helper()
	conf, err := prepareWebrtcConfig(loopbackWebrtcConfig())
	require.NoError(t, err)
	peer := &media.MediaSessionWebrtc{Codecs: []media.Codec{media.CodecAudioUlaw}}
	require.NoError(t, peer.Init(t.Context(), conf))
	t.Cleanup(func() { _ = peer.Close() })
	return peer
}

// newWebrtcPeerOffer returns a WebRTC peer and the offer it puts in its
// INVITE.
func newWebrtcPeerOffer(t *testing.T) (*media.MediaSessionWebrtc, []byte) {
	t.Helper()
	peer := newWebrtcPeer(t)
	offer, err := peer.LocalSDP(t.Context(), false)
	require.NoError(t, err)
	return peer, offer
}

// webrtcAnswerConfig is the media config of the inbound dialogs of the WebRTC
// answer tests: PCMU, the ICE socket bound on loopback.
func webrtcAnswerConfig() MediaConfig {
	return MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	}
}

// webrtcAnswerOptions are the answer options of the WebRTC answer tests.
func webrtcAnswerOptions() AnswerWebrtcOptions {
	return AnswerWebrtcOptions{WebrtcConfig: loopbackWebrtcConfig()}
}

// finalizeWebrtcPeer runs the peer's side of ICE and DTLS on another
// goroutine and returns the channel it reports on.
func finalizeWebrtcPeer(t *testing.T, peer *media.MediaSessionWebrtc) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- peer.Finalize(t.Context()) }()
	return done
}

// requireWebrtcPeerFinalized requires that the peer's side of ICE and DTLS
// completed.
func requireWebrtcPeerFinalized(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(20 * time.Second):
		t.Fatal("the peer's ICE and DTLS did not complete")
	}
}

// webrtcContextAnswer is what a WebRTC answer whose wait ends with a ctx
// returned.
type webrtcContextAnswer struct {
	med *DialogWebrtc
	err error
}

// startWebrtcAnswer runs answer on another goroutine with t's context and
// returns the first response it sends, with the channel it reports on.
func startWebrtcAnswer(t *testing.T, inviteTx *firstResponseTx, answer func(ctx context.Context) (*DialogWebrtc, error)) (*sip.Response, <-chan webrtcContextAnswer) {
	t.Helper()
	answered := make(chan webrtcContextAnswer, 1)
	go func() {
		med, err := answer(t.Context())
		answered <- webrtcContextAnswer{med: med, err: err}
	}()
	return awaitFirstResponse(t, inviteTx), answered
}

// awaitWebrtcAnswer waits for a WebRTC answer to report.
func awaitWebrtcAnswer(t *testing.T, answered <-chan webrtcContextAnswer) webrtcContextAnswer {
	t.Helper()
	select {
	case answer := <-answered:
		return answer
	case <-time.After(20 * time.Second):
		t.Fatal("the answer did not return")
	}
	return webrtcContextAnswer{}
}

// requireWebrtcMediaRunsWith requires that med and peer, both completed,
// carry audio to each other.
func requireWebrtcMediaRunsWith(t *testing.T, med *DialogWebrtc, peer *media.MediaSessionWebrtc) {
	t.Helper()
	sess := med.MediaSession()
	require.NotNil(t, sess, "the media has its transport set up")
	require.NotNil(t, med.RTPSession())
	assert.Error(t, med.RTPSession().MonitorBackground(), "the RTCP monitor is already running")

	peerRTP := media.NewRTPSessionWebrtc(peer)
	require.NoError(t, sess.StopRTP(1, 5*time.Second))
	require.NoError(t, peer.StopRTP(1, 5*time.Second))
	payload := bytes.Repeat([]byte{0x31}, int(media.CodecAudioUlaw.SampleTimestamp()))
	received := make([]byte, media.RTPBufSize)

	writer, err := med.AudioWriter()
	require.NoError(t, err)
	n, err := writer.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	peerReader := media.NewRTPPacketReader(peerRTP, peer.Codec())
	n, err = peerReader.Read(received)
	require.NoError(t, err)
	require.Equal(t, payload, received[:n], "the peer reads what the media writes")

	peerWriter := media.NewRTPPacketWriter(peerRTP, peer.Codec())
	n, err = peerWriter.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	reader, err := med.AudioReader()
	require.NoError(t, err)
	n, err = reader.Read(received)
	require.NoError(t, err)
	require.Equal(t, payload, received[:n], "the media reads what the peer writes")
}

// TestDialogServerAnswerWebrtcContextAnswers pins the call AnswerWebrtcContext
// hands back once the ACK is read: the media the WebRTC offer in the INVITE and
// the answer in the 2xx agreed on, completed by the reading of the ACK, on a
// confirmed dialog.
func TestDialogServerAnswerWebrtcContextAnswers(t *testing.T) {
	peer, offer := newWebrtcPeerOffer(t)
	inviteTx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
	d, sent := newRecordingServerDialog(t, inviteTx, offer, webrtcAnswerConfig())

	res, answered := startWebrtcAnswer(t, inviteTx, func(ctx context.Context) (*DialogWebrtc, error) {
		return d.AnswerWebrtcContext(ctx, webrtcAnswerOptions())
	})
	require.Equal(t, sip.StatusOK, res.StatusCode)
	require.NoError(t, peer.RemoteSDP(t.Context(), res.Body(), true), "the 2xx carries the answer to the offer")
	peerDone := finalizeWebrtcPeer(t, peer)

	readAck(t, d)
	answer := awaitWebrtcAnswer(t, answered)
	require.NoError(t, answer.err)
	require.NotNil(t, answer.med)
	requireWebrtcPeerFinalized(t, peerDone)
	assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
	requireWebrtcMediaRunsWith(t, answer.med, peer)

	// A copy of the ACK completes nothing again and the call goes on.
	require.NoError(t, d.ReadAck(t.Context(), newLateAck(d, nil), newByeServerTx()))
	assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
	select {
	case req := <-sent:
		t.Errorf("the dialog sent %s on an answered call", req.Method)
	default:
	}

	// The session closes the media it was answered with.
	mediaClosed := false
	answer.med.OnClose(func() error {
		mediaClosed = true
		return nil
	})
	require.NoError(t, d.Close())
	assert.True(t, mediaClosed, "the media was not closed with the session")
}

// TestDialogServerAnswerWebrtcLateContextAnswers pins the call
// AnswerWebrtcLateContext hands back: its 2xx carries our WebRTC offer, the
// ACK the answer (RFC 3261 section 13.2.1), applied to the transport that made
// the offer, and the media is completed by the reading of that ACK.
func TestDialogServerAnswerWebrtcLateContextAnswers(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, webrtcAnswerConfig())
	res, answered := startWebrtcAnswer(t, inviteTx, func(ctx context.Context) (*DialogWebrtc, error) {
		return d.AnswerWebrtcLateContext(ctx, webrtcAnswerOptions())
	})
	require.Equal(t, sip.StatusOK, res.StatusCode)

	peer := newWebrtcPeer(t)
	require.NoError(t, peer.RemoteSDP(t.Context(), res.Body(), false))
	peerAnswer, err := peer.LocalSDP(t.Context(), true)
	require.NoError(t, err)
	peerDone := finalizeWebrtcPeer(t, peer)
	require.NoError(t, d.ReadAck(t.Context(), newLateAck(d, peerAnswer), newByeServerTx()))

	answer := awaitWebrtcAnswer(t, answered)
	require.NoError(t, answer.err)
	require.NotNil(t, answer.med)
	requireWebrtcPeerFinalized(t, peerDone)
	assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
	requireWebrtcMediaRunsWith(t, answer.med, peer)
	select {
	case req := <-sent:
		t.Errorf("the dialog sent %s on an answered call", req.Method)
	default:
	}
}

// TestDialogServerAnswerWebrtcContextLateAck is AnswerWebrtcContext cut while
// its 2xx awaits the ACK, whose ACK arrives after the cut: it returns at the
// cut, the dialog reads the late ACK, completes the media with it, and then
// ends the call nobody waits for with a BYE.
func TestDialogServerAnswerWebrtcContextLateAck(t *testing.T) {
	peer, offer := newWebrtcPeerOffer(t)
	inviteTx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
	d, sent := newRecordingServerDialog(t, inviteTx, offer, webrtcAnswerConfig())
	res := cutWhileAwaitingAck(t, d, inviteTx, func(ctx context.Context) error {
		_, err := d.AnswerWebrtcContext(ctx, webrtcAnswerOptions())
		return err
	})

	requireNoRequest(t, sent, 2*sip.T1, "a BYE before the ACK")
	require.Equal(t, sip.DialogStateEstablished, d.LoadState())

	require.NoError(t, peer.RemoteSDP(t.Context(), res.Body(), true))
	peerDone := finalizeWebrtcPeer(t, peer)
	readAck(t, d)
	requireWebrtcPeerFinalized(t, peerDone)
	requireByeEndsDialog(t, d, sent, 5*time.Second)
	requireCloseJoins(t, d)
}

// TestDialogServerAnswerWebrtcLateContextCut is AnswerWebrtcLateContext cut
// while its 2xx awaits the ACK that carries the answer: it returns at the cut,
// and the late ACK is still read, its answer applied and the media completed,
// and followed by the dialog's BYE.
func TestDialogServerAnswerWebrtcLateContextCut(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, webrtcAnswerConfig())
	res := cutWhileAwaitingAck(t, d, inviteTx, func(ctx context.Context) error {
		_, err := d.AnswerWebrtcLateContext(ctx, webrtcAnswerOptions())
		return err
	})

	requireNoRequest(t, sent, 2*sip.T1, "a BYE before the ACK")

	peer := newWebrtcPeer(t)
	require.NoError(t, peer.RemoteSDP(t.Context(), res.Body(), false))
	peerAnswer, err := peer.LocalSDP(t.Context(), true)
	require.NoError(t, err)
	peerDone := finalizeWebrtcPeer(t, peer)
	require.NoError(t, d.ReadAck(t.Context(), newLateAck(d, peerAnswer), newByeServerTx()))
	requireWebrtcPeerFinalized(t, peerDone)
	requireByeEndsDialog(t, d, sent, 5*time.Second)
	requireCloseJoins(t, d)
}

// TestDialogServerAnswerWebrtcLateContextCutAckWithoutAnswer is
// AnswerWebrtcLateContext cut while its 2xx awaits the ACK, whose ACK then
// brings no answer to the offer in that 2xx: the dialog itself ends the call,
// with one BYE, once the ACK has confirmed it.
func TestDialogServerAnswerWebrtcLateContextCutAckWithoutAnswer(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, webrtcAnswerConfig())
	cutWhileAwaitingAck(t, d, inviteTx, func(ctx context.Context) error {
		_, err := d.AnswerWebrtcLateContext(ctx, webrtcAnswerOptions())
		return err
	})

	assert.ErrorIs(t, d.ReadAck(t.Context(), newLateAck(d, nil), newByeServerTx()), errAckCarriesNoAnswer)

	requireByeEndsDialog(t, d, sent, 5*time.Second)
	requireCloseJoins(t, d)
	select {
	case req := <-sent:
		t.Errorf("the dialog sent a second request, %s, to end the call", req.Method)
	default:
	}
}

// TestDialogServerAnswerWebrtcLateContextAckWithoutAnswerEndsTheCall pins RFC
// 3261 section 13.2.1 for AnswerWebrtcLateContext: an ACK that brings no
// answer to the offer in our 2xx ends the call with our BYE, and the answer
// reports the missing answer and closes the media it made.
func TestDialogServerAnswerWebrtcLateContextAckWithoutAnswerEndsTheCall(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, webrtcAnswerConfig())
	res, answered := startWebrtcAnswer(t, inviteTx, func(ctx context.Context) (*DialogWebrtc, error) {
		return d.AnswerWebrtcLateContext(ctx, webrtcAnswerOptions())
	})
	require.Equal(t, sip.StatusOK, res.StatusCode)

	acked := make(chan error, 1)
	go func() { acked <- d.ReadAck(t.Context(), newLateAck(d, nil), newByeServerTx()) }()

	answer := awaitWebrtcAnswer(t, answered)
	assert.ErrorIs(t, answer.err, errAckCarriesNoAnswer)
	assert.Nil(t, answer.med)
	select {
	case req := <-sent:
		assert.Equal(t, sip.BYE, req.Method, "the call is ended")
	case <-time.After(5 * time.Second):
		t.Error("no BYE was sent")
	}
	select {
	case err := <-acked:
		assert.ErrorIs(t, err, errAckCarriesNoAnswer)
	case <-time.After(5 * time.Second):
		t.Error("ReadAck did not return")
	}
}

// TestDialogServerAnswerWebrtcContextCutWhileRefusing is a WebRTC answer that
// refuses the INVITE 422, its Session-Expires being below our Min-SE (RFC 4028
// section 6), cut while that final awaits its ACK: the answer returns at the
// cut, and the INVITE's transaction ends the refusal on its own.
func TestDialogServerAnswerWebrtcContextCutWhileRefusing(t *testing.T) {
	d, inviteTx, _ := newLateOfferTimerDialog(t,
		sip.NewHeader("Supported", "timer"),
		sip.NewHeader("Session-Expires", "60"),
	)
	ctx, cut := context.WithCancelCause(t.Context())
	defer cut(nil)
	answered := make(chan error, 1)
	go func() {
		_, err := d.AnswerWebrtcLateContext(ctx, webrtcAnswerOptions())
		answered <- err
	}()
	res := awaitFirstResponse(t, inviteTx)
	require.Equal(t, statusSessionIntervalTooSmall, res.StatusCode)
	assert.Equal(t, "90", headerValue(res, "Min-SE"), "the 422 carries our Min-SE")

	cutAt := time.Now()
	cut(errAnswerCut)
	select {
	case err := <-answered:
		require.ErrorIs(t, err, errAnswerCut)
		require.Less(t, time.Since(cutAt), sip.T1, "the answer must return at its context's end")
	case <-time.After(5 * time.Second):
		t.Fatal("the answer outlived its context while its 422 awaited the ACK")
	}

	// The INVITE transaction ends, as it does once the peer acknowledges
	// the 422.
	close(inviteTx.done)
	requireCloseJoins(t, d)
}

// TestDialogServerAnswerWebrtcNegotiatesTheSessionTimer pins that the WebRTC
// answers negotiate the session timer as AnswerContext does (RFC 4028 section
// 9): the 2xx carries the timer headers, and the session timer starts with it.
func TestDialogServerAnswerWebrtcNegotiatesTheSessionTimer(t *testing.T) {
	d, inviteTx, _ := newLateOfferTimerDialog(t,
		sip.NewHeader("Supported", "timer"),
		sip.NewHeader("Session-Expires", "1800;refresher=uac"),
	)
	res, answered := startWebrtcAnswer(t, inviteTx, func(ctx context.Context) (*DialogWebrtc, error) {
		return d.AnswerWebrtcLateContext(ctx, webrtcAnswerOptions())
	})
	require.Equal(t, sip.StatusOK, res.StatusCode)
	assert.Equal(t, "1800;refresher=uac", headerValue(res, "Session-Expires"))
	assert.Equal(t, "timer", headerValue(res, "Require"), "the 2xx naming the caller the refresher requires timer")

	peer := newWebrtcPeer(t)
	require.NoError(t, peer.RemoteSDP(t.Context(), res.Body(), false))
	peerAnswer, err := peer.LocalSDP(t.Context(), true)
	require.NoError(t, err)
	peerDone := finalizeWebrtcPeer(t, peer)
	require.NoError(t, d.ReadAck(t.Context(), newLateAck(d, peerAnswer), newByeServerTx()))
	answer := awaitWebrtcAnswer(t, answered)
	require.NoError(t, answer.err)
	requireWebrtcPeerFinalized(t, peerDone)
	assert.NotNil(t, d.timer.Load(), "the 2xx started the session timer")
}

// TestDialogServerAnswerWebrtcLateContextHandshakeFailure is a WebRTC late
// offer whose answer, in the ACK, carries a fingerprint that the certificate
// the peer then presents does not match. The handshake fails, which leaves the
// call with no SRTP keys: ReadAck and the answer return the error, and the call
// is ended with a BYE, as RFC 5763 section 5 has the media session torn down
// on a fingerprint mismatch.
func TestDialogServerAnswerWebrtcLateContextHandshakeFailure(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, webrtcAnswerConfig())
	res, answered := startWebrtcAnswer(t, inviteTx, func(ctx context.Context) (*DialogWebrtc, error) {
		return d.AnswerWebrtcLateContext(ctx, webrtcAnswerOptions())
	})
	require.Equal(t, sip.StatusOK, res.StatusCode)

	peer := newWebrtcPeer(t)
	require.NoError(t, peer.RemoteSDP(t.Context(), res.Body(), false))
	peerAnswer, err := peer.LocalSDP(t.Context(), true)
	require.NoError(t, err)
	_, otherOffer := newWebrtcPeerOffer(t)
	otherFP := sdpLineWithPrefix(t, otherOffer, "a=fingerprint:")
	peerAnswer = []byte(strings.Replace(string(peerAnswer), sdpLineWithPrefix(t, peerAnswer, "a=fingerprint:"), otherFP, 1))

	peerCtx, cancelPeer := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancelPeer()
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Finalize(peerCtx) }()

	acked := make(chan error, 1)
	go func() { acked <- d.ReadAck(t.Context(), newLateAck(d, peerAnswer), newByeServerTx()) }()
	select {
	case err := <-acked:
		assert.Error(t, err, "a failed handshake must not be swallowed")
	case <-time.After(30 * time.Second):
		t.Fatal("ReadAck did not return")
	}
	answer := awaitWebrtcAnswer(t, answered)
	assert.Error(t, answer.err, "an answer whose media could not be keyed is not a success")
	assert.Nil(t, answer.med)
	select {
	case req := <-sent:
		assert.Equal(t, sip.BYE, req.Method, "the call must be ended")
	case <-time.After(20 * time.Second):
		t.Fatal("no BYE was sent")
	}
	cancelPeer()
	select {
	case <-peerDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the peer's handshake did not return")
	}
}
