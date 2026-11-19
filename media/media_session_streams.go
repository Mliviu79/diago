// SPDX-License-Identifier: MPL-2.0

package media

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/emiago/diago/media/sdp"
)

// offeredStreams is the stream list of an offer: its m= lines in order, and
// audio, the index of the one that carries the call. Its zero value, before
// any offer, is the audio stream alone.
type offeredStreams struct {
	media []sdp.MediaDescription
	audio int

	// tags holds the identification-tag the offer gave each stream of media
	// in its a=mid line, empty for a stream it gave none. It is nil when the
	// offer identifies no stream: see streamTags. The list a WebRTC session
	// writes its own first offer around holds the tag of ours instead: see
	// ownOfferStreams.
	tags []string

	// formatLines holds, for each stream of media, the a=rtpmap lines it keeps
	// when it is rejected: see offeredFormatLines. Only the WebRTC media type
	// reads an offer for them, so the list of a MediaSession holds none and
	// rejects a stream with its m= line and its tag alone.
	formatLines [][]string

	// audioSSRCs holds the SSRCs the offer declares for the sources of its
	// audio stream, in its a=ssrc lines (RFC 5576). Only the WebRTC media type
	// reads an offer for them: see MediaSessionWebrtc.declaredSourceLines.
	audioSSRCs []uint32

	// bundle is what the offer says of the BUNDLE group of its audio stream.
	// Only the WebRTC media type reads an offer for it: a MediaSession does
	// not carry several streams on one transport, and answers a group it
	// does not implement without the a=group line (RFC 5888 section 9.2).
	bundle bundledAudio

	// extensions is what the offer maps to RTP header extensions. Only the
	// WebRTC media type reads a description for it, to give the extension an
	// offer of ours maps an identifier of its own and to learn whether an
	// answer kept that mapping: see ownOfferMID.
	extensions mappedExtensions
}

// rejected returns the lines rejecting every stream but the audio one: those
// before it and those after it. RFC 3264 section 6: "To reject an offered
// stream, the port number in the corresponding stream in the answer MUST be
// set to zero. Any media formats listed are ignored. At least one MUST be
// present". The m= line keeps the offered transport and formats. A stream the
// offer identified keeps its a=mid line, see midLine, and the a=rtpmap lines
// the list holds for it, see offeredFormatLines, and carries no other
// attribute.
func (o offeredStreams) rejected() (before, after []string) {
	for i, md := range o.media {
		if i == o.audio {
			continue
		}
		lines := []string{(&sdp.MediaDescription{MediaType: md.MediaType, Proto: md.Proto, Formats: md.Formats}).String()}
		if tag := o.tag(i); tag != "" {
			lines = append(lines, midLine(tag))
		}
		lines = append(lines, o.formatLinesOf(i)...)
		if i < o.audio {
			before = append(before, lines...)
		} else {
			after = append(after, lines...)
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

// streamAttributes are the a= values an SDP gives one stream, apart by the
// level they stand at: session, ahead of the first m= line, and media, in the
// stream's own section. An attribute whose media-level value replaces the
// session-level one instead of joining it, as RFC 8122 section 5 has
// a=fingerprint, is read from here, since the stream's description holds both
// levels as one list.
type streamAttributes struct {
	session []string
	media   []string
}

// all returns the values of both levels, the session level first, in the
// order the stream's description has them.
func (a streamAttributes) all() []string {
	return slices.Concat(a.session, a.media)
}

// attributeValues returns the values of the a= lines among lines.
func attributeValues(lines []sdp.Line) []string {
	var values []string
	for _, l := range lines {
		if l.Type == "a" {
			values = append(values, l.Value)
		}
	}
	return values
}

// readAudioStream reads body section by section and returns the stream that
// carries the call: sd, the session level together with that stream's own
// section, md, its m= line, streams, the body's stream list, and attrs, the
// stream's attributes by level.
//
// sd holds no line of another stream: a c=, a=rtpmap or a=fingerprint of a
// video section says nothing of the audio, and a c= inside the audio section
// governs it whatever the other sections carry (RFC 4566 section 5.7). A body
// with no audio stream is an [ErrNoCommonMedia].
func readAudioStream(body []byte) (sd sdp.SessionDescription, md sdp.MediaDescription, streams offeredStreams, attrs streamAttributes, err error) {
	sections, err := sdp.UnmarshalSections(body)
	if err != nil {
		return nil, md, streams, attrs, fmt.Errorf("fail to parse received SDP: %w", err)
	}
	audio := audioStream(sections.Media)
	if audio < 0 {
		return nil, md, streams, attrs, fmt.Errorf("%w: no audio media description", ErrNoCommonMedia)
	}

	streams = offeredStreams{media: make([]sdp.MediaDescription, len(sections.Media)), audio: audio}
	tags := make([]string, len(sections.Media))
	for i, sec := range sections.Media {
		streams.media[i] = sec.Media
		tags[i] = identificationTag(attributeValues(sec.Lines))
	}
	streams.tags = streamTags(tags)
	attrs = streamAttributes{
		session: attributeValues(sections.Session),
		media:   attributeValues(sections.Media[audio].Lines),
	}
	return sections.Description(audio), sections.Media[audio].Media, streams, attrs, nil
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
