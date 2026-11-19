// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/pion/ice/v4"
	"github.com/pion/rtcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// iceTestSocket is one socket an iceTestSockets gave: the address it is bound
// to and how many times it was closed.
type iceTestSocket struct {
	*net.UDPConn
	addr   *net.UDPAddr
	closes atomic.Int32
}

// Close counts the close, as a source does that has to know when its port is
// free again, and closes the socket.
func (s *iceTestSocket) Close() error {
	s.closes.Add(1)
	return s.UDPConn.Close()
}

// iceTestSockets is the socket source of the tests: every call binds a socket
// of its own on the loopback interface, and the sockets given are kept in the
// order they were asked for.
type iceTestSockets struct {
	mu    sync.Mutex
	given []*iceTestSocket
}

func (s *iceTestSockets) source(context.Context) (net.PacketConn, error) {
	conn, err := ListenICESocket(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		return nil, err
	}
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil, errors.Join(errors.New("the test socket has no UDP address"), conn.Close())
	}
	sock := &iceTestSocket{UDPConn: conn, addr: addr}
	s.mu.Lock()
	s.given = append(s.given, sock)
	s.mu.Unlock()
	return sock, nil
}

// sockets returns the sockets given so far.
func (s *iceTestSockets) sockets() []*iceTestSocket {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.given)
}

// iceSocketTestConfig returns the configuration of one side of a WebRTC
// session on the loopback interface that takes its sockets from source.
func iceSocketTestConfig(source ICESocketSource, cert tls.Certificate) MediaSessionWebrtcConfig {
	conf := iceLiteTestConfig(false, cert)
	conf.ICE.Socket = source
	return conf
}

// newICESocketTestSession builds one side of a WebRTC session that takes its
// sockets from sockets, and closes it when the test ends.
func newICESocketTestSession(t *testing.T, sockets *iceTestSockets, cert tls.Certificate) *MediaSessionWebrtc {
	t.Helper()
	s := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}, Mode: sdp.ModeSendrecv}
	require.NoError(t, s.Init(t.Context(), iceSocketTestConfig(sockets.source, cert)))
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// requireUDPPortFree requires that nothing holds the UDP port of addr.
func requireUDPPortFree(t *testing.T, addr *net.UDPAddr, msg string) {
	t.Helper()
	conn, err := net.ListenUDP("udp", addr)
	require.NoError(t, err, msg)
	require.NoError(t, conn.Close())
}

// requireUDPPortHeld requires that a socket is bound to the UDP port of addr.
func requireUDPPortHeld(t *testing.T, addr *net.UDPAddr, msg string) {
	t.Helper()
	conn, err := net.ListenUDP("udp", addr)
	if err == nil {
		require.NoError(t, conn.Close())
	}
	require.Error(t, err, msg)
}

// sdpAudioPort returns the port of the m=audio line of body.
func sdpAudioPort(t *testing.T, body []byte) int {
	t.Helper()
	lines := sdpLines(body, "m=audio ")
	require.Len(t, lines, 1, "%s", body)
	fields := strings.Fields(lines[0])
	require.GreaterOrEqual(t, len(fields), 2, lines[0])
	port, err := strconv.Atoi(fields[1])
	require.NoError(t, err, lines[0])
	return port
}

// sdpCandidates returns the candidates of body.
func sdpCandidates(t *testing.T, body []byte) []ice.Candidate {
	t.Helper()
	var candidates []ice.Candidate
	for _, l := range sdpLines(body, "a=candidate:") {
		c, err := ice.UnmarshalCandidate(strings.TrimPrefix(l, "a=candidate:"))
		require.NoError(t, err, l)
		candidates = append(candidates, c)
	}
	return candidates
}

// selectedLocalPort returns the local port of the candidate pair the
// connectivity checks of s selected.
func selectedLocalPort(t *testing.T, s *MediaSessionWebrtc) int {
	t.Helper()
	s.mu.Lock()
	agent := s.agent
	s.mu.Unlock()
	pair, err := agent.GetSelectedCandidatePair()
	require.NoError(t, err)
	require.NotNil(t, pair)
	return pair.Local.Port()
}

