// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newPeerBye builds a BYE the peer sends inside an outgoing dialog, carrying
// an RFC 3326 Reason header: the cause the stash exists to preserve.
func newPeerBye(seq uint32) *sip.Request {
	bye := newPeerInDialogRequest(sip.BYE, seq)
	bye.AppendHeader(sip.NewHeader("Reason", "Q.850;cause=16;text=\"Normal call clearing\""))
	return bye
}

// TestDialogClientTerminatingBye pins the terminating-BYE stash on the calling
// side, as TestDialogServerTerminatingBye does on the answering side: the
// peer's BYE that ends an outgoing dialog stays reachable after ReadBye has
// answered it, and an ending that is not the peer's BYE leaves it nil, so the
// dialog's owner can tell the peer's BYE from our own.
func TestDialogClientTerminatingBye(t *testing.T) {
	t.Run("nil before any BYE", func(t *testing.T) {
		d, _ := newAnsweredTestClientDialog(t)
		assert.Nil(t, d.TerminatingBye(), "a live dialog has no terminating BYE")
	})

	t.Run("the peer's BYE is kept", func(t *testing.T) {
		d, _ := newAnsweredTestClientDialog(t)
		bye := newPeerBye(2)

		tx := newByeServerTx()
		require.NoError(t, d.ReadBye(bye, tx))

		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusOK, tx.responses[0].StatusCode)
		assert.Equal(t, sip.DialogStateEnded, d.LoadState())

		require.Same(t, bye, d.TerminatingBye(), "the terminating BYE must be the peer's request")
		assert.Equal(t, "Q.850;cause=16;text=\"Normal call clearing\"",
			d.TerminatingBye().GetHeader("Reason").Value(),
			"the peer's stated cause must survive ReadBye")
	})

	t.Run("stashed before the dialog is seen to end", func(t *testing.T) {
		// The state callback fires inside the transition, after the context
		// is cancelled: the earliest moment the ending is observable.
		d, _ := newAnsweredTestClientDialog(t)
		bye := newPeerBye(2)

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

	t.Run("an out-of-order BYE plants no cause", func(t *testing.T) {
		// A BYE below the CSeq of a request the peer sent before it is out of
		// order (RFC 3261 section 12.2.2): answered 500, not this dialog's
		// ending, and the dialog goes on.
		d, _ := newAnsweredTestClientDialog(t)
		require.NoError(t, d.ReadRequest(newPeerInDialogRequest(sip.INVITE, 5), newByeServerTx()))

		tx := newByeServerTx()
		assert.ErrorIs(t, d.ReadBye(newPeerBye(4), tx), sipgo.ErrDialogInvalidCseq)
		require.Len(t, tx.responses, 1, "an out-of-order BYE must be answered")
		assert.Equal(t, sip.StatusInternalServerError, tx.responses[0].StatusCode)
		assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
		assert.Nil(t, d.TerminatingBye(), "a refused BYE must not be stashed")
	})

	t.Run("kept when the 200 cannot be sent", func(t *testing.T) {
		// The BYE is taken, but the 200 does not go out. Whether that ends
		// the dialog is sipgo's to decide; when it does, the BYE is what ended
		// it, and it stays reachable as for any other ending.
		d, _ := newAnsweredTestClientDialog(t)
		bye := newPeerBye(2)

		tx := newByeServerTx()
		tx.respondErr = sip.ErrTransactionTerminated
		require.ErrorIs(t, d.ReadBye(bye, tx), sip.ErrTransactionTerminated)
		if d.LoadState() == sip.DialogStateEnded {
			assert.Same(t, bye, d.TerminatingBye(), "the BYE that ended the dialog must stay stashed")
		} else {
			assert.Nil(t, d.TerminatingBye(), "a BYE that did not end the dialog must not be stashed")
		}
	})

	t.Run("our own hang-up stays nil", func(t *testing.T) {
		// Our BYE ends the dialog; the peer stated no cause.
		d, _ := newAnsweredTestClientDialog(t)

		require.NoError(t, d.Hangup(context.Background()))

		require.Equal(t, sip.DialogStateEnded, d.LoadState())
		assert.Nil(t, d.TerminatingBye(), "an ending by our own BYE stays nil")
	})
}
