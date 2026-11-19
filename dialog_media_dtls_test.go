// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
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
	caller      *DialogClientSession
	callerMedia *DialogMedia
	callee      *DialogServerSession
	calleeMedia *DialogMedia
	// calleeUpdated receives once the callee has installed the media of a
	// re-INVITE it answered.
	calleeUpdated chan struct{}
}

// newDTLSCall places a DTLS-SRTP call from one diago to another. The callee's
// handler answers and holds the call until the test ends.
func newDTLSCall(t *testing.T, calleePort int, callerPort int) dtlsCall {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())

	type answeredCall struct {
		d   *DialogServerSession
		med *DialogMedia
	}
	callee := newDTLSDiago(t, calleePort, testdata.ServerCertificate())
	answered := make(chan answeredCall, 1)
	answerErr := make(chan error, 1)
	calleeUpdated := make(chan struct{}, 1)
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	require.NoError(t, serveBackground(t, callee, ctx, func(_ context.Context, d *DialogServerSession) {
		defer close(handlerDone)
		med, err := d.AnswerOptions(t.Context(), AnswerOptions{OnMediaUpdate: func(*DialogMedia) {
			select {
			case calleeUpdated <- struct{}{}:
			default:
			}
		}})
		if err != nil {
			answerErr <- err
			return
		}
		answered <- answeredCall{d: d, med: med}
		select {
		case <-release:
		case <-d.Context().Done():
		}
	}))

	caller := newDTLSDiago(t, callerPort, testdata.ClientCertificate())
	// It serves only the requests inside its call, such as a re-INVITE.
	require.NoError(t, serveBackground(t, caller, ctx, nil))
	d, med, err := caller.Invite(ctx, sip.Uri{User: "callee", Host: "127.0.0.1", Port: calleePort}, InviteOptions{})
	require.NoError(t, err)

	call := dtlsCall{caller: d, callerMedia: med, calleeUpdated: calleeUpdated}
	select {
	case a := <-answered:
		call.callee, call.calleeMedia = a.d, a.med
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
	requireEncryptedMedia(t, call.callerMedia, call.calleeMedia, seq)
	requireEncryptedMedia(t, call.calleeMedia, call.callerMedia, seq+10)
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

	callerSess := call.callerMedia.MediaSession()
	require.NoError(t, call.caller.ReInvite(ctx))
	require.NotSame(t, callerSess, call.callerMedia.MediaSession(), "the re-INVITE must install a fork")
	requireCallEncrypted(t, call, 200)

	calleeSess := call.calleeMedia.MediaSession()
	require.NoError(t, call.callee.ReInvite(ctx))
	require.NotSame(t, calleeSess, call.calleeMedia.MediaSession(), "the re-INVITE must install a fork")
	requireCallEncrypted(t, call, 300)
}

// forkOfferingMedia is the media of a call, made to offer from fork in a
// re-INVITE of ours: a fork of its media session the test has moved to a new
// transport or given another certificate, as an application does that moves
// its media. The answer is applied to that fork, and the fork installed, by
// the media itself.
type forkOfferingMedia struct {
	*DialogMedia
	fork *media.MediaSession
}

func (m forkOfferingMedia) onLocalSDP(ctx context.Context, answered bool, mode string) ([]byte, error) {
	if answered {
		return m.DialogMedia.onLocalSDP(ctx, answered, mode)
	}
	m.mu.Lock()
	m.pendingMediaSession = m.fork
	m.mu.Unlock()
	return m.fork.LocalSDP(false), nil
}

// requirePortFree requires that the UDP port of addr can be bound again,
// which it can once the socket that held it is closed.
func requirePortFree(t *testing.T, addr net.UDPAddr, msg string) {
	t.Helper()
	reuse, err := net.ListenUDP("udp", &net.UDPAddr{IP: addr.IP, Port: addr.Port})
	require.NoError(t, err, msg)
	require.NoError(t, reuse.Close())
}

