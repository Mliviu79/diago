// SPDX-License-Identifier: MPL-2.0

package media

import "net"

// ForkOffer returns a fork of the session in the state its offer left it: on
// the same sockets, with the same codecs, direction, SDP numbering and keys of
// its own, and with the DTLS association Fork carries, but without what an
// answer to that offer gave it: the remote address and protocol, and the
// peer's SDES keys.
//
// It is for an offer whose answer ended with the early dialog that gave it:
// a non-2xx final response to an INVITE ends every early dialog of it (RFC
// 3261 section 12.3), and the INVITE sent again, as after a digest challenge
// (section 22.2), carries the same offer, from the same sockets. The answer to
// it is applied to the fork as an answer is to the session that made the
// offer. This session is done with, and only its sockets are closed, by
// whoever closes the fork.
func (s *MediaSession) ForkOffer() *MediaSession {
	cp := s.Fork()
	cp.Raddr = net.UDPAddr{}
	cp.remoteProto = ""
	if s.SecureRTP == SecureRTPModeSDES {
		// The peer's keys came with its answer. Our own stay: the offer sent
		// again carries them.
		cp.remoteCtxSRTP = nil
		cp.srtpRemoteTag = 0
	}
	return cp
}
