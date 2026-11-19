// SPDX-License-Identifier: MPL-2.0

package media

import (
	"slices"
	"strings"
)

// The identification-tag of a stream is the value of its a=mid line (RFC 5888
// section 4). An offer that carries them fixes the tags of the session.
//
// RFC 5888 section 9.1: "The "mid" attribute is an identifier for a particular
// media stream. Therefore, the "mid" value in the offer MUST be the same as
// the "mid" value in the answer. Besides, subsequent offers (e.g., in a
// re-INVITE) SHOULD use the same "mid" value for the already existing media
// streams." Section 9.2 keeps the lines whatever becomes of the group they
// were offered in, "the "mid" lines MUST still be present in the answer", and
// its example answers a refused stream with its m= line on port 0 and its
// a=mid line, as the examples of RFC 8843 section 18.5 do. So every stream of
// an offer, the rejected ones included, keeps its tag in the answer and in the
// SDPs of ours that follow it.

// midAttribute is the name of the attribute carrying an identification-tag.
const midAttribute = "mid"

// midLine returns the a=mid line of the stream identified by tag.
func midLine(tag string) string {
	return "a=" + midAttribute + ":" + tag
}

// identificationTag returns the identification-tag among attrs, the values of
// the a= lines of one media section, or "" when the section has no a=mid line.
func identificationTag(attrs []string) string {
	for _, attr := range attrs {
		if tag, ok := strings.CutPrefix(attr, midAttribute+":"); ok {
			return tag
		}
	}
	return ""
}

// streamTags returns tags, the identification-tag of each stream of an offer
// in the offer's order, as a stream list keeps them: nil when the offer
// identifies no stream. The SDPs written around such a list are the ones
// written before any offer.
func streamTags(tags []string) []string {
	if !slices.ContainsFunc(tags, func(tag string) bool { return tag != "" }) {
		return nil
	}
	return tags
}

// tagged reports whether the offer identified a stream.
func (o offeredStreams) tagged() bool {
	return len(o.tags) > 0
}

// tag returns the identification-tag of stream i, or "" when the offer gave it
// none.
func (o offeredStreams) tag(i int) string {
	if i >= len(o.tags) {
		return ""
	}
	return o.tags[i]
}

// audioTag returns the identification-tag of the stream that carries the
// call, or "" when the offer gave it none.
func (o offeredStreams) audioTag() string {
	return o.tag(o.audio)
}

// withAudioTag returns body, an SDP of ours describing the audio stream alone,
// with the a=mid line of that stream when the offer identified it. The line
// closes the audio section, which is the body's last.
func (o offeredStreams) withAudioTag(body []byte) []byte {
	tag := o.audioTag()
	if tag == "" {
		return body
	}
	return slices.Concat(body, []byte(midLine(tag)+"\r\n"))
}
