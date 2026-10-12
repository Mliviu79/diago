// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type DialogSession interface {
	Id() string
	Context() context.Context
	Hangup(ctx context.Context) error
	Media() *DialogMedia
	DialogSIP() *sipgo.Dialog
	Do(ctx context.Context, req *sip.Request) (*sip.Response, error)
	Close() error
}

// maxReferNotifies bounds how many NOTIFYs one REFER attempt records. A peer may
// send any number of NOTIFYs while the subscription lives, so the list is
// capped; the rest are counted in ReferObservation.NotifiesDropped and a
// terminal NOTIFY is always kept as the last entry.
const maxReferNotifies = 32

// ReferFailureError is returned by a waiting Refer when a blind transfer does not
// complete successfully: either a terminal sipfrag NOTIFY carried a non-2xx
// status, or no final sipfrag arrived. It carries only the numeric SIP status and
// a short reason phrase — never Via/Contact/host or other routing internals — so
// a caller can classify the outcome (busy / unavailable / rejected) without risk
// of leaking SIP internals.
type ReferFailureError struct {
	// Status is the SIP status from the terminal sipfrag (e.g. 486). It is 0 when
	// no final sipfrag was received: the refer subscription's own timer ended
	// it, the context was cancelled, or the notifier ended the subscription
	// without one. Reason says which.
	Status int
	// Reason is a short, non-sensitive phrase from the sipfrag status line
	// (e.g. "Busy Here"), or "timeout", "cancelled" or "subscription ended" when
	// Status is 0. Classify on Status, not this.
	Reason string
}

// Error renders the failure without any SIP routing internals.
func (e *ReferFailureError) Error() string {
	if e.Status == 0 {
		return fmt.Sprintf("refer transfer failed: %s", e.Reason)
	}
	return fmt.Sprintf("refer transfer failed: %d %s", e.Status, e.Reason)
}

// referCarryError is what the observing REFER returns when the REFER could not
// be carried. sipgo builds that error from the REFER's request line, which is
// the far end's Contact URI with its user part, and from socket addresses, so
// its text is dropped and only its class is kept.
//
// The type has no unwrapping method and implements neither fmt.Formatter nor an
// error-list method: errors.As, %+v and structured loggers reach Error() only.
type referCarryError struct {
	cause error
}

// Error renders fixed text, followed by the transaction or context class the
// failure wrapped when it is one this package names. It never renders the
// cause's own text.
func (e *referCarryError) Error() string {
	class := referCarryClass(e.cause)
	if class == nil {
		return "refer: the REFER could not be carried"
	}
	return "refer: the REFER could not be carried: " + class.Error()
}

// Is matches the sipgo transaction sentinel or context error the failure
// wrapped, including classes this package does not name.
func (e *referCarryError) Is(target error) bool {
	return errors.Is(e.cause, target)
}

// referCarryClass returns the class a carry error names in its text: the first
// sipgo transaction sentinel or context error err wraps, or nil. It names only
// the sentinels every supported sipgo defines.
func referCarryClass(err error) error {
	switch {
	case errors.Is(err, sip.ErrTransactionTimeout):
		return sip.ErrTransactionTimeout
	case errors.Is(err, sip.ErrTransactionTransport):
		return sip.ErrTransactionTransport
	case errors.Is(err, sip.ErrTransactionCanceled):
		return sip.ErrTransactionCanceled
	case errors.Is(err, sip.ErrTransactionTerminated):
		return sip.ErrTransactionTerminated
	case errors.Is(err, context.DeadlineExceeded):
		return context.DeadlineExceeded
	case errors.Is(err, context.Canceled):
		return context.Canceled
	}
	return nil
}

// ReferEnd is how an observed REFER attempt ended. Its zero value is not a kind,
// and no kind is a SIP status.
//
// An attempt lives as long as the refer subscription its REFER created (RFC 3515
// section 2.4.4; RFC 6665 section 4.1.3): from the REFER's 2xx, or a NOTIFY that
// raced ahead of it, to the first of a NOTIFY carrying a final status, a NOTIFY
// terminating the subscription, the subscription's deadline, or the caller
// giving the attempt up. Before the first NOTIFY the deadline is 64×T1 from the
// 2xx. The first NOTIFY that does not end the subscription sets it to the
// expires that NOTIFY gave, counted from the NOTIFY's arrival and held to at most
// one hour (a local limit, see referSubscriptionMaxBound). A NOTIFY that gives no
// expires this package can read sets it to 64×T1 from that NOTIFY's arrival
// instead. A later NOTIFY can only bring the deadline earlier, never later (RFC
// 6665 section 4.2.2). So the attempt ends at most 64×T1 plus one hour after the
// 2xx. A refused REFER creates none. A BYE on the dialog ends none of these (RFC
// 5057 section 5.5), and no deadline of the caller's applies.
type ReferEnd int

