// SPDX-License-Identifier: MPL-2.0

package mediaweb

import (
	"errors"
	"fmt"
	"strings"

	"github.com/emiago/diago/media"
	webrtcsdp "github.com/pion/sdp/v3"
)

// remoteDTLSFingerprints returns the certificate fingerprints a remote SDP
// gives for its audio, from the attributes of its session level and of its
// audio section. The list is what the DTLS configuration verifies the peer's
// certificate against (media.DTLSConfig.ToLibConf), and it is made as RFC 8122
// has it:
//
//   - Every a=fingerprint line counts, not the first one alone. Section 5.1:
//     "Multiple SDP fingerprint attributes can be associated with an 'm'
//     line. This can occur if multiple fingerprints have been calculated for
//     a certificate using different hash functions. It can also occur if one
//     or more fingerprints associated with multiple certificates have been
//     calculated." The same section has the certificate verified against
//     those under the strongest hash function offered, which the verification
//     chooses from the whole list, so a line left out here could be the one
//     the peer has to match.
//   - The lines of the audio section apply, and those of the session level
//     only when the section has none. Section 5: "If it is a session-level
//     attribute, it applies to all TLS sessions for which no media-level
//     fingerprint attribute is defined." A session-level line beside the
//     section's own names the certificate of other media, so it is not read.
//
// A description with no line that applies is refused, and so is one where a
// line that applies is not a hash function name followed by one value
// (section 5: fingerprint-attribute = "fingerprint" ":" hash-func SP
// fingerprint). Such a line may be the one under the strongest hash function
// the peer offered, and passing over it would verify the certificate under a
// weaker one.
func remoteDTLSFingerprints(session, audio []webrtcsdp.Attribute) ([]media.DTLSFingerprint, error) {
	values := fingerprintValues(audio)
	if len(values) == 0 {
		values = fingerprintValues(session)
	}
	if len(values) == 0 {
		return nil, errors.New("remote WebRTC SDP has no DTLS fingerprint")
	}

	fingerprints := make([]media.DTLSFingerprint, 0, len(values))
	for _, value := range values {
		fields := strings.Fields(value)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid remote DTLS fingerprint %q", value)
		}
		fingerprints = append(fingerprints, media.DTLSFingerprint{
			Algorithm: fields[0],
			Value:     fields[1],
		})
	}
	return fingerprints, nil
}

// fingerprintValues returns the values of the a=fingerprint lines among attrs,
// the attributes of one level of a description, in the order given.
func fingerprintValues(attrs []webrtcsdp.Attribute) []string {
	var values []string
	for _, attr := range attrs {
		if attr.Key == "fingerprint" {
			values = append(values, attr.Value)
		}
	}
	return values
}
