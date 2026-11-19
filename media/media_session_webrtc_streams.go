// SPDX-License-Identifier: MPL-2.0

package media

import (
	"strings"

	"github.com/emiago/diago/media/sdp"
	webrtcsdp "github.com/pion/sdp/v3"
)

// webrtcStreams is the stream list a MediaSessionWebrtc writes its SDPs
// around. MediaSessionWebrtc.mu guards it.
//
// RFC 3264 section 6: "For each "m=" line in the offer, there MUST be a
// corresponding "m=" line in the answer. The answer MUST contain exactly the
// same number of "m=" lines as the offer." Section 8 keeps them in every later
// SDP: "Deleted media streams from a previous SDP MUST NOT be removed in a new
// SDP". The session carries the audio stream alone, so every other stream of
// an offer is written rejected, in the offer's order: see offeredStreams.
//
// A rejected stream is its m= line with port 0, its a=mid line when the offer
// identified it, and the a=rtpmap lines the offer gave its formats: see
// offeredFormatLines. That is also what RFC 8843 section 7.3.3 asks of a
// stream rejected out of a BUNDLE group: the answerer "MUST assign a zero port
// value" and "MUST NOT include an SDP 'bundle-only' attribute", without which
// port 0 means rejected and not bundled.
//
// The list also holds how the offer identified its streams, which our SDPs
// follow: the tag of each stream, and the BUNDLE group and the tag's header
// extension of the audio one. See offeredStreams.bundleGroupLines.
type webrtcStreams struct {
	// current is the stream list of the last offer the session committed to.
	// Its zero value, before any offer, is the audio stream alone, which our
	// SDPs identify as our own first offer does: see localStreams.
	current offeredStreams

	// staged is the stream list of an offer accepted on a negotiated session
	// whose exchange is neither committed nor rolled back, when hasStaged is
	// set. The answer to that offer is written around it, and every other SDP
	// around current until Finalize commits the exchange.
	staged    offeredStreams
	hasStaged bool

	// mid is the header extension the packets of the session carry the tag
	// of the audio stream in, as the last exchange the session committed to
	// left it: the mapping of an offer of the peer, which our answer
	// repeats, or the mapping of an offer of ours that the answer kept. Its
	// zero value is no extension, which is also what an answer leaves that
	// dropped our mapping.
	mid midExtension

	// answeredMID is what mid becomes when Finalize commits the answer to a
	// re-offer of ours, when hasAnsweredMID is set. Like a staged list it
	// is dropped by Rollback.
	answeredMID    midExtension
	hasAnsweredMID bool
}

// webRTCMediaLine returns the m= line of a media section of a parsed SDP.
func webRTCMediaLine(md *webrtcsdp.MediaDescription) sdp.MediaDescription {
	return sdp.MediaDescription{
		MediaType: md.MediaName.Media,
		Port:      md.MediaName.Port.Value,
		Proto:     strings.Join(md.MediaName.Protos, "/"),
		Formats:   md.MediaName.Formats,
	}
}

// webRTCAudioStream returns the media section of desc that carries the call:
// the first audio stream on a transport this media stack carries and on a port
// other than 0, as on a MediaSession (see carriesAudio). A description with no
// such stream has the stream findWebRTCAudio picks, whose use then fails as
// the unusable stream it is, or none.
func webRTCAudioStream(desc *webrtcsdp.SessionDescription) *webrtcsdp.MediaDescription {
	for _, md := range desc.MediaDescriptions {
		if carriesAudio(webRTCMediaLine(md)) {
			return md
		}
	}
	return findWebRTCAudio(desc)
}

// webRTCRTPMaps returns the values of the a=rtpmap lines of md.
func webRTCRTPMaps(md *webrtcsdp.MediaDescription) []string {
	var values []string
	for _, attr := range md.Attributes {
		if attr.Key == rtpmapAttribute {
			values = append(values, attr.Value)
		}
	}
	return values
}

// webRTCOfferedStreams returns the stream list of a description: its m= lines
// in order, with audio, one of the media sections of desc, as the one that
// carries the call, the identification-tag of each stream, the a=rtpmap lines
// each keeps when it is rejected and what the description says of the BUNDLE
// group of the audio one. The description is desc with unread, the streams of
// it the SDP parser was not given, which stand in the list at their place: see
// unreadStreams.among.
func webRTCOfferedStreams(desc *webrtcsdp.SessionDescription, unread unreadStreams, audio *webrtcsdp.MediaDescription) offeredStreams {
	streams := offeredStreams{
		media:       make([]sdp.MediaDescription, len(desc.MediaDescriptions)),
		formatLines: make([][]string, len(desc.MediaDescriptions)),
	}
	tags := make([]string, len(desc.MediaDescriptions))
	for i, md := range desc.MediaDescriptions {
		streams.media[i] = webRTCMediaLine(md)
		tags[i] = webRTCIdentificationTag(md)
		streams.formatLines[i] = offeredFormatLines(streams.media[i].Formats, webRTCRTPMaps(md))
		if md == audio {
			streams.audio = i
		}
	}
	streams.tags = streamTags(tags)
	streams.audioSSRCs = webRTCDeclaredSSRCs(audio)
	streams.bundle = webRTCBundledAudio(desc, unread, audio)
	streams.extensions = webRTCMappedExtensions(desc, audio)
	return unread.among(streams)
}

