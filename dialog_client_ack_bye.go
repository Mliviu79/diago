// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"time"

	"github.com/emiago/sipgo/sip"
)

// ackTarget is where the ACK to the 2xx goes: the Contact of the 2xx, or the
// Request-URI of the INVITE when it has none.
func (d *DialogClientSession) ackTarget() sip.Uri {
	recipient := d.InviteRequest.Recipient
	if contact := d.InviteResponse.Contact(); contact != nil {
		recipient = contact.Address
	}
	return recipient
}

// ackAndBye ends a call whose 2xx we do not carry on with. RFC 3261 section
// 13.2.2.4 has the UAC acknowledge every 2xx and end a dialog it does not want
// with a BYE, so the 2xx is acknowledged, unless Ack did that already, and a
// BYE is sent. Both are bounded by the dialog and by 10 seconds, since the
// context of the call that failed may be done already.
//
// Both are acts of the session alone: the media of the call takes no part in
// them, and is left to the session's Close.
func (d *DialogClientSession) ackAndBye() error {
	ctx, cancel := context.WithTimeout(d.Context(), 10*time.Second)
	defer cancel()
	if d.LoadState() == sip.DialogStateEstablished {
		if err := d.ack(ctx, d.ackTarget(), nil); err != nil {
			return err
		}
	}
	return d.Bye(ctx)
}
