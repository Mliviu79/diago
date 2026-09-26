// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// newDTLSDiago builds a diago on a UDP transport at port whose media is
// DTLS-SRTP without ICE.
func newDTLSDiago(t *testing.T, port int, cert tls.Certificate) *Diago {
	t.Helper()
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	return NewDiago(ua,
		WithTransport(Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  port,
			MediaSRTP: media.SecureRTPModeDTLS,
			MediaDTLSConf: media.DTLSConfig{
				Certificates: []tls.Certificate{cert},
			},
		}),
		WithMediaConfig(MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}),
	)
}

// dtlsCall is a DTLS-SRTP call between two diago instances, answered and
// acknowledged, and hung up when the test ends.
type dtlsCall struct {
	caller *DialogClientSession
	callee *DialogServerSession
	// calleeUpdated receives once the callee has installed the media of a
	// re-INVITE it answered.
	calleeUpdated chan struct{}
}

// newDTLSCall places a DTLS-SRTP call from one diago to another. The callee's
// handler answers and holds the call until the test ends.
func newDTLSCall(t *testing.T, calleePort int, callerPort int) dtlsCall {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	callee := newDTLSDiago(t, calleePort, testdata.ServerCertificate())
	answered := make(chan *DialogServerSession, 1)
	answerErr := make(chan error, 1)
	calleeUpdated := make(chan struct{}, 1)
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	require.NoError(t, callee.ServeBackground(ctx, func(d *DialogServerSession) {
		defer close(handlerDone)
		if err := d.AnswerOptions(AnswerOptions{OnMediaUpdate: func(*DialogMedia) {
			select {
			case calleeUpdated <- struct{}{}:
			default:
			}
		}}); err != nil {
			answerErr <- err
			return
		}
		answered <- d
		select {
		case <-release:
		case <-d.Context().Done():
		}
	}))

	caller := newDTLSDiago(t, callerPort, testdata.ClientCertificate())
	// It serves only the requests inside its call, such as a re-INVITE.
	require.NoError(t, caller.ServeBackground(ctx, nil))
	d, err := caller.Invite(ctx, sip.Uri{User: "callee", Host: "127.0.0.1", Port: calleePort}, InviteOptions{})
	require.NoError(t, err)

	call := dtlsCall{caller: d, calleeUpdated: calleeUpdated}
	select {
	case call.callee = <-answered:
	case err := <-answerErr:
		t.Fatalf("the callee did not answer: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the callee did not answer")
	}

	t.Cleanup(func() {
		hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer hcancel()
		_ = d.Hangup(hctx)
		close(release)
		select {
		case <-handlerDone:
		case <-time.After(10 * time.Second):
			t.Error("the callee's handler did not return")
		}
		_ = d.Close()
		cancel()
	})
	return call
}

// requireEncryptedMedia writes one RTP packet through the installed media
// session of from and requires that it reaches to encrypted: the payload is not
// on the wire, and to decrypts it. A second packet carries the decryption
// check, since the first is read raw off the socket.
func requireEncryptedMedia(t *testing.T, from, to *DialogMedia, seq uint16) {
	t.Helper()
	fromSess := from.MediaSession()
	toSess := to.MediaSession()

	payload := bytes.Repeat([]byte{0x5a, byte(seq)}, 80)
	write := func(seq uint16) {
		require.NoError(t, fromSess.WriteRTP(&rtp.Packet{
			Header: rtp.Header{
				Version:        2,
				PayloadType:    media.CodecAudioUlaw.PayloadType,
				SequenceNumber: seq,
				Timestamp:      uint32(seq) * 160,
				SSRC:           0x5eed0000 + uint32(seq),
			},
			Payload: payload,
		}))
	}

	write(seq)
	buf := make([]byte, media.RTPBufSize)
	n, err := toSess.ReadRTPRawDeadline(buf, time.Now().Add(5*time.Second))
	require.NoError(t, err, "no media reached the peer")
	require.False(t, bytes.Contains(buf[:n], payload), "the media went out unencrypted")

	write(seq + 1)
	require.NoError(t, toSess.StopRTP(1, 5*time.Second))
	got := rtp.Packet{}
	_, err = toSess.ReadRTP(buf, &got)
	require.NoError(t, err, "the peer could not decrypt the media")
	require.NoError(t, toSess.StartRTP(1))
	require.Equal(t, seq+1, got.SequenceNumber)
	require.Equal(t, payload, got.Payload)
}

