// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"

	"github.com/emiago/sipgo/sip"
)

var errInviteMissingHeader = errors.New("new INVITE rejected with 400 Bad Request")

// rejectInviteMissingHeader answers a new INVITE that lacks a header dialog
// setup needs with 400 and a reason phrase naming the header (RFC 3261 section
// 21.4.1), and returns the rejection joined with the error of sending it. It
// returns nil, and answers nothing, when the INVITE has every header. Answering
// through the transaction lets it answer the retransmissions and absorb the
// ACK, so the sender stops retransmitting and the rejection is reported once.
func rejectInviteMissingHeader(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) error {
	reason := describeMissingInviteHeader(req)
	if reason == "" {
		return nil
	}
	res := sip.NewResponseFromRequest(req, sip.StatusBadRequest, reason, nil)
	return errors.Join(fmt.Errorf("%w: %s", errInviteMissingHeader, reason), tx.Respond(ctx, res))
}

// describeMissingInviteHeader returns the reason phrase for the first header a
// new INVITE lacks among those sipgo.DialogUA.ReadInvite needs: From, the From
// tag, To, Call-ID, CSeq and Contact. It returns "" when all are present. The
// check runs before ReadInvite so a malformed INVITE is answered, because most
// of the errors ReadInvite returns for these headers carry no type to match on.
func describeMissingInviteHeader(req *sip.Request) string {
	from := req.From()
	switch {
	case from == nil:
		return "Missing From Header Field"
	case !from.Params.Has("tag"):
		return "Missing From Tag"
	case req.To() == nil:
		return "Missing To Header Field"
	case req.CallID() == nil:
		return "Missing Call-ID Header Field"
	case req.CSeq() == nil:
		// The transaction layer answers a request without CSeq before any
		// handler runs; the case keeps the list complete for ReadInvite.
		return "Missing CSeq Header Field"
	case req.Contact() == nil:
		return "Missing Contact Header Field"
	}
	return ""
}

// fromAndToForLog returns the From header and the To value of a request whose
// handling failed, for the warning logged about it. A header the request lacks
// is returned empty: dialog matching and INVITE reading fail exactly when From
// or To is missing, so the failure path cannot assume either.
func fromAndToForLog(req *sip.Request) (from, to string) {
	if h := req.From(); h != nil {
		from = h.String()
	}
	if h := req.To(); h != nil {
		to = h.Value()
	}
	return from, to
}
