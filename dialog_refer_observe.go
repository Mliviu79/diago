// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// maxReferNotifies bounds how many NOTIFYs one REFER attempt records. A peer may
// send any number of NOTIFYs while the subscription lives, so the list is
// capped; the rest are counted in ReferObservation.NotifiesDropped and a
// terminal NOTIFY is always kept as the last entry.
const maxReferNotifies = 32

// ReferFailureError is returned, as *ReferFailureError, by ReferAndWait when a
// blind transfer does not complete successfully: either a terminal sipfrag
// NOTIFY carried a non-2xx status, or no final sipfrag arrived. It carries only
// the numeric SIP status and a short reason phrase — never Via/Contact/host or
// other routing internals — so a caller can classify the outcome (busy /
// unavailable / rejected) without risk of leaking SIP internals.
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

// ReferObserveOptions configures an observed REFER attempt. The attempt's
// lifetime is its refer subscription's (see ReferEnd), and a NOTIFY after that is
// reported through the session's OnReferNotifyUnmatched.
type ReferObserveOptions struct {
	// OnResponse, when set, is called once with the REFER's final response, 2xx or
	// not, as soon as it arrives and before the attempt waits on its subscription,
	// on the goroutine that sends the REFER. It is not called when the attempt ends
	// with no final response. It must not block: the attempt waits for it.
	OnResponse func(ReferResponse)
}

// referDialog is a dialog session with what sending a REFER and taking its
// NOTIFYs need of it beyond DialogSession. DialogServerSession and
// DialogClientSession are the two.
type referDialog interface {
	DialogSession
	// sipState returns the session's own SIP state, where the attempts of the
	// REFERs it sent are kept.
	sipState() *sessionState
	// referNotifyCallback returns the OnNotify callback a ReferOptions call
	// left on the dialog, or nil when it has none.
	referNotifyCallback() func(statusCode int)
}

// sipState returns s, so code that holds a session as an interface reaches the
// session's SIP state.
func (s *sessionState) sipState() *sessionState {
	return s
}

// referNotifyCallback returns the OnNotify callback a ReferOptions call left
// on the dialog, or nil when it has none.
func (d *dialogCallbacks) referNotifyCallback() func(statusCode int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.onReferNotify
}

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
func dialogReferObserve(ctx context.Context, d referDialog, recipient, referTo, referredBy sip.Uri, opts ReferObserveOptions, headers ...sip.Header) (ReferObservation, *sip.Response, error) {
	if d.DialogSIP().LoadState() != sip.DialogStateConfirmed {
		return ReferObservation{}, nil, fmt.Errorf("can only be called on answered dialog")
	}
	if ctx.Err() != nil {
		return ReferObservation{End: ReferEndCancelled}, nil, nil
	}

	// Register the attempt BEFORE sending, so a NOTIFY that races ahead of the
	// REFER's response is recorded on it rather than lost.
	s := d.sipState()
	attempt := s.beginReferAttempt()

	req, res, err := dialogReferSend(ctx, d, recipient, referTo, referredBy, headers)
	// The dialog assigns the CSeq during Do. Record it even when Do failed:
	// the dialog's highest REFER CSeq keeps a NOTIFY for this REFER from being
	// given to a later attempt.
	if req != nil {
		if cseq := req.CSeq(); cseq != nil {
			s.setReferAttemptCSeq(attempt, cseq.SeqNo)
		}
	}
	if err != nil {
		if req != nil && ctx.Err() != nil {
			// The context ended before the REFER's final response: the caller
			// gave the attempt up.
			return s.finishReferAttempt(attempt, ReferEndCancelled), nil, nil
		}
		s.dropReferAttempt(attempt)
		if req == nil {
			// The REFER was never built; the error is this package's own text.
			return ReferObservation{}, nil, err
		}
		return ReferObservation{}, nil, &referCarryError{cause: err}
	}

	response := ReferResponse{Status: res.StatusCode, Reason: res.Reason}
	if opts.OnResponse != nil {
		opts.OnResponse(response)
	}
	if !referAccepted(res.StatusCode) {
		obs := s.finishReferAttempt(attempt, ReferEndRefused)
		obs.Response, obs.ResponseReceived = response, true
		return obs, res, nil
	}

	// The attempt ends on its own, by a NOTIFY or its subscription's timer,
	// which have recorded the end before done closes, and finishing with the
	// zero end keeps it; or the caller gives it up.
	s.startReferSubscription(attempt)
	var end ReferEnd
	select {
	case <-attempt.done:
	case <-ctx.Done():
		end = ReferEndCancelled
	}
	obs := s.finishReferAttempt(attempt, end)
	obs.Response, obs.ResponseReceived = response, true
	return obs, res, nil
}

