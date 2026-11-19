// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"sync/atomic"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// TerminatingBye returns the BYE request that ended this dialog, or nil when the
// dialog was not ended by a remote BYE or has not ended yet. The request is
// read-only; callers must not mutate it.
//
// Safe to call from any goroutine. The store happens before the dialog moves to
// DialogStateEnded, so an observer that reacts to Context() being done sees it.
func (d *DialogServerSession) TerminatingBye() *sip.Request {
	return d.terminatingBye.Load()
}

// TerminatingBye returns the BYE request that ended this dialog, or nil when the
// dialog was not ended by a remote BYE or has not ended yet. The request is
// read-only; callers must not mutate it.
//
// Safe to call from any goroutine. The store happens before the dialog moves to
// DialogStateEnded, so an observer that reacts to Context() being done sees it.
func (d *DialogClientSession) TerminatingBye() *sip.Request {
	return d.terminatingBye.Load()
}

// ReadBye stashes the terminating BYE and then hands it to the embedded session,
// which owns BYE handling: validation, the 200, and the move to
// DialogStateEnded. The request is recorded here, not acted upon differently,
// but for the answer the embedded session leaves to its caller when it refuses
// a BYE, described below.
//
// A BYE that arrives before our answer is complete is read once it is. The
// wait is for the media: an answer that starts its media after the ACK would
// otherwise have that media closed under it by the BYE's handler. It also has
// the answer see the ACK the peer sent ahead of the BYE, rather than the dialog
// ending before any ACK, which the answer would report as a missing ACK. A BYE
// that arrives while a re-INVITE is being handled does not wait for it: the BYE
// is answered, and the re-INVITE, when it has no final response yet, is
// answered 487 (RFC 3261 section 15.1.2), so the peer never gets a 200 to the
// re-INVITE after the one to its BYE.
//
// The stash must precede the delegation, because the delegate is what ends the
// dialog: storing afterwards would let an observer woken by Context() read nil.
// A rejected BYE is therefore rolled back rather than pre-filtered, which keeps
// validation in one place instead of duplicating the embedded session's rules
// here and letting the copies drift. Such a BYE is briefly visible, but it is
// one no observer can be looking at: only a BYE the delegate accepts ends the
// dialog, and until the dialog ends nothing has cause to read the stash.
//
// The delegate refuses a BYE it finds out of order with
// sipgo.ErrDialogInvalidCseq, and RFC 3261 section 12.2.2 has such a BYE
// rejected with 500. Depending on the sipgo version, the delegate answers it
// 500 itself or leaves the answer to its caller; readByeOnce answers it only
// when the delegate did not, and the error is returned: the dialog goes on.
func (d *DialogServerSession) ReadBye(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) error {
	// The BYE is read whether or not the answer completed in time.
	d.awaitAnswer(ctx, tx)
	return d.readPeerBye(ctx, req, tx, &d.Dialog, d.DialogServerSession.ReadBye)
}

// ReadBye reads the peer's BYE as the embedded session does. It does not wait
// for a re-INVITE being handled: a re-INVITE with no final response yet is
// answered 487 (RFC 3261 section 15.1.2), so none is answered 200 after the
// BYE that ended the dialog. A BYE the embedded session refuses as out of
// order, with sipgo.ErrDialogInvalidCseq, is answered 500 once, as
// DialogServerSession.ReadBye describes, and the dialog goes on.
//
// The BYE is stashed for TerminatingBye before the embedded session ends the
// dialog, and rolled back when it was refused or did not end the dialog, as
// DialogServerSession.ReadBye does.
func (d *DialogClientSession) ReadBye(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) error {
	return d.readPeerBye(ctx, req, tx, &d.Dialog, d.DialogClientSession.ReadBye)
}

