// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

var (
	HTTPDebug = os.Getenv("HTTP_DEBUG") == "true"

	DefaultPlaybackHTTPClient = http.Client{
		Timeout: 20 * time.Second,
	}

	errNoRTPSession = errors.New("no rtp session")

	// ErrNoMediaSetup is returned when the dialog's audio is asked for before
	// its media is set up: before the answer, or before ProgressMedia keys early
	// media, and so after ErrEarlyMediaNotKeyed.
	ErrNoMediaSetup = errors.New("no media setup")

	// errMediaUpdateAfterAnswer marks a media update that failed after its
	// answer was sent. The peer has moved to the new session by then, so the
	// call has no media left.
	errMediaUpdateAfterAnswer = errors.New("media update failed after its answer")

	// errMediaClosed refuses to install a media session once the dialog media
	// is closed or the dialog has ended, since nothing would close it.
	errMediaClosed = errors.New("dialog media closed")
)

// jitterBufferStopTimeout bounds how long Close waits for a jitter buffer's
// read loop once the sockets it reads are closed. Closing them fails its read
// at once, so the bound only matters for a read Close did not end.
const jitterBufferStopTimeout = 5 * time.Second

func init() {
	if HTTPDebug {
		DefaultPlaybackHTTPClient.Transport = &loggingTransport{}
	}
}

// DialogMedia is common struct for server and client session and it shares same functionality
// which is mostly arround media
type DialogMedia struct {
	mu sync.Mutex

	// requestMu makes the peer's re-INVITEs take turns, each held for the
	// whole of its handling. It is taken before mu and answerMu.
	requestMu sync.Mutex

	// answerMu makes the peer's BYE and the final response to its re-INVITE
	// take turns. The BYE holds it while it ends the dialog and answers, and
	// answers a re-INVITE still pending 487 (RFC 3261 section 15.1.2); a
	// re-INVITE is answered under it only while the dialog is live. A
	// re-INVITE is then answered either before a BYE ends the dialog or with
	// 487, never with a 200 behind the BYE's, and the BYE never waits for the
	// re-INVITE's media update. It is never taken with mu held, since ending
	// the dialog runs the state callbacks, which may take mu.
	answerMu sync.Mutex
	// peerReInvite is the peer's re-INVITE being handled, until its handling
	// ends. Guarded by answerMu.
	peerReInvite *pendingReInvite

	// media session is RTP local and remote
	// it is forked on media changes and updated on writer and reader
	// must be mutex protected
	// It MUST be always created on Media Session Init
	// Only safe to use after dialog Answered (Completed state)
	mediaSession *media.MediaSession

	// rtp session is created for usage with RTPPacketReader and RTPPacketWriter
	// it adds RTCP layer and RTP monitoring before passing packets to MediaSession
	rtpSession *media.RTPSession

	// iceAgentOwner is the media session that created this dialog's ICE agent,
	// kept after a re-INVITE replaced it with a fork. The fork shares the ICE
	// pair but not the agent, and closing the pair does not release the UDP
	// mux or the socket it wraps; only the owner's Close does. Dropping the
	// owner on the swap would leak that socket and its mux goroutine for the
	// life of the process, and hand the port back to the allocator while it is
	// still bound. Nil until a swap, and for a dialog without ICE.
	iceAgentOwner *media.MediaSession
	// Packet reader is default reader for RTP audio stream
	// Use always AudioReader to get current Audio reader
	// Use this only as read only
	// It MUST be always created on Media Session Init
	// Only safe to use after dialog Answered (Completed state)
	RTPPacketReader *media.RTPPacketReader

	// Packet writer is default writer for RTP audio stream
	// Use always AudioWriter to get current Audio reader
	// Use this only as read only
	RTPPacketWriter *media.RTPPacketWriter

	// jitterBuffer is the buffer WithAudioReaderJitterBuffer put between the
	// RTP session and RTPPacketReader, nil without one. A media update moves it
	// onto the new RTP session rather than taking it off the packet reader.
	jitterBuffer *media.RTPJitterBuffer

	// In case we are chaining audio readers
	audioReader io.Reader
	audioWriter io.Writer

	// remoteContactTarget is actual target changed caused by incomign or outgoing REINVITE
	// We do not use sipgo as this needs mutex but also keeping original invite
	remoteContactTarget *sip.ContactHeader

	onReferNotify func(statusCode int)

	// referMu guards referAttempts, referHighestCSeq, referLatest and the
	// attempts they hold. It is separate from mu so observing a REFER NOTIFY
	// never contends with media state.
	referMu sync.Mutex
	// referAttempts are the registered REFER attempts, oldest first: those
	// waiting for their outcome, and those whose wait ended on the deadline or
	// on cancellation and can still receive a late terminal NOTIFY.
	referAttempts []*referAttempt
	// referHighestCSeq is the highest CSeq recorded for a REFER sent on this
	// dialog, dropped attempts included. In-dialog CSeqs only grow, so an Event
	// id at or below it belongs to an earlier REFER.
	referHighestCSeq uint32
	// referLatest is the attempt begun most recently. It is kept after it is
	// dropped, so a NOTIFY without an Event id is never given to an older
	// attempt once a newer one has started.
	referLatest *referAttempt

	// releaseRTPPort returns this dialog RTP port to the allocator that handed
	// it out. Nil when no allocator is installed, as the OS takes the port back
	// when the socket closes. Set next to mediaSession and consumed by Close.
	releaseRTPPort func()

	onClose       func() error
	onMediaUpdate func(*DialogMedia)

	// ackTimers are the timers of the retransmission of a 2xx to a re-INVITE
	// of the peer's. They are set before the dialog is used, and zero outside
	// tests.
	ackTimers ackTimers

	// mediaUpdating is set while a media update waits, with mu released, for
	// the handshake of a new DTLS association. Another update is kept out
	// until it is done.
	mediaUpdating bool

	// ackOffer is the offer we sent in the 2xx to a re-INVITE of the peer's
	// that carried none, until the ACK to that 2xx brings the answer (RFC 3261
	// section 14.2). Guarded by mu.
	ackOffer *ackOffer

	// mediaUpdateDone is set while a re-INVITE of the peer's or one of ours
	// is in progress, and closed when it ends. Our re-INVITE waits for the
	// peer's to end, and the peer's finding ours in progress is answered 491
	// (RFC 3261 sections 14.1 and 14.2). Guarded by mu.
	mediaUpdateDone chan struct{}

	closed bool
}

// ackOffer is an offer we sent in a 2xx to a re-INVITE, awaiting its answer in
// the ACK.
type ackOffer struct {
	// cseq is the CSeq of the re-INVITE, which the ACK to its 2xx carries.
	cseq uint32
	// msess is the fork that made the offer, which the answer is applied to.
	msess *media.MediaSession
}

// pendingReInvite is a re-INVITE of the peer's being handled. Every field is
// guarded by DialogMedia.answerMu, but for ackErr, which retransmitted
// publishes.
type pendingReInvite struct {
	req *sip.Request
	tx  sip.ServerTransaction
	// cseq is the CSeq of the re-INVITE, which the ACK to its 2xx carries.
	cseq uint32
	// dialog is the dialog the re-INVITE belongs to. Once it has ended the
	// re-INVITE is answered 487.
	dialog *sipgo.Dialog
	// answered is set once the re-INVITE has its final response.
	answered bool
	// acked is made when a 2xx is sent, and closed once its ACK is read.
	acked chan struct{}
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
// and RFC 3261 section 12.2.2 has it answered 481.
func (d *DialogMedia) beginPeerReInvite(dialog *sipgo.Dialog, req *sip.Request, tx sip.ServerTransaction) (bool, error) {
	d.answerMu.Lock()
	defer d.answerMu.Unlock()
	if dialog.LoadState() == sip.DialogStateEnded {
		res := sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)
		return true, tx.Respond(res)
	}
	p := &pendingReInvite{req: req, tx: tx, dialog: dialog}
	if cseq := req.CSeq(); cseq != nil {
		p.cseq = cseq.SeqNo
	}
	d.peerReInvite = p
	return false, nil
}