const (
	// ReferEndFinalNotify means a NOTIFY carried a final sipfrag status of 200 or
	// above. That NOTIFY is the last entry of ReferObservation.Notifies.
	ReferEndFinalNotify ReferEnd = iota + 1
	// ReferEndRefused means the REFER's final response was not 2xx.
	// ReferObservation.Response holds it.
	ReferEndRefused
	// ReferEndCancelled means the caller's context ended, and the attempt was
	// given up then: a NOTIFY for it afterwards is answered 481. The REFER's
	// response may be absent (ReferObservation.ResponseReceived is false).
	ReferEndCancelled
	// ReferEndSubscriptionEnded means a NOTIFY with Subscription-State terminated
	// and a 1xx sipfrag ended the implicit subscription without a final status.
	ReferEndSubscriptionEnded
	// ReferEndSubscriptionExpired means the subscription's deadline passed with
	// no NOTIFY ending it. The first NOTIFY that does not end the subscription
	// sets the deadline, whatever its state token: to the expires it gave (RFC
	// 6665 sections 4.1.3 and 4.2.1.4), at most one hour from that NOTIFY's
	// arrival. A NOTIFY that gave no expires this package can read counts 64×T1
	// from its arrival instead. A later NOTIFY can only bring the deadline
	// earlier.
	ReferEndSubscriptionExpired
	// ReferEndNoNotify means the REFER was accepted and no NOTIFY arrived within
	// Timer N, 64×T1 from its 2xx (RFC 6665 section 4.1.2.4).
	ReferEndNoNotify
)

// String returns the kind as a lowercase slug, or ReferEnd(<n>) for a value that
// is not a kind.
func (e ReferEnd) String() string {
	switch e {
	case ReferEndFinalNotify:
		return "final_notify"
	case ReferEndRefused:
		return "refused"
	case ReferEndCancelled:
		return "cancelled"
	case ReferEndSubscriptionEnded:
		return "subscription_ended"
	case ReferEndSubscriptionExpired:
		return "subscription_expired"
	case ReferEndNoNotify:
		return "no_notify"
	}
	return fmt.Sprintf("ReferEnd(%d)", int(e))
}

// ReferResponse is the REFER's final response: its status code and reason
// phrase. Like every REFER observation type it carries only status codes, reason
// phrases, subscription state and correlation, never Via, Contact, host or URI.
type ReferResponse struct {
	Status int
	Reason string
}

// ReferNotify is one NOTIFY received for a REFER attempt: the sipfrag status
// line, the Subscription-State header and the Event id. It carries only status
// codes, reason phrases, subscription state and correlation, never Via,
// Contact, host or URI.
type ReferNotify struct {
	// Status and Reason are the sipfrag status line, 1xx progress included.
	Status int
	Reason string
	// SubscriptionState is the Subscription-State header value exactly as
	// received. HasSubscriptionState is false when the header was absent.
	SubscriptionState    string
	HasSubscriptionState bool
	// EventID is the Event header's id parameter, which RFC 3515 makes the CSeq
	// of the REFER. HasEventID is false when the NOTIFY carried no usable id.
	EventID    uint32
	HasEventID bool
}

// isFinal reports whether the sipfrag carries a final status.
func (n ReferNotify) isFinal() bool {
	return n.Status >= 200
}

// endsSubscription reports whether the NOTIFY is terminal for the attempt: a
// final status, or a subscription the notifier terminated.
func (n ReferNotify) endsSubscription() bool {
	return n.isFinal() || (n.HasSubscriptionState && referSubscriptionTerminated(n.SubscriptionState))
}

// ReferObservation is what one REFER attempt observed: the REFER's final
// response, every NOTIFY in the order it was handled, the REFER's CSeq and how
// the attempt ended. It carries only status codes, reason phrases, subscription
// state and correlation, never Via, Contact, host or URI.
type ReferObservation struct {
	End ReferEnd
	// Response is the REFER's final response. ResponseReceived is false when the
	// context ended before one arrived.
	Response         ReferResponse
	ResponseReceived bool
	// CSeq is the REFER's CSeq, the value a NOTIFY's Event id is matched against.
	// CSeqKnown is false when it was never recorded.
	CSeq      uint32
	CSeqKnown bool
	// Notifies holds at most maxReferNotifies NOTIFYs, duplicates kept, with a
	// terminal NOTIFY always last. NotifiesDropped counts the ones not kept.
	Notifies        []ReferNotify
	NotifiesDropped int
}

