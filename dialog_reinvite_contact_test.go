// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// reInviteNoContactSDP is the answer both far ends in this file send.
func reInviteNoContactSDP() []byte {
	return sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW})
}

// TestDialogClientReinviteContactMissing checks a 2xx to a re-INVITE that
// carries no Contact fails the re-INVITE rather than panicking on the missing
// ACK target. RFC 3261 §12.1.1 requires the Contact on a 2xx to INVITE.
func TestDialogClientReinviteContactMissing(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", reInviteNoContactSDP())
		if _, inDialog := req.To().Params.Get("tag"); !inDialog {
			res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5099}})
		}
		return res
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	dialog, err := dg.NewDialog(sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5099}, NewDialogOptions{})
	require.NoError(t, err)
	defer dialog.Close()

	require.NoError(t, dialog.Invite(ctx, InviteClientOptions{}))
	require.NotNil(t, dialog.InviteResponse.Contact(), "the initial 2xx must carry a Contact")
	require.NoError(t, dialog.Ack(ctx))

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		err = dialog.ReInvite(ctx)
	}()
	require.Nil(t, recovered, "ReInvite panicked")
	require.ErrorIs(t, err, sipgo.ErrDialogInviteNoContact)
}

// TestDialogServerReinviteContactMissing checks the same on an inbound dialog:
// a re-INVITE this side sends, answered 2xx without Contact, is an error and not
// a panic.
func TestDialogServerReinviteContactMissing(t *testing.T) {
	caller := sip.Uri{User: "bob", Host: "127.0.0.2", Port: 5060}
	recipient := sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}

	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	client, _ := sipgo.NewClient(ua)
	client.TxRequester = &clientTxRequester{onRequest: func(req *sip.Request) *sip.Response {
		return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", reInviteNoContactSDP())
	}}

	invite := sip.NewRequest(sip.INVITE, recipient)
	invite.AppendHeader(&sip.ContactHeader{Address: caller})
	fromParams := sip.NewParams()
	fromParams.Add("tag", "caller-tag")
	invite.AppendHeader(&sip.FromHeader{Address: caller, Params: fromParams})
	invite.AppendHeader(&sip.ToHeader{Address: recipient, Params: sip.NewParams()})
	invite.AppendHeader(sip.NewHeader("Call-ID", "reinvite-no-contact-call-id"))
	invite.AppendHeader(&sip.CSeqHeader{SeqNo: 100, MethodName: sip.INVITE})

	dialogUA := &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: recipient}}
	sess, err := dialogUA.ReadInvite(invite, newByeServerTx())
	require.NoError(t, err)
	answer := sip.NewResponseFromRequest(invite, sip.StatusOK, "OK", reInviteNoContactSDP())
	answer.AppendHeader(&sip.ContactHeader{Address: recipient})
	sess.InviteResponse = answer
	d := &DialogServerSession{DialogServerSession: sess}

	req := sip.NewRequest(sip.INVITE, caller)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(reInviteNoContactSDP())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, err = d.reInviteDo(ctx, req)
	}()
	require.Nil(t, recovered, "reInviteDo panicked")
	require.ErrorIs(t, err, sipgo.ErrDialogInviteNoContact)
}
