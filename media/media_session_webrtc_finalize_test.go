// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// setNegotiationCaps sets ICEConnectTimeout and DTLSHandshakeTimeout for the
// test and puts them back when it ends.
func setNegotiationCaps(t *testing.T, iceConnect, dtlsHandshake time.Duration) {
	t.Helper()
	prevICE, prevDTLS := ICEConnectTimeout, DTLSHandshakeTimeout
	ICEConnectTimeout, DTLSHandshakeTimeout = iceConnect, dtlsHandshake
	t.Cleanup(func() { ICEConnectTimeout, DTLSHandshakeTimeout = prevICE, prevDTLS })
}

// newNegotiatedWebrtcPair returns two WebRTC sessions on the loopback
// interface that have exchanged an offer and an answer, the answerer writing
// a=setup:answererSetup, or its default when that is empty. Both agents run
// their connectivity checks from here on; neither session has run its DTLS
// handshake, which waits for Finalize or FinalizeWithin.
func newNegotiatedWebrtcPair(t *testing.T, answererSetup string) (offerer, answerer *MediaSessionWebrtc) {
	t.Helper()
	ctx := t.Context()
	var answererRole func(bool) string
	if answererSetup != "" {
		answererRole = func(bool) string { return answererSetup }
	}
	offerer = newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer = newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), answererRole)

	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	if answererSetup != "" {
		require.Equal(t, "a=setup:"+answererSetup, sdpSetupLine(t, answer))
	}
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))
	return offerer, answerer
}

// requireWebrtcSRTPBetween writes one packet from one session and requires the
// other to read and decrypt it.
func requireWebrtcSRTPBetween(t *testing.T, from, to *MediaSessionWebrtc, seq uint16) {
	t.Helper()
	pkt := dtlsTestPacket(seq)
	require.NoError(t, from.WriteRTP(pkt))
	require.NoError(t, to.StopRTP(1, 5*time.Second))
	got := rtp.Packet{}
	_, err := to.ReadRTP(make([]byte, RTPBufSize), &got)
	require.NoError(t, err, "the media did not arrive and decrypt")
	require.NoError(t, to.StartRTP(1))
	require.Equal(t, seq, got.SequenceNumber)
	require.Equal(t, pkt.Payload, got.Payload)
}

// requireBothFinalize runs Finalize on both sessions at once and requires both
// to complete: each side waits for the other.
func requireBothFinalize(t *testing.T, offerer, answerer *MediaSessionWebrtc, wait time.Duration, msg string) {
	t.Helper()
	errCh := make(chan error, 2)
	go func() { errCh <- offerer.Finalize(t.Context()) }()
	go func() { errCh <- answerer.Finalize(t.Context()) }()
	for range 2 {
		select {
		case err := <-errCh:
			require.NoError(t, err, msg)
		case <-time.After(wait):
			t.Fatal("the negotiation did not return: " + msg)
		}
	}
}

// TestDTLSWebrtcHandshakeTimeout pins that a DTLS handshake the peer never
// completes ends after DTLSHandshakeTimeout, whatever context Finalize was
// given. The DTLS stack retransmits its flights for as long as its context
// lives, so a peer that completes the connectivity checks and never answers
// the handshake parked Finalize for good. Both roles are covered: the client
// sends a ClientHello nobody answers, and the server waits for one that never
// comes.
func TestDTLSWebrtcHandshakeTimeout(t *testing.T) {
	setNegotiationCaps(t, ICEConnectTimeout, 300*time.Millisecond)

	for _, setup := range []string{"active", "passive"} {
		t.Run("answerer_"+setup, func(t *testing.T) {
			_, answerer := newNegotiatedWebrtcPair(t, setup)

			// Only the answerer runs its side; the offerer's agent answers
			// the connectivity checks and its DTLS stack never runs.
			done := make(chan error, 1)
			go func() { done <- answerer.Finalize(t.Context()) }()
			select {
			case err := <-done:
				require.ErrorIs(t, err, context.DeadlineExceeded)
				require.NotErrorIs(t, err, ErrICEConnection, "the connectivity checks were to complete")
			case <-time.After(10 * time.Second):
				t.Fatal("the handshake outlived DTLSHandshakeTimeout")
			}
		})
	}
}

// TestICEWebrtcConnectTimeout pins that connectivity checks no peer answers
// stop holding Finalize after ICEConnectTimeout, whatever context it was
// given: the agent goes on checking for as long as it lives.
func TestICEWebrtcConnectTimeout(t *testing.T) {
	setNegotiationCaps(t, 300*time.Millisecond, DTLSHandshakeTimeout)

	ctx := t.Context()
	offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	// The offerer is gone: nothing answers a check sent to its candidates.
	require.NoError(t, offerer.Close())

	done := make(chan error, 1)
	go func() { done <- answerer.Finalize(ctx) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, ErrICEConnection)
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(10 * time.Second):
		t.Fatal("the connectivity checks outlived ICEConnectTimeout")
	}
	require.True(t, answerer.FinalizePending(), "the agent still checks, and Finalize may wait for it again")
}