// LastNotify returns the last recorded NOTIFY and true, or the zero value and
// false when none was recorded.
func (o ReferObservation) LastNotify() (ReferNotify, bool) {
	if len(o.Notifies) == 0 {
		return ReferNotify{}, false
	}
	return o.Notifies[len(o.Notifies)-1], true
}

// ReferObserveOptions configures an observed REFER attempt. It has no field:
// the attempt's lifetime is its refer subscription's (see ReferEnd), and a
// NOTIFY after that is reported through DialogMedia.OnReferNotifyUnmatched.
type ReferObserveOptions struct{}

// parseSipfragStatus parses the status code and reason phrase from a
// message/sipfrag status line of the form "SIP/2.0 <code> <reason>" (the shape
// sendNotify writes). Only the first line is the status line. It returns
// ok=false when the line is malformed or the code is not a valid SIP status, so
// an untrusted NOTIFY body can never be mistaken for a terminal outcome.
func parseSipfragStatus(frag string) (status int, reason string, ok bool) {
	line := frag
	if i := strings.IndexAny(frag, "\r\n"); i >= 0 {
		line = frag[:i]
	}
	const prefix = "SIP/2.0 "
	if !strings.HasPrefix(line, prefix) {
		return 0, "", false
	}
	rest := strings.TrimSpace(line[len(prefix):])
	codeStr := rest
	if sp := strings.IndexByte(rest, ' '); sp >= 0 {
		codeStr = rest[:sp]
		reason = strings.TrimSpace(rest[sp+1:])
	}
	code, err := strconv.Atoi(codeStr)
	if err != nil || code < 100 || code > 699 {
		return 0, "", false
	}
	return code, reason, true
}

// parseReferNotifyEventID extracts the RFC 3515 subscription id from a REFER
// NOTIFY's Event header value of the form "refer;id=<n>". sipgo exposes no typed
// Event header, so the generic value is parsed here. Peers MAY omit ;id, so
// hasID=false is a normal case.
func parseReferNotifyEventID(req *sip.Request) (id uint32, hasID bool) {
	h := req.GetHeader("Event")
	if h == nil {
		return 0, false
	}
	// The value is "<event-type>[;param=value]*"; scan the params for id=.
	for _, part := range strings.Split(h.Value(), ";") {
		part = strings.TrimSpace(part)
		const idPrefix = "id="
		if !strings.HasPrefix(part, idPrefix) {
			continue
		}
		n, err := strconv.ParseUint(strings.TrimSpace(part[len(idPrefix):]), 10, 32)
		if err != nil {
			return 0, false
		}
		return uint32(n), true
	}
	return 0, false
}

// parseReferSubscriptionState returns the Subscription-State header value of a
// REFER NOTIFY exactly as received, and whether the header was present.
func parseReferSubscriptionState(req *sip.Request) (value string, present bool) {
	h := req.GetHeader("Subscription-State")
	if h == nil {
		return "", false
	}
	return h.Value(), true
}

// referSubscriptionTerminated reports whether a Subscription-State value is the
// terminated state: the token before the first ";", trimmed, compared
// case-insensitively.
func referSubscriptionTerminated(value string) bool {
	state, _, _ := strings.Cut(value, ";")
	return strings.EqualFold(strings.TrimSpace(state), "terminated")
}

// referSubscriptionMaxBound is the longest a refer subscription may run after
// the first NOTIFY that does not end it. It is our local limit, not a value RFC
// 6665 sets: the notifier owns the duration it sends (section 4.1.3), but a
// duration longer than this is cut to it and ended on our side, the same way an
// expired deadline ends the attempt. Because a later NOTIFY can only shorten the
// deadline (section 4.2.2), the subscription ends at most this long after its
// first NOTIFY, however many NOTIFYs follow. A REFER subscription only reports
// transfer progress, and a final NOTIFY ends it, so the limit is reached only
// when no final NOTIFY comes. A notifier that keeps the subscription alive for up
// to an hour is not cut short.
const referSubscriptionMaxBound = time.Hour

// referSubscriptionBound is the duration a NOTIFY that did not end the
// subscription gives it, counted from that NOTIFY's arrival. The expires
// parameter of its Subscription-State is "the authoritative subscription
// duration" (RFC 6665 section 4.1.3), so it is used as given, up to
// referSubscriptionMaxBound. recordReferNotify turns this into the attempt's
// deadline: the first such NOTIFY sets the deadline to it, replacing the 64×T1
// wait for a first NOTIFY (section 4.1.2.4), and a later one can only bring the
// deadline earlier (section 4.2.2). A NOTIFY that is active or pending and
// carries no expires this package can read breaks section 4.2.2's MUST that the
// expires be present; one with no Subscription-State at all breaks section
// 4.1.3's MUST that NOTIFY carry it. Either gives 64×T1 from its arrival, so no
// attempt lives unbounded.
func referSubscriptionBound(n ReferNotify) time.Duration {
	if n.HasSubscriptionState {
		if seconds, ok := referSubscriptionExpires(n.SubscriptionState); ok {
			// A uint32 count of seconds fits a Duration, so the product cannot overflow.
			return min(time.Duration(seconds)*time.Second, referSubscriptionMaxBound)
		}
	}
	return 64 * sip.T1
}

