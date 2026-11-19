// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/emiago/sipgo/sip"
)

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

// dialogRouteReferNotify answers a refer NOTIFY the peer sent on d and routes
// it. onReferNotify is the OnNotify callback a ReferOptions call left on d, nil
// when it has none.
//
// A NOTIFY belongs to a REFER d sent when an open REFER attempt of d holds it
// or d has the callback, which takes every refer NOTIFY of the dialog. Such a
// NOTIFY is recorded on its attempt, if one holds it, and handed to
// dialogHandleReferNotify, which answers it 200. After that 200 an attempt the
// NOTIFY ended is retired (RFC 6665 section 4.4.1), and then the sipfrag
// status goes to the callback. With no callback, a final status on an attempt
// nobody observes (referAttempt.unobserved) hangs the call up, and a NOTIFY of
// an attempt its caller observes leaves the dialog to its owner: what follows
// a transfer result is that caller's decision.
//
// Any other NOTIFY matches none of d's subscriptions: it is answered 481
// (RFC 6665 section 4.1.3), never applied, and reported to the function
// OnReferNotifyUnmatched set. That holds on an ended dialog as on a live one:
// a BYE ends the INVITE usage of a dialog, not the subscription usage a REFER
// created, and the dialog lives on until its last usage ends (RFC 5057
// sections 2, 4.1 and 5.5), so a transfer outcome is still taken after the
// call has ended, and only a NOTIFY nothing of d expects is answered as a
// request for an ended dialog is.
//
// A body that is not a sipfrag starting with a status line (RFC 3515 section
// 2.4.5) is answered 400 on a live dialog and is never an outcome.
func dialogRouteReferNotify(ctx context.Context, d referDialog, req *sip.Request, tx sip.ServerTransaction, onReferNotify func(statusCode int)) {
	s := d.sipState()
	n, parsed := parseReferNotify(req)

	if res := notifyDialogEndedResponse(d, req, s.referNotifyExpected(req, onReferNotify)); res != nil {
		tx.Respond(ctx, res)
		if parsed {
			s.reportUnmatchedReferNotify(n)
		}
		return
	}
	if !parsed {
		tx.Respond(ctx, sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
		return
	}

	attempt := s.recordReferNotify(n)
	if attempt == nil && onReferNotify == nil {
		tx.Respond(ctx, sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Subscription Does Not Exist", nil))
		s.reportUnmatchedReferNotify(n)
		return
	}

	// dialogHandleReferNotify answers 200 and then calls what it is given. The
	// status handed on is n's, the one read strictly above and recorded on the
	// attempt, not the one that handler reads again from the body.
	dialogHandleReferNotify(ctx, d, req, tx, func(int) {
		if attempt != nil {
			s.retireEndedReferAttempt(attempt)
		}
		if onReferNotify != nil {
			onReferNotify(n.Status)
			return
		}
		if attempt.unobserved && n.isFinal() {
			d.Hangup(ctx)
		}
	})
}
