// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/require"
)

// TestDTLSHandshakeTimeout pins that a DTLS handshake the peer never completes
// ends after DTLSHandshakeTimeout, whatever context Finalize was given. The
// DTLS stack retransmits its flights for as long as its context lives, so a
// peer that never answers parked Finalize, and the socket with it, for good.
// Both roles are covered: the client sends a ClientHello nobody answers, and
// the server waits for one that never comes.
func TestDTLSHandshakeTimeout(t *testing.T) {
	prev := DTLSHandshakeTimeout
	DTLSHandshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { DTLSHandshakeTimeout = prev })

	for _, setup := range []string{"active", "passive"} {
		t.Run("answerer "+setup, func(t *testing.T) {
			offerer := newDTLSRoleSession(t, testdata.ClientCertificate())
			answerer := newDTLSRoleSession(t, testdata.ServerCertificate())
			answerer.DTLSConf.SDPSetupRole = func(bool) string { return setup }
			require.NoError(t, answerer.RemoteSDP(offerer.LocalSDP(false), false))
			answer := answerer.LocalSDP(true)
			require.NoError(t, offerer.RemoteSDP(answer, true))
			require.Contains(t, string(answer), "a=setup:"+setup)

			// Only the answerer runs its side; the offerer never does.
			done := make(chan error, 1)
			go func() { done <- answerer.Finalize() }()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.DeadlineExceeded)
			case <-time.After(10 * time.Second):
				t.Fatal("the handshake outlived DTLSHandshakeTimeout")
			}
		})
	}
}

// TestDTLSFinalizeContextEndsWithContext pins that the context FinalizeContext
// is given ends a DTLS handshake the peer never completes, well before
// DTLSHandshakeTimeout, and that FinalizePending follows the handshake: armed
// by RemoteSDP, and nothing left to run once FinalizeContext has returned.
// Both roles are covered, as in TestDTLSHandshakeTimeout.
func TestDTLSFinalizeContextEndsWithContext(t *testing.T) {
	for _, setup := range []string{"active", "passive"} {
		t.Run("answerer_"+setup, func(t *testing.T) {
			offerer := newDTLSRoleSession(t, testdata.ClientCertificate())
			answerer := newDTLSRoleSession(t, testdata.ServerCertificate())
			answerer.DTLSConf.SDPSetupRole = func(bool) string { return setup }
			require.False(t, answerer.FinalizePending(), "no SDP was applied yet")
			require.NoError(t, answerer.RemoteSDP(offerer.LocalSDP(false), false))
			require.True(t, answerer.FinalizePending(), "the offer armed a handshake")

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- answerer.FinalizeContext(ctx) }()
			if setup == "active" {
				// The ClientHello shows the handshake is running. Nobody
				// answers it.
				require.NoError(t, offerer.rtpConn.SetReadDeadline(time.Now().Add(10*time.Second)))
				n, _, err := offerer.rtpConn.ReadFrom(make([]byte, RTPBufSize))
				require.NoError(t, err, "the handshake never started")
				require.NotZero(t, n)
			}
			cancel()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.Canceled)
			case <-time.After(10 * time.Second):
				t.Fatal("the handshake outlived its context")
			}
			require.False(t, answerer.FinalizePending(), "the handshake was run")
		})
	}

	t.Run("plain_RTP_has_nothing_to_run", func(t *testing.T) {
		s := &MediaSession{Codecs: []Codec{CodecAudioUlaw}}
		require.False(t, s.FinalizePending())
		require.NoError(t, s.FinalizeContext(t.Context()))
	})
}
