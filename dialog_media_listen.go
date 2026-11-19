// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
)

// stopReadsAtEnd ends a read of the dialog's media when ctx ends, with a read
// deadline set at that moment on the media session in use.
//
// The caller calls the function returned once it reads no more. When ctx has
// ended, that waits for the deadline to be set and clears it again, so the
// next read of the dialog does not fail on it, and returns what setting or
// clearing it failed with. When ctx has not ended, no deadline was set, and
// none is set later.
func (d *DialogMedia) stopReadsAtEnd(ctx context.Context) (restore func() error) {
	stopped := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { stopped <- d.StopRTP(1, 0) })
	return func() error {
		if stop() {
			return nil
		}
		return errors.Join(<-stopped, d.StartRTP(1, 0))
	}
}
