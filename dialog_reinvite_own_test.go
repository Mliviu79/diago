// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reInviteCall is an answered call with media, seen through what the tests of
// its own re-INVITE drive and read.
type reInviteCall struct {
	med           *DialogMedia
	reInvite      func(ctx context.Context) error
	hold          func(ctx context.Context) error
	unhold        func(ctx context.Context) error
	remoteContact func() *sip.ContactHeader
}

// reInviteSides builds, for each side of a call, an answered call whose media
// was built from conf and runs on sess. Once setup has returned, the peer
// answers what the call sends with onRequest.
var reInviteSides = []struct {
	name  string
	setup func(t *testing.T, conf MediaConfig, sess *media.MediaSession, onRequest func(*sip.Request) *sip.Response) reInviteCall
}{
	{
		name: "inbound",
		setup: func(t *testing.T, conf MediaConfig, sess *media.MediaSession, onRequest func(*sip.Request) *sip.Response) reInviteCall {
			ua, err := sipgo.NewUA()
			require.NoError(t, err)
			t.Cleanup(func() { _ = ua.Close() })
			client, err := sipgo.NewClient(ua)
			require.NoError(t, err)
			client.TxRequester = &clientTxRequester{onRequest: onRequest}
			d := newTestDialogOverUA(t, newByeServerTx(), &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}}})
			res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
			res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
			d.InviteResponse = res
			confirm(t, d)

			conf.bindIP = net.IPv4(127, 0, 0, 1)
			med, err := newDialogMedia(t.Context(), conf, nil)
			require.NoError(t, err)
			med.registerDialogCallbacks(&d.dialogCallbacks)
			installTestMedia(t, med, sess)
			return reInviteCall{med: med, reInvite: d.ReInvite, hold: d.Hold, unhold: d.Unhold, remoteContact: d.RemoteContact}
		},
	},
	{
		name: "outbound",
		setup: func(t *testing.T, conf MediaConfig, sess *media.MediaSession, onRequest func(*sip.Request) *sip.Response) reInviteCall {
			reInviting := false
			dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
				if reInviting {
					return onRequest(req)
				}
				res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				if req.IsInvite() {
					res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
					res.SetBody(peerSDP())
				}
				return res
			}, WithMediaConfig(conf))
			d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = d.Close() })
			med, err := d.Invite(t.Context(), InviteClientOptions{})
			require.NoError(t, err)
			require.NoError(t, d.Ack(t.Context()))
			reInviting = true

			installTestMedia(t, med, sess)
			return reInviteCall{med: med, reInvite: d.ReInvite, hold: d.Hold, unhold: d.Unhold, remoteContact: d.RemoteContact}
		},
	},
}

// installTestMedia installs sess in m in place of the media session it has,
// with an RTP session whose monitor runs.
func installTestMedia(t *testing.T, m *DialogMedia, sess *media.MediaSession) {
	t.Helper()
	rtpSess := media.NewRTPSession(sess)
	m.mu.Lock()
	oldRTPSess, oldSess := m.rtpSession, m.mediaSession
	m.initRTPSessionUnsafe(sess, rtpSess)
	m.mu.Unlock()
	if oldRTPSess != nil {
		require.NoError(t, oldRTPSess.MonitorClose())
	}
	if oldSess != nil {
		require.NoError(t, oldSess.Close())
	}
	require.NoError(t, rtpSess.MonitorBackground())
	t.Cleanup(func() { _ = m.Close() })
}

// pendingFork returns the fork m holds for an offer of ours that has no
// answer yet.
func pendingFork(m *DialogMedia) *media.MediaSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pendingMediaSession
}

// TestDialogReInviteAppliesAnswer pins that ReInvite applies the answer it
// gets, on either side: here the peer answers from another port, and the
// media then goes there. Before, the answer was dropped and the media kept
// going to the old port.
func TestDialogReInviteAppliesAnswer(t *testing.T) {
	for _, side := range reInviteSides {
		t.Run(side.name, func(t *testing.T) {
			peer := newMediaSessionForTest(t)
			sess := newMediaSessionForTest(t)
			require.NoError(t, sess.RemoteSDP(peer.LocalSDP(false), false))

			moved := newMediaSessionForTest(t)
			call := side.setup(t, MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}, sess, func(req *sip.Request) *sip.Response {
				res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
				if req.IsInvite() {
					if err := moved.RemoteSDP(req.Body(), false); err == nil {
						res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
						res.SetBody(moved.LocalSDP(true))
					}
				}
				return res
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, call.reInvite(ctx))
			require.Equal(t, moved.Laddr.Port, call.med.MediaSession().Raddr.Port, "the answer was not applied")
			require.Nil(t, pendingFork(call.med), "the installed fork was left staged")
		})
	}
}

