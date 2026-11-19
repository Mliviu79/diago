// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"

	"github.com/pion/ice/v4"
)

// errICESocketMissing is what Init reports, inside an ICEError, for an
// ICESocketSource that returned neither a socket nor an error.
var errICESocketMissing = errors.New("ice: the socket source returned no socket")

// An ICESocketSource returns the UDP socket one ICE generation of a WebRTC
// media session runs on. [MediaSessionWebrtc.Init] calls it once, with its
// ctx, and so does the Init of every session [MediaSessionWebrtc.Fork]
// starts: a fork is a new generation and takes a socket of its own while the
// session it was forked from still runs on its socket.
//
// The agent gathers its host candidates from that socket and from no other,
// so the port the source bound is the port of the host candidates and of the
// m= line, and the connectivity checks, the DTLS handshake, RTP and RTCP of
// the generation all travel over it. Which local addresses become host
// candidates is decided by the address the socket is bound to: the IP family,
// interface and IP filters of the configuration choose only among the server
// reflexive and relayed candidates, for which the agent goes on binding
// sockets itself, within PortMin and PortMax when those are set.
//
// The socket's LocalAddr must be a *net.UDPAddr. The session owns the socket
// from the moment the source returns it and closes it exactly once: in Close,
// also when no connectivity check ever ran, or in Init when Init fails. A
// source that has to know when the port is free again, to hand it back to
// whatever chose it, returns a socket whose Close does that after closing.
//
// An error of the source fails Init with an *ICEError that matches
// ErrICEGathering and wraps it. [ListenICESocket] binds a socket for a source
// to return.
type ICESocketSource func(ctx context.Context) (net.PacketConn, error)

// ListenICESocket binds the UDP socket of one ICE generation on laddr, for an
// [ICESocketSource] to return. The port laddr names is bound as it is. With
// port 0 a free port of RTPPortStart..RTPPortEnd is taken when that range is
// set, in the walk the RTP sessions of this package make through it, and a
// port the system chooses otherwise. A nil laddr is the unspecified address
// with port 0. laddr is left as it was: the address bound is the socket's
// LocalAddr.
//
// One socket is bound and no neighbour for RTCP: an ICE generation has one
// selected candidate pair, which carries both.
func ListenICESocket(laddr *net.UDPAddr) (*net.UDPConn, error) {
	var bind net.UDPAddr
	if laddr != nil {
		bind = *laddr
	}
	if bind.Port != 0 || RTPPortStart <= 0 || RTPPortEnd <= RTPPortStart {
		return net.ListenUDP("udp", &bind)
	}

	// The range is walked two ports at a time from where the last session
	// left it, as createListeners walks it, so that RTP sessions bound from
	// the same range keep finding an even port with its RTCP neighbour free.
	span := (RTPPortEnd - RTPPortStart + 1) &^ 1
	start := int(rtpPortOffset.Load()) % span
	var lastErr error
	for i := 0; i < span; i += 2 {
		bind.Port = RTPPortStart + (start+i)%span
		conn, err := net.ListenUDP("udp", &bind)
		if err != nil {
			lastErr = err
			continue
		}
		rtpPortOffset.Store(int32((bind.Port + 2 - RTPPortStart) % span))
		return conn, nil
	}
	return nil, fmt.Errorf("no available ports in range %d:%d: %w", RTPPortStart, RTPPortEnd, lastErr)
}

// iceSocket is the socket an ICESocketSource gave one ICE generation.
type iceSocket struct {
	// mux hands the socket to the agent as the transport of its host
	// candidates. It owns the socket: closing it closes the socket, once.
	mux *ice.UDPMuxDefault
	// addr is the address the socket is bound to.
	addr net.UDPAddr
}

// takeICESocket takes the socket of this ICE generation from conf.ICE.Socket,
// when there is one, and returns opts followed by the option that makes the
// agent gather its host candidates from it. Init calls it as its last step
// before it builds the agent.
//
// The session holds the socket from here on. Close releases it, and so does
// releaseUnusedICESocket for an Init that ends without an agent.
func (m *MediaSessionWebrtc) takeICESocket(ctx context.Context, opts []ice.AgentOption, conf MediaSessionWebrtcConfig) ([]ice.AgentOption, error) {
	if conf.ICE.Socket == nil {
		return opts, nil
	}
	conn, err := conf.ICE.Socket(ctx)
	if err != nil {
		return nil, fmt.Errorf("take the ICE socket: %w", err)
	}
	if conn == nil {
		return nil, errICESocketMissing
	}
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr == nil {
		// The ICE stack reads the socket as a UDP one and would gather
		// nothing from it.
		notUDP := fmt.Errorf("ice: the socket of the socket source is bound to %v, which is not a UDP address", conn.LocalAddr())
		return nil, errors.Join(notUDP, conn.Close())
	}

	sock := &iceSocket{
		mux:  ice.NewUDPMuxDefault(ice.UDPMuxParams{UDPConn: conn}),
		addr: net.UDPAddr{IP: slices.Clone(addr.IP), Port: addr.Port, Zone: addr.Zone},
	}
	m.mu.Lock()
	m.iceSocket = sock
	m.mu.Unlock()
	return append(opts, ice.WithUDPMux(sock.mux)), nil
}

// releaseUnusedICESocket closes the socket takeICESocket took when Init ends
// without having recorded an agent, and returns initErr, the error Init
// returns, joined by the error of that close when there is one. Nothing else
// would release the socket then: the session was never initialized, and a
// caller drops it without a Close. An Init that recorded its agent leaves the
// socket to Close, which its own failure paths have already called.
func (m *MediaSessionWebrtc) releaseUnusedICESocket(initErr error) error {
	m.mu.Lock()
	sock := m.iceSocket
	unused := m.agent == nil
	if unused {
		m.iceSocket = nil
	}
	m.mu.Unlock()
	if !unused || sock == nil {
		return initErr
	}
	if err := sock.mux.Close(); err != nil {
		return errors.Join(initErr, err)
	}
	return initErr
}

// closeICESocket closes the socket of this ICE generation, when the session
// was given one. Close calls it after it has closed the agent, whose
// candidates are conns of the mux. It closes the socket whether or not a
// connectivity check ever ran, and returns once the socket is closed.
func (m *MediaSessionWebrtc) closeICESocket() error {
	m.mu.Lock()
	sock := m.iceSocket
	m.mu.Unlock()
	if sock == nil {
		return nil
	}
	return sock.mux.Close()
}

// ICESocketAddr returns the address of the socket Config.ICE.Socket gave this
// ICE generation, whose port is the port of the session's host candidates and
// of its m= line, or nil for a session that was given no socket. It keeps
// returning the address after Close.
func (m *MediaSessionWebrtc) ICESocketAddr() *net.UDPAddr {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.iceSocket == nil {
		return nil
	}
	addr := m.iceSocket.addr
	return &net.UDPAddr{IP: slices.Clone(addr.IP), Port: addr.Port, Zone: addr.Zone}
}