// endPeerReInvite waits until the retransmission of a 2xx sent to the
// re-INVITE beginPeerReInvite recorded for tx has ended, and then forgets the
// re-INVITE. It returns errReInviteAckTimeout when the ACK did not come, for
// the caller to end the call.
func (d *DialogMedia) endPeerReInvite(tx sip.ServerTransaction) error {
	d.answerMu.Lock()
	p := d.peerReInvite
	if p == nil || p.tx != tx {
		d.answerMu.Unlock()
		return nil
	}
	retransmitted := p.retransmitted
	d.answerMu.Unlock()

	var err error
	if retransmitted != nil {
		<-retransmitted
		err = p.ackErr
	}

	d.answerMu.Lock()
	if d.peerReInvite == p {
		d.peerReInvite = nil
	}
	d.answerMu.Unlock()
	return err
}

// ackPeerReInvite tells the re-INVITE of the peer's being handled that ack,
// the ACK to our 2xx, has been read, which ends the retransmission of the 2xx.
func (d *DialogMedia) ackPeerReInvite(ack *sip.Request) {
	cseq := ack.CSeq()
	if cseq == nil {
		return
	}
	d.answerMu.Lock()
	defer d.answerMu.Unlock()
	p := d.peerReInvite
	if p == nil || p.acked == nil || p.cseq != cseq.SeqNo {
		return
	}
	select {
	case <-p.acked:
	default:
		close(p.acked)
	}
}