// TestDialogReInviteAnsweredAfterMediaClosed is a re-INVITE of ours whose 2xx
// arrives after the media of the call was closed. Close has taken what it
// closes by then, so a fork installed after it would be closed by nothing.
// The fork is not installed, the media drops it, and the sockets it ran on
// are released.
func TestDialogReInviteAnsweredAfterMediaClosed(t *testing.T) {
	d, med, h := newHeldReInviteDialog(t)
	sess := med.MediaSession()

	reinvited := make(chan error, 1)
	go func() { reinvited <- d.ReInvite(t.Context()) }()
	select {
	case <-h.requested:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not sent")
	}
	require.NotNil(t, pendingFork(med), "the offer was made by no fork")
	require.NoError(t, med.Close())
	h.release <- struct{}{}

	select {
	case err := <-reinvited:
		require.ErrorIs(t, err, errMediaClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE did not return")
	}
	require.True(t, sess == med.MediaSession(), "a fork was installed on closed media")
	require.Nil(t, pendingFork(med), "the fork that was not installed is still staged")
	reuse, err := net.ListenUDP("udp", &net.UDPAddr{IP: sess.Laddr.IP, Port: sess.Laddr.Port})
	require.NoError(t, err, "the fork's socket must be released")
	require.NoError(t, reuse.Close())
}

// TestDialogReInviteAnsweredWithoutSDP is a re-INVITE of ours answered 2xx
// with no session description, on either side. The 2xx is acknowledged and
// its Contact becomes the remote target (RFC 3261 section 12.2.1.2), but
// there is no answer to apply: the media session stays as it was, the fork
// that made the offer is dropped, and the caller is told why.
func TestDialogReInviteAnsweredWithoutSDP(t *testing.T) {
	moved := sip.Uri{User: "moved", Host: "127.0.0.9", Port: 5090}
	for _, side := range reInviteSides {
		t.Run(side.name, func(t *testing.T) {
			peer := newMediaSessionForTest(t)
			sess := newMediaSessionForTest(t)
			require.NoError(t, sess.RemoteSDP(peer.LocalSDP(false), false))

			call := side.setup(t, MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}, sess, func(req *sip.Request) *sip.Response {
				res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				res.AppendHeader(&sip.ContactHeader{Address: moved})
				return res
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.ErrorIs(t, call.reInvite(ctx), errReInviteAnswerWithoutSDP)
			require.True(t, sess == call.med.MediaSession(), "the media session was replaced")
			require.Nil(t, pendingFork(call.med), "the fork that got no answer is still staged")
			require.Equal(t, moved.String(), call.remoteContact().Address.String())
		})
	}
}

// offeringMedia is a recordingMedia that counts the offers it is asked for.
type offeringMedia struct {
	recordingMedia
	offers atomic.Int32
}

func (m *offeringMedia) onLocalSDP(ctx context.Context, answered bool, mode string) ([]byte, error) {
	if !answered {
		m.offers.Add(1)
	}
	return m.recordingMedia.onLocalSDP(ctx, answered, mode)
}

// ackWithoutTransaction sends requests as clientTxRequester does, but for an
// ACK, which it only shows to onRequest. An ACK has no transaction (RFC 3261
// section 17), and one made for it would hold a response nobody reads and
// timers that a clock the test controls could never get past.
type ackWithoutTransaction struct {
	clientTxRequester
}

func (r *ackWithoutTransaction) Request(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
	if req.IsAck() {
		r.onRequest(req)
		return nil, nil
	}
	return r.clientTxRequester.Request(ctx, req)
}

// newOfferingMediaClientCall returns an outgoing call, invited, answered and
// acknowledged over a client that answers with onRequest without a transport,
// whose media is an offeringMedia. Nothing of it opens a socket, so it can be
// used on a clock the test controls.
func newOfferingMediaClientCall(t *testing.T, onRequest func(*sip.Request) *sip.Response) (*DialogClientSession, *offeringMedia) {
	t.Helper()
	return newOfferingMediaClientCallOver(t, &ackWithoutTransaction{clientTxRequester{onRequest: onRequest}})
}

// newOfferingMediaClientCallOver is newOfferingMediaClientCall over a client
// whose requests requester takes.
func newOfferingMediaClientCallOver(t *testing.T, requester sipgo.ClientTransactionRequester) (*DialogClientSession, *offeringMedia) {
	t.Helper()
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	require.NoError(t, err)
	client.TxRequester = requester
	dialogUA := &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "dg", Host: "127.0.0.1", Port: 5060}}}

	sess, err := dialogUA.Invite(t.Context(), sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = sess.Close() })
	require.NoError(t, sess.WaitAnswer(t.Context(), sipgo.AnswerOptions{}))
	require.NoError(t, sess.Ack(t.Context()))

	d := &DialogClientSession{DialogClientSession: sess}
	med := &offeringMedia{}
	d.dialogCallbacks.mu.Lock()
	d.mediaHanshaker = med
	d.onMediaFailure = med.abort
	d.dialogCallbacks.mu.Unlock()
	return d, med
}

