// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"sync"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// TestReferTransactionAcceptCarriesReferredBy checks that the INVITE a
// ReferTransaction's Accept triggers carries the REFER's Referred-By, which
// the referee includes in the triggered request (RFC 3892 section 3), whether
// the REFER spells the header in full or in its compact form "b".
func TestReferTransactionAcceptCarriesReferredBy(t *testing.T) {
	const referredBy = "<sip:transferor@example.com>"

	for _, tc := range []struct {
		name   string
		header string
	}{
		{name: "full", header: "Referred-By"},
		{name: "compact", header: "b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var (
				mu      sync.Mutex
				invites []*sip.Request
			)
			dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
				res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				if req.IsInvite() {
					mu.Lock()
					invites = append(invites, req)
					mu.Unlock()
					res.SetBody(peerSDP())
				}
				return res
			})
			// The referring dialog the NOTIFYs of the subscription go in.
			referrer, _ := newAnsweredTestClientDialog(t)

			refer := sip.NewRequest(sip.REFER, sip.Uri{User: "transferee", Host: "127.0.0.1", Port: 5060})
			refer.AppendHeader(sip.NewHeader("Refer-To", "<sip:target@127.0.0.1:5090>"))
			refer.AppendHeader(sip.NewHeader(tc.header, referredBy))
			refer.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "transferor", Host: "127.0.0.1", Port: 5070}})
			tx := newByeServerTx()

			ctx := context.Background()
			err := dialogHandleReferTransaction(ctx, referrer, dg, refer, tx, func(rtx ReferTransaction) error {
				referDialog, err := rtx.Accept(ctx, InviteClientOptions{})
				if err != nil {
					return err
				}
				return referDialog.Close()
			})
			require.NoError(t, err)

			require.Len(t, tx.responses, 1)
			require.Equal(t, 202, tx.responses[0].StatusCode, "the REFER is accepted")

			mu.Lock()
			defer mu.Unlock()
			require.Len(t, invites, 1, "Accept sends one INVITE to the Refer-To target")
			require.Equal(t, "target", invites[0].Recipient.User)
			h := invites[0].GetHeader("Referred-By")
			require.NotNil(t, h, "the triggered INVITE carries the REFER's Referred-By")
			require.Equal(t, referredBy, h.Value())
		})
	}
}

// referNotify is a NOTIFY of a refer subscription as the referrer received it.
type referNotify struct {
	subscriptionState string
	sipfrag           string
}

// newNotifyRecordingReferrer returns an answered outgoing call that stands for
// the dialog a REFER came on, and the NOTIFYs sent in it, each answered 200.
func newNotifyRecordingReferrer(t *testing.T) (*DialogClientSession, func() []referNotify) {
	t.Helper()
	var (
		mu      sync.Mutex
		notifys []referNotify
	)
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		switch {
		case req.IsInvite():
			res.SetBody(peerSDP())
		case req.Method == sip.NOTIFY:
			n := referNotify{sipfrag: string(req.Body())}
			if h := req.GetHeader("Subscription-State"); h != nil {
				n.subscriptionState = h.Value()
			}
			mu.Lock()
			notifys = append(notifys, n)
			mu.Unlock()
		}
		return res
	})
	ctx := context.Background()
	d, err := dg.NewDialog(sip.Uri{User: "transferor", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Invite(ctx, InviteClientOptions{})
	require.NoError(t, err)
	require.NoError(t, d.Ack(ctx))
	return d, func() []referNotify {
		mu.Lock()
		defer mu.Unlock()
		return append([]referNotify(nil), notifys...)
	}
}

// TestReferTransactionAcceptNotifiesOutcome checks the NOTIFYs a
// ReferTransaction's Accept sends in the dialog the REFER came on: one at
// once for the subscription the accepted REFER creates (RFC 3515 section
// 2.4.4; RFC 6665 section 4.2.1), then one terminating NOTIFY whose sipfrag is
// the final response of the triggered INVITE (RFC 3515 section 2.4.5), its
// 2xx or the failure, which Accept also returns with its cause.
func TestReferTransactionAcceptNotifiesOutcome(t *testing.T) {
	const (
		active     = "active;expires=60"
		terminated = "terminated;reason=noresource"
	)
	for _, tc := range []struct {
		name         string
		targetStatus int
		targetReason string
		want         []referNotify
	}{
		{
			name:         "answered",
			targetStatus: sip.StatusOK,
			targetReason: "OK",
			want: []referNotify{
				{subscriptionState: active, sipfrag: "SIP/2.0 100 Trying"},
				{subscriptionState: terminated, sipfrag: "SIP/2.0 200 OK"},
			},
		},
		{
			name:         "busy",
			targetStatus: sip.StatusBusyHere,
			targetReason: "Busy Here",
			want: []referNotify{
				{subscriptionState: active, sipfrag: "SIP/2.0 100 Trying"},
				{subscriptionState: terminated, sipfrag: "SIP/2.0 486 Busy Here"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
				res := sip.NewResponseFromRequest(req, tc.targetStatus, tc.targetReason, nil)
				if req.IsInvite() && tc.targetStatus == sip.StatusOK {
					res.SetBody(peerSDP())
				}
				return res
			})
			referrer, notifys := newNotifyRecordingReferrer(t)

			refer := sip.NewRequest(sip.REFER, sip.Uri{User: "transferee", Host: "127.0.0.1", Port: 5060})
			refer.AppendHeader(sip.NewHeader("Refer-To", "<sip:target@127.0.0.1:5090>"))
			refer.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "transferor", Host: "127.0.0.1", Port: 5070}})
			tx := newByeServerTx()

			ctx := context.Background()
			err := dialogHandleReferTransaction(ctx, referrer, dg, refer, tx, func(rtx ReferTransaction) error {
				referDialog, err := rtx.Accept(ctx, InviteClientOptions{})
				if err != nil {
					return err
				}
				return referDialog.Close()
			})

			if tc.targetStatus == sip.StatusOK {
				require.NoError(t, err)
			} else {
				var resErr *sipgo.ErrDialogResponse
				require.ErrorAs(t, err, &resErr, "Accept returns the INVITE's failure")
				require.Equal(t, tc.targetStatus, resErr.Res.StatusCode)
			}
			require.Len(t, tx.responses, 1)
			require.Equal(t, 202, tx.responses[0].StatusCode, "the REFER is accepted")
			require.Equal(t, tc.want, notifys())
		})
	}
}