// retransmitReInvite2xx passes res, the 2xx sent to the re-INVITE p, to the
// transaction again until its ACK is read: the server transaction does not
// retransmit a 2xx, the UAS core does (RFC 3261 section 13.3.1.4, RFC 6026
// section 7.1). The interval starts at T1 and doubles up to T2. It gives up
// with errReInviteAckTimeout after 64*T1, and stops once the dialog has ended:
// a retransmission is sent under answerMu while the dialog is live, so none
// follows the 200 to a BYE.
func (d *DialogMedia) retransmitReInvite2xx(p *pendingReInvite, res *sip.Response) {
	defer close(p.retransmitted)
	t1, t2, timeout := d.ackTimers.values()
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
			d.answerMu.Lock()
			ended := p.dialog.LoadState() == sip.DialogStateEnded
			var err error
			if !ended {
				err = p.tx.Respond(res)
			}
			d.answerMu.Unlock()
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
func (d *DialogMedia) respondPeerReInvite(tx sip.ServerTransaction, res *sip.Response) (bool, error) {
	d.answerMu.Lock()
	defer d.answerMu.Unlock()
	p := d.peerReInvite
	if p == nil || p.tx != tx {
		return true, tx.Respond(res)
	}
	if p.answered {
		return false, nil
	}
	p.answered = true
	if p.dialog.LoadState() == sip.DialogStateEnded {
		return false, tx.Respond(sip.NewResponseFromRequest(p.req, sip.StatusRequestTerminated, "Request Terminated", nil))
	}
	if !res.IsSuccess() {
		return true, tx.Respond(res)
	}
	// Made before the 2xx goes out, which its ACK can follow at once.
	p.acked = make(chan struct{})
	if err := tx.Respond(res); err != nil {
		return true, err
	}
	p.retransmitted = make(chan struct{})
	go d.retransmitReInvite2xx(p, res)
	return true, nil
}

// terminatePeerReInviteLocked answers the peer's re-INVITE 487 when it is
// still pending once a BYE has ended the dialog: RFC 3261 section 15.1.2 has
// every pending request answered, and recommends 487. Called with answerMu
// held.
func (d *DialogMedia) terminatePeerReInviteLocked() error {
	p := d.peerReInvite
	if p == nil || p.answered {
		return nil
	}
	p.answered = true
	return p.tx.Respond(sip.NewResponseFromRequest(p.req, sip.StatusRequestTerminated, "Request Terminated", nil))
}

// referAttempt is one REFER sent on the dialog and what has been observed for
// it. Every field is guarded by DialogMedia.referMu.
type referAttempt struct {
	// cseq is the CSeq of the sent REFER. RFC 3515 makes the NOTIFY's Event id
	// the CSeq of the REFER that created the subscription, so this is what an
	// id-carrying NOTIFY is matched against.
	cseq uint32
	// cseqKnown is false until setReferAttemptCSeq records the sent REFER's CSeq.
	// Until then an id-carrying NOTIFY cannot be told apart from this attempt's.
	cseqKnown bool
	// registered is true while the attempt is in referAttempts.
	registered bool
	// end is how the attempt ended, zero while its wait is open.
	end ReferEnd
	// notifies are the NOTIFYs recorded while the wait was open, at most
	// maxReferNotifies, and dropped counts the ones not kept.
	notifies []ReferNotify
	dropped  int
	// wake tells the waiting REFER that a NOTIFY ended the attempt. Capacity 1,
	// sent to without blocking.
	wake chan struct{}
	// onLate receives a terminal NOTIFY that arrives after the wait ended on the
	// deadline or on cancellation.
	onLate func(ReferLateNotify)
}

// beginReferAttempt registers a new REFER attempt as the most recent one and
// returns it. Called before the REFER is sent so a NOTIFY that races ahead of
// the REFER's response is recorded rather than lost.
func (d *DialogMedia) beginReferAttempt(onLate func(ReferLateNotify)) *referAttempt {
	a := &referAttempt{registered: true, wake: make(chan struct{}, 1), onLate: onLate}
	d.referMu.Lock()
	d.referAttempts = append(d.referAttempts, a)
	d.referLatest = a
	d.referMu.Unlock()
	return a
}

// setReferAttemptCSeq records the sent REFER's CSeq: on the dialog's high-water
// mark whatever a's state, and on a only while a is registered.
func (d *DialogMedia) setReferAttemptCSeq(a *referAttempt, cseq uint32) {
	d.referMu.Lock()
	if cseq > d.referHighestCSeq {
		d.referHighestCSeq = cseq
	}
	if a.registered {
		a.cseq = cseq
		a.cseqKnown = true
	}
	d.referMu.Unlock()
}

// observeReferNotify routes one parsed NOTIFY to the REFER attempt it belongs
// to. With an Event id that is the registered attempt whose known CSeq equals
// it, or else the most recent attempt while its CSeq is still unknown, provided
// no other attempt awaits its CSeq and the id is above every REFER CSeq the
// dialog has recorded; without an id it is the most recent attempt. A NOTIFY
// with no such attempt is dropped.
//
// While the attempt's wait is open the NOTIFY is recorded, and a terminal one
// ends the wait. After the wait ended on the deadline or on cancellation a
// terminal NOTIFY is late: the attempt is dropped and its late callback is
// returned with the late NOTIFY, for the caller to run once the lock is
// released. The returned callback is nil when there is nothing to deliver.
func (d *DialogMedia) observeReferNotify(n ReferNotify) (onLate func(ReferLateNotify), late ReferLateNotify) {
	d.referMu.Lock()
	defer d.referMu.Unlock()

	a := d.referNotifyTargetLocked(n)
	if a == nil {
		return nil, ReferLateNotify{}
	}
	switch a.end {
	case 0:
		a.recordLocked(n)
	case ReferEndDeadline, ReferEndCancelled:
		if !n.endsSubscription() {
			return nil, ReferLateNotify{}
		}
		d.removeReferAttemptLocked(a)
		return a.onLate, ReferLateNotify{CSeq: a.cseq, CSeqKnown: a.cseqKnown, Notify: n}
	case ReferEndFinalNotify, ReferEndRefused, ReferEndSubscriptionEnded:
		// The attempt's outcome is already decided and about to be collected.
	}
	return nil, ReferLateNotify{}
}

// referNotifyExpected reports whether a NOTIFY carrying req's Event header has
// a REFER sent on this dialog to go to: the OnNotify callback of ReferOptions,
// or a registered attempt it is routed to.
func (d *DialogMedia) referNotifyExpected(req *sip.Request) bool {
	d.mu.Lock()
	onNotify := d.onReferNotify
	d.mu.Unlock()
	if onNotify != nil {
		return true
	}

	var n ReferNotify
	n.EventID, n.HasEventID = parseReferNotifyEventID(req)
	d.referMu.Lock()
	defer d.referMu.Unlock()
	return d.referNotifyTargetLocked(n) != nil
}

// referNotifyTargetLocked returns the registered attempt n belongs to, or nil.
// Called with referMu held.
func (d *DialogMedia) referNotifyTargetLocked(n ReferNotify) *referAttempt {
	if n.HasEventID {
		awaitingCSeq := 0
		for _, a := range d.referAttempts {
			if !a.cseqKnown {
				awaitingCSeq++
				continue
			}
			if a.cseq == n.EventID {
				return a
			}
		}
		// The NOTIFY may have raced ahead of the REFER's response, while the
		// most recent attempt's CSeq is not known yet. In-dialog CSeqs only
		// grow, so an id at or below the highest REFER CSeq recorded belongs to
		// an earlier REFER, even one already dropped. With a second attempt
		// also awaiting its CSeq the id may be that attempt's, so it is given
		// to neither.
		if latest := d.referLatest; latest != nil && latest.registered && !latest.cseqKnown &&
			awaitingCSeq == 1 && n.EventID > d.referHighestCSeq {
			return latest
		}
		return nil
	}
	if latest := d.referLatest; latest != nil && latest.registered {
		return latest
	}
	return nil
}

// recordLocked records n on an attempt whose wait is open, and ends the wait
// when n is terminal. Past maxReferNotifies a non-terminal NOTIFY is only
// counted, and a terminal one takes the last slot so it is always last.
// Called with referMu held.
func (a *referAttempt) recordLocked(n ReferNotify) {
	switch {
	case len(a.notifies) < maxReferNotifies:
		a.notifies = append(a.notifies, n)
	case n.endsSubscription():
		a.notifies[len(a.notifies)-1] = n
		a.dropped++
	default:
		a.dropped++
	}

	switch {
	case n.isFinal():
		a.end = ReferEndFinalNotify
	case n.endsSubscription():
		a.end = ReferEndSubscriptionEnded
	default:
		return
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// finishReferAttempt ends a's wait with end unless a NOTIFY already ended it,
// or with ReferEndRefused regardless, and returns what a observed. An attempt
// that ended on a final NOTIFY, an ended subscription or a refused REFER is
// dropped; one that ended on the deadline or on cancellation stays registered
// for a late terminal NOTIFY.
func (d *DialogMedia) finishReferAttempt(a *referAttempt, end ReferEnd) ReferObservation {
	d.referMu.Lock()
	defer d.referMu.Unlock()

	// A refused REFER created no subscription, so its response decides the end
	// even if a NOTIFY claiming an outcome raced ahead of it.
	if a.end == 0 || end == ReferEndRefused {
		a.end = end
	}
	switch a.end {
	case ReferEndFinalNotify, ReferEndSubscriptionEnded, ReferEndRefused:
		d.removeReferAttemptLocked(a)
	case ReferEndDeadline, ReferEndCancelled:
		// Stays registered, so a late terminal NOTIFY finds this attempt.
	}
	return ReferObservation{
		End:             a.end,
		CSeq:            a.cseq,
		CSeqKnown:       a.cseqKnown,
		Notifies:        slices.Clone(a.notifies),
		NotifiesDropped: a.dropped,
	}
}

// dropReferAttempt unregisters a. Dropping an attempt never affects another.
func (d *DialogMedia) dropReferAttempt(a *referAttempt) {
	d.referMu.Lock()
	d.removeReferAttemptLocked(a)
	d.referMu.Unlock()
}

// removeReferAttemptLocked unregisters a, leaving referLatest as it is. Called
// with referMu held.
func (d *DialogMedia) removeReferAttemptLocked(a *referAttempt) {
	if !a.registered {
		return
	}
	a.registered = false
	d.referAttempts = slices.DeleteFunc(d.referAttempts, func(r *referAttempt) bool { return r == a })
}

func (d *DialogMedia) Close() error {
	// Any hook attached
	// Prevent double exec
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true

	onClose := d.onClose
	d.onClose = nil
	m := d.mediaSession
	rtpSess := d.rtpSession
	iceOwner := d.iceAgentOwner
	d.iceAgentOwner = nil
	releasePort := d.releaseRTPPort
	d.releaseRTPPort = nil
	jitter := d.jitterBuffer

	d.mu.Unlock()

	// A NOTIFY after close finds no REFER attempt, late ones included.
	d.referMu.Lock()
	for _, a := range d.referAttempts {
		a.registered = false
	}
	d.referAttempts = nil
	d.referLatest = nil
	d.referMu.Unlock()

	var e1, e2, e3, e4, e5 error
	if onClose != nil {
		e1 = onClose()
	}

	if rtpSess != nil {
		e2 = rtpSess.MonitorClose()
	}

	if m != nil {
		e3 = m.Close()
	}

	// The agent goes after the session using its pair, so nothing is still
	// reading or writing on the socket it releases, and before the port is
	// returned below.
	if iceOwner != nil && iceOwner != m {
		e4 = iceOwner.Close()
	}

	// The jitter buffer was closed with the hooks above, but its read loop may
	// still be in a read on the session. Closing the sockets fails that read,
	// and the loop has stopped reading them once Done is closed.
	if jitter != nil {
		select {
		case <-jitter.Done():
		case <-time.After(jitterBufferStopTimeout):
			e5 = errors.New("jitter buffer read loop did not stop")
		}
	}

	// After the sockets are closed, never before. An allocator may hold the port
	// for a drain window so late RTP from this call can not land on the next
	// call socket, and that window has to start when the wire is actually down.
	// The closed latch above makes this exactly once.
	if releasePort != nil {
		releasePort()
	}
	return errors.Join(e1, e2, e3, e4, e5)
}

func (d *DialogMedia) OnClose(f func() error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onCloseUnsafe(f)
}

func (d *DialogMedia) onCloseUnsafe(f func() error) {
	if d.onClose != nil {
		prev := d.onClose
		d.onClose = func() error {
			return errors.Join(prev(), f())
		}
		return
	}
	d.onClose = f
}

func (d *DialogMedia) InitMediaSession(m *media.MediaSession, r *media.RTPPacketReader, w *media.RTPPacketWriter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initMediaSessionUnsafe(m, r, w)
}

func (d *DialogMedia) initMediaSessionUnsafe(m *media.MediaSession, r *media.RTPPacketReader, w *media.RTPPacketWriter) {
	d.mediaSession = m
	d.RTPPacketReader = r
	d.RTPPacketWriter = w
}

func (d *DialogMedia) initRTPSessionUnsafe(m *media.MediaSession, rtpSess *media.RTPSession) {
	d.mediaSession = m
	d.rtpSession = rtpSess
	d.RTPPacketReader = media.NewRTPPacketReaderSession(rtpSess)
	d.RTPPacketWriter = media.NewRTPPacketWriterSession(rtpSess)
}

func (d *DialogMedia) initMediaSessionFromConf(conf MediaConfig) error {
	if d.mediaSession != nil {
		// To allow testing or customizing current underhood session, this may be
		// precreated, so we want to return if already initialized.
		// Ex: To fake IO on RTP connection or different media stacks
		return nil
	}

	bindIP := conf.bindIP
	if bindIP == nil {
		var err error
		bindIP, _, err = sip.ResolveInterfacesIP("ip4", nil)
		if err != nil {
			return err
		}
	}

	// Port 0 leaves the choice to the OS or to the media package port range. An
	// allocator, when installed, makes it instead, so the port comes from a range
	// the operator has bounded and opened on the firewall.
	rtpPort := 0
	var releasePort func()
	if alloc := conf.RTPPortAllocator; alloc != nil {
		port, err := alloc.AllocateRTPPort()
		if err != nil {
			return err
		}
		rtpPort = port
		releasePort = func() { alloc.ReleaseRTPPort(port) }
	}

	// The session takes DTLS by value, so an unset config is the zero one. That
	// keeps a caller that never touched DTLS on the path it had before.
	var dtlsConf media.DTLSConfig
	if conf.DTLSConfig != nil {
		dtlsConf = *conf.DTLSConfig
	}

	sess := &media.MediaSession{
		Codecs:     slices.Clone(conf.Codecs),
		Laddr:      net.UDPAddr{IP: bindIP, Port: rtpPort},
		ExternalIP: conf.externalIP,
		Mode:       sdp.ModeSendrecv,
		SecureRTP:  conf.secureRTP,
		SRTPAlg:    conf.SecureRTPAlg,
		RTPNAT:     conf.rtpNAT,
		DTLSConf:   dtlsConf,
		ICEConf:    conf.ICEConfig,
	}

	if err := sess.Init(); err != nil {
		// Nothing was bound, so the port goes straight back rather than wait for
		// a Close that will not come: this dialog has no media session.
		if releasePort != nil {
			releasePort()
		}
		return err
	}
	d.mediaSession = sess
	d.releaseRTPPort = releasePort
	return nil
}

// RTPSession returns underhood rtp session
// NOTE: this can be nil
func (d *DialogMedia) RTPSession() *media.RTPSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rtpSession
}

func (d *DialogMedia) MediaSession() *media.MediaSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mediaSession
}

// handleMediaUpdate answers an in-dialog INVITE and applies its offer, if it
// has one, to a fork of the media session. ctx is the dialog's: it bounds a
// DTLS handshake the offer asks for, which the end of the dialog ends. An error
// wrapping errMediaUpdateAfterAnswer means the update failed after the 2xx was
// sent, and the caller ends the call.
//
// The answer is sent through respondPeerReInvite with mu released, so a BYE
// that ends the dialog meanwhile has it answered 487 instead.
func (d *DialogMedia) handleMediaUpdate(ctx context.Context, req *sip.Request, tx sip.ServerTransaction, contactHDR sip.Header) error {
	respond := func(res *sip.Response) error {
		_, err := d.respondPeerReInvite(tx, res)
		return err
	}

	d.mu.Lock()
	// A BYE handled while this request was pending has closed the media. The
	// request is still answered, with the 487 RFC 3261 section 15.1.2
	// recommends, and nothing is negotiated on the closed session.
	if d.closed {
		d.mu.Unlock()
		return respond(sip.NewResponseFromRequest(req, sip.StatusRequestTerminated, "Request Terminated", nil))
	}
	// The previous update is still running the handshake of a new DTLS
	// association. RFC 3261 section 14.2 lets a request that cannot be taken
	// now be retried after a 491.
	if d.mediaUpdating {
		d.mu.Unlock()
		return respond(sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil))
	}
	d.remoteContactTarget = req.Contact().Clone()

	// When body is not present this can mean client is doing keep alive
	// Still offer needs to be responded.
	// Content-Length: 0 can reach us as an empty but non nil body, so length is
	// checked rather than nilness.
	if len(req.Body()) > 0 {
		msess, err := d.sdpReInviteUnsafe(req.Body())
		if err == nil && msess.FinalizePending() {
			d.mediaUpdating = true
			d.mu.Unlock()
			return d.answerNewAssociation(ctx, req, tx, contactHDR, msess)
		}
		if err == nil {
			err = d.replaceRTPSessionUnsafe(msess)
		}
		if err != nil {
			d.mu.Unlock()
			// The offer is well formed but needs an ICE restart, to restart ICE
			// or to run a new DTLS association over it (RFC 8842 section 6),
			// which this session cannot perform. The fork was never installed,
			// so the call carries on over its current pair. The reason phrase
			// is fixed and carries no error text, so nothing internal reaches
			// the peer.
			if errors.Is(err, media.ErrICERestartUnsupported) {
				return respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptableHere, "Not Acceptable Here", nil))
			}
			return respond(sip.NewResponseFromRequest(req, sip.StatusRequestTerminated, "Request Terminated - "+err.Error(), nil))
		}

		if d.onMediaUpdate != nil {
			d.mu.Unlock()
			d.onMediaUpdate(d)
			d.mu.Lock()
		}
	}

	// A request for an offer can not be answered with an SDP we never built.
	// The bodied path rejects this inside sdpReInviteUnsafe.
	if d.mediaSession == nil {
		d.mu.Unlock()
		return respond(sip.NewResponseFromRequest(req, sip.StatusRequestTerminated, "Request Terminated - no media session present", nil))
	}

	var sd []byte
	var offer *ackOffer
	if len(req.Body()) > 0 {
		// The installed fork answers the peer's offer.
		sd = d.mediaSession.LocalSDP()
	} else {
		// Our 2xx carries an offer, which the ACK answers (RFC 3261 section
		// 14.2). A fork makes it and takes the answer, so neither changes the
		// installed session, which carries the media meanwhile.
		offer = &ackOffer{msess: d.mediaSession.Fork()}
		if cseq := req.CSeq(); cseq != nil {
			offer.cseq = cseq.SeqNo
		}
		sd = negotiatedOffer(offer.msess, d.mediaSession)
		d.ackOffer = offer
	}
	d.mu.Unlock()
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", sd)
	res.AppendHeader(contactHDR)
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	sent, err := d.respondPeerReInvite(tx, res)
	if offer != nil && !sent {
		// No 2xx went out, so no ACK will answer the offer.
		d.mu.Lock()
		if d.ackOffer == offer {
			d.ackOffer = nil
		}
		d.mu.Unlock()
	}
	return err
}

