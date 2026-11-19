// SPDX-License-Identifier: MPL-2.0

package media

import (
	"slices"
	"strconv"
	"strings"

	webrtcsdp "github.com/pion/sdp/v3"
)

// An offer of ours that has a BUNDLE group maps the MID header extension in
// its audio section. RFC 8843 section 9.1: "The RTP MID header extension MUST
// be enabled, by including an SDP 'extmap' attribute [RFC8285], with a
// 'urn:ietf:params:rtp-hdrext:sdes:mid' URI value, in each bundled RTP-based
// "m=" section in every offer and answer." That is the first offer of a
// session in its default form, and every later offer of a session whose audio
// stream is bundled, whether or not the peer's own offer mapped the
// extension. An offer without a BUNDLE group maps nothing.
//
// The peer's answer decides whether the tag then travels. RFC 8285 section 7:
// "An answerer that has no desire to receive the extension or does not
// understand the extension SHOULD remove it from the SDP answer", and "the
// same identifier MUST be used for the same offered extension in the answer".
// So the packets of the session carry the tag once an answer holds our mapping
// under our identifier, and stop carrying it once an answer leaves it out.
//
// The identifier is ours to choose in an offer. "The local identifiers MUST be
// unique in an RTP session" and "Identifier values in the valid range MUST NOT
// be altered (remapped)" (RFC 8285 section 7), and "All the "m=" lines in a
// BUNDLE group are considered to be part of the same local identifier (ID)
// space", where an extension offered in several of them "MUST use the same ID
// in all of these "m=" lines". So an offer of ours keeps the identifier the
// peer's offer gave the extension, and otherwise takes one that offer mapped
// to nothing.

const (
	// extmapAttribute is the name of the attribute that maps an identifier
	// to an RTP header extension (RFC 8285 section 5).
	extmapAttribute = "extmap"

	// maxExtensionID is the highest identifier a header extension element
	// carries, in the two-byte form (RFC 8285 section 4.3). An a=extmap line
	// above it only offers its extension for remapping (section 7).
	maxExtensionID = 255

	// reservedOneByteExtensionID is the identifier the one-byte form keeps
	// for a later extension of the form itself (RFC 8285 section 4.2).
	reservedOneByteExtensionID = 15
)

// The direction qualifiers of an a=extmap line (RFC 8285 section 5), each
// "with relation to the device being configured": the peer, in a description
// of the peer. A line without one runs both ways.
const (
	extmapSendrecv = "sendrecv"
	extmapRecvonly = "recvonly"
)

// mappedExtensions is what a description of the peer maps to RTP header
// extensions in its a=extmap lines.
type mappedExtensions struct {
	// ids holds the identifiers the description maps to an extension, at the
	// session level and in every media section its SDP parser reads, each
	// once and in the order of the description. Only the identifiers a packet
	// can carry are kept.
	ids []uint8

	// mid is the identifier the audio stream of the description maps to
	// sdesMIDURI, or 0 when it maps the extension to none a packet can
	// carry.
	mid uint8

	// midDirection is the direction qualifier of that mapping, empty for a
	// mapping without one.
	midDirection string
}

// webRTCExtmap returns what attr, an attribute of a description, maps when it
// is an a=extmap line: the identifier, 0 for one a packet cannot carry, the
// direction qualifier, empty when the line has none, and the URI of the
// extension.
func webRTCExtmap(attr webrtcsdp.Attribute) (id uint8, direction, uri string, ok bool) {
	if attr.Key != extmapAttribute {
		return 0, "", "", false
	}
	fields := strings.Fields(attr.Value)
	if len(fields) < 2 {
		return 0, "", "", false
	}
	value, direction, _ := strings.Cut(fields[0], "/")
	// An identifier above 255 only offers the extension for remapping (RFC
	// 8285 section 7), and 0 is padding: neither is one a packet carries.
	parsed, err := strconv.ParseUint(value, 10, 8)
	if err != nil {
		parsed = 0
	}
	return uint8(parsed), direction, fields[1], true
}

