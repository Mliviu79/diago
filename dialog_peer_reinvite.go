// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// pendingReInvite is a re-INVITE of the peer's being handled. Every field is
// guarded by sessionState.answerMu, but for ackErr, which retransmitted
// publishes.
type pendingReInvite struct {
	req *sip.Request
	tx  sip.ServerTransaction
	// cseq is the CSeq of the re-INVITE, which the ACK to its 2xx carries.
	cseq uint32
	// dialog is the dialog the re-INVITE belongs to. Once it has ended the
	// re-INVITE is answered 487.
	dialog *sipgo.Dialog
	// remoteTarget is where the dialog keeps its remote target, which the 2xx
	// to the re-INVITE moves.
	remoteTarget *dialogCallbacks
	// answerInAck is set for a re-INVITE that carries no offer: our 2xx
	// carries the offer, and the ACK to it the answer (RFC 3261 section 14.2).
	answerInAck bool
	// answered is set once the re-INVITE has its final response.
	answered bool
	// acked is made when a 2xx is sent, and closed once its ACK is read.
	acked chan struct{}
	// ackHandled is made when the ACK is read, and closed once the handler
	// of that ACK has had the media complete the update.
	ackHandled chan struct{}
	// retransmitted is made when a 2xx is sent, and closed once its
	// retransmission has ended; ackErr then says why, nil for an ACK or the
	// end of the dialog.
	retransmitted chan struct{}
	ackErr        error
}

// errReInviteAckTimeout is why a re-INVITE of the peer's ended when the ACK to
// our 2xx did not come within 64*T1. RFC 3261 section 13.3.1.4 has the dialog
// confirmed, and the session ended with a BYE.
var errReInviteAckTimeout = errors.New("no ACK received for the 2xx to a re-INVITE")

// ackTimers are the timers of the retransmission of a 2xx to a re-INVITE of
// the peer's: the first interval, the interval it doubles up to, and how long
// it waits for the ACK. Zero values take T1, T2 and 64*T1 (RFC 3261 section
// 13.3.1.4).
type ackTimers struct {
	t1, t2, timeout time.Duration
}

func (t ackTimers) values() (t1, t2, timeout time.Duration) {
	t1, t2, timeout = t.t1, t.t2, t.timeout
	if t1 <= 0 {
		t1 = sip.T1
	}
	if t2 <= 0 {
		t2 = sip.T2
	}
	if timeout <= 0 {
		timeout = 64 * sip.T1
	}
	return t1, t2, timeout
}

// beginPeerReInvite answers req 481 when dialog has ended, and otherwise
// records it as the peer's re-INVITE being handled, until endPeerReInvite. It
// reports whether it answered. An ended dialog stays in the dialog cache until
// its call handler returns, for an inbound call, or until it is closed, for an
// outbound one, so a re-INVITE can still find it. It matches no live dialog,
// and RFC 3261 section 12.2.2 has it answered 481. remoteTarget is where
// dialog's remote target is kept.
func (s *sessionState) beginPeerReInvite(ctx context.Context, dialog *sipgo.Dialog, remoteTarget *dialogCallbacks, req *sip.Request, tx sip.ServerTransaction) (bool, error) {
	s.answerMu.Lock()
	defer s.answerMu.Unlock()
	if dialog.LoadState() == sip.DialogStateEnded {
		res := sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)
		return true, tx.Respond(ctx, res)
	}
	p := &pendingReInvite{req: req, tx: tx, dialog: dialog, remoteTarget: remoteTarget, answerInAck: reInviteOffer(req) == nil}
	if cseq := req.CSeq(); cseq != nil {
		p.cseq = cseq.SeqNo
	}
	s.peerReInvite = p
	return false, nil
}

