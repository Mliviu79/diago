// SPDX-License-Identifier: MPL-2.0

package media

// AbandonNegotiation ends the ICE and DTLS negotiation FinalizeWithin left
// running, and returns once it has returned. A session that left none running
// is left as it is.
//
// It is for an answer that another answer to the same offer replaces before
// the call is answered: a UAC takes the answer of each early dialog its offer
// creates (RFC 3261 sections 12.1 and 13.2.1), and the call goes on with the
// dialog the 2xx confirms. The negotiation of the replaced answer reads and
// writes what RemoteSDP reads and writes, so it ends before that dialog's
// answer is applied. A negotiation it ended stays ended: the session's
// Finalize returns its error from then on.
//
// It must not be called beside a Finalize or a FinalizeWithin of the session.
func (m *MediaSessionWebrtc) AbandonNegotiation() {
	m.leftRunning.stop()
}
