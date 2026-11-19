// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// inDialogHandlers are the handlers diago runs for INFO, REFER and NOTIFY on a
// dialog found in the cache, on either side.
type inDialogHandlers interface {
	referDialog
	readSIPInfoDTMF(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) error
	handleRefer(ctx context.Context, dg *Diago, req *sip.Request, tx sip.ServerTransaction)
	handleReferNotify(ctx context.Context, req *sip.Request, tx sip.ServerTransaction)
}

// TestDialogEndedAnswersInDialogRequests481 pins how INFO, REFER and NOTIFY are
// answered on a dialog that has ended, on either side. An ended dialog stays in
// the cache until the call handler returns, for an inbound call, or until it is
// closed, for an outbound one, so these requests still find it. INFO belongs to
// the INVITE usage the BYE ended, and a REFER asks to transfer a call that is
// gone, so both match no live dialog and are answered 481 (RFC 3261 section
// 12.2.2), without the REFER reaching the transfer. A BYE does not end the
// subscription usage a REFER created, and the dialog lives on until its last
// usage ends (RFC 5057 sections 2 and 4.1), so a NOTIFY for a REFER the dialog
// still tracks is taken as on a live dialog, and any other is answered 481.
func TestDialogEndedAnswersInDialogRequests481(t *testing.T) {
	sides := []struct {
		name string
		// newEnded returns an ended dialog with a transfer callback that
		// records its calls and the callback a REFER it sent takes its
		// NOTIFYs with, and what the dialog sent.
		newEnded func(t *testing.T, onRefer OnReferDialogFunc, onNotify func(statusCode int)) (inDialogHandlers, *sentRequests)
	}{
		{
			name: "inbound",
			newEnded: func(t *testing.T, onRefer OnReferDialogFunc, onNotify func(statusCode int)) (inDialogHandlers, *sentRequests) {
				ua, sent := newRecordingDialogUA(t)
				d := newTestDialogOverUA(t, newByeServerTx(), ua)
				d.InviteResponse = sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
				confirm(t, d)
				d.onReferDialog = onRefer
				d.onReferNotify = onNotify
				require.NoError(t, d.ReadBye(t.Context(), newBye(t, d, d.InviteRequest.CSeq().SeqNo+1), newByeServerTx()))
				require.Equal(t, sip.DialogStateEnded, d.LoadState())
				return d, sent
			},
		},
		{
			name: "outbound",
			newEnded: func(t *testing.T, onRefer OnReferDialogFunc, onNotify func(statusCode int)) (inDialogHandlers, *sentRequests) {
				d, sent := newAnsweredTestClientDialog(t)
				d.onReferDialog = onRefer
				d.onReferNotify = onNotify
				require.NoError(t, d.Hangup(context.Background()))
				require.Equal(t, sip.DialogStateEnded, d.LoadState())
				sent.reset()
				return d, sent
			},
		},
	}

	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			t.Run("INFO", func(t *testing.T) {
				d, _ := side.newEnded(t, nil, nil)
				req := newPeerInDialogRequest(sip.INFO, 500)
				req.AppendHeader(sip.NewHeader("Content-Type", "application/dtmf-relay"))
				req.SetBody([]byte("Signal=1\r\nDuration=160\r\n"))

				tx := newByeServerTx()
				require.NoError(t, d.readSIPInfoDTMF(t.Context(), req, tx))
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
			})

			t.Run("REFER", func(t *testing.T) {
				var transfers atomic.Int32
				d, sent := side.newEnded(t, func(*DialogClientSession) error {
					transfers.Add(1)
					return nil
				}, nil)
				ua, err := sipgo.NewUA()
				require.NoError(t, err)
				t.Cleanup(func() { _ = ua.Close() })
				dg := NewDiago(ua)

				req := newPeerInDialogRequest(sip.REFER, 500)
				req.AppendHeader(sip.NewHeader("Refer-To", "<sip:carol@127.0.0.1:5080>"))

				tx := newByeServerTx()
				d.handleRefer(t.Context(), dg, req, tx)
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
				assert.Empty(t, sent.sent(), "the REFER on an ended dialog sent a NOTIFY")
				assert.Zero(t, transfers.Load(), "the REFER on an ended dialog reached the transfer")
			})

			t.Run("NOTIFY for no REFER", func(t *testing.T) {
				d, _ := side.newEnded(t, nil, nil)
				req := newPeerInDialogRequest(sip.NOTIFY, 500)
				req.AppendHeader(sip.NewHeader("Event", "refer"))
				req.AppendHeader(sip.NewHeader("Subscription-State", "terminated;reason=noresource"))
				req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
				req.SetBody([]byte("SIP/2.0 200 OK"))

				tx := newByeServerTx()
				d.handleReferNotify(t.Context(), req, tx)
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
			})

			t.Run("NOTIFY for a tracked REFER", func(t *testing.T) {
				d, _ := side.newEnded(t, nil, nil)
				attempt := d.sipState().beginReferAttempt()
				d.sipState().setReferAttemptCSeq(attempt, 7)

				req := newPeerInDialogRequest(sip.NOTIFY, 500)
				req.AppendHeader(sip.NewHeader("Event", "refer;id=7"))
				req.AppendHeader(sip.NewHeader("Subscription-State", "terminated;reason=noresource"))
				req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
				req.SetBody([]byte("SIP/2.0 200 OK"))

				tx := newByeServerTx()
				d.handleReferNotify(t.Context(), req, tx)
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusOK, tx.responses[0].StatusCode)
				d.sipState().referMu.Lock()
				end := attempt.end
				d.sipState().referMu.Unlock()
				assert.Equal(t, ReferEndFinalNotify, end, "the transfer outcome was not delivered")
			})

			t.Run("NOTIFY for a REFER its callback tracks", func(t *testing.T) {
				// The REFER is tracked by the OnNotify callback its
				// ReferOptions set.
				var outcome atomic.Int32
				d, _ := side.newEnded(t, nil, func(statusCode int) { outcome.Store(int32(statusCode)) })

				req := newPeerInDialogRequest(sip.NOTIFY, 500)
				req.AppendHeader(sip.NewHeader("Event", "refer"))
				req.AppendHeader(sip.NewHeader("Subscription-State", "terminated;reason=noresource"))
				req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
				req.SetBody([]byte("SIP/2.0 200 OK"))

				tx := newByeServerTx()
				d.handleReferNotify(t.Context(), req, tx)
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusOK, tx.responses[0].StatusCode)
				assert.Equal(t, int32(sip.StatusOK), outcome.Load(), "the transfer outcome was not delivered")
			})
		})
	}
}

