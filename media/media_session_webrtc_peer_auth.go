// SPDX-License-Identifier: MPL-2.0

package media

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	webrtcsdp "github.com/pion/sdp/v3"
)

// webRTCFingerprints returns the certificate fingerprints a remote WebRTC SDP
// gives for its audio: every a=fingerprint of the audio media description and
// of the session level, never one of another media description, which says
// nothing of the audio. The peer's certificate has to match one of them.
//
// RFC 5763 section 5 makes the attribute mandatory. The certificates are self
// signed and the fingerprint is the only thing the peer's one can be checked
// against, so a description with no fingerprint under a hash function we
// compare is refused here, before any ICE work is done for it, instead of
// failing the handshake after the answer was sent. A fingerprint under another
// hash function is kept: it is part of the set a later description is compared
// with.
func webRTCFingerprints(session, media []webrtcsdp.Attribute) ([]DTLSFingerprint, error) {
	var fingerprints []DTLSFingerprint
	for _, attrs := range [][]webrtcsdp.Attribute{media, session} {
		for _, attr := range attrs {
			if attr.Key != "fingerprint" {
				continue
			}
			fields := strings.Fields(attr.Value)
			if len(fields) < 2 {
				return nil, fmt.Errorf("invalid remote DTLS fingerprint %q", attr.Value)
			}
			fingerprints = append(fingerprints, DTLSFingerprint{
				Algorithm: fields[0],
				Value:     fields[1],
			})
		}
	}
	if len(fingerprints) == 0 {
		return nil, errors.New("remote WebRTC SDP has no DTLS fingerprint")
	}
	if !hasCheckableDTLSFingerprint(fingerprints) {
		return nil, fmt.Errorf("remote WebRTC SDP has no DTLS fingerprint with a supported hash function fingerprints=%d", len(fingerprints))
	}
	return fingerprints, nil
}

// continuesRemoteFingerprints reports whether fingerprints are the set the
// DTLS association of this ICE generation was negotiated with. RFC 8842
// section 3.1 asks for a new association when a fingerprint is modified, added
// or removed, and this session builds one only with a new ICE generation, so a
// description of the same generation has to carry the same set. The caller
// holds m.mu.
func (m *MediaSessionWebrtc) continuesRemoteFingerprints(fingerprints []DTLSFingerprint) bool {
	return m.dtlsAssoc != nil && slices.Equal(fingerprintSet(fingerprints), m.dtlsAssoc.remoteFingerprints)
}

// localFingerprintLines returns the a=fingerprint lines of our SDP, one for
// each configured certificate. The DTLS stack presents whichever of them suits
// the handshake, and RFC 8122 section 5.1 has a fingerprint calculated for
// each certificate that may be used, so that the peer can match the one it is
// shown.
func (m *MediaSessionWebrtc) localFingerprintLines() ([]string, error) {
	lines := make([]string, 0, len(m.Config.DTLS.Certificates))
	for i, cert := range m.Config.DTLS.Certificates {
		fingerprint, err := dtlsSHA256Fingerprint(cert)
		if err != nil {
			return nil, fmt.Errorf("certificate %d: %w", i, err)
		}
		lines = append(lines, "a=fingerprint:sha-256 "+fingerprint)
	}
	return lines, nil
}