// referSubscriptionExpires returns the expires parameter of a Subscription-State
// value in seconds, and whether it carried one that parses as delta-seconds.
func referSubscriptionExpires(value string) (uint32, bool) {
	_, params, _ := strings.Cut(value, ";")
	for _, param := range strings.Split(params, ";") {
		name, v, ok := strings.Cut(param, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(name), "expires") {
			continue
		}
		seconds, err := strconv.ParseUint(strings.TrimSpace(v), 10, 32)
		if err != nil {
			return 0, false
		}
		return uint32(seconds), true
	}
	return 0, false
}

//
// Here are many common functions built for dialog
//

// dialogReferSend builds a REFER to recipient carrying Refer-To and Referred-By
// and sends it within the dialog. The request is returned whenever it was sent,
// so its CSeq can be read even when Do fails; it is nil when the REFER could not
// be built.
func dialogReferSend(ctx context.Context, d DialogSession, recipient, referTo, referredBy sip.Uri, headers []sip.Header) (*sip.Request, *sip.Response, error) {
	req := sip.NewRequest(sip.REFER, recipient)
	// Invite request tags must be preserved but switched
	req.AppendHeader(sip.NewHeader("Refer-To", uri2Header(referTo)))
	req.AppendHeader(sip.NewHeader("Referred-By", uri2Header(referredBy)))
	for _, h := range headers {
		if h == nil {
			return nil, nil, fmt.Errorf("refer header is nil")
		}

		switch h.Name() {
		// Avoid duplicates
		case "Refer-To", "refer-to", "Referred-By", "reffered-by":
			req.ReplaceHeader(h)
			continue
		default:
			req.AppendHeader(h)
		}
	}

	res, err := d.Do(ctx, req)
	return req, res, err
}

// dialogReferObserve sends a REFER and observes it for as long as its refer
// subscription lives (see ReferEnd), or until the caller's context ends, which
// gives the attempt up. It never ends the dialog. The returned response is the
// REFER's final response when one arrived.
//
// A context that ends before the REFER's final response is reported as
// ReferEndCancelled with a nil error. The error return is for a REFER that could
// not be carried for any other reason: a dialog that is not confirmed, a bad
// header or a transport failure. A transport failure comes back as an error that
// keeps its class for errors.Is and none of sipgo's text, which carries the
// REFER's request line and socket addresses.
func dialogReferObserve(ctx context.Context, d DialogSession, recipient, referTo, referredBy sip.Uri, _ ReferObserveOptions, headers ...sip.Header) (ReferObservation, *sip.Response, error) {
	if d.DialogSIP().LoadState() != sip.DialogStateConfirmed {
		return ReferObservation{}, nil, fmt.Errorf("can only be called on answered dialog")
	}
	if ctx.Err() != nil {
		return ReferObservation{End: ReferEndCancelled}, nil, nil
	}

	// Register the attempt BEFORE sending, so a NOTIFY that races ahead of the
	// REFER's response is recorded on it rather than lost.
	med := d.Media()
	attempt := med.beginReferAttempt()

	req, res, err := dialogReferSend(ctx, d, recipient, referTo, referredBy, headers)
	// The dialog assigns the CSeq during Do. Record it even when Do failed:
	// the dialog's highest REFER CSeq keeps a NOTIFY for this REFER from being
	// given to a later attempt.
	if req != nil {
		if cseq := req.CSeq(); cseq != nil {
			med.setReferAttemptCSeq(attempt, cseq.SeqNo)
		}
	}
	if err != nil {
		if req != nil && ctx.Err() != nil {
			// The context ended before the REFER's final response: the caller
			// gave the attempt up.
			return med.finishReferAttempt(attempt, ReferEndCancelled), nil, nil
		}
		med.dropReferAttempt(attempt)
		if req == nil {
			// The REFER was never built; the error is this package's own text.
			return ReferObservation{}, nil, err
		}
		return ReferObservation{}, nil, &referCarryError{cause: err}
	}

	response := ReferResponse{Status: res.StatusCode, Reason: res.Reason}
	if !referAccepted(res.StatusCode) {
		obs := med.finishReferAttempt(attempt, ReferEndRefused)
		obs.Response, obs.ResponseReceived = response, true
		return obs, res, nil
	}

	// The attempt ends on its own, by a NOTIFY or its subscription's timer,
	// which have recorded the end before done closes, and finishing with the
	// zero end keeps it; or the caller gives it up.
	med.startReferSubscription(attempt)
	var end ReferEnd
	select {
	case <-attempt.done:
	case <-ctx.Done():
		end = ReferEndCancelled
	}
	obs := med.finishReferAttempt(attempt, end)
	obs.Response, obs.ResponseReceived = response, true
	return obs, res, nil
}