// endPeerReInvite waits until the retransmission of a 2xx sent to the
// re-INVITE beginPeerReInvite recorded for tx has ended and, when its ACK was
// read, until the handler of that ACK has had the media complete the update,
// and then forgets the re-INVITE. It reports whether the ACK was read: a
// re-INVITE that ends without it has completed no media update. It returns
// errReInviteAckTimeout when the ACK did not come, for the caller to end the
// call.
func (s *sessionState) endPeerReInvite(tx sip.ServerTransaction) (acked bool, err error) {
	s.answerMu.Lock()
	p := s.peerReInvite
	if p == nil || p.tx != tx {
		s.answerMu.Unlock()
		return false, nil
	}
	retransmitted := p.retransmitted
	s.answerMu.Unlock()

	if retransmitted != nil {
		<-retransmitted
		err = p.ackErr
	}

	s.answerMu.Lock()
	ackHandled := p.ackHandled
	if ackHandled == nil {
		// Forgotten under the lock that found no ACK read, so an ACK read
		// from here on is no longer taken for this re-INVITE.
		s.peerReInvite = nil
	}
	s.answerMu.Unlock()
	if ackHandled == nil {
		return false, err
	}

	<-ackHandled
	s.answerMu.Lock()
	if s.peerReInvite == p {
		s.peerReInvite = nil
	}
	s.answerMu.Unlock()
	return true, err
}

// ackPeerReInvite tells the re-INVITE of the peer's being handled that ack,
// the ACK to our 2xx, has been read, which ends the retransmission of the 2xx.
// The caller then has the media complete the update and calls handled, which
// the handling of the re-INVITE waits for, so the next re-INVITE in either
// direction finds the update complete.
//
// first is false for another copy of an ACK already read, which the peer
// sends for each retransmission of the 2xx that reached it: it brings nothing
// the first did not, and the caller hands the media nothing for it. An ACK
// that belongs to no re-INVITE being handled is reported as a first one, with
// a handled that does nothing.
//
// answerDue is set for the first ACK to a 2xx that carried our offer, the
// re-INVITE having carried none: this ACK, which carries the CSeq of that
// re-INVITE, is the one that carries the answer.
func (s *sessionState) ackPeerReInvite(ack *sip.Request) (handled func(), first, answerDue bool) {
	cseq := ack.CSeq()
	if cseq == nil {
		return func() {}, true, false
	}
	s.answerMu.Lock()
	defer s.answerMu.Unlock()
	p := s.peerReInvite
	if p == nil || p.acked == nil || p.cseq != cseq.SeqNo {
		return func() {}, true, false
	}
	if p.ackHandled != nil {
		return func() {}, false, false
	}
	close(p.acked)
	ackHandled := make(chan struct{})
	p.ackHandled = ackHandled
	return func() { close(ackHandled) }, true, p.answerInAck
}

// retransmitReInvite2xx passes res, the 2xx sent to the re-INVITE p, to the
// transaction again until its ACK is read: the server transaction does not
// retransmit a 2xx, the UAS core does (RFC 3261 section 13.3.1.4, RFC 6026
// section 7.1). The interval starts at T1 and doubles up to T2. It gives up
// with errReInviteAckTimeout after 64*T1, and stops once the dialog has ended:
// a retransmission is sent under answerMu while the dialog is live, so none
// follows the 200 to a BYE.
func (s *sessionState) retransmitReInvite2xx(p *pendingReInvite, res *sip.Response) {
	defer close(p.retransmitted)
	t1, t2, timeout := s.ackTimers.values()
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	interval := t1
	timer := time.NewTimer(interval)
	defer timer.Stop()

	for {
		select {
		case <-p.acked:
			return
		case <-p.dialog.Context().Done():
			return
		case <-deadline.C:
			p.ackErr = errReInviteAckTimeout
			return
		case <-timer.C:
			s.answerMu.Lock()
			ended := p.dialog.LoadState() == sip.DialogStateEnded
			var err error
			if !ended {
				err = p.tx.Respond(p.dialog.Context(), res)
			}
			s.answerMu.Unlock()
			if ended {
				return
			}
			if err != nil {
				// Timer L ends the transaction 64*T1 after the 2xx, when
				// the wait is over too.
				p.ackErr = errors.Join(errReInviteAckTimeout, err)
				return
			}
			interval = min(2*interval, t2)
			timer.Reset(interval)
		}
	}
}