// requireWebrtcSRTCPBetween writes one RTCP packet from one session and
// requires the other to read and decrypt it.
func requireWebrtcSRTCPBetween(t *testing.T, from, to *MediaSessionWebrtc, ssrc uint32) {
	t.Helper()
	type result struct {
		pkts []rtcp.Packet
		err  error
	}
	read := make(chan result, 1)
	go func() {
		pkts := make([]rtcp.Packet, 4)
		n, err := to.ReadRTCP(make([]byte, RTPBufSize), pkts)
		read <- result{pkts: pkts[:n], err: err}
	}()
	require.NoError(t, from.WriteRTCP(&rtcp.ReceiverReport{SSRC: ssrc}))
	select {
	case got := <-read:
		require.NoError(t, got.err, "the RTCP did not arrive and decrypt")
		require.Len(t, got.pkts, 1)
		report, ok := got.pkts[0].(*rtcp.ReceiverReport)
		require.True(t, ok, "read %T, want a receiver report", got.pkts[0])
		require.Equal(t, ssrc, report.SSRC)
	case <-time.After(5 * time.Second):
		t.Fatal("the RTCP did not arrive")
	}
}

// TestICEBindsSingleSocket pins the single socket layout of a session that is
// given its socket. The port of the m= line is taken from the host candidate,
// and the agent gathers that candidate from the caller's socket. They must be
// the same port, or the offer advertises a port the caller did not choose and
// its socket carries nothing. An ICE generation has one selected candidate
// pair, so no second socket is bound beside it: every candidate is on the
// caller's port, and the connectivity checks, the DTLS handshake, RTP and RTCP
// all travel over that socket.
func TestICEBindsSingleSocket(t *testing.T) {
	ctx := t.Context()
	offererSockets, answererSockets := &iceTestSockets{}, &iceTestSockets{}
	offerer := newICESocketTestSession(t, offererSockets, testdata.ClientCertificate())
	answerer := newICESocketTestSession(t, answererSockets, testdata.ServerCertificate())

	offer, answer := negotiateWebrtc(t, offerer, answerer, nil)
	for _, side := range []struct {
		name    string
		sess    *MediaSessionWebrtc
		sockets *iceTestSockets
		body    []byte
	}{
		{name: "offerer", sess: offerer, sockets: offererSockets, body: offer},
		{name: "answerer", sess: answerer, sockets: answererSockets, body: answer},
	} {
		given := side.sockets.sockets()
		require.Len(t, given, 1, "%s: one generation takes one socket", side.name)
		bound := given[0].addr
		require.NotZero(t, bound.Port, side.name)
		require.Equal(t, bound.String(), side.sess.ICESocketAddr().String(), side.name)

		assert.Equal(t, bound.Port, sdpAudioPort(t, side.body), "%s: the m= port is the socket's port", side.name)
		candidates := sdpCandidates(t, side.body)
		require.NotEmpty(t, candidates, side.name)
		for _, c := range candidates {
			assert.Equal(t, ice.CandidateTypeHost, c.Type(), "%s: %s", side.name, c)
			assert.Equal(t, bound.IP.String(), c.Address(), "%s: %s", side.name, c)
			assert.Equal(t, bound.Port, c.Port(), "%s: every candidate is on the socket's port: %s", side.name, c)
		}
	}

	requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE and DTLS negotiation over the given sockets")
	require.Equal(t, offererSockets.sockets()[0].addr.Port, selectedLocalPort(t, offerer))
	require.Equal(t, answererSockets.sockets()[0].addr.Port, selectedLocalPort(t, answerer))
	requireWebrtcSRTPBetween(t, offerer, answerer, 81)
	requireWebrtcSRTPBetween(t, answerer, offerer, 82)
	requireWebrtcSRTCPBetween(t, offerer, answerer, 0x5eed0001)
	requireWebrtcSRTCPBetween(t, answerer, offerer, 0x5eed0002)
	require.Len(t, offererSockets.sockets(), 1, "the negotiation took no further socket")
	require.Len(t, answererSockets.sockets(), 1, "the negotiation took no further socket")

	// Without a source the agent binds its own sockets and there is none of
	// the caller's to name.
	own := newICELiteTestSession(t, false, testdata.ServerCertificate())
	require.Nil(t, own.ICESocketAddr())
	ownOffer, err := own.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NotZero(t, sdpAudioPort(t, ownOffer))
}

