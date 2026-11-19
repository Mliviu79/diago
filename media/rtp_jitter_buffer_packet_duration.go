// SPDX-License-Identifier: MPL-2.0

package media

import "time"

// maxLearnedRTPPacketDuration is the longest packet duration the buffer
// learns from RTP timestamps. An audio packet carries at most 120 ms, and a
// longer step between consecutive packets is a pause in the stream, as a
// sender in DTX makes between its frames, not the duration of a packet.
const maxLearnedRTPPacketDuration = 200 * time.Millisecond

// learnPacketDuration compares the packet in slot with the one that arrived
// before it. Two consecutive packets of the same payload type, the second not
// starting a talkspurt, step their RTP timestamps by the duration of a packet.
// A step becomes the packet duration once the next pair repeats it, so a
// single step spanning a pause does not. Called with mu held.
func (j *RTPJitterBuffer) learnPacketDuration(slot *rtpJitterSlot) {
	if j.clockRate == 0 {
		return
	}
	pkt := &slot.packet
	if j.stepSet && slot.seq == j.stepSeq+1 && pkt.PayloadType == j.stepPayloadType && !pkt.Marker {
		step := pkt.Timestamp - j.stepTimestamp
		if step > 0 && step == j.pendingStep {
			if d := time.Duration(step) * time.Second / time.Duration(j.clockRate); d <= maxLearnedRTPPacketDuration {
				j.packetDuration = d
			}
		}
		j.pendingStep = step
	}
	if !j.stepSet || int16(slot.seq-j.stepSeq) > 0 {
		j.stepSeq = slot.seq
		j.stepTimestamp = pkt.Timestamp
		j.stepPayloadType = pkt.PayloadType
		j.stepSet = true
	}
}
