// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"sync"
	"testing"
	"time"
)

// serveEndBound is how long a test waits for serving to end: longer than the 10
// seconds diago allows a handler's hang-up after the handler returns.
const serveEndBound = 15 * time.Second

// waitEnded fails the test when ended is not closed within serveEndBound.
func waitEnded(t testing.TB, ended <-chan struct{}) {
	t.Helper()
	select {
	case <-ended:
	case <-time.After(serveEndBound):
		t.Error("serving did not end with the test")
	}
}

// serveBackground serves dg as ServeBackground does and runs its request
// serving, with f as the dialog handler when it is not nil, on a context that
// ends with ctx or when the test ends, and has the test wait, bounded, until
// both have returned. sipgo closes a listener from a goroutine of its own once
// the context is done, so a test that returned without waiting could leave its
// port bound, and the next test binding the same fixed port failed with
// "address already in use". Serving returns only once its listener is closed.
// dg serves a single transport, since serving returns with its first
// listener.
func serveBackground(t testing.TB, dg *Diago, ctx context.Context, f ServeDialogFunc) error {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	var terminations sync.WaitGroup
	if f != nil {
		// A dialog ends with the run its handler was given: the call is ended
		// with a BYE, which is bounded since its peer may be gone, and its
		// media closed, which frees a handler blocked in it. It ends so even
		// when the handler returned before, as the request run waits for the
		// BYE diago sends after the handler.
		dg.HandleFunc(func(ctx context.Context, d *DialogServerSession) {
			terminations.Add(1)
			context.AfterFunc(ctx, func() {
				defer terminations.Done()
				hangupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
				defer cancel()
				_ = d.Hangup(hangupCtx)
				_ = d.Close()
			})
			f(ctx, d)
		})
	}
	ready := make(chan struct{})
	done := make(chan struct{})
	var serveErr error
	go func() {
		defer close(done)
		serveErr = dg.ServeWithReady(ctx, func() { close(ready) })
	}()
	requestsReady := make(chan struct{})
	requestsDone := make(chan struct{})
	go func() {
		defer close(requestsDone)
		dg.ServeRequests(ctx, func() { close(requestsReady) })
	}()
	t.Cleanup(func() {
		cancel()
		// A handler registers its termination before it runs, and the request run
		// returns only after every handler has, so no registration is left when
		// the wait for terminations starts. The wait is longer than the 10
		// seconds diago allows a handler's hang-up after the handler returns.
		waitEnded(t, requestsDone)
		terminated := make(chan struct{})
		go func() {
			defer close(terminated)
			terminations.Wait()
		}()
		waitEnded(t, terminated)
		waitEnded(t, done)
	})

	select {
	case <-requestsReady:
	case <-requestsDone:
	}
	select {
	case <-ready:
		return nil
	case <-done:
		return serveErr
	}
}

// serveRequests runs dg.ServeRequests for the test, with dg's dialog handler
// as it is, and has the test wait, bounded, until it has returned. It returns
// once requests are taken.
func serveRequests(t testing.TB, dg *Diago) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	ready := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		dg.ServeRequests(ctx, func() { close(ready) })
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("request serving did not end with the test")
		}
	})
	select {
	case <-ready:
	case <-done:
		t.Fatal("request serving ended before it took requests")
	}
}
