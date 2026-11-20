// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"time"

	"github.com/emiago/sipgo/sip"
)

// teardownWait bounds a teardown that a failure of the call starts: the ACK
// and BYE that end a call given up, or the BYE that ends a call left without
// usable media. The ctx of the work that failed may have ended already, and
// the BYE ends the dialog as it is sent, so neither bounds it.
const teardownWait = 10 * time.Second

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
// BYE is sent.
//
// It owns this teardown, which ctx, the ctx of the call given up, does not
// bound: that ctx may be what ended the call, and the BYE ends the dialog as
// it is sent. Both are bounded by teardownWait instead, and the wait for the
// BYE's answer goes on past the end of the dialog.
//
// Both are acts of the session alone: the media of the call takes no part in
// them, and is left to the session's Close.
func (d *DialogClientSession) ackAndBye(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), teardownWait)
	defer cancel()
	if d.LoadState() == sip.DialogStateEstablished {
		if err := d.ack(ctx, d.ackTarget(), nil); err != nil {
			return err
		}
	}
	return d.Bye(ctx)
}