// TestDTLSWebrtcFinalizePending pins what FinalizePending reports of a session
// that leaves no negotiation running: nothing before a remote description is
// applied, a negotiation to run from then until Finalize has completed it, and
// nothing afterwards. FinalizeWithin answers a session with nothing to wait
// for the peer in as Finalize does.
func TestDTLSWebrtcFinalizePending(t *testing.T) {
	ctx := t.Context()
	offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
	require.False(t, answerer.FinalizePending(), "no SDP was applied yet")
	unconfigured := answerer.Finalize(ctx)
	require.Error(t, unconfigured, "no remote description to negotiate with")
	require.EqualError(t, answerer.FinalizeWithin(ctx, time.Second), unconfigured.Error())
	require.EqualError(t, answerer.FinalizeWithin(ctx, 0), unconfigured.Error())
	require.False(t, answerer.FinalizePending())

	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	require.True(t, answerer.FinalizePending(), "the offer started a negotiation")
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))
	require.True(t, offerer.FinalizePending(), "the answer started a negotiation")

	requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE + DTLS negotiation")
	require.False(t, offerer.FinalizePending())
	require.False(t, answerer.FinalizePending())
	require.NoError(t, answerer.FinalizeWithin(ctx, 0), "a negotiated session has nothing to wait for")
	require.NoError(t, answerer.WriteRTP(dtlsTestPacket(1)), "nothing was left running, so nothing refuses media")

	require.NoError(t, answerer.Close())
	require.ErrorIs(t, answerer.FinalizeWithin(ctx, time.Second), errWebRTCSessionClosed)
}

// TestDTLSWebrtcFinalizeWithinLeavesHandshakeRunning pins that FinalizeWithin
// gives up waiting, not the handshake. A caller may ignore the answer in a 183
// (RFC 3960), so the answerer of an early offer stops waiting for it, but the
// handshake has to go on: as the client (RFC 5763 section 6.2) it has already
// sent ClientHellos, and a peer whose DTLS stack reads them later cannot
// complete a new association in their place, while it completes this one at
// once. The handshake also outlives DTLSHandshakeTimeout until it is waited
// for, since the caller may take longer than that to answer.
func TestDTLSWebrtcFinalizeWithinLeavesHandshakeRunning(t *testing.T) {
	setNegotiationCaps(t, ICEConnectTimeout, time.Second)

	for _, setup := range []string{"active", "passive"} {
		t.Run("answerer_"+setup, func(t *testing.T) {
			offerer, answerer := newNegotiatedWebrtcPair(t, setup)

			// The offerer ignores the answer for now and runs no handshake.
			err := answerer.FinalizeWithin(t.Context(), 300*time.Millisecond)
			require.ErrorIs(t, err, ErrFinalizeInProgress)
			require.True(t, answerer.FinalizePending(), "the handshake must be left to wait for")
			require.ErrorIs(t, answerer.WriteRTP(dtlsTestPacket(1)), ErrDTLSNotKeyed)

			// Longer than DTLSHandshakeTimeout, which does not run until the
			// handshake is waited for.
			time.Sleep(1500 * time.Millisecond)

			requireBothFinalize(t, offerer, answerer, 10*time.Second, "the handshake left running did not complete")
			require.False(t, answerer.FinalizePending())
			requireWebrtcSRTPBetween(t, answerer, offerer, 10)
			requireWebrtcSRTPBetween(t, offerer, answerer, 20)
		})
	}
}

// TestDTLSWebrtcFinalizeWithinJoinIsBounded pins that a negotiation left
// running is bounded by ICEConnectTimeout plus DTLSHandshakeTimeout once it is
// waited for, so a peer that never takes part does not hold the Finalize that
// waits for it.
func TestDTLSWebrtcFinalizeWithinJoinIsBounded(t *testing.T) {
	setNegotiationCaps(t, 300*time.Millisecond, 300*time.Millisecond)

	_, answerer := newNegotiatedWebrtcPair(t, "")

	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 100*time.Millisecond), ErrFinalizeInProgress)

	done := make(chan error, 1)
	go func() { done <- answerer.Finalize(t.Context()) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.DeadlineExceeded)
	case <-time.After(10 * time.Second):
		t.Fatal("waiting for the handshake outlived its bounds")
	}
	require.False(t, answerer.FinalizePending())
	require.ErrorIs(t, answerer.WriteRTP(dtlsTestPacket(1)), ErrDTLSNotKeyed, "a failed handshake leaves no keys")
}