// requireCallEncrypted checks the media of call in both directions.
func requireCallEncrypted(t *testing.T, call dtlsCall, seq uint16) {
	t.Helper()
	requireEncryptedMedia(t, &call.caller.DialogMedia, &call.callee.DialogMedia, seq)
	requireEncryptedMedia(t, &call.callee.DialogMedia, &call.caller.DialogMedia, seq+10)
}

// TestIntegrationDialogDTLSReinviteKeepsSRTP is a re-INVITE on a DTLS-SRTP call
// without ICE, sent by the caller and then by the callee. Neither changes what
// RFC 8842 section 3.1 ties the association to, so both sides carry on over it
// and the media stays encrypted both ways. Before, the side that forked lost
// its keys at every re-INVITE and sent its media unencrypted.
func TestIntegrationDialogDTLSReinviteKeepsSRTP(t *testing.T) {
	call := newDTLSCall(t, 16451, 16452)
	requireCallEncrypted(t, call, 100)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	callerSess := call.caller.MediaSession()
	require.NoError(t, call.caller.reInviteMediaSession(ctx, call.caller.MediaSession().Fork()))
	require.NotSame(t, callerSess, call.caller.MediaSession(), "the re-INVITE must install a fork")
	requireCallEncrypted(t, call, 200)

	calleeSess := call.callee.MediaSession()
	require.NoError(t, call.callee.reInviteMediaSession(ctx, call.callee.MediaSession().Fork()))
	require.NotSame(t, calleeSess, call.callee.MediaSession(), "the re-INVITE must install a fork")
	requireCallEncrypted(t, call, 300)
}

// TestIntegrationDialogDTLSReinviteNewAssociation is a re-INVITE by which the
// caller moves its media to a new transport and a new certificate, which asks
// for a new DTLS association (RFC 8842 section 3.1). The callee answers it on
// a transport of its own (section 5.1), both run the handshake, and the media
// continues encrypted under the new keys.
func TestIntegrationDialogDTLSReinviteNewAssociation(t *testing.T) {
	call := newDTLSCall(t, 16453, 16454)
	requireCallEncrypted(t, call, 100)
	calleePort := call.callee.MediaSession().Laddr.Port

	cert, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	fork := call.caller.MediaSession().Fork()
	fork.DTLSConf.Certificates = []tls.Certificate{cert}
	fork.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	require.NoError(t, fork.Init())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	require.NoError(t, call.caller.reInviteMediaSession(ctx, fork))
	require.Same(t, fork, call.caller.MediaSession())
	// The callee installs its side once its own handshake is done.
	select {
	case <-call.calleeUpdated:
	case <-time.After(10 * time.Second):
		t.Fatal("the callee did not install the new association")
	}
	require.NotEqual(t, calleePort, call.callee.MediaSession().Laddr.Port, "the callee must answer on a transport of its own")
	requireCallEncrypted(t, call, 200)
}

