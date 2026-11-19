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