// applyAckAnswer applies the answer an ACK carries to the offer we sent in the
// 2xx to a re-INVITE of the peer's, on the fork that made the offer, and
// installs the fork. It reports whether req is the ACK to that 2xx. An ACK
// without an answer, or once the media is closed, drops the offer. ctx is the
// dialog's: it bounds a DTLS handshake the answer asks for.
//
// An ACK cannot be refused, so an error means the answer could not be applied
// or keyed, the media is left as it was, and the caller ends the call, as RFC
// 3261 section 14.2 has a UAC do with an offer it cannot accept.
func (d *DialogMedia) applyAckAnswer(ctx context.Context, req *sip.Request) (bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	offer := d.ackOffer
	cseq := req.CSeq()
	if offer == nil || cseq == nil || cseq.SeqNo != offer.cseq {
		return false, nil
	}
	d.ackOffer = nil
	msess := offer.msess

	contentType := req.ContentType()
	if d.closed || len(req.Body()) == 0 || contentType == nil || contentType.Value() != "application/sdp" {
		return true, d.discardForkUnsafe(msess)
	}
	msess.RemoteSDPIsAnswer = true
	if err := msess.RemoteSDP(req.Body()); err != nil {
		return true, errors.Join(fmt.Errorf("sdp update media remote SDP applying failed: %w", err), d.discardForkUnsafe(msess))
	}
	if msess.FinalizePending() {
		return true, d.finalizeAndReplaceUnsafe(ctx, msess)
	}
	return true, d.replaceRTPSessionUnsafe(msess)
}

