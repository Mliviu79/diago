// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// firstResponseTx is a byeServerTx that hands its first response to a channel
// and drops the rest, such as the retransmissions of a 2xx waiting for its ACK.
type firstResponseTx struct {
	*byeServerTx
	once  sync.Once
	first chan *sip.Response
}

func (tx *firstResponseTx) Respond(_ context.Context, res *sip.Response) error {
	tx.once.Do(func() { tx.first <- res })
	return nil
}

// newLateOfferDialog builds an inbound dialog over a fake transaction whose
// INVITE carries no offer, and whose media config is conf. Requests the dialog
// sends, such as a BYE, are answered 200 and handed to the returned channel.
func newLateOfferDialog(t *testing.T, conf MediaConfig) (*DialogServerSession, *firstResponseTx, <-chan *sip.Request) {
	t.Helper()
	inviteTx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
	d, sent := newRecordingServerDialog(t, inviteTx, nil, conf)
	return d, inviteTx, sent
}

// lateAnswer is what AnswerLate returned.
type lateAnswer struct {
	med *DialogMedia
	err error
}

// startAnswerLate runs AnswerLate on another goroutine and returns the first
// response it sends, with the channel AnswerLate reports on.
func startAnswerLate(t *testing.T, d *DialogServerSession, inviteTx *firstResponseTx) (*sip.Response, <-chan lateAnswer) {
	t.Helper()
	answered := make(chan lateAnswer, 1)
	go func() {
		med, err := d.AnswerLate()
		answered <- lateAnswer{med: med, err: err}
	}()
	select {
	case res := <-inviteTx.first:
		return res, answered
	case <-time.After(5 * time.Second):
		t.Fatal("no response was sent")
	}
	return nil, nil
}

// answerLate runs AnswerLate on another goroutine and returns the offer its
// 2xx carries, with the channel AnswerLate reports on.
func answerLate(t *testing.T, d *DialogServerSession, inviteTx *firstResponseTx) ([]byte, <-chan lateAnswer) {
	t.Helper()
	res, answered := startAnswerLate(t, d, inviteTx)
	require.Equal(t, sip.StatusOK, res.StatusCode)
	return res.Body(), answered
}

// awaitAnswerLate waits for AnswerLate to report.
func awaitAnswerLate(t *testing.T, answered <-chan lateAnswer) lateAnswer {
	t.Helper()
	select {
	case answer := <-answered:
		return answer
	case <-time.After(5 * time.Second):
		t.Fatal("AnswerLate did not return")
	}
	return lateAnswer{}
}

// newLateAck builds the ACK to our 2xx, carrying the answer to its offer.
func newLateAck(d *DialogServerSession, answer []byte) *sip.Request {
	ack := sip.NewRequest(sip.ACK, d.InviteRequest.Contact().Address)
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: d.InviteRequest.CSeq().SeqNo, MethodName: sip.ACK})
	ack.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	ack.SetBody(answer)
	return ack
}

// TestDialogServerLateOfferAckIsAnswer pins that the SDP in the ACK is applied
// as the answer to the offer in our 2xx (RFC 3261 section 13.2.1), not as an
// offer, and to the media session that made the offer. The role decides, among
// other things, whether SDPCodecPreferLocalOrder re-ranks the codecs: it
// applies to the answerer only, and the order of an answer is the peer's
// decision. Applied as an offer, our local order would replace the one the
// peer answered with.
func TestDialogServerLateOfferAckIsAnswer(t *testing.T) {
	prev := media.SDPCodecPreferLocalOrder
	media.SDPCodecPreferLocalOrder = 1
	t.Cleanup(func() { media.SDPCodecPreferLocalOrder = prev })

	d, inviteTx, _ := newLateOfferDialog(t, MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	})
	offer, answered := answerLate(t, d, inviteTx)

	// The peer prefers PCMA and answers with it first.
	peer, err := media.NewMediaSession(net.IPv4(127, 0, 0, 1), 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	peer.Codecs = []media.Codec{media.CodecAudioAlaw, media.CodecAudioUlaw}
	require.NoError(t, peer.RemoteSDP(offer, false))
	answer := peer.LocalSDP(true)
	require.Contains(t, string(answer), "RTP/AVP 8 0")

	require.NoError(t, d.ReadAck(newLateAck(d, answer), newByeServerTx()))
	late := awaitAnswerLate(t, answered)
	require.NoError(t, late.err)

	sess := late.med.MediaSession()
	require.NotEmpty(t, sess.CommonCodecs())
	assert.Equal(t, media.CodecAudioAlaw.Name, sess.CommonCodecs()[0].Name, "the order the peer answered with is the negotiation result")
	assert.Equal(t, peer.Laddr.Port, sess.Raddr.Port, "the media goes where the answer says")
	assert.Contains(t, string(offer), "m=audio "+strconv.Itoa(sess.Laddr.Port)+" ", "the answer was applied to the session that made the offer")
}

