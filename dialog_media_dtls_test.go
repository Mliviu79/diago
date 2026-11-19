// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
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
	release := make(chan struct{})
	handlerDone := make(chan struct{})
	require.NoError(t, serveBackground(t, callee, ctx, func(_ context.Context, d *DialogServerSession) {
		defer close(handlerDone)
		med, err := d.AnswerOptions(t.Context(), AnswerOptions{})
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

	call := dtlsCall{caller: d, callerMedia: med}
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
