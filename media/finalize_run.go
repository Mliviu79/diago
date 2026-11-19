// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrFinalizeInProgress is returned by FinalizeWithin when the peer has
	// not completed the negotiation within the wait it was given. The
	// negotiation is still running, and a later Finalize waits for it.
	ErrFinalizeInProgress = errors.New("media negotiation still in progress")

	// ErrDTLSNotKeyed is returned by WriteRTP and WriteRTCP while a
	// negotiation FinalizeWithin left running has not keyed SRTP, and after
	// it failed, and is wrapped by the error of a write on a
	// MediaSessionWebrtc that no Finalize has keyed. A DTLS-SRTP
	// MediaSession returns it from WriteRTP, WriteRTCP and WriteRTCPs until
	// its handshake has keyed it, a fork that starts a new DTLS association
	// included. The media is refused rather than sent unprotected: SRTP
	// processing does not start before the handshake completes (RFC 5764
	// section 5.1), and the profile protects media with nothing else.
	ErrDTLSNotKeyed = errors.New("dtls-srtp not keyed")
)

// finalizeRun is a negotiation running on a goroutine of its own. The session
// that started it owns it: its Finalize or its Close waits for it to end.
type finalizeRun struct {
	cancel context.CancelFunc
	// done is closed when the negotiation has returned, and err is its
	// result, which is read only once done is closed.
	done chan struct{}
	err  error
}

// negotiationLeftRunning is what a media session keeps of the negotiation its
// FinalizeWithin left running. Its zero value is a session that left none
// running. It is safe for concurrent use, since Close may run on another
// goroutine than the one that waits for the negotiation.
type negotiationLeftRunning struct {
	// mu guards run and err.
	mu sync.Mutex
	// run is the negotiation left running, until a Finalize, a FinalizeWithin
	// or Close has seen it end.
	run *finalizeRun
	// err is the error of a negotiation left running that failed. It is
	// reported from then on, since the session was never keyed.
	err error

	// unkeyed is set when a negotiation is left running and cleared once
	// that negotiation has keyed SRTP; the session refuses to write media
	// while it is set. It is cleared only after the negotiation has
	// returned, so a writer that finds it clear finds the SRTP contexts,
	// although another goroutine set them, and a writer that finds it set
	// reads none of what that goroutine writes.
	unkeyed atomic.Bool
}

// start runs negotiate on a goroutine of its own and returns the run. The
// negotiation is bounded by ctx alone until it is waited for; await, within
// and stop are what wait for it, and stop is what a session's Close calls.
func (l *negotiationLeftRunning) start(ctx context.Context, negotiate func(ctx context.Context) error) *finalizeRun {
	ctx, cancel := context.WithCancel(ctx)
	run := &finalizeRun{cancel: cancel, done: make(chan struct{})}
	l.mu.Lock()
	l.run = run
	l.mu.Unlock()
	// No media goes out until the negotiation has keyed SRTP, and nothing the
	// negotiation writes is read meanwhile.
	l.unkeyed.Store(true)
	go func() {
		defer close(run.done)
		defer cancel()
		run.err = negotiate(ctx)
		if run.err == nil {
			l.unkeyed.Store(false)
		}
	}()
	return run
}

// current returns the negotiation left running, nil when there is none, and
// the error kept of one that failed.
func (l *negotiationLeftRunning) current() (*finalizeRun, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.run, l.err
}

// forget drops a negotiation that has returned, keeping the error of one that
// failed.
func (l *negotiationLeftRunning) forget(run *finalizeRun) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.run != run {
		return
	}
	l.run = nil
	if run.err != nil {
		l.err = run.err
	}
}

// end forgets a negotiation that has returned and reports its result.
func (l *negotiationLeftRunning) end(run *finalizeRun) error {
	l.forget(run)
	return run.err
}

// within waits for run as long as wait and reports its result, or
// ErrFinalizeInProgress when it has not returned by then. A wait of zero or
// less does not wait. It takes no ctx: the ctx the negotiation was started
// with ends the negotiation, and with it this wait.
func (l *negotiationLeftRunning) within(run *finalizeRun, wait time.Duration) error {
	if wait <= 0 {
		select {
		case <-run.done:
			return l.end(run)
		default:
			return ErrFinalizeInProgress
		}
	}

	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-run.done:
		return l.end(run)
	case <-timer.C:
		return ErrFinalizeInProgress
	}
}

// await waits for run for as long as ctx lives and at most bound, and ends
// the negotiation when either ends first: the error then wraps
// context.Cause(ctx), or context.DeadlineExceeded for the bound. A negotiation
// that completes while it is being ended is kept.
//
// The peer is expected to take part from this call on, so resendHello, which
// sends again a ClientHello the peer has not answered, is called at once and
// then on the schedule RFC 6347 section 4.2.4.1 gives a retransmission,
// starting at a second and doubling. The DTLS stack's own timer has backed off
// meanwhile, up to a minute.
func (l *negotiationLeftRunning) await(ctx context.Context, run *finalizeRun, bound time.Duration, resendHello func() error) error {
	timer := time.NewTimer(bound)
	defer timer.Stop()

	resend := time.NewTimer(0)
	defer resend.Stop()
	resendAfter := time.Duration(0)

	var stop error
wait:
	for {
		select {
		case <-run.done:
			return l.end(run)
		case <-ctx.Done():
			stop = fmt.Errorf("media session: negotiation: %w", context.Cause(ctx))
			break wait
		case <-timer.C:
			stop = fmt.Errorf("media session: negotiation did not complete within %s: %w", bound, context.DeadlineExceeded)
			break wait
		case <-resend.C:
			if err := resendHello(); err != nil {
				DefaultLogger().Debug("Resending the DTLS ClientHello failed", "error", err)
			}
			resendAfter = max(time.Second, 2*resendAfter)
			resend.Reset(resendAfter)
		}
	}
	run.cancel()
	<-run.done
	if err := l.end(run); err == nil {
		return nil
	}
	return stop
}

// stop ends a negotiation left running and returns once it has returned. A
// session's Close calls it before it releases what the negotiation reads and
// writes.
func (l *negotiationLeftRunning) stop() {
	run, _ := l.current()
	if run == nil {
		return
	}
	run.cancel()
	<-run.done
	l.forget(run)
}
