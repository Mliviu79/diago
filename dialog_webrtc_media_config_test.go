// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// iceTestAllocator hands out the ports of a fixed list, in turn, and records
// every port released to it. It is safe for concurrent use: an ICE socket is
// released by whichever goroutine closes its media session.
type iceTestAllocator struct {
	mu       sync.Mutex
	ports    []int
	err      error
	released []int
}

func (a *iceTestAllocator) AllocateRTPPort(ctx context.Context) (int, error) {
	if ctx.Err() != nil {
		return 0, context.Cause(ctx)
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return 0, a.err
	}
	if len(a.ports) == 0 {
		return 0, errors.New("iceTestAllocator: no port left")
	}
	port := a.ports[0]
	a.ports = a.ports[1:]
	return port, nil
}

func (a *iceTestAllocator) ReleaseRTPPort(port int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.released = append(a.released, port)
}

func (a *iceTestAllocator) releasedPorts() []int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]int(nil), a.released...)
}

// initWebrtcSessionFor builds the WebRTC media session a DialogWebrtc on tran
// starts with for a call that passed call, through the config path AnswerWebrtc
// and InviteWebrtc take. The session is closed with the test.
func initWebrtcSessionFor(t *testing.T, dg *Diago, tran *Transport, call media.MediaSessionWebrtcConfig) (*media.MediaSessionWebrtc, error) {
	t.Helper()
	conf, err := prepareWebrtcConfig(dg.mediaConfForTransport(tran).webrtcConfig(call))
	require.NoError(t, err)
	sess := &media.MediaSessionWebrtc{Codecs: []media.Codec{media.CodecAudioUlaw}}
	if err := sess.Init(t.Context(), conf); err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = sess.Close() })
	return sess, nil
}