// TestIntegrationDialogDTLSReinviteNewAssociation is a re-INVITE by which the
// caller moves its media to a new transport and a new certificate, which asks
// for a new DTLS association (RFC 8842 section 3.1). The callee answers it on
// a transport of its own (section 5.1), both run the handshake, and the media
// continues encrypted under the new keys. Each side closes the sockets of the
// session it replaced.
func TestIntegrationDialogDTLSReinviteNewAssociation(t *testing.T) {
	call := newDTLSCall(t, 16453, 16454)
	requireCallEncrypted(t, call, 100)
	calleeLaddr := call.calleeMedia.MediaSession().Laddr
	callerLaddr := call.callerMedia.MediaSession().Laddr

	cert, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	fork := call.callerMedia.MediaSession().Fork()
	fork.DTLSConf.Certificates = []tls.Certificate{cert}
	fork.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	require.NoError(t, fork.Init())

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	offering := forkOfferingMedia{DialogMedia: call.callerMedia, fork: fork}
	require.NoError(t, call.caller.ownReInvite(offering, "").run(ctx))
	require.Same(t, fork, call.callerMedia.MediaSession())
	// The callee installs its side once its own handshake is done.
	select {
	case <-call.calleeUpdated:
	case <-time.After(10 * time.Second):
		t.Fatal("the callee did not install the new association")
	}
	require.NotEqual(t, calleeLaddr.Port, call.calleeMedia.MediaSession().Laddr.Port, "the callee must answer on a transport of its own")
	requireCallEncrypted(t, call, 200)
	requirePortFree(t, calleeLaddr, "the callee left the sockets it replaced open")
	requirePortFree(t, callerLaddr, "the caller left the sockets it replaced open")
}

// newEstablishedDTLSMedia negotiates and keys a DTLS-SRTP session without ICE
// between an offerer and an answerer on the loopback, and closes both when the
// test ends.
func newEstablishedDTLSMedia(t *testing.T) (offerer, answerer *media.MediaSession) {
	t.Helper()
	offerer = newDTLSMediaSession(t, testdata.ClientCertificate())
	answerer = newDTLSMediaSession(t, testdata.ServerCertificate())

	require.NoError(t, answerer.RemoteSDP(offerer.LocalSDP(false), false))
	require.NoError(t, offerer.RemoteSDP(answerer.LocalSDP(true), true))
	finalizeMediaBoth(t, offerer, answerer, true)
	return offerer, answerer
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

// offerWithOtherFingerprint returns the offer of peer, carrying the
// fingerprint of a certificate other than the one peer presents.
func offerWithOtherFingerprint(t *testing.T, peer *media.MediaSession) []byte {
	t.Helper()
	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	offer := string(peer.LocalSDP(false))
	otherFP := sdpLineWithPrefix(t, newDTLSMediaSession(t, other).LocalSDP(false), "a=fingerprint:")
	return []byte(strings.Replace(offer, sdpLineWithPrefix(t, []byte(offer), "a=fingerprint:"), otherFP, 1))
}

// requirePortReleased requires that the RTP port of the m= line of body can be
// bound again.
func requirePortReleased(t *testing.T, body []byte) {
	t.Helper()
	var port int
	_, err := fmt.Sscanf(sdpLineWithPrefix(t, body, "m=audio "), "m=audio %d", &port)
	require.NoError(t, err)
	requirePortFree(t, net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port}, "the fork's socket must be released")
}