// answerNewAssociation answers a re-INVITE whose offer msess found to ask for
// a new DTLS association, runs the handshake, and only then installs msess.
// The handshake needs the answer, which tells the peer where msess is and
// which certificate it presents (RFC 8842 section 5.3), and msess has no SRTP
// keys until it completes, so the current session carries the media
// meanwhile. It is called without mu and with mediaUpdating set, which keeps
// other media updates out until it clears it.
//
// A dialog that ends before the answer has the re-INVITE answered 487 by the
// BYE, and one that ends while the handshake runs ends it through ctx. Either
// way nothing is installed and the fork is discarded.
func (d *DialogMedia) answerNewAssociation(ctx context.Context, req *sip.Request, tx sip.ServerTransaction, contactHDR sip.Header, msess *media.MediaSession) error {
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", msess.LocalSDP())
	res.AppendHeader(contactHDR)
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	sent, err := d.respondPeerReInvite(tx, res)
	if err != nil || !sent {
		d.mu.Lock()
		d.mediaUpdating = false
		err = errors.Join(err, d.discardForkUnsafe(msess))
		d.mu.Unlock()
		return err
	}

	err = msess.FinalizeContext(ctx)

	d.mu.Lock()
	d.mediaUpdating = false
	switch {
	case ctx.Err() != nil:
		// The dialog ended, which is no failure of the update.
		err = d.discardForkUnsafe(msess)
		d.mu.Unlock()
		return err
	case err == nil && d.closed:
		err = fmt.Errorf("media closed during the media update")
	}
	if err != nil {
		err = errors.Join(err, d.discardForkUnsafe(msess))
	} else {
		err = d.replaceRTPSessionUnsafe(msess)
	}
	onMediaUpdate := d.onMediaUpdate
	d.mu.Unlock()
	if err != nil {
		return fmt.Errorf("%w: %w", errMediaUpdateAfterAnswer, err)
	}

	if onMediaUpdate != nil {
		onMediaUpdate(d)
	}
	return nil
}

// finalizeAndReplaceUnsafe runs the negotiation msess has pending and then
// installs it. d.mu is released while the negotiation waits for the peer, and
// mediaUpdating keeps other media updates out meanwhile. ctx is the dialog's.
// When the negotiation fails, or the dialog media is closed or the dialog ends
// meanwhile, msess is not installed and is discarded.
func (d *DialogMedia) finalizeAndReplaceUnsafe(ctx context.Context, msess *media.MediaSession) error {
	d.mediaUpdating = true
	d.mu.Unlock()
	err := msess.FinalizeContext(ctx)
	d.mu.Lock()
	d.mediaUpdating = false

	if err == nil && (d.closed || ctx.Err() != nil) {
		err = errMediaClosed
	}
	if err == nil {
		err = d.replaceRTPSessionUnsafe(msess)
	}
	if err != nil {
		return errors.Join(err, d.discardForkUnsafe(msess))
	}
	return nil
}

