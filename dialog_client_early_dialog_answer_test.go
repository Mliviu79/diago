// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"crypto/tls"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newStagedCallerDialog returns an outgoing dialog, not invited yet, over a
// dialer whose transactions never reach the network: respond is given each
// request and delivers the responses to it. A request other than the INVITE
// is answered 200. The media offers PCMU over tr. The dialog is closed with
// the test.
func newStagedCallerDialog(t *testing.T, tr Transport, respond func(req *sip.Request, deliver func(*sip.Response))) *DialogClientSession {
	t.Helper()
	requester := &stagedTxRequester{respond: func(req *sip.Request, deliver func(*sip.Response)) {
		if req.Method != sip.INVITE {
			go deliver(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
			return
		}
		respond(req, deliver)
	}}
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	require.NoError(t, err)
	client.TxRequester = requester
	tr.Transport = "udp"
	tr.BindHost = "127.0.0.1"
	dg := NewDiago(ua, WithClient(client), WithTransport(tr),
		WithMediaConfig(MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}),
	)
	d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5099}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// earlyDialogResponse returns the response to req with status, in the early
// dialog toTag names, none when it is empty, carrying body as its session
// description when body is not nil. A 2xx names the callee's contact.
func earlyDialogResponse(req *sip.Request, status int, reason, toTag string, body []byte) *sip.Response {
	res := sip.NewResponseFromRequest(req, status, reason, body)
	if toTag != "" {
		res.To().Params.Add("tag", toTag)
	}
	if body != nil {
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	if status >= 200 {
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5099}})
	}
	return res
}

// answerOf has peer take the offer req carries and returns its answer.
func answerOf(t *testing.T, peer *media.MediaSession, req *sip.Request) []byte {
	t.Helper()
	if err := peer.RemoteSDP(req.Body(), false); err != nil {
		t.Errorf("the peer took no offer: %v", err)
		return nil
	}
	return peer.LocalSDP(true)
}

// requireRemote fails the test unless the media of the call sends to peer.
func requireRemote(t *testing.T, med *DialogMedia, peer *media.MediaSession, msg string) {
	t.Helper()
	sess := med.MediaSession()
	require.NotNil(t, sess)
	assert.Equal(t, peer.Laddr.Port, sess.Raddr.Port, msg)
}

// TestDialogClientEarlyDialogAnswerWithoutDetect pins that the first session
// description a provisional response of an early dialog carries is that
// dialog's answer also without EarlyMediaDetect (RFC 3261 section 13.2.1): the
// 200 of the same dialog then carries another, which is not applied, and no
// media is started before the 200.
func TestDialogClientEarlyDialogAnswerWithoutDetect(t *testing.T) {
	early := newMediaSessionForTest(t)
	final := newMediaSessionForTest(t)
	d := newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
		progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", answerOf(t, early, req))
		ok := earlyDialogResponse(req, sip.StatusOK, "OK", "a", answerOf(t, final, req))
		go func() {
			deliver(progress)
			deliver(ok)
		}()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	med, err := d.Invite(ctx, InviteClientOptions{})
	require.NoError(t, err)
	requireRemote(t, med, early, "the call took the 200's session description, not the 183's")
}

// TestDialogClientEarlyDialogAnswerFinalWithoutSDP pins that a 200 without a
// session description is answered when its early dialog has an answer: the
// first session description received is the answer, and the final response
// need not repeat it (RFC 3261 section 13.2.1, RFC 6337 section 3.1.1).
func TestDialogClientEarlyDialogAnswerFinalWithoutSDP(t *testing.T) {
	t.Run("EarlyMediaDetect", func(t *testing.T) {
		early := newMediaSessionForTest(t)
		answerNow := make(chan struct{})
		d := newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
			progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", answerOf(t, early, req))
			ok := earlyDialogResponse(req, sip.StatusOK, "OK", "a", nil)
			go func() {
				deliver(progress)
				select {
				case <-answerNow:
				case <-t.Context().Done():
					return
				}
				deliver(ok)
			}()
		})

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		med, err := d.Invite(ctx, InviteClientOptions{EarlyMediaDetect: true})
		require.ErrorIs(t, err, ErrClientEarlyMedia)
		sess := med.MediaSession()
		close(answerNow)
		require.NoError(t, d.WaitAnswer(ctx, med, sipgo.AnswerOptions{}))
		require.Same(t, sess, med.MediaSession(), "the 200 changed the media of the 183")
		requireRemote(t, med, early, "the call left the media of the 183")
	})
	t.Run("WithoutDetect", func(t *testing.T) {
		early := newMediaSessionForTest(t)
		d := newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
			progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", answerOf(t, early, req))
			ok := earlyDialogResponse(req, sip.StatusOK, "OK", "a", nil)
			go func() {
				deliver(progress)
				deliver(ok)
			}()
		})

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		med, err := d.Invite(ctx, InviteClientOptions{})
		require.NoError(t, err)
		requireRemote(t, med, early, "the call did not take the answer of the 183")
	})
}

