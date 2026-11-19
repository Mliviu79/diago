// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"fmt"

	"github.com/emiago/diago/media"
)

// replaceMediaSessionUnsafe puts msess in place of the media session in use,
// as a media update does, together with what has to follow the media across
// it: the RTP read and write deadlines StopRTP and StartRTP last set. A read
// stopped on the session replaced then stays stopped on msess, and a read
// that was to end at a deadline still ends at it, whether msess runs on the
// same sockets or on its own.
//
// The deadlines go first. Moving the packet reader ends a read waiting on the
// replaced session, and that read is taken up again on msess with the read
// deadline msess holds by then.
//
// The sockets of the session replaced are closed last, when msess runs on
// its own, see closeReplacedSockets.
//
// Must be called with d.mu held.
func (d *DialogMedia) replaceMediaSessionUnsafe(msess *media.MediaSession) error {
	old := d.mediaSession
	if old != nil && old != msess {
		if err := msess.CarryRTPDeadlines(old); err != nil {
			return fmt.Errorf("carrying RTP deadlines to the new media session: %w", err)
		}
	}
	if err := d.replaceRTPSessionUnsafe(msess); err != nil {
		return err
	}
	return closeReplacedSockets(old, msess)
}
