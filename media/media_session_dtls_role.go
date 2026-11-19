// SPDX-License-Identifier: MPL-2.0

package media

import "strings"

// DTLSEndpointRole is the offer/answer role of this endpoint in an SDP
// exchange. It decides the default a=setup value of a DTLS-SRTP session and
// what DTLSConfig.SDPSetupRole is told. DTLSEndpointRoleUnknown lets
// MediaSession take the role from the exchange itself: the arguments of
// LocalSDP and RemoteSDP say whether a body is an offer or an answer.
type DTLSEndpointRole int

// The offer/answer roles of [MediaSession.DTLSRole].
const (
	// DTLSEndpointRoleUnknown leaves the role to the exchange.
	DTLSEndpointRoleUnknown DTLSEndpointRole = iota
	// DTLSEndpointRoleOfferer makes the session the offerer of every exchange.
	DTLSEndpointRoleOfferer
	// DTLSEndpointRoleAnswerer makes the session the answerer of every
	// exchange.
	DTLSEndpointRoleAnswerer
)

// The a=setup attribute values this stack signals (RFC 4145 section 4). They
// are named because the advertised value and the DTLS role derived from it are
// compared against the same spellings in both directions.
const (
	// dtlsSetupActive: this endpoint initiates the connection, so it is the
	// DTLS client that sends the ClientHello (RFC 5763 section 5).
	dtlsSetupActive = "active"
	// dtlsSetupPassive: this endpoint accepts the connection, so it is the
	// DTLS server.
	dtlsSetupPassive = "passive"
	// dtlsSetupActPass: this endpoint will do either and leaves the choice to
	// the peer. RFC 5763 section 5 requires an offerer to use it.
	dtlsSetupActPass = "actpass"
)

// mediaSessionState is the state a MediaSession keeps beside the fields it
// negotiates with. Like them it is not safe for concurrent use, and Fork does
// not carry it: a fork starts an exchange of its own.
type mediaSessionState struct {
	// remoteSDPIsAnswer is the offered argument of the last RemoteSDP: the
	// body it applied answered an offer of ours. Together with remoteProto,
	// which RemoteSDP sets on every body, it says whether an offer was
	// applied.
	remoteSDPIsAnswer bool

	// dtlsRemoteSetup is the peer's a=setup value (RFC 4145 section 4), as
	// RemoteSDP read it. Our own role is complementary to it, so it is kept:
	// the role is advertised in LocalSDP and played from RemoteSDP on, and
	// both follow from this one value.
	dtlsRemoteSetup string
}

// answeringOffer reports whether the last body RemoteSDP applied was an offer,
// which leaves this session the answerer of the exchange. It is false before
// any body is applied and after the answer to an offer of ours.
func (s *MediaSession) answeringOffer() bool {
	return s.remoteProto != "" && !s.remoteSDPIsAnswer
}

// dtlsEndpointRole resolves the role this session has in the exchange RemoteSDP
// last took part in. Without an explicit DTLSRole it comes from whether an
// offer was applied: an applied offer makes us the answerer, and no SDP yet, or
// the answer to our own offer, leaves us the offerer. It never comes from
// whether the remote address is known: an offerer knows whom it is calling, and
// has read the answerer's address once the answer is applied.
func (s *MediaSession) dtlsEndpointRole() DTLSEndpointRole {
	return s.dtlsEndpointRoleAs(!s.answeringOffer())
}

// dtlsEndpointRoleAs resolves the role in an exchange where offerer says
// whether this endpoint made the offer. An explicit DTLSRole wins.
func (s *MediaSession) dtlsEndpointRoleAs(offerer bool) DTLSEndpointRole {
	if s.DTLSRole != DTLSEndpointRoleUnknown {
		return s.DTLSRole
	}
	if offerer {
		return DTLSEndpointRoleOfferer
	}
	return DTLSEndpointRoleAnswerer
}

// dtlsSetupFor is the one place the DTLS role is decided: it returns the
// a=setup value of this endpoint in an exchange where it has role and the peer
// sent the a=setup value peer, empty when the peer has sent none in it. RFC
// 4145 section 4 defines active as the endpoint that initiates the connection
// and passive as the one that accepts it; RFC 5763 section 5 maps those onto
// DTLS as the client that sends the ClientHello and the server that answers
// it.
//
// LocalSDP advertises what it returns and RemoteSDP builds the DTLS conn of
// the role it implies, so the role we advertise and the role we play cannot
// disagree. Were they to, against an actpass offer we could advertise passive,
// telling the peer to initiate, and initiate as well: both endpoints would
// send a ClientHello and the handshake would fail.
func (s *MediaSession) dtlsSetupFor(role DTLSEndpointRole, peer string) string {
	peer = strings.ToLower(strings.TrimSpace(peer))
	// A peer that took a side leaves us the other one (RFC 4145 section 4.1),
	// whatever SDPSetupRole would choose: the same side would have both
	// endpoints wait, or both send a ClientHello.
	switch peer {
	case dtlsSetupActive:
		return dtlsSetupPassive
	case dtlsSetupPassive:
		return dtlsSetupActive
	}

	// The choice is ours: the peer left it to us, or has not spoken in this
	// exchange. An application that set SDPSetupRole makes it.
	offerer := role == DTLSEndpointRoleOfferer
	if s.DTLSConf.SDPSetupRole != nil {
		return s.DTLSConf.SDPSetupRole(offerer)
	}

	// RFC 5763 section 5: "setup:active allows the answer and the DTLS
	// handshake to occur in parallel. Thus, setup:active is RECOMMENDED". A
	// passive answer stalls the handshake until the answer reaches the peer.
	// An offer without a=setup is answered as one of actpass.
	if peer == dtlsSetupActPass || !offerer {
		return dtlsSetupActive
	}
	// RFC 5763 section 5: "The endpoint that is the offerer MUST use the setup
	// attribute value of setup:actpass". RFC 8842 section 5.5 asks the same of
	// every subsequent offer.
	return dtlsSetupActPass
}

// localDTLSSetup returns the a=setup value LocalSDP advertises: answered says
// whether the body it builds answers the offer RemoteSDP applied, or is an
// offer of ours. An offer opens an exchange in which the peer has said
// nothing, so an a=setup value of an earlier body of its does not count.
func (s *MediaSession) localDTLSSetup(answered bool) string {
	if !answered {
		return s.dtlsSetupFor(s.dtlsEndpointRoleAs(true), "")
	}
	return s.dtlsSetupFor(s.dtlsEndpointRoleAs(false), s.dtlsRemoteSetup)
}

// dtlsActsAsClient reports whether this endpoint sends the ClientHello against
// the a=setup value RemoteSDP last read, which is what advertising active
// commits us to (RFC 5763 section 5).
//
// While both sides say actpass the choice is nobody's yet, so we are not the
// client: RFC 5763 section 5 has the offerer "be prepared to receive a
// client_hello before it receives the answer", which is to behave as the
// server until the answer says otherwise.
func (s *MediaSession) dtlsActsAsClient() bool {
	return s.dtlsSetupFor(s.dtlsEndpointRole(), s.dtlsRemoteSetup) == dtlsSetupActive
}
