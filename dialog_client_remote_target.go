// SPDX-License-Identifier: MPL-2.0

package diago

import "github.com/emiago/sipgo/sip"

// remoteTarget is the Request-URI of a request we send within the dialog
// (RFC 3261 section 12.2.1.1): the remote target, RemoteContact, or the
// Request-URI of the INVITE when the answer to it carried no Contact and no
// target refresh has set one since.
//
// RFC 3261 section 12.1.2 sets the remote target from the Contact of the
// response that establishes the dialog and says nothing of a response without
// one, which leaves no target to send to. Such an answer is accepted, and its
// ACK and the BYE go to the INVITE's Request-URI (ackTarget, NewByeRequest),
// as sipgo's own ACK and BYE do; every other request of the dialog goes there
// too, so none of them is left without a Request-URI.
func (d *DialogClientSession) remoteTarget() sip.Uri {
	if contact := d.RemoteContact(); contact != nil {
		return *contact.Address.Clone()
	}
	return *d.InviteRequest.Recipient.Clone()
}
