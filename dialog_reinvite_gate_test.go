// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// heldReInvites is a client that answers every request 200 without a
// transport, the INVITE with peerSDP, and holds each INVITE after the first
// until the test releases it.
type heldReInvites struct {
	mu      sync.Mutex
	log     []string
	invites int
	// requested receives each held INVITE, and release lets it be answered.
	requested chan struct{}
	release   chan struct{}
}

func newHeldReInvites() *heldReInvites {
	return &heldReInvites{requested: make(chan struct{}, 4), release: make(chan struct{}, 4)}
}

func (h *heldReInvites) record(entry string) {
	h.mu.Lock()
	h.log = append(h.log, entry)
	h.mu.Unlock()
}

func (h *heldReInvites) entries() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.log...)
}

func (h *heldReInvites) onRequest(req *sip.Request) *sip.Response {
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	if !req.IsInvite() {
		return res
	}
	h.mu.Lock()
	h.invites++
	held := h.invites > 1
	h.mu.Unlock()
	if held {
		h.record("our re-INVITE")
		h.requested <- struct{}{}
		select {
		case <-h.release:
		case <-time.After(10 * time.Second):
		}
	}
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	res.SetBody(peerSDP())
	return res
}

// newHeldReInviteDialog returns an outgoing call, invited, answered with
// peerSDP and acknowledged, whose later INVITEs h holds, and the media its
// Invite handed back.
func newHeldReInviteDialog(t *testing.T) (*DialogClientSession, *DialogMedia, *heldReInvites) {
	t.Helper()
	h := newHeldReInvites()
	dg := testDiagoClient(t, h.onRequest)
	ctx := context.Background()
	d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	med, err := d.Invite(ctx, InviteClientOptions{})
	require.NoError(t, err)
	require.NoError(t, d.Ack(ctx))
	return d, med, h
}

// TestDialogReInviteWaitsForPeerMediaUpdate is a Hold made while a re-INVITE
// of the peer's is being handled, its media update callback still running.
// RFC 3261 section 14.1 has a UAC start no re-INVITE while another INVITE
// transaction is in progress in either direction, so our re-INVITE goes out
// once the peer's has been answered, and forks the session the peer's
// installed.
func TestDialogReInviteWaitsForPeerMediaUpdate(t *testing.T) {
	d, med, h := newHeldReInviteDialog(t)
	peerReInvite := newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1)
	peerReInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	peerReInvite.SetBody(peerSDP())

	inCallback := make(chan struct{})
	releaseCallback := make(chan struct{})
	// The media tells the callback of the update our own re-INVITE makes
	// too: only the peer's is held.
	var peerUpdate sync.Once
	med.mu.Lock()
	med.onMediaUpdate = func(*DialogMedia) {
		peerUpdate.Do(func() {
			close(inCallback)
			select {
			case <-releaseCallback:
			case <-time.After(10 * time.Second):
			}
		})
	}
	med.mu.Unlock()

	handled := make(chan error, 1)
	go func() {
		handled <- d.handleReInvite(t.Context(), peerReInvite, newAckedServerTx(&loggedPeerTx{byeServerTx: newByeServerTx(), h: h}, func() {
			_ = d.handleReInviteACK(newReInviteAck(peerReInvite, nil), newByeServerTx())
		}))
	}()
	select {
	case <-inCallback:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer's media update did not start")
	}

	held := make(chan error, 1)
	go func() { held <- d.Hold(context.Background()) }()
	// Our re-INVITE must not go out while the peer's is being handled.
	select {
	case <-h.requested:
	case <-time.After(500 * time.Millisecond):
	}
	close(releaseCallback)
	select {
	case err := <-handled:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the peer's re-INVITE was not handled")
	}
	h.release <- struct{}{}
	select {
	case <-h.requested:
	default:
	}
	select {
	case err := <-held:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Hold did not return")
	}
	require.Equal(t, []string{"peer's re-INVITE 200", "our re-INVITE"}, h.entries())
}

// TestDialogReInviteGlare is a re-INVITE of the peer's arriving while one of
// ours awaits its answer. RFC 3261 section 14.2 has it answered 491.
func TestDialogReInviteGlare(t *testing.T) {
	d, _, h := newHeldReInviteDialog(t)
	held := make(chan error, 1)
	go func() { held <- d.Hold(context.Background()) }()
	select {
	case <-h.requested:
	case <-time.After(5 * time.Second):
		t.Fatal("our re-INVITE was not sent")
	}

	peerReInvite := newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1)
	peerReInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	peerReInvite.SetBody(peerSDP())
	tx := newByeServerTx()
	require.NoError(t, d.handleReInvite(t.Context(), peerReInvite, tx))
	h.release <- struct{}{}
	select {
	case err := <-held:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Hold did not return")
	}
	require.Len(t, tx.responses, 1)
	require.Equal(t, sip.StatusRequestPending, tx.responses[0].StatusCode)
}

// loggedPeerTx is a byeServerTx that records the peer's re-INVITE being
// answered in the log of h.
type loggedPeerTx struct {
	*byeServerTx
	h *heldReInvites
}

func (tx *loggedPeerTx) Respond(_ context.Context, res *sip.Response) error {
	tx.h.record(fmt.Sprintf("peer's re-INVITE %d", res.StatusCode))
	return nil
}