// TestDialogReInviteAnsweredAfterDialogEnded is a re-INVITE of ours whose 2xx
// arrives after the dialog has ended, here with our BYE. Nothing would close
// what the answer installs, so the media is handed no answer and is told to
// drop the offer it staged.
func TestDialogReInviteAnsweredAfterDialogEnded(t *testing.T) {
	h := newHeldReInvites()
	d, med := newOfferingMediaClientCallOver(t, h)

	reinvited := make(chan error, 1)
	go func() { reinvited <- d.ReInvite(t.Context()) }()
	select {
	case <-h.requested:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not sent")
	}
	require.NoError(t, d.Hangup(t.Context()))
	require.Equal(t, sip.DialogStateEnded, d.LoadState())
	h.release <- struct{}{}

	select {
	case err := <-reinvited:
		require.ErrorIs(t, err, errMediaClosed)
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE did not return")
	}
	answers, _, aborted := med.seen()
	assert.Empty(t, answers, "the media was handed an answer on an ended dialog")
	assert.Equal(t, 1, aborted, "the media was not told to drop the offer")
	// One of ours on a dialog that has ended is not sent at all.
	require.ErrorIs(t, d.ReInvite(t.Context()), errMediaClosed)
	assert.EqualValues(t, 1, med.offers.Load(), "an offer was made on an ended dialog")
}

// TestDialogReInviteAnswerNotKeyedEndsTheCall is a re-INVITE of ours whose
// answer the media took and then could not key, as when the handshake of a
// new DTLS association fails. The 2xx was acknowledged by then, so the peer
// has moved to a session that has no keys here: the call is ended with a BYE
// (RFC 5763 section 5), and the caller is told the update failed after its
// answer. An answer the media refuses outright leaves the call up.
func TestDialogReInviteAnswerNotKeyedEndsTheCall(t *testing.T) {
	answering := func(byes *atomic.Int32) func(*sip.Request) *sip.Response {
		return func(req *sip.Request) *sip.Response {
			res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			if req.Method == sip.BYE {
				byes.Add(1)
			}
			if req.IsInvite() {
				res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
				res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
				res.SetBody(peerSDP())
			}
			return res
		}
	}

	t.Run("not_keyed", func(t *testing.T) {
		var byes atomic.Int32
		d, med := newOfferingMediaClientCall(t, answering(&byes))
		errHandshake := errors.New("handshake failed")
		med.answerErr = fmt.Errorf("%w: %w", errMediaKeyingFailed, errHandshake)

		err := d.ReInvite(t.Context())
		require.ErrorIs(t, err, errMediaUpdateAfterAnswer)
		require.ErrorIs(t, err, errHandshake)
		assert.EqualValues(t, 1, byes.Load(), "the call was not ended with one BYE")
		assert.Equal(t, sip.DialogStateEnded, d.LoadState())
	})

	t.Run("refused", func(t *testing.T) {
		var byes atomic.Int32
		d, med := newOfferingMediaClientCall(t, answering(&byes))
		errRefused := errors.New("answer refused")
		med.answerErr = errRefused

		err := d.ReInvite(t.Context())
		require.ErrorIs(t, err, errRefused)
		require.NotErrorIs(t, err, errMediaUpdateAfterAnswer)
		assert.Zero(t, byes.Load(), "a refused answer ended the call")
		assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
		_, _, aborted := med.seen()
		assert.Equal(t, 1, aborted, "the media was not told to drop the offer")
	})
}

