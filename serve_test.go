// SPDX-License-Identifier: MPL-2.0

package diago

import "context"

// serveBackground registers f as dg's dialog handler, opens dg's listeners as
// ServeBackground does and serves dg's requests on ctx. It returns once the
// listeners are up and requests are taken, or with the error of whichever
// failed.
//
// The request run is not joined: it returns only after the handlers it
// admitted have, and a handler may wait on its dialog past the end of ctx.
func serveBackground(ctx context.Context, dg *Diago, f ServeDialogFunc) error {
	dg.HandleFunc(f)
	if err := dg.ServeBackground(ctx); err != nil {
		return err
	}
	return serveRequests(ctx, dg)
}

// serveRequests serves dg's requests on ctx and returns once requests are
// taken, or with the run's error when it ended before that. A dg that only
// dials needs it too: the requests its peer sends inside a dialog, a BYE or a
// re-INVITE, are answered 480 while no run serves them.
//
// The run is not joined, for the reason serveBackground gives.
func serveRequests(ctx context.Context, dg *Diago) error {
	ready := make(chan struct{})
	served := make(chan error, 1)
	go func() {
		served <- dg.ServeRequests(ctx, func() { close(ready) })
	}()
	select {
	case <-ready:
		return nil
	case err := <-served:
		return err
	}
}
