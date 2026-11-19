// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
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

// sentInvitesAndAcks records the INVITEs and the ACKs a call sends.
type sentInvitesAndAcks struct {
	mu      sync.Mutex
	invites []*sip.Request
	acks    []*sip.Request
}

func (s *sentInvitesAndAcks) record(req *sip.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch req.Method {
	case sip.INVITE:
		s.invites = append(s.invites, req)
	case sip.ACK:
		s.acks = append(s.acks, req)
	}
}

func (s *sentInvitesAndAcks) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invites, s.acks = nil, nil
}

// requireAckedAt checks that the call sent one re-INVITE since the last reset
// and acknowledged its 2xx once, with an ACK addressed to target that carries
// the CSeq number of that re-INVITE (RFC 3261 section 13.2.2.4).
func (s *sentInvitesAndAcks) requireAckedAt(t *testing.T, target string) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	require.Len(t, s.invites, 1, "re-INVITEs sent")
	require.Len(t, s.acks, 1, "the 2xx to a re-INVITE must be acknowledged once")
	require.Equal(t, target, s.acks[0].Recipient.String(), "ACK Request-URI")
	require.Equal(t, s.invites[0].CSeq().SeqNo, s.acks[0].CSeq().SeqNo, "ACK CSeq number")
}

// TestDialogClientReinviteContactMissing checks a 2xx to a re-INVITE that
// carries no Contact is taken like any other 2xx rather than panicking on the
// missing ACK target. The header table of RFC 3261 section 20 has a 2xx to
// INVITE carry a Contact, which binds the peer that sends it; for the side
// that receives it, section 12.2.1.2 replaces the remote target with the
// Contact of a 2xx "if present".
//
// So the 2xx is acknowledged, as section 13.2.2.4 has every 2xx, at the remote
// target in place, which stays where an earlier re-INVITE moved it; the
// re-INVITE succeeds; and the answer in the 2xx is applied: section 14.1
// leaves the session unchanged after a non-2xx final response only, and the
// peer, whose 2xx was acknowledged, runs the session it answered with.
func TestDialogClientReinviteContactMissing(t *testing.T) {
	moved := sip.Uri{User: "alice", Host: "127.0.0.9", Port: 5090}
	sent := &sentInvitesAndAcks{}
	// withContact has the peer put the Contact moved in its 2xx to a
	// re-INVITE. answerPort is the port of the last answer it gave to one,
	// each from a port of its own.
	var withContact atomic.Bool
	var answerPort atomic.Int32
	answerPort.Store(34455)
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		sent.record(req)
		if _, inDialog := req.To().Params.Get("tag"); !inDialog {
			res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", reInviteNoContactSDP())
			res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5099}})
			return res
		}
		if !req.IsInvite() {
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		}
		answer := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), int(answerPort.Add(1)), sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW})
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", answer)
		if withContact.Load() {
			res.AppendHeader(&sip.ContactHeader{Address: moved})
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

	// A re-INVITE answered with a Contact moves the remote target first, so
	// that a 2xx without one is seen to leave it there, and not to put it
	// back at the Contact of the answer to the INVITE.
	withContact.Store(true)
	require.NoError(t, dialog.ReInvite(ctx))
	withContact.Store(false)
	target := dialog.RemoteContact().Address.String()
	require.Equal(t, moved.String(), target, "the Contact of a 2xx did not move the remote target")

	for _, tc := range []struct {
		name     string
		reInvite func(context.Context) error
		// offers tells whether the re-INVITE carries an offer of ours, which
		// the 2xx then answers.
		offers bool
	}{
		{name: "ReInvite", reInvite: dialog.ReInvite, offers: true},
		{name: "Hold", reInvite: dialog.Hold, offers: true},
		{name: "Unhold", reInvite: dialog.Unhold, offers: true},
		{name: "keepalive", reInvite: dialog.reInviteKeepAlive},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent.reset()
			var recovered any
			var err error
			func() {
				defer func() { recovered = recover() }()
				err = tc.reInvite(ctx)
			}()
			require.Nil(t, recovered, "the re-INVITE panicked")
			require.NoError(t, err)
			sent.requireAckedAt(t, target)
			require.Equal(t, target, dialog.RemoteContact().Address.String(), "a 2xx without Contact moved the remote target")
			require.Nil(t, pendingFork(med), "a fork was left staged")
			if tc.offers {
				require.Equal(t, int(answerPort.Load()), med.MediaSession().Raddr.Port, "the answer was not applied")
			}
		})
	}
}