// dialogRefer sends a REFER and returns once it is accepted. When wait is true it
// additionally waits, for as long as the refer subscription lives or until ctx
// ends, for the final sipfrag NOTIFY and reports the real transfer outcome as a
// *ReferFailureError, rather than reporting the acceptance as success. Callers
// that supply an OnNotify callback observe the outcome asynchronously instead
// and pass wait=false.
func dialogRefer(ctx context.Context, d DialogSession, recipient sip.Uri, referTo sip.Uri, refferedBy sip.Uri, wait bool, headers ...sip.Header) error {
	if !wait {
		if d.DialogSIP().LoadState() != sip.DialogStateConfirmed {
			return fmt.Errorf("can only be called on answered dialog")
		}
		_, res, err := dialogReferSend(ctx, d, recipient, referTo, refferedBy, headers)
		if err != nil {
			return err
		}
		if !referAccepted(res.StatusCode) {
			return sipgo.ErrDialogResponse{Res: res}
		}
		return nil
	}

	obs, res, err := dialogReferObserve(ctx, d, recipient, referTo, refferedBy, ReferObserveOptions{}, headers...)
	if err != nil {
		return err
	}
	switch obs.End {
	case ReferEndRefused:
		return sipgo.ErrDialogResponse{Res: res}
	case ReferEndFinalNotify:
		last, _ := obs.LastNotify()
		if last.Status < 300 {
			return nil
		}
		return &ReferFailureError{Status: last.Status, Reason: last.Reason}
	case ReferEndSubscriptionExpired, ReferEndNoNotify:
		return &ReferFailureError{Status: 0, Reason: "timeout"}
	case ReferEndCancelled:
		return &ReferFailureError{Status: 0, Reason: "cancelled"}
	case ReferEndSubscriptionEnded:
		return &ReferFailureError{Status: 0, Reason: "subscription ended"}
	}
	return fmt.Errorf("refer ended as %s", obs.End)
}

// referAccepted reports whether a REFER's final response accepts it. Any 2xx
// does: RFC 7647 section 5 has a conforming peer answer 200, and a 202 is
// treated as a 200.
func referAccepted(status int) bool {
	return status >= 200 && status <= 299
}

// readByeOnce hands the peer's BYE to readBye, the ReadBye of an embedded
// sipgo session, and answers it 500 when readBye refused it as out of order
// (RFC 3261 section 12.2.2) without answering it. Some sipgo versions answer
// such a BYE themselves and some leave the answer to their caller; the BYE is
// answered once either way.
func readByeOnce(req *sip.Request, tx sip.ServerTransaction, readBye func(*sip.Request, sip.ServerTransaction) error) error {
	rtx := &respondRecordingTx{ServerTransaction: tx}
	err := readBye(req, rtx)
	if errors.Is(err, sipgo.ErrDialogInvalidCseq) && !rtx.responded.Load() {
		res := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
		return errors.Join(err, tx.Respond(res))
	}
	return err
}

// respondRecordingTx is a server transaction that records whether a response
// was passed to it.
type respondRecordingTx struct {
	sip.ServerTransaction
	responded atomic.Bool
}

func (tx *respondRecordingTx) Respond(res *sip.Response) error {
	tx.responded.Store(true)
	return tx.ServerTransaction.Respond(res)
}

// respondDialogEnded answers req 481 when d has ended, and reports whether it
// did. An ended dialog stays in the dialog cache until its call handler
// returns, for an inbound call, or until it is closed, for an outbound one, so
// an in-dialog request can still find it. It matches no live dialog, and RFC
// 3261 section 12.2.2 has such a request answered 481.
func respondDialogEnded(d DialogSession, req *sip.Request, tx sip.ServerTransaction) (bool, error) {
	if d.DialogSIP().LoadState() != sip.DialogStateEnded {
		return false, nil
	}
	res := sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)
	return true, tx.Respond(res)
}

