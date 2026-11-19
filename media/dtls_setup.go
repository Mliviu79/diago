// SPDX-License-Identifier: MPL-2.0

package media

import "strings"

// knownDTLSSetup reports whether setup is an a=setup value a DTLS association
// can be negotiated with: active, passive or actpass, in any case. RFC 4145
// section 4 defines holdconn as well, which RFC 8842 section 5.1 excludes for
// DTLS.
func knownDTLSSetup(setup string) bool {
	switch strings.ToLower(strings.TrimSpace(setup)) {
	case dtlsSetupActive, dtlsSetupPassive, dtlsSetupActPass:
		return true
	}
	return false
}

// setupFor is the one place the DTLS role is decided, for every media type: it
// returns the a=setup value of this endpoint in an exchange where offerer says
// whether it made the offer and the peer sent the a=setup value peer, empty
// when the peer has sent none in it. RFC 4145 section 4 defines active as the
// endpoint that initiates the connection and passive as the one that accepts
// it; RFC 5763 section 5 maps those onto DTLS as the client that sends the
// ClientHello and the server that answers it.
//
// A media session advertises what it returns and plays the role that value
// commits to, so the two come from one decision.
func (conf *DTLSConfig) setupFor(offerer bool, peer string) string {
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
	if conf.SDPSetupRole != nil {
		return conf.SDPSetupRole(offerer)
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
