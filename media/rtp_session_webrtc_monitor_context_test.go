// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

// newPipeWebrtcRTPSession returns the RTP session of a WebRTC media session
// that a monitor can be started on, and the peer's end of the in-memory
// connection its packet mux reads.
func newPipeWebrtcRTPSession(t *testing.T) (*RTPSessionWebrtc, net.Conn) {
	t.Helper()

	local, peer := net.Pipe()
	t.Cleanup(func() {
		_ = local.Close()
		_ = peer.Close()
	})
	sess := newMuxedWebrtcSession(t, local)
	sess.mu.Lock()
	sess.ready = true
	sess.mu.Unlock()
	return NewRTPSessionWebrtc(sess), peer
}

// requireWebrtcMonitorsEnded requires every goroutine of the session's
// monitor to have returned. It is called in a synctest bubble, where the
// minute it allows passes only once nothing in the bubble can run.
func requireWebrtcMonitorsEnded(t *testing.T, rtpSess *RTPSessionWebrtc) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		rtpSess.monitorWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Minute):
		t.Fatal("RTCP monitors still running after their ctx ended")
	}
}

// TestRTPSessionWebrtcMonitorEndsWithCtx pins that the RTCP monitors of a
// WebRTC RTP session return when the ctx they run on ends, with no
// MonitorClose, whether ctx ends after they are started or had ended before,
// and that the session's transport stays open.
func TestRTPSessionWebrtcMonitorEndsWithCtx(t *testing.T) {
	tests := []struct {
		name             string
		endedBeforeStart bool
	}{
		{name: "ends_after_start"},
		{name: "ended_before_start", endedBeforeStart: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				rtpSess, peer := newPipeWebrtcRTPSession(t)

				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				if tc.endedBeforeStart {
					cancel()
				}
				require.NoError(t, rtpSess.MonitorBackgroundContext(ctx))
				cancel()

				requireWebrtcMonitorsEnded(t, rtpSess)

				// The stop is MonitorClose's, not the session's Close: the mux
				// still hands the peer's media to the session.
				raw, err := webrtcRTPPacket(1, "after the monitors").Marshal()
				require.NoError(t, err)
				_, err = peer.Write(raw)
				require.NoError(t, err, "the end of ctx closed the session's connection")
				n, _, err := rtpSess.Sess.mux.rtp.ReadFrom(make([]byte, RTPBufSize))
				require.NoError(t, err, "the end of ctx closed the session's packet mux")
				require.Equal(t, len(raw), n)
			})
		})
	}
}

// TestRTPSessionWebrtcMonitorCloseEndsCtxWatch pins that MonitorClose ends the
// goroutine that watches the monitors' ctx while that ctx is live. The
// session's RTCP endpoint outlives the monitors, and the reader of a later
// monitor on it clears the deadline that stopped the earlier one, so a ctx
// that ends after MonitorClose must not expire that deadline again.
func TestRTPSessionWebrtcMonitorCloseEndsCtxWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rtpSess, peer := newPipeWebrtcRTPSession(t)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, rtpSess.MonitorBackgroundContext(ctx))
		require.NoError(t, rtpSess.MonitorClose())

		// The next reader of the endpoint clears the deadline MonitorClose
		// expired, as a monitor's reader does when it starts.
		endpoint := rtpSess.Sess.mux.rtcp
		require.NoError(t, endpoint.SetReadDeadline(time.Time{}))
		cancel()
		// A goroutine still watching ctx has stopped the monitors once more
		// by now, which expires the deadline.
		synctest.Wait()

		raw := plainRTCP(t, 0x5eed0000)
		_, err := peer.Write(raw)
		require.NoError(t, err)
		n, _, err := endpoint.ReadFrom(make([]byte, RTPBufSize))
		require.NoError(t, err, "the end of ctx after MonitorClose expired the RTCP endpoint's deadline")
		require.Equal(t, len(raw), n)
	})
}

// TestRTPSessionWebrtcReaderStartedAfterStopReturns pins that an RTCP reader
// which starts only after the monitors were stopped returns. The stop is a
// passed read deadline, and the reader clears the deadline when it starts: one
// that cleared it after the stop read on with nothing left to stop it, and
// MonitorClose waited for it for as long as the session stayed open.
func TestRTPSessionWebrtcReaderStartedAfterStopReturns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rtpSess, _ := newPipeWebrtcRTPSession(t)
		require.NoError(t, rtpSess.Close())

		read := make(chan error, 1)
		go func() { read <- rtpSess.readRTCP() }()
		select {
		case err := <-read:
			var timeout net.Error
			require.ErrorAs(t, err, &timeout)
			require.True(t, timeout.Timeout(), "readRTCP() = %v, want the passed deadline", err)
		case <-time.After(time.Minute):
			t.Fatal("the RTCP reader cleared the deadline that stopped the monitors and reads on")
		}
	})
}

// TestRTPSessionWebrtcMonitorBackgroundContextStartErrors pins that
// MonitorBackgroundContext returns what MonitorBackground refuses a start
// with, and that a refused start leaves nothing for MonitorClose to wait for.
func TestRTPSessionWebrtcMonitorBackgroundContextStartErrors(t *testing.T) {
	t.Run("not_finalized", func(t *testing.T) {
		rtpSess := NewRTPSessionWebrtc(&MediaSessionWebrtc{})

		require.Error(t, rtpSess.MonitorBackgroundContext(t.Context()))
		rtpSess.monitorWG.Wait()
	})

	t.Run("closed", func(t *testing.T) {
		rtpSess, _ := newPipeWebrtcRTPSession(t)
		require.NoError(t, rtpSess.Close())

		require.ErrorIs(t, rtpSess.MonitorBackgroundContext(t.Context()), errRTPSessionClosed)
		rtpSess.monitorWG.Wait()
	})

	t.Run("already_started", func(t *testing.T) {
		rtpSess, _ := newPipeWebrtcRTPSession(t)
		require.NoError(t, rtpSess.MonitorBackgroundContext(t.Context()))

		require.ErrorIs(t, rtpSess.MonitorBackgroundContext(t.Context()), errRTPSessionMonitorStarted)
		// The refused start added no second watch of ctx: the one of the first
		// start is all MonitorClose ends and waits for.
		require.NoError(t, rtpSess.MonitorClose())
	})
}