// TestIntegrationDialogDTLSEarlyMediaAnswerKeepsSRTP is a DTLS-SRTP call
// without ICE answered after early media. The caller keys the early media
// from the answer in the 183, and the 200 repeats that answer, so it
// continues the association: acknowledging it runs no second handshake, and
// the media after the answer stays encrypted. Before, the caller's fork
// applying the 200 armed a handshake the callee never ran, so Ack waited for
// it forever.
func TestIntegrationDialogDTLSEarlyMediaAnswerKeepsSRTP(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	callee := newDTLSDiago(t, 16455, testdata.ServerCertificate())
	answered := make(chan *DialogServerSession, 1)
	answerErr := make(chan error, 1)
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	require.NoError(t, callee.ServeBackground(ctx, func(d *DialogServerSession) {
		defer close(handlerDone)
		if err := d.ProgressMedia(); err != nil {
			answerErr <- err
			return
		}
		if err := d.Answer(); err != nil {
			answerErr <- err
			return
		}
		answered <- d
		select {
		case <-release:
		case <-d.Context().Done():
		}
	}))

	caller := newDTLSDiago(t, 16456, testdata.ClientCertificate())
	require.NoError(t, caller.ServeBackground(ctx, nil))
	dialog, err := caller.NewDialog(sip.Uri{User: "callee", Host: "127.0.0.1", Port: 16455}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer hcancel()
		_ = dialog.Hangup(hctx)
		close(release)
		select {
		case <-handlerDone:
		case <-time.After(10 * time.Second):
			t.Error("the callee's handler did not return")
		}
		_ = dialog.Close()
	})

	err = dialog.Invite(ctx, InviteClientOptions{EarlyMediaDetect: true})
	require.ErrorIs(t, err, ErrClientEarlyMedia)

	acked := make(chan error, 1)
	go func() {
		if err := dialog.WaitAnswer(ctx, sipgo.AnswerOptions{}); err != nil {
			acked <- err
			return
		}
		acked <- dialog.Ack(ctx)
	}()
	select {
	case err := <-acked:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the caller did not acknowledge the answer")
	}

	var d *DialogServerSession
	select {
	case d = <-answered:
	case err := <-answerErr:
		t.Fatalf("the callee did not answer: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the callee did not answer")
	}
	requireEncryptedMedia(t, &dialog.DialogMedia, &d.DialogMedia, 100)
	requireEncryptedMedia(t, &d.DialogMedia, &dialog.DialogMedia, 110)
}

// newEstablishedDTLSMedia negotiates and keys a DTLS-SRTP session without ICE
// between an offerer and an answerer on the loopback, and closes both when the
// test ends.
func newEstablishedDTLSMedia(t *testing.T) (offerer, answerer *media.MediaSession) {
	t.Helper()
	offerer = newDTLSMediaSession(t, testdata.ClientCertificate())
	answerer = newDTLSMediaSession(t, testdata.ServerCertificate())

	require.NoError(t, answerer.RemoteSDP(offerer.LocalSDP()))
	offerer.RemoteSDPIsAnswer = true
	require.NoError(t, offerer.RemoteSDP(answerer.LocalSDP()))
	finalizeMediaBoth(t, offerer, answerer, true)
	return offerer, answerer
}

