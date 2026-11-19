// SPDX-License-Identifier: MPL-2.0

package media

import (
	"bytes"
	"errors"
	"io"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestICEMuxDemultiplexes asserts each packet class reaches its own stream and
// no other. This is the property the single socket ICE layout depends on: the
// DTLS handshake, media and control all arrive on the nominated pair.
func TestICEMuxDemultiplexes(t *testing.T) {
	local, remote := udpConnPair(t)

	mux := newWebRTCPacketMux(local)
	t.Cleanup(func() { _ = mux.Close() })

	dtlsPkt := []byte{22, 0xfe, 0xfd, 0x01, 0x02}
	rtcpPkt := []byte{0x80, 200, 0x00, 0x06, 0xde, 0xad}
	rtpPkt := []byte{0x80, 8, 0x00, 0x01, 0xbe, 0xef}

	_, err := remote.Write(dtlsPkt)
	require.NoError(t, err)
	assert.Equal(t, dtlsPkt, readWithTimeout(t, mux.dtls), "DTLS record must reach the DTLS stream")

	_, err = remote.Write(rtcpPkt)
	require.NoError(t, err)
	assert.Equal(t, rtcpPkt, readWithTimeout(t, mux.rtcp), "RTCP must reach the RTCP stream")

	_, err = remote.Write(rtpPkt)
	require.NoError(t, err)
	assert.Equal(t, rtpPkt, readWithTimeout(t, mux.rtp), "RTP must reach the RTP stream")

	// Each stream got exactly its own packet: nothing crossed over.
	for name, c := range map[string]*webRTCMuxEndpoint{"dtls": mux.dtls, "rtcp": mux.rtcp, "rtp": mux.rtp} {
		require.NoError(t, c.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
		buf := make([]byte, 1500)
		_, _, err := c.ReadFrom(buf)
		assert.Error(t, err, "%s stream should have no further packets", name)
	}
}

// TestICEMuxClassifiesByLeadingBytes pins which endpoint a packet reaches by
// its first two bytes, and that a packet of no known class reaches none.
//
// RFC 7983 section 7 assigns the first byte: 0-3 STUN, 16-19 ZRTP, 20-63 DTLS,
// 64-79 TURN channel, 128-191 RTP or RTCP, and a packet outside every range
// must be dropped. Of these the mux serves DTLS and RTP/RTCP: STUN is taken by
// the ICE agent before the mux reads, and ZRTP and TURN channel data have no
// consumer on a WebRTC session.
//
// Within 128-191 the second byte tells RTCP from RTP. It is the RTCP packet
// type, and RFC 5761 section 4 keeps the types in use within 192-223 by
// forbidding the RTP payload types 64-95, which with the marker bit set would
// read as 192-223. The mux compares the whole byte, so a second byte of 64-95,
// a forbidden payload type without the marker bit, is delivered as RTP.
func TestICEMuxClassifiesByLeadingBytes(t *testing.T) {
	const (
		dropped = ""
		toDTLS  = "dtls"
		toRTP   = "rtp"
		toRTCP  = "rtcp"
	)
	tests := []struct {
		name string
		pkt  []byte
		want string
	}{
		{"stun_lower_bound_0", []byte{0, 8, 0x01}, dropped},
		{"stun_upper_bound_3", []byte{3, 8, 0x01}, dropped},
		{"unassigned_4", []byte{4, 8, 0x01}, dropped},
		{"zrtp_lower_bound_16", []byte{16, 8, 0x01}, dropped},
		{"zrtp_upper_bound_19", []byte{19, 8, 0x01}, dropped},
		{"dtls_lower_bound_20", []byte{20, 8, 0x01}, toDTLS},
		{"dtls_handshake_22", []byte{22, 0xfe, 0xfd}, toDTLS},
		{"dtls_upper_bound_63", []byte{63, 8, 0x01}, toDTLS},
		{"dtls_single_byte", []byte{22}, toDTLS},
		{"turn_channel_lower_bound_64", []byte{64, 8, 0x01}, dropped},
		{"turn_channel_upper_bound_79", []byte{79, 8, 0x01}, dropped},
		{"unassigned_80", []byte{80, 8, 0x01}, dropped},
		{"unassigned_127", []byte{127, 8, 0x01}, dropped},
		{"rtp_lower_bound_128", []byte{128, 8, 0x01}, toRTP},
		{"rtp_upper_bound_191", []byte{191, 8, 0x01}, toRTP},
		{"rtcp_first_byte_191", []byte{191, 200, 0x01}, toRTCP},
		{"unassigned_192", []byte{192, 8, 0x01}, dropped},
		{"unassigned_192_with_rtcp_type", []byte{192, 200, 0x01}, dropped},
		{"unassigned_255", []byte{255, 8, 0x01}, dropped},

		{"rtp_payload_type_0", []byte{0x80, 0, 0x01}, toRTP},
		{"rtp_payload_type_8_with_marker", []byte{0x80, 0x80 | 8, 0x01}, toRTP},
		{"rtp_payload_type_63", []byte{0x80, 63, 0x01}, toRTP},
		{"rtp_forbidden_payload_type_64", []byte{0x80, 64, 0x01}, toRTP},
		{"rtp_forbidden_payload_type_72", []byte{0x80, 72, 0x01}, toRTP},
		{"rtp_forbidden_payload_type_95", []byte{0x80, 95, 0x01}, toRTP},
		{"rtp_dynamic_payload_type_96", []byte{0x80, 96, 0x01}, toRTP},
		{"rtp_payload_type_63_with_marker_191", []byte{0x80, 191, 0x01}, toRTP},
		{"rtcp_lower_bound_192", []byte{0x80, 192, 0x01}, toRTCP},
		{"rtcp_sender_report_200", []byte{0x80, 200, 0x01}, toRTCP},
		{"rtcp_upper_bound_223", []byte{0x80, 223, 0x01}, toRTCP},
		{"rtp_payload_type_96_with_marker_224", []byte{0x80, 224, 0x01}, toRTP},
		{"rtp_single_byte", []byte{0x80}, toRTP},
	}

	// One packet of each class follows the packet under test. The read loop
	// takes packets in the order the peer sent them, so whatever an endpoint
	// holds before its own marker is what the packet under test put there.
	markers := map[string][]byte{
		toDTLS: {22, 0xee, 0xee, 0xee},
		toRTP:  {0x80, 127, 0xee, 0xee},
		toRTCP: {0x80, 201, 0xee, 0xee},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			local, remote := udpConnPair(t)
			mux := newWebRTCPacketMux(local)
			t.Cleanup(func() { _ = mux.Close() })

			_, err := remote.Write(tc.pkt)
			require.NoError(t, err)
			for _, class := range []string{toDTLS, toRTP, toRTCP} {
				_, err = remote.Write(markers[class])
				require.NoError(t, err)
			}

			want := map[string][][]byte{}
			if tc.want != dropped {
				want[tc.want] = [][]byte{tc.pkt}
			}
			got := map[string][][]byte{}
			for class, endpoint := range map[string]*webRTCMuxEndpoint{toDTLS: mux.dtls, toRTP: mux.rtp, toRTCP: mux.rtcp} {
				for {
					pkt := readWithTimeout(t, endpoint)
					if bytes.Equal(pkt, markers[class]) {
						break
					}
					got[class] = append(got[class], pkt)
				}
			}
			assert.Equal(t, want, got, "packets delivered per endpoint for % x", tc.pkt)
		})
	}
}

