// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ICEConnectTimeout caps how long Finalize of a MediaSessionWebrtc waits for
// the ICE connectivity checks to select a candidate pair, whatever context it
// was given. The ICE agent goes on checking for as long as it lives, so
// without a cap a peer that never answers a check would hold Finalize and its
// caller for good. Checks FinalizeWithin left running are capped from the
// moment Finalize waits for them.
var ICEConnectTimeout = 30 * time.Second

// errWebRTCSessionClosed is what FinalizeWithin returns on a closed session,
// which has no negotiation to leave running.
var errWebRTCSessionClosed = errors.New("webrtc media session is closed")

// negotiationStep runs step, one wait of a negotiation for the peer, under
// ctx, and when capped is set for no longer than limit. The error of a step
// that its context ended wraps the cause of that end, context.DeadlineExceeded
// for the limit, also where the step itself says only that its caller gave up,
// as the ICE agent does.
func negotiationStep(ctx context.Context, capped bool, limit time.Duration, step func(ctx context.Context) error) error {
	if capped {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, limit)
		defer cancel()
	}
	err := step(ctx)
	if err == nil || ctx.Err() == nil {
		return err
	}
	cause := context.Cause(ctx)
	if errors.Is(err, cause) {
		return err
	}
	return fmt.Errorf("%w: %w", err, cause)
}

// finalize is Finalize. It waits for a negotiation FinalizeWithin left
// running, reports the error of one that failed, and otherwise runs the
// negotiation itself, capped by ICEConnectTimeout and DTLSHandshakeTimeout.
func (m *MediaSessionWebrtc) finalize(ctx context.Context) error {
	run, kept := m.leftRunning.current()
	if run != nil {
		// The same bounds as a negotiation Finalize runs itself, counted
		// from this call.
		err := m.leftRunning.await(ctx, run, ICEConnectTimeout+DTLSHandshakeTimeout, m.resendClientHello)
		if err != nil {
			return err
		}
		// The session is negotiated now, and commits below what was staged
		// since the negotiation ended.
		return m.negotiate(ctx, true)
	}
	if kept != nil {
		return kept
	}
	if err := m.negotiate(ctx, true); err != nil {
		return err
	}
	// An answer applied after AbandonNegotiation ended a negotiation was
	// staged, as one that continues the transport is, and is committed now
	// that the transport is negotiated.
	return m.negotiate(ctx, true)
}

// resendClientHello sends again the ClientHello of this session's DTLS
// handshake while the peer has not answered it: see
// dtlsKeyExchangeConn.resendHello. A session that has started no handshake as
// the DTLS client sends nothing.
func (m *MediaSessionWebrtc) resendClientHello() error {
	m.mu.Lock()
	tr := m.dtlsTr
	m.mu.Unlock()
	if tr == nil {
		return nil
	}
	return tr.resendHello()
}

// FinalizeWithin runs the negotiation as Finalize does, but waits for it only
// as long as wait, for a peer that may not take part yet: a caller given our
// answer in a 183 is free to ignore it (RFC 3960) and to run the connectivity
// checks and the handshake only once the call is answered. It then returns
// ErrFinalizeInProgress and leaves the negotiation running, for a later
// Finalize to wait for.
//
// The negotiation is left running because it cannot be run again in its place.
// As the DTLS client this session has sent ClientHellos, which wait at the
// peer until its DTLS stack reads them: it completes this association from
// them, and fails a new one they are read ahead of. A peer that discarded them
// instead gets the last one again from Finalize.
//
// Until Finalize waits for it, the negotiation is bounded by ctx alone, which
// may therefore not end with this call, and by Close, which ends it and waits
// for it. Finalize then waits for it as long as its own ctx lives and at most
// ICEConnectTimeout plus DTLSHandshakeTimeout, and ends it when either ends
// first. The session carries no media meanwhile: WriteRTP and WriteRTCP return
// ErrDTLSNotKeyed, ReadRTP and ReadRTCP discard what arrives, and nothing may
// apply a description to the session, which the negotiation reads.
//
// A negotiation that ends within wait, successfully or not, is reported as
// Finalize reports it. One that failed stays failed: every later
// FinalizeWithin and Finalize returns its error. A wait of zero or less does
// not wait: it reports a negotiation left running that has ended, and
// ErrFinalizeInProgress for one that has not. A session with nothing to wait
// for the peer in, one that is negotiated already or has no remote description
// yet, is answered as Finalize answers it.
func (m *MediaSessionWebrtc) FinalizeWithin(ctx context.Context, wait time.Duration) error {
	run, err := m.leaveRunning(ctx)
	if run == nil {
		return err
	}
	if err := m.leftRunning.within(run, wait); err != nil {
		return err
	}
	// The session is negotiated now, and commits what was staged since the
	// negotiation ended, as Finalize does.
	return m.negotiate(ctx, true)
}

// leaveRunning returns the negotiation FinalizeWithin waits for: the one left
// running already, or the one it starts on ctx. It returns no run, and what
// FinalizeWithin is to return, when there is none to wait for.
func (m *MediaSessionWebrtc) leaveRunning(ctx context.Context) (*finalizeRun, error) {
	// The lock makes one decision of the check and the start, and Close
	// marks the session closed under it before it stops a run: a negotiation
	// is either started before Close looks for one or not at all.
	m.mu.Lock()
	run, kept := m.leftRunning.current()
	if run != nil {
		m.mu.Unlock()
		return run, nil
	}
	if kept != nil {
		m.mu.Unlock()
		return nil, kept
	}
	if m.closed {
		m.mu.Unlock()
		return nil, errWebRTCSessionClosed
	}
	if m.ready || m.agent == nil || m.dtlsConn == nil {
		m.mu.Unlock()
		// Nothing here waits for the peer: a negotiated session commits what
		// is staged and one without a remote description is refused.
		return nil, m.negotiate(ctx, true)
	}
	run = m.leftRunning.start(ctx, func(ctx context.Context) error {
		return m.negotiate(ctx, false)
	})
	m.mu.Unlock()
	return run, nil
}

// FinalizePending reports whether Finalize has negotiation left to run or to
// wait for: the connectivity checks and the DTLS handshake of a session whose
// remote description was applied and that is not negotiated yet, or a
// negotiation FinalizeWithin left running that has not been waited for. A
// session without a remote description, a negotiated one, and one whose
// negotiation left running failed have none.
func (m *MediaSessionWebrtc) FinalizePending() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	run, kept := m.leftRunning.current()
	if run != nil {
		return true
	}
	return kept == nil && m.dtlsConn != nil && !m.ready
}