// TestDialogServerLateOfferAckCopyAnswersNothing pins that only the ACK
// confirming the dialog answers the offer in our 2xx. A peer sends that ACK
// again for each retransmission of the 2xx it receives; the copy carries the
// same session description, and it is no new answer: the media stays as the
// first ACK left it and the call goes on.
func TestDialogServerLateOfferAckCopyAnswersNothing(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	})
	offer, answered := answerLate(t, d, inviteTx)
	answer := peerAnswer(t, offer)

	require.NoError(t, d.ReadAck(newLateAck(d, answer), newByeServerTx()))
	late := awaitAnswerLate(t, answered)
	require.NoError(t, late.err)
	sess := late.med.MediaSession()

	require.NoError(t, d.ReadAck(newLateAck(d, answer), newByeServerTx()))
	assert.Same(t, sess, late.med.MediaSession(), "a copy of the ACK must not replace the media session")
	assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
	select {
	case req := <-sent:
		t.Errorf("the dialog sent %s on a copy of its ACK", req.Method)
	default:
	}
}

// peerAnswer is the answer a peer puts in its ACK to offer.
func peerAnswer(t *testing.T, offer []byte) []byte {
	t.Helper()
	peer, err := media.NewMediaSession(net.IPv4(127, 0, 0, 1), 0)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	peer.Codecs = []media.Codec{media.CodecAudioAlaw}
	require.NoError(t, peer.RemoteSDP(offer, false))
	return peer.LocalSDP(true)
}

// newLateOfferTimerDialog builds the dialog newLateOfferDialog does, with the
// session-timer policy enabled and inviteHeaders on its INVITE. When the answer
// started a session timer, its cleanup ends the call and waits for the timer.
func newLateOfferTimerDialog(t *testing.T, inviteHeaders ...sip.Header) (*DialogServerSession, *firstResponseTx, <-chan *sip.Request) {
	t.Helper()
	d, inviteTx, sent := newLateOfferDialog(t, MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	})
	d.sessionTimers = SessionTimerPolicy{Enabled: true, PreferUASRefresher: true}
	for _, h := range inviteHeaders {
		d.InviteRequest.AppendHeader(h)
	}
	t.Cleanup(func() {
		timer := d.timer.Load()
		if timer == nil {
			return
		}
		// t.Context() is done by the time cleanups run.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = d.Hangup(ctx)
		select {
		case <-timer.done:
		case <-time.After(5 * time.Second):
			t.Error("the session timer did not return after the call ended")
		}
	})
	return d, inviteTx, sent
}

