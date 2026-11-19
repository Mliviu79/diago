// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"

	"github.com/pion/rtp"
)

// RTPSourceLockInSequence makes a session that runs with RTPSourceLock learn
// its first source only from four packets in sequence with each other. Off,
// the first source is the one of the fourth packet read, whatever their
// order. A session forked toward another peer learns that peer's source in
// sequence either way. It is read on every packet of a session that is still
// learning: set it once, before any session reads.
var RTPSourceLockInSequence bool

// learnsSourceInSequence reports whether the source is learned by counting
// packets against the last one counted. A session that has read packets
// before is a fork toward another peer: its read statistics hold the last
// sequence number of the previous peer's stream, which no packet refused
// while learning moves on, so a count kept against them starts again on
// every packet and the new source is never learned.
func (s *RTPSession) learnsSourceInSequence() bool {
	return RTPSourceLockInSequence || s.readStats.lastSeq.seqNum != 0
}

// learnSourceInSequence counts pkt toward the four packets in sequence that
// make from the session's source, and reports whether pkt is to be read. A
// packet out of sequence starts the count again with itself.
func (s *RTPSession) learnSourceInSequence(pkt *rtp.Packet, from *net.UDPAddr) bool {
	if s.sourceLockPackets > 0 && s.sourceLockSeq+1 != pkt.SequenceNumber {
		s.sourceLockPackets = 0
	}
	s.sourceLockSeq = pkt.SequenceNumber

	// We wait couple packets before considering this is right source
	s.sourceLockPackets++
	if s.sourceLockPackets < 4 {
		return false
	}

	s.sourceLockAddr = from
	return true
}