func newDTLSMediaSession(t *testing.T, cert tls.Certificate) *media.MediaSession {
	t.Helper()
	s := &media.MediaSession{
		Codecs:    []media.Codec{media.CodecAudioUlaw},
		Mode:      sdp.ModeSendrecv,
		SecureRTP: media.SecureRTPModeDTLS,
		DTLSConf:  media.DTLSConfig{Certificates: []tls.Certificate{cert}},
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
	}
	require.NoError(t, s.Init())
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// finalizeMediaBoth runs Finalize on both sessions at once, and requires they
// succeed, or when succeed is false that they return.
func finalizeMediaBoth(t *testing.T, a, b *media.MediaSession, succeed bool) {
	t.Helper()
	errCh := make(chan error, 2)
	go func() { errCh <- a.Finalize() }()
	go func() { errCh <- b.Finalize() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			if succeed {
				require.NoError(t, err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("DTLS negotiation did not return")
		}
	}
}

// TestDialogDTLSNewAssociationFailedHandshake is a re-INVITE asking for a new
// DTLS association whose peer then presents a certificate matching none of the
// fingerprints of its offer. The offer was answered 200 before the handshake,
// so the update reports a failure after its answer, for the dialog to end the
// call (RFC 5763 section 5), and the fork is never installed: its sockets are
// released, and the current session is left as it was.
func TestDialogDTLSNewAssociationFailedHandshake(t *testing.T) {
	_, answerer := newEstablishedDTLSMedia(t)
	d := newICEDialogMedia(t, answerer)

	// The call moves to another peer, whose offer carries the fingerprint of a
	// certificate other than the one it presents.
	peer := newDTLSMediaSession(t, testdata.ClientCertificate())
	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	otherSess := newDTLSMediaSession(t, other)
	offer := string(peer.LocalSDP())
	otherFP := iceSDPLine(t, otherSess.LocalSDP(), "a=fingerprint:")
	offer = strings.Replace(offer, iceSDPLine(t, []byte(offer), "a=fingerprint:"), otherFP, 1)

	tx := newRespondedServerTx()
	contact := &sip.ContactHeader{Address: sip.Uri{User: "us", Host: "127.0.0.1"}}
	updated := make(chan error, 1)
	go func() {
		updated <- d.handleMediaUpdate(context.Background(), newReInvite(t, []byte(offer)), tx, contact)
	}()

	// The answer goes out before the handshake, which the peer then runs.
	var answer []byte
	select {
	case res := <-tx.responded:
		require.Equal(t, sip.StatusOK, res.StatusCode, "reason: %s", res.Reason)
		answer = res.Body()
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not answered")
	}

	peer.RemoteSDPIsAnswer = true
	require.NoError(t, peer.RemoteSDP(answer))
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Finalize() }()

	select {
	case err := <-updated:
		require.Error(t, err)
		require.True(t, errors.Is(err, errMediaUpdateAfterAnswer), "want errMediaUpdateAfterAnswer, got %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the media update did not return")
	}
	select {
	case <-peerDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the peer's handshake did not return")
	}

	require.Same(t, answerer, d.MediaSession(), "a failed association must leave the session unchanged")

	// The fork answered on a port of its own, which it must have released.
	var forkPort int
	_, err = fmt.Sscanf(iceSDPLine(t, answer, "m=audio "), "m=audio %d", &forkPort)
	require.NoError(t, err)
	require.NotEqual(t, answerer.Laddr.Port, forkPort)
	reuse, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: forkPort})
	require.NoError(t, err, "the fork's socket must be released")
	require.NoError(t, reuse.Close())
}

// newRecordingServerDialog builds an inbound dialog over the fake transaction
// inviteTx, whose INVITE carries body, and whose media config is conf.
// Requests the dialog sends, such as a BYE, are answered 200 and handed to the
// returned channel.
func newRecordingServerDialog(t *testing.T, inviteTx sip.ServerTransaction, body []byte, conf MediaConfig) (*DialogServerSession, <-chan *sip.Request) {
	t.Helper()

	sent := make(chan *sip.Request, 4)
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	require.NoError(t, err)
	client.TxRequester = &clientTxRequester{onRequest: func(req *sip.Request) *sip.Response {
		sent <- req
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	}}

	recipient := sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}
	caller := sip.Uri{User: "bob", Host: "127.0.0.2", Port: 5060}
	invite := sip.NewRequest(sip.INVITE, recipient)
	invite.AppendHeader(&sip.ContactHeader{Address: caller})
	fromParams := sip.NewParams()
	fromParams.Add("tag", "caller-tag")
	invite.AppendHeader(&sip.FromHeader{Address: caller, Params: fromParams})
	invite.AppendHeader(&sip.ToHeader{Address: recipient, Params: sip.NewParams()})
	invite.AppendHeader(sip.NewHeader("Call-ID", "recording-dialog-test-call-id"))
	invite.AppendHeader(&sip.CSeqHeader{SeqNo: 100, MethodName: sip.INVITE})
	if body != nil {
		invite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		invite.SetBody(body)
	}

	dialogUA := &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: recipient}}
	sess, err := dialogUA.ReadInvite(invite, inviteTx)
	require.NoError(t, err)

	d := &DialogServerSession{DialogServerSession: sess, mediaConf: conf}
	t.Cleanup(func() { _ = d.DialogMedia.Close() })
	return d, sent
}

