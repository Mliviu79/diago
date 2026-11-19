// SPDX-License-Identifier: MPL-2.0

package media

import (
	"slices"
	"strings"
)

// CodecsWithRegisteredNames returns a copy of codecs in which the formats this
// package knows are named in their registered spelling: PCMU, PCMA and G722
// (RFC 3551 section 6), opus (RFC 7587 section 7) and telephone-event
// (RFC 4733 section 7.1.1). A codec of any other format keeps its name, nothing
// but a name changes, and codecs itself is left as it is. No codecs give nil,
// which a configuration reads as codecs that were not set.
//
// An SDP we write names a format by its codec's Name, and the codec negotiation
// settles on keeps the Name of the local codec the peer's matched. The spelling
// of the codecs a session is configured with is therefore the spelling of
// every offer and answer it writes.
//
// Encoding names compare case insensitively (RFC 4855 section 3), so either
// spelling describes the same format, and [CodecAudioUlaw], [CodecAudioAlaw]
// and [CodecAudioG722] are named in lower case. Configure the codecs with the
// result of this function for a peer that takes only the registered spelling.
func CodecsWithRegisteredNames(codecs ...Codec) []Codec {
	named := slices.Clone(codecs)
	for i := range named {
		named[i].Name = registeredEncodingName(named[i].Name)
	}
	return named
}

// registeredEncodingName returns the registered spelling of name where name is
// one of the formats this package knows in any case, and name itself otherwise.
func registeredEncodingName(name string) string {
	for _, registered := range [...]string{"PCMU", "PCMA", "G722", "opus", "telephone-event"} {
		if strings.EqualFold(name, registered) {
			return registered
		}
	}
	return name
}