// TestDialogServerReinviteContactMissing checks the same on an inbound dialog:
// a re-INVITE this side sends, answered 2xx without Contact, does not panic,
// and its 2xx is acknowledged at the remote target in place, the caller's
// Contact, and returned as any other 2xx is.
func TestDialogServerReinviteContactMissing(t *testing.T) {
	caller := sip.Uri{User: "bob", Host: "127.0.0.2", Port: 5060}
	recipient := sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}

	sent := &sentInvitesAndAcks{}
	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	client, _ := sipgo.NewClient(ua)
	client.TxRequester = &clientTxRequester{onRequest: func(req *sip.Request) *sip.Response {
		sent.record(req)
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
	var res *sip.Response
	func() {
		defer func() { recovered = recover() }()
		res, err = d.reInviteSend(ctx, req)
	}()
	require.Nil(t, recovered, "reInviteSend panicked")
	require.NoError(t, err)
	require.Equal(t, sip.StatusOK, res.StatusCode)
	sent.requireAckedAt(t, caller.String())
}

// TestDialogReInvite2xxWithoutContact pins what a re-INVITE of ours does with
// a 2xx that carries no Contact, on either side of a call and whatever the
// re-INVITE asks for. It acknowledges the 2xx, once, at the remote target in
// place: RFC 3261 section 13.2.2.4 has every 2xx acknowledged, and section
// 12.2.1.2 has it replace the remote target only when it carries a Contact,
// so the target stays where it was. And it takes the answer, as it does from
// any other 2xx: the peer has answered and been acknowledged, so it runs the
// session it answered with, on hold where it was asked to hold.
func TestDialogReInvite2xxWithoutContact(t *testing.T) {
	for _, side := range reInviteSides {
		for _, tc := range []struct {
			name     string
			reInvite func(ctx context.Context, c reInviteCall) error
		}{
			{name: "ReInvite", reInvite: func(ctx context.Context, c reInviteCall) error { return c.reInvite(ctx) }},
			{name: "Hold", reInvite: func(ctx context.Context, c reInviteCall) error { return c.hold(ctx) }},
			{name: "Unhold", reInvite: func(ctx context.Context, c reInviteCall) error { return c.unhold(ctx) }},
		} {
			t.Run(side.name+"/"+tc.name, func(t *testing.T) {
				peer := newMediaSessionForTest(t)
				sess := newMediaSessionForTest(t)
				require.NoError(t, sess.RemoteSDP(peer.LocalSDP(false), false))
				moved := newMediaSessionForTest(t)
				sent := &sentInvitesAndAcks{}
				call := side.setup(t, MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}, sess, func(req *sip.Request) *sip.Response {
					sent.record(req)
					res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
					if req.IsInvite() {
						if err := moved.RemoteSDP(req.Body(), false); err == nil {
							res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
							res.SetBody(moved.LocalSDP(true))
						}
					}
					return res
				})
				target := call.remoteContact().Address.String()
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
				defer cancel()
				require.NoError(t, tc.reInvite(ctx, call))
				sent.requireAckedAt(t, target)
				require.Equal(t, moved.Laddr.Port, call.med.MediaSession().Raddr.Port, "the answer was not applied")
				require.Nil(t, pendingFork(call.med), "the installed fork was left staged")
				require.Equal(t, target, call.remoteContact().Address.String(), "a 2xx without Contact moved the remote target")
			})
		}
	}
}

// TestSessionRefresh2xxWithoutContact is a session refresh answered 2xx
// without Contact. The 2xx is acknowledged at the remote target in place, its
// answer goes to the media, and refreshing goes on: such a 2xx refreshes the
// session as any other does.
func TestSessionRefresh2xxWithoutContact(t *testing.T) {
	sent := &sentInvitesAndAcks{}
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	require.NoError(t, err)
	client.TxRequester = &clientTxRequester{onRequest: func(req *sip.Request) *sip.Response {
		sent.record(req)
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		if req.IsInvite() {
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			res.SetBody(reInviteNoContactSDP())
		}
		return res
	}}
	d := newTestDialogOverUA(t, newByeServerTx(), &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}}})
	res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
	res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
	d.InviteResponse = res
	confirm(t, d)
	med := &recordingMedia{}
	med.register(d)
	target := d.RemoteContact().Address.String()

	require.Equal(t, refreshGoesOn, d.refreshSession(t.Context(), nil))
	sent.requireAckedAt(t, target)
	require.Equal(t, target, d.RemoteContact().Address.String(), "a 2xx without Contact moved the remote target")
	med.mu.Lock()
	answers := med.answers
	med.mu.Unlock()
	require.Equal(t, []string{string(reInviteNoContactSDP())}, answers, "answers handed to the media")
}

// TestReInviteAckWithoutRemoteTarget is a 2xx without Contact on a dialog that
// has no remote target: there is nowhere to acknowledge it, which is reported
// with sipgo.ErrDialogInviteNoContact, and no ACK is sent.
func TestReInviteAckWithoutRemoteTarget(t *testing.T) {
	req := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "127.0.0.2", Port: 5060})
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
	acks := 0
	err := reInviteAck(t.Context(), res,
		func(context.Context, sip.Uri, []byte) error { acks++; return nil },
		func() *sip.ContactHeader { return nil },
	)
	require.ErrorIs(t, err, sipgo.ErrDialogInviteNoContact)
	require.Zero(t, acks, "ACKs sent")
}
