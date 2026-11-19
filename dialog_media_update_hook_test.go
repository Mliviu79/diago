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
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDialogMediaUpdateHooks checks that every update that replaces an
// established dialog's media session runs the media update hooks once, with
// the new session in place and the media's lock released, that an update that
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
		inner := newByeServerTx()
		tx := newAckedServerTx(inner, func() {
			ackErr <- d.handleReInviteACK(newReInviteAck(req, ackBody), nil)
		})
		handleErr := d.handleReInvite(t.Context(), req, tx)
		if len(inner.responses) == 0 {
			return 0, fmt.Errorf("the re-INVITE was not answered: %w", handleErr)
		}
		res := inner.responses[len(inner.responses)-1]
		if !res.IsSuccess() {
			// A refused re-INVITE also hands its handler what refused it.
			return res.StatusCode, nil
		}
		if handleErr != nil {
			return 0, handleErr
		}
		select {
		case <-tx.acked:
		case <-time.After(5 * time.Second):
			return 0, fmt.Errorf("the ACK was not read")
		}
		if err := <-ackErr; err != nil {
			return 0, err
		}
		return res.StatusCode, nil
	}

	for _, tc := range []struct {
		name string
		// update runs on its own goroutine, so it returns what went wrong
		update func(d *DialogClientSession, med *DialogMedia) error
		// replaced is whether the update replaces the media session
		replaced bool
	}{
		{name: "PeerReInvite", replaced: true, update: func(d *DialogClientSession, _ *DialogMedia) error {
			status, err := reInvite(d, peerSDP(), nil)
			if err == nil && status != sip.StatusOK {
				err = fmt.Errorf("re-INVITE answered %d", status)
			}
			return err
		}},
		{name: "OurReInvite", replaced: true, update: func(d *DialogClientSession, _ *DialogMedia) error {
			return d.Hold(context.Background())
		}},
		{name: "AnswerInACK", replaced: true, update: func(d *DialogClientSession, _ *DialogMedia) error {
			// A re-INVITE without an offer is answered with ours, which the
			// ACK answers.
			status, err := reInvite(d, nil, peerSDP())
			if err == nil && status != sip.StatusOK {
				err = fmt.Errorf("re-INVITE answered %d", status)
			}
			return err
		}},
		{name: "AnswerAfterEarlyMedia", replaced: true, update: func(d *DialogClientSession, med *DialogMedia) error {
			return d.applyRemoteSDP(med, peerSDP())
		}},
		{name: "RefusedReInvite", replaced: false, update: func(d *DialogClientSession, _ *DialogMedia) error {
			status, err := reInvite(d, []byte("v=0\r\n"), nil)
			if err == nil && status != sip.StatusNotAcceptableHere {
				err = fmt.Errorf("a malformed offer was answered %d", status)
			}
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, med := newHookTestClientCall(t)
			before := med.MediaSession()

			// A hook reads the session through the media's lock, which a hook
			// run with the lock held would wait on forever.
			var seen []*media.MediaSession
			remove := med.addMediaUpdateHook(func() { seen = append(seen, med.MediaSession()) })
			defer remove()
			removedRan := false
			med.addMediaUpdateHook(func() { removedRan = true })()

			updated := make(chan error, 1)
			go func() { updated <- tc.update(d, med) }()
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
			assert.Same(t, med.MediaSession(), seen[0])
		})
	}
}

// newHookTestClientCall returns an outgoing call, invited, answered with
// peerSDP and acknowledged, over a client that answers every request 200
// without a transport, and every INVITE with peerSDP and the peer's Contact,
// together with the media its Invite handed back.
func newHookTestClientCall(t *testing.T) (*DialogClientSession, *DialogMedia) {
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
	med, err := d.Invite(ctx, InviteClientOptions{})
	require.NoError(t, err)
	require.NoError(t, d.Ack(ctx))
	return d, med
}

// TestDialogMediaUpdateHooksAnswerInACKInbound checks that on an incoming
// call too, the answer an ACK brings to the offer in our 2xx to a re-INVITE of
// the peer's without one runs the hooks once, with the fork that made the
// offer, and took the answer, in place.
func TestDialogMediaUpdateHooksAnswerInACKInbound(t *testing.T) {
	d, med := newAnsweredTestServerCall(t)
	before := med.MediaSession()

	var seen []*media.MediaSession
	remove := med.addMediaUpdateHook(func() { seen = append(seen, med.MediaSession()) })
	defer remove()

	req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
	tx := newRespondedServerTx()
	handled := make(chan error, 1)
	go func() { handled <- d.handleReInvite(t.Context(), req, tx) }()
	var ok *sip.Response
	select {
	case ok = <-tx.responded:
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE was not answered")
	}
	require.Equal(t, sip.StatusOK, ok.StatusCode, "reason: %s", ok.Reason)
	med.mu.Lock()
	ranAtOffer := len(seen)
	med.mu.Unlock()
	assert.Zero(t, ranAtOffer, "the offer in the 2xx ran the hooks before its answer")

	answerer := newMediaSessionForTest(t)
	require.NoError(t, answerer.RemoteSDP(ok.Body(), false))
	require.NoError(t, d.ReadAck(newReInviteAck(req, answerer.LocalSDP(true)), newByeServerTx()))
	select {
	case err := <-handled:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the re-INVITE handler did not return")
	}

	require.Len(t, seen, 1, "the answer in the ACK must run the hook once")
	assert.NotSame(t, before, seen[0], "the hook ran before the fork was in place")
	assert.Same(t, med.MediaSession(), seen[0])
}

// TestDialogMediaUpdateHooksStagedFork checks that an offer of the peer's
// asking for a new DTLS association, which is answered from a fork that is
// installed only once it is keyed, runs the hooks when that fork is installed
// and not when it is staged: until then the session in place carries the
// media, and a bridge told earlier would read the codec of that session.
func TestDialogMediaUpdateHooksStagedFork(t *testing.T) {
	_, answerer := newEstablishedDTLSMedia(t)
	d := newICEDialogMedia(t, answerer)

	seen := make(chan *media.MediaSession, 4)
	remove := d.addMediaUpdateHook(func() { seen <- d.MediaSession() })
	defer remove()

	// The call moves to another peer, with a certificate of its own.
	other, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	peer := newDTLSMediaSession(t, other)

	ctx := t.Context()
	require.NoError(t, d.onRemoteSDP(ctx, peer.LocalSDP(false), false))
	answer, err := d.onLocalSDP(ctx, true, "")
	require.NoError(t, err)
	require.Same(t, answerer, d.MediaSession(), "a fork without keys was installed")
	require.Empty(t, seen, "a staged fork ran the hooks")

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

	require.Len(t, seen, 1, "the installed fork must run the hook once")
	installed := <-seen
	assert.NotSame(t, answerer, installed, "the hook ran before the fork was in place")
	assert.Same(t, d.MediaSession(), installed)
}