// parseReferNotify reads a refer NOTIFY: its message/sipfrag status line, its
// Subscription-State and its Event id. ok is false when the body is not a
// sipfrag carrying a valid status line, so an untrusted body is never mistaken
// for an outcome.
func parseReferNotify(req *sip.Request) (n ReferNotify, ok bool) {
	// TODO how to know this is refer
	contentType := req.ContentType()
	// For now very basic check
	if contentType == nil || !strings.HasPrefix(contentType.Value(), "message/sipfrag") {
		return ReferNotify{}, false
	}
	frag := string(req.Body())
	if len(frag) < len("SIP/2.0 100 xx") {
		return ReferNotify{}, false
	}
	n.Status, n.Reason, ok = parseSipfragStatus(frag)
	if !ok {
		return ReferNotify{}, false
	}
	n.SubscriptionState, n.HasSubscriptionState = parseReferSubscriptionState(req)
	n.EventID, n.HasEventID = parseReferNotifyEventID(req)
	return n, true
}

// respondNotifyDialogEnded answers a NOTIFY as respondDialogEnded does, unless
// an open REFER attempt of d holds it, and reports whether it did. A BYE ends
// the INVITE usage of a dialog, not the subscription usage a REFER created, and
// the dialog lives on until its last usage ends (RFC 5057 sections 2, 4.1 and
// 5.5), so a transfer outcome is still taken after the call has ended. A refer
// NOTIFY answered here is one no subscription holds, and is reported to the
// function DialogMedia.OnReferNotifyUnmatched set.
func respondNotifyDialogEnded(d DialogSession, req *sip.Request, tx sip.ServerTransaction) bool {
	med := d.Media()
	if d.DialogSIP().LoadState() != sip.DialogStateEnded || med.referNotifyExpected(req) {
		return false
	}
	ended, _ := respondDialogEnded(d, req, tx)
	if n, parsed := parseReferNotify(req); ended && parsed {
		med.reportUnmatchedReferNotify(n)
	}
	return ended
}