// TestDialogDTLSNewAssociationKeyedBeforeInstalled is a re-INVITE of the
// peer's asking for a new DTLS association, as the media of the call sees it.
// The offer is answered from a fork on a transport of its own, which has no
// SRTP keys until its handshake has run, so the session in place carries the
// media until then: the fork is installed, and the application told, only by
// the commit that follows the answer, once it is keyed. The sockets of the
// session it replaces are closed then.
func TestDialogDTLSNewAssociationKeyedBeforeInstalled(t *testing.T) {
	_, answerer := newEstablishedDTLSMedia(t)
	d := newICEDialogMedia(t, answerer)
	updates := make(chan struct{}, 4)
	d.onMediaUpdate = func(*DialogMedia) { updates <- struct{}{} }
	replaced := answerer.Laddr

	// The call moves to another peer, with a certificate of its own.
	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	peer := newDTLSMediaSession(t, other)

	ctx := t.Context()
	require.NoError(t, d.onRemoteSDP(ctx, peer.LocalSDP(false), false))
	answer, err := d.onLocalSDP(ctx, true, "")
	require.NoError(t, err)
	var forkPort int
	_, err = fmt.Sscanf(sdpLineWithPrefix(t, answer, "m=audio "), "m=audio %d", &forkPort)
	require.NoError(t, err)
	require.NotEqual(t, replaced.Port, forkPort, "a new association is answered on a transport of its own")
	require.Same(t, answerer, d.MediaSession(), "a fork without keys was installed")
	require.Empty(t, updates, "the application was told of an update that is not in place")

	require.NoError(t, peer.RemoteSDP(answer, true))
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Finalize() }()
	committed := make(chan error, 1)
	go func() { committed <- d.onFinalize(ctx) }()
	for _, done := range []chan error{committed, peerDone} {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(20 * time.Second):
			t.Fatal("the handshake of the new association did not complete")
		}
	}

	installed := d.MediaSession()
	require.NotSame(t, answerer, installed)
	require.Equal(t, forkPort, installed.Laddr.Port)
	require.False(t, installed.FinalizePending())
	require.Len(t, updates, 1)
	peerMedia := &DialogMedia{}
	peerMedia.InitMediaSession(peer, nil, nil)
	requireEncryptedMedia(t, d, peerMedia, 100)
	requireEncryptedMedia(t, peerMedia, d, 110)
	requirePortFree(t, replaced, "the sockets of the replaced session were left open")

	// The commit that follows a later ACK, or a copy of the same one, finds
	// nothing left to do.
	require.NoError(t, d.onFinalize(ctx))
	require.Same(t, installed, d.MediaSession())
}

// TestDialogDTLSNewAssociationFailedHandshake is a re-INVITE asking for a new
// DTLS association whose peer then presents a certificate matching none of the
// fingerprints of its offer. The offer was answered before the handshake, so
// the commit reports that the update could not be keyed, for the dialog to
// end the call (RFC 5763 section 5), and the fork is never installed: its
// sockets are released, and the current session is left as it was.
func TestDialogDTLSNewAssociationFailedHandshake(t *testing.T) {
	_, answerer := newEstablishedDTLSMedia(t)
	d := newICEDialogMedia(t, answerer)

	// The call moves to another peer, whose offer carries the fingerprint of a
	// certificate other than the one it presents.
	peer := newDTLSMediaSession(t, testdata.ClientCertificate())
	offer := offerWithOtherFingerprint(t, peer)

	ctx := t.Context()
	require.NoError(t, d.onRemoteSDP(ctx, offer, false))
	// The answer goes out before the handshake, which the peer then runs.
	answer, err := d.onLocalSDP(ctx, true, "")
	require.NoError(t, err)

	require.NoError(t, peer.RemoteSDP(answer, true))
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Finalize() }()
	updated := make(chan error, 1)
	go func() { updated <- d.onFinalize(ctx) }()

	select {
	case err := <-updated:
		require.ErrorIs(t, err, errMediaKeyingFailed)
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
	_, err = fmt.Sscanf(sdpLineWithPrefix(t, answer, "m=audio "), "m=audio %d", &forkPort)
	require.NoError(t, err)
	require.NotEqual(t, answerer.Laddr.Port, forkPort)
	requirePortReleased(t, answer)
}

