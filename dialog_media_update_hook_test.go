// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDialogMediaUpdateHooks checks that every update that replaces an
// established dialog's media session runs the media update hooks once, with
// the new session in place and the dialog's lock released, that an update that
// fails runs none, and that a hook once removed is not run.
func TestDialogMediaUpdateHooks(t *testing.T) {
	// reInvite hands d the peer's re-INVITE carrying body, and plays the
	// peer's ACK carrying ackBody to a 2xx, which the handling waits for (RFC
	// 3261 section 13.3.1.4). It returns once the ACK is read, with the status
	// the re-INVITE was answered with.
	reInvite := func(d *DialogClientSession, body, ackBody []byte) (int, error) {
		req := newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1)
		if body != nil {
			req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			req.SetBody(body)
		}
		ackErr := make(chan error, 1)
		inner := &fakeServerTransaction{}
		tx := newAckedServerTx(inner, func() {
			ackErr <- d.handleReInviteACK(newReInviteAck(req, ackBody), nil)
		})
		if err := d.handleReInvite(req, tx); err != nil {
			return 0, err
		}
		if inner.res.IsSuccess() {
			select {
			case <-tx.acked:
			case <-time.After(5 * time.Second):
				return 0, fmt.Errorf("the ACK was not read")
			}
			if err := <-ackErr; err != nil {
				return 0, err
			}
		}
		return inner.res.StatusCode, nil
	}

	for _, tc := range []struct {
		name string
		// update runs on its own goroutine, so it returns what went wrong
		update func(d *DialogClientSession) error
		// replaced is whether the update replaces the media session
		replaced bool
	}{
		{name: "PeerReInvite", replaced: true, update: func(d *DialogClientSession) error {
			status, err := reInvite(d, peerSDP(), nil)
			if err == nil && status != sip.StatusOK {
				err = fmt.Errorf("re-INVITE answered %d", status)
			}
			return err
		}},
		{name: "OurReInvite", replaced: true, update: func(d *DialogClientSession) error {
			return d.Hold(context.Background())
		}},
		{name: "AnswerInACK", replaced: true, update: func(d *DialogClientSession) error {
			// A re-INVITE without an offer is answered with ours, which the
			// ACK answers.
			status, err := reInvite(d, nil, peerSDP())
			if err == nil && status != sip.StatusOK {
				err = fmt.Errorf("re-INVITE answered %d", status)
			}
			return err
		}},
		{name: "AnswerAfterEarlyMedia", replaced: true, update: func(d *DialogClientSession) error {
			return d.applyRemoteSDP(&d.DialogMedia, peerSDP())
		}},
		{name: "RefusedReInvite", replaced: false, update: func(d *DialogClientSession) error {
			status, err := reInvite(d, []byte("v=0\r\n"), nil)
			if err == nil && status == sip.StatusOK {
				err = fmt.Errorf("a malformed offer was answered 200")
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newHookTestClientDialog(t)
			before := d.MediaSession()

			// A hook reads the session through the dialog's lock, which a hook
			// run with the lock held would wait on forever.
			var seen []*media.MediaSession
			remove := d.addMediaUpdateHook(func() { seen = append(seen, d.MediaSession()) })
			defer remove()
			removedRan := false
			d.addMediaUpdateHook(func() { removedRan = true })()

			updated := make(chan error, 1)
			go func() { updated <- tc.update(d) }()
			select {
			case err := <-updated:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the update did not return")
			}

			assert.False(t, removedRan, "a removed hook ran")
			if !tc.replaced {
				assert.Empty(t, seen, "an update that replaced nothing ran the hooks")
				return
			}
			require.Len(t, seen, 1, "the update must run the hook once")
			assert.NotSame(t, before, seen[0], "the hook ran before the new session was in place")
			assert.Same(t, d.MediaSession(), seen[0])
		})
	}
}

// newHookTestClientDialog returns an outgoing call, invited, answered with
// peerSDP and acknowledged, over a client that answers every request 200
// without a transport, and every INVITE with peerSDP and the peer's Contact.
func newHookTestClientDialog(t *testing.T) *DialogClientSession {
	t.Helper()
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		if req.IsInvite() {
			res.SetBody(peerSDP())
			res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
		}
		return res
	})
	ctx := context.Background()
	d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.Invite(ctx, InviteClientOptions{}))
	require.NoError(t, d.Ack(ctx))
	return d
}

// TestDialogMediaUpdateHooksAnswerInACKInbound checks that on an incoming
// call too, the answer an ACK brings to the offer in our 2xx to a re-INVITE of
// the peer's without one runs the hooks once, with the fork that made the
// offer, and took the answer, in place.
func TestDialogMediaUpdateHooksAnswerInACKInbound(t *testing.T) {
	d, _ := newByeTestDialog(t)
	res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
	res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
	d.InviteResponse = res
	confirm(t, d)

	peer := newMediaSessionForTest(t)
	sess := newMediaSessionForTest(t)
	require.NoError(t, sess.RemoteSDP(peer.LocalSDP()))
	rtpSess := media.NewRTPSession(sess)
	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	d.mu.Unlock()
	require.NoError(t, rtpSess.MonitorBackground())
	t.Cleanup(func() { _ = d.DialogMedia.Close() })

	var seen []*media.MediaSession
	remove := d.addMediaUpdateHook(func() { seen = append(seen, d.MediaSession()) })
	defer remove()

	req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
	tx := newRespondedServerTx()
	handled := make(chan error, 1)
	go func() { handled <- d.handleReInvite(req, tx) }()
	var ok *sip.Response
	select {
	case ok = <-tx.responded:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not answered")
	}
	require.Equal(t, sip.StatusOK, ok.StatusCode, "reason: %s", ok.Reason)
	assert.Empty(t, seen, "the offer in the 2xx ran the hooks before its answer")

	answerer := newMediaSessionForTest(t)
	require.NoError(t, answerer.RemoteSDP(ok.Body()))
	require.NoError(t, d.ReadAck(newReInviteAck(req, answerer.LocalSDP()), newByeServerTx()))
	select {
	case err := <-handled:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE handler did not return")
	}

	require.Len(t, seen, 1, "the answer in the ACK must run the hook once")
	assert.NotSame(t, sess, seen[0], "the hook ran before the fork was in place")
	assert.Same(t, d.MediaSession(), seen[0])
}