// answerKeyingMedia is media that keys the session an answer to an offer of
// ours asks for, as the handshake of a new DTLS association the peer never
// completes does, until the context it was given ends.
type answerKeyingMedia struct {
	*offeringMedia
	keying chan struct{}
}

func (m *answerKeyingMedia) onRemoteSDP(ctx context.Context, remoteSDP []byte, offered bool) error {
	if !offered {
		return m.offeringMedia.onRemoteSDP(ctx, remoteSDP, offered)
	}
	close(m.keying)
	<-ctx.Done()
	return fmt.Errorf("%w: %w", errMediaKeyingFailed, context.Cause(ctx))
}

// TestDialogReInviteKeyingEndsWithDialog pins that the keying a re-INVITE of
// ours waits for, once its answer is in, ends with the dialog as well as with
// the caller's context: a peer that has hung up never completes a handshake.
func TestDialogReInviteKeyingEndsWithDialog(t *testing.T) {
	d, med := newOfferingMediaClientCall(t, func(req *sip.Request) *sip.Response {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		if req.IsInvite() {
			res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			res.SetBody(peerSDP())
		}
		return res
	})
	keying := &answerKeyingMedia{offeringMedia: med, keying: make(chan struct{})}
	d.dialogCallbacks.mu.Lock()
	d.mediaHanshaker = keying
	d.dialogCallbacks.mu.Unlock()

	reinvited := make(chan error, 1)
	go func() { reinvited <- d.ReInvite(t.Context()) }()
	select {
	case <-keying.keying:
	case <-time.After(5 * time.Second):
		t.Fatal("the media was not handed the answer")
	}

	require.NoError(t, d.Hangup(t.Context()))
	select {
	case err := <-reinvited:
		require.ErrorIs(t, err, errMediaUpdateAfterAnswer)
	case <-time.After(5 * time.Second):
		t.Fatal("the end of the dialog did not end the keying")
	}
}

// TestDialogReInviteOffersActpass pins the offer of ReInvite on a DTLS-SRTP
// call, on either side: a subsequent offer carries a=setup:actpass (RFC 8842
// section 5.5), whatever role the endpoint plays. The media session in place
// has its role set, passive for the offerer the peer answered active, active
// for the answerer of an actpass offer, and offering from it signalled that
// role.
func TestDialogReInviteOffersActpass(t *testing.T) {
	for _, side := range reInviteSides {
		t.Run(side.name, func(t *testing.T) {
			sess := newDTLSMediaSession(t, testdata.ClientCertificate())
			peer := newDTLSMediaSession(t, testdata.ServerCertificate())
			if side.name == "inbound" {
				require.NoError(t, sess.RemoteSDP(peer.LocalSDP(false), false))
			} else {
				require.NoError(t, peer.RemoteSDP(sess.LocalSDP(false), false))
				require.NoError(t, sess.RemoteSDP(peer.LocalSDP(true), true))
			}

			offers := make(chan []byte, 1)
			call := side.setup(t, MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}, sess, func(req *sip.Request) *sip.Response {
				offers <- req.Body()
				return sip.NewResponseFromRequest(req, sip.StatusNotAcceptableHere, "Not Acceptable Here", nil)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := call.reInvite(ctx)
			var resErr sipgo.ErrDialogResponse
			require.ErrorAs(t, err, &resErr)
			require.Equal(t, "a=setup:actpass", sdpLineWithPrefix(t, <-offers, "a=setup:"))
		})
	}
}

// TestDialogReInviteRetriesAfter491 is a re-INVITE of ours answered 491 and
// then 200. RFC 3261 section 14.1 has it sent again after a wait, 2.1 to 4 s
// for the side that generated the Call-ID. While it waits no re-INVITE of
// ours is in progress, so one the peer retries meanwhile is handled and not
// refused 491 again, and the media holds no offer: the one the refused
// attempt staged is dropped, and the next attempt has a fresh one made.
func TestDialogReInviteRetriesAfter491(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		established := false
		var reInvites atomic.Int32
		d, med := newOfferingMediaClientCall(t, func(req *sip.Request) *sip.Response {
			res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			if !req.IsInvite() {
				return res
			}
			if established && reInvites.Add(1) == 1 {
				return sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil)
			}
			res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			res.SetBody(peerSDP())
			return res
		})
		established = true

		start := time.Now()
		reinvited := make(chan error, 1)
		go func() { reinvited <- d.ReInvite(t.Context()) }()
		synctest.Wait()

		// The first attempt was answered 491, and ours waits to send again.
		require.EqualValues(t, 1, reInvites.Load())
		require.EqualValues(t, 1, med.offers.Load())
		_, _, aborted := med.seen()
		require.Equal(t, 1, aborted, "the media was not told to drop the offer of the refused attempt")
		endPeerUpdate, free := d.beginPeerMediaUpdate()
		require.True(t, free, "a re-INVITE of ours is marked in progress while it waits to retry")
		endPeerUpdate()

		require.NoError(t, <-reinvited)
		waited := time.Since(start)
		assert.GreaterOrEqual(t, waited, 2100*time.Millisecond)
		assert.LessOrEqual(t, waited, 4*time.Second)
		assert.EqualValues(t, 2, reInvites.Load())
		assert.EqualValues(t, 2, med.offers.Load(), "the second attempt must have a fresh offer made")
		answers, _, aborted := med.seen()
		assert.Equal(t, []string{string(peerSDP())}, answers)
		assert.Equal(t, 1, aborted, "the offer that was answered was dropped")
	})
}

