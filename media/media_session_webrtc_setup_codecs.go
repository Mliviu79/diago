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

// restoreSetupCodecsLocked puts back into Codecs the codecs the session was
// set up with, which its offer carried, when a negotiation narrowed them: the
// answer applied next answers that same offer, and is matched against every
// codec it carried, not only those an earlier answer kept (RFC 3264 section
// 6.1). The caller holds m.mu.
func (m *MediaSessionWebrtc) restoreSetupCodecsLocked() {
	if m.setupCodecs != nil {
		m.Codecs = slices.Clone(m.setupCodecs)
	}
}

// RestoreOfferCodecs readies the session for another answer to the offer it
// made, as the 2xx of another early dialog than the one whose answer the
// session took carries (RFC 3261 sections 12.1 and 13.2.1): that answer is
// matched against every codec the offer carried, not only those the first
// answer kept (RFC 3264 section 6.1). The codec the session runs on stays
// until the next answer is applied and finalized. A session that took no
// answer is left as it is.
func (m *MediaSessionWebrtc) RestoreOfferCodecs() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.restoreSetupCodecsLocked()
}
