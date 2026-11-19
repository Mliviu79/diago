// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"crypto/tls"
	"net"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
)

// TestWebrtcDefaultConfig pins how the default WebRTC configuration of a
// MediaConfig meets the configuration a call passes: a call that passes none
// runs on the default, and the call's configuration wins field by field, the
// fields of the nested DTLS, ICE and Timeouts configurations included.
func TestWebrtcDefaultConfig(t *testing.T) {
	cert := testdata.ServerCertificate()
	def := media.MediaSessionWebrtcConfig{
		ICEURLs:         []string{"stun:stun.example.org"},
		IPFamilies:      []media.ICEIPFamily{media.ICEIPFamilyIPv4},
		CandidateTypes:  []media.ICECandidateType{media.ICECandidateHost},
		PortMin:         20000,
		PortMax:         20100,
		Timeouts:        media.ICETimeouts{Disconnected: time.Second, Failed: 2 * time.Second, Keepalive: 3 * time.Second},
		IncludeLoopback: true,
		DTLS: media.DTLSConfig{
			Certificates: []tls.Certificate{cert},
			ServerName:   "default",
			SRTPProfiles: []uint16{1},
		},
		ICE:                media.ICEConfig{Lite: true, ExternalIP: net.IPv4(203, 0, 113, 1)},
		OfferWithoutBundle: true,
	}

	t.Run("NoCallConfig", func(t *testing.T) {
		got := withDefaultWebrtcConfig(media.MediaSessionWebrtcConfig{}, def)
		assert.Equal(t, def.ICEURLs, got.ICEURLs)
		assert.Equal(t, def.IPFamilies, got.IPFamilies)
		assert.Equal(t, def.CandidateTypes, got.CandidateTypes)
		assert.Equal(t, def.PortMin, got.PortMin)
		assert.Equal(t, def.PortMax, got.PortMax)
		assert.Equal(t, def.Timeouts, got.Timeouts)
		assert.True(t, got.IncludeLoopback)
		assert.Equal(t, def.DTLS.Certificates, got.DTLS.Certificates)
		assert.Equal(t, def.DTLS.ServerName, got.DTLS.ServerName)
		assert.Equal(t, def.DTLS.SRTPProfiles, got.DTLS.SRTPProfiles)
		assert.Equal(t, def.ICE, got.ICE)
		assert.True(t, got.OfferWithoutBundle)
	})

	t.Run("CallWinsFieldByField", func(t *testing.T) {
		call := media.MediaSessionWebrtcConfig{
			ICEURLs:  []string{"stun:call.example.org"},
			PortMin:  30000,
			Timeouts: media.ICETimeouts{Failed: 9 * time.Second},
			DTLS:     media.DTLSConfig{ServerName: "call"},
			ICE:      media.ICEConfig{ExternalIP: net.IPv4(198, 51, 100, 1)},
		}
		got := withDefaultWebrtcConfig(call, def)
		assert.Equal(t, call.ICEURLs, got.ICEURLs)
		assert.Equal(t, call.PortMin, got.PortMin)
		assert.Equal(t, def.PortMax, got.PortMax, "a field the call left unset takes the default")
		assert.Equal(t, media.ICETimeouts{Disconnected: time.Second, Failed: 9 * time.Second, Keepalive: 3 * time.Second}, got.Timeouts)
		assert.Equal(t, "call", got.DTLS.ServerName)
		assert.Equal(t, def.DTLS.Certificates, got.DTLS.Certificates, "the DTLS fields the call left unset take the default")
		assert.True(t, got.ICE.ExternalIP.Equal(call.ICE.ExternalIP))
		assert.True(t, got.ICE.Lite)
	})

	t.Run("ReachesTheDialog", func(t *testing.T) {
		dg := &Diago{mediaConf: MediaConfig{WebrtcConfig: def}}
		conf := dg.mediaConfForTransport(&Transport{})
		got := conf.webrtcConfig(media.MediaSessionWebrtcConfig{})
		assert.Equal(t, def.ICEURLs, got.ICEURLs, "the per dialog config dropped WebrtcConfig")
		assert.Equal(t, def.PortMin, got.PortMin)
	})
}
