// SPDX-License-Identifier: MPL-2.0

package media

import (
	"sync"
	"time"
)

// sdpOrigin gives the o= line of the SDPs one call's WebRTC media sends: the
// session id, drawn once, and the session version of each, see sdpVersions.
// RFC 3264 section 8 has the o= line of an offer that modifies the session be
// "identical to that in the previous SDP, except that the version in the
// origin field MUST increment by one", and an SDP sent under the version of
// the one before it be that SDP again.
//
// A MediaSessionWebrtc and the ICE generations forked from it share one, since
// an ICE restart modifies the same session: a generation that numbered its
// SDPs on its own would send a new session id in the middle of the call, or a
// version the generation before it had already used. It is safe for
// concurrent use.
type sdpOrigin struct {
	mu       sync.Mutex
	id       int64
	versions sdpVersions
}

// numbered returns the SDP write gives under the session id of the call and
// the session version that SDP is to carry. write returns the SDP for an id
// and a version, and the same one whenever it is called with the same two.
func (o *sdpOrigin) numbered(write func(id int64, version uint64) []byte) []byte {
	o.mu.Lock()
	if o.id == 0 {
		// A timestamp, as RFC 8866 section 5.2 recommends for a session id,
		// and the first version is the same value.
		o.id = time.Now().UnixNano()
	}
	id := o.id
	o.mu.Unlock()
	body, _ := o.versions.numbered(uint64(id), func(version uint64) []byte { return write(id, version) })
	return body
}

// originLocked returns the origin the SDPs of this session are numbered from.
// The caller holds m.mu.
func (m *MediaSessionWebrtc) originLocked() *sdpOrigin {
	if m.origin == nil {
		m.origin = &sdpOrigin{}
	}
	return m.origin
}

// sharedOrigin returns the origin of this session, for Fork to hand to the
// ICE generation it creates.
func (m *MediaSessionWebrtc) sharedOrigin() *sdpOrigin {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.originLocked()
}
