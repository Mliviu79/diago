// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"sync"
	"testing"

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
				// dialogHandleReferTransaction leaves the referring dialog
				// unset, and Accept sends the subscription's NOTIFY in it.
				rtx.d = referrer
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