// dialogReferWait sends a REFER and waits, for as long as its refer
// subscription lives or until ctx ends, for the final sipfrag NOTIFY, and
// reports the real transfer outcome rather than the REFER's acceptance: nil
// for a final status below 300, *ReferFailureError for any other outcome, and
// sipgo.ErrDialogResponse for a REFER that was refused. It never ends the
// dialog.
func dialogReferWait(ctx context.Context, d referDialog, recipient sip.Uri, referTo sip.Uri, referredBy sip.Uri, headers ...sip.Header) error {
	obs, res, err := dialogReferObserve(ctx, d, recipient, referTo, referredBy, ReferObserveOptions{}, headers...)
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

// dialogReferDo sends req, the REFER of a Refer or ReferOptions call, which
// return once it is accepted and leave its outcome to the dialog.
//
// An attempt is kept for the REFER until a NOTIFY ends its refer subscription
// (see referAttempt.unobserved), so a NOTIFY after that one is answered 481
// (RFC 6665 section 4.1.3). Without the OnNotify callback of ReferOptions a
// NOTIFY is answered 200 only for a REFER the dialog sent, and it is that
// attempt's final NOTIFY that hangs the call up; with the callback, the
// callback takes the NOTIFYs.
func dialogReferDo(ctx context.Context, d DialogSession, req *sip.Request) (*sip.Response, error) {
	rd, ok := d.(referDialog)
	if !ok {
		return d.Do(ctx, req)
	}

	// Registered before the REFER is sent, so a NOTIFY that races ahead of the
	// REFER's response finds it.
	s := rd.sipState()
	attempt := s.beginUnobservedReferAttempt()
	res, err := d.Do(ctx, req)
	if cseq := req.CSeq(); cseq != nil {
		s.setReferAttemptCSeq(attempt, cseq.SeqNo)
	}
	if err != nil || !referAccepted(res.StatusCode) {
		// A REFER that was not accepted created no subscription.
		s.dropReferAttempt(attempt)
	}
	return res, err
}

// referAccepted reports whether a REFER's final response accepts it. Any 2xx
// does: RFC 7647 section 5 has a conforming peer answer 200, and a 202 is
// treated as a 200.
func referAccepted(status int) bool {
	return status >= 200 && status <= 299
}

// ReferAndWait sends a blind REFER for referTo, as Refer does, and blocks until
// the transfer outcome is known: the terminal sipfrag NOTIFY, or the end of the
// refer subscription (see ReferEnd), or of ctx, without one. It returns nil
// when the transfer succeeded, *ReferFailureError when it did not, and
// sipgo.ErrDialogResponse when the REFER itself was refused.
//
// Unlike Refer it never ends the dialog, so what follows the outcome is the
// caller's decision, and it installs no callback on the dialog.
func (d *DialogClientSession) ReferAndWait(ctx context.Context, referTo sip.Uri, headers ...sip.Header) error {
	// Referred-By carries the URI ReferOptions puts there.
	return dialogReferWait(ctx, d, d.remoteTarget(), referTo, d.ackTarget(), headers...)
}

// ReferAndWait sends a blind REFER for referTo, as Refer does, and blocks until
// the transfer outcome is known: the terminal sipfrag NOTIFY, or the end of the
// refer subscription (see ReferEnd), or of ctx, without one. It returns nil
// when the transfer succeeded, *ReferFailureError when it did not, and
// sipgo.ErrDialogResponse when the REFER itself was refused.
//
// Unlike Refer it never ends the dialog, so what follows the outcome is the
// caller's decision, and it installs no callback on the dialog.
func (d *DialogServerSession) ReferAndWait(ctx context.Context, referTo sip.Uri, headers ...sip.Header) error {
	cont := d.RemoteContact()
	return dialogReferWait(ctx, d, cont.Address, referTo, d.InviteResponse.Contact().Address, headers...)
}

// ReferAndObserve sends a blind REFER for referTo and returns what the far end
// sent back: the REFER's final response, the status line of each NOTIFY and how
// the attempt ended. The attempt lives as long as the refer subscription the
// REFER created, at most 64×T1 plus one hour from the REFER's 2xx (see ReferEnd),
// past a BYE on the dialog, or until ctx ends; a NOTIFY after that is answered 481
// and reported through OnReferNotifyUnmatched. Neither this method nor any NOTIFY
// of its attempt ends the dialog, so what follows the outcome is the caller's
// decision. Referred-By names this side: the Contact of the INVITE we sent. It
// installs no callback on the dialog, so it does not change what a later Refer or
// ReferOptions call reports.
func (d *DialogClientSession) ReferAndObserve(ctx context.Context, referTo sip.Uri, opts ReferObserveOptions) (ReferObservation, error) {
	ourContact := d.InviteRequest.Contact()
	if ourContact == nil {
		return ReferObservation{}, fmt.Errorf("refer: dialog has no local contact")
	}
	obs, _, err := dialogReferObserve(ctx, d, d.remoteTarget(), referTo, ourContact.Address, opts)
	return obs, err
}

// ReferAndObserve sends a blind REFER for referTo and returns what the far end
// sent back: the REFER's final response, the status line of each NOTIFY and how
// the attempt ended. The attempt lives as long as the refer subscription the
// REFER created, at most 64×T1 plus one hour from the REFER's 2xx (see ReferEnd),
// past a BYE on the dialog, or until ctx ends; a NOTIFY after that is answered 481
// and reported through OnReferNotifyUnmatched. Neither this method nor any NOTIFY
// of its attempt ends the dialog, so what follows the outcome is the caller's
// decision. Referred-By names this side: the Contact of the 200 OK we answered
// with. It installs no callback on the dialog, so it does not change what a later
// Refer or ReferOptions call reports.
func (d *DialogServerSession) ReferAndObserve(ctx context.Context, referTo sip.Uri, opts ReferObserveOptions) (ReferObservation, error) {
	recipient := d.RemoteContact()
	ourContact := d.InviteResponse.Contact()
	if recipient == nil {
		return ReferObservation{}, fmt.Errorf("refer: dialog has no remote contact")
	}
	if ourContact == nil {
		return ReferObservation{}, fmt.Errorf("refer: dialog has no local contact")
	}
	obs, _, err := dialogReferObserve(ctx, d, recipient.Address, referTo, ourContact.Address, opts)
	return obs, err
}
