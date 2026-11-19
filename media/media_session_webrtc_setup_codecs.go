// SPDX-License-Identifier: MPL-2.0

package media

import "slices"

// keepSetupCodecsLocked records Codecs as the codecs the session was set up
// with, unless a record exists already. RemoteSDP calls it before it narrows
// Codecs to the codecs in common, so the record holds every configured codec
// for Fork. The caller holds m.mu.
func (m *MediaSessionWebrtc) keepSetupCodecsLocked() {
	if m.setupCodecs == nil {
		m.setupCodecs = slices.Clone(m.Codecs)
	}
}

// forkCodecsLocked returns the codecs a fork of the session starts from: every
// codec the session was set up with, as MediaSession.Fork does, and not only
// those the last negotiation kept. RFC 3264 section 8.3.2 lets a new offer
// include formats the previous SDP left out, and an answer to a peer's offer
// is formulated from every format we support (section 6). The caller holds
// m.mu.
func (m *MediaSessionWebrtc) forkCodecsLocked() []Codec {
	if m.setupCodecs != nil {
		return slices.Clone(m.setupCodecs)
	}
	return slices.Clone(m.Codecs)
}