// TestDialogDTLSNewAssociationRefusesExchangesWhileKeying pins that the media
// takes no other offer/answer exchange while the handshake of a new DTLS
// association runs, which it does with the media's lock released: a second
// exchange would fork the session that handshake is about to replace. The
// refusal is the one a session answers 491. A handshake cut short by its
// context leaves the session in place as it was and releases the fork.
func TestDialogDTLSNewAssociationRefusesExchangesWhileKeying(t *testing.T) {
	_, answerer := newEstablishedDTLSMedia(t)
	d := newICEDialogMedia(t, answerer)

	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	peer := newDTLSMediaSession(t, other)
	offer := peer.LocalSDP(false)

	require.NoError(t, d.onRemoteSDP(t.Context(), offer, false))
	answer, err := d.onLocalSDP(t.Context(), true, "")
	require.NoError(t, err)

	errGivenUp := errors.New("given up")
	ctx, giveUp := context.WithCancelCause(t.Context())
	defer giveUp(nil)
	committed := make(chan error, 1)
	go func() { committed <- d.onFinalize(ctx) }()
	// We answered actpass with active, so our side sends the ClientHello,
	// which the peer never answers.
	requireClientHello(t, peer)

	require.ErrorIs(t, d.onRemoteSDP(t.Context(), offer, false), errMediaUpdateInProgress)
	_, err = d.onLocalSDP(t.Context(), false, "")
	require.ErrorIs(t, err, errMediaUpdateInProgress)

	giveUp(errGivenUp)
	select {
	case err := <-committed:
		require.ErrorIs(t, err, errMediaKeyingFailed)
	case <-time.After(10 * time.Second):
		t.Fatal("the handshake did not end with its context")
	}
	require.Same(t, answerer, d.MediaSession())
	requirePortReleased(t, answer)
	// The media takes an exchange again.
	_, err = d.onLocalSDP(t.Context(), false, "")
	require.NoError(t, err)
}

// TestDialogDTLSNewAssociationDroppedWithoutCommit pins what becomes of the
// fork that answered an offer asking for a new DTLS association when the
// exchange ends without the commit: the answer was not sent, or its ACK never
// came. The session tells the media to drop what it staged, or closes it, and
// either releases the transport the fork had bound, with the session in place
// left as it was.
func TestDialogDTLSNewAssociationDroppedWithoutCommit(t *testing.T) {
	for _, tc := range []struct {
		name string
		drop func(t *testing.T, d *DialogMedia)
	}{
		{name: "aborted", drop: func(_ *testing.T, d *DialogMedia) { d.abortPendingMediaSession() }},
		{name: "closed", drop: func(t *testing.T, d *DialogMedia) { require.NoError(t, d.Close()) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, answerer := newEstablishedDTLSMedia(t)
			d := newICEDialogMedia(t, answerer)

			other, err := selfsign.GenerateSelfSigned()
			require.NoError(t, err)
			peer := newDTLSMediaSession(t, other)
			require.NoError(t, d.onRemoteSDP(t.Context(), peer.LocalSDP(false), false))
			answer, err := d.onLocalSDP(t.Context(), true, "")
			require.NoError(t, err)

			tc.drop(t, d)
			require.Same(t, answerer, d.MediaSession())
			requirePortReleased(t, answer)
			// Nothing is left for a commit that comes late to key or install.
			require.NoError(t, d.onFinalize(t.Context()))
			require.Same(t, answerer, d.MediaSession())
		})
	}
}

// TestDialogDTLSNewAssociationAnswerKeepsExternalIP pins the address the
// answer to a re-INVITE asking for a new DTLS association publishes. The fork
// binds a new port on the address the session runs on, which is reached
// through the same external address, so the answer's c= line carries the
// external address the call was set up with, and its m= line the new port.
func TestDialogDTLSNewAssociationAnswerKeepsExternalIP(t *testing.T) {
	_, answerer := newEstablishedDTLSMedia(t)
	answerer.ExternalIP = net.IPv4(203, 0, 113, 7)
	d := newICEDialogMedia(t, answerer)

	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	peer := newDTLSMediaSession(t, other)
	require.NoError(t, d.onRemoteSDP(t.Context(), peer.LocalSDP(false), false))
	answer, err := d.onLocalSDP(t.Context(), true, "")
	require.NoError(t, err)
	t.Cleanup(d.abortPendingMediaSession)

	sd := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(answer, &sd))
	connInfo, err := sd.ConnectionInformation()
	require.NoError(t, err)
	require.Equal(t, "203.0.113.7", connInfo.IP.String())
	var forkPort int
	_, err = fmt.Sscanf(sdpLineWithPrefix(t, answer, "m=audio "), "m=audio %d", &forkPort)
	require.NoError(t, err)
	require.NotEqual(t, answerer.Laddr.Port, forkPort)
}