// beginOwnMediaUpdate waits, bounded by ctx, until no re-INVITE is in
// progress, and marks one of ours in progress until the returned func is
// called. RFC 3261 section 14.1 has a UAC start no re-INVITE while another
// INVITE transaction is in progress in either direction.
func (d *DialogMedia) beginOwnMediaUpdate(ctx context.Context) (func(), error) {
	for {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return nil, errMediaClosed
		}
		done := d.mediaUpdateDone
		if done == nil {
			done = make(chan struct{})
			d.mediaUpdateDone = done
			d.mu.Unlock()
			return func() { d.endMediaUpdate(done) }, nil
		}
		d.mu.Unlock()

		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// beginPeerMediaUpdate marks a re-INVITE of the peer's in progress until the
// returned func is called, and reports false when one of ours is in progress
// instead, which RFC 3261 section 14.2 has the peer's answered 491. The peer's
// re-INVITEs take turns under requestMu, so only ours can be in progress.
func (d *DialogMedia) beginPeerMediaUpdate() (func(), bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mediaUpdateDone != nil {
		return nil, false
	}
	done := make(chan struct{})
	d.mediaUpdateDone = done
	return func() { d.endMediaUpdate(done) }, true
}

// reInviteRetryWait waits, bounded by ctx, before a re-INVITE of ours answered
// 491 is sent again. callIDOwner tells whether we generated the dialog's
// Call-ID, as the caller does. RFC 3261 section 14.1:
//
//	If a UAC receives a 491 response to a re-INVITE, it SHOULD start a
//	timer with a value T chosen as follows:
//	   1. If the UAC is the owner of the Call-ID of the dialog ID
//	      (meaning it generated the value), T has a randomly chosen value
//	      between 2.1 and 4 seconds in units of 10 ms.
//	   2. If the UAC is not the owner of the Call-ID of the dialog ID, T
//	      has a randomly chosen value of between 0 and 2 seconds in units
//	      of 10 ms.
func reInviteRetryWait(ctx context.Context, callIDOwner bool) error {
	wait := time.Duration(mrand.IntN(201)) * 10 * time.Millisecond
	if callIDOwner {
		wait = time.Duration(210+mrand.IntN(191)) * 10 * time.Millisecond
	}
	select {
	case <-time.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (d *DialogMedia) endMediaUpdate(done chan struct{}) {
	d.mu.Lock()
	if d.mediaUpdateDone == done {
		d.mediaUpdateDone = nil
	}
	d.mu.Unlock()
	close(done)
}

// discardForkUnsafe closes a fork that is not going to be installed, if it was
// moved to sockets of its own. A fork on the sockets of the installed session
// is only dropped, since closing it would close them.
func (d *DialogMedia) discardForkUnsafe(msess *media.MediaSession) error {
	cur := d.mediaSession
	if cur != nil && cur.Laddr.IP.Equal(msess.Laddr.IP) && cur.Laddr.Port == msess.Laddr.Port {
		return nil
	}
	return msess.Close()
}

// sdpReInviteUnsafe applies the offer of an inbound re-INVITE to a fork of the
// media session, and returns the fork without installing it.
// Must be protected with lock
func (d *DialogMedia) sdpReInviteUnsafe(sdp []byte) (*media.MediaSession, error) {
	if d.mediaSession == nil {
		return nil, fmt.Errorf("no media session present")
	}

	// An inbound re-INVITE carries an offer, whichever side of the dialog we are.
	d.mediaSession.RemoteSDPIsAnswer = false
	msess := d.mediaSession.Fork()
	if err := msess.RemoteSDP(sdp); err != nil {
		return nil, fmt.Errorf("sdp update media remote SDP applying failed: %w", err)
	}
	return msess, nil
}

// negotiatedOffer returns the offer of fork, a fork of the installed session
// cur, made to renegotiate nothing. It carries the codecs cur runs on, with the
// payload types they were negotiated with, as the offer of cur itself would,
// so the peer is not asked to move the call to another codec. fork keeps every
// codec it was set up with for the answer and what follows.
func negotiatedOffer(fork, cur *media.MediaSession) []byte {
	codecs := fork.Codecs
	if negotiated := cur.CommonCodecs(); len(negotiated) > 0 {
		fork.Codecs = slices.Clone(negotiated)
	}
	offer := fork.LocalSDP()
	fork.Codecs = codecs
	return offer
}

func (d *DialogMedia) checkEarlyMedia(remoteSDP []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// RTP Session is only created when negotiation is finished. We use this to detect existing media
	if d.rtpSession == nil {
		return errNoRTPSession
	}
	return d.sdpUpdateUnsafe(remoteSDP)
}

func (d *DialogMedia) sdpUpdateUnsafe(sdp []byte) error {
	msess := d.mediaSession.Fork()
	if err := msess.RemoteSDP(sdp); err != nil {
		return fmt.Errorf("sdp update media remote SDP applying failed: %w", err)
	}

	return d.replaceRTPSessionUnsafe(msess)
}

func (d *DialogMedia) mediaUpdateUnsafe(msess *media.MediaSession) error {
	return d.replaceRTPSessionUnsafe(msess)
}

// replaceRTPSessionUnsafe replaces the RTP session after the old monitor has
// fully stopped. A fork preserves statistics and shared connections; a new
// session is used when media connections were recreated.
func (d *DialogMedia) replaceRTPSessionUnsafe(msess *media.MediaSession) error {
	// Close has taken what it closes already, and would not close msess.
	if d.closed {
		return errMediaClosed
	}
	oldRTPSess := d.rtpSession
	if oldRTPSess == nil {
		return fmt.Errorf("RTP Session is nil while trying to update it")
	}
	// This will block until read  RTCP is closed fully, which we need before starting new one
	if err := oldRTPSess.MonitorClose(); err != nil {
		return err
	}

	// Same as media, we are forking RTP Session
	rtpSess := oldRTPSess.Fork(msess)
	if err := rtpSess.MonitorBackground(); err != nil {
		return errors.Join(err, rtpSess.Close())
	}

	// Make sure any current reader is not consuming old media session. A
	// jitter buffer stays in front of the packet reader and reads the new
	// session itself, so its queue and playout carry over and no read loop is
	// left on the old session.
	if d.jitterBuffer != nil {
		d.jitterBuffer.UpdateRTPSession(rtpSess)
	} else {
		d.RTPPacketReader.UpdateRTPSession(rtpSess)
	}
	d.RTPPacketWriter.UpdateRTPSession(rtpSess)

	old := d.mediaSession
	// Every path forks from the session installed at setup, so exactly one
	// session ever owns an ICE agent, and it is the one replaced first. It is
	// kept for Close rather than closed here, because the fork's media still
	// runs on the pair that agent holds open.
	if old != nil && old != msess && old.OwnsICEAgent() && d.iceAgentOwner == nil {
		d.iceAgentOwner = old
	}
	d.mediaSession = msess
	d.rtpSession = rtpSess

	// A fork rebound to a new local address runs on sockets of its own, and the
	// reader and writer were moved off the replaced session above, so its
	// sockets carry nothing any more. Closing them releases the ports and fails
	// a read still blocked on the old RTP socket, which the packet reader then
	// retries on the new session. Without it that read never returns, because
	// the peer now sends to the new address.
	if old != nil && old != d.iceAgentOwner && (!old.Laddr.IP.Equal(msess.Laddr.IP) || old.Laddr.Port != msess.Laddr.Port) {
		if err := old.Close(); err != nil {
			return fmt.Errorf("closing replaced media session: %w", err)
		}
	}
	return nil
}

type AudioReaderOption func(d *DialogMedia) error

type MediaProps struct {
	Codec media.Codec
	Laddr string
	Raddr string
}

func WithAudioReaderMediaProps(p *MediaProps) AudioReaderOption {
	return func(d *DialogMedia) error {
		p.Codec = media.CodecAudioFromSession(d.mediaSession)
		p.Laddr = d.mediaSession.Laddr.String()
		p.Raddr = d.mediaSession.Raddr.String()
		return nil
	}
}

// WithAudioReaderJitterBuffer inserts an RTP jitter buffer before the payload reader.
// Playout starts at the packet duration of the negotiated audio codec and then
// follows the duration of the peer's packets, learned from their RTP timestamps
// in the codec's clock rate, or in opts.ClockRate when it is set.
// The buffer stays in place across media updates, reading each new RTP session.
// A dialog has at most one; asking for a second is an error.
func WithAudioReaderJitterBuffer(opts media.RTPJitterBufferOptions) AudioReaderOption {
	return func(d *DialogMedia) error {
		if d.mediaSession == nil || d.RTPPacketReader == nil {
			return fmt.Errorf("no media setup")
		}
		if d.jitterBuffer != nil {
			return fmt.Errorf("jitter buffer already set up")
		}

		codec := media.CodecAudioFromSession(d.mediaSession)
		if codec.SampleDur <= 0 {
			return fmt.Errorf("invalid audio codec packet duration: %s", codec.SampleDur)
		}

		reader := d.RTPPacketReader.Reader()
		if reader == nil {
			return fmt.Errorf("no RTP reader setup")
		}

		if opts.ClockRate == 0 {
			opts.ClockRate = codec.SampleRate
		}
		jitter := media.NewRTPJitterBuffer(reader, codec.SampleDur, opts)
		d.RTPPacketReader.UpdateReader(jitter)
		d.jitterBuffer = jitter

		d.onCloseUnsafe(jitter.Close)
		return nil
	}
}

// WithAudioReaderRTPStats creates RTP Statistics interceptor on audio reader
func WithAudioReaderRTPStats(hook media.OnRTPReadStats) AudioReaderOption {
	return func(d *DialogMedia) error {
		r := &media.RTPStatsReader{
			Reader:         d.getAudioReader(),
			RTPSession:     d.rtpSession,
			OnRTPReadStats: hook,
		}
		d.audioReader = r
		return nil
	}
}

// WithAudioReaderDTMF creates DTMF interceptor
func WithAudioReaderDTMF(r *DTMFReader) AudioReaderOption {
	return func(d *DialogMedia) error {
		r.dtmfReader = media.NewRTPDTMFReader(dtmfCodec(d.mediaSession), d.RTPPacketReader, d.getAudioReader())
		r.mediaSession = d.mediaSession

		d.audioReader = r
		return nil
	}
}

func WithAudioReaderPCMMonitor(mon *audio.MonitorPCMReader, w io.Writer) AudioReaderOption {
	return func(d *DialogMedia) error {
		codec := media.CodecAudioFromSession(d.mediaSession)
		if err := mon.Init(w, codec, d.getAudioReader()); err != nil {
			return err
		}
		mon.FlushOnError = true // It will flush on writer stop
		d.audioReader = mon
		return nil
	}
}

// AudioReader returns io.Reader on which you can read your ENCODED audio.
// By default it is RTPPacketReader unless overwritten with SetAudioReader().
//
// NOTE: AudioReader must be called after negotiation is finished, like Answer()
// Reading buffer should be equal or bigger of media.RTPBufSize
// Use AuidioListen for optimized reading.
//
// Before the media is set up it returns ErrNoMediaSetup.
func (d *DialogMedia) AudioReader(opts ...AudioReaderOption) (io.Reader, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// The options read the media session and the packet reader.
	if len(opts) > 0 && (d.mediaSession == nil || d.RTPPacketReader == nil) {
		return nil, ErrNoMediaSetup
	}
	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}
	r := d.getAudioReader()
	if r == nil {
		return nil, ErrNoMediaSetup
	}
	return r, nil
}

// getAudioReader returns the audio reader, or nil when there is none.
func (d *DialogMedia) getAudioReader() io.Reader {
	if d.audioReader != nil {
		return d.audioReader
	}
	if d.RTPPacketReader == nil {
		return nil
	}
	return d.RTPPacketReader
}

// audioReaderProps fills p from the media session and returns the audio
// reader. It returns nil when there is no reader, and without a media session,
// which describes the audio.
func (d *DialogMedia) audioReaderProps(p *MediaProps) io.Reader {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.mediaSession == nil {
		return nil
	}
	WithAudioReaderMediaProps(p)(d)
	return d.getAudioReader()
}

// SetAudioReader adds/changes audio reader.
// Use this when you want to have interceptors of your audio
func (d *DialogMedia) SetAudioReader(r io.Reader) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audioReader = r
}

type AudioWriterOption func(d *DialogMedia) error

func WithAudioWriterMediaProps(p *MediaProps) AudioWriterOption {
	return func(d *DialogMedia) error {
		p.Codec = media.CodecAudioFromSession(d.mediaSession)
		p.Laddr = d.mediaSession.Laddr.String()
		p.Raddr = d.mediaSession.Raddr.String()
		return nil
	}
}

// WithAudioReaderRTPStats creates RTP Statistics interceptor on audio reader
func WithAudioWriterRTPStats(hook media.OnRTPWriteStats) AudioWriterOption {
	return func(d *DialogMedia) error {
		w := media.RTPStatsWriter{
			Writer:          d.getAudioWriter(),
			RTPSession:      d.rtpSession,
			OnRTPWriteStats: hook,
		}
		d.audioWriter = &w
		return nil
	}
}

// WithAudioWriterDTMF adds DTMF into audio pipeline
func WithAudioWriterDTMF(r *DTMFWriter) AudioWriterOption {
	return func(d *DialogMedia) error {
		r.dtmfWriter = media.NewRTPDTMFWriter(dtmfCodec(d.mediaSession), d.RTPPacketWriter, d.getAudioWriter())
		r.mediaSession = d.mediaSession
		d.audioWriter = r
		return nil
	}
}

// WithAudioWriterMonitor initializes and adds PCM monitor in audio pipeline. It records and decodes stream into PCM.
func WithAudioWriterMonitor(mon *audio.MonitorPCMWriter, w io.Writer) AudioWriterOption {
	return func(d *DialogMedia) error {
		codec := media.CodecAudioFromSession(d.mediaSession)
		if err := mon.Init(w, codec, d.getAudioWriter()); err != nil {
			return err
		}
		mon.FlushOnError = true // It will flush on writer stop
		d.audioWriter = mon
		return nil
	}
}

// AudioWriter returns io.Writer on which you can write your ENCODED audio.
// By default it is RTPPacketWriter unless overwritten with SetAudioWriter().
// NOTE: RTPPacketWriter has running sample clock, but it expects samples sent, match sample duration of codec.
//
// Before the media is set up it returns ErrNoMediaSetup.
func (d *DialogMedia) AudioWriter(opts ...AudioWriterOption) (io.Writer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// The options read the media session and the packet writer.
	if len(opts) > 0 && (d.mediaSession == nil || d.RTPPacketWriter == nil) {
		return nil, ErrNoMediaSetup
	}
	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}

	w := d.getAudioWriter()
	if w == nil {
		return nil, ErrNoMediaSetup
	}
	return w, nil
}

