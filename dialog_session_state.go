// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"log/slog"
	"sync"
	"sync/atomic"

	"github.com/emiago/sipgo/sip"
)

// sessionState is the SIP-side state a dialog session keeps of its own. Both
// session types embed it beside dialogCallbacks. dialogCallbacks is what the
// media of a call registers with and is handed, so state that only the session
// reads and writes is kept here, apart from it and from every lock of the
// media.
type sessionState struct {
	// terminatingBye holds the BYE request that ended this dialog, captured by
	// ReadBye for read-only inspection. Without it the BYE is unrecoverable:
	// ReadBye answers 200 and drops the request, so an application observes the
	// ending only as a Context() cancel and cannot tell the peer's BYE from our
	// own, nor see a cause the peer stated for itself, such as an RFC 3326
	// Reason header.
	//
	// nil until a remote BYE arrives. A local Hangup, a transport death or a
	// cancel all leave it nil, which is the truthful "the peer stated no cause".
	terminatingBye atomic.Pointer[sip.Request]

	// uncache removes the dialog from its Diago's dialog cache, and the Close
	// of an outbound dialog runs it. An ended dialog stays findable until its
	// owner closes it, as an inbound one does until its serve handler returns,
	// so a NOTIFY for a REFER subscription that outlived the BYE still reaches
	// it (RFC 5057 section 5.5). Nil for a dialog no Diago made, and for an
	// inbound one, which the handler of its INVITE removes. Set before the
	// dialog is handed out and not changed after.
	uncache func() error

	// referMu guards referAttempts, referHighestCSeq, referLatest,
	// referEndedByNotify, the attempts they hold and onReferNotifyUnmatched. It is a lock of the
	// session's own, so observing a REFER NOTIFY contends with nothing else of
	// the call.
	referMu sync.Mutex
	// referAttempts are the open REFER attempts, oldest first: each lives as
	// long as its refer subscription, up to its deadline (see referAttempt),
	// past the dialog's BYE and its media's close (RFC 5057 section 5.5).
	referAttempts []*referAttempt
	// referHighestCSeq is the highest CSeq recorded for a REFER sent on this
	// dialog, retired attempts included. In-dialog CSeqs only grow, so an Event
	// id at or below it belongs to an earlier REFER.
	referHighestCSeq uint32
	// referLatest is the attempt begun most recently. It is kept after it is
	// retired, so a NOTIFY without an Event id is never given to an older
	// attempt once a newer one has started.
	referLatest *referAttempt
	// referEndedByNotify are the retired attempts whose subscription a NOTIFY
	// ended, so a NOTIFY that comes for one of them after that is answered
	// 481 also on a dialog whose OnNotify callback takes every other refer
	// NOTIFY (RFC 6665 section 4.1.3).
	referEndedByNotify []*referAttempt
	// onReferNotifyUnmatched is told of a refer NOTIFY no open attempt holds.
	// Set by OnReferNotifyUnmatched.
	onReferNotifyUnmatched func(ReferNotify)

	// answeringMu guards answering, ackAnswerErr, ackCarriesAnswer and
	// timerAnswer, which the answer to an inbound call and the reading of its
	// ACK share, acking, and earlyAnswerSDP, earlyUnkeyed, earlyAnswerApplied
	// and earlyAnswerTag. It is a lock of
	// the session's own and is never held across a call.
	answeringMu sync.Mutex
	// answering is set by an answer before it sends its 2xx, and closed when
	// that answer returns: after the ACK is read and, for an answer that sets
	// up its media after the ACK, after that too. awaitAnswer waits on it.
	answering chan struct{}
	// ackAnswerErr is why the media of the call could not be completed with
	// the ACK to our 2xx: the answer the ACK carried to the offer in the 2xx
	// could not be applied or keyed, or was missing. ReadAck sets it before
	// the ACK confirms the dialog, for the answer waiting on that ACK to
	// report.
	ackAnswerErr error
	// ackCarriesAnswer is set by AnswerLate before its 2xx, which carries our
	// offer, so the ACK confirming the dialog must carry the answer (RFC 3261
	// section 13.2.1).
	ackCarriesAnswer bool
	// acking is set by the Ack of an outbound call before it sends the ACK,
	// and closed when that Ack returns: once the media it finalizes is keyed,
	// or has failed. awaitAck waits on it.
	acking chan struct{}
	// earlyAnswerSDP is the answer the 183 of ProgressMedia carried, which the
	// 2xx repeats: RFC 3261 section 13.2.1 has an answer placed in a
	// provisional response be "that same exact answer". Describing the media
	// again could change it, and the media may not be asked to while a
	// handshake ProgressMedia left running uses it. Nil on a call ProgressMedia
	// gave no early media.
	earlyAnswerSDP []byte
	// earlyUnkeyed is set when the answer in a 183 is out, or was taken, and
	// the peer had not keyed the media within the wait it was given: on an
	// inbound call ProgressMedia then returned ErrEarlyMediaNotKeyed, and on
	// an outbound one the call goes on as one without early media. The
	// handshake goes on, and the media completes it when the ACK is read, or
	// sent. It is read until the INVITE is answered, and not after.
	earlyUnkeyed bool
	// earlyAnswerApplied is set on an outbound call once the answer in a 183 to
	// our INVITE was applied to its media, and earlyAnswerTag is the To tag of
	// that 183, the early dialog the answer belongs to. The first session
	// description is the answer (RFC 3261 section 13.2.1), so no later one in
	// a response of that dialog to the INVITE is applied.
	earlyAnswerApplied bool
	earlyAnswerTag   string
	// timerAnswer is the RFC 4028 session timer the answer negotiated, which
	// its 2xx carries and starts; nil until an answer negotiates one. Guarded
	// by answeringMu.
	timerAnswer *timerAnswer

	// sessionTimers is the RFC 4028 policy of the Diago that made this dialog,
	// which the answer to an inbound call negotiates with. Set before the
	// dialog is handed out and not changed after.
	sessionTimers SessionTimerPolicy
	// timer is the running session timer, set once by the 2xx that answered
	// the INVITE with one, and nil without one. Our re-INVITEs carry its
	// headers and their 2xx sets it, as does the 2xx we send to a re-INVITE
	// of the peer's.
	timer atomic.Pointer[sessionTimer]

	// requestMu makes the peer's re-INVITEs take turns, each held for the
	// whole of its handling, until the ACK to its 2xx is read. It is taken
	// before answerMu and mediaUpdateMu.
	requestMu sync.Mutex

	// answerMu makes the peer's BYE and the final response to its re-INVITE
	// take turns. The BYE holds it while it ends the dialog and answers, and
	// answers a re-INVITE still pending 487 (RFC 3261 section 15.1.2); a
	// re-INVITE is answered under it only while the dialog is live. A
	// re-INVITE is then answered either before a BYE ends the dialog or with
	// 487, never with a 200 behind the BYE's, and the BYE never waits for the
	// re-INVITE's media update.
	answerMu sync.Mutex
	// peerReInvite is the peer's re-INVITE being handled, until its handling
	// ends. Guarded by answerMu.
	peerReInvite *pendingReInvite
	// ackTimers are the timers of the retransmission of a 2xx to a re-INVITE
	// of the peer's. They are set before the dialog is used, and zero outside
	// tests.
	ackTimers ackTimers

	// mediaUpdateMu guards mediaUpdateDone.
	mediaUpdateMu sync.Mutex
	// mediaUpdateDone is set while a re-INVITE of the peer's or one of ours
	// is in progress, and closed when it ends. Our re-INVITE waits for the
	// peer's to end, and the peer's finding ours in progress is answered 491
	// (RFC 3261 sections 14.1 and 14.2), so the media is asked for one
	// offer/answer exchange at a time.
	mediaUpdateDone chan struct{}

	// answers counts the answers still running from their final response on,
	// each on a goroutine of the dialog's (answerWithin). Close waits for
	// them.
	answers sync.WaitGroup

	// log is the logger of the Diago that made this dialog, nil for a dialog
	// no Diago made. Set before the dialog is handed out and not changed
	// after; read it through logger.
	log *slog.Logger
}

// logger returns the logger the dialog reports with: its Diago's, or the
// default one for a dialog no Diago made.
func (s *sessionState) logger() *slog.Logger {
	if s.log != nil {
		return s.log
	}
	return sip.DefaultLogger()
}

// leaveCache removes the dialog from the dialog cache it was stored in, if
// any.
func (s *sessionState) leaveCache() error {
	if s.uncache == nil {
		return nil
	}
	return s.uncache()
}
