// SPDX-License-Identifier: MPL-2.0

package media

import "strings"

// rtpmapEncodingName returns the encoding name our SDP writes for codec in its
// a=rtpmap line.
//
// The formats this package knows are written in their registered spelling,
// whatever case the codec's Name carries: PCMU, PCMA and G722 (RFC 3551
// section 6), opus (RFC 7587 section 7) and telephone-event (RFC 4733).
// Encoding names compare case insensitively (RFC 4566 section 6), so a codec
// may carry any spelling, and one that came from a peer carries the peer's; the
// wire carries the registered one, which a peer comparing names literally also
// takes. Any other format keeps the name it was given.
func rtpmapEncodingName(codec Codec) string {
	for _, name := range [...]string{"PCMU", "PCMA", "G722", "opus", "telephone-event"} {
		if strings.EqualFold(codec.Name, name) {
			return name
		}
	}
	return codec.Name
}