// TestICESessionClosesSocket asserts a session that never reached connectivity
// checks still releases the socket it was given and its agent. Init takes the
// socket and gathers, so a call abandoned during signalling has resources that
// Close is the only thing that frees.
func TestICESessionClosesSocket(t *testing.T) {
	sockets := &iceTestSockets{}
	s := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}, Mode: sdp.ModeSendrecv}
	require.NoError(t, s.Init(t.Context(), iceSocketTestConfig(sockets.source, testdata.ServerCertificate())))

	given := sockets.sockets()
	require.Len(t, given, 1)
	sock := given[0]
	requireUDPPortHeld(t, sock.addr, "the session holds the socket it was given")

	require.NoError(t, s.Close())

	// The port is free again, so the socket really was released.
	requireUDPPortFree(t, sock.addr, "the ICE socket was not released by Close")
	require.EqualValues(t, 1, sock.closes.Load(), "the socket is closed once")

	// Close is idempotent: it may be reached twice on error paths.
	require.NoError(t, s.Close())
	require.EqualValues(t, 1, sock.closes.Load(), "the socket is closed once")
	require.Equal(t, sock.addr.String(), s.ICESocketAddr().String(), "the address is still known after Close")
}

// TestICESocketReleasedWhenInitFails pins that an Init that fails leaves no
// socket behind: its caller holds no initialized session to close, so the
// socket the source gave is closed by Init itself, once, on every path that
// took one. It also pins what Init reports for a source that gives no usable
// socket.
func TestICESocketReleasedWhenInitFails(t *testing.T) {
	newSession := func() *MediaSessionWebrtc {
		return &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}, Mode: sdp.ModeSendrecv}
	}
	requireReleased := func(t *testing.T, s *MediaSessionWebrtc, sockets *iceTestSockets) {
		t.Helper()
		given := sockets.sockets()
		require.Len(t, given, 1)
		require.EqualValues(t, 1, given[0].closes.Load(), "the socket is closed once")
		requireUDPPortFree(t, given[0].addr, "the socket of a failed Init was not released")
		require.NoError(t, s.Close())
		require.EqualValues(t, 1, given[0].closes.Load(), "a Close after the failed Init closes nothing again")
	}

	t.Run("agent_refused", func(t *testing.T) {
		// A lite agent with more than host candidates is refused when the
		// agent is built, after the socket was taken.
		sockets := &iceTestSockets{}
		conf := iceSocketTestConfig(sockets.source, testdata.ServerCertificate())
		conf.ICE.Lite = true
		conf.CandidateTypes = []ICECandidateType{ICECandidateHost, ICECandidateServerReflexive}
		s := newSession()
		require.ErrorIs(t, s.Init(t.Context(), conf), ErrICEGathering)
		require.Nil(t, s.ICESocketAddr(), "a session that was not initialized holds no socket")
		requireReleased(t, s, sockets)
	})

	t.Run("no_candidate_gathered", func(t *testing.T) {
		// Loopback candidates are left out unless asked for, so the socket
		// gives the agent nothing to offer.
		sockets := &iceTestSockets{}
		conf := iceSocketTestConfig(sockets.source, testdata.ServerCertificate())
		conf.IncludeLoopback = false
		s := newSession()
		require.ErrorIs(t, s.Init(t.Context(), conf), ErrICEGathering)
		requireReleased(t, s, sockets)
	})

	t.Run("refused_before_the_socket", func(t *testing.T) {
		// What the configuration alone decides is refused before a socket
		// is asked for.
		sockets := &iceTestSockets{}
		conf := iceSocketTestConfig(sockets.source, testdata.ServerCertificate())
		conf.ICE.Lite = true
		conf.ICEURLs = []string{"stun:stun.example.com:3478"}
		require.ErrorIs(t, newSession().Init(t.Context(), conf), errICELiteWithURLs)
		require.Empty(t, sockets.sockets())
	})

	t.Run("source_fails", func(t *testing.T) {
		errNoPort := errors.New("no port left")
		type ctxKey struct{}
		ctx := context.WithValue(t.Context(), ctxKey{}, "the call")
		var sawCtx bool
		source := func(ctx context.Context) (net.PacketConn, error) {
			sawCtx = ctx.Value(ctxKey{}) == "the call"
			return nil, errNoPort
		}
		s := newSession()
		err := s.Init(ctx, iceSocketTestConfig(source, testdata.ServerCertificate()))
		require.ErrorIs(t, err, ErrICEGathering)
		require.ErrorIs(t, err, errNoPort, "the error of the source stays matchable")
		require.True(t, sawCtx, "the source is called with the ctx of Init")
		require.Nil(t, s.ICESocketAddr())
	})

	t.Run("source_gives_nothing", func(t *testing.T) {
		source := func(context.Context) (net.PacketConn, error) { return nil, nil }
		err := newSession().Init(t.Context(), iceSocketTestConfig(source, testdata.ServerCertificate()))
		require.ErrorIs(t, err, ErrICEGathering)
		require.ErrorIs(t, err, errICESocketMissing)
	})

	t.Run("not_a_udp_socket", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "sock")
		var closed atomic.Bool
		source := func(context.Context) (net.PacketConn, error) {
			conn, err := net.ListenPacket("unixgram", path)
			if err != nil {
				return nil, err
			}
			return &closeRecordingConn{PacketConn: conn, closed: &closed}, nil
		}
		err := newSession().Init(t.Context(), iceSocketTestConfig(source, testdata.ServerCertificate()))
		require.ErrorIs(t, err, ErrICEGathering)
		require.ErrorContains(t, err, "not a UDP address")
		require.True(t, closed.Load(), "the refused socket is closed")
	})
}

