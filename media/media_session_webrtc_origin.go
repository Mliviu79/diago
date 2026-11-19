// SPDX-License-Identifier: MPL-2.0

package media

import (
	"sync"
	"time"
)

// sdpOrigin numbers the SDPs one call's WebRTC media sends: it gives the
// session id and the session version of their o= line. RFC 3264 section 8 has
// the o= line of an offer that modifies the session be "identical to that in
// the previous SDP, except that the version in the origin field MUST
// increment by one", so the id is drawn once and every SDP built after the
// first takes the next version.
//
// A MediaSessionWebrtc and the ICE generations forked from it share one, since
// an ICE restart modifies the same session: a generation that numbered its
// SDPs on its own would send a new session id in the middle of the call, or a
// version the generation before it had already used. It is safe for
// concurrent use.
type sdpOrigin struct {
	mu      sync.Mutex
	id      int64
	version int64
}

// next returns the session id and the session version of the next SDP.
func (o *sdpOrigin) next() (id int64, version int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.id == 0 {
		// A timestamp, as RFC 8866 section 5.2 recommends for a session id,
		// and the first version is the same value.
		o.id = time.Now().UnixNano()
		o.version = o.id
		return o.id, o.version
	}
	o.version++
	return o.id, o.version
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
