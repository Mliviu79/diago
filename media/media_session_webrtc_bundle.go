// SPDX-License-Identifier: MPL-2.0

package media

import (
	"slices"
	"strings"

	webrtcsdp "github.com/pion/sdp/v3"
)

// bundleSemantics is the semantics value of the a=group line of a BUNDLE
// group (RFC 8843 section 5).
const bundleSemantics = "BUNDLE"

// bundledAudio is what an offer says of the BUNDLE group of its audio stream.
// Its zero value is an audio stream the offer put in no group.
type bundledAudio struct {
	// grouped is set when an a=group:BUNDLE line of the offer lists the tag
	// of the audio stream. The stream then stays in that group in our answer
	// and in the SDPs of ours that follow, and is the only one there: every
	// other stream is rejected.
	grouped bool

	// mid is the RTP header extension the offer maps to the tag of a grouped
	// audio stream: see midExtension. Its zero value is no extension.
	mid midExtension
}

// webRTCBundleGroup returns the identification-tags of the BUNDLE group of
// desc that lists tag, or nil when no group does. A stream without a tag is in
// no group.
func webRTCBundleGroup(desc *webrtcsdp.SessionDescription, tag string) []string {
	if tag == "" {
		return nil
	}
	for _, attr := range desc.Attributes {
		if attr.Key != "group" {
			continue
		}
		fields := strings.Fields(attr.Value)
		if len(fields) < 2 || fields[0] != bundleSemantics {
			continue
		}
		if slices.Contains(fields[1:], tag) {
			return fields[1:]
		}
	}
	return nil
}

// webRTCIdentificationTag returns the identification-tag of md, or "" when md
// has no a=mid line or one whose value is no tag: see offeredTag.
func webRTCIdentificationTag(md *webrtcsdp.MediaDescription) string {
	tag, _ := md.Attribute(midAttribute)
	return offeredTag(tag)
}

// webRTCBundledAudio returns what desc says of the BUNDLE group of audio, one
// of its media sections.
func webRTCBundledAudio(desc *webrtcsdp.SessionDescription, audio *webrtcsdp.MediaDescription) bundledAudio {
	tag := webRTCIdentificationTag(audio)
	if webRTCBundleGroup(desc, tag) == nil {
		return bundledAudio{}
	}
	return bundledAudio{grouped: true, mid: webRTCMIDExtension(desc.Attributes, audio.Attributes, tag)}
}

// ownAudioTag is the identification-tag our own first offer gives its audio
// stream when it offers it in a BUNDLE group.
const ownAudioTag = "0"

// ownOfferStreams returns the stream list a session writes its SDPs around
// before any offer of the peer: the audio stream alone, identified as our own
// first offer identifies it. That is under the tag 0 and in a BUNDLE group of
// its own, or, when withoutBundle is set, with neither the tag nor the group.
//
// Both are an offerer's to write. The group is in "the first offer, within an
// SDP session" of an offerer that "wants to negotiate a given BUNDLE group"
// (RFC 8843 section 7.2), and an offerer that asks for none writes an SDP
// without the grouping framework, where a stream needs no tag.
func ownOfferStreams(withoutBundle bool) offeredStreams {
	if withoutBundle {
		return offeredStreams{}
	}
	return offeredStreams{tags: []string{ownAudioTag}, bundle: bundledAudio{grouped: true}}
}

// bundleGroupLines returns the session-level a=group lines of an SDP of ours
// written around the stream list.
//
// RFC 8843 section 7.3: "The answerer is only allowed to include a BUNDLE
// group in an initial BUNDLE answer if the offerer requested the BUNDLE group
// to be created in the corresponding initial BUNDLE offer" and "is only
// allowed to include a bundled "m=" section in an answer if the "m=" section
// was indicated as bundled in the corresponding offer", so the group is
// written only when the offer listed the audio stream in one: an offer
// without an a=group line, or without an a=mid line a group could list, is
// answered without the line. The list of the group is the tag of the audio
// stream alone, since section 7.3.3 has the answerer that rejects a bundled
// stream "MUST NOT place the identification-tag associated with the "m="
// section in the SDP 'group:BUNDLE' attribute identification-tag list".
//
// The list before any offer of the peer has the group of our own offer: see
// ownOfferStreams.
func (o offeredStreams) bundleGroupLines() []string {
	if !o.bundle.grouped {
		return nil
	}
	return []string{"a=group:" + bundleSemantics + " " + o.audioTag()}
}

// bundledAudioLines returns the lines that identify the audio section of an
// SDP of ours written around the stream list: its a=mid line, and the
// a=extmap line of the tag's header extension when the offer mapped one.
//
// The a=mid line is the offer's (RFC 5888 section 9.1), and an audio stream
// the offer gave no tag has none: RFC 8829 section 5.3.1 has each m= section
// of an answer contain, "If and only if present in the offer, an "a=mid"
// line". The list before any offer of the peer has the tag of our own offer:
// see ownOfferStreams.
func (o offeredStreams) bundledAudioLines() []string {
	var lines []string
	if tag := o.audioTag(); tag != "" {
		lines = append(lines, midLine(tag))
	}
	return append(lines, o.bundle.mid.extmapLines()...)
}

// hasCandidate reports whether attrs, the attributes of one media section,
// hold an a=candidate line.
func hasCandidate(attrs []webrtcsdp.Attribute) bool {
	return slices.ContainsFunc(attrs, func(attr webrtcsdp.Attribute) bool { return attr.Key == "candidate" })
}

// webRTCCandidateAttributes returns the attributes among which the remote
// candidates of audio, a media section of desc, are found: its own. RFC 8843
// section 7.1.3 and RFC 8829 section 5.2.1 have the offerer of a group that
// is not negotiated yet give each bundled section that is not bundle-only
// candidates of its own.
//
// An audio section without a candidate that is in a BUNDLE group has the
// candidates of the sections of that group that carry its ICE credentials. A
// BUNDLE group runs on one transport, whose "ICE properties are applied to
// each bundled "m=" section within the BUNDLE group" (RFC 8843 section 10),
// and an endpoint that bundles from the start gathers once, repeats one
// username fragment and password in each bundled section and lists the
// candidates in the first of them, which is not the audio section when
// another stream is offered ahead of it. A section with other credentials is
// another ICE agent, whose candidates the checks made with the audio
// stream's credentials would not reach, so it gives none.
func webRTCCandidateAttributes(desc *webrtcsdp.SessionDescription, audio *webrtcsdp.MediaDescription) []webrtcsdp.Attribute {
	if hasCandidate(audio.Attributes) {
		return audio.Attributes
	}
	group := webRTCBundleGroup(desc, webRTCIdentificationTag(audio))
	if group == nil {
		return audio.Attributes
	}
	ufrag, _ := webRTCAttribute(desc.Attributes, audio.Attributes, "ice-ufrag")
	pwd, _ := webRTCAttribute(desc.Attributes, audio.Attributes, "ice-pwd")

	var attrs []webrtcsdp.Attribute
	for _, md := range desc.MediaDescriptions {
		if md == audio {
			continue
		}
		if !slices.Contains(group, webRTCIdentificationTag(md)) {
			continue
		}
		sectionUfrag, _ := webRTCAttribute(desc.Attributes, md.Attributes, "ice-ufrag")
		sectionPwd, _ := webRTCAttribute(desc.Attributes, md.Attributes, "ice-pwd")
		if sectionUfrag != ufrag || sectionPwd != pwd {
			continue
		}
		attrs = append(attrs, md.Attributes...)
	}
	return attrs
}
