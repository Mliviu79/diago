// SPDX-License-Identifier: MPL-2.0

package media

import (
	"slices"

	webrtcsdp "github.com/pion/sdp/v3"
)

// An offerer that cannot send and receive RTCP on a port of its own says so
// with a=rtcp-mux-only beside a=rtcp-mux (RFC 8858 section 3). The session
// multiplexes RTP and RTCP on every call and refuses a description without
// a=rtcp-mux, so it meets the attribute by what it does already; what is left
// is whether the answer repeats it.
//
// Inside a BUNDLE group it does. RFC 8843 section 9.3.1.2: "if the "m="
// section that is selected as the offerer-tagged "m=" section contained an
// SDP 'rtcp-mux-only' attribute, the answerer MUST include an SDP
// 'rtcp-mux-only' attribute in the answerer-tagged "m=" section." The audio
// stream is the one bundled section the session accepts, so it is both: the
// first section of the offer's list the answerer neither rejects nor moves
// out (section 7.3.1), and the only tag of the answer's list.
//
// Outside one it does not. RFC 8858 section 4.3: "The answerer MUST NOT
// associate an SDP 'rtcp-mux-only' attribute with an SDP media description
// ("m=" line) in the answer", where the a=rtcp-mux line of the answer is what
// accepts the multiplexing. A stream the session rejects carries neither
// line.
//
// An offer of ours carries none. RFC 8843 section 9.3.1.1 leaves it to the
// offerer, which "MAY include an SDP 'rtcp-mux-only' attribute", and RFC 8858
// section 4.2 asks for it only of an offerer that "wants to indicate
// exclusive RTP/RTCP multiplexing".

// rtcpMuxOnlyAttribute is the name of the media-level attribute by which an
// offerer states that it multiplexes RTP and RTCP and has no port for RTCP
// alone (RFC 8858 section 3).
const rtcpMuxOnlyAttribute = "rtcp-mux-only"

// webRTCRTCPMuxOnly reports whether md, a media section of an offer, carries
// a=rtcp-mux-only. The attribute is a media-level one (RFC 8858 section 3),
// so a line at the session level is not read for it.
func webRTCRTCPMuxOnly(md *webrtcsdp.MediaDescription) bool {
	return slices.ContainsFunc(md.Attributes, func(attr webrtcsdp.Attribute) bool { return attr.Key == rtcpMuxOnlyAttribute })
}

// rtcpMuxOnlyLines returns the a=rtcp-mux-only line of the audio section of
// an SDP of ours written around the stream list, or nothing when the section
// has none: see bundledAudio.muxOnly.
func (o offeredStreams) rtcpMuxOnlyLines() []string {
	if !o.bundle.muxOnly {
		return nil
	}
	return []string{"a=" + rtcpMuxOnlyAttribute}
}
