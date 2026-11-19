// SPDX-License-Identifier: MPL-2.0

package sdp

import (
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Line is one <type>=<value> line of an SDP body.
type Line struct {
	Type  string
	Value string
}

// MediaSection is one media description of an SDP body: its m= line and the
// lines after it up to the next m= line (RFC 4566 section 5).
type MediaSection struct {
	Media MediaDescription
	// Lines are the section's lines, its m= line first.
	Lines []Line
}

// Sections is an SDP body read section by section: the session-level lines,
// which come before the first m= line, and every media description in the
// order of the body.
type Sections struct {
	Session []Line
	Media   []MediaSection
}

// UnmarshalSections reads data section by section, line by line as Unmarshal
// does. An m= line that does not parse is an error, since the answer to an
// offer has one m= line for each of the offer's (RFC 3264 section 6).
func UnmarshalSections(data []byte) (Sections, error) {
	reader := bufReader.Get().(*bytes.Buffer)
	defer bufReader.Put(reader)
	reader.Reset()
	reader.Write(data)

	var s Sections
	for {
		line, err := nextLine(reader)
		if err != nil {
			if err == io.EOF {
				return s, nil
			}
			return s, err
		}

		if len(line) < 2 {
			continue
		}

		ind := strings.Index(line, "=")
		if ind < 1 {
			return s, fmt.Errorf("Not a type=value line found. line=%q", line)
		}
		l := Line{Type: line[:ind], Value: line[ind+1:]}

		if l.Type == "m" {
			md, err := parseMediaLine(l.Value)
			if err != nil {
				return s, fmt.Errorf("media description %d: %w", len(s.Media), err)
			}
			s.Media = append(s.Media, MediaSection{Media: md, Lines: []Line{l}})
			continue
		}
		if n := len(s.Media); n > 0 {
			s.Media[n-1].Lines = append(s.Media[n-1].Lines, l)
			continue
		}
		s.Session = append(s.Session, l)
	}
}

// Description returns the session-level lines and the lines of media section i
// as one SessionDescription. It describes that stream alone: a line of another
// section, a c= or an a=rtpmap of another stream, is never read as one of its
// own. Its ConnectionInformation is therefore the section's own c= line when
// the section has one and the session-level one otherwise (RFC 4566 section
// 5.7), however many m= lines the body has.
func (s Sections) Description(i int) SessionDescription {
	sd := SessionDescription{}
	for _, l := range s.Session {
		sd[l.Type] = append(sd[l.Type], l.Value)
	}
	for _, l := range s.Media[i].Lines {
		sd[l.Type] = append(sd[l.Type], l.Value)
	}
	return sd
}

// parseMediaLine parses the value of an m= line. RFC 4566 section 5.14 gives it
// a media type, a port, a transport protocol and at least one format.
func parseMediaLine(v string) (MediaDescription, error) {
	md := MediaDescription{}
	fields := strings.Fields(v)
	if len(fields) < 4 {
		return md, fmt.Errorf("Not enough fields in media description")
	}

	md.MediaType = fields[0]

	ports := strings.Split(fields[1], "/")
	md.Port, _ = strconv.Atoi(ports[0])
	if len(ports) > 1 {
		md.PortNumbers, _ = strconv.Atoi(ports[1])
	}

	md.Proto = fields[2]

	md.Formats = fields[3:]
	return md, nil
}
