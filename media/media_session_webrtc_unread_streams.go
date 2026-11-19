// SPDX-License-Identifier: MPL-2.0

package media

import (
	"bytes"
	"slices"
	"strings"

	"github.com/emiago/diago/media/sdp"
	webrtcsdp "github.com/pion/sdp/v3"
)

// The SDP parser a MediaSessionWebrtc reads a description with knows the m=
// lines of the media it could meet: it refuses a media type other than audio,
// video, text, application and message, and a transport with a token outside
// its list, and with the line the whole description. An offer may carry any
// stream beside its audio, "m=image 30004 udptl t38" for one, and RFC 3264
// section 6 has it answered stream by stream: "To reject an offered stream,
// the port number in the corresponding stream in the answer MUST be set to
// zero". So a stream whose m= line the parser refuses is kept from it, and
// answered as every other stream the session does not carry: rejected, its
// m= line repeated as offered.

// webRTCParserSession is the session level of the SDP an m= line is shown to
// the parser under, to learn whether it reads the line.
const webRTCParserSession = "v=0\r\no=- 0 0 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\n"

// unreadStream is a stream of a description whose m= line the SDP parser
// refuses: the description was parsed without its section.
type unreadStream struct {
	// index is the place of the stream among those of the description.
	index int
	// media is its m= line.
	media sdp.MediaDescription
	// tag is its identification-tag, or "" when it has none: see offeredTag.
	tag string
	// formatLines are the a=rtpmap lines it keeps when it is rejected: see
	// offeredFormatLines.
	formatLines []string
}

// unreadStreams are the streams of a description the SDP parser was not
// given, in the order of the description.
type unreadStreams []unreadStream

// sdpSectionSpans returns body cut where its m= lines begin: session, what
// stands ahead of the first, and media, one span for each m= line that runs to
// the next. The spans are slices of body.
func sdpSectionSpans(body []byte) (session []byte, media [][]byte) {
	var starts []int
	for at := 0; at < len(body); {
		if bytes.HasPrefix(body[at:], []byte("m=")) {
			starts = append(starts, at)
		}
		end := bytes.IndexByte(body[at:], '\n')
		if end < 0 {
			break
		}
		at += end + 1
	}
	if len(starts) == 0 {
		return body, nil
	}
	for i, start := range starts {
		end := len(body)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		media = append(media, body[start:end])
	}
	return body[:starts[0]], media
}

// webRTCParserReadsMediaLine reports whether the SDP parser reads the m= line
// that section, one media section of a description, begins with.
func webRTCParserReadsMediaLine(section []byte) bool {
	line := section
	if end := bytes.IndexByte(section, '\n'); end >= 0 {
		line = section[:end+1]
	}
	var probe webrtcsdp.SessionDescription
	return probe.Unmarshal(slices.Concat([]byte(webRTCParserSession), line)) == nil
}

// unmarshalWebRTCDescription parses body, the SDP RemoteSDP is given. A body
// the parser refuses is parsed again without the sections whose m= line it
// does not read, when it has such sections, and those are returned as unread.
// Whatever else the parser refuses stays an error, the parser's own.
//
// The description returned has no a=group line when one of the unread streams
// has no identification-tag: see unreadStreams.allIdentified.
func unmarshalWebRTCDescription(body []byte) (webrtcsdp.SessionDescription, unreadStreams, error) {
	var desc webrtcsdp.SessionDescription
	err := desc.Unmarshal(body)
	if err == nil {
		return desc, nil, nil
	}
	// The parser refuses a body whose last line is not ended, whatever its
	// streams, and such a line could not be told from the section it ends.
	if !bytes.HasSuffix(body, []byte("\n")) {
		return desc, nil, err
	}

	session, media := sdpSectionSpans(body)
	readable := slices.Clone(session)
	var unread unreadStreams
	for i, section := range media {
		if webRTCParserReadsMediaLine(section) {
			readable = append(readable, section...)
			continue
		}
		sections, sectionErr := sdp.UnmarshalSections(section)
		if sectionErr != nil || len(sections.Media) != 1 {
			// An m= line our own reader refuses too: it has no form to
			// repeat in an answer.
			return desc, nil, err
		}
		attrs := attributeValues(sections.Media[0].Lines)
		var rtpmaps []string
		for _, attr := range attrs {
			if value, ok := strings.CutPrefix(attr, rtpmapAttribute+":"); ok {
				rtpmaps = append(rtpmaps, value)
			}
		}
		unread = append(unread, unreadStream{
			index:       i,
			media:       sections.Media[0].Media,
			tag:         identificationTag(attrs),
			formatLines: offeredFormatLines(sections.Media[0].Media.Formats, rtpmaps),
		})
	}
	if len(unread) == 0 {
		return desc, nil, err
	}

	var reduced webrtcsdp.SessionDescription
	if reducedErr := reduced.Unmarshal(readable); reducedErr != nil {
		return reduced, nil, reducedErr
	}
	if !unread.allIdentified() {
		reduced.Attributes = webRTCWithoutGroups(reduced.Attributes)
	}
	return reduced, unread, nil
}

// allIdentified reports whether every unread stream has an
// identification-tag. A description with a stream that has none groups no
// stream (RFC 5888 section 6, see webRTCStreamsAllIdentified), and the
// description parsed without that stream no longer shows it, so it is handed
// on without its a=group lines.
func (u unreadStreams) allIdentified() bool {
	return !slices.ContainsFunc(u, func(stream unreadStream) bool { return stream.tag == "" })
}

// among returns streams, the stream list read from a description parsed
// without the unread streams, with each of them back at its place: the list
// then has one stream for each m= line of the description, in its order. An
// unread stream is never the one that carries the call, so it is written
// rejected like every other stream but the audio one.
func (u unreadStreams) among(streams offeredStreams) offeredStreams {
	if len(u) == 0 {
		return streams
	}
	tags := streams.tags
	if tags == nil {
		tags = make([]string, len(streams.media))
	}
	// The unread streams are in the order of the description, so each is
	// inserted among the streams that stand ahead of it there.
	for _, stream := range u {
		streams.media = slices.Insert(streams.media, stream.index, stream.media)
		streams.formatLines = slices.Insert(streams.formatLines, stream.index, stream.formatLines)
		tags = slices.Insert(tags, stream.index, stream.tag)
		if stream.index <= streams.audio {
			streams.audio++
		}
	}
	streams.tags = streamTags(tags)
	return streams
}
