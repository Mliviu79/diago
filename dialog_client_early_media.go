// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"time"
)

// keyEarlyAnswer has keyer, the media the answer in a 183 was just applied
// to, keyed by the callee, and reports whether it is: the call has early
// media then, and none when it reports false without an error.
//
// The callee may take part in the handshake only once the call is answered,
// as a callee that ignores early media does (RFC 3960), so the wait for the
// keys is bounded by wait, five seconds when it is zero, and the handshake is
// not ended with it: it runs until the call ends, and Ack completes it.
// Meanwhile the media sends nothing and has no audio set up, the answer of the
// 183 stands, and no later session description is applied.
//
// The wait also ends with ctx, which the wait for the INVITE's answer it runs
// in cannot see end while it runs, and the error is then ctx's cause.
func (d *DialogClientSession) keyEarlyAnswer(ctx context.Context, keyer mediaEarlyKeyer, wait time.Duration) (keyed bool, err error) {
	if wait <= 0 {
		wait = defaultEarlyMediaKeyTimeout
	}
	err = d.awaitEarlyKeys(ctx, keyer, wait)
	if errors.Is(err, errMediaKeyingInProgress) {
		d.noteEarlyKeyingLeft()
		return false, nil
	}
	return err == nil, err
}

// awaitEarlyKeys has keyer run the negotiation the answer in a 183 left it,
// and waits for it up to wait, or until ctx ends. The negotiation is bounded
// by the dialog only, and a wait that ends before it does returns
// errMediaKeyingInProgress and leaves it running.
func (d *DialogClientSession) awaitEarlyKeys(ctx context.Context, keyer mediaEarlyKeyer, wait time.Duration) error {
	// Checked in steps, as the keyer's own wait is not bounded by ctx: the
	// ctx it takes is the one the negotiation is left running under.
	const step = 50 * time.Millisecond
	deadline := time.Now().Add(wait)
	for {
		err := keyer.onFinalizeWithin(d.Context(), min(time.Until(deadline), step))
		if !errors.Is(err, errMediaKeyingInProgress) {
			return err
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if time.Until(deadline) <= 0 {
			return errMediaKeyingInProgress
		}
	}
}