// closeRecordingConn records that it was closed.
type closeRecordingConn struct {
	net.PacketConn
	closed *atomic.Bool
}

func (c *closeRecordingConn) Close() error {
	c.closed.Store(true)
	return c.PacketConn.Close()
}

// TestICEForkTakesItsOwnSocket pins that every ICE generation runs on a
// socket of its own. A Fork starts a new generation while the one it was
// forked from still carries the call, so it cannot be given that generation's
// port: it asks the source again, both generations pass media on their own
// sockets at once, and each socket is released when its generation is closed,
// not before and not with the other's.
func TestICEForkTakesItsOwnSocket(t *testing.T) {
	ctx := t.Context()
	offererSockets, answererSockets := &iceTestSockets{}, &iceTestSockets{}
	offerer := newICESocketTestSession(t, offererSockets, testdata.ClientCertificate())
	answerer := newICESocketTestSession(t, answererSockets, testdata.ServerCertificate())
	negotiateWebrtc(t, offerer, answerer, nil)
	requireBothFinalize(t, offerer, answerer, 40*time.Second, "first generation")

	offererFork, err := offerer.Fork(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = offererFork.Close() })
	answererFork, err := answerer.Fork(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = answererFork.Close() })

	for _, side := range []struct {
		name    string
		first   *MediaSessionWebrtc
		fork    *MediaSessionWebrtc
		sockets *iceTestSockets
	}{
		{name: "offerer", first: offerer, fork: offererFork, sockets: offererSockets},
		{name: "answerer", first: answerer, fork: answererFork, sockets: answererSockets},
	} {
		given := side.sockets.sockets()
		require.Len(t, given, 2, "%s: the fork asked the source for its own socket", side.name)
		require.NotEqual(t, given[0].addr.Port, given[1].addr.Port, side.name)
		require.Equal(t, given[0].addr.String(), side.first.ICESocketAddr().String(), side.name)
		require.Equal(t, given[1].addr.String(), side.fork.ICESocketAddr().String(), side.name)
		requireUDPPortHeld(t, given[0].addr, side.name+": the first generation keeps its socket")
		require.Zero(t, given[0].closes.Load(), side.name)
	}

	forkOffer, forkAnswer := negotiateWebrtc(t, offererFork, answererFork, nil)
	require.Equal(t, offererSockets.sockets()[1].addr.Port, sdpAudioPort(t, forkOffer))
	require.Equal(t, answererSockets.sockets()[1].addr.Port, sdpAudioPort(t, forkAnswer))
	requireBothFinalize(t, offererFork, answererFork, 40*time.Second, "second generation")
	require.Equal(t, offererSockets.sockets()[1].addr.Port, selectedLocalPort(t, offererFork))
	require.Equal(t, answererSockets.sockets()[1].addr.Port, selectedLocalPort(t, answererFork))

	// Both generations carry media, each over its own socket.
	requireWebrtcSRTPBetween(t, offererFork, answererFork, 91)
	requireWebrtcSRTPBetween(t, answererFork, offererFork, 92)
	requireWebrtcSRTPBetween(t, offerer, answerer, 93)
	requireWebrtcSRTPBetween(t, answerer, offerer, 94)

	// The first generation is closed once the new one carries the call. Its
	// socket goes with it and the fork's stays.
	require.NoError(t, offerer.Close())
	first, second := offererSockets.sockets()[0], offererSockets.sockets()[1]
	requireUDPPortFree(t, first.addr, "the first generation's socket was not released by its Close")
	require.EqualValues(t, 1, first.closes.Load())
	require.Zero(t, second.closes.Load(), "closing the first generation must leave the fork's socket open")
	requireUDPPortHeld(t, second.addr, "the fork keeps its socket")
	require.NoError(t, answerer.Close())
	requireWebrtcSRTPBetween(t, offererFork, answererFork, 95)

	require.NoError(t, offererFork.Close())
	requireUDPPortFree(t, second.addr, "the fork's socket was not released by its Close")
	require.EqualValues(t, 1, second.closes.Load())
	require.EqualValues(t, 1, first.closes.Load())
	require.Len(t, offererSockets.sockets(), 2, "no further socket was taken")
}

