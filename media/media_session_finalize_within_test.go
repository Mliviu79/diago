// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/pion/rtcp"
	"github.com/stretchr/testify/require"
)

// TestDTLSFinalizeWithinLeavesHandshakeRunning pins that FinalizeWithin gives
// up waiting, not the handshake. A caller may ignore the answer in a 183 (RFC
// 3960), so the answerer of an early offer stops waiting for it, but the
// handshake has to go on: as the client (RFC 5763 section 6.2) it has already
// sent ClientHellos, and a peer whose DTLS stack reads them later cannot
// complete a new association in their place, while it completes this one at
// once. The handshake also outlives DTLSHandshakeTimeout until it is waited
// for, since the caller may take longer than that to answer.
func TestDTLSFinalizeWithinLeavesHandshakeRunning(t *testing.T) {
	prev := DTLSHandshakeTimeout
	DTLSHandshakeTimeout = time.Second
	t.Cleanup(func() { DTLSHandshakeTimeout = prev })

	for _, setup := range []string{"active", "passive"} {
		t.Run("answerer_"+setup, func(t *testing.T) {
			offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
			answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
			answerer.DTLSConf.SDPSetupRole = func(bool) string { return setup }
			_, answer := negotiateDTLS(t, offerer, answerer, nil)
			require.Contains(t, string(answer), "a=setup:"+setup)

			// The offerer ignores the answer for now and runs nothing.
			err := answerer.FinalizeWithin(t.Context(), 300*time.Millisecond)
			require.ErrorIs(t, err, ErrFinalizeInProgress)
			require.True(t, answerer.FinalizePending(), "the handshake must be left to wait for")
			require.ErrorIs(t, answerer.WriteRTP(dtlsTestPacket(1)), ErrDTLSNotKeyed)

			// Longer than DTLSHandshakeTimeout, which does not run until the
			// handshake is waited for.
			time.Sleep(1500 * time.Millisecond)

			errCh := make(chan error, 2)
			go func() { errCh <- offerer.Finalize() }()
			go func() { errCh <- answerer.FinalizeContext(t.Context()) }()
			for i := 0; i < 2; i++ {
				select {
				case err := <-errCh:
					require.NoError(t, err, "the handshake left running did not complete")
				case <-time.After(10 * time.Second):
					t.Fatal("the handshake left running did not return")
				}
			}
			require.False(t, answerer.FinalizePending())
			requireSRTPBetween(t, answerer, offerer, 10)
			requireSRTPBetween(t, offerer, answerer, 20)
		})
	}
}

// TestDTLSFinalizeWithinJoinIsBounded pins that a handshake left running is
// bounded by DTLSHandshakeTimeout once it is waited for, so a peer that never
// takes part does not hold the FinalizeContext that waits for it.
func TestDTLSFinalizeWithinJoinIsBounded(t *testing.T) {
	prev := DTLSHandshakeTimeout
	DTLSHandshakeTimeout = 300 * time.Millisecond
	t.Cleanup(func() { DTLSHandshakeTimeout = prev })

	offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
	negotiateDTLS(t, offerer, answerer, nil)

	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 100*time.Millisecond), ErrFinalizeInProgress)

	done := make(chan error, 1)
	go func() { done <- answerer.FinalizeContext(t.Context()) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(10 * time.Second):
		t.Fatal("waiting for the handshake outlived DTLSHandshakeTimeout")
	}
	require.False(t, answerer.FinalizePending())
	require.ErrorIs(t, answerer.WriteRTP(dtlsTestPacket(1)), ErrDTLSNotKeyed, "a failed handshake leaves no keys")
}

// TestDTLSFinalizeWithinEndsWithClose pins that Close ends a handshake left
// running and waits for it, so it does not outlive the session or run on the
// sockets Close releases.
func TestDTLSFinalizeWithinEndsWithClose(t *testing.T) {
	baseline := dtlsGoroutines()
	offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
	negotiateDTLS(t, offerer, answerer, nil)

	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 100*time.Millisecond), ErrFinalizeInProgress)

	closed := make(chan error, 1)
	go func() { closed <- answerer.Close() }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not end the handshake left running")
	}
	require.Eventually(t, func() bool {
		return dtlsGoroutines() <= baseline
	}, 5*time.Second, 10*time.Millisecond, "the handshake left running outlived Close")
}

// TestDTLSFinalizeWithinEndsWithContext pins that the context FinalizeWithin
// is given still ends the handshake, as it ends the call.
func TestDTLSFinalizeWithinEndsWithContext(t *testing.T) {
	offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
	negotiateDTLS(t, offerer, answerer, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.ErrorIs(t, answerer.FinalizeWithin(ctx, 100*time.Millisecond), ErrFinalizeInProgress)
	cancel()

	done := make(chan error, 1)
	go func() { done <- answerer.FinalizeContext(t.Context()) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("the handshake outlived the context it was started with")
	}
}

// TestDTLSFinalizeWithinFailureIsKept pins that a handshake left running that
// fails stays failed: every later FinalizeWithin and FinalizeContext reports
// its error, where they reported a session with nothing left to negotiate once
// the error had been returned. A FinalizeWithin that does not wait reports
// whether the handshake has ended, without waiting for it.
func TestDTLSFinalizeWithinFailureIsKept(t *testing.T) {
	offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
	negotiateDTLS(t, offerer, answerer, nil)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.ErrorIs(t, answerer.FinalizeWithin(ctx, 100*time.Millisecond), ErrFinalizeInProgress)
	require.ErrorIs(t, answerer.FinalizeWithin(ctx, 0), ErrFinalizeInProgress, "the handshake has not ended")

	cancel()
	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 10*time.Second), context.Canceled)
	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 0), context.Canceled)
	require.ErrorIs(t, answerer.FinalizeContext(t.Context()), context.Canceled)
	require.False(t, answerer.FinalizePending())
	require.ErrorIs(t, answerer.WriteRTP(dtlsTestPacket(1)), ErrDTLSNotKeyed)
}

