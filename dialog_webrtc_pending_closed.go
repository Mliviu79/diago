// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"

	"github.com/emiago/diago/media"
)

// refusePendingOnClosed is what becomes of sess, a transport an offer/answer
// exchange set up to be stored as pending, when Close has run since the
// exchange found the media open: Close has already taken what it closes, so
// nothing would ever close sess, and it is closed here, unless it is current,
// the transport in place, which Close closed itself. The exchange is refused
// with errMediaClosed.
func refusePendingOnClosed(sess, current *media.MediaSessionWebrtc) error {
	if sess == nil || sess == current {
		return errMediaClosed
	}
	return errors.Join(errMediaClosed, sess.Close())
}