// newDTLSServerCall returns a confirmed inbound dialog whose DTLS-SRTP media
// is keyed and installed, the media, its media session, and what the dialog
// sends, which is answered 200.
func newDTLSServerCall(t *testing.T) (*DialogServerSession, *DialogMedia, *media.MediaSession, <-chan *sip.Request) {
	t.Helper()
	d, sent := newRecordingServerDialog(t, newByeServerTx(), nil, MediaConfig{})
	res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
	res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
	d.InviteResponse = res
	confirm(t, d)

	_, answerer := newEstablishedDTLSMedia(t)
	med := newICEDialogMedia(t, answerer)
	med.registerDialogCallbacks(&d.dialogCallbacks)
	return d, med, answerer, sent
}

// TestDialogServerReInviteNewAssociationFailureHangsUp is a re-INVITE asking
// for a new DTLS association whose peer then presents a certificate matching
// none of the fingerprints of its offer. The offer was answered 200 before the
// handshake, so the peer has moved to the new association, which has no keys:
// the dialog ends the call with a BYE, as RFC 5763 section 5 has the media
// session torn down on a fingerprint mismatch. The handshake runs when the ACK
// to that 200 is read, whose reading reports the failure.
func TestDialogServerReInviteNewAssociationFailureHangsUp(t *testing.T) {
	d, med, answerer, sent := newDTLSServerCall(t)

	// The call moves to another peer, whose offer carries the fingerprint of a
	// certificate other than the one it presents.
	peer := newDTLSMediaSession(t, testdata.ClientCertificate())
	offer := offerWithOtherFingerprint(t, peer)

	req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(offer)
	tx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
	handled := make(chan error, 1)
	go func() { handled <- d.handleReInvite(t.Context(), req, tx) }()

	var answer []byte
	select {
	case res := <-tx.first:
		require.Equal(t, sip.StatusOK, res.StatusCode, "reason: %s", res.Reason)
		answer = res.Body()
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not answered")
	}
	require.NoError(t, peer.RemoteSDP(answer, true))
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Finalize() }()
	acked := make(chan error, 1)
	go func() { acked <- d.ReadAck(newReInviteAck(req, nil), newByeServerTx()) }()

	select {
	case err := <-acked:
		require.ErrorIs(t, err, errMediaUpdateAfterAnswer)
	case <-time.After(20 * time.Second):
		t.Fatal("the ACK's handler did not return")
	}
	select {
	case err := <-handled:
		require.NoError(t, err)
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
	require.Same(t, answerer, med.MediaSession(), "a failed association must leave the session unchanged")
	requirePortReleased(t, answer)
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
// handshake. That session is returned too, for a test to see the handshake
// start.
func newNewAssociationReInviteDialog(t *testing.T) (*DialogServerSession, *DialogMedia, *media.MediaSession, *media.MediaSession, *sip.Request) {
	t.Helper()
	d, med, answerer, _ := newDTLSServerCall(t)

	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	peer := newDTLSMediaSession(t, other)
	req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(peer.LocalSDP(false))
	return d, med, answerer, peer, req
}

// TestDialogByeEndsNewAssociationHandshake is a BYE that arrives while the
// handshake of a new DTLS association, asked for by a re-INVITE already
// answered 200 and acknowledged, is still running. The peer never runs it, so
// it would take until DTLSHandshakeTimeout. The BYE is answered at once, the
// end of the dialog ends the handshake, and the fork is not installed: its
// sockets are released and the session is left as it was.
func TestDialogByeEndsNewAssociationHandshake(t *testing.T) {
	d, med, answerer, peer, req := newNewAssociationReInviteDialog(t)

	tx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
	handled := make(chan error, 1)
	go func() { handled <- d.handleReInvite(t.Context(), req, tx) }()

	var answer []byte
	select {
	case res := <-tx.first:
		require.Equal(t, sip.StatusOK, res.StatusCode, "reason: %s", res.Reason)
		answer = res.Body()
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not answered")
	}
	acked := make(chan error, 1)
	go func() { acked <- d.ReadAck(newReInviteAck(req, nil), newByeServerTx()) }()
	// We answered actpass with active, so our side sends the ClientHello.
	requireClientHello(t, peer)
	require.Same(t, answerer, med.MediaSession(), "a fork without keys was installed")

	byeTx := newByeServerTx()
	byeRead := make(chan error, 1)
	go func() { byeRead <- d.ReadBye(t.Context(), newBye(t, d, req.CSeq().SeqNo+1), byeTx) }()
	select {
	case err := <-byeRead:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the BYE waited for the handshake")
	}
	require.Len(t, byeTx.responses, 1)
	require.Equal(t, sip.StatusOK, byeTx.responses[0].StatusCode)

	select {
	case err := <-acked:
		require.NoError(t, err, "the end of the dialog is no failure of the update")
	case <-time.After(5 * time.Second):
		t.Fatal("the end of the dialog did not end the handshake")
	}
	select {
	case err := <-handled:
		require.NoError(t, err, "the end of the dialog is no failure of the re-INVITE")
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE handler did not return")
	}
	require.Same(t, answerer, med.MediaSession(), "nothing may be installed on an ended dialog")
	requirePortReleased(t, answer)
}

// byeBeforeAnswerMedia is the media of a call whose peer's BYE is read while
// the media is handed the offer of a re-INVITE: after the re-INVITE was found
// live, and before its answer. It keeps the answer the media then describes.
type byeBeforeAnswerMedia struct {
	*DialogMedia
	readBye func() error

	byeErr error
	answer []byte
}

func (m *byeBeforeAnswerMedia) onRemoteSDP(ctx context.Context, remoteSDP []byte, offered bool) error {
	m.byeErr = m.readBye()
	return m.DialogMedia.onRemoteSDP(ctx, remoteSDP, offered)
}

func (m *byeBeforeAnswerMedia) onLocalSDP(ctx context.Context, answered bool, mode string) ([]byte, error) {
	answer, err := m.DialogMedia.onLocalSDP(ctx, answered, mode)
	m.answer = answer
	return answer, err
}

// TestDialogByeBeforeNewAssociationAnswer is a BYE that ends the dialog after a
// re-INVITE asking for a new DTLS association was found live and before its
// answer. The BYE answers it 487 (RFC 3261 section 15.1.2), so the update
// sends nothing, runs no handshake and closes the fork it built.
func TestDialogByeBeforeNewAssociationAnswer(t *testing.T) {
	d, med, answerer, _, req := newNewAssociationReInviteDialog(t)
	hooked := &byeBeforeAnswerMedia{DialogMedia: med, readBye: func() error {
		return d.ReadBye(t.Context(), newBye(t, d, req.CSeq().SeqNo+1), newByeServerTx())
	}}
	d.dialogCallbacks.mu.Lock()
	d.mediaHanshaker = hooked
	d.dialogCallbacks.mu.Unlock()

	tx := newByeServerTx()
	require.NoError(t, d.handleReInvite(t.Context(), req, tx))
	require.NoError(t, hooked.byeErr)
	require.Len(t, tx.responses, 1, "the re-INVITE got a second final response")
	require.Equal(t, sip.StatusRequestTerminated, tx.responses[0].StatusCode)

	require.Same(t, answerer, med.MediaSession())
	require.NotEmpty(t, hooked.answer, "the media built no fork for the offer")
	requirePortReleased(t, hooked.answer)
}

// newAckingDTLSClientDialog places a DTLS-SRTP call without ICE from a dialog
// whose client answers the INVITE with the answer of peer, and starts its Ack.
// The ACK reaches nobody, so Ack runs the handshake until peer takes part in
// it. It returns once the ACK is out, with the result of Ack to come; the test
// ends Ack and waits for it when it ends.
func newAckingDTLSClientDialog(t *testing.T) (*DialogClientSession, *DialogMedia, *media.MediaSession, <-chan error) {
	t.Helper()
	peer := newDTLSMediaSession(t, testdata.ServerCertificate())
	var peerErr error
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		if req.IsInvite() {
			peerErr = peer.RemoteSDP(req.Body(), false)
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			res.SetBody(peer.LocalSDP(true))
		}
		return res
	},
		WithTransport(Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			MediaSRTP: media.SecureRTPModeDTLS,
			MediaDTLSConf: media.DTLSConfig{
				Certificates: []tls.Certificate{testdata.ClientCertificate()},
			},
		}),
		WithMediaConfig(MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}),
	)

	ctx, cancel := context.WithCancel(context.Background())
	d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
	require.NoError(t, err)
	med, err := d.Invite(ctx, InviteClientOptions{})
	require.NoError(t, err)
	require.NoError(t, peerErr)

	confirmed := make(chan struct{})
	var once sync.Once
	d.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateConfirmed {
			once.Do(func() { close(confirmed) })
		}
	})
	acked := make(chan error, 1)
	ackDone := make(chan struct{})
	go func() {
		defer close(ackDone)
		acked <- d.Ack(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-ackDone:
		case <-time.After(10 * time.Second):
			t.Error("Ack did not return")
		}
		_ = d.Close()
	})
	select {
	case <-confirmed:
	case <-time.After(5 * time.Second):
		t.Fatal("the ACK was not sent")
	}
	return d, med, peer, acked
}

