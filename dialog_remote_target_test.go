// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// byeTargets records the Request-URI of every BYE a dialog sends.
type byeTargets struct {
	mu   sync.Mutex
	uris []string
}

func (b *byeTargets) record(req *sip.Request) {
	if req.Method != sip.BYE {
		return
	}
	b.mu.Lock()
	b.uris = append(b.uris, req.Recipient.String())
	b.mu.Unlock()
}

func (b *byeTargets) sent() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.uris...)
}

// remoteTargetDialog is a confirmed dialog with media, seen through what the
// remote target tests drive and read.
type remoteTargetDialog struct {
	// initial is the remote target the dialog was established with.
	initial sip.Uri
	// peerReInvite builds a re-INVITE of the peer's whose Contact is contact.
	peerReInvite   func(contact sip.Uri) *sip.Request
	handleReInvite func(req *sip.Request, tx sip.ServerTransaction) error
	readAck        func(ack *sip.Request) error
	reInvite       func(ctx context.Context) error
	hangup         func(ctx context.Context) error
	remoteContact  func() *sip.ContactHeader
}

// remoteTargetSides builds, for each side of a call, a confirmed dialog with
// media whose requests after it is set up the peer answers with onRequest.
var remoteTargetSides = []struct {
	name  string
	setup func(t *testing.T, onRequest func(*sip.Request) *sip.Response) remoteTargetDialog
}{
	{
		name: "inbound",
		setup: func(t *testing.T, onRequest func(*sip.Request) *sip.Response) remoteTargetDialog {
			ua, err := sipgo.NewUA()
			require.NoError(t, err)
			t.Cleanup(func() { _ = ua.Close() })
			client, err := sipgo.NewClient(ua)
			require.NoError(t, err)
			client.TxRequester = &clientTxRequester{onRequest: onRequest}
			d, _ := newAnsweredTestServerCallOverUA(t, &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}}})
			return remoteTargetDialog{
				initial: d.InviteRequest.Contact().Address,
				peerReInvite: func(contact sip.Uri) *sip.Request {
					req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
					req.ReplaceHeader(&sip.ContactHeader{Address: contact})
					return req
				},
				handleReInvite: func(req *sip.Request, tx sip.ServerTransaction) error {
					return d.handleReInvite(t.Context(), req, tx)
				},
				readAck:       func(ack *sip.Request) error { return d.ReadAck(t.Context(), ack, newByeServerTx()) },
				reInvite:      d.ReInvite,
				hangup:        d.Hangup,
				remoteContact: d.RemoteContact,
			}
		},
	},
	{
		name: "outbound",
		setup: func(t *testing.T, onRequest func(*sip.Request) *sip.Response) remoteTargetDialog {
			initial := sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}
			established := false
			dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
				if established {
					return onRequest(req)
				}
				res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				if req.IsInvite() {
					res.AppendHeader(&sip.ContactHeader{Address: initial})
					res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
					res.SetBody(peerSDP())
				}
				return res
			})
			ctx := context.Background()
			d, err := dg.NewDialog(initial, NewDialogOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = d.Close() })
			_, err = d.Invite(ctx, InviteClientOptions{})
			require.NoError(t, err)
			require.NoError(t, d.Ack(ctx))
			established = true
			return remoteTargetDialog{
				initial: initial,
				peerReInvite: func(contact sip.Uri) *sip.Request {
					req := newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1)
					req.ReplaceHeader(&sip.ContactHeader{Address: contact})
					return req
				},
				handleReInvite: func(req *sip.Request, tx sip.ServerTransaction) error {
					return d.handleReInvite(t.Context(), req, tx)
				},
				readAck:       func(ack *sip.Request) error { return d.handleReInviteACK(t.Context(), ack, newByeServerTx()) },
				reInvite:      d.ReInvite,
				hangup:        d.Hangup,
				remoteContact: d.RemoteContact,
			}
		},
	},
}

