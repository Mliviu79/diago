// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"crypto/tls"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
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

func (tx *firstResponseTx) Respond(res *sip.Response) error {
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

// answerLate runs AnswerLate on another goroutine and returns the offer its
// 2xx carries, with the channel AnswerLate reports on.
func answerLate(t *testing.T, d *DialogServerSession, inviteTx *firstResponseTx) ([]byte, <-chan error) {
	t.Helper()
	answered := make(chan error, 1)
	go func() { answered <- d.AnswerLate() }()
	select {
	case res := <-inviteTx.first:
		require.Equal(t, sip.StatusOK, res.StatusCode)
		return res.Body(), answered
	case <-time.After(5 * time.Second):
		t.Fatal("no 2xx was sent")
	}
	return nil, nil
}

// newLateAck builds the ACK to our 2xx, carrying the answer to its offer.
func newLateAck(d *DialogServerSession, answer []byte) *sip.Request {
	ack := sip.NewRequest(sip.ACK, d.InviteRequest.Contact().Address)
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: d.InviteRequest.CSeq().SeqNo, MethodName: sip.ACK})
	ack.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	ack.SetBody(answer)
	return ack
}

// TestDialogServerLateOfferHandshakeFailure is a late offer over DTLS-SRTP
// whose answer, in the ACK, carries a fingerprint that the certificate the
// peer then presents does not match, as it would be from an attacker on the
// media path. The handshake fails, which leaves the call with no SRTP keys.
// Before, ReadAck swallowed that failure and the call went on, with its media
// unencrypted. Now ReadAck and AnswerLate return the error, and the call is
// ended with a BYE: an ACK cannot be refused, and RFC 5763 section 5 has the
// media session torn down on a fingerprint mismatch.
func TestDialogServerLateOfferHandshakeFailure(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, MediaConfig{
		Codecs:     []media.Codec{media.CodecAudioUlaw},
		DTLSConfig: &media.DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
		secureRTP:  media.SecureRTPModeDTLS,
		bindIP:     net.IPv4(127, 0, 0, 1),
	})
	offer, answered := answerLate(t, d, inviteTx)

	peer := &media.MediaSession{
		Codecs:    []media.Codec{media.CodecAudioUlaw},
		Mode:      sdp.ModeSendrecv,
		SecureRTP: media.SecureRTPModeDTLS,
		DTLSConf:  media.DTLSConfig{Certificates: []tls.Certificate{testdata.ClientCertificate()}},
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
	}
	require.NoError(t, peer.Init())
	t.Cleanup(func() { _ = peer.Close() })
	require.NoError(t, peer.RemoteSDP(offer))
	answer := string(peer.LocalSDP())

	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	otherFP := iceSDPLine(t, newDTLSMediaSession(t, other).LocalSDP(), "a=fingerprint:")
	answer = strings.Replace(answer, iceSDPLine(t, []byte(answer), "a=fingerprint:"), otherFP, 1)

	// The peer answered actpass with active, so it starts the handshake.
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Finalize() }()

	acked := make(chan error, 1)
	go func() { acked <- d.ReadAck(newLateAck(d, []byte(answer)), newByeServerTx()) }()
	select {
	case err := <-acked:
		assert.Error(t, err, "a failed handshake must not be swallowed")
	case <-time.After(20 * time.Second):
		t.Fatal("ReadAck did not return")
	}
	select {
	case err := <-answered:
		assert.Error(t, err, "an answer whose media could not be keyed is not a success")
	case <-time.After(20 * time.Second):
		t.Fatal("AnswerLate did not return")
	}
	select {
	case req := <-sent:
		assert.Equal(t, sip.BYE, req.Method, "the call must be ended")
	case <-time.After(20 * time.Second):
		t.Fatal("no BYE was sent")
	}
	select {
	case <-peerDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the peer's handshake did not return")
	}
}

// TestDialogServerLateOfferAckIsAnswer pins that the SDP in the ACK is applied
// as the answer to the offer in our 2xx (RFC 3261 section 13.2.1), not as an
// offer. The role decides, among other things, whether
// SDPCodecPreferLocalOrder re-ranks the codecs: it applies to the answerer
// only, and the order of an answer is the peer's decision. Before, the ACK's
// SDP was applied as an offer, so our local order replaced the one the peer
// answered with.
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
	require.NoError(t, peer.RemoteSDP(offer))
	answer := peer.LocalSDP()
	require.Contains(t, string(answer), "RTP/AVP 8 0")

	require.NoError(t, d.ReadAck(newLateAck(d, answer), newByeServerTx()))
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("AnswerLate did not return")
	}

	sess := d.MediaSession()
	assert.True(t, sess.RemoteSDPIsAnswer, "the ACK's SDP answers our offer")
	require.NotEmpty(t, sess.CommonCodecs())
	assert.Equal(t, "PCMA", sess.CommonCodecs()[0].Name, "the order the peer answered with is the negotiation result")
}
