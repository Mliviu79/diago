// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"
	"fmt"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// errMediaUpdateAfterAnswer marks a media update that failed after its
// answer. The offer/answer exchange was complete by then, so the peer has
// moved to the new session, which this side could not complete: the session
// that reports it has ended the call with a BYE. When the update was a new
// DTLS association whose handshake failed, RFC 5763 section 5 has the media
// session torn down at once, and with a single audio stream that is the call.
var errMediaUpdateAfterAnswer = errors.New("media update failed after its answer")

// updateFailedAfterAnswer says what becomes of err, why the media did not
// complete an update of the peer's when the ACK to our 2xx was read. It
// returns err marked with errMediaUpdateAfterAnswer, for the caller to end
// the call on, and nil once dialog has ended: its end ends the handshake the
// update waited for, which is no failure of the update, and leaves no call to
// end.
func updateFailedAfterAnswer(dialog *sipgo.Dialog, err error) error {
	if dialog.LoadState() == sip.DialogStateEnded {
		return nil
	}
	return fmt.Errorf("%w: %w", errMediaUpdateAfterAnswer, err)
}