// keepOfferedStreams records the stream list of the description RemoteSDP
// accepted for the first exchange of this ICE generation, when that
// description is an offer: offered says it answers an offer of ours instead,
// which keeps the list that offer was written from. Nothing runs yet that a
// failed exchange would go back to, so the list is the session's at once.
//
// Either description settles the header extension the packets carry the tag
// in: an offer by mapping it, which our answer repeats, and an answer by
// keeping the mapping of our offer or leaving it out (see
// midExtension.keptBy). The caller holds m.mu.
func (m *MediaSessionWebrtc) keepOfferedStreams(streams offeredStreams, offered bool) {
	if offered {
		m.streams.mid = m.localStreams(false).bundle.mid.keptBy(streams.extensions)
		return
	}
	m.streams.current = streams
	m.streams.mid = streams.bundle.mid
}

// stageOfferedStreams records the stream list of the description RemoteSDP
// accepted on a negotiated session, when that description is an offer. The
// answer to it has one m= line for each of its streams, and the list becomes
// the session's when Finalize commits the exchange; Rollback drops it and the
// session keeps the list of the offer before.
//
// A description that answers a re-offer of ours keeps the list that offer
// was written from, and stages what it leaves of the header extension the
// offer mapped: see midExtension.keptBy. The caller holds m.mu.
func (m *MediaSessionWebrtc) stageOfferedStreams(streams offeredStreams, offered bool) {
	if offered {
		m.streams.answeredMID = m.localStreams(false).bundle.mid.keptBy(streams.extensions)
		m.streams.hasAnsweredMID = true
		return
	}
	m.streams.staged = streams
	m.streams.hasStaged = true
}

// commitOfferedStreams makes what an exchange staged the session's: the
// stream list of an offer of the peer with the header extension it mapped,
// and what the answer to an offer of ours left of the extension that offer
// mapped. The exchange is committed. The caller holds m.mu.
func (m *MediaSessionWebrtc) commitOfferedStreams() {
	committed := webrtcStreams{current: m.streams.current, mid: m.streams.mid}
	if m.streams.hasStaged {
		committed.current = m.streams.staged
		committed.mid = m.streams.staged.bundle.mid
	}
	if m.streams.hasAnsweredMID {
		committed.mid = m.streams.answeredMID
	}
	m.streams = committed
}

// dropStagedStreams forgets what an exchange staged: it failed, the SDPs of
// the session stay written around the list it had and its packets carry the
// tag as they did. The caller holds m.mu.
func (m *MediaSessionWebrtc) dropStagedStreams() {
	m.streams = webrtcStreams{current: m.streams.current, mid: m.streams.mid}
}

// ofAnOffer reports whether the list is that of an offer of the peer, which
// holds at least the m= line of the audio stream. The zero value is the list
// before any offer.
func (o offeredStreams) ofAnOffer() bool {
	return len(o.media) > 0
}

// localStreams returns the stream list the body LocalSDP builds is written
// around, where answered says the body answers the offer RemoteSDP accepted:
// that offer's list, which is a staged one on a negotiated session. An offer
// of ours is written around the list the session has committed to, as an
// offer has it: see offeredStreams.inOwnOffer.
//
// A session that has committed to no offer of the peer writes around the list
// of our own first offer, in the form Config.OfferWithoutBundle chooses. The
// form is read from the configuration each time, which Fork copies, so every
// SDP of ours keeps the form of the first until an offer of the peer is
// committed. The caller holds m.mu.
func (m *MediaSessionWebrtc) localStreams(answered bool) offeredStreams {
	if answered && m.streams.hasStaged {
		return m.streams.staged
	}
	if !m.streams.current.ofAnOffer() {
		return ownOfferStreams(m.Config.OfferWithoutBundle)
	}
	if answered {
		return m.streams.current
	}
	return m.streams.current.inOwnOffer()
}

// inOwnOffer returns the list as an offer of ours is written around it: with
// its streams and their identification, with the header extension an offer of
// ours maps to the tag of a bundled audio stream (see ownOfferMID), and
// without what only the answer to an offer of the peer repeats, which is its
// a=rtcp-mux-only line (see rtcpMuxOnlyAttribute). The list itself is left as
// it is.
func (o offeredStreams) inOwnOffer() offeredStreams {
	o.bundle.muxOnly = false
	o.bundle.mid = o.ownOfferMID()
	return o
}

// forkedStreams returns the stream list of this session, for Fork to hand to
// the ICE generation it creates: an ICE restart modifies the same session, so
// its SDPs keep the streams of the offer, the rejected ones included (RFC 3264
// section 8), until that generation accepts an offer of its own. A list is
// replaced and never changed, so the two sessions can share it.
func (m *MediaSessionWebrtc) forkedStreams() offeredStreams {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.streams.current
}
