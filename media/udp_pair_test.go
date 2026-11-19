// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// udpPeer presents a bound UDP socket as a net.Conn aimed at one peer. It
// keeps packet boundaries, which net.Pipe would not, and it never rebinds a
// port, which dialing both ends of a pair would.
type udpPeer struct {
	*net.UDPConn
	peer *net.UDPAddr
}

func (p *udpPeer) Read(b []byte) (int, error) {
	n, _, err := p.UDPConn.ReadFromUDP(b)
	return n, err
}

func (p *udpPeer) Write(b []byte) (int, error) {
	return p.UDPConn.WriteToUDP(b, p.peer)
}

func (p *udpPeer) RemoteAddr() net.Addr { return p.peer }

// udpConnPair returns two UDP sockets pointed at each other, standing in for
// the selected ICE pair. webRTCPacketMux only needs a net.Conn, so this
// exercises the real read loop over a real socket.
func udpConnPair(t *testing.T) (local net.Conn, remote net.Conn) {
	t.Helper()

	a, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)
	b, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0})
	require.NoError(t, err)

	t.Cleanup(func() {
		_ = a.Close()
		_ = b.Close()
	})

	return &udpPeer{UDPConn: a, peer: b.LocalAddr().(*net.UDPAddr)},
		&udpPeer{UDPConn: b, peer: a.LocalAddr().(*net.UDPAddr)}
}

func readWithTimeout(t *testing.T, c net.PacketConn) []byte {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(2*time.Second)))
	buf := make([]byte, 1500)
	n, _, err := c.ReadFrom(buf)
	require.NoError(t, err)
	return buf[:n]
}
