// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"net"
	"sync"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
)

// webrtcConfig returns the transport configuration of a DialogWebrtc built
// from conf, for a call that passed call. What the call set is kept, field by
// field. A field the call left at its zero value takes the value of
// conf.WebrtcConfig, and conf supplies the rest, as the media of every other
// call on the transport takes it:
//
//   - The socket of each ICE generation is bound on the transport's media
//     address, on the port conf.RTPPortAllocator hands out, which goes back
//     to the allocator once the generation's socket is closed. Without an
//     allocator the port is that of the media package port range, or one the
//     system chooses. A call that gives its own ICE.Socket, or a PortMin or
//     PortMax of its own, keeps that: its port choice is the call's. So does
//     a call that takes one of them from conf.WebrtcConfig.
//   - The c= line gives the transport's external media address, unless the
//     call, or conf.WebrtcConfig, gives its own ICE.ExternalIP.
func (conf MediaConfig) webrtcConfig(call media.MediaSessionWebrtcConfig) media.MediaSessionWebrtcConfig {
	call = withDefaultWebrtcConfig(call, conf.WebrtcConfig)
	if call.ICE.Socket == nil && call.PortMin == 0 && call.PortMax == 0 {
		call.ICE.Socket = conf.iceSocket
	}
	if call.ICE.ExternalIP == nil {
		call.ICE.ExternalIP = conf.externalIP
	}
	return call
}

// iceSocket binds the socket of one ICE generation of a WebRTC media built from
// conf, on ctx, the context of the call the media is for or of the media
// update that starts the generation. It is a media.ICESocketSource: the media
// session owns the socket it returns and closes it once, which hands the port
// back to the allocator that chose it.
func (conf MediaConfig) iceSocket(ctx context.Context) (net.PacketConn, error) {
	bindIP := conf.bindIP
	if bindIP == nil {
		var err error
		bindIP, _, err = sip.ResolveInterfacesIP("ip4", nil)
		if err != nil {
			return nil, err
		}
	}
	port, release, err := conf.allocateRTPPort(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := media.ListenICESocket(&net.UDPAddr{IP: bindIP, Port: port})
	if err != nil {
		// Nothing was bound, so the port goes straight back.
		if release != nil {
			release()
		}
		return nil, err
	}
	if release == nil {
		return conn, nil
	}
	return &allocatedICESocket{UDPConn: conn, release: release}, nil
}

// allocatedICESocket is the socket of an ICE generation bound on a port an
// RTPPortAllocator chose. Its Close hands the port back, once and only after
// the socket is closed: an allocator may hold the port for a drain window,
// which has to start once nothing can be received on it any more.
type allocatedICESocket struct {
	*net.UDPConn
	once    sync.Once
	release func()
}

// Close closes the socket and then releases its port, on the first call.
func (s *allocatedICESocket) Close() error {
	err := s.UDPConn.Close()
	if errors.Is(err, net.ErrClosed) {
		// Closed before: the first Close released the port.
		return err
	}
	s.once.Do(s.release)
	return err
}
