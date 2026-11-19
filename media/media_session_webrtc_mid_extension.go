// SPDX-License-Identifier: MPL-2.0

package media

import (
	"slices"
	"strconv"
	"strings"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	webrtcsdp "github.com/pion/sdp/v3"
)

// The identification-tag of a bundled stream travels with its media, so that
// an endpoint receiving several streams on the one transport of a BUNDLE group
// can tell which m= section a packet belongs to.
//
// RFC 8843 section 9.1: "The RTP MID header extension MUST be enabled, by
// including an SDP 'extmap' attribute [RFC8285], with a
// 'urn:ietf:params:rtp-hdrext:sdes:mid' URI value, in each bundled RTP-based
// "m=" section in every offer and answer." Section 9.2: "the offerer and
// answerer using the BUNDLE extension MUST support the mechanism defined in
// Section 15, where the offerer and answerer insert the identification-tag
// associated with an "m=" section (provided by the remote peer) into RTP and
// RTCP packets associated with a BUNDLE group."
//
// An answer can only enable what its offer mapped, under the offer's
// identifier: "the same identifier MUST be used for the same offered extension
// in the answer" (RFC 8285 section 7). So the extension is in use on a session
// whose peer offered the audio stream in a BUNDLE group with the extension
// mapped, and on no other.

const (
	// sdesMIDURI names the RTP header extension that carries an
	// identification-tag (RFC 8843 section 16.2).
	sdesMIDURI = "urn:ietf:params:rtp-hdrext:sdes:mid"

	// sdesMID is the RTCP SDES item that carries an identification-tag (RFC
	// 8843 section 15.1).
	sdesMID rtcp.SDESType = 15

	// maxOneByteExtensionID and maxOneByteExtensionLen bound what the
	// one-byte form of a header extension element carries (RFC 8285 section
	// 4.2). An element beyond either takes the two-byte form.
	maxOneByteExtensionID  = 14
	maxOneByteExtensionLen = 16

	// maxMIDLen is the longest identification-tag an SDES item and a two-byte
	// extension element carry.
	maxMIDLen = 255
)

// midExtension is the RTP header extension carrying the identification-tag of
// the audio stream, as the peer's offer mapped it. Its zero value is no
// extension: packets are sent as they are.
type midExtension struct {
	// id is the identifier the offer maps to sdesMIDURI.
	id uint8
	// tag is the identification-tag of the audio stream. It is never written
	// to, so every copy of the value shares it.
	tag []byte
}

// webRTCMIDExtension returns the header extension an offer maps to the
// identification-tag tag of its audio stream, whose attributes are media,
// under the session-level attributes session. The media-level mappings are
// read first, since an SDP has them at one level alone (RFC 8285 section 5).
//
// It returns no extension when the offer maps none, maps it to an identifier a
// packet cannot carry, qualifies it with a direction other than sendrecv, in
// which the tag would not travel both ways, or when the tag is longer than an
// extension element holds.
func webRTCMIDExtension(session, media []webrtcsdp.Attribute, tag string) midExtension {
	if tag == "" || len(tag) > maxMIDLen {
		return midExtension{}
	}
	for _, attr := range slices.Concat(media, session) {
		if attr.Key != "extmap" {
			continue
		}
		fields := strings.Fields(attr.Value)
		if len(fields) < 2 || fields[1] != sdesMIDURI {
			continue
		}
		value, direction, _ := strings.Cut(fields[0], "/")
		if direction != "" && direction != "sendrecv" {
			return midExtension{}
		}
		// An identifier above 255 only offers the extension for remapping
		// (RFC 8285 section 7), and 0 is padding.
		id, err := strconv.ParseUint(value, 10, 8)
		if err != nil || id == 0 {
			return midExtension{}
		}
		return midExtension{id: uint8(id), tag: []byte(tag)}
	}
	return midExtension{}
}

// extmapLines returns the a=extmap line that enables the extension in the
// audio section of an SDP of ours, or nothing when there is none.
func (x midExtension) extmapLines() []string {
	if x.id == 0 {
		return nil
	}
	return []string{"a=extmap:" + strconv.Itoa(int(x.id)) + " " + sdesMIDURI}
}

// tagged returns pkt as it is sent to the peer: a copy whose header carries
// the identification-tag of the audio stream, or pkt itself when the session
// has no such extension. The caller's packet is left as it is.
//
// Every packet carries the tag. RFC 8843 section 15 asks for it "in some RTP
// packets at the start of the session and whenever the SSRC changes" and
// leaves the number open; the session does not own the packets' SSRC, and a
// peer that has several m= sections cannot place a stream before a tagged
// packet reaches it.
//
// A packet whose header cannot take the element is sent without it: one that
// already has a header extension of another form than the tag needs, which the
// caller chose.
func (x midExtension) tagged(pkt *rtp.Packet) *rtp.Packet {
	if x.id == 0 {
		return pkt
	}
	tagged := *pkt
	tagged.Header.Extensions = slices.Clone(pkt.Header.Extensions)
	if !tagged.Header.Extension {
		tagged.Header.Extension = true
		tagged.Header.ExtensionProfile = rtp.ExtensionProfileOneByte
		if x.id > maxOneByteExtensionID || len(x.tag) > maxOneByteExtensionLen {
			tagged.Header.ExtensionProfile = rtp.ExtensionProfileTwoByte
		}
	}
	if err := tagged.Header.SetExtension(x.id, x.tag); err != nil {
		return pkt
	}
	return &tagged
}

// identifiedSource returns sd, the source description of an RTCP report of
// this session, with the identification-tag of the audio stream as an SDES MID
// item of each of its chunks (RFC 8843 section 15.1), or sd itself when the
// session has no MID extension. sd is left as it is.
func (m *MediaSessionWebrtc) identifiedSource(sd *rtcp.SourceDescription) *rtcp.SourceDescription {
	mid := m.packetPath().mid
	if mid.id == 0 {
		return sd
	}
	identified := &rtcp.SourceDescription{Chunks: slices.Clone(sd.Chunks)}
	for i, chunk := range identified.Chunks {
		identified.Chunks[i].Items = append(slices.Clone(chunk.Items), rtcp.SourceDescriptionItem{Type: sdesMID, Text: string(mid.tag)})
	}
	return identified
}
