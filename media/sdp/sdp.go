// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package sdp

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
)

var bufReader = sync.Pool{
	New: func() interface{} {
		// The Pool's New function should generally only return pointer
		// types, since a pointer can be put into the return interface
		// value without an allocation:
		return new(bytes.Buffer)
	},
}

const (
	// https://datatracker.ietf.org/doc/html/rfc4566#section-6
	ModeRecvonly string = "recvonly"
	ModeSendrecv string = "sendrecv"
	ModeSendonly string = "sendonly"
	ModeInactive string = "inactive"
)

type SessionDescription map[string][]string

func (sd SessionDescription) Values(key string) []string {
	return sd[key]
}

func (sd SessionDescription) Value(key string) string {
	values := sd[key]
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

// MediaDescription represents a media type.
// m=<media> <port>/<number of ports> <proto> <fmt> ...
// https://tools.ietf.org/html/rfc4566#section-5.14
type MediaDescription struct {
	MediaType string

	Port        int
	PortNumbers int

	Proto string

	Formats []string
}

func (m *MediaDescription) String() string {
	ports := strconv.Itoa(m.Port)
	if m.PortNumbers > 0 {
		ports += "/" + strconv.Itoa(m.PortNumbers)
	}

	return fmt.Sprintf("m=%s %s %s %s", m.MediaType, ports, m.Proto, strings.Join(m.Formats, " "))
}

func (sd SessionDescription) MediaDescription(mediaType string) (MediaDescription, error) {
	values := sd.Values("m")
	md := MediaDescription{}
	if len(values) > 1 {
		return md, fmt.Errorf("more than 1 media line for type %q", mediaType)
	}

	var v string
	for _, val := range values {
		ind := strings.Index(val, " ")
		if ind < 1 {
			continue
		}
		media := val[:ind]
		if media == mediaType {
			v = val
			break
		}
	}

	if v == "" {
		return md, fmt.Errorf("Media not found for %q", mediaType)
	}

	return parseMediaLine(v)
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

// c=<nettype> <addrtype> <connection-address>
// https://tools.ietf.org/html/rfc4566#section-5.7
type ConnectionInformation struct {
	NetworkType string
	AddressType string
	IP          net.IP
	TTL         int
	Range       int
}

// ConnectionInformation returns the connection information that applies to the
// media stream. Per RFC 4566 section 5.7 a c= inside the media section overrides
// the session level one, so the last c= wins. Line order alone cannot bind a c=
// to its section, so this is only sound while the body carries a single m= line,
// which MediaDescription already requires. Bodies with more media lines are
// rejected here as well rather than answering with an address that may belong to
// another stream.
func (sd SessionDescription) ConnectionInformation() (ci ConnectionInformation, err error) {
	if len(sd.Values("m")) > 1 {
		return ci, fmt.Errorf("more than 1 media line")
	}

	values := sd.Values("c")
	if len(values) == 0 {
		return ci, fmt.Errorf("Connection information does not exists")
	}
	v := values[len(values)-1]

	fields := strings.Fields(v)
	if len(fields) < 3 {
		// Body comes from remote peer. Indexing unchecked turns a truncated
		// c= line into a panic.
		return ci, fmt.Errorf("sdp - not enough fields in connection information c=%s", v)
	}
	ci.NetworkType = fields[0]
	ci.AddressType = fields[1]
	addr := strings.Split(fields[2], "/")
	ci.IP = net.ParseIP(addr[0])

	switch ci.AddressType {
	case "IP4":
		ci.IP = ci.IP.To4()
		if ci.IP == nil {
			return ci, fmt.Errorf("sdp - failed to convert to IP4 c=%s", v)
		}
	case "IP6":
		ci.IP = ci.IP.To16()
		if ci.IP == nil {
			return ci, fmt.Errorf("sdp - failed to convert to IP6 c=%s", v)
		}
	}

	if len(addr) > 1 {
		ci.TTL, _ = strconv.Atoi(addr[1])
	}

	if len(addr) > 2 {
		ci.Range, _ = strconv.Atoi(addr[2])
	}
	return ci, nil
}

type SessionInformation struct {
	Origin         string
	SessionID      uint64
	SessionVersion uint64
	NetworkType    string
	AddressType    string
	Address        string // Informational address
}

func (sd SessionDescription) SessionInformation() (i SessionInformation, err error) {
	v := sd.Value("o")
	if v == "" {
		return i, fmt.Errorf("Connection information does not exists")
	}
	fields := strings.Fields(v)
	if len(fields) < 6 {
		return i, fmt.Errorf("Not enough session fields")
	}
	i.Origin = fields[0]
	sessId, sessVersion := fields[1], fields[2]
	i.SessionID, err = strconv.ParseUint(sessId, 10, 64)
	if err != nil {
		return i, err
	}
	i.SessionVersion, err = strconv.ParseUint(sessVersion, 10, 64)
	if err != nil {
		return i, err
	}
	i.NetworkType = fields[3]
	i.AddressType = fields[4]
	i.Address = fields[5]
	return i, nil
}

func (sd SessionDescription) MediaDirection() string {
	attrs := sd.Values("a")
	if attrs == nil {
		// Assume default per
		return ModeSendrecv
	}
	mode := ModeSendrecv
	for _, v := range attrs {
		switch v {
		case ModeSendrecv, ModeSendonly, ModeRecvonly, ModeInactive:
			mode = v
		}
	}
	return mode
}

// Unmarshal is non validate version of sdp parsing
// Validation of values needs to be seperate
// NOT OPTIMIZED
func Unmarshal(data []byte, sdptr *SessionDescription) error {
	sd := *sdptr
	return readLines(data, func(key, value string) error {
		sd[key] = append(sd[key], value)
		return nil
	})
}

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
	var s Sections
	err := readLines(data, func(key, value string) error {
		l := Line{Type: key, Value: value}
		if key == "m" {
			md, err := parseMediaLine(value)
			if err != nil {
				return fmt.Errorf("media description %d: %w", len(s.Media), err)
			}
			s.Media = append(s.Media, MediaSection{Media: md, Lines: []Line{l}})
			return nil
		}
		if n := len(s.Media); n > 0 {
			s.Media[n-1].Lines = append(s.Media[n-1].Lines, l)
			return nil
		}
		s.Session = append(s.Session, l)
		return nil
	})
	return s, err
}

// Description returns the session-level lines and the lines of media section i
// as one SessionDescription. It describes that stream alone: a line of another
// section, a c= or an a=rtpmap of another stream, is never read as one of its
// own.
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

// readLines calls line with the type and value of each line of data, in order.
func readLines(data []byte, line func(key, value string) error) error {
	reader := bufReader.Get().(*bytes.Buffer)
	defer bufReader.Put(reader)
	reader.Reset()
	reader.Write(data)

	for {
		l, err := nextLine(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}

		if len(l) < 2 {
			continue
		}

		ind := strings.Index(l, "=")
		if ind < 1 {
			return fmt.Errorf("Not a type=value line found. line=%q", l)
		}
		if err := line(l[:ind], l[ind+1:]); err != nil {
			return err
		}
	}
}

func nextLine(reader *bytes.Buffer) (line string, err error) {
	// Scan full line without buffer
	// If we need to continue then try to grow
	line, err = reader.ReadString('\n')
	if err != nil {
		// We may get io.EOF and line till it was read
		return line, err
	}

	lenline := len(line)

	// Be tolerant for CRLF.
	// A bare LF line arrives here as a 1 byte string, so the length must be
	// checked or this indexes -1 on a body any remote peer can send.
	if lenline >= 2 && line[lenline-2] == '\r' {
		return line[:lenline-2], nil
	}

	line = line[:lenline-1]
	return line, nil
}