// TestDialogServerReInviteNewAssociationFailureHangsUp is a re-INVITE asking
// for a new DTLS association whose peer then presents a certificate matching
// none of the fingerprints of its offer. The offer was answered 200 before the
// handshake, so the peer has moved to the new association, which has no keys:
// the dialog ends the call with a BYE, as RFC 5763 section 5 has the media
// session torn down on a fingerprint mismatch.
func TestDialogServerReInviteNewAssociationFailureHangsUp(t *testing.T) {
	d, sent := newRecordingServerDialog(t, newByeServerTx(), nil, MediaConfig{})
	res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
	res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
	d.InviteResponse = res
	confirm(t, d)

	_, answerer := newEstablishedDTLSMedia(t)
	rtpSess := media.NewRTPSession(answerer)
	d.mu.Lock()
	d.initRTPSessionUnsafe(answerer, rtpSess)
	d.mu.Unlock()
	require.NoError(t, rtpSess.MonitorBackground())

	// The call moves to another peer, whose offer carries the fingerprint of a
	// certificate other than the one it presents.
	peer := newDTLSMediaSession(t, testdata.ClientCertificate())
	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	offer := string(peer.LocalSDP())
	offer = strings.Replace(offer, iceSDPLine(t, []byte(offer), "a=fingerprint:"), iceSDPLine(t, newDTLSMediaSession(t, other).LocalSDP(), "a=fingerprint:"), 1)

	req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody([]byte(offer))
	tx := newRespondedServerTx()
	handled := make(chan error, 1)
	go func() { handled <- d.handleReInvite(req, tx) }()

	var answer []byte
	select {
	case res := <-tx.responded:
		require.Equal(t, sip.StatusOK, res.StatusCode, "reason: %s", res.Reason)
		answer = res.Body()
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not answered")
	}
	peer.RemoteSDPIsAnswer = true
	require.NoError(t, peer.RemoteSDP(answer))
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Finalize() }()

	select {
	case err := <-handled:
		require.True(t, errors.Is(err, errMediaUpdateAfterAnswer), "want errMediaUpdateAfterAnswer, got %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the re-INVITE handler did not return")
	}
	select {
	case req := <-sent:
		require.Equal(t, sip.BYE, req.Method, "the call must be ended")
	case <-time.After(5 * time.Second):
		t.Fatal("no BYE was sent")
	}
	require.Equal(t, sip.DialogStateEnded, d.LoadState())
	select {
	case <-peerDone:
	case <-time.After(20 * time.Second):
		t.Fatal("the peer's handshake did not return")
	}
}

// newNewAssociationReInviteDialog returns a confirmed inbound dialog whose
// DTLS-SRTP media is keyed, the session installed, and a re-INVITE from the
// peer whose offer asks for a new DTLS association: it comes from another
// session with a certificate of its own, which never takes part in the
// handshake.
func newNewAssociationReInviteDialog(t *testing.T) (*DialogServerSession, *media.MediaSession, *sip.Request) {
	t.Helper()
	d, _ := newRecordingServerDialog(t, newByeServerTx(), nil, MediaConfig{})
	res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
	res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
	d.InviteResponse = res
	confirm(t, d)

	_, answerer := newEstablishedDTLSMedia(t)
	rtpSess := media.NewRTPSession(answerer)
	d.mu.Lock()
	d.initRTPSessionUnsafe(answerer, rtpSess)
	d.mu.Unlock()
	require.NoError(t, rtpSess.MonitorBackground())

	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	peer := newDTLSMediaSession(t, other)
	req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(peer.LocalSDP())
	return d, answerer, req
}

// requirePortReleased requires that the RTP port of the m= line of sdp can be
// bound again.
func requirePortReleased(t *testing.T, body []byte) {
	t.Helper()
	var port int
	_, err := fmt.Sscanf(iceSDPLine(t, body, "m=audio "), "m=audio %d", &port)
	require.NoError(t, err)
	reuse, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port})
	require.NoError(t, err, "the fork's socket must be released")
	require.NoError(t, reuse.Close())
}