// TestDialogServerLateOfferNegotiatesTheSessionTimer pins that the answer to an
// INVITE without an offer negotiates the session timer as the answer to one
// with an offer does (RFC 4028 section 9): the 2xx carrying our offer carries
// the timer headers, a Session-Expires below our Min-SE is refused 422 with
// Min-SE and no 2xx (section 6), and an INVITE with no Session-Expires gets no
// timer headers. An INVITE requiring timer is accepted (RFC 3261 section
// 8.2.2.3) on the understanding that every answer does this.
func TestDialogServerLateOfferNegotiatesTheSessionTimer(t *testing.T) {
	t.Run("the caller refreshes", func(t *testing.T) {
		d, inviteTx, _ := newLateOfferTimerDialog(t,
			sip.NewHeader("Supported", "timer"),
			sip.NewHeader("Session-Expires", "1800;refresher=uac"),
		)
		res, answered := startAnswerLate(t, d, inviteTx)

		require.Equal(t, sip.StatusOK, res.StatusCode)
		assert.Contains(t, string(res.Body()), "m=audio ", "the 2xx carries our offer")
		assert.Equal(t, "1800;refresher=uac", headerValue(res, "Session-Expires"))
		assert.Equal(t, "timer", headerValue(res, "Require"), "the 2xx naming the caller the refresher requires timer")
		assert.Equal(t, "timer", headerValue(res, "Supported"))

		assert.NoError(t, d.ReadAck(newLateAck(d, peerAnswer(t, res.Body())), newByeServerTx()))
		assert.NoError(t, awaitAnswerLate(t, answered).err)
		assert.NotNil(t, d.timer.Load(), "the 2xx started the session timer")
	})

	t.Run("a Session-Expires below our Min-SE", func(t *testing.T) {
		d, inviteTx, _ := newLateOfferTimerDialog(t,
			sip.NewHeader("Supported", "timer"),
			sip.NewHeader("Session-Expires", "60"),
		)
		res, answered := startAnswerLate(t, d, inviteTx)

		assert.Equal(t, statusSessionIntervalTooSmall, res.StatusCode, "the first response is the 422, no 2xx")
		assert.Equal(t, "90", headerValue(res, "Min-SE"), "the 422 carries our Min-SE")
		// The INVITE transaction ends, as it does once the peer acknowledges
		// the 422.
		close(inviteTx.done)
		select {
		case answer := <-answered:
			assert.ErrorIs(t, answer.err, errSessionIntervalTooSmall, "AnswerLate returns what Answer returns")
		case <-time.After(5 * time.Second):
			t.Error("AnswerLate did not return after the 422")
		}
	})

	t.Run("no Session-Expires", func(t *testing.T) {
		d, inviteTx, _ := newLateOfferTimerDialog(t, sip.NewHeader("Supported", "timer"))
		res, answered := startAnswerLate(t, d, inviteTx)

		require.Equal(t, sip.StatusOK, res.StatusCode)
		assert.Empty(t, headerValue(res, "Session-Expires"))
		assert.Empty(t, headerValue(res, "Require"))

		assert.NoError(t, d.ReadAck(newLateAck(d, peerAnswer(t, res.Body())), newByeServerTx()))
		assert.NoError(t, awaitAnswerLate(t, answered).err)
		assert.Nil(t, d.timer.Load(), "no session timer runs")
	})
}

// TestDialogServerLateOfferAckWithoutAnswerEndsTheCall pins RFC 3261 section
// 13.2.1: when the 2xx carries the offer, "the answer MUST be in the
// acknowledgement for that message". An ACK bringing none leaves the call with
// no remote media to send to. An ACK cannot be refused, so the call is ended
// with our BYE, as it is for an answer we cannot apply, and AnswerLate reports
// the missing answer.
func TestDialogServerLateOfferAckWithoutAnswerEndsTheCall(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	})
	res, answered := startAnswerLate(t, d, inviteTx)
	require.Equal(t, sip.StatusOK, res.StatusCode)

	ack := sip.NewRequest(sip.ACK, d.InviteRequest.Contact().Address)
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: d.InviteRequest.CSeq().SeqNo, MethodName: sip.ACK})
	acked := make(chan error, 1)
	go func() { acked <- d.ReadAck(ack, newByeServerTx()) }()

	assert.ErrorIs(t, awaitAnswerLate(t, answered).err, errAckCarriesNoAnswer)
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
