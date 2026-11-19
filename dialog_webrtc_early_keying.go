// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"time"

	"github.com/emiago/diago/media"
)

var _ mediaEarlyKeyer = (*DialogWebrtc)(nil)

// onFinalizeWithin completes ICE and DTLS on the transport of the media's
// first exchange within wait, or leaves them running, as mediaEarlyKeyer
// describes. A transport completed in time is set up as the media of the
// call at once, as onFinalize sets it up, so early media carries audio from
// then on.
//
// Media whose negotiation is left running has no transport set up: it has
// no RTP session, packet reader or packet writer, and its transport refuses
// to send with media.ErrDTLSNotKeyed. A failed negotiation leaves the
// transport in place, failed, for onFinalize to report again and close.
func (d *DialogWebrtc) onFinalizeWithin(ctx context.Context, wait time.Duration) error {
	sess, ok := d.firstExchangeSession()
	if !ok {
		d.mu.Lock()
		closed := d.closed
		installed := d.mediaSession != nil
		d.mu.Unlock()
		if closed {
			return errMediaClosed
		}
		if installed {
			// Keyed already: nothing is left to complete.
			return nil
		}
		return errNoMediaSession
	}

	err := sess.FinalizeWithin(ctx, wait)
	if errors.Is(err, media.ErrFinalizeInProgress) {
		return errMediaKeyingInProgress
	}
	if err != nil {
		return err
	}
	return d.installFirstExchange(sess)
}

// takeFirstExchange makes sess, a transport whose offer went out in an INVITE,
// the transport of the media's first exchange, as newDialogWebrtc makes the
// transport it gathers, so the answer of a 183 to that offer is applied and
// keyed through the media. Media with a transport already keeps it, and
// closed media refuses sess with errMediaClosed; sess stays its caller's to
// close either way.
func (d *DialogWebrtc) takeFirstExchange(sess *media.MediaSessionWebrtc) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errMediaClosed
	}
	if d.mediaSession != nil || d.pendingMediaSession != nil {
		return nil
	}
	d.pendingMediaSession = sess
	return nil
}
