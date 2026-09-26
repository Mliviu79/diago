// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"crypto/tls"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireClientHello waits for the DTLS ClientHello a session sends to peer,
// which shows its handshake is running. peer never answers it.
func requireClientHello(t *testing.T, peer *media.MediaSession) {
	t.Helper()
	buf := make([]byte, media.RTPBufSize)
	n, err := peer.ReadRTPRawDeadline(buf, time.Now().Add(10*time.Second))
	require.NoError(t, err, "the handshake never started")
	require.NotZero(t, n)
	// RFC 5764 section 5.1.2: a first byte of 20 to 63 is a DTLS record.
	require.True(t, buf[0] >= 20 && buf[0] <= 63, "first byte %d is not a DTLS record", buf[0])
}

// requireReturns waits for done, bounded, and returns what it carried.
func requireReturns(t *testing.T, done <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not return while the DTLS handshake was never completed", what)
	}
	return nil
}

// withSetup rewrites the a=setup of body.
func withSetup(body []byte, setup string) []byte {
	lines := strings.Split(string(body), "\r\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "a=setup:") {
			lines[i] = "a=setup:" + setup
		}
	}
	return []byte(strings.Join(lines, "\r\n"))
}

// TestDialogServerAnswerDTLSEndsWithDialog is an answer whose DTLS handshake,
// run once the ACK is read, the caller never completes. The handshake waits for
// the peer and retransmits for as long as its context lives, so with a context
// that never ends the answer, and the goroutine of the call handler with it,
// was parked for good, even after the caller hung up. It now ends with the
// dialog.
func TestDialogServerAnswerDTLSEndsWithDialog(t *testing.T) {
	d, peer, inviteTx := newProgressMediaDTLSDialog(t)

	answered := make(chan error, 1)
	go func() { answered <- d.Answer() }()
	select {
	case res := <-inviteTx.responded:
		require.Equal(t, sip.StatusOK, res.StatusCode)
	case <-time.After(5 * time.Second):
		t.Fatal("no 2xx was sent")
	}
	confirm(t, d)

	// We answered actpass with active, so our side sends the ClientHello.
	requireClientHello(t, peer)

	byeTx := newByeServerTx()
	require.NoError(t, d.ReadBye(newBye(t, d, d.InviteRequest.CSeq().SeqNo+1), byeTx))
	assert.Error(t, requireReturns(t, answered, "Answer"), "media that was never keyed is not an answer")
}

// TestDialogServerLateOfferDTLSEndsWithDialog is a late offer whose DTLS
// handshake, run when the ACK brings the answer, the caller never completes.
// ReadAck ran it with a context that never ends, so the ACK's handler was
// parked for good. It now ends with the dialog.
func TestDialogServerLateOfferDTLSEndsWithDialog(t *testing.T) {
	d, inviteTx, _ := newLateOfferDialog(t, MediaConfig{
		Codecs:     []media.Codec{media.CodecAudioUlaw},
		DTLSConfig: &media.DTLSConfig{Certificates: []tls.Certificate{testdata.ServerCertificate()}},
		secureRTP:  media.SecureRTPModeDTLS,
		bindIP:     net.IPv4(127, 0, 0, 1),
	})
	offer, answered := answerLate(t, d, inviteTx)

	peer := newDTLSMediaSession(t, testdata.ClientCertificate())
	require.NoError(t, peer.RemoteSDP(offer))
	// Passive, so our side starts the handshake the peer then ignores.
	answer := withSetup(peer.LocalSDP(), "passive")

	acked := make(chan error, 1)
	go func() { acked <- d.ReadAck(newLateAck(d, answer), newByeServerTx()) }()
	requireClientHello(t, peer)

	require.NoError(t, d.ReadBye(newBye(t, d, d.InviteRequest.CSeq().SeqNo+1), newByeServerTx()))
	assert.Error(t, requireReturns(t, acked, "ReadAck"))
	assert.Error(t, requireReturns(t, answered, "AnswerLate"))
}

// newSilentDTLSPeerClient builds a diago whose calls go over a fake
// transaction layer to a DTLS-SRTP peer that answers the INVITE with status and
// its SDP, with setup:passive so that our side starts the handshake, and then
// never runs its side. Every other request is answered 200.
func newSilentDTLSPeerClient(t *testing.T, status int) (*Diago, *media.MediaSession) {
	t.Helper()
	peer := newDTLSMediaSession(t, testdata.ServerCertificate())
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		if req.Method != sip.INVITE {
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		}
		if err := peer.RemoteSDP(req.Body()); err != nil {
			return sip.NewResponseFromRequest(req, sip.StatusNotAcceptableHere, "Not Acceptable Here", nil)
		}
		res := sip.NewResponseFromRequest(req, status, "", withSetup(peer.LocalSDP(), "passive"))
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5099}})
		return res
	}, WithTransport(Transport{
		Transport: "udp",
		BindHost:  "127.0.0.1",
		BindPort:  5098,
		MediaSRTP: media.SecureRTPModeDTLS,
		MediaDTLSConf: media.DTLSConfig{
			Certificates: []tls.Certificate{testdata.ClientCertificate()},
		},
	}))
	return dg, peer
}

// TestDialogClientAckDTLSEnds is an Ack whose DTLS handshake the callee never
// completes. Ack ran it with a context that never ends, so it never returned,
// whatever the caller's context or the dialog did. It now ends with either.
func TestDialogClientAckDTLSEnds(t *testing.T) {
	cases := []struct {
		name string
		end  func(d *DialogClientSession, cancel context.CancelFunc)
	}{
		{
			name: "caller context",
			end:  func(_ *DialogClientSession, cancel context.CancelFunc) { cancel() },
		},
		{
			name: "dialog",
			end: func(d *DialogClientSession, _ context.CancelFunc) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = d.Hangup(ctx)
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dg, peer := newSilentDTLSPeerClient(t, sip.StatusOK)
			dialog, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5099}, NewDialogOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = dialog.Close() })
			require.NoError(t, dialog.Invite(context.Background(), InviteClientOptions{}))

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			acked := make(chan error, 1)
			go func() { acked <- dialog.Ack(ctx) }()
			requireClientHello(t, peer)

			tc.end(dialog, cancel)
			assert.Error(t, requireReturns(t, acked, "Ack"))
		})
	}
}

// TestDialogClientEarlyMediaDTLSEndsWithInvite is early media whose DTLS
// handshake the callee never completes. It runs inside the wait for the answer,
// which cannot see the caller give up while it runs, and it ran with a context
// that never ends, so Invite never returned. It now ends with the caller's
// context.
func TestDialogClientEarlyMediaDTLSEndsWithInvite(t *testing.T) {
	dg, peer := newSilentDTLSPeerClient(t, sip.StatusSessionInProgress)
	dialog, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5099}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = dialog.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	invited := make(chan error, 1)
	go func() { invited <- dialog.Invite(ctx, InviteClientOptions{EarlyMediaDetect: true}) }()
	requireClientHello(t, peer)

	cancel()
	assert.Error(t, requireReturns(t, invited, "Invite"))
}
