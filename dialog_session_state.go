// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"sync/atomic"

	"github.com/emiago/sipgo/sip"
)

// sessionState is the SIP-side state a dialog session keeps of its own. Both
// session types embed it beside dialogCallbacks. dialogCallbacks is what the
// media of a call registers with and is handed, so state that only the session
// reads and writes is kept here, apart from it and from every lock of the
// media.
type sessionState struct {
	// terminatingBye holds the BYE request that ended this dialog, captured by
	// ReadBye for read-only inspection. Without it the BYE is unrecoverable:
	// ReadBye answers 200 and drops the request, so an application observes the
	// ending only as a Context() cancel and cannot tell the peer's BYE from our
	// own, nor see a cause the peer stated for itself, such as an RFC 3326
	// Reason header.
	//
	// nil until a remote BYE arrives. A local Hangup, a transport death or a
	// cancel all leave it nil, which is the truthful "the peer stated no cause".
	terminatingBye atomic.Pointer[sip.Request]

	// uncache removes the dialog from its Diago's dialog cache, and the Close
	// of an outbound dialog runs it. An ended dialog stays findable until its
	// owner closes it, as an inbound one does until its serve handler returns,
	// so a NOTIFY for a REFER subscription that outlived the BYE still reaches
	// it (RFC 5057 section 5.5). Nil for a dialog no Diago made, and for an
	// inbound one, which the handler of its INVITE removes. Set before the
	// dialog is handed out and not changed after.
	uncache func() error
}

// leaveCache removes the dialog from the dialog cache it was stored in, if
// any.
func (s *sessionState) leaveCache() error {
	if s.uncache == nil {
		return nil
	}
	return s.uncache()
}
