// SPDX-License-Identifier: MPL-2.0

package media

import (
	"fmt"
	"io"
	"net"
	"sync/atomic"
)

// dtlsKeyExchangeConn lends a media transport to the DTLS stack.
//
// RFC 5764 section 5.1.2 multiplexes the handshake onto the transport that
// carries media, so the transport belongs to the media session and merely
// serves DTLS for the length of the exchange. The DTLS stack does not know
// that: every one of its teardown paths, whether a fatal alert, a handshake
// timeout or a peer close_notify, ends in a Close of its underlying conn. On a
// session without ICE that conn is the RTP socket, so an alert on the handshake
// would silence the call's media for good. A WebRTC session lends the DTLS
// stream of its packet mux the same way.
//
// DTLS here is only a key exchange: the association carries no application
// data, and nothing reads or writes it once the keying material is exported.
// Retiring it is therefore a local act, and detach makes that explicit.
type dtlsKeyExchangeConn struct {
	net.PacketConn
	detached atomic.Bool
}

func newDTLSKeyExchangeConn(conn net.PacketConn) *dtlsKeyExchangeConn {
	return &dtlsKeyExchangeConn{PacketConn: conn}
}

// detach retires the transport. Reads report EOF, which the DTLS read loop
// treats as a clean end, and writes are dropped instead of reaching the peer.
// It must be called before the conn is closed, so that the close_notify that
// Close emits is dropped with them: the peer's DTLS association is still live
// and is what its SRTP keys hang off, so telling it we are gone would fail the
// call. Retiring is invisible on the wire by design.
func (c *dtlsKeyExchangeConn) detach() {
	c.detached.Store(true)
}

func (c *dtlsKeyExchangeConn) ReadFrom(p []byte) (int, net.Addr, error) {
	if c.detached.Load() {
		return 0, nil, io.EOF
	}
	return c.PacketConn.ReadFrom(p)
}

func (c *dtlsKeyExchangeConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c.detached.Load() {
		return len(p), nil
	}
	return c.PacketConn.WriteTo(p, addr)
}

// Close is deliberately a no-op. The transport outlives the DTLS association
// and is closed by the media session, which is what owns it.
func (c *dtlsKeyExchangeConn) Close() error { return nil }

// retire ends the DTLS stack conn that ran its handshake on this transport,
// without a word to the peer, and takes the transport back from it.
//
// DTLS-SRTP uses the association for nothing but the key exchange. Left alone
// the stack keeps a read loop and a handshake goroutine parked on the
// transport for the life of the call, and where the transport is the socket
// SRTP arrives on, the read loop consumes media, which it then discards as a
// malformed record per RFC 6347 section 4.1.2.7, so the loss is silent.
//
// detach comes first, so that the close_notify Close emits is dropped rather
// than sent: the peer keeps its association. Close is then what retires the
// read loop and the handshake goroutine, and it cannot reach the transport
// because detach has already taken it away.
func (c *dtlsKeyExchangeConn) retire(conn io.Closer) error {
	c.detach()
	if err := conn.Close(); err != nil {
		return fmt.Errorf("dtls conn close: %w", err)
	}
	return nil
}