// respondPeerReInvite sends res as the final response to the peer's re-INVITE
// on tx, and reports whether res was sent. A re-INVITE the end of the dialog
// has answered gets nothing more, and one found pending once the dialog has
// ended is answered 487 instead (RFC 3261 section 15.1.2). A request
// beginPeerReInvite did not record is answered res as it is.
//
// A 2xx is retransmitted from a goroutine of the re-INVITE's handling until
// its ACK is read, which endPeerReInvite waits for.
func (s *sessionState) respondPeerReInvite(ctx context.Context, tx sip.ServerTransaction, res *sip.Response) (bool, error) {
	s.answerMu.Lock()
	defer s.answerMu.Unlock()
	p := s.peerReInvite
	if p == nil || p.tx != tx {
		return true, tx.Respond(ctx, res)
	}
	if p.answered {
		return false, nil
	}
	p.answered = true
	if p.dialog.LoadState() == sip.DialogStateEnded {
		return false, tx.Respond(ctx, sip.NewResponseFromRequest(p.req, sip.StatusRequestTerminated, "Request Terminated", nil))
	}
	if !res.IsSuccess() {
		return true, tx.Respond(ctx, res)
	}
	// The re-INVITE is a target refresh request, which moves the remote
	// target as its 2xx is sent, and never when it is refused (RFC 6141
	// section 4.6).
	p.remoteTarget.setRemoteContact(p.req.Contact())
	// Made before the 2xx goes out, which its ACK can follow at once.
	p.acked = make(chan struct{})
	if err := tx.Respond(ctx, res); err != nil {
		return true, err
	}
	p.retransmitted = make(chan struct{})
	go s.retransmitReInvite2xx(p, res)
	return true, nil
}

// terminatePeerReInviteLocked answers the peer's re-INVITE 487 when it is
// still pending once a BYE has ended the dialog: RFC 3261 section 15.1.2 has
// every pending request answered, and recommends 487. Called with answerMu
// held.
func (s *sessionState) terminatePeerReInviteLocked(ctx context.Context) error {
	p := s.peerReInvite
	if p == nil || p.answered {
		return nil
	}
	p.answered = true
	return p.tx.Respond(ctx, sip.NewResponseFromRequest(p.req, sip.StatusRequestTerminated, "Request Terminated", nil))
}

// refusePeerReInvite answers req, a re-INVITE of the peer's, when the media
// refused its offer or could not describe the answer, mediaErr being why.
//
// Media that is closed was closed by a BYE handled while the request was
// pending, or by the application. The request is still answered, with the 487
// RFC 3261 section 15.1.2 recommends, and that is no failure to report: only
// what sending the 487 returned is. Any other refusal is answered 400 and
// returned together with what sending it returned.
func (s *sessionState) refusePeerReInvite(ctx context.Context, req *sip.Request, tx sip.ServerTransaction, mediaErr error) error {
	if errors.Is(mediaErr, errMediaClosed) {
		_, err := s.respondPeerReInvite(ctx, tx, sip.NewResponseFromRequest(req, sip.StatusRequestTerminated, "Request Terminated", nil))
		return err
	}
	_, err := s.respondPeerReInvite(ctx, tx, sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
	return errors.Join(mediaErr, err)
}

// hangupNoMedia ends a call that a media update of the peer's left without
// usable media once it was answered, or whose 2xx to a re-INVITE was never
// acknowledged. The offer/answer exchange was complete by then, so the peer has
// moved to the new session. When that was a new DTLS association whose
// handshake failed, RFC 5763 section 5 has the media session torn down at
// once, and with a single audio stream that is the call.
func (d *DialogClientSession) hangupNoMedia() error {
	ctx, cancel := context.WithTimeout(d.Context(), 10*time.Second)
	defer cancel()
	return d.Hangup(ctx)
}
