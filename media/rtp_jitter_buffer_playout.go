// SPDX-License-Identifier: MPL-2.0

package media

// notify wakes ReadRTP. One pending wake covers any number of packets, since
// ReadRTP decides on everything filed when it wakes.
func (j *RTPJitterBuffer) notify() {
	select {
	case j.arrived <- struct{}{}:
	default:
	}
}

// dropBefore moves the window on so that next is the first sequence number to
// play, dropping the packets queued before it as stale. Sequence numbers it
// passes that were never queued are not counted as lost, as the window moved
// past them rather than playout.
func (j *RTPJitterBuffer) dropBefore(next uint16) {
	passed := int(int16(next - j.expectedSeq))
	for i := 0; i < passed && i < j.maxPackets; i++ {
		seq := j.expectedSeq + uint16(i)
		position := int(seq) % j.maxPackets
		slotIndex := j.sequence[position]
		if slotIndex < 0 || j.slots[slotIndex].seq != seq {
			continue
		}
		j.stats.packetsDropped.Add(1)
		j.debugPacketDecision("dropped", &j.slots[slotIndex], "stale")
		j.sequence[position] = -1
		j.queued--
		j.freeSlot(slotIndex)
	}
	j.expectedSeq = next
}

// stopPlayout stops playout when it finds nothing queued. Counting every tick
// of a sender's pause as a lost packet would move expectedSeq onto sequence
// numbers the sender has not used yet, and every packet after the pause would
// then be late. Playout waits for the stream instead and keeps expectedSeq.
func (j *RTPJitterBuffer) stopPlayout() {
	j.stats.underruns.Add(1)
	j.debugPlayoutStopped()
	j.playout = false
	j.resync = true
}

func (j *RTPJitterBuffer) debugPlayoutStopped() {
	if !rtpJitterDebug {
		return
	}

	jitterDebugf("event=playout_stopped expected_seq=%d delay_packets=%d max_packets=%d reason=nothing_queued",
		j.expectedSeq,
		j.delayPackets,
		j.maxPackets,
	)
}