// TestDialogClientReInviteDuringAckHandshake is a re-INVITE the peer sends once
// our ACK reached it, while our Ack still runs the DTLS handshake of the call.
// A fork of the session made then has no association to continue, and would
// arm a second handshake on the socket the first one runs on. The re-INVITE is
// handled once Ack has returned, and continues the association Ack keyed. When
// Ack does not return in time the re-INVITE is answered 491, which asks the
// peer to retry (RFC 5407 section 3.1.4).
func TestDialogClientReInviteDuringAckHandshake(t *testing.T) {
	t.Run("keyed meanwhile", func(t *testing.T) {
		d, med, peer, acked := newAckingDTLSClientDialog(t)
		offer := peer.Fork().LocalSDP(false)
		req := newPeerReInvite(d, 2)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(offer)

		tx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
		handled := make(chan error, 1)
		go func() { handled <- d.handleReInvite(t.Context(), req, tx) }()
		peerDone := make(chan error, 1)
		go func() { peerDone <- peer.Finalize() }()

		select {
		case err := <-acked:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("Ack did not return")
		}
		select {
		case err := <-peerDone:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the peer's handshake did not return")
		}

		var res *sip.Response
		select {
		case res = <-tx.first:
		case <-time.After(5 * time.Second):
			t.Fatal("the re-INVITE was not answered")
		}
		require.Equal(t, sip.StatusOK, res.StatusCode, "reason: %s", res.Reason)
		// We offered actpass and the peer answered active, so we are the
		// passive end of the association, and keep that role (RFC 8842
		// section 5.3).
		require.Equal(t, "a=setup:passive", sdpLineWithPrefix(t, res.Body(), "a=setup:"), "the answer did not continue the association")
		require.NoError(t, d.handleReInviteACK(newReInviteAck(req, nil), newByeServerTx()))
		select {
		case err := <-handled:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the re-INVITE handler did not return")
		}
		require.False(t, med.MediaSession().FinalizePending(), "the installed fork has a handshake pending")
	})

	t.Run("not keyed in time", func(t *testing.T) {
		d, med, peer, _ := newAckingDTLSClientDialog(t)
		sess := med.MediaSession()
		req := newPeerReInvite(d, 2)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(peer.Fork().LocalSDP(false))

		tx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
		handled := make(chan error, 1)
		go func() { handled <- d.handleReInvite(t.Context(), req, tx) }()
		select {
		case res := <-tx.first:
			require.Equal(t, sip.StatusRequestPending, res.StatusCode, "reason: %s", res.Reason)
		case <-time.After(5 * time.Second):
			t.Fatal("the re-INVITE was not answered")
		}
		select {
		case err := <-handled:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the re-INVITE handler did not return")
		}
		require.Same(t, sess, med.MediaSession(), "the re-INVITE must not reach the media")
	})
}