// TestDialogClientOtherEarlyDialogAnswer pins that the 200 of another early
// dialog than the 183 whose early media the call took is answered by its own
// dialog's first session description, here that of the 180 of that dialog,
// and not by the 200's (RFC 3261 sections 12.1 and 13.2.1). The media moves
// to that dialog's answer.
func TestDialogClientOtherEarlyDialogAnswer(t *testing.T) {
	early := newMediaSessionForTest(t)
	other := newMediaSessionForTest(t)
	final := newMediaSessionForTest(t)
	answerNow := make(chan struct{})
	d := newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
		progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", answerOf(t, early, req))
		ringing := earlyDialogResponse(req, sip.StatusRinging, "Ringing", "b", answerOf(t, other, req))
		ok := earlyDialogResponse(req, sip.StatusOK, "OK", "b", answerOf(t, final, req))
		go func() {
			deliver(progress)
			select {
			case <-answerNow:
			case <-t.Context().Done():
				return
			}
			deliver(ringing)
			deliver(ok)
		}()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	med, err := d.Invite(ctx, InviteClientOptions{EarlyMediaDetect: true})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	requireRemote(t, med, early, "the early media is not the 183's")
	close(answerNow)
	require.NoError(t, d.WaitAnswer(ctx, med, sipgo.AnswerOptions{}))
	requireRemote(t, med, other, "the call did not take the answer of the 200's dialog from its 180")
}

// TestDialogClientUntaggedProvisionalNoDialogAnswer pins that a provisional
// response without a To tag, which creates no early dialog (RFC 3261 section
// 12.1), gives no dialog its answer: the 200 is answered by the first session
// description of its own dialog.
func TestDialogClientUntaggedProvisionalNoDialogAnswer(t *testing.T) {
	t.Run("TaggedProvisionalAfter", func(t *testing.T) {
		untagged := newMediaSessionForTest(t)
		tagged := newMediaSessionForTest(t)
		final := newMediaSessionForTest(t)
		d := newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
			first := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "", answerOf(t, untagged, req))
			second := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "t", answerOf(t, tagged, req))
			ok := earlyDialogResponse(req, sip.StatusOK, "OK", "t", answerOf(t, final, req))
			go func() {
				deliver(first)
				deliver(second)
				deliver(ok)
			}()
		})

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		med, err := d.Invite(ctx, InviteClientOptions{})
		require.NoError(t, err)
		requireRemote(t, med, tagged, "the call did not take the answer of the 200's dialog")
	})
	t.Run("FinalOnly", func(t *testing.T) {
		untagged := newMediaSessionForTest(t)
		final := newMediaSessionForTest(t)
		d := newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
			first := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "", answerOf(t, untagged, req))
			ok := earlyDialogResponse(req, sip.StatusOK, "OK", "t", answerOf(t, final, req))
			go func() {
				deliver(first)
				deliver(ok)
			}()
		})

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		med, err := d.Invite(ctx, InviteClientOptions{})
		require.NoError(t, err)
		requireRemote(t, med, final, "the answer of a 183 without a To tag was taken as the 200's dialog's")
	})
}

// TestDialogClientOtherEarlyDialogEndsLeftHandshake is a DTLS-SRTP call with
// EarlyMediaDetect whose 183 comes from a callee that never keys it, so the
// handshake it started is left running, and whose 200 comes from another
// early dialog, with a callee of its own. That handshake is ended, and the
// call takes the 200's dialog's answer and is keyed by its callee once the
// ACK is out, with encrypted media both ways.
func TestDialogClientOtherEarlyDialogEndsLeftHandshake(t *testing.T) {
	early := newDTLSMediaSession(t, testdata.ServerCertificate())
	cert, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	answering := newDTLSMediaSession(t, cert)
	d := newStagedCallerDialog(t, Transport{
		MediaSRTP:     media.SecureRTPModeDTLS,
		MediaDTLSConf: media.DTLSConfig{Certificates: []tls.Certificate{testdata.ClientCertificate()}},
	}, func(req *sip.Request, deliver func(*sip.Response)) {
		progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", answerOf(t, early, req))
		ok := earlyDialogResponse(req, sip.StatusOK, "OK", "b", answerOf(t, answering, req))
		go func() {
			deliver(progress)
			deliver(ok)
		}()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	med, err := d.Invite(ctx, InviteClientOptions{EarlyMediaDetect: true, EarlyMediaKeyTimeout: 300 * time.Millisecond})
	require.NoError(t, err, "unkeyed early media is not reported, and the call is answered")
	requireRemote(t, med, answering, "the call did not take the answer of the 200's dialog")

	acked := make(chan error, 1)
	go func() { acked <- d.Ack(ctx) }()
	peerDone := make(chan error, 1)
	go func() { peerDone <- answering.Finalize() }()
	for _, done := range []chan error{acked, peerDone} {
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the handshake with the callee of the 200 did not complete")
		}
	}
	require.NotNil(t, med.RTPSession(), "Ack did not set the media up")
	requireEncryptedMedia(t, med, &DialogMedia{mediaSession: answering}, 100)
	requireEncryptedMedia(t, &DialogMedia{mediaSession: answering}, med, 200)
}
