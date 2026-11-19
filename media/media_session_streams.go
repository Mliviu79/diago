// SPDX-License-Identifier: MPL-2.0

package media

import (
	"bytes"
	"fmt"

	"github.com/emiago/diago/media/sdp"
)

// offeredStreams is the stream list of an offer: its m= lines in order, and
// audio, the index of the one that carries the call. Its zero value, before
// any offer, is the audio stream alone.
type offeredStreams struct {
	media []sdp.MediaDescription
	audio int
}

// rejected returns the m= lines rejecting every stream but the audio one: those
// before it and those after it. RFC 3264 section 6: "To reject an offered
// stream, the port number in the corresponding stream in the answer MUST be
// set to zero. Any media formats listed are ignored. At least one MUST be
// present". The line keeps the offered transport and formats and carries no
// attribute.
func (o offeredStreams) rejected() (before, after []string) {
	for i, md := range o.media {
		if i == o.audio {
			continue
		}
		line := (&sdp.MediaDescription{MediaType: md.MediaType, Proto: md.Proto, Formats: md.Formats}).String()
		if i < o.audio {
			before = append(before, line)
		} else {
			after = append(after, line)
		}
	}
	return before, after
}

// around returns body, an SDP of ours describing the audio stream alone, with
// every other stream of the list rejected in its place around the audio
// section, so that the body has one m= line for each of the offer's, in the
// offer's order (RFC 3264 section 6). A body is returned as it is when the
// list rejects nothing, which a list of one audio stream and the zero value
// both do.
func (o offeredStreams) around(body []byte) []byte {
	before, after := o.rejected()
	if len(before)+len(after) == 0 {
		return body
	}
	// The audio section runs from the body's one m= line to its end. A body
	// without an m= line describes no stream to write the others around.
	audio := bytes.Index(body, []byte("\r\nm="))
	if audio < 0 {
		return body
	}
	audio += len("\r\n")

	var out bytes.Buffer
	write := func(lines []string) {
		for _, l := range lines {
			out.WriteString(l)
			out.WriteString("\r\n")
		}
	}
	out.Write(body[:audio])
	write(before)
	out.Write(body[audio:])
	write(after)
	return out.Bytes()
}

// carriesAudio reports whether md is a stream that can carry the call: an audio
// stream on a transport this media stack carries and on a port other than 0,
// since an offered stream on port 0 "MUST NOT be used" (RFC 3264 section 5.1).
func carriesAudio(md sdp.MediaDescription) bool {
	if md.MediaType != "audio" || md.Port <= 0 {
		return false
	}
	_, ok := parseRTPProfile(md.Proto)
	return ok
}

// audioStream returns the index of the media section that carries the call:
// the first one that can, see carriesAudio. With no such stream it is the
// first audio stream, whose use then fails as the unusable stream it is, and
// with no audio stream at all -1.
func audioStream(sections []sdp.MediaSection) int {
	first := -1
	for i, sec := range sections {
		if sec.Media.MediaType != "audio" {
			continue
		}
		if first < 0 {
			first = i
		}
		if carriesAudio(sec.Media) {
			return i
		}
	}
	return first
}

// readAudioStream reads body section by section and returns the stream that
// carries the call: sd, the session level together with that stream's own
// section, md, its m= line, and streams, the body's stream list.
//
// sd holds no line of another stream: a c=, a=rtpmap or a=fingerprint of a
// video section says nothing of the audio, and a c= inside the audio section
// governs it whatever the other sections carry (RFC 4566 section 5.7). A body
// with no audio stream is an [ErrNoCommonMedia].
func readAudioStream(body []byte) (sd sdp.SessionDescription, md sdp.MediaDescription, streams offeredStreams, err error) {
	sections, err := sdp.UnmarshalSections(body)
	if err != nil {
		return nil, md, streams, fmt.Errorf("fail to parse received SDP: %w", err)
	}
	audio := audioStream(sections.Media)
	if audio < 0 {
		return nil, md, streams, fmt.Errorf("%w: no audio media description", ErrNoCommonMedia)
	}

	streams = offeredStreams{media: make([]sdp.MediaDescription, len(sections.Media)), audio: audio}
	for i, sec := range sections.Media {
		streams.media[i] = sec.Media
	}
	return sections.Description(audio), sections.Media[audio].Media, streams, nil
}

// keepOfferedStreams records the stream list of the body RemoteSDP is
// applying, when that body is an offer.
//
// RFC 3264 section 6: "For each "m=" line in the offer, there MUST be a
// corresponding "m=" line in the answer. The answer MUST contain exactly the
// same number of "m=" lines as the offer." An offer's stream list is what our
// answer, and every later SDP of ours, is written around. An answer to our own
// offer keeps the list that offer was written from.
func (s *MediaSession) keepOfferedStreams(streams offeredStreams, offered bool) {
	if offered {
		return
	}
	s.offeredStreams = streams
}
