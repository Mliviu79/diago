// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/stretchr/testify/require"
)

// heldResend is the INVITE sent again with credentials, handed to the test
// before any response to it is delivered.
type heldResend struct {
	req     *sip.Request
	deliver func(*sip.Response)
}

// holdResends returns the resent argument of challengedEarlyDialogCaller that
// hands each INVITE sent again to the returned channel.
func holdResends() (func(req *sip.Request, deliver func(*sip.Response)), <-chan heldResend) {
	held := make(chan heldResend, 1)
	return func(req *sip.Request, deliver func(*sip.Response)) {
		held <- heldResend{req: req, deliver: deliver}
	}, held
}

// awaitResend returns the INVITE sent again, after the challenge.
func awaitResend(t *testing.T, held <-chan heldResend) heldResend {
	t.Helper()
	select {
	case r := <-held:
		return r
	case <-time.After(10 * time.Second):
		t.Fatal("the INVITE was not sent again after the challenge")
		return heldResend{}
	}
}

// requireNothingFromCaller fails the test when peer, the media of an ended
// early dialog, receives anything on its RTP socket within the duration,
// once what had arrived before the call is drained.
func requireNothingFromCaller(t *testing.T, peer *media.MediaSession, within time.Duration) {
	t.Helper()
	buf := make([]byte, media.RTPBufSize)
	for {
		if _, err := peer.ReadRTPRawDeadline(buf, time.Now().Add(50*time.Millisecond)); err != nil {
			break
		}
	}
	n, err := peer.ReadRTPRawDeadline(buf, time.Now().Add(within))
	require.Error(t, err, "the media of the ended early dialog sent %d bytes to its peer", n)
	var netErr net.Error
	require.True(t, errors.As(err, &netErr) && netErr.Timeout(), "reading the peer socket failed: %v", err)
}

// requirePlainAudio requires a frame written to med's audio writer to reach
// peer.
func requirePlainAudio(t *testing.T, med *DialogMedia, peer *media.MediaSession) {
	t.Helper()
	w, err := med.AudioWriter()
	require.NoError(t, err)
	_, err = w.Write(make([]byte, 160))
	require.NoError(t, err)
	buf := make([]byte, media.RTPBufSize)
	_, err = peer.ReadRTPRawDeadline(buf, time.Now().Add(5*time.Second))
	require.NoError(t, err, "no audio reached the peer")
}

// TestDialogClientChallengeStopsKeyedEarlyMedia is a call with
// EarlyMediaDetect whose 183 is keyed early media and whose INVITE is then
// challenged. The challenge, a non-2xx final response, ends the early dialog
// (RFC 3261 section 12.3), and its media stops with it: the writer handed out
// for it refuses, and nothing reaches its peer. The INVITE sent again with
// credentials is answered by another callee, whose answer the call takes,
// with audio.
func TestDialogClientChallengeStopsKeyedEarlyMedia(t *testing.T) {
	early := newMediaSessionForTest(t)
	final := newMediaSessionForTest(t)
	release := make(chan struct{})
	resent, held := holdResends()
	d := challengedEarlyDialogCaller(t, early, release, resent)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	med, err := d.Invite(ctx, InviteClientOptions{EarlyMediaDetect: true})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	requirePlainAudio(t, med, early)
	earlyWriter, err := med.AudioWriter()
	require.NoError(t, err)

	close(release)
	answered := make(chan error, 1)
	go func() {
		answered <- d.WaitAnswer(ctx, med, sipgo.AnswerOptions{Username: "caller", Password: "secret"})
	}()
	again := awaitResend(t, held)

	_, err = earlyWriter.Write(make([]byte, 160))
	require.ErrorIs(t, err, errEarlyMediaEnded, "the early media writer still writes")
	require.Nil(t, med.RTPSession(), "the RTP session of the ended early dialog runs on")
	_, err = med.AudioWriter()
	require.ErrorIs(t, err, ErrNoMediaSetup)
	requireNothingFromCaller(t, early, 500*time.Millisecond)

	body := answerOf(t, final, again.req)
	again.deliver(earlyDialogResponse(again.req, sip.StatusSessionInProgress, "Session Progress", "a", body))
	again.deliver(earlyDialogResponse(again.req, sip.StatusOK, "OK", "a", body))
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the INVITE sent again was not answered")
	}
	requireRemote(t, med, final, "the call did not take the answer of the INVITE sent again")
	requirePlainAudio(t, med, final)
	requireNothingFromCaller(t, early, 200*time.Millisecond)
}

