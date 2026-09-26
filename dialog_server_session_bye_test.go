// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// byeServerTx is the minimal sip.ServerTransaction the BYE path touches: it
// records what was responded and satisfies the callbacks ReadInvite registers.
type byeServerTx struct {
	responses   []*sip.Response
	done        chan struct{}
	onTerminate sip.FnTxTerminate
	// respondErr, when set, is what Respond returns, as a transaction that
	// cannot send the response.
	respondErr error
}

func newByeServerTx() *byeServerTx {
	return &byeServerTx{done: make(chan struct{})}
}

func (tx *byeServerTx) Respond(res *sip.Response) error {
	tx.responses = append(tx.responses, res)
	return tx.respondErr
}
func (tx *byeServerTx) Acks() <-chan *sip.Request      { return nil }
func (tx *byeServerTx) OnCancel(f sip.FnTxCancel) bool { return true }
func (tx *byeServerTx) OnTerminate(f sip.FnTxTerminate) bool {
	tx.onTerminate = f
	return true
}
func (tx *byeServerTx) Terminate()            {}
func (tx *byeServerTx) Done() <-chan struct{} { return tx.done }
func (tx *byeServerTx) Err() error            { return nil }

// newByeTestDialog builds an answered inbound dialog over a fake transaction, so
// the BYE path can be driven without a transport.
func newByeTestDialog(t *testing.T) (*DialogServerSession, *byeServerTx) {
	t.Helper()
	inviteTx := newByeServerTx()
	return newTestDialogOver(t, inviteTx), inviteTx
}

// newTestDialogOver builds the dialog newByeTestDialog does over inviteTx.
func newTestDialogOver(t *testing.T, inviteTx sip.ServerTransaction) *DialogServerSession {
	t.Helper()
	return newTestDialogOverUA(t, inviteTx, &sipgo.DialogUA{})
}

// newTestDialogOverUA builds the dialog newTestDialogOver does, sending its own
// requests through ua.
func newTestDialogOverUA(t *testing.T, inviteTx sip.ServerTransaction, ua *sipgo.DialogUA) *DialogServerSession {
	t.Helper()

	recipient := sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}
	caller := sip.Uri{User: "bob", Host: "127.0.0.2", Port: 5060}

	invite := sip.NewRequest(sip.INVITE, recipient)
	invite.AppendHeader(&sip.ContactHeader{Address: caller})
	fromParams := sip.NewParams()
	fromParams.Add("tag", "caller-tag")
	invite.AppendHeader(&sip.FromHeader{Address: caller, Params: fromParams})
	invite.AppendHeader(&sip.ToHeader{Address: recipient, Params: sip.NewParams()})
	invite.AppendHeader(sip.NewHeader("Call-ID", "bye-stash-test-call-id"))
	invite.AppendHeader(&sip.CSeqHeader{SeqNo: 100, MethodName: sip.INVITE})

	sess, err := ua.ReadInvite(invite, inviteTx)
	require.NoError(t, err)

	return &DialogServerSession{DialogServerSession: sess}
}

// confirm drives the dialog to a confirmed state, where a BYE is meaningful.
func confirm(t *testing.T, d *DialogServerSession) {
	t.Helper()
	ack := sip.NewRequest(sip.ACK, d.InviteRequest.Contact().Address)
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: d.InviteRequest.CSeq().SeqNo, MethodName: sip.ACK})
	require.NoError(t, d.ReadAck(ack, newByeServerTx()))
	require.Equal(t, sip.DialogStateConfirmed, d.LoadState())
}

// newBye builds a BYE inside the dialog, carrying an RFC 3326 Reason header —
// the cause the stash exists to preserve.
func newBye(t *testing.T, d *DialogServerSession, seq uint32) *sip.Request {
	t.Helper()

	inv := d.InviteRequest
	bye := sip.NewRequest(sip.BYE, inv.Contact().Address)
	bye.AppendHeader(sip.HeaderClone(inv.From()))
	bye.AppendHeader(sip.HeaderClone(inv.To()))
	bye.AppendHeader(sip.HeaderClone(inv.CallID()))
	bye.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: sip.BYE})
	bye.AppendHeader(sip.NewHeader("Reason", "Q.850;cause=31;text=\"Normal, unspecified\""))
	return bye
}

