// SPDX-License-Identifier: MPL-2.0

package media

import (
	"fmt"
	"io"
	"net"
	"sync"
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
//
// It also keeps the last ClientHello the DTLS stack sent while nothing has been
// heard from the peer, so it can be sent again: see resendHello.
type dtlsKeyExchangeConn struct {
	net.PacketConn
	detached atomic.Bool

	// mu guards hello, helloAddr and heard. hello is the last ClientHello
	// sent, to helloAddr, and heard is set once a DTLS record has arrived from
	// the peer, after which the DTLS stack's retransmissions answer for it.
	mu        sync.Mutex
	hello     []byte
	helloAddr net.Addr
	heard     bool
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
	n, addr, err := c.PacketConn.ReadFrom(p)
	// A DTLS record by its first byte (RFC 7983 section 7), as against media
	// the peer sends on the same socket.
	if n > 0 && p[0] >= 20 && p[0] <= 63 {
		c.mu.Lock()
		c.heard = true
		c.hello = nil
		c.mu.Unlock()
	}
	return n, addr, err
}

func (c *dtlsKeyExchangeConn) WriteTo(p []byte, addr net.Addr) (int, error) {
	if c.detached.Load() {
		return len(p), nil
	}
	n, err := c.PacketConn.WriteTo(p, addr)
	if err == nil && isClientHelloDatagram(p) {
		c.mu.Lock()
		if !c.heard {
			c.hello = append(c.hello[:0], p...)
			c.helloAddr = addr
		}
		c.mu.Unlock()
	}
	return n, err
}

// resendHello sends the last ClientHello again, as it was sent, while nothing
// has been heard from the peer. It is a retransmission of the client's first
// flight (RFC 6347 section 4.2.4). A peer that received it already takes the
// copy as one, or discards it as a replayed record (section 3.3).
func (c *dtlsKeyExchangeConn) resendHello() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.heard || c.hello == nil || c.detached.Load() {
		return nil
	}
	_, err := c.PacketConn.WriteTo(c.hello, c.helloAddr)
	return err
}

// isClientHelloDatagram reports whether p is a datagram holding one DTLS
// record, in epoch 0, that carries a whole ClientHello (RFC 6347 sections 4.1
// and 4.2.2), which a client may send again as it is.
func isClientHelloDatagram(p []byte) bool {
	const recordHeaderLen, handshakeHeaderLen = 13, 12
	const contentTypeHandshake, handshakeTypeClientHello = 22, 1
	if len(p) < recordHeaderLen+handshakeHeaderLen || p[0] != contentTypeHandshake || p[3] != 0 || p[4] != 0 {
		return false
	}
	recordLen := int(p[11])<<8 | int(p[12])
	if recordHeaderLen+recordLen != len(p) {
		return false
	}
	h := p[recordHeaderLen:]
	if h[0] != handshakeTypeClientHello {
		return false
	}
	msgLen := int(h[1])<<16 | int(h[2])<<8 | int(h[3])
	fragOffset := int(h[6])<<16 | int(h[7])<<8 | int(h[8])
	fragLen := int(h[9])<<16 | int(h[10])<<8 | int(h[11])
	return fragOffset == 0 && fragLen == msgLen && handshakeHeaderLen+msgLen == recordLen
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