// TestDialogByeEndsNewAssociationHandshake is a BYE that arrives while the
// handshake of a new DTLS association, asked for by a re-INVITE already
// answered 200, is still running. The peer never runs it, so it would take
// until DTLSHandshakeTimeout. The BYE is answered at once, the end of the
// dialog ends the handshake, and the fork is not installed: its sockets are
// released and the session is left as it was.
func TestDialogByeEndsNewAssociationHandshake(t *testing.T) {
	d, answerer, req := newNewAssociationReInviteDialog(t)

	tx := newRespondedServerTx()
	handled := make(chan error, 1)
	go func() { handled <- d.handleReInvite(req, tx) }()

	var answer []byte
	select {
	case res := <-tx.responded:
		require.Equal(t, sip.StatusOK, res.StatusCode, "reason: %s", res.Reason)
		answer = res.Body()
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not answered")
	}

	byeTx := newByeServerTx()
	byeRead := make(chan error, 1)
	go func() { byeRead <- d.ReadBye(newBye(t, d, req.CSeq().SeqNo+1), byeTx) }()
	select {
	case err := <-byeRead:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the BYE waited for the handshake")
	}
	require.Len(t, byeTx.responses, 1)
	require.Equal(t, sip.StatusOK, byeTx.responses[0].StatusCode)

	select {
	case err := <-handled:
		require.NoError(t, err, "the end of the dialog is no failure of the re-INVITE")
	case <-time.After(5 * time.Second):
		t.Fatal("the end of the dialog did not end the handshake")
	}
	require.Same(t, answerer, d.MediaSession(), "nothing may be installed on an ended dialog")
	requirePortReleased(t, answer)
}

// TestDialogByeBeforeNewAssociationAnswer is a BYE that ends the dialog after a
// re-INVITE asking for a new DTLS association was found live and before its
// answer. The BYE answers it 487 (RFC 3261 section 15.1.2), so the update
// sends nothing, runs no handshake and closes the fork it built.
func TestDialogByeBeforeNewAssociationAnswer(t *testing.T) {
	d, answerer, req := newNewAssociationReInviteDialog(t)

	tx := newByeServerTx()
	answered, err := d.beginPeerReInvite(&d.Dialog, req, tx)
	require.NoError(t, err)
	require.False(t, answered)
	require.NoError(t, d.ReadBye(newBye(t, d, req.CSeq().SeqNo+1), newByeServerTx()))
	require.Len(t, tx.responses, 1)
	require.Equal(t, sip.StatusRequestTerminated, tx.responses[0].StatusCode)

	d.mu.Lock()
	msess, err := d.sdpReInviteUnsafe(req.Body())
	require.NoError(t, err)
	require.True(t, msess.FinalizePending())
	d.mediaUpdating = true
	d.mu.Unlock()

	contact := &sip.ContactHeader{Address: d.InviteRequest.Recipient}
	require.NoError(t, d.answerNewAssociation(d.Context(), req, tx, contact, msess))
	require.Len(t, tx.responses, 1, "the re-INVITE got a second final response")
	require.Same(t, answerer, d.MediaSession())
	requirePortReleased(t, msess.LocalSDP())
}

// sentPacketConn records every datagram a media session writes through it, so
// a test sees what went on the wire from that session.
type sentPacketConn struct {
	net.PacketConn
	mu   sync.Mutex
	sent [][]byte
}

func (c *sentPacketConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	c.mu.Lock()
	c.sent = append(c.sent, bytes.Clone(p))
	c.mu.Unlock()
	return c.PacketConn.WriteTo(p, addr)
}

func (c *sentPacketConn) datagrams() [][]byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.sent)
}

// isDTLSRecord reports whether a datagram is a DTLS record rather than RTP or
// RTCP, by its first byte (RFC 7983 section 7).
func isDTLSRecord(datagram []byte) bool {
	return len(datagram) > 0 && datagram[0] >= 20 && datagram[0] <= 63
}

// newSentDTLSMediaSession builds the DTLS-SRTP media session a callee on the
// loopback builds, over an RTP socket that records what it sends. A handler
// installs it with InitMediaSession before it answers.
func newSentDTLSMediaSession(t *testing.T) (*media.MediaSession, *sentPacketConn) {
	t.Helper()
	rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)

	sent := &sentPacketConn{PacketConn: rtpConn}
	sess := &media.MediaSession{
		Codecs:    []media.Codec{media.CodecAudioUlaw},
		SecureRTP: media.SecureRTPModeDTLS,
		DTLSConf:  media.DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
	}
	sess.InitWithListeners(sent, rtcpConn, &net.UDPAddr{})
	// InitWithListeners takes the local address from the RTCP socket, and the
	// SDP has to name the RTP one.
	sess.Laddr = *rtpConn.LocalAddr().(*net.UDPAddr)
	t.Cleanup(func() { _ = sess.Close() })
	return sess, sent
}

