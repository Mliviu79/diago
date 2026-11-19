// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/stretchr/testify/require"
)

// TestRTPSessionMonitorEndsWithCtx pins that the RTCP monitors return when the
// ctx they run on ends, with no MonitorClose and while the sockets stay open.
func TestRTPSessionMonitorEndsWithCtx(t *testing.T) {
	rtpConn, rtpPeer := net.Pipe()
	rtcpConn, rtcpPeer := net.Pipe()
	t.Cleanup(func() {
		_ = rtpConn.Close()
		_ = rtpPeer.Close()
		_ = rtcpConn.Close()
		_ = rtcpPeer.Close()
	})

	sess := &MediaSession{
		Codecs:    []Codec{CodecAudioUlaw},
		Mode:      sdp.ModeSendrecv,
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
		Raddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9876},
		rtcpRaddr: net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9877},
		rtpConn:   &pipePacketConn{Conn: rtpConn},
		rtcpConn:  &pipePacketConn{Conn: rtcpConn},
	}
	rtpSess := NewRTPSession(sess)

	ctx, cancel := context.WithCancel(context.Background())
	require.NoError(t, rtpSess.MonitorBackgroundContext(ctx))
	cancel()

	done := make(chan struct{})
	go func() {
		rtpSess.monitorWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RTCP monitors still running after their ctx ended")
	}
}

// TestRTPSessionMonitorCloseEndsCtxWatch pins that MonitorClose ends the
// goroutine that watches the monitors' ctx while that ctx is live. The RTCP
// connection outlives the monitors, for the monitor of a fork, so a ctx that
// ends after MonitorClose must not expire the connection's deadline again.
func TestRTPSessionMonitorCloseEndsCtxWatch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		rtpConn, rtpPeer := net.Pipe()
		rtcpConn, rtcpPeer := net.Pipe()
		t.Cleanup(func() {
			_ = rtpConn.Close()
			_ = rtpPeer.Close()
			_ = rtcpConn.Close()
			_ = rtcpPeer.Close()
		})

		sess := &MediaSession{
			Codecs:    []Codec{CodecAudioUlaw},
			Mode:      sdp.ModeSendrecv,
			Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
			Raddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9876},
			rtcpRaddr: net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 9877},
			rtpConn:   &pipePacketConn{Conn: rtpConn},
			rtcpConn:  &pipePacketConn{Conn: rtcpConn},
		}
		rtpSess := NewRTPSession(sess)

		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		require.NoError(t, rtpSess.MonitorBackgroundContext(ctx))
		require.NoError(t, rtpSess.MonitorClose())

		// The next monitor on the connection clears the deadline MonitorClose
		// expired, as startMonitor does for a fork.
		require.NoError(t, rtcpConn.SetDeadline(time.Time{}))
		cancel()
		// A goroutine still watching ctx has stopped the monitors once more
		// by now, which expires the deadline.
		synctest.Wait()

		wrote := make(chan error, 1)
		go func() {
			_, err := rtcpPeer.Write([]byte{1})
			wrote <- err
		}()
		n, err := rtcpConn.Read(make([]byte, 1))
		require.NoError(t, err, "the end of ctx after MonitorClose expired the RTCP connection's deadline")
		require.Equal(t, 1, n)
		require.NoError(t, <-wrote)
	})
}
