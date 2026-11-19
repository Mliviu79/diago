// SPDX-License-Identifier: MPL-2.0

package diago

import "github.com/emiago/diago/media"

// offersNegotiatedCodecs reports whether an offer of ours that asks for no
// change of direction carries only the codecs the call runs on
// (MediaConfig.ReInviteNegotiatedCodecs).
func (d *DialogWebrtc) offersNegotiatedCodecs() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.offerNegotiatedCodecs
}

// offerNegotiatedCodecs has fork, a fork of the session cur the call runs on
// that has made no SDP yet, offer the codecs cur runs on, with the payload
// types they were negotiated with, so that its offer renegotiates nothing.
// The answer can only hold codecs of the offer, so fork is matched against
// these alone; it keeps every configured codec for the forks made from it.
// Before any negotiation cur has no codecs in common, and fork offers every
// codec it holds.
func offerNegotiatedCodecs(fork, cur *media.MediaSessionWebrtc) {
	if negotiated := cur.CommonCodecs(); len(negotiated) > 0 {
		fork.Codecs = negotiated
	}
}