// readPeerBye stashes req, the peer's BYE, hands it to readBye, the ReadBye of
// the embedded sipgo session of dialog, and drops the stash again when the BYE
// was not dialog's ending.
//
// It holds answerMu while the BYE ends the dialog and is answered, so the
// final response to a re-INVITE of the peer's goes out either before that or
// not at all: a re-INVITE still pending once the BYE has ended the dialog is
// answered 487 here (RFC 3261 section 15.1.2).
func (s *sessionState) readPeerBye(ctx context.Context, req *sip.Request, tx sip.ServerTransaction, dialog *sipgo.Dialog, readBye func(context.Context, *sip.Request, sip.ServerTransaction) error) error {
	s.answerMu.Lock()
	defer s.answerMu.Unlock()
	s.terminatingBye.Store(req)
	if err := readByeOnce(ctx, req, tx, readBye); err != nil {
		// Not this dialog's ending: leave no cause planted on it. A BYE
		// taken whose 200 could not be sent may still have ended it.
		if errors.Is(err, sipgo.ErrDialogInvalidCseq) || dialog.LoadState() != sip.DialogStateEnded {
			s.terminatingBye.CompareAndSwap(req, nil)
		}
		return err
	}
	return s.terminatePeerReInviteLocked(ctx)
}

// readByeOnce hands the peer's BYE to readBye, the ReadBye of an embedded
// sipgo session, and answers it 500 when readBye refused it as out of order
// (RFC 3261 section 12.2.2) without answering it. Some sipgo versions answer
// such a BYE themselves and some leave the answer to their caller; the BYE is
// answered once either way.
func readByeOnce(ctx context.Context, req *sip.Request, tx sip.ServerTransaction, readBye func(context.Context, *sip.Request, sip.ServerTransaction) error) error {
	rtx := &respondRecordingTx{ServerTransaction: tx}
	err := readBye(ctx, req, rtx)
	if errors.Is(err, sipgo.ErrDialogInvalidCseq) && !rtx.responded.Load() {
		res := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
		return errors.Join(err, tx.Respond(ctx, res))
	}
	return err
}

// respondRecordingTx is a server transaction that records whether a response
// was passed to it.
type respondRecordingTx struct {
	sip.ServerTransaction
	responded atomic.Bool
}

func (tx *respondRecordingTx) Respond(ctx context.Context, res *sip.Response) error {
	tx.responded.Store(true)
	return tx.ServerTransaction.Respond(ctx, res)
}

// endMediaOnBye ends the media of a call whose peer sent a BYE, given what
// ReadBye returned for it. A BYE refused as out of order is rejected and ends
// nothing (RFC 3261 section 12.2.2): the call goes on, and so does its media.
// Any other BYE ended the call, and its media is stopped (RFC 3261 section
// 15.1.2), which also unblocks whoever is still reading or writing it.
//
// The media is reached only through the close callbacks it registered with
// the session. The session itself is left to its owner: the BYE ends the
// INVITE's use of the dialog, not what else was set up on it, such as the
// subscription of a REFER, whose NOTIFYs still have to find the dialog (RFC
// 3261 section 15, RFC 5057 section 5.5).
func (d *dialogCallbacks) endMediaOnBye(byeErr error) error {
	if errors.Is(byeErr, sipgo.ErrDialogInvalidCseq) {
		return nil
	}
	return d.closeCallbacks()
}

// NewByeRequest returns a BYE to the dialog's remote target, RemoteContact,
// over the INVITE's transport (RFC 3261 section 12.2.1.1). WriteBye fills in
// the dialog's headers.
func (d *DialogServerSession) NewByeRequest() *sip.Request {
	bye := sip.NewRequest(sip.BYE, *d.RemoteContact().Address.Clone())
	bye.SetTransport(d.InviteRequest.Transport())
	return bye
}

// Bye sends NewByeRequest and waits within ctx for its final answer, see
// WriteBye.
func (d *DialogServerSession) Bye(ctx context.Context) error {
	return d.WriteBye(ctx, d.NewByeRequest())
}

// NewByeRequest returns a BYE to the dialog's remote target, RemoteContact,
// or to the INVITE's Request-URI when the answer carried no Contact, over the
// INVITE's transport (RFC 3261 section 12.2.1.1). It needs the INVITE's
// answer. WriteBye fills in the dialog's headers.
func (d *DialogClientSession) NewByeRequest() *sip.Request {
	bye := sip.NewRequest(sip.BYE, d.remoteTarget())
	bye.SetTransport(d.InviteRequest.Transport())
	return bye
}

// Bye sends NewByeRequest once the INVITE has an answer, and waits within ctx
// for its final answer, see WriteBye.
func (d *DialogClientSession) Bye(ctx context.Context) error {
	if d.InviteResponse == nil {
		return errors.New("bye: can not send as no invite response present")
	}
	return d.WriteBye(ctx, d.NewByeRequest())
}