// TestDialogServerTerminatingBye pins the terminating-BYE stash: the request
// that ends a dialog stays reachable after ReadBye has answered it and dropped
// it, so an application can read the cause the peer stated for itself.
func TestDialogServerTerminatingBye(t *testing.T) {
	t.Run("nil before any BYE", func(t *testing.T) {
		d, _ := newByeTestDialog(t)
		confirm(t, d)
		assert.Nil(t, d.TerminatingBye(), "a live dialog has no terminating BYE")
	})

	t.Run("captured on the terminate path", func(t *testing.T) {
		d, _ := newByeTestDialog(t)
		confirm(t, d)
		bye := newBye(t, d, d.InviteRequest.CSeq().SeqNo+1)

		tx := newByeServerTx()
		require.NoError(t, d.ReadBye(bye, tx))

		// BYE semantics are untouched: still answered 200, still ends the dialog.
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusOK, tx.responses[0].StatusCode)
		assert.Equal(t, sip.DialogStateEnded, d.LoadState())

		require.Same(t, bye, d.TerminatingBye(), "the terminating BYE must be the stashed request")
		assert.Equal(t, "Q.850;cause=31;text=\"Normal, unspecified\"",
			d.TerminatingBye().GetHeader("Reason").Value(),
			"the peer's stated cause must survive ReadBye")
	})

	t.Run("stashed before the dialog is seen to end", func(t *testing.T) {
		// The ordering guarantee, pinned at the earliest moment the ending is
		// observable: the state callback fires inside the transition, after the
		// context is cancelled. Anything that stores the BYE later than this —
		// including storing it once ReadBye has returned — reads back nil here.
		d, _ := newByeTestDialog(t)
		confirm(t, d)
		bye := newBye(t, d, d.InviteRequest.CSeq().SeqNo+1)

		var (
			ended    bool
			atEnding *sip.Request
			ctxDone  bool
		)
		d.OnState(func(s sip.DialogState) {
			if s != sip.DialogStateEnded {
				return
			}
			ended = true
			atEnding = d.TerminatingBye()
			ctxDone = d.Context().Err() != nil
		})

		require.NoError(t, d.ReadBye(bye, newByeServerTx()))

		require.True(t, ended, "the dialog must be seen to end")
		assert.True(t, ctxDone, "the ending is already observable via the context")
		assert.Same(t, bye, atEnding, "the BYE must be stashed before the ending is observable")
	})

	t.Run("a rejected BYE plants no cause", func(t *testing.T) {
		// A stale-CSeq BYE is not this dialog's ending, so it must leave no cause
		// behind for the real one.
		d, _ := newByeTestDialog(t)
		confirm(t, d)
		stale := newBye(t, d, d.InviteRequest.CSeq().SeqNo-1)

		require.Error(t, d.ReadBye(stale, newByeServerTx()))
		assert.Nil(t, d.TerminatingBye(), "a rejected BYE must not be stashed")
		assert.NotEqual(t, sip.DialogStateEnded, d.LoadState(), "a rejected BYE must not end the dialog")
	})

	t.Run("kept when the 200 cannot be sent", func(t *testing.T) {
		// The BYE is answered, but the 200 does not go out. Whether that ends
		// the dialog is sipgo's to decide; when it does, the BYE is what ended
		// it, and it stays reachable as for any other ending.
		d, _ := newByeTestDialog(t)
		confirm(t, d)
		bye := newBye(t, d, d.InviteRequest.CSeq().SeqNo+1)

		tx := newByeServerTx()
		tx.respondErr = sip.ErrTransactionTerminated
		require.ErrorIs(t, d.ReadBye(bye, tx), sip.ErrTransactionTerminated)
		if d.LoadState() == sip.DialogStateEnded {
			assert.Same(t, bye, d.TerminatingBye(), "the BYE that ended the dialog must stay stashed")
		} else {
			assert.Nil(t, d.TerminatingBye(), "a BYE that did not end the dialog must not be stashed")
		}
	})

	t.Run("an out-of-order BYE is answered 500", func(t *testing.T) {
		// A BYE below the INVITE's CSeq is out of order. RFC 3261 section
		// 12.2.2 has it rejected with 500, so the peer gets an answer while
		// the dialog goes on.
		d, _ := newByeTestDialog(t)
		confirm(t, d)
		stale := newBye(t, d, d.InviteRequest.CSeq().SeqNo-1)

		tx := newByeServerTx()
		assert.ErrorIs(t, d.ReadBye(stale, tx), sipgo.ErrDialogInvalidCseq)
		require.Len(t, tx.responses, 1, "an out-of-order BYE must be answered")
		assert.Equal(t, sip.StatusInternalServerError, tx.responses[0].StatusCode)
		assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
	})

	t.Run("an ending with no BYE stays nil", func(t *testing.T) {
		// The dialog dies with its invite transaction, so the peer stated no cause
		// and nil is the truthful answer.
		d, inviteTx := newByeTestDialog(t)
		require.NotNil(t, inviteTx.onTerminate)

		inviteTx.onTerminate("key", nil)

		require.Equal(t, sip.DialogStateEnded, d.LoadState())
		assert.Nil(t, d.TerminatingBye(), "an ending with no remote BYE stays nil")
	})
}
