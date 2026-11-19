// SPDX-License-Identifier: MPL-2.0

package media

import (
	cryptorand "crypto/rand"
	"math"
	"math/rand"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	webrtcsdp "github.com/pion/sdp/v3"
)

// A description with more than one m= section leaves its receiver to find
// which section an RTP stream belongs to. RFC 8843 section 9.2: "it MUST be
// possible to associate an RTP stream with the correct "m=" section", and "An
// offerer and answerer can inform each other which SSRC values they will use
// for an RTP stream by using the SDP 'ssrc' attribute [RFC5576]". An endpoint
// that places a stream by the sources its peer declared, and neither by the
// tag of the MID extension alone nor by the payload type, plays our audio only
// when the section states its source. So the audio section of an SDP of ours
// with more than one m= section declares the source the session sends as.
//
// The packets of a session are made outside it: an RTPPacketWriter chooses
// their SSRC and an RTPSessionWebrtc the CNAME of their reports, both after
// the answer is sent. For the SDP to state them, the session chooses the two
// and the first writer and the reports made over it take them from it.

// ssrcAttribute is the name of the media attribute that describes a source
// (RFC 5576 section 4.1).
const ssrcAttribute = "ssrc"

// webrtcSource is the RTP source one call's WebRTC media sends as: the SSRC of
// its packets and the CNAME of its RTCP source description. A
// MediaSessionWebrtc and the ICE generations forked from it share one, since
// the writer of the call's packets outlives an ICE restart and keeps its SSRC
// across it. It is safe for concurrent use.
type webrtcSource struct {
	// ssrc and cname are set once, when the source is made.
	ssrc  uint32
	cname string

	// taken is set once a packet writer sends as this source. One writer
	// does: two would number their packets apart under one SSRC.
	taken atomic.Bool
}

// newWebRTCSource returns a source with an SSRC and a CNAME drawn at random.
// The SSRC is never 0, which the report state of an RTPSessionWebrtc reads as
// nothing sent yet.
func newWebRTCSource() *webrtcSource {
	return &webrtcSource{
		ssrc:  rand.Uint32()%math.MaxUint32 + 1,
		cname: cryptorand.Text(),
	}
}

// sourceLocked returns the source of this session. The caller holds m.mu.
func (m *MediaSessionWebrtc) sourceLocked() *webrtcSource {
	if m.source == nil {
		m.source = newWebRTCSource()
	}
	return m.source
}

// sharedSource returns the source of this session, for Fork to hand to the ICE
// generation it creates and for the writer and the reports of the session to
// send as.
func (m *MediaSessionWebrtc) sharedSource() *webrtcSource {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sourceLocked()
}

// declaredSSRCWriter is an RTPWriter over a media session that declares the
// source it sends in its SDPs.
type declaredSSRCWriter interface {
	// takeDeclaredSSRC returns the SSRC the session declares, and true for
	// the one writer that sends as that source. Every later call returns
	// false: a further writer is a source of its own.
	takeDeclaredSSRC() (uint32, bool)
}

// takeDeclaredSSRC makes a packet writer over the session the one that sends
// as the source the session declares.
func (m *MediaSessionWebrtc) takeDeclaredSSRC() (uint32, bool) {
	if m == nil {
		return 0, false
	}
	source := m.sharedSource()
	if !source.taken.CompareAndSwap(false, true) {
		return 0, false
	}
	return source.ssrc, true
}

// takeDeclaredSSRC makes a packet writer over the RTP session the one that
// sends as the source its media session declares. An RTP session over no media
// session declares none.
func (s *RTPSessionWebrtc) takeDeclaredSSRC() (uint32, bool) {
	if s == nil {
		return 0, false
	}
	return s.Sess.takeDeclaredSSRC()
}

// writerSSRC returns the SSRC a new packet writer over writer sends with: the
// one the media session under writer declares in its SDPs, when it declares
// one and no writer sends as it yet, and otherwise one drawn at random.
func writerSSRC(writer RTPWriter) uint32 {
	if declared, ok := writer.(declaredSSRCWriter); ok {
		if ssrc, ok := declared.takeDeclaredSSRC(); ok {
			return ssrc
		}
	}
	return rand.Uint32()
}

// webRTCDeclaredSSRCs returns the SSRCs md declares for its sources, in its
// a=ssrc lines.
func webRTCDeclaredSSRCs(md *webrtcsdp.MediaDescription) []uint32 {
	var ssrcs []uint32
	for _, attr := range md.Attributes {
		if attr.Key != ssrcAttribute {
			continue
		}
		id, _, _ := strings.Cut(attr.Value, " ")
		ssrc, err := strconv.ParseUint(id, 10, 32)
		if err != nil || slices.Contains(ssrcs, uint32(ssrc)) {
			continue
		}
		ssrcs = append(ssrcs, uint32(ssrc))
	}
	return ssrcs
}

// declaredSourceLines returns the a=ssrc line of the audio section of an SDP
// of ours written around streams, or nothing when the SDP declares no source.
//
// RFC 5576 section 4.1: "For each source mentioned in SDP, the source-level
// attribute "cname", defined in Section 6.1, MUST be provided", and section
// 6.1 has it be "the CNAME value that the media sender will place in its RTCP
// SDES packets".
//
// An SDP with one m= section declares nothing: its receiver has no section to
// choose between. Nor does one whose offer declares our SSRC for a source of
// the peer, since "The SSRC IDs for sources described by an SDP answer MUST
// be distinct from the SSRC IDs for sources of that media stream in the
// offer" (RFC 5576 section 8). The caller holds m.mu.
func (m *MediaSessionWebrtc) declaredSourceLines(streams offeredStreams) []string {
	if len(streams.media) < 2 {
		return nil
	}
	source := m.sourceLocked()
	if slices.Contains(streams.audioSSRCs, source.ssrc) {
		return nil
	}
	return []string{"a=" + ssrcAttribute + ":" + strconv.FormatUint(uint64(source.ssrc), 10) + " cname:" + source.cname}
}

// withDeclaredSource returns body, an SDP of ours describing the audio stream
// alone, with the lines that declare the source the session sends: see
// declaredSourceLines. They close the audio section, which is the body's
// last. The caller holds m.mu.
func (m *MediaSessionWebrtc) withDeclaredSource(streams offeredStreams, body []byte) []byte {
	for _, line := range m.declaredSourceLines(streams) {
		body = slices.Concat(body, []byte(line+"\r\n"))
	}
	return body
}
