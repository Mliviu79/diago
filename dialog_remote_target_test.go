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
	reInvite      func(ctx context.Context) error
	hangup        func(ctx context.Context) error
	remoteContact func() *sip.ContactHeader
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
				reInvite:      d.ReInvite,
				hangup:        d.Hangup,
				remoteContact: d.RemoteContact,
			}
		},
	},
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