// TestDialogPeerReInviteMovesTheRemoteTargetOnlyWhenAccepted pins when a
// re-INVITE of the peer's carrying a new Contact moves the dialog's remote
// target, on either side. RFC 6141 section 4.6 has the UA replace the remote
// target on sending a 2xx to the target refresh request, and not update it
// when it answers with an error response. A rejected re-INVITE kept its
// Contact as the target, and our BYE then went there. The BYE goes to the
// remote target (RFC 3261 section 12.2.1.1), whichever way it is sent.
func TestDialogPeerReInviteMovesTheRemoteTargetOnlyWhenAccepted(t *testing.T) {
	moved := sip.Uri{User: "moved", Host: "127.0.0.9", Port: 5090}
	for _, side := range remoteTargetSides {
		t.Run(side.name+"/rejected", func(t *testing.T) {
			byes := &byeTargets{}
			d := side.setup(t, func(req *sip.Request) *sip.Response {
				byes.record(req)
				return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			})

			req := d.peerReInvite(moved)
			req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			req.SetBody([]byte("not a session description"))
			tx := newByeServerTx()
			// The handler reports the offer it refused.
			require.Error(t, d.handleReInvite(req, tx))
			require.Len(t, tx.responses, 1)
			require.False(t, tx.responses[0].IsSuccess(), "the re-INVITE was not rejected: %d", tx.responses[0].StatusCode)

			assert.Equal(t, d.initial.String(), d.remoteContact().Address.String(), "the rejected re-INVITE moved the remote target")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, d.hangup(ctx))
			assert.Equal(t, []string{d.initial.String()}, byes.sent())
		})

		t.Run(side.name+"/accepted", func(t *testing.T) {
			byes := &byeTargets{}
			d := side.setup(t, func(req *sip.Request) *sip.Response {
				byes.record(req)
				return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			})

			req := d.peerReInvite(moved)
			req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			req.SetBody(peerSDP())
			inner := newByeServerTx()
			tx := newAckedServerTx(inner, func() {
				_ = d.readAck(newReInviteAck(req, nil))
			})
			require.NoError(t, d.handleReInvite(req, tx))
			require.Len(t, inner.responses, 1)
			require.True(t, inner.responses[0].IsSuccess(), "the re-INVITE was not accepted: %d", inner.responses[0].StatusCode)

			assert.Equal(t, moved.String(), d.remoteContact().Address.String(), "the accepted re-INVITE did not move the remote target")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, d.hangup(ctx))
			assert.Equal(t, []string{moved.String()}, byes.sent())
		})
	}
}

// TestDialogByeGoesToTheTargetOurReInviteAnswerSet pins that the BYE the
// dialog sends itself goes to the remote target the 2xx to our re-INVITE set
// (RFC 3261 sections 12.2.1.1 and 12.2.1.2), on either side. It went to the
// Contact the dialog was established with, the INVITE's on an inbound dialog
// and its 2xx's on an outbound one, whatever target refresh followed.
func TestDialogByeGoesToTheTargetOurReInviteAnswerSet(t *testing.T) {
	moved := sip.Uri{User: "moved", Host: "127.0.0.9", Port: 5090}
	for _, side := range remoteTargetSides {
		t.Run(side.name, func(t *testing.T) {
			byes := &byeTargets{}
			d := side.setup(t, func(req *sip.Request) *sip.Response {
				byes.record(req)
				res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				if req.IsInvite() {
					res.AppendHeader(&sip.ContactHeader{Address: moved})
					res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
					res.SetBody(peerSDP())
				}
				return res
			})

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, d.reInvite(ctx))
			require.Equal(t, moved.String(), d.remoteContact().Address.String())
			require.NoError(t, d.hangup(ctx))
			assert.Equal(t, []string{moved.String()}, byes.sent())
		})
	}
}

// TestDialogPeerReInviteWithoutContactKeepsTheRemoteTarget pins that a
// re-INVITE of the peer's that carries no Contact leaves the remote target
// where it is, on either side, once an earlier target refresh has moved it.
// RFC 3261 section 12.2.2 has the UAS replace the remote target with the URI
// from the Contact of a target refresh request "if present". Accepting such a
// re-INVITE put the target back at the Contact the dialog was established
// with, and our BYE then went there.
func TestDialogPeerReInviteWithoutContactKeepsTheRemoteTarget(t *testing.T) {
	moved := sip.Uri{User: "moved", Host: "127.0.0.9", Port: 5090}
	for _, side := range remoteTargetSides {
		t.Run(side.name, func(t *testing.T) {
			byes := &byeTargets{}
			d := side.setup(t, func(req *sip.Request) *sip.Response {
				byes.record(req)
				return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			})
			accept := func(req *sip.Request) {
				t.Helper()
				req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
				req.SetBody(peerSDP())
				inner := newByeServerTx()
				tx := newAckedServerTx(inner, func() {
					_ = d.readAck(newReInviteAck(req, nil))
				})
				require.NoError(t, d.handleReInvite(req, tx))
				require.Len(t, inner.responses, 1)
				require.True(t, inner.responses[0].IsSuccess(), "the re-INVITE was not accepted: %d", inner.responses[0].StatusCode)
			}

			accept(d.peerReInvite(moved))
			require.Equal(t, moved.String(), d.remoteContact().Address.String(), "the accepted re-INVITE did not move the remote target")

			req := d.peerReInvite(moved)
			req.CSeq().SeqNo++
			req.RemoveHeader("Contact")
			require.Nil(t, req.Contact())
			accept(req)

			assert.Equal(t, moved.String(), d.remoteContact().Address.String(), "a re-INVITE without Contact moved the remote target")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, d.hangup(ctx))
			assert.Equal(t, []string{moved.String()}, byes.sent())
		})
	}
}
