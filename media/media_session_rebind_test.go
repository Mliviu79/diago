// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ExternalIP describes how one socket is reached, so a fork that rebinds to a
// new local address must not keep publishing the old socket's address.
func TestMediaSessionExternalIPDroppedOnRebind(t *testing.T) {
	m := &MediaSession{
		Codecs:     []Codec{CodecAudioAlaw},
		Laddr:      net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Mode:       sdp.ModeSendrecv,
		ExternalIP: net.IPv4(1, 1, 1, 1),
	}
	require.NoError(t, m.Init())
	defer m.Close()

	f := m.Fork()
	f.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 2), Port: 0}
	require.NoError(t, f.Init())
	defer f.Close()

	assert.Nil(t, f.ExternalIP, "the external address of the old socket cannot describe the new one")

	sd := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(f.LocalSDP(false), &sd))
	connInfo, err := sd.ConnectionInformation()
	require.NoError(t, err)
	assert.Equal(t, net.IPv4(127, 0, 0, 2).To4(), connInfo.IP.To4())
}
