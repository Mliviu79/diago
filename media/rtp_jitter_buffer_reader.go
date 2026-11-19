// SPDX-License-Identifier: MPL-2.0

package media

// readUpstream reads one packet into slot. A read that fails on a reader
// replaced meanwhile, typically because UpdateRTPSession moved the buffer off a
// session that was then closed under the read, continues on the reader in
// place. Replacements can follow each other while a read is blocked, so this
// repeats until a read succeeds, fails on the reader still in place, or fails
// after Close.
func (j *RTPJitterBuffer) readUpstream(slot *rtpJitterSlot) (int, error) {
	reader := j.upstream()
	for {
		n, err := reader.ReadRTP(slot.raw, &slot.packet)
		if err == nil {
			return n, nil
		}
		next := j.upstream()
		if next == reader {
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

func (j *RTPJitterBuffer) upstream() RTPReader {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.reader
}

// UpdateRTPSession moves the buffer onto rtpSess, the session a media update
// installed in place of the one it reads. Playout paces by the packet duration
// of its negotiated audio codec, and a buffer that learns packet durations
// learns them again from the packets that follow, in the clock rate of that
// codec. What is queued and the playout state carry over. A read the read loop
// has in progress on the previous reader finishes there, or, if it fails, for
// example because the previous session was closed, continues on rtpSess.
func (j *RTPJitterBuffer) UpdateRTPSession(rtpSess *RTPSession) {
	codec := CodecAudioFromSession(rtpSess.Sess)
	j.mu.Lock()
	defer j.mu.Unlock()
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