// TestDTLSWebrtcFinalizeWithinEndsWithClose pins that Close ends a handshake
// left running and waits for it, so it does not outlive the session or run on
// the mux and the agent Close releases.
func TestDTLSWebrtcFinalizeWithinEndsWithClose(t *testing.T) {
	baseline := dtlsGoroutines()
	offerer, answerer := newNegotiatedWebrtcPair(t, "active")

	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 100*time.Millisecond), ErrFinalizeInProgress)
	// The ClientHello shows the handshake is running. Nobody answers it.
	require.NoError(t, offerer.mux.dtls.SetReadDeadline(time.Now().Add(10*time.Second)))
	n, _, err := offerer.mux.dtls.ReadFrom(make([]byte, RTPBufSize))
	require.NoError(t, err, "the handshake never started")
	require.NotZero(t, n)

	run, _ := answerer.leftRunning.current()
	require.NotNil(t, run)

	closed := make(chan error, 1)
	go func() { closed <- answerer.Close() }()
	select {
	case <-closed:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not end the handshake left running")
	}
	select {
	case <-run.done:
	default:
		t.Fatal("Close returned before the handshake left running did")
	}
	// Close ended it, ahead of the mux and the agent it then closed: the
	// handshake did not end on a transport taken from under it.
	_, kept := answerer.leftRunning.current()
	require.ErrorIs(t, kept, context.Canceled)
	require.Eventually(t, func() bool {
		return dtlsGoroutines() <= baseline
	}, 5*time.Second, 10*time.Millisecond, "the handshake left running outlived Close")
}

// TestDTLSWebrtcFinalizeWithinEndsWithContext pins that the context
// FinalizeWithin is given still ends the negotiation, as it ends the call.
func TestDTLSWebrtcFinalizeWithinEndsWithContext(t *testing.T) {
	_, answerer := newNegotiatedWebrtcPair(t, "")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.ErrorIs(t, answerer.FinalizeWithin(ctx, 100*time.Millisecond), ErrFinalizeInProgress)
	cancel()

	done := make(chan error, 1)
	go func() { done <- answerer.Finalize(t.Context()) }()
	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(10 * time.Second):
		t.Fatal("the handshake outlived the context it was started with")
	}
}

// TestDTLSWebrtcFinalizeWithinFailureIsKept pins that a negotiation left
// running that fails stays failed: every later FinalizeWithin and Finalize
// reports its error. A FinalizeWithin that does not wait reports whether the
// negotiation has ended, without waiting for it.
func TestDTLSWebrtcFinalizeWithinFailureIsKept(t *testing.T) {
	_, answerer := newNegotiatedWebrtcPair(t, "")

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	require.ErrorIs(t, answerer.FinalizeWithin(ctx, 100*time.Millisecond), ErrFinalizeInProgress)
	require.ErrorIs(t, answerer.FinalizeWithin(ctx, 0), ErrFinalizeInProgress, "the handshake has not ended")

	cancel()
	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 10*time.Second), context.Canceled)
	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 0), context.Canceled)
	require.ErrorIs(t, answerer.Finalize(t.Context()), context.Canceled)
	require.False(t, answerer.FinalizePending())
	require.ErrorIs(t, answerer.WriteRTP(dtlsTestPacket(1)), ErrDTLSNotKeyed)
}

// TestDTLSWebrtcFinalizeWithinSendsNoMedia pins that a session whose handshake
// FinalizeWithin left running sends no media: SRTP processing does not start
// before the handshake completes (RFC 5764 section 5.1), and media is
// protected solely with SRTP (section 4.1). Once the handshake has keyed the
// session, media goes out encrypted.
func TestDTLSWebrtcFinalizeWithinSendsNoMedia(t *testing.T) {
	// As the DTLS server the answerer sends nothing of its own until the
	// offerer starts the handshake, so any datagram is the media refused below.
	offerer, answerer := newNegotiatedWebrtcPair(t, "passive")

	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 100*time.Millisecond), ErrFinalizeInProgress)
	require.ErrorIs(t, answerer.WriteRTP(dtlsTestPacket(1)), ErrDTLSNotKeyed)
	require.ErrorIs(t, answerer.WriteRTCP(&rtcp.ReceiverReport{SSRC: 1}), ErrDTLSNotKeyed)

	for name, endpoint := range map[string]*webRTCMuxEndpoint{"RTP": offerer.mux.rtp, "RTCP": offerer.mux.rtcp} {
		require.NoError(t, endpoint.SetReadDeadline(time.Now().Add(300*time.Millisecond)))
		_, _, err := endpoint.ReadFrom(make([]byte, RTPBufSize))
		require.True(t, os.IsTimeout(err), "%s went out before the handshake: %v", name, err)
		require.NoError(t, endpoint.SetReadDeadline(time.Time{}))
	}

	requireBothFinalize(t, offerer, answerer, 10*time.Second, "the handshake left running did not complete")
	requireWebrtcSRTPBetween(t, answerer, offerer, 10)
}

