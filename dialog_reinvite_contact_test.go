// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emiago/diago/media"
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

	med, err := dialog.Invite(ctx, InviteClientOptions{})
	require.NoError(t, err)
	require.NotNil(t, dialog.InviteResponse.Contact(), "the initial 2xx must carry a Contact")
	require.NoError(t, dialog.Ack(ctx))
	sess := med.MediaSession()

	for name, reInvite := range map[string]func(context.Context) error{
		"ReInvite":  dialog.ReInvite,
		"Hold":      dialog.Hold,
		"keepalive": dialog.reInviteKeepAlive,
	} {
		t.Run(name, func(t *testing.T) {
			var recovered any
			var err error
			func() {
				defer func() { recovered = recover() }()
				err = reInvite(ctx)
			}()
			require.Nil(t, recovered, "the re-INVITE panicked")
			require.ErrorIs(t, err, sipgo.ErrDialogInviteNoContact)
			require.True(t, sess == med.MediaSession(), "the media session was replaced")
			require.Nil(t, pendingFork(med), "the fork that made the offer is still staged")
		})
	}
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
		_, err = d.reInviteSend(ctx, req)
	}()
	require.Nil(t, recovered, "reInviteSend panicked")
	require.ErrorIs(t, err, sipgo.ErrDialogInviteNoContact)
}

// TestDialogServerReInvite2xxWithoutContact pins what each re-INVITE of an
// inbound call does with a 2xx that carries no Contact. ReInvite fails, as
// TestDialogServerReinviteContactMissing has its one attempt fail, and leaves
// the media as it was. Hold acknowledges the 2xx at the remote target in
// place and takes its answer: RFC 3261 section 13.2.2.4 has every 2xx
// acknowledged, and section 12.2.1.2 has it replace the remote target only
// when it carries a Contact, so the target stays where it was.
func TestDialogServerReInvite2xxWithoutContact(t *testing.T) {
	setup := func(t *testing.T) (reInviteCall, *media.MediaSession, *media.MediaSession) {
		peer := newMediaSessionForTest(t)
		sess := newMediaSessionForTest(t)
		require.NoError(t, sess.RemoteSDP(peer.LocalSDP(false), false))
		moved := newMediaSessionForTest(t)
		call := reInviteSides[0].setup(t, MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}, sess, func(req *sip.Request) *sip.Response {
			res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			if req.IsInvite() {
				if err := moved.RemoteSDP(req.Body(), false); err == nil {
					res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
					res.SetBody(moved.LocalSDP(true))
				}
			}
			return res
		})
		return call, sess, moved
	}
	require.Equal(t, "inbound", reInviteSides[0].name)

	t.Run("ReInvite", func(t *testing.T) {
		call, sess, _ := setup(t)
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		require.ErrorIs(t, call.reInvite(ctx), sipgo.ErrDialogInviteNoContact)
		require.True(t, sess == call.med.MediaSession(), "the media session was replaced")
		require.Nil(t, pendingFork(call.med), "the fork that made the offer is still staged")
	})

	t.Run("Hold", func(t *testing.T) {
		call, _, moved := setup(t)
		target := call.remoteContact().Address.String()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		require.NoError(t, call.hold(ctx))
		require.Equal(t, moved.Laddr.Port, call.med.MediaSession().Raddr.Port, "the answer was not applied")
		require.Equal(t, target, call.remoteContact().Address.String(), "a 2xx without Contact moved the remote target")
	})
}
