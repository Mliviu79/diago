// SPDX-License-Identifier: MPL-2.0

package media

import (
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/require"
)

// TestWebrtcAbandonNegotiationRunsAfresh pins that a negotiation
// AbandonNegotiation ended is not what the next Finalize reports: the offerer
// leaves the negotiation of an answer running, ends it for a second answer
// with the same ICE credentials and fingerprint, and Finalize then keys the
// session with a negotiation of its own, whose DTLS handshake runs on a fresh
// conn.
func TestWebrtcAbandonNegotiationRunsAfresh(t *testing.T) {
	ctx := t.Context()
	offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))

	// The answerer, the DTLS client, sends nothing yet.
	require.ErrorIs(t, offerer.FinalizeWithin(ctx, 300*time.Millisecond), ErrFinalizeInProgress)
	require.NoError(t, offerer.AbandonNegotiation())
	require.True(t, offerer.FinalizePending(), "the ended negotiation left nothing for Finalize to run")
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true), "the same endpoint's answer was refused")
	require.True(t, offerer.FinalizePending())

	requireBothFinalize(t, offerer, answerer, 40*time.Second, "negotiation after the abandon")
	require.NoError(t, offerer.WriteRTP(dtlsTestPacket(1)), "the session was not keyed")
}

// TestWebrtcAbandonNegotiationNoneRunning pins that a session that left no
// negotiation running is left as it is.
func TestWebrtcAbandonNegotiationNoneRunning(t *testing.T) {
	ctx := t.Context()
	offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))

	offerer.mu.Lock()
	conn := offerer.dtlsConn
	offerer.mu.Unlock()
	require.NoError(t, offerer.AbandonNegotiation())
	offerer.mu.Lock()
	kept := offerer.dtlsConn
	offerer.mu.Unlock()
	require.Same(t, conn, kept, "the DTLS conn of a negotiation never run was replaced")

	requireBothFinalize(t, offerer, answerer, 40*time.Second, "negotiation")
}