// TestICEMuxReportsNominatedAddr asserts readers see the address ICE settled
// on: it is the only source the pair accepts, and the session names it when it
// discards a packet.
func TestICEMuxReportsNominatedAddr(t *testing.T) {
	local, remote := udpConnPair(t)
	raddr := remote.LocalAddr()

	mux := newWebRTCPacketMux(local)
	t.Cleanup(func() { _ = mux.Close() })

	_, err := remote.Write([]byte{0x80, 8, 0x00, 0x01})
	require.NoError(t, err)

	require.NoError(t, mux.rtp.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1500)
	_, addr, err := mux.rtp.ReadFrom(buf)
	require.NoError(t, err)
	assert.Equal(t, raddr.String(), addr.String())
}

// TestICEMuxWriteGoesToPair asserts every stream writes to the one ICE
// connection, since all three share the nominated pair.
func TestICEMuxWriteGoesToPair(t *testing.T) {
	local, remote := udpConnPair(t)

	mux := newWebRTCPacketMux(local)
	t.Cleanup(func() { _ = mux.Close() })

	for _, c := range []*webRTCMuxEndpoint{mux.dtls, mux.rtp, mux.rtcp} {
		_, err := c.WriteTo([]byte{0x01, 0x02, 0x03}, nil)
		require.NoError(t, err)

		require.NoError(t, remote.SetReadDeadline(time.Now().Add(2*time.Second)))
		buf := make([]byte, 1500)
		n, err := remote.Read(buf)
		require.NoError(t, err)
		assert.Equal(t, []byte{0x01, 0x02, 0x03}, buf[:n])
	}
}