// getAudioWriter returns the audio writer, or nil when there is none.
func (d *DialogMedia) getAudioWriter() io.Writer {
	if d.audioWriter != nil {
		return d.audioWriter
	}
	if d.RTPPacketWriter == nil {
		return nil
	}
	return d.RTPPacketWriter
}

// audioWriterProps fills p from the media session and returns the audio
// writer. It returns nil when there is no writer, and without a media session,
// which describes the audio.
func (d *DialogMedia) audioWriterProps(p *MediaProps) io.Writer {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.mediaSession == nil {
		return nil
	}
	WithAudioWriterMediaProps(p)(d)
	return d.getAudioWriter()
}

// SetAudioWriter adds/changes audio reader.
// Use this when you want to have pipelines of your audio
func (d *DialogMedia) SetAudioWriter(r io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audioWriter = r
}

func (d *DialogMedia) Media() *DialogMedia {
	return d
}

// Echo does audio echo for you
func (d *DialogMedia) Echo() error {
	audioR, err := d.AudioReader()
	if err != nil {
		return err
	}
	audioW, err := d.AudioWriter()
	if err != nil {
		return err
	}

	_, err = media.Copy(audioR, audioW)
	return err
}

// PlaybackCreate creates playback for audio
func (d *DialogMedia) PlaybackCreate() (AudioPlayback, error) {
	mprops := MediaProps{}
	w := d.audioWriterProps(&mprops)
	if w == nil {
		return AudioPlayback{}, ErrNoMediaSetup
	}
	p := NewAudioPlayback(w, mprops.Codec)
	// On each play it needs reset RTP timestamp
	d.mu.Lock()
	packetWriter := d.RTPPacketWriter
	d.mu.Unlock()
	if packetWriter != nil {
		p.onPlay = packetWriter.ResetTimestamp
	}
	return p, nil
}

// PlaybackControlCreate creates playback for audio with controls like mute unmute
func (d *DialogMedia) PlaybackControlCreate() (AudioPlaybackControl, error) {
	// NOTE we should avoid returning pointers for any IN dialplan to avoid heap
	mprops := MediaProps{}
	w := d.audioWriterProps(&mprops)

	if w == nil {
		return AudioPlaybackControl{}, ErrNoMediaSetup
	}
	// Audio is controled via audio reader/writer
	control := &audioControl{
		Writer: w,
	}

	p := AudioPlaybackControl{
		AudioPlayback: NewAudioPlayback(control, mprops.Codec),
		control:       control,
	}
	return p, nil
}

// PlaybackRingtoneCreate is creating playback for ringtone
//
// Experimental
func (d *DialogMedia) PlaybackRingtoneCreate() (AudioRingtone, error) {
	mprops := MediaProps{}
	w := d.audioWriterProps(&mprops)
	if w == nil {
		return AudioRingtone{}, ErrNoMediaSetup
	}

	ringtone, err := audio.RingtoneLoadPCM(mprops.Codec)
	if err != nil {
		return AudioRingtone{}, err
	}

	encoder := audio.PCMEncoderWriter{}
	if err := encoder.Init(mprops.Codec, w); err != nil {
		return AudioRingtone{}, err
	}

	ar := AudioRingtone{
		writer:       &encoder,
		ringtone:     ringtone,
		sampleSize:   mprops.Codec.Samples16(),
		mediaSession: d.mediaSession,
	}
	return ar, nil
}