// webRTCMIDMapping returns the identifier and the direction qualifier under
// which a stream maps sdesMIDURI, where media are the attributes of its
// section and session those of the session level. The media-level mappings
// are read first, since an SDP has them at one level alone (RFC 8285 section
// 5), and the first mapping of the extension is the stream's. The identifier
// is 0 when the stream does not map the extension, or maps it to an
// identifier a packet cannot carry.
func webRTCMIDMapping(session, media []webrtcsdp.Attribute) (id uint8, direction string) {
	for _, attr := range slices.Concat(media, session) {
		mapped, qualifier, uri, ok := webRTCExtmap(attr)
		if !ok || uri != sdesMIDURI {
			continue
		}
		return mapped, qualifier
	}
	return 0, ""
}

// webRTCMappedExtensions returns what desc maps to RTP header extensions,
// with audio, one of its media sections, as the stream that carries the call.
func webRTCMappedExtensions(desc *webrtcsdp.SessionDescription, audio *webrtcsdp.MediaDescription) mappedExtensions {
	var mapped mappedExtensions
	keep := func(attrs []webrtcsdp.Attribute) {
		for _, attr := range attrs {
			id, _, _, ok := webRTCExtmap(attr)
			if !ok || id == 0 || slices.Contains(mapped.ids, id) {
				continue
			}
			mapped.ids = append(mapped.ids, id)
		}
	}
	keep(desc.Attributes)
	for _, md := range desc.MediaDescriptions {
		keep(md.Attributes)
	}
	mapped.mid, mapped.midDirection = webRTCMIDMapping(desc.Attributes, audio.Attributes)
	return mapped
}

// freeID returns an identifier the description maps to no extension: the
// lowest the one-byte form of an extension element carries, or the lowest of
// the two-byte form when the description maps every one of those. It returns
// 0 when the description leaves none.
func (x mappedExtensions) freeID() uint8 {
	for id := 1; id <= maxExtensionID; id++ {
		if id == reservedOneByteExtensionID || slices.Contains(x.ids, uint8(id)) {
			continue
		}
		return uint8(id)
	}
	return 0
}

// ownOfferMID returns the header extension an offer of ours written around
// the list maps to the tag of its audio stream, or none when that stream is
// in no BUNDLE group.
//
// The mapping is the one the list's offer made, when it made one that runs
// both ways. Otherwise the identifier is the one that offer gave the
// extension in a mapping we could not use, or one it mapped to nothing: see
// mappedExtensions.freeID. The list before any offer of the peer maps
// nothing, so the extension of our own first offer has the identifier 1.
//
// No extension is returned for a tag longer than an extension element holds,
// or when the offer left no identifier.
func (o offeredStreams) ownOfferMID() midExtension {
	if !o.bundle.grouped {
		return midExtension{}
	}
	if o.bundle.mid.id != 0 {
		return o.bundle.mid
	}
	tag := o.audioTag()
	if tag == "" || len(tag) > maxMIDLen {
		return midExtension{}
	}
	id := o.extensions.mid
	if id == 0 {
		id = o.extensions.freeID()
	}
	if id == 0 {
		return midExtension{}
	}
	return midExtension{id: id, tag: []byte(tag)}
}

// keptBy returns x, the extension an offer of ours mapped, as the answer that
// maps answer leaves it: x itself when the answer holds the mapping under the
// offer's identifier in a direction in which the peer receives the tag, and
// no extension otherwise.
//
// RFC 8285 section 7 has the answerer that does not want the extension
// "remove it from the SDP answer", use "the same identifier" when it keeps it,
// and mark it "recvonly" when it wants to receive it and not to send it.
func (x midExtension) keptBy(answer mappedExtensions) midExtension {
	if x.id == 0 || answer.mid != x.id {
		return midExtension{}
	}
	switch answer.midDirection {
	case "", extmapSendrecv, extmapRecvonly:
		return x
	}
	return midExtension{}
}
