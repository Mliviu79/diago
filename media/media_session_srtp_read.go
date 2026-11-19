// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"sync/atomic"
)

// srtpDiscards records the packets a read path discards instead of handing
// them to its reader. Both media session types keep one: the rules are those
// of SRTP and of DTLS-SRTP, whichever transport the packets arrive on.
type srtpDiscards struct {
	// srtpAuthFailures counts the RTP packets ReadRTP discarded because the
	// SRTP context refused them (RFC 3711 section 3.3). They are normal in
	// small numbers, media in flight before a handshake settles or a stray
	// datagram to a port named in the SDP, so the count is the diagnostic and
	// not any single occurrence: one that climbs with the stream means the
	// keys disagree. It is atomic, since a session is read on another
	// goroutine than the ones that negotiate it.
	srtpAuthFailures atomic.Uint64
}

// srtpReadState is what the read path of a MediaSession keeps about SRTP. All
// of it is atomic, because a session is read while it is keyed: a leg is
// bridged, and so read, ahead of the ACK that runs its DTLS handshake.
type srtpReadState struct {
	srtpDiscards

	// dtlsKeyed is set once the SRTP contexts of a DTLS handshake are in
	// place. ReadRTP and ReadRTCP discard every packet of a DTLS-SRTP session
	// while it is clear. The handshake sets it only after the contexts, so a
	// reader that finds it set finds them too, although another goroutine set
	// them, and a reader that finds it clear reads neither.
	dtlsKeyed atomic.Bool
}

// dtlsSRTP reports whether this session keys SRTP from a DTLS handshake.
// RemoteSDP negotiates DTLS for such a session and refuses SDP without a
// fingerprint to check the handshake against.
func (s *MediaSession) dtlsSRTP() bool {
	return len(s.DTLSConf.Certificates) > 0 || s.SecureRTP == SecureRTPModeDTLS
}

// awaitsDTLSKeys reports whether this is a DTLS-SRTP session its handshake
// has not keyed yet. The media of such a session is SRTP, and "SRTP
// processing MUST NOT take place before the DTLS handshake completes" (RFC
// 5764 section 5.1), so nothing that arrives before can be the peer's media:
// anyone can send a datagram to a port named in the SDP.
func (s *MediaSession) awaitsDTLSKeys() bool {
	return s.dtlsSRTP() && !s.dtlsKeyed.Load()
}

// discardUnkeyedRTP reports whether the RTP datagram of n bytes just read from
// from is to be discarded because the session awaits its DTLS keys.
func (s *MediaSession) discardUnkeyedRTP(from net.Addr, n int) bool {
	if !s.awaitsDTLSKeys() {
		return false
	}
	logUnkeyedRTP(from, n)
	return true
}

// discardUnkeyedRTCP is discardUnkeyedRTP for a datagram read from the RTCP
// socket.
func (s *MediaSession) discardUnkeyedRTCP(from net.Addr, n int) bool {
	if !s.awaitsDTLSKeys() {
		return false
	}
	logUnkeyedRTCP(from, n)
	return true
}

// logUnkeyedRTP records an RTP datagram of n bytes, read from from, that a
// DTLS-SRTP session discards because its handshake has not keyed it yet.
func logUnkeyedRTP(from net.Addr, n int) {
	DefaultLogger().Debug("DTLS-SRTP: discarding RTP that arrived before the session is keyed",
		"from", from,
		"bytes", n,
	)
}

// logUnkeyedRTCP is logUnkeyedRTP for an RTCP datagram.
func logUnkeyedRTCP(from net.Addr, n int) {
	DefaultLogger().Debug("DTLS-SRTP: discarding RTCP that arrived before the session is keyed",
		"from", from,
		"bytes", n,
	)
}

// discardUnauthenticRTP records an RTP datagram the SRTP context refused with
// err, which ReadRTP discards before it reads the next one. RFC 3711 section
// 3.3 has a replayed packet discarded and the event logged, and for the
// authentication tag: 'If the result is "AUTHENTICATION FAILURE" (see Section
// 4.2), the packet MUST be discarded from further processing and the event
// SHOULD be logged.'
//
// The discard is the packet's and not the session's. Such packets are ordinary
// on a live stream: a peer that sends the instant it answers puts media in
// flight before its own handshake settles, and anyone can send a datagram to a
// port named in the SDP. A caller handed that as a read error cannot tell it
// from a socket that is gone and stops reading on either, so one datagram
// would end the media of a healthy call.
func (d *srtpDiscards) discardUnauthenticRTP(from net.Addr, n int, err error) {
	total := d.srtpAuthFailures.Add(1)
	DefaultLogger().Debug("SRTP: discarding packet that failed authentication",
		"from", from,
		"bytes", n,
		"total_discarded", total,
		"error", err,
	)
}

// discardUnauthenticRTCP records an RTCP datagram the SRTP context refused
// with err, which ReadRTCP discards before it reads the next one: "Secure RTCP
// follows the definition of Secure RTP" (RFC 3711 section 3.4), so
// discardUnauthenticRTP gives the reason.
func (*srtpDiscards) discardUnauthenticRTCP(from net.Addr, n int, err error) {
	DefaultLogger().Debug("SRTCP: discarding packet that failed authentication",
		"from", from,
		"bytes", n,
		"error", err,
	)
}
