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
// A rejected stream is its m= line with port 0 and no attribute. That is also
// what RFC 8843 section 7.3.3 asks of a stream rejected out of a BUNDLE group:
// the answerer "MUST assign a zero port value" and "MUST NOT include an SDP
// 'bundle-only' attribute", without which port 0 means rejected and not
// bundled.
type webrtcStreams struct {
	// current is the stream list of the last offer the session committed to.
	// Its zero value, before any offer, is the audio stream alone.
	current offeredStreams

	// staged is the stream list of an offer accepted on a negotiated session
	// whose exchange is neither committed nor rolled back, when hasStaged is
	// set. The answer to that offer is written around it, and every other SDP
	// around current until Finalize commits the exchange.
	staged    offeredStreams
	hasStaged bool
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

// webRTCOfferedStreams returns the stream list of desc: its m= lines in order,
// with audio, one of its media sections, as the one that carries the call.
func webRTCOfferedStreams(desc *webrtcsdp.SessionDescription, audio *webrtcsdp.MediaDescription) offeredStreams {
	streams := offeredStreams{media: make([]sdp.MediaDescription, len(desc.MediaDescriptions))}
	for i, md := range desc.MediaDescriptions {
		streams.media[i] = webRTCMediaLine(md)
		if md == audio {
			streams.audio = i
		}
	}
	return streams
}

// keepOfferedStreams records the stream list of the description RemoteSDP
// accepted for the first exchange of this ICE generation, when that
// description is an offer: offered says it answers an offer of ours instead,
// which keeps the list that offer was written from. Nothing runs yet that a
// failed exchange would go back to, so the list is the session's at once. The
// caller holds m.mu.
func (m *MediaSessionWebrtc) keepOfferedStreams(streams offeredStreams, offered bool) {
	if offered {
		return
	}
	m.streams.current = streams
}

// stageOfferedStreams records the stream list of the description RemoteSDP
// accepted on a negotiated session, when that description is an offer. The
// answer to it has one m= line for each of its streams, and the list becomes
// the session's when Finalize commits the exchange; Rollback drops it and the
// session keeps the list of the offer before. The caller holds m.mu.
func (m *MediaSessionWebrtc) stageOfferedStreams(streams offeredStreams, offered bool) {
	if offered {
		return
	}
	m.streams.staged = streams
	m.streams.hasStaged = true
}

// commitOfferedStreams makes a staged stream list the session's: the exchange
// that staged it is committed. The caller holds m.mu.
func (m *MediaSessionWebrtc) commitOfferedStreams() {
	if !m.streams.hasStaged {
		return
	}
	m.streams = webrtcStreams{current: m.streams.staged}
}

// dropStagedStreams forgets a staged stream list: its exchange failed, and the
// SDPs of the session stay written around the list it had. The caller holds
// m.mu.
func (m *MediaSessionWebrtc) dropStagedStreams() {
	m.streams = webrtcStreams{current: m.streams.current}
}

// localStreams returns the stream list the body LocalSDP builds is written
// around, where answered says the body answers the offer RemoteSDP accepted:
// that offer's list, which is a staged one on a negotiated session. An offer
// of ours is written around the list the session has committed to. The caller
// holds m.mu.
func (m *MediaSessionWebrtc) localStreams(answered bool) offeredStreams {
	if answered && m.streams.hasStaged {
		return m.streams.staged
	}
	return m.streams.current
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
