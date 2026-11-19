// SPDX-License-Identifier: MPL-2.0

package diago

import "errors"

// errMediaClosed is how an offer/answer exchange is refused once the media of
// the call is closed, or its dialog has ended: nothing would close what the
// exchange set up. The media returns it from the handshake calls, and the
// session from a re-INVITE of its own on an ended dialog. A re-INVITE of the
// peer's that meets it is answered 487, see refusePeerReInvite.
var errMediaClosed = errors.New("dialog media closed")