// TestIntegrationDialogDTLSEarlyMediaIgnoredByCaller is a DTLS-SRTP call
// without ICE whose caller ignores the answer in the 183, as RFC 3960 lets it,
// and takes part in the handshake only after the 200: diago's own Invite
// without EarlyMediaDetect. ProgressMedia gives up on early media after its
// KeyTimeout and returns ErrEarlyMediaNotKeyed, instead of waiting out
// DTLSHandshakeTimeout. Media written meanwhile is refused and nothing but DTLS
// goes on the wire. The call stays answerable: Answer sends the 200 and the
// handshake ProgressMedia started completes with the caller, so the media is
// encrypted both ways.
func TestIntegrationDialogDTLSEarlyMediaIgnoredByCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, sent := newSentDTLSMediaSession(t)
	callee := newDTLSDiago(t, 16457, testdata.ServerCertificate())

	type progressResult struct {
		err  error
		took time.Duration
	}
	type answerResult struct {
		d *DialogServerSession
		// writeErr is a write made after ProgressMedia gave up and before the
		// answer, and sent is what the session had put on the wire when the
		// answer returned.
		writeErr error
		sent     [][]byte
		err      error
	}
	progressed := make(chan progressResult, 1)
	answered := make(chan answerResult, 1)
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	require.NoError(t, callee.ServeBackground(ctx, func(d *DialogServerSession) {
		defer close(handlerDone)
		d.InitMediaSession(sess, nil, nil)
		start := time.Now()
		err := d.ProgressMediaOptions(ProgressMediaOptions{KeyTimeout: time.Second})
		progressed <- progressResult{err: err, took: time.Since(start)}

		r := answerResult{d: d}
		r.writeErr = d.MediaSession().WriteRTP(&rtp.Packet{
			Header:  rtp.Header{Version: 2, PayloadType: media.CodecAudioUlaw.PayloadType, SequenceNumber: 1, Timestamp: 160, SSRC: 0x5eed},
			Payload: bytes.Repeat([]byte{0x5a}, 160),
		})
		r.err = d.Answer()
		r.sent = sent.datagrams()
		answered <- r
		if r.err != nil {
			return
		}
		select {
		case <-release:
		case <-d.Context().Done():
		}
	}))

	caller := newDTLSDiago(t, 16458, testdata.ClientCertificate())
	require.NoError(t, caller.ServeBackground(ctx, nil))
	type inviteResult struct {
		d   *DialogClientSession
		err error
	}
	invited := make(chan inviteResult, 1)
	go func() {
		ictx, icancel := context.WithTimeout(ctx, 30*time.Second)
		defer icancel()
		d, err := caller.Invite(ictx, sip.Uri{User: "callee", Host: "127.0.0.1", Port: 16457}, InviteOptions{})
		invited <- inviteResult{d: d, err: err}
	}()

	select {
	case r := <-progressed:
		require.ErrorIs(t, r.err, ErrEarlyMediaNotKeyed)
		require.Less(t, r.took, 5*time.Second, "ProgressMedia waited past its KeyTimeout")
	case <-time.After(10 * time.Second):
		t.Fatal("ProgressMedia did not give up on early media within its KeyTimeout")
	}

	var answer answerResult
	select {
	case answer = <-answered:
	case <-time.After(20 * time.Second):
		t.Fatal("the callee did not answer")
	}
	require.NoError(t, answer.err, "the call must stay answerable")
	require.ErrorIs(t, answer.writeErr, media.ErrDTLSNotKeyed)
	require.NotEmpty(t, answer.sent, "the handshake was never started")
	for i, datagram := range answer.sent {
		require.True(t, isDTLSRecord(datagram), "datagram %d sent before the media was keyed is not DTLS: first byte %d", i, datagram[0])
	}

	var call inviteResult
	select {
	case call = <-invited:
	case <-time.After(20 * time.Second):
		t.Fatal("the caller's Invite did not return")
	}
	require.NoError(t, call.err)
	t.Cleanup(func() {
		hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer hcancel()
		_ = call.d.Hangup(hctx)
		close(release)
		select {
		case <-handlerDone:
		case <-time.After(10 * time.Second):
			t.Error("the callee's handler did not return")
		}
		_ = call.d.Close()
	})

	requireEncryptedMedia(t, &call.d.DialogMedia, &answer.d.DialogMedia, 100)
	requireEncryptedMedia(t, &answer.d.DialogMedia, &call.d.DialogMedia, 110)
}

