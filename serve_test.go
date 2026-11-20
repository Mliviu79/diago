// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
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
//
// A dialog is ended by its handler, never by the helper: the handler's ctx
// ends with serving, the handler returns, and diago hangs the call up and
// closes it after the handler. A DialogServerSession is used by one goroutine
// at a time, so a hang-up from another goroutine while the handler still
// responds would race with it.
func serveBackground(t testing.TB, dg *Diago, ctx context.Context, f ServeDialogFunc) error {
	t.Helper()
	ctx, cancel := context.WithCancel(ctx)
	if f != nil {
		dg.HandleFunc(f)
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
		// The request run returns only after every handler has, and after the
		// hang-up diago makes when a handler returns, so its end is the end
		// of every dialog it served.
		waitEnded(t, requestsDone)
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
