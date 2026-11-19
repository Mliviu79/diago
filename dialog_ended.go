// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"github.com/emiago/sipgo/sip"
)

// dialogEndedResponse returns the 481 that answers req when d has ended, and
// nil while d is live. An ended dialog stays in the dialog cache until its call
// handler returns, for an inbound call, or until it is closed, for an outbound
// one, so an in-dialog request can still find it. It matches no live dialog,
// and RFC 3261 section 12.2.2 has such a request answered 481.
func dialogEndedResponse(d DialogSession, req *sip.Request) *sip.Response {
	if d.DialogSIP().LoadState() != sip.DialogStateEnded {
		return nil
	}
	return sip.NewResponseFromRequest(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)
}

// notifyDialogEndedResponse returns what dialogEndedResponse does for a
// NOTIFY, unless the NOTIFY is for a REFER d still tracks, which referTracked
// tells. A BYE ends the INVITE usage of a dialog, not the subscription usage a
// REFER created, and the dialog lives on until its last usage ends (RFC 5057
// sections 2 and 4.1), so a transfer outcome is still taken after the call has
// ended.
func notifyDialogEndedResponse(d DialogSession, req *sip.Request, referTracked bool) *sip.Response {
	if referTracked {
		return nil
	}
	return dialogEndedResponse(d, req)
}