// TestIntegrationDialogDTLSEarlyMediaKeyedByCaller is a DTLS-SRTP call without
// ICE whose caller takes the answer in the 183 and runs the handshake at once,
// diago's own Invite with EarlyMediaDetect. ProgressMedia returns once the
// early media is keyed, well within its KeyTimeout, the early media is
// encrypted both ways, and after Answer the media stays encrypted.
func TestIntegrationDialogDTLSEarlyMediaKeyedByCaller(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sess, _ := newSentDTLSMediaSession(t)
	callee := newDTLSDiago(t, 16459, testdata.ServerCertificate())

	type progressResult struct {
		d   *DialogServerSession
		err error
	}
	progressed := make(chan progressResult, 1)
	answerNow := make(chan struct{})
	answered := make(chan error, 1)
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	require.NoError(t, callee.ServeBackground(ctx, func(d *DialogServerSession) {
		defer close(handlerDone)
		d.InitMediaSession(sess, nil, nil)
		err := d.ProgressMediaOptions(ProgressMediaOptions{KeyTimeout: 5 * time.Second})
		progressed <- progressResult{d: d, err: err}
		if err != nil {
			return
		}
		select {
		case <-answerNow:
		case <-d.Context().Done():
			return
		}
		err = d.Answer()
		answered <- err
		if err != nil {
			return
		}
		select {
		case <-release:
		case <-d.Context().Done():
		}
	}))

	caller := newDTLSDiago(t, 16460, testdata.ClientCertificate())
	require.NoError(t, caller.ServeBackground(ctx, nil))
	dialog, err := caller.NewDialog(sip.Uri{User: "callee", Host: "127.0.0.1", Port: 16459}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() {
		hctx, hcancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer hcancel()
		_ = dialog.Hangup(hctx)
		close(release)
		select {
		case <-handlerDone:
		case <-time.After(10 * time.Second):
			t.Error("the callee's handler did not return")
		}
		_ = dialog.Close()
	})

	ictx, icancel := context.WithTimeout(ctx, 20*time.Second)
	defer icancel()
	err = dialog.Invite(ictx, InviteClientOptions{EarlyMediaDetect: true})
	require.ErrorIs(t, err, ErrClientEarlyMedia)

	var d *DialogServerSession
	select {
	case r := <-progressed:
		require.NoError(t, r.err, "early media the caller keyed must be reported keyed")
		d = r.d
	case <-time.After(10 * time.Second):
		t.Fatal("ProgressMedia did not return")
	}
	requireEncryptedMedia(t, &d.DialogMedia, &dialog.DialogMedia, 100)
	requireEncryptedMedia(t, &dialog.DialogMedia, &d.DialogMedia, 110)

	close(answerNow)
	acked := make(chan error, 1)
	go func() {
		if err := dialog.WaitAnswer(ictx, sipgo.AnswerOptions{}); err != nil {
			acked <- err
			return
		}
		acked <- dialog.Ack(ictx)
	}()
	select {
	case err := <-acked:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the caller did not acknowledge the answer")
	}
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the callee did not answer")
	}
	requireEncryptedMedia(t, &dialog.DialogMedia, &d.DialogMedia, 200)
	requireEncryptedMedia(t, &d.DialogMedia, &dialog.DialogMedia, 210)
}
