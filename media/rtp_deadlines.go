// SPDX-License-Identifier: MPL-2.0

package media

import (
	"errors"
	"sync"
	"time"
)

// errNoRTPSocket is what StopRTP and StartRTP return on a session that has no
// RTP socket, one that was never initialized: there is no read or write to
// give a deadline.
var errNoRTPSocket = errors.New("media: session has no RTP socket")

// rtpDeadlines holds the read and write deadlines StopRTP and StartRTP last
// asked for on an RTP socket. Sessions that run on one socket share one, once
// CarryRTPDeadlines has handed it from one to the other, so a deadline asked
// for through either is the one both know.
//
// The socket may carry another read deadline for a moment:
// RTPPacketReader.UpdateReader ends a waiting read with one of its own, which
// is not recorded here, so the read that takes over puts the recorded one back
// instead of reading on without any.
type rtpDeadlines struct {
	// mu is held from recording a deadline until the socket has it, so the
	// record and the socket never disagree about what was asked for last.
	mu    sync.Mutex
	read  time.Time
	write time.Time
}

// rtpDeadlineRecord returns the record of the deadlines asked for on the
// session's RTP socket, starting an empty one when the session has none.
func (s *MediaSession) rtpDeadlineRecord() *rtpDeadlines {
	if rec := s.deadlines.Load(); rec != nil {
		return rec
	}
	s.deadlines.CompareAndSwap(nil, &rtpDeadlines{})
	return s.deadlines.Load()
}

// holdRTPDeadline records t as the deadline asked for on the side of the RTP
// socket StopRTP and StartRTP set it on for rw: the read side when bit 1 is
// set, the write side when only bit 2 is, and both when neither is. It returns
// with the record locked. The caller sets t on the socket and then calls the
// function returned, which unlocks the record.
func (s *MediaSession) holdRTPDeadline(rw int8, t time.Time) (release func()) {
	rec := s.rtpDeadlineRecord()
	rec.mu.Lock()
	switch {
	case rw&1 > 0:
		rec.read = t
	case rw&2 > 0:
		rec.write = t
	default:
		rec.read, rec.write = t, t
	}
	return rec.mu.Unlock
}

// CarryRTPDeadlines takes over the RTP read and write deadlines StopRTP and
// StartRTP last set on from, a session this one replaces. A deadline set to
// end a read or a write then also ends the one that carries on over this
// session, as a read of RTPPacketReader does once the reader is moved.
//
// When the two run on different sockets, the deadlines are set on this
// session's socket. When they share one, the socket has them already, and the
// two sessions go on recording what is asked of it in one place: a deadline
// set or cleared through from afterwards is set or cleared for this session
// too.
//
// It is called before this session is put in from's place, so that a read
// taken up on this session finds the deadlines here.
func (s *MediaSession) CarryRTPDeadlines(from *MediaSession) error {
	rec := from.rtpDeadlineRecord()
	if from.rtpConn == s.rtpConn {
		s.deadlines.Store(rec)
		return nil
	}

	rec.mu.Lock()
	read, write := rec.read, rec.write
	rec.mu.Unlock()

	own := s.rtpDeadlineRecord()
	own.mu.Lock()
	defer own.mu.Unlock()
	own.read, own.write = read, write
	return errors.Join(s.rtpConn.SetReadDeadline(read), s.rtpConn.SetWriteDeadline(write))
}

// RestoreRTPReadDeadline sets the RTP socket's read deadline to the one
// StopRTP or StartRTP last asked for, which is none when neither did.
//
// RTPPacketReader.UpdateReader ends a read waiting on the session with a read
// deadline of its own, which stays on the socket until the read that takes
// over replaces it. A caller that puts a reader of this same session in the
// packet reader's place, as a jitter buffer over the session is, calls this
// before that reader reads: its reads then go on, and a stop the application
// set with StopRTP is still in force, where StartRTP would clear it.
func (s *MediaSession) RestoreRTPReadDeadline() error {
	rec := s.rtpDeadlineRecord()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	return s.rtpConn.SetReadDeadline(rec.read)
}
