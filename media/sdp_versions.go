// SPDX-License-Identifier: MPL-2.0

package media

import (
	"bytes"
	"sync"
)

// sdpVersions numbers the session descriptions the media of one call sends:
// it gives each one the session version of its o= line. RFC 3264 section 8:
//
//	When issuing an offer that modifies the session, the "o=" line of the
//	new SDP MUST be identical to that in the previous SDP, except that the
//	version in the origin field MUST increment by one from the previous
//	SDP.  If the version in the origin line does not increment, the SDP
//	MUST be identical to the SDP with that version number.
//
// The previous SDP is "the last SDP provided to the other party (which may
// have been provided in an offer or an answer)", whatever became of the
// exchange it was provided in. So a description that differs from the one
// before it takes the next version, and one that repeats it is that
// description again, version included. That is how RFC 4028 section 7.4 has
// the offer of a session refresh that changes nothing, and the answer to one,
// "indicate that it has not changed": "by including the same value for the
// origin field as did previous SDP messages to its peer".
//
// A media session and the sessions forked from it share one. A fork describes
// the same session, and one that numbered its descriptions on its own would
// give the version of an offer that came to nothing to the description made
// after it. It is safe for concurrent use.
type sdpVersions struct {
	mu sync.Mutex
	// described says a description was numbered: last is that description,
	// and version the session version it carries.
	described bool
	last      []byte
	version   uint64
}

// numbered returns the description write gives and the session version it
// carries. write returns the description under a session version, and gives
// the same one whenever it is called with the same version: it is called
// once or twice. The first description of the call takes the version first.
func (v *sdpVersions) numbered(first uint64, write func(version uint64) []byte) (body []byte, version uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if !v.described {
		v.described = true
		v.version = first
		v.last = write(v.version)
		return bytes.Clone(v.last), v.version
	}
	// Written under the version of the description before it, the two are
	// the same bytes when nothing else differs.
	if body = write(v.version); bytes.Equal(body, v.last) {
		return body, v.version
	}
	v.version++
	v.last = write(v.version)
	return bytes.Clone(v.last), v.version
}

// sdpVersionRecord returns the numbering of the SDPs of this session, starting
// one when the session has none. Fork hands it to the session it makes.
func (s *MediaSession) sdpVersionRecord() *sdpVersions {
	if s.versions == nil {
		s.versions = &sdpVersions{}
	}
	return s.versions
}

// numbered returns the description LocalSDP writes with write, under the
// session version sdpVersions gives it, and makes that version the session's.
// The version LocalSDP counted before, one more than the session's last,
// stands for the first description of the call only: after it the count is
// the call's, not this session's, since a fork made before another one
// described the session knows nothing of what that one sent.
func (s *MediaSession) numbered(write func(version uint64) []byte) []byte {
	body, version := s.sdpVersionRecord().numbered(s.sessionVersion, write)
	s.sessionVersion = version
	return body
}