// TestDialogClientChallengeEndsLeftEarlyHandshake is a DTLS-SRTP call with
// EarlyMediaDetect whose 183 comes from a callee that never keys it, so the
// handshake it started, in which we are the DTLS client, is left running and
// the wait for the answer goes on. The challenge ends that early dialog
// (RFC 3261 section 12.3) and the handshake with it: no ClientHello is sent
// again to its peer. The 183 to the INVITE sent again is taken as early media
// as the first one was, keyed by its callee, and its 200 confirms the call,
// with encrypted media both ways.
func TestDialogClientChallengeEndsLeftEarlyHandshake(t *testing.T) {
	early := newDTLSMediaSession(t, testdata.ServerCertificate())
	cert, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	renewed := newDTLSMediaSession(t, cert)
	resent, held := holdResends()
	challenged := make(chan struct{})
	d := newStagedCallerDialog(t, Transport{
		MediaSRTP:     media.SecureRTPModeDTLS,
		MediaDTLSConf: media.DTLSConfig{Certificates: []tls.Certificate{testdata.ClientCertificate()}},
	}, func(req *sip.Request, deliver func(*sip.Response)) {
		if req.GetHeader("Authorization") != nil {
			resent(req, deliver)
			return
		}
		// Passive, so we are the DTLS client and send the ClientHellos.
		progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", withSetup(answerOf(t, early, req), "passive"))
		challenge := earlyDialogResponse(req, sip.StatusUnauthorized, "Unauthorized", "a", nil)
		challenge.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="pbx", nonce="early-nonce", algorithm=MD5`))
		go func() {
			deliver(progress)
			select {
			case <-challenged:
			case <-t.Context().Done():
				return
			}
			deliver(challenge)
		}()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()
	invited := make(chan error, 1)
	var med *DialogMedia
	go func() {
		var err error
		med, err = d.Invite(ctx, InviteClientOptions{
			EarlyMediaDetect:     true,
			EarlyMediaKeyTimeout: 2 * time.Second,
			Username:             "caller",
			Password:             "secret",
		})
		invited <- err
	}()

	// The ClientHello of the handshake the 183 started reaches its peer.
	buf := make([]byte, media.RTPBufSize)
	_, err = early.ReadRTPRawDeadline(buf, time.Now().Add(5*time.Second))
	require.NoError(t, err, "the handshake of the 183 sent nothing")
	// Challenged once the wait for the keys has ended, with the handshake
	// left running.
	require.Eventually(t, d.earlyKeyingLeft, 10*time.Second, 20*time.Millisecond, "the handshake of the 183 was not left running")
	close(challenged)
	again := awaitResend(t, held)
	// The handshake would send its ClientHello again within its
	// retransmission timer, one second and doubling (RFC 6347 section
	// 4.2.4.1).
	requireNothingFromCaller(t, early, 3*time.Second)

	body := answerOf(t, renewed, again.req)
	peerDone := make(chan error, 1)
	go func() { peerDone <- renewed.Finalize() }()
	again.deliver(earlyDialogResponse(again.req, sip.StatusSessionInProgress, "Session Progress", "a", body))
	select {
	case err := <-invited:
		require.ErrorIs(t, err, ErrClientEarlyMedia, "the 183 to the INVITE sent again was not taken as early media")
	case <-time.After(20 * time.Second):
		t.Fatal("the INVITE sent again had no early media")
	}
	select {
	case err := <-peerDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the callee of the INVITE sent again did not key the early media")
	}
	requireRemote(t, med, renewed, "the early media is not the 183's of the INVITE sent again")
	requireEncryptedMedia(t, med, &DialogMedia{mediaSession: renewed}, 100)

	answered := make(chan error, 1)
	go func() { answered <- d.WaitAnswer(ctx, med, sipgo.AnswerOptions{}) }()
	again.deliver(earlyDialogResponse(again.req, sip.StatusOK, "OK", "a", body))
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the INVITE sent again was not answered")
	}
	require.NoError(t, d.Ack(ctx))
	requireEncryptedMedia(t, med, &DialogMedia{mediaSession: renewed}, 200)
	requireEncryptedMedia(t, &DialogMedia{mediaSession: renewed}, med, 300)
}