// dialogHandleReferNotify answers a refer NOTIFY and routes it. A NOTIFY that
// an open REFER attempt of d holds is recorded on it and answered 200, and one
// that ends the attempt retires it after that 200 (RFC 6665 section 4.4.1). A
// NOTIFY no open attempt holds matches none of d's subscriptions: it is answered
// 481 (section 4.1.3), never applied, and reported to the function
// DialogMedia.OnReferNotifyUnmatched set. A body that is not a sipfrag status
// line is answered 400. The dialog is left to its owner: what follows a
// transfer result, success or failure, is the caller's decision.
func dialogHandleReferNotify(d DialogSession, req *sip.Request, tx sip.ServerTransaction) {
	n, parsed := parseReferNotify(req)
	if !parsed {
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}

	med := d.Media()
	attempt := med.recordReferNotify(n)
	// TODO: We need find better way to store this on refer.
	// best case this would be dialogSIP
	med.mu.Lock()
	onNot := med.onReferNotify
	med.mu.Unlock()
	if attempt == nil && onNot == nil {
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Subscription Does Not Exist", nil))
		med.reportUnmatchedReferNotify(n)
		return
	}

	tx.Respond(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	if attempt != nil {
		med.retireEndedReferAttempt(attempt)
	}
	if onNot != nil {
		onNot(n.Status)
	}
}

func dialogHandleRefer(d DialogSession, dg *Diago, req *sip.Request, tx sip.ServerTransaction, onReferDialog OnReferDialogFunc) error {
	// https://datatracker.ietf.org/doc/html/rfc3515#section-2.4.2
	// 	An agent responding to a REFER method MUST return a 400 (Bad Request)
	//    if the request contained zero or more than one Refer-To header field
	//    values.
	log := dg.log
	referTo := req.GetHeader("Refer-To")
	if referTo == nil {
		log.Info("Received REFER without Refer-To header")
		return tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
	}

	referToUri := sip.Uri{}
	headerParams := sip.NewParams()

	_, err := sip.ParseAddressValue(referTo.Value(), &referToUri, &headerParams)
	if err != nil {
		log.Info("Received REFER but failed to parse Refer-To uri", "error", err)
		return tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
	}

	contact := req.Contact()
	if contact == nil {
		log.Info("Received REFER but no Contact Header present")
		return tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", []byte("No Contact Header")))
	}

	// TODO can we locate this more checks
	log.Info("Accepting refer")
	if err := tx.Respond(sip.NewResponseFromRequest(req, 202, "Accepted", nil)); err != nil {
		return fmt.Errorf("failed to send 202 Accepted")
	}

	// This transcation can now terminate?
	return dialogReferInvite(d, dg, referToUri, contact.Address, onReferDialog, req)
}

func dialogReferInvite(d DialogSession, dg *Diago, referToUri sip.Uri, remoteTarget sip.Uri, onReferDialog OnReferDialogFunc, referReq *sip.Request) error {

	// TODO after this we could get BYE immediately, but caller would not be able
	// to take control over refer dialog

	// REFER State reasons summary
	/* | Reason | Meaning | Should Retry? | Automatic Retry? |
	   |--------|---------|---------------|------------------|
	   | **noresource** | Success or final failure | No (completed) | No |
	   | **rejected** | Policy denied | Maybe later | No |
	   | **deactivated** | Feature disabled | Yes, after retry-after | No |
	   | **probation** | Temporary failure | Yes, after retry-after | No |
	   | **timeout** | Request timed out | Maybe | No |
	   | **giveup** | Referee gave up | Maybe | No |	 */

	type subState struct {
		state      string
		reason     string
		expires    int
		retryAfter int
	}

	// TODO: Multiple Refers require having ID
	referID := ""

	sendNotify := func(ctx context.Context, statusCode int, reason string, sub subState) error {
		req := sip.NewRequest(sip.NOTIFY, remoteTarget)

		referParams := sip.NewParams()
		referParams.Add("refer", "")
		if referID != "" {
			referParams.Add("id", referID)
		}

		stateParams := sip.NewParams()
		stateParams.Add(sub.state, "")
		if sub.reason != "" {
			stateParams.Add("reason", sub.reason)
		}
		if sub.retryAfter > 0 {
			stateParams.Add("retry-after", strconv.Itoa(sub.retryAfter))
		}

		if sub.expires > 0 {
			stateParams.Add("expires", strconv.Itoa(sub.expires))
		}

		req.AppendHeader(sip.NewHeader("Event", referParams.ToString(';')))
		req.AppendHeader(sip.NewHeader("Subscription-State", stateParams.ToString(';')))
		req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
		frag := fmt.Sprintf("SIP/2.0 %d %s", statusCode, reason)
		req.SetBody([]byte(frag))

		res, err := d.Do(ctx, req)
		if err != nil {
			return err
		}
		if res.StatusCode != 200 {
			return fmt.Errorf("notify received non 200 response. code=%d", res.StatusCode)
		}
		return nil
	}

	ctx := d.Context()

	// TODO  Mutliple Refers require IDs in NOTIFY event header
	// https://datatracker.ietf.org/doc/html/rfc3515#section-2.4.6

	// FROM, TO, CALLID must be same to make SUBSCRIBE working

	// onReferRequest(referToUri, )

	// Check is this REFER RFC 3892 compatible
	// https://datatracker.ietf.org/doc/html/rfc3892#autoid-3
	// if referredBy != nil {
	// 	opts.Headers = append(opts.Headers, sip.HeaderClone(referredBy))
	// }

	referDialog, err := dg.NewDialog(referToUri, NewDialogOptions{})
	if err != nil {
		return err
	}
	defer referDialog.Close()

	if h := referReq.GetHeader("referred-by"); h != nil {
		referDialog.InviteRequest.AppendHeader(sip.HeaderClone(h))
	}
	if h := referReq.GetHeader("replaces"); h != nil {
		referDialog.InviteRequest.AppendHeader(sip.HeaderClone(h))
	}

	// 	The final NOTIFY sent in response to a REFER MUST indicate
	//    the subscription has been "terminated" with a reason of "noresource".
	//    (The resource being subscribed to is the state of the referenced
	//    request).
	referDialog.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateConfirmed || s == sip.DialogStateEnded {
			if err := sendNotify(ctx, 200, "OK", subState{
				state:  "terminated",
				reason: "noresource",
			}); err != nil {
				dg.log.Info("REFER Notify failed for 200", "error", err)
			}
		}
	})

	if err := sendNotify(ctx, 100, "Trying", subState{state: "active", expires: 60}); err != nil {
		// log.Info("REFER NOTIFY 100 failed to sent", "error", err)
		return fmt.Errorf("refer NOTIFY 100 failed to sent : %w", err)
	}

	// We send ref dialog to processing. After sending 200 OK this session will terminate
	if err := onReferDialog(referDialog); err != nil {
		// DO notify?
		dg.log.Info("OnReferDialog handling failed with", "error", err)
		var resErr *sipgo.ErrDialogResponse
		if errors.As(err, &resErr) {
			return sendNotify(ctx, resErr.Res.StatusCode, resErr.Res.Reason, subState{
				state:  "terminated",
				reason: "noresource",
			})
		}

		// If call failed to be established but not yet confirmed
		state := referDialog.LoadState()
		if state == 0 || state == sip.DialogStateEstablished {
			return sendNotify(ctx, 400, "Bad Request", subState{
				state:  "terminated",
				reason: "noresource",
			})
		}
		return err
	}
	// Now this dialog will receive BYE and it will terminate
	// We need to send this referDialog to control of caller
	return nil
}

type OnReferTransactionFunc func(referTransaction ReferTransaction) error