// TestDTLSFinalizeWithinSendsNoMedia pins that a session whose handshake
// FinalizeWithin left running sends no media. Without keys WriteRTP and
// WriteRTCP would put it on the wire in plaintext, where the profile has none:
// SRTP processing does not start before the handshake completes (RFC 5764
// section 5.1), and media is protected solely with SRTP (section 4.1). Once
// the handshake has keyed the session, media goes out encrypted.
func TestDTLSFinalizeWithinSendsNoMedia(t *testing.T) {
	offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
	// As the DTLS server the answerer sends nothing of its own until the
	// offerer starts the handshake, so any datagram is the media refused below.
	answerer.DTLSConf.SDPSetupRole = func(bool) string { return "passive" }
	negotiateDTLS(t, offerer, answerer, nil)

	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 100*time.Millisecond), ErrFinalizeInProgress)
	require.ErrorIs(t, answerer.WriteRTP(dtlsTestPacket(1)), ErrDTLSNotKeyed)
	require.ErrorIs(t, answerer.WriteRTCP(&rtcp.ReceiverReport{SSRC: 1}), ErrDTLSNotKeyed)

	require.NoError(t, offerer.rtpConn.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
	_, _, err := offerer.rtpConn.ReadFrom(make([]byte, RTPBufSize))
	require.True(t, os.IsTimeout(err), "media went out before the handshake: %v", err)
	require.NoError(t, offerer.rtpConn.SetReadDeadline(time.Time{}))

	errCh := make(chan error, 2)
	go func() { errCh <- offerer.Finalize() }()
	go func() { errCh <- answerer.FinalizeContext(t.Context()) }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the handshake left running did not return")
		}
	}
	requireSRTPBetween(t, answerer, offerer, 10)
}

// TestDTLSFinalizeWithinResendsClientHelloWhenWaitedFor is a caller that
// ignores the answer in a 183 (RFC 3960) and discards our ClientHellos, as a
// NAT not open yet or a stack that drops DTLS before the 200 does, instead of
// queueing them. The handshake FinalizeWithin left running retransmits its
// ClientHello on the DTLS stack's timer, 1 s at first and doubling up to 60 s
// (RFC 6347 section 4.2.4.1): after three, the next is 4 s away, and after a
// minute of ringing up to a minute away, while FinalizeContext waits for the
// handshake only DTLSHandshakeTimeout from the answer. Once the call is
// answered the caller takes part, so FinalizeContext resends the ClientHello
// at once, and on the RFC's schedule until the caller answers it.
func TestDTLSFinalizeWithinResendsClientHelloWhenWaitedFor(t *testing.T) {
	prev := DTLSHandshakeTimeout
	DTLSHandshakeTimeout = 2 * time.Second
	t.Cleanup(func() { DTLSHandshakeTimeout = prev })

	offerer := newDTLSForkTestSession(t, testdata.ClientCertificate())
	answerer := newDTLSForkTestSession(t, testdata.ServerCertificate())
	answerer.DTLSConf.SDPSetupRole = func(bool) string { return "active" }
	negotiateDTLS(t, offerer, answerer, nil)

	// The caller discards what reaches it until it answers.
	hellos := make(chan struct{}, 16)
	stopDiscard := make(chan struct{})
	discarded := make(chan struct{})
	go func() {
		defer close(discarded)
		buf := make([]byte, RTPBufSize)
		for {
			select {
			case <-stopDiscard:
				return
			default:
			}
			_ = offerer.rtpConn.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			if _, _, err := offerer.rtpConn.ReadFrom(buf); err == nil {
				hellos <- struct{}{}
			}
		}
	}()

	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 100*time.Millisecond), ErrFinalizeInProgress)
	// ClientHellos at 0, 1 and 3 s are discarded; the DTLS stack sends the
	// next at 7 s.
	for i := 0; i < 3; i++ {
		select {
		case <-hellos:
		case <-time.After(10 * time.Second):
			t.Fatalf("ClientHello %d was not sent", i+1)
		}
	}
	close(stopDiscard)
	<-discarded
	require.NoError(t, offerer.rtpConn.SetReadDeadline(time.Time{}))

	// The call is answered: the caller runs its DTLS server now.
	answered := time.Now()
	errCh := make(chan error, 2)
	go func() { errCh <- offerer.Finalize() }()
	go func() { errCh <- answerer.FinalizeContext(t.Context()) }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err, "the handshake did not complete within DTLSHandshakeTimeout of the answer")
		case <-time.After(10 * time.Second):
			t.Fatal("the handshake did not return")
		}
	}
	require.Less(t, time.Since(answered), time.Second, "the ClientHello waited for the DTLS stack's retransmission timer")
	requireSRTPBetween(t, answerer, offerer, 10)
	requireSRTPBetween(t, offerer, answerer, 20)
}