// TestMediaConfigICEWiring pins that the ICE socket and the external address of
// a DialogWebrtc's transport come from the Diago wide media config and the
// transport, as the media of every other call does: one socket on the
// transport's media address, on the port the RTPPortAllocator hands out, which
// is the port of the host candidates and of the m= line and goes back to the
// allocator when the socket is closed, and the transport's external media
// address in c=. A call's own settings keep precedence.
func TestMediaConfigICEWiring(t *testing.T) {
	loopback := net.IPv4(127, 0, 0, 1)
	external := net.IPv4(203, 0, 113, 7)
	tran := Transport{BindHost: "127.0.0.1", MediaExternalIP: external}

	t.Run("AllocatedPortIsTheICEPort", func(t *testing.T) {
		port := freeEvenPort(t, loopback)
		alloc := &iceTestAllocator{ports: []int{port}}
		dg, tr := newMediaConfDiago(t, tran, MediaConfig{RTPPortAllocator: alloc})

		sess, err := initWebrtcSessionFor(t, dg, tr, loopbackWebrtcConfig())
		require.NoError(t, err)
		addr := sess.ICESocketAddr()
		require.NotNil(t, addr, "the session must run on the socket the media config binds")
		assert.Equal(t, port, addr.Port)
		assert.True(t, addr.IP.Equal(loopback), "the socket is bound on the transport's media address, got %v", addr.IP)

		offer, err := sess.LocalSDP(t.Context(), false)
		require.NoError(t, err)
		sdp := string(offer)
		assert.Contains(t, sdp, fmt.Sprintf("m=audio %d ", port), "the m= port is the allocated port")
		assert.Contains(t, sdp, fmt.Sprintf(" %d typ host", port), "the host candidate is on the allocated port")
		assert.Contains(t, sdp, "c=IN IP4 203.0.113.7", "c= gives the transport's external media address")
		assert.NotContains(t, sdp, "203.0.113.7 ", "no candidate is made up for the external address")

		require.Empty(t, alloc.releasedPorts(), "the port went back before the socket was closed")
		require.NoError(t, sess.Close())
		assert.Equal(t, []int{port}, alloc.releasedPorts())
		require.NoError(t, sess.Close())
		assert.Equal(t, []int{port}, alloc.releasedPorts(), "a second Close released the port again")

		// Once released the port can be bound again: the socket is closed.
		conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback, Port: port})
		require.NoError(t, err)
		require.NoError(t, conn.Close())
	})

	// Every ICE generation is a new transport with a socket of its own, so a
	// fork takes a port of its own while the session it was forked from still
	// runs on its port, and each port goes back when its socket is closed.
	t.Run("NewTransportTakesItsOwnPort", func(t *testing.T) {
		first, second := freeEvenPort(t, loopback), 0
		for second == 0 || second == first {
			second = freeEvenPort(t, loopback)
		}
		alloc := &iceTestAllocator{ports: []int{first, second}}
		dg, tr := newMediaConfDiago(t, tran, MediaConfig{RTPPortAllocator: alloc})

		sess, err := initWebrtcSessionFor(t, dg, tr, loopbackWebrtcConfig())
		require.NoError(t, err)
		fork, err := sess.Fork(t.Context())
		require.NoError(t, err)
		assert.Equal(t, second, fork.ICESocketAddr().Port)
		offer, err := fork.LocalSDP(t.Context(), false)
		require.NoError(t, err)
		assert.Contains(t, string(offer), "c=IN IP4 203.0.113.7", "the fork keeps the external media address")

		require.NoError(t, sess.Close())
		assert.Equal(t, []int{first}, alloc.releasedPorts())
		require.NoError(t, fork.Close())
		assert.Equal(t, []int{first, second}, alloc.releasedPorts())
	})

	t.Run("AllocationFailureFailsInit", func(t *testing.T) {
		refused := errors.New("no port for this call")
		alloc := &iceTestAllocator{err: refused}
		dg, tr := newMediaConfDiago(t, tran, MediaConfig{RTPPortAllocator: alloc})

		_, err := initWebrtcSessionFor(t, dg, tr, loopbackWebrtcConfig())
		require.ErrorIs(t, err, refused)
		assert.Empty(t, alloc.releasedPorts(), "nothing was allocated, so nothing goes back")
	})

	// A port that can not be bound goes straight back: no socket will close
	// on it later.
	t.Run("BindFailureReleasesAtOnce", func(t *testing.T) {
		taken, err := net.ListenUDP("udp", &net.UDPAddr{IP: loopback})
		require.NoError(t, err)
		t.Cleanup(func() { taken.Close() })
		port := taken.LocalAddr().(*net.UDPAddr).Port
		alloc := &iceTestAllocator{ports: []int{port}}
		dg, tr := newMediaConfDiago(t, tran, MediaConfig{RTPPortAllocator: alloc})

		_, err = initWebrtcSessionFor(t, dg, tr, loopbackWebrtcConfig())
		require.Error(t, err)
		assert.Equal(t, []int{port}, alloc.releasedPorts())
	})

	// Without an allocator the socket is still bound on the transport's media
	// address, on a port the system chooses.
	t.Run("NoAllocator", func(t *testing.T) {
		dg, tr := newMediaConfDiago(t, tran, MediaConfig{})

		sess, err := initWebrtcSessionFor(t, dg, tr, loopbackWebrtcConfig())
		require.NoError(t, err)
		addr := sess.ICESocketAddr()
		require.NotNil(t, addr)
		assert.True(t, addr.IP.Equal(loopback))
		assert.NotZero(t, addr.Port)
	})

	t.Run("CallSocketWins", func(t *testing.T) {
		alloc := &iceTestAllocator{}
		dg, tr := newMediaConfDiago(t, tran, MediaConfig{RTPPortAllocator: alloc})
		var own *net.UDPAddr
		call := loopbackWebrtcConfig()
		call.ICE.Socket = func(context.Context) (net.PacketConn, error) {
			conn, err := media.ListenICESocket(&net.UDPAddr{IP: loopback})
			if err == nil {
				own = conn.LocalAddr().(*net.UDPAddr)
			}
			return conn, err
		}

		sess, err := initWebrtcSessionFor(t, dg, tr, call)
		require.NoError(t, err)
		require.NotNil(t, own)
		assert.Equal(t, own.Port, sess.ICESocketAddr().Port)
		require.NoError(t, sess.Close())
		assert.Empty(t, alloc.releasedPorts(), "the allocator was consulted for a call with its own socket")
	})

	// A call's own port range is its port choice: the agent binds its sockets
	// in that range, as it does for a call on no Diago at all.
	t.Run("CallPortRangeWins", func(t *testing.T) {
		alloc := &iceTestAllocator{}
		dg, tr := newMediaConfDiago(t, tran, MediaConfig{RTPPortAllocator: alloc})
		call := loopbackWebrtcConfig()
		call.PortMin, call.PortMax = 40000, 60000

		sess, err := initWebrtcSessionFor(t, dg, tr, call)
		require.NoError(t, err)
		assert.Nil(t, sess.ICESocketAddr(), "the media config gave a socket to a call with its own port range")
		offer, err := sess.LocalSDP(t.Context(), false)
		require.NoError(t, err)
		assert.Contains(t, string(offer), "c=IN IP4 203.0.113.7", "the external address still comes from the transport")
	})

	t.Run("CallExternalIPWins", func(t *testing.T) {
		dg, tr := newMediaConfDiago(t, tran, MediaConfig{})
		call := loopbackWebrtcConfig()
		call.ICE.ExternalIP = net.IPv4(198, 51, 100, 9)

		sess, err := initWebrtcSessionFor(t, dg, tr, call)
		require.NoError(t, err)
		offer, err := sess.LocalSDP(t.Context(), false)
		require.NoError(t, err)
		assert.True(t, strings.Contains(string(offer), "c=IN IP4 198.51.100.9"), "the call's own external address must win")
	})
}
