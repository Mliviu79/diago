// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"cmp"

	"github.com/emiago/diago/media"
)

// withDefaultWebrtcConfig returns call with each field it left at its zero
// value set to the value def has, field by field, the fields of the nested
// DTLS, ICE and Timeouts configurations included. A field call set is kept as
// it is, so the configuration a call passes wins over the default.
func withDefaultWebrtcConfig(call, def media.MediaSessionWebrtcConfig) media.MediaSessionWebrtcConfig {
	call.ICEURLs = sliceOr(call.ICEURLs, def.ICEURLs)
	call.IPFamilies = sliceOr(call.IPFamilies, def.IPFamilies)
	call.CandidateTypes = sliceOr(call.CandidateTypes, def.CandidateTypes)
	call.PortMin = cmp.Or(call.PortMin, def.PortMin)
	call.PortMax = cmp.Or(call.PortMax, def.PortMax)
	call.Timeouts.Disconnected = cmp.Or(call.Timeouts.Disconnected, def.Timeouts.Disconnected)
	call.Timeouts.Failed = cmp.Or(call.Timeouts.Failed, def.Timeouts.Failed)
	call.Timeouts.Keepalive = cmp.Or(call.Timeouts.Keepalive, def.Timeouts.Keepalive)
	call.IncludeLoopback = cmp.Or(call.IncludeLoopback, def.IncludeLoopback)
	if call.InterfaceFilter == nil {
		call.InterfaceFilter = def.InterfaceFilter
	}
	if call.IPFilter == nil {
		call.IPFilter = def.IPFilter
	}
	if call.RemoteIPFilter == nil {
		call.RemoteIPFilter = def.RemoteIPFilter
	}
	call.NetworkTypes = sliceOr(call.NetworkTypes, def.NetworkTypes)
	call.DTLS = withDefaultDTLSConfig(call.DTLS, def.DTLS)
	call.ICE.Lite = cmp.Or(call.ICE.Lite, def.ICE.Lite)
	if call.ICE.Socket == nil {
		call.ICE.Socket = def.ICE.Socket
	}
	call.ICE.ExternalIP = sliceOr(call.ICE.ExternalIP, def.ICE.ExternalIP)
	call.OfferWithoutBundle = cmp.Or(call.OfferWithoutBundle, def.OfferWithoutBundle)
	return call
}

// withDefaultDTLSConfig is withDefaultWebrtcConfig for the DTLS configuration.
func withDefaultDTLSConfig(call, def media.DTLSConfig) media.DTLSConfig {
	call.Certificates = sliceOr(call.Certificates, def.Certificates)
	call.ServerName = cmp.Or(call.ServerName, def.ServerName)
	call.ServerClientAuth = cmp.Or(call.ServerClientAuth, def.ServerClientAuth)
	call.SRTPProfiles = sliceOr(call.SRTPProfiles, def.SRTPProfiles)
	if call.SDPSetupRole == nil {
		call.SDPSetupRole = def.SDPSetupRole
	}
	call.EllipticCurves = sliceOr(call.EllipticCurves, def.EllipticCurves)
	return call
}

// sliceOr returns s, or def when s is empty.
func sliceOr[S ~[]E, E any](s, def S) S {
	if len(s) == 0 {
		return def
	}
	return s
}
