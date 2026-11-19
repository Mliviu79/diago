// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"time"
)

// FinalizeWithin runs the negotiation as FinalizeContext does, but waits for it
// only as long as wait, for a peer that may not take part yet: a caller given
// our answer in a 183 is free to ignore it (RFC 3960) and to run the handshake
// only once the call is answered. It then returns ErrFinalizeInProgress and
// leaves the negotiation running, for a later FinalizeContext to wait for.
//
// The negotiation is left running because it cannot be run again in its place.
// As the DTLS client this session has sent ClientHellos, which wait on the
// peer's socket until its DTLS stack reads them: it completes this association
// from them, and fails a new one they are read ahead of. A peer that discarded
// them instead gets the last one again from FinalizeContext.
//
// Until FinalizeContext waits for it, the negotiation is bounded by ctx alone,
// which may therefore not end with this call, and by Close, which ends it and
// waits for it. FinalizeContext then waits for it as long as its own ctx lives
// and at most DTLSHandshakeTimeout, and ends it when either ends first. The
// session carries no media meanwhile: WriteRTP, WriteRTCP and WriteRTCPs
// return ErrDTLSNotKeyed, ReadRTP and ReadRTCP discard what arrives, and
// nothing may apply a description to the session or fork it, since the
// negotiation reads and writes what those do.
//
// A negotiation that ends within wait, successfully or not, is reported as
// FinalizeContext reports it. One that failed stays failed: every later
// FinalizeWithin and FinalizeContext returns its error. A wait of zero or less
// does not wait: it reports a negotiation left running that has ended, and
// ErrFinalizeInProgress for one that has not. A session with no negotiation to
// run, one that needs none or has run it, returns nil.
func (s *MediaSession) FinalizeWithin(ctx context.Context, wait time.Duration) error {
	run, kept := s.leftRunning.current()
	if run != nil {
		return s.leftRunning.within(run, wait)
	}
	if kept != nil {
		return kept
	}
	if s.onFinalize == nil {
		return nil
	}

	// The run takes the negotiation: nothing else may run it beside the run,
	// and FinalizeContext finds the run to wait for instead.
	negotiate := s.onFinalize
	s.onFinalize = nil
	run = s.leftRunning.start(ctx, func(ctx context.Context) error {
		// Not capped by DTLSHandshakeTimeout: the caller may take longer than
		// that to answer, and the cap counts from the FinalizeContext that
		// waits for the handshake.
		return negotiate(ctx, false)
	})
	return s.leftRunning.within(run, wait)
}

// resendClientHello sends again the ClientHello of this session's DTLS
// handshake while the peer has not answered it: see
// dtlsKeyExchangeConn.resendHello. A session that armed no handshake sends
// nothing.
func (s *MediaSession) resendClientHello() error {
	if s.dtlsTr == nil {
		return nil
	}
	return s.dtlsTr.resendHello()
}