// AudioStereoRecordingCreate creates Stereo Recording audio Pipeline and stores as Wav file format
// For audio to be recorded use AudioReader and AudioWriter from Recording
//
// Tips:
// If you want to make permanent in audio pipeline use SetAudioReader, SetAudioWriter
//
// NOTE: API WILL change
func (d *DialogMedia) AudioStereoRecordingCreate(wavFile *os.File) (AudioStereoRecordingWav, error) {
	mpropsW := MediaProps{}
	aw := d.audioWriterProps(&mpropsW)
	if aw == nil {
		return AudioStereoRecordingWav{}, ErrNoMediaSetup
	}

	mpropsR := MediaProps{}
	ar := d.audioReaderProps(&mpropsR)
	if ar == nil {
		return AudioStereoRecordingWav{}, ErrNoMediaSetup
	}

	return newDialogRecordingWav(wavFile, ar, mpropsR, aw, mpropsW)
}

// Listen keeps reading stream until it gets closed or deadlined
// Use ListenBackground or ListenContext for better control
func (d *DialogMedia) Listen() (err error) {
	buf := make([]byte, media.RTPBufSize)
	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}

	for {
		_, err := audioReader.Read(buf)
		if err != nil {
			return err
		}
	}
}

// ListenBackground listens on stream in background and allows correct stoping of stream on network layer
func (d *DialogMedia) ListenBackground() (stop func() error, err error) {
	buf := make([]byte, media.RTPBufSize)
	audioReader, err := d.AudioReader()
	if err != nil {
		return nil, err
	}

	wg := sync.WaitGroup{}
	var readErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, err := audioReader.Read(buf)
			if err != nil {
				if err, ok := err.(net.Error); ok && err.Timeout() {
					return
				}
				readErr = err
				return
			}
		}
	}()

	return func() error {
		if err := d.StopRTP(1, 0); err != nil {
			return err
		}
		wg.Wait() // This makes sure we have exited reading
		if err := d.StartRTP(1, 0); err != nil {
			return err
		}
		return readErr
	}, nil
}

// ListenContext listens until context is canceled.
func (d *DialogMedia) ListenContext(pctx context.Context) error {
	buf := make([]byte, media.RTPBufSize)
	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	go func() {
		<-ctx.Done()
		if pctx.Err() != nil {
			d.StopRTP(1, 0)
		}
	}()
	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}
	for {
		_, err := audioReader.Read(buf)
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Timeout() {
				return nil
			}
			return err
		}
	}
}

func (d *DialogMedia) ListenUntil(dur time.Duration) error {
	buf := make([]byte, media.RTPBufSize)

	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}
	d.StopRTP(1, dur)
	for {
		_, err := audioReader.Read(buf)
		if err != nil {
			return err
		}
	}
}

// StopRTP sets a read or write deadline, as MediaSession.StopRTP does, on the
// dialog's current media session, which it takes under the dialog's lock since
// a re-INVITE replaces the session under it. Without a media session it
// returns ErrNoMediaSetup.
func (d *DialogMedia) StopRTP(rw int8, dur time.Duration) error {
	sess := d.MediaSession()
	if sess == nil {
		return ErrNoMediaSetup
	}
	return sess.StopRTP(rw, dur)
}

// StartRTP clears the deadline StopRTP sets, on the dialog's current media
// session.
func (d *DialogMedia) StartRTP(rw int8, dur time.Duration) error {
	sess := d.MediaSession()
	if sess == nil {
		return ErrNoMediaSetup
	}
	return sess.StartRTP(rw)
}

// dtmfCodec returns the telephone-event codec DTMF is carried on for this
// session. It is the negotiated one: telephone-event is a dynamic format, so the
// number on the wire is the peer's rather than our default (RFC 3264 section
// 6.1).
//
// A session that negotiated no telephone-event keeps the package default, which
// is the behaviour every caller had before.
func dtmfCodec(s *media.MediaSession) media.Codec {
	if codec, ok := media.CodecTelephoneEventFromSession(s); ok {
		return codec
	}
	return media.CodecTelephoneEvent8000
}

type DTMFReader struct {
	mediaSession *media.MediaSession
	dtmfReader   *media.RTPDtmfReader
	onDTMF       func(dtmf rune) error
}

// AudioReaderDTMF is DTMF over RTP. It reads audio and provides hook for dtmf while listening for audio
// Use Listen or OnDTMF after this call
func (m *DialogMedia) AudioReaderDTMF() (*DTMFReader, error) {
	ar, err := m.AudioReader()
	if err != nil {
		return nil, err
	}
	return &DTMFReader{
		dtmfReader:   media.NewRTPDTMFReader(dtmfCodec(m.mediaSession), m.RTPPacketReader, ar),
		mediaSession: m.mediaSession,
	}, nil
}

func (d *DTMFReader) Listen(onDTMF func(dtmf rune) error, dur time.Duration) error {
	d.onDTMF = onDTMF
	buf := make([]byte, media.RTPBufSize)
	for {
		if _, err := d.readDeadline(buf, dur); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil
			}
			return err
		}
	}
}

// readDeadline(reads RTP until
func (d *DTMFReader) readDeadline(buf []byte, dur time.Duration) (n int, err error) {
	mediaSession := d.mediaSession
	if dur > 0 {
		// Stop RTP
		mediaSession.StopRTP(1, dur)
		defer mediaSession.StartRTP(2)
	}
	return d.Read(buf)
}

// OnDTMF must be called before audio reading
func (d *DTMFReader) OnDTMF(onDTMF func(dtmf rune) error) {
	d.onDTMF = onDTMF
}

// Read exposes io.Reader that can be used as AudioReader
func (d *DTMFReader) Read(buf []byte) (n int, err error) {
	// This is optimal way of reading audio and DTMF
	dtmfReader := d.dtmfReader
	n, err = dtmfReader.Read(buf)
	if err != nil {
		return n, err
	}

	if dtmf, ok := dtmfReader.ReadDTMF(); ok {
		if err := d.onDTMF(dtmf); err != nil {
			return n, err
		}
	}
	return n, nil
}

type DTMFWriter struct {
	mediaSession *media.MediaSession
	dtmfWriter   *media.RTPDtmfWriter
}

func (m *DialogMedia) AudioWriterDTMF() (*DTMFWriter, error) {
	aw, err := m.AudioWriter()
	if err != nil {
		return nil, err
	}

	return &DTMFWriter{
		dtmfWriter:   media.NewRTPDTMFWriter(dtmfCodec(m.mediaSession), m.RTPPacketWriter, aw),
		mediaSession: m.mediaSession,
	}, nil
}

func (w *DTMFWriter) WriteDTMF(dtmf rune) error {
	return w.dtmfWriter.WriteDTMF(dtmf)
}

// AudioReader exposes DTMF audio writer. You should use this for parallel audio processing
func (w *DTMFWriter) AudioWriter() *media.RTPDtmfWriter {
	return w.dtmfWriter
}

// Write exposes as io.Writer that can be used as AudioWriter
func (w *DTMFWriter) Write(buf []byte) (n int, err error) {
	return w.dtmfWriter.Write(buf)
}