// TestDialogReInviteRetryWaitsForThePeers is a re-INVITE of ours that, when
// its wait after a 491 is over, finds one of the peer's in progress: it is
// sent again only once the peer's has ended (RFC 3261 section 14.1).
func TestDialogReInviteRetryWaitsForThePeers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		established := false
		var reInvites atomic.Int32
		d, _ := newOfferingMediaClientCall(t, func(req *sip.Request) *sip.Response {
			res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			if !req.IsInvite() {
				return res
			}
			if established && reInvites.Add(1) == 1 {
				return sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil)
			}
			res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			res.SetBody(peerSDP())
			return res
		})
		established = true

		reinvited := make(chan error, 1)
		go func() { reinvited <- d.ReInvite(t.Context()) }()
		synctest.Wait()
		endPeerUpdate, free := d.beginPeerMediaUpdate()
		require.True(t, free)

		// Past the longest wait, ours is held by the peer's alone.
		time.Sleep(5 * time.Second)
		synctest.Wait()
		require.EqualValues(t, 1, reInvites.Load(), "ours was sent again while the peer's was in progress")
		endPeerUpdate()
		require.NoError(t, <-reinvited)
		require.EqualValues(t, 2, reInvites.Load())
	})
}

// TestReInviteRetryWait pins the wait before a re-INVITE answered 491 is sent
// again, which RFC 3261 section 14.1 sets by who generated the Call-ID: 2.1 to
// 4 s for that side and 0 to 2 s for the other, in units of 10 ms, so that of
// two endpoints refused at once one retries first.
func TestReInviteRetryWait(t *testing.T) {
	for _, tt := range []struct {
		name        string
		callIDOwner bool
		min, max    time.Duration
	}{
		{name: "call_id_owner", callIDOwner: true, min: 2100 * time.Millisecond, max: 4 * time.Second},
		{name: "not_call_id_owner", callIDOwner: false, min: 0, max: 2 * time.Second},
	} {
		t.Run(tt.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				for range 2000 {
					start := time.Now()
					require.NoError(t, reInviteRetryWait(t.Context(), tt.callIDOwner))
					waited := time.Since(start)
					if waited < tt.min || waited > tt.max || waited%(10*time.Millisecond) != 0 {
						t.Fatalf("reInviteRetryWait(callIDOwner=%t) waited %s, want %s to %s in units of 10 ms", tt.callIDOwner, waited, tt.min, tt.max)
					}
				}
			})
		})
	}

	t.Run("ends_with_ctx", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			errStopped := errors.New("stopped")
			ctx, cancel := context.WithCancelCause(t.Context())
			cancel(errStopped)
			require.ErrorIs(t, reInviteRetryWait(ctx, true), errStopped)
		})
	})
}