// TestDialogServerReInviteEndedDialog pins how a re-INVITE for a dialog that is
// no longer live is answered. An ended dialog stays in the cache until the call
// handler returns, so the request still finds it. It matches no live dialog,
// and is answered 481 (RFC 3261 section 12.2.2) without reaching the media.
func TestDialogServerReInviteEndedDialog(t *testing.T) {
	t.Run("ended", func(t *testing.T) {
		d, _ := newAnsweredTestServerCall(t)
		require.NoError(t, d.ReadBye(t.Context(), newBye(t, d, d.InviteRequest.CSeq().SeqNo+1), newByeServerTx()))
		require.Equal(t, sip.DialogStateEnded, d.LoadState())

		tx := newByeServerTx()
		require.NoError(t, d.handleReInvite(t.Context(), newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+2), tx))
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
		assert.Nil(t, d.remoteContactTarget, "the re-INVITE reached the media")
	})
}

// TestDialogClientReInviteEndedDialog pins how a peer's re-INVITE for an
// outgoing dialog that is no longer live is answered. After our BYE the dialog
// stays in the cache until it is closed, so the request still finds it. It
// matches no live dialog, and is answered 481 (RFC 3261 section 12.2.2)
// without reaching the media.
func TestDialogClientReInviteEndedDialog(t *testing.T) {
	t.Run("ended", func(t *testing.T) {
		d, _ := newAnsweredTestClientDialog(t)
		require.NoError(t, d.Hangup(t.Context()))
		require.Equal(t, sip.DialogStateEnded, d.LoadState())

		tx := newByeServerTx()
		require.NoError(t, d.handleReInvite(t.Context(), newPeerInDialogRequest(sip.INVITE, 5), tx))
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
		assert.Nil(t, d.remoteContactTarget, "the re-INVITE reached the media")
	})
}
