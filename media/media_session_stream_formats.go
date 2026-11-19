// SPDX-License-Identifier: MPL-2.0

package media

import (
	"slices"
	"strconv"
	"strings"
)

// The m= line of a stream an answer rejects repeats the formats of the offer:
// "Any media formats listed are ignored. At least one MUST be present" (RFC
// 3264 section 6). A dynamic payload type among them has no meaning but the
// one its a=rtpmap line gives it, and the examples of RFC 8843 section 18.5
// write a disabled section with those lines: "m=video 0 RTP/AVP 66",
// "a=mid:zen", "a=rtpmap:66 H261/90000". An endpoint that reads the formats of
// every m= line it is given refuses a description naming a payload type it
// cannot look up, so a rejected stream of a WebRTC session keeps the a=rtpmap
// lines the offer gave the formats of its m= line.

// rtpmapAttribute is the name of the attribute that maps a payload type to an
// encoding.
const rtpmapAttribute = "rtpmap"

// rtpmapPayloadType returns the payload type of value, the value of an
// a=rtpmap line of the peer, when value follows the grammar of RFC 8866
// section 6.6, "payload-type SP encoding-name "/" clock-rate [ "/"
// encoding-params ]", and false otherwise.
//
// The value is written back into our own SDPs, so one outside the grammar is
// not taken: a carriage return or a space in it would become a line or a field
// of ours that the peer chose.
func rtpmapPayloadType(value string) (string, bool) {
	payloadType, encoding, found := strings.Cut(value, " ")
	if !found {
		return "", false
	}
	if _, err := strconv.ParseUint(payloadType, 10, 7); err != nil {
		return "", false
	}
	parts := strings.Split(encoding, "/")
	if len(parts) < 2 || len(parts) > 3 {
		return "", false
	}
	if _, err := strconv.ParseUint(parts[1], 10, 32); err != nil {
		return "", false
	}
	// The name and the parameters are tokens, the parameters of an audio
	// encoding being its channel count.
	for _, part := range slices.Concat(parts[:1], parts[2:]) {
		if part == "" || strings.ContainsFunc(part, notTokenChar) {
			return "", false
		}
	}
	return payloadType, true
}

// offeredFormatLines returns the a=rtpmap lines a rejected stream keeps:
// those among rtpmaps, the values of the a=rtpmap lines of its section of the
// offer, that map a format of its m= line, one for each format and in the
// order of the offer. A value that is no rtpmap, see rtpmapPayloadType, or
// that maps a payload type the m= line does not list, is left out.
func offeredFormatLines(formats, rtpmaps []string) []string {
	var lines []string
	var mapped []string
	for _, value := range rtpmaps {
		payloadType, ok := rtpmapPayloadType(value)
		if !ok || !slices.Contains(formats, payloadType) || slices.Contains(mapped, payloadType) {
			continue
		}
		mapped = append(mapped, payloadType)
		lines = append(lines, "a="+rtpmapAttribute+":"+value)
	}
	return lines
}

// formatLinesOf returns the a=rtpmap lines stream i keeps when it is
// rejected, or nothing when the list holds none for it.
func (o offeredStreams) formatLinesOf(i int) []string {
	if i >= len(o.formatLines) {
		return nil
	}
	return o.formatLines[i]
}
