// SPDX-License-Identifier: MPL-2.0

package media

import "time"

// readUpstream reads one packet into slot. A read that fails on a reader
// replaced meanwhile, because UpdateRTPSession ended it or because the session
// it moved the buffer off was then closed under the read, continues on the
// reader in place. So does a read that UpdateRTPSession failed on the reader
// in place, which shares its socket with the one replaced. Replacements can
// follow each other while a read is blocked, so this repeats until a read
// succeeds, fails on the reader still in place with no move since the read
// before, or fails after Close.
func (j *RTPJitterBuffer) readUpstream(slot *rtpJitterSlot) (int, error) {
	reader := j.upstream()
	for {
		n, err := reader.ReadRTP(slot.raw, &slot.packet)
		if err == nil {
			return n, nil
		}
		next, moved := j.resumeUpstream()
		if next == reader && !moved {
			return n, err
		}
		select {
		case <-j.done:
			return n, err
		default:
		}
		reader = next
	}
}

// resumeUpstream returns the reader in place after a read failed, and whether
// UpdateRTPSession moved the buffer since the read loop last asked. After a
// move it first clears the read deadline the move ended the read with, which
// is still on the socket when the reader in place shares it with the one
// replaced: the deadline put back is the one StopRTP or StartRTP last asked
// for on the session, so a stop a caller set is not undone.
func (j *RTPJitterBuffer) resumeUpstream() (RTPReader, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	moved := j.moved
	j.moved = false
	if moved {
		// Under the lock, so the deadline of a later move is never the one
		// cleared.
		resumeRead(j.reader)
	}
	return j.reader, moved
}

// interruptRead ends a read waiting on reader with a read deadline, as
// RTPPacketReader.UpdateReader does for the reader it replaces. A reader that
// is no session of this package is left to finish its read. A socket that
// takes no deadline is closed, which ends the read as well.
func interruptRead(reader RTPReader) {
	switch r := reader.(type) {
	case *MediaSession:
		r.rtpConn.SetReadDeadline(time.Now())
	case *RTPSession:
		r.Sess.rtpConn.SetReadDeadline(time.Now())
	case *RTPSessionWebrtc:
		r.Sess.interruptRTPRead()
	}
}

func (j *RTPJitterBuffer) upstream() RTPReader {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.reader
}

// RTPAudioSession is an RTP session of this package that carries one
// negotiated audio stream, the session a jitter buffer reads: an *RTPSession
// and an *RTPSessionWebrtc.
type RTPAudioSession interface {
	RTPReader
	// audioCodec returns the audio codec the session negotiated.
	audioCodec() Codec
}

func (s *RTPSession) audioCodec() Codec {
	return CodecAudioFromSession(s.Sess)
}

func (s *RTPSessionWebrtc) audioCodec() Codec {
	return s.Sess.Codec()
}

// UpdateRTPSession moves the buffer onto rtpSess, the session a media update
// installed in place of the one it reads. Playout paces by the packet duration
// of its negotiated audio codec, and a buffer that learns packet durations
// learns them again from the packets that follow, in the clock rate of that
// codec. What is queued and the playout state carry over.
//
// A read the read loop has in progress on the previous reader is ended, as
// RTPPacketReader.UpdateReader ends the read of a reader it replaces, and
// continues on rtpSess: left to finish, it would return only with a packet the
// previous session takes for its own, and never when rtpSess negotiated
// another payload type. The read continues under the read deadline StopRTP or
// StartRTP last asked for. A previous reader that is no session of this
// package finishes its read, or, if it fails, continues on rtpSess.
func (j *RTPJitterBuffer) UpdateRTPSession(rtpSess RTPAudioSession) {
	codec := rtpSess.audioCodec()
	j.mu.Lock()
	defer j.mu.Unlock()
	j.moved = true
	interruptRead(j.reader)
	j.reader = rtpSess
	if codec.SampleDur > 0 {
		j.packetDuration = codec.SampleDur
	}
	if j.clockRate != 0 && codec.SampleRate != 0 {
		j.clockRate = codec.SampleRate
	}
	j.stepSet = false
	j.pendingStep = 0
}

// Done returns a channel that is closed once the buffer has stopped reading the
// injected reader and will not read it again. That happens when the injected
// reader returns an error, or after Close once the injected reader's ReadRTP
// in progress, if any, returns.
//
// An owner that needs the injected reader released, to hand it to another
// consumer or to be sure nothing reads it any more, calls Close, then makes
// the injected reader's ReadRTP return, for example by closing it, and then
// waits on Done.
func (j *RTPJitterBuffer) Done() <-chan struct{} {
	return j.readLoopDone
}