// TestICEMuxCloseUnblocksReaders is the teardown guard.
//
// A reader parked in ReadFrom must be released by Close, otherwise session
// teardown blocks on it for the life of the process. An endpoint closed on its
// own before the mux, and the mux closed after that, must not block either.
func TestICEMuxCloseUnblocksReaders(t *testing.T) {
	local, _ := udpConnPair(t)

	mux := newWebRTCPacketMux(local)

	parked := make(chan error, 3)
	for _, c := range []*webRTCMuxEndpoint{mux.dtls, mux.rtp, mux.rtcp} {
		go func(c *webRTCMuxEndpoint) {
			buf := make([]byte, 1500)
			_, _, err := c.ReadFrom(buf)
			parked <- err
		}(c)
	}
	// Let all three readers reach their blocking read.
	time.Sleep(100 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		_ = mux.rtcp.Close()
		_ = mux.rtp.Close()
		_ = mux.Close()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("mux Close did not return: teardown is blocked on a parked reader")
	}

	for i := 0; i < 3; i++ {
		select {
		case err := <-parked:
			assert.Error(t, err, "parked reader must be released with an error")
		case <-time.After(2 * time.Second):
			t.Fatal("parked reader was not released by Close")
		}
	}
}

// TestICEMuxReadLoopStopsOnConnError asserts a dead connection terminates the
// read loop instead of spinning on it, and that consumers are unblocked.
func TestICEMuxReadLoopStopsOnConnError(t *testing.T) {
	local, _ := udpConnPair(t)
	mux := newWebRTCPacketMux(local)

	// Kill the connection under the mux without going through mux.Close.
	require.NoError(t, local.Close())

	// The read loop must notice and release consumers rather than spin.
	err := make(chan error, 1)
	go func() {
		buf := make([]byte, 1500)
		_, _, e := mux.rtp.ReadFrom(buf)
		err <- e
	}()

	select {
	case e := <-err:
		assert.Error(t, e)
	case <-time.After(2 * time.Second):
		t.Fatal("read loop did not release consumers after connection error")
	}

	// Close must still return cleanly after the loop already exited.
	done := make(chan struct{})
	go func() { _ = mux.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close blocked after read loop already exited")
	}
}

// scriptedMuxConn is a connection whose one Read parks until unblock is closed
// and then fails with readErr, and whose Close neither ends that Read nor
// succeeds: it counts its calls and returns closeErr. Beyond these the mux
// asks a connection only for the peer's address, on behalf of a reader, unless
// an endpoint is written.
type scriptedMuxConn struct {
	net.Conn

	unblock  chan struct{}
	readErr  error
	closeErr error

	readReturned chan struct{}
	closes       atomic.Int32
}

func newScriptedMuxConn(readErr, closeErr error) *scriptedMuxConn {
	return &scriptedMuxConn{
		unblock:      make(chan struct{}),
		readErr:      readErr,
		closeErr:     closeErr,
		readReturned: make(chan struct{}),
	}
}

func (c *scriptedMuxConn) Read([]byte) (int, error) {
	<-c.unblock
	close(c.readReturned)
	return 0, c.readErr
}

func (c *scriptedMuxConn) Close() error {
	c.closes.Add(1)
	return c.closeErr
}

func (c *scriptedMuxConn) RemoteAddr() net.Addr { return &net.UDPAddr{} }

// TestICEMuxCloseWaitsForReadLoop asserts Close returns only after the read
// loop has ended. The session closes the ICE agent under the connection right
// after the mux, so a loop still running then would read a connection its
// owner has already given up.
func TestICEMuxCloseWaitsForReadLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		conn := newScriptedMuxConn(net.ErrClosed, nil)
		mux := newWebRTCPacketMux(conn)

		closed := make(chan error, 1)
		go func() { closed <- mux.Close() }()

		// Close has closed the connection and every endpoint; the loop is still
		// parked in its read, which this connection's Close does not end.
		synctest.Wait()
		require.EqualValues(t, 1, conn.closes.Load(), "Close must close the connection")
		select {
		case err := <-closed:
			close(conn.unblock) // lets the loop end, so the failure is this one alone
			t.Fatalf("Close() = %v while the read loop was still reading the connection, want it to wait", err)
		default:
		}

		close(conn.unblock)
		require.NoError(t, <-closed)
		select {
		case <-conn.readReturned:
		default:
			t.Fatal("Close returned before the read loop's read had returned")
		}
		require.EqualValues(t, 1, conn.closes.Load(), "the connection is closed once, by Close")
	})
}

// TestICEMuxCloseReportsCloseErrorOfDeadConn asserts a connection that dies
// under the mux is closed by the read loop, which releases the readers, and
// that Close still reports what closing it returned although the loop made
// the close.
func TestICEMuxCloseReportsCloseErrorOfDeadConn(t *testing.T) {
	errRead := errors.New("connection reset")
	errClose := errors.New("close failed")
	conn := newScriptedMuxConn(errRead, errClose)
	mux := newWebRTCPacketMux(conn)

	close(conn.unblock)

	require.NoError(t, mux.rtp.SetReadDeadline(time.Now().Add(2*time.Second)))
	_, _, err := mux.rtp.ReadFrom(make([]byte, 1500))
	require.ErrorIs(t, err, io.EOF, "a reader must be released when the connection dies")

	require.ErrorIs(t, mux.Close(), errClose)
	require.EqualValues(t, 1, conn.closes.Load(), "the connection is closed once, by the read loop")
}
