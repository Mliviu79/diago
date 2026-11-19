// SPDX-License-Identifier: MPL-2.0

package media

import "io"

// webrtcSessionState is the state a MediaSessionWebrtc keeps beside the ICE,
// DTLS and SRTP fields it negotiates with. MediaSessionWebrtc.mu guards it,
// srtpDiscards and leftRunning apart.
type webrtcSessionState struct {
	// srtpDiscards is what ReadRTP and ReadRTCP record of the packets they
	// discard. It is atomic.
	srtpDiscards

	// dtlsTr is the transport dtlsConn was built on. It is held so Finalize
	// can take the DTLS stream of the mux back from the DTLS stack once the
	// keying material is exported.
	dtlsTr *dtlsKeyExchangeConn

	// dtlsAssociationState is the DTLS association of this ICE generation,
	// which the session negotiates once and every later description has to
	// continue, and the a=tls-id values that identify it: see
	// recordDTLSAssociation.
	dtlsAssociationState

	// origin gives the o= line of the session's SDPs: see sdpOrigin. It is
	// nil until the first SDP is built or a fork is handed it, and guards
	// itself.
	origin *sdpOrigin

	// reOffer is what ReOffer staged: see stagedModePreference.
	reOffer stagedModePreference

	// offeredProto is the m= transport of the last offer RemoteSDP accepted,
	// as the peer wrote it, empty before any. The answer to that offer
	// repeats it.
	offeredProto string

	// remoteICELite is set when the peer's description of this ICE generation
	// carries a=ice-lite: it runs a lite agent, which decides the ICE roles
	// (RFC 8445 section 6.1.1).
	remoteICELite bool

	// leftRunning is the negotiation FinalizeWithin left running. It guards
	// itself.
	leftRunning negotiationLeftRunning
}

// dtlsTransport returns the packet conn the DTLS handshake runs over, which is
// the DTLS stream of the mux: the handshake shares the selected candidate pair
// with media and the mux keeps the two apart. It records the conn as the one
// retireDTLS takes back. The caller holds m.mu.
//
// The stream is only lent to the DTLS stack: see dtlsKeyExchangeConn.
func (m *MediaSessionWebrtc) dtlsTransport() *dtlsKeyExchangeConn {
	tr := newDTLSKeyExchangeConn(m.mux.dtls)
	m.dtlsTr = tr
	return tr
}

// retireDTLS ends the DTLS stack conn once its handshake has exported the
// keys, so that its read loop and its handshake goroutine do not stay parked
// on the mux for the life of the call, and so that nothing the stack does
// afterwards, a close_notify included, reaches the peer: see
// dtlsKeyExchangeConn.retire.
func (m *MediaSessionWebrtc) retireDTLS(conn io.Closer) error {
	m.mu.Lock()
	tr := m.dtlsTr
	m.mu.Unlock()
	return tr.retire(conn)
}
