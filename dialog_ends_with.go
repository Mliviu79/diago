// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"

	"github.com/emiago/sipgo"
)

// endsWithDialog returns a context derived from ctx that also ends when
// dialog does, with the cause the dialog ended with. It is for work a caller
// bounds with a context of its own that belongs to the call as well, and has
// to end when the call does: the handshake that keys a call's media waits for
// the peer, and a peer that has hung up never completes it.
//
// The caller calls the returned func once the work is done. It releases the
// derived context and waits for the watch on the dialog to be over.
func endsWithDialog(ctx context.Context, dialog *sipgo.Dialog) (context.Context, func()) {
	ctx, cancel := context.WithCancelCause(ctx)
	done := make(chan struct{})
	stop := context.AfterFunc(dialog.Context(), func() {
		defer close(done)
		cancel(context.Cause(dialog.Context()))
	})
	return ctx, func() {
		if !stop() {
			<-done
		}
		cancel(nil)
	}
}