// TestDTLSWebrtcFinalizeWithinResendsClientHelloWhenWaitedFor is a caller that
// ignores the answer in a 183 (RFC 3960) and discards our ClientHellos, as a
// stack that drops DTLS before the 200 does, instead of queueing them. The
// handshake FinalizeWithin left running retransmits its ClientHello on the
// DTLS stack's timer, 1 s at first and doubling up to 60 s (RFC 6347 section
// 4.2.4.1): after three, the next is 4 s away, and after a minute of ringing
// up to a minute away, while Finalize waits for the handshake only
// DTLSHandshakeTimeout from the answer. Once the call is answered the caller
// takes part, so Finalize resends the ClientHello at once, and on the RFC's
// schedule until the caller answers it.
func TestDTLSWebrtcFinalizeWithinResendsClientHelloWhenWaitedFor(t *testing.T) {
	setNegotiationCaps(t, ICEConnectTimeout, 2*time.Second)

	offerer, answerer := newNegotiatedWebrtcPair(t, "active")

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
			_ = offerer.mux.dtls.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			if _, _, err := offerer.mux.dtls.ReadFrom(buf); err == nil {
				hellos <- struct{}{}
			}
		}
	}()

	require.ErrorIs(t, answerer.FinalizeWithin(t.Context(), 100*time.Millisecond), ErrFinalizeInProgress)
	// ClientHellos at 0, 1 and 3 s are discarded; the DTLS stack sends the
	// next at 7 s.
	for i := range 3 {
		select {
		case <-hellos:
		case <-time.After(10 * time.Second):
			t.Fatalf("ClientHello %d was not sent", i+1)
		}
	}
	close(stopDiscard)
	<-discarded
	require.NoError(t, offerer.mux.dtls.SetReadDeadline(time.Time{}))

	// The call is answered: the caller runs its DTLS server now.
	answered := time.Now()
	requireBothFinalize(t, offerer, answerer, 10*time.Second, "the handshake did not complete within DTLSHandshakeTimeout of the answer")
	require.Less(t, time.Since(answered), time.Second, "the ClientHello waited for the DTLS stack's retransmission timer")
	requireWebrtcSRTPBetween(t, answerer, offerer, 10)
	requireWebrtcSRTPBetween(t, offerer, answerer, 20)
}

// TestICEWebrtcFinalizeWithinContinuesChecks pins that FinalizeWithin gives up
// waiting for the connectivity checks, not the checks: an answer to an early
// offer that stopped them could not complete once the caller takes part after
// the 200.
func TestICEWebrtcFinalizeWithinContinuesChecks(t *testing.T) {
	ctx := t.Context()
	offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)

	// The offerer, the controlling agent, has not been given the answer: it
	// starts nothing yet and answers no check.
	require.ErrorIs(t, answerer.FinalizeWithin(ctx, 300*time.Millisecond), ErrFinalizeInProgress)

	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))
	requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE + DTLS negotiation left running did not complete")

	requireWebrtcSRTPBetween(t, answerer, offerer, 1)
}

// TestDTLSWebrtcFinalizeCommitsUpdateStagedAfterLeftRunning pins that the
// Finalize that waits for a negotiation left running also commits a
// subsequent description staged since that negotiation ended, as it does on a
// session it negotiated itself: nobody had waited for the negotiation yet, and
// the update is not to wait for a second Finalize.
func TestDTLSWebrtcFinalizeCommitsUpdateStagedAfterLeftRunning(t *testing.T) {
	ctx := t.Context()
	offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
	answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))

	require.ErrorIs(t, answerer.FinalizeWithin(ctx, 0), ErrFinalizeInProgress)
	run, _ := answerer.leftRunning.current()
	require.NotNil(t, run)
	require.NoError(t, offerer.Finalize(ctx))
	select {
	case <-run.done:
	case <-time.After(40 * time.Second):
		t.Fatal("the negotiation left running did not complete")
	}

	// The same ICE generation and DTLS association, offered send-only.
	require.Contains(t, string(offer), "a=sendrecv\r\n")
	hold := strings.Replace(string(offer), "a=sendrecv\r\n", "a=sendonly\r\n", 1)
	require.NoError(t, answerer.RemoteSDP(ctx, []byte(hold), false))
	require.NoError(t, answerer.Finalize(ctx))
	require.False(t, answerer.FinalizePending())
	require.Equal(t, sdp.ModeRecvonly, answerer.Mode)
}
