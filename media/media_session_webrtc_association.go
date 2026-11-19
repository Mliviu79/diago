// SPDX-License-Identifier: MPL-2.0

package media

import (
	"errors"
	"fmt"
	"strings"

	webrtcsdp "github.com/pion/sdp/v3"
)

// ErrWebRTCICERestartRequired is returned, wrapped, by
// [MediaSessionWebrtc.RemoteSDP] for a description that keeps the ICE
// credentials of the generation in use and asks for a new DTLS association
// over it: one that changes the DTLS roles, the fingerprint set or the
// a=tls-id value (RFC 8842 section 3.1).
//
// Over UDP a new association can only be told apart from the one in use by
// its transport, so RFC 8842 sections 5.1 and 6 have it run on ICE candidates
// that no other association used, which an offerer provides with an ICE
// restart and an answerer cannot provide on its own. A description that asks
// for one on the candidates in use can therefore not be followed. It is well
// formed and the session carries on as it was, so its caller answers such an
// offer 488 Not Acceptable Here, as section 5.3 has an answerer that does not
// accept a new association reject the media. Match it with errors.Is.
//
// It is not [ErrWebRTCICERestart], which reports new ICE credentials, a
// request the caller completes with [MediaSessionWebrtc.Fork].
var ErrWebRTCICERestartRequired = errors.New("webrtc: a new DTLS association needs an ICE restart")

// webRTCTLSID returns the a=tls-id value a remote WebRTC SDP gives for its
// audio, or "" when it gives none.
func webRTCTLSID(session, media []webrtcsdp.Attribute) string {
	tlsID, _ := webRTCAttribute(session, media, "tls-id")
	return strings.TrimSpace(tlsID)
}

// recordDTLSAssociation records the DTLS association RemoteSDP negotiates for
// this ICE generation, which every later description of the generation is
// compared with: setup is the side we take in it, and fingerprints and tlsID
// are the peer's a=fingerprint set and a=tls-id value. offered says the
// description answers an offer of ours. The caller holds m.mu.
//
// The association is new, so a session answering the offer draws its a=tls-id
// for it, and a session reading the answer to its own offer keeps the value
// that offer carried (RFC 8842 sections 5.2 and 5.3).
func (m *MediaSessionWebrtc) recordDTLSAssociation(setup string, fingerprints []DTLSFingerprint, tlsID string, offered bool) {
	m.answerTLSID(offered, tlsID)
	m.dtlsRemoteTLSID = tlsID
	m.dtlsAssoc = &dtlsAssociation{
		setup:              setup,
		remoteFingerprints: fingerprintSet(fingerprints),
		remoteTLSID:        tlsID,
		localTLSID:         m.localTLSID,
	}
}

// continueDTLSAssociation takes tlsID, the a=tls-id value of a description of
// the ICE generation in use, "" when it carries none. A peer changes the value
// to ask for a new DTLS association (RFC 8842 section 4), which this session
// builds only with a new ICE generation, so a changed value is refused with
// ErrWebRTCICERestartRequired. The caller holds m.mu and has found the
// fingerprint set to be the one of the association on record.
func (m *MediaSessionWebrtc) continueDTLSAssociation(tlsID string) error {
	if m.dtlsAssoc.tlsIDChanged(tlsID) {
		return fmt.Errorf("remote DTLS tls-id changed without an ICE restart: %w", ErrWebRTCICERestartRequired)
	}
	// An answer of ours carries a tls-id only when the offer does.
	m.dtlsRemoteTLSID = tlsID
	return nil
}

// localTLSIDFor returns the a=tls-id value of the body LocalSDP or ReOffer
// builds, or "" for none; answered says the body answers the offer RemoteSDP
// accepted. The caller holds m.mu.
//
// Every offer carries our value for the association of this ICE generation,
// drawn with the first one, and an answer carries it when the offer carried
// one (RFC 8842 sections 5.2, 5.3 and 5.5). The value never changes on one
// session: our certificates and candidates are those of its generation, and a
// new association comes with a new generation, a session of its own that
// draws its own value (section 3.3).
func (m *MediaSessionWebrtc) localTLSIDFor(answered bool) string {
	return m.tlsIDFor(answered, false)
}

// dtlsAssociationLines returns the lines of our SDP that follow the
// fingerprints of a DTLS association: a=setup, and a=tls-id when tlsID is not
// empty.
func dtlsAssociationLines(setup string, tlsID string) []string {
	lines := []string{"a=setup:" + setup}
	if tlsID != "" {
		lines = append(lines, "a=tls-id:"+tlsID)
	}
	return lines
}
