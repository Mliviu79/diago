// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// sendFinalNotify sends the NOTIFY that terminates the refer subscription
// Accept created, reporting the outcome of the INVITE the REFER triggered:
// "Each NOTIFY MUST contain a body of type message/sipfrag" with the status
// line of the referenced request, and "the final NOTIFY sent in response to a
// REFER MUST indicate the subscription has been 'terminated' with a reason of
// 'noresource'" (RFC 3515 section 2.4.5).
func (r *ReferTransaction) sendFinalNotify(ctx context.Context, referDialog *DialogClientSession, inviteErr error) error {
	statusCode, reason := referInviteFinalStatus(referDialog, inviteErr)
	if err := r.sendNotify(ctx, statusCode, reason, subState{
		state:  "terminated",
		reason: "noresource",
	}); err != nil {
		return fmt.Errorf("refer final NOTIFY failed to send: %w", err)
	}
	return nil
}

// referInviteFinalStatus gives the status line the terminating NOTIFY carries
// for the INVITE a REFER triggered: the 2xx it was answered with, the final
// response it was refused with, and 400 Bad Request when it failed without a
// final response, as the OnRefer path reports it.
func referInviteFinalStatus(referDialog *DialogClientSession, inviteErr error) (int, string) {
	if inviteErr == nil {
		res := referDialog.InviteResponse
		return res.StatusCode, res.Reason
	}
	var resErr *sipgo.ErrDialogResponse
	if errors.As(inviteErr, &resErr) {
		return resErr.Res.StatusCode, resErr.Res.Reason
	}
	return sip.StatusBadRequest, "Bad Request"
}