// TestListenICESocket pins where the socket of an ICE generation is bound: on
// the port its caller names, and for port 0 on a port of the package's RTP
// port range when one is set, walked as the RTP sessions walk it, and on a
// port the system chooses otherwise.
func TestListenICESocket(t *testing.T) {
	loopback := net.IPv4(127, 0, 0, 1)
	listen := func(t *testing.T, port int) (*net.UDPConn, *net.UDPAddr, error) {
		t.Helper()
		laddr := &net.UDPAddr{IP: loopback, Port: port}
		conn, err := ListenICESocket(laddr)
		require.Equal(t, port, laddr.Port, "the caller's address is left as it was")
		if err != nil {
			return nil, nil, err
		}
		t.Cleanup(func() { _ = conn.Close() })
		addr, ok := conn.LocalAddr().(*net.UDPAddr)
		require.True(t, ok)
		return conn, addr, nil
	}

	t.Run("system_chosen", func(t *testing.T) {
		setRTPPortRange(t, 0, 0)
		_, addr, err := listen(t, 0)
		require.NoError(t, err)
		require.NotZero(t, addr.Port)
		require.True(t, addr.IP.Equal(loopback))
	})

	t.Run("named_port", func(t *testing.T) {
		// A named port is bound as it is, also with a range set.
		setRTPPortRange(t, 5360, 5365)
		_, addr, err := listen(t, 5361)
		require.NoError(t, err)
		require.Equal(t, 5361, addr.Port, "one socket is bound, so an odd port is as good as an even one")
		_, _, err = listen(t, 5361)
		require.Error(t, err, "a named port that is taken is not replaced by another")
	})

	t.Run("range", func(t *testing.T) {
		setRTPPortRange(t, 5340, 5345) // ports 5340, 5342, 5344
		var first *net.UDPConn
		for i, want := range []int{5340, 5342, 5344} {
			conn, addr, err := listen(t, 0)
			require.NoError(t, err)
			require.Equal(t, want, addr.Port)
			if i == 0 {
				first = conn
			}
		}
		_, _, err := listen(t, 0)
		require.ErrorContains(t, err, "no available ports in range 5340:5345")

		// The walk wraps around to a port that was freed behind it.
		require.NoError(t, first.Close())
		_, addr, err := listen(t, 0)
		require.NoError(t, err)
		require.Equal(t, 5340, addr.Port)
	})

	t.Run("range_shared_with_rtp_sessions", func(t *testing.T) {
		// One walk serves both kinds of session, so an RTP session goes on
		// from the port an ICE socket took and the other way round.
		setRTPPortRange(t, 5380, 5387) // ports 5380, 5382, 5384, 5386
		_, addr, err := listen(t, 0)
		require.NoError(t, err)
		require.Equal(t, 5380, addr.Port)

		rtpSession, err := NewMediaSession(loopback, 0)
		require.NoError(t, err)
		t.Cleanup(func() { _ = rtpSession.Close() })
		require.Equal(t, 5382, rtpSession.Laddr.Port)

		_, addr, err = listen(t, 0)
		require.NoError(t, err)
		require.Equal(t, 5384, addr.Port)
	})
}