type ReferTransaction struct {
	d            DialogSession
	Refer        *sip.Request
	referTo      sip.Uri
	tx           sip.ServerTransaction
	dg           *Diago
	remoteTarget sip.Uri
}

func (r *ReferTransaction) Accept(ctx context.Context, opts InviteClientOptions) (*DialogClientSession, error) {
	dg := r.dg
	req := r.Refer
	referToUri := r.referTo

	log := dg.log
	log.Info("Accepting refer")
	if err := r.tx.Respond(sip.NewResponseFromRequest(req, 202, "Accepted", nil)); err != nil {
		return nil, fmt.Errorf("failed to send 202 Accepted")
	}

	referDialog, err := dg.NewDialog(referToUri, NewDialogOptions{})
	if err != nil {
		return nil, err
	}
	// defer referDialog.Close()

	// 	The final NOTIFY sent in response to a REFER MUST indicate
	//    the subscription has been "terminated" with a reason of "noresource".
	//    (The resource being subscribed to is the state of the referenced
	//    request).

	err = func() error {
		if h := req.GetHeader("refered-by"); h != nil {
			opts.Headers = append(opts.Headers, sip.HeaderClone(h))
		}
		if h := req.GetHeader("replaces"); h != nil {
			opts.Headers = append(opts.Headers, sip.HeaderClone(h))
		}

		if err := referDialog.Invite(ctx, opts); err != nil {
			return err
		}

		dg.log.Info("OnReferDialog handling failed with", "error", err)
		var resErr *sipgo.ErrDialogResponse
		if errors.As(err, &resErr) {
			return r.sendNotify(ctx, resErr.Res.StatusCode, resErr.Res.Reason, subState{
				state:  "terminated",
				reason: "noresource",
			})
		}

		// If call failed to be established but not yet confirmed
		state := referDialog.LoadState()
		if state == 0 || state == sip.DialogStateEstablished {
			return r.sendNotify(ctx, 400, "Bad Request", subState{
				state:  "terminated",
				reason: "noresource",
			})
		}
		return r.sendNotify(ctx, 200, "OK", subState{
			state:  "terminated",
			reason: "noresource",
		})
	}()
	if err != nil {
		r.sendNotify(ctx, 200, "OK", subState{
			state:  "terminated",
			reason: "noresource",
		})

		referDialog.Close()
		return nil, err
	}
	return referDialog, nil
}

type subState struct {
	state      string
	reason     string
	expires    int
	retryAfter int
}

func (r *ReferTransaction) sendNotify(ctx context.Context, statusCode int, reason string, sub subState) error {
	req := sip.NewRequest(sip.NOTIFY, r.remoteTarget)

	referParams := sip.NewParams()
	referParams.Add("refer", "")

	// TODO: Multiple Refers require having ID
	referID := ""
	if referID != "" {
		referParams.Add("id", referID)
	}

	stateParams := sip.NewParams()
	stateParams.Add(sub.state, "")
	if sub.reason != "" {
		stateParams.Add("reason", sub.reason)
	}
	if sub.retryAfter > 0 {
		stateParams.Add("retry-after", strconv.Itoa(sub.retryAfter))
	}

	if sub.expires > 0 {
		stateParams.Add("expires", strconv.Itoa(sub.expires))
	}

	req.AppendHeader(sip.NewHeader("Event", referParams.ToString(';')))
	req.AppendHeader(sip.NewHeader("Subscription-State", stateParams.ToString(';')))
	req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
	frag := fmt.Sprintf("SIP/2.0 %d %s", statusCode, reason)
	req.SetBody([]byte(frag))

	res, err := r.d.Do(ctx, req)
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("notify received non 200 response. code=%d", res.StatusCode)
	}
	return nil
}

func dialogHandleReferTransaction(d DialogSession, dg *Diago, req *sip.Request, tx sip.ServerTransaction, onReferDialog OnReferTransactionFunc) error {
	log := dg.log
	referTo := req.GetHeader("Refer-To")
	if referTo == nil {
		log.Info("Received REFER without Refer-To header")
		return tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
	}

	referToUri := sip.Uri{}
	headerParams := sip.NewParams()

	_, err := sip.ParseAddressValue(referTo.Value(), &referToUri, &headerParams)
	if err != nil {
		log.Info("Received REFER but failed to parse Refer-To uri", "error", err)
		return tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", nil))
	}

	contact := req.Contact()
	if contact == nil {
		log.Info("Received REFER but no Contact Header present")
		return tx.Respond(sip.NewResponseFromRequest(req, 400, "Bad Request", []byte("No Contact Header")))
	}

	rtx := ReferTransaction{
		dg:           dg,
		tx:           tx,
		referTo:      referToUri,
		remoteTarget: contact.Address,
		Refer:        req,
	}
	return onReferDialog(rtx)
}
