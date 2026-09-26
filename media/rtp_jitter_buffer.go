// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/rtp"
)

const (
	defaultRTPJitterBufferDelayPackets = 20
	defaultRTPJitterBufferMaxPackets   = 40

	// maxLearnedRTPPacketDuration is the longest packet duration the buffer
	// learns from RTP timestamps. An audio packet carries at most 120 ms, and a
	// longer step between consecutive packets is a pause in the stream, as a
	// sender in DTX makes between its frames, not the duration of a packet.
	maxLearnedRTPPacketDuration = 200 * time.Millisecond
)

var rtpJitterDebug = envBool("JITTER_DEBUG")

// RTPJitterBufferOptions configures a fixed RTP jitter buffer.
type RTPJitterBufferOptions struct {
	// DelayPackets is the initial fixed playout delay in packets. If unset, 20 is used.
	DelayPackets int
	// MaxPackets caps buffered packets and the forward reordering window. If unset, 40 is used.
	// A value below DelayPackets is raised to DelayPackets.
	MaxPackets int
	// ClockRate is the RTP clock rate of the stream in Hz. When set, the buffer
	// learns the duration of the sender's packets from their RTP timestamps and
	// paces playout by it, so a sender whose packets are shorter or longer than
	// the packet duration the buffer was given is played at its own pace. If
	// unset, playout paces by the packet duration the buffer was given.
	ClockRate uint32
}

// RTPJitterBufferStatistics contains simple counters for buffer decisions.
type RTPJitterBufferStatistics struct {
	PacketsRead        uint64
	PacketsReleased    uint64
	PacketsLost        uint64
	PacketsLate        uint64
	PacketsDuplicate   uint64
	PacketsDropped     uint64
	SSRCResets         uint64
	Underruns          uint64
	LastSequenceNumber uint16
	// PacketDuration is what playout paces by: the packet duration the buffer
	// was given, or the one it learned from the sender's RTP timestamps.
	PacketDuration time.Duration
}

type rtpJitterBufferStats struct {
	packetsRead        atomic.Uint64
	packetsReleased    atomic.Uint64
	packetsLost        atomic.Uint64
	packetsLate        atomic.Uint64
	packetsDuplicate   atomic.Uint64
	packetsDropped     atomic.Uint64
	ssrcResets         atomic.Uint64
	underruns          atomic.Uint64
	lastSequenceNumber atomic.Uint32
}

type rtpJitterSlot struct {
	raw    []byte
	packet rtp.Packet
	n      int
	ssrc   uint32
	seq    uint16
}

// RTPJitterBuffer is a fixed-delay, single-consumer RTPReader wrapper.
//
// It sits after RTPSession and before RTPPacketReader, so RTPSession observes
// true network arrival while downstream readers get reordered packets. Its read
// loop takes each packet from upstream as it arrives, whether or not the
// consumer is reading.
//
// Playout releases one packet per packet duration on a clock that keeps its
// pace however late a release is made, so a consumer that reads late gets the
// packets due meanwhile at once. The packet duration is the one the buffer was
// given or, with RTPJitterBufferOptions.ClockRate, the one the sender's RTP
// timestamps show. A missing packet is skipped as lost when later ones are
// queued. When nothing is queued at all, because the sender paused (hold,
// silence suppression, DTX) or the network stalled for longer than the playout
// delay, playout stops, counting an underrun and no packet lost, and buffers
// again, as at the start of the stream, from the next packet at or after where
// it stopped. Until it plays again, a packet behind that point is late, except
// that one MaxPackets or more away in either direction, arriving with nothing
// queued, starts the stream again, as from a sender that restarted its sequence
// numbers.
//
// A packet MaxPackets or more ahead of the next one to play does not fit the
// window, as happens when the consumer stops reading while the sender goes on.
// It is the newest audio, so the window moves on to end at it with DelayPackets
// sequence numbers, and the older packets queued are dropped as stale.
type RTPJitterBuffer struct {
	// mu guards every field below up to arrived. The read loop files each packet
	// under it as the packet arrives, and ReadRTP plays them out under it.
	mu sync.Mutex
	// reader is the upstream network-facing RTP source.
	reader RTPReader
	// packetDuration is the interval between playout decisions.
	packetDuration time.Duration
	// clockRate is the RTP clock rate packet durations are learned in, or 0 when
	// they are not learned.
	clockRate uint32

	// delayPackets is the number of queued packets required to start playout early.
	delayPackets int
	// maxPackets is both the queue capacity and accepted forward sequence window.
	maxPackets int

	// slots contains all reusable packet metadata and packet-sized byte regions.
	slots []rtpJitterSlot
	// freeSlots holds the indexes of the slots neither queued nor being read into.
	freeSlots []int
	// sequence maps sequenceNumber % maxPackets to an occupied slot index, or -1.
	sequence []int
	// queued is the number of occupied entries in sequence.
	queued int

	// ssrc identifies the RTP source currently accepted by the buffer.
	ssrc uint32
	// ssrcSet reports whether the buffer has learned an RTP source.
	ssrcSet bool
	// expectedSeq is the next sequence number scheduled for playout.
	expectedSeq uint16
	// expectedSet reports whether expectedSeq has been initialized.
	expectedSet bool

	// startAt is when buffering ends at the latest and playout starts with what
	// is queued. It is zero while nothing is buffering.
	startAt time.Time
	// releaseAt is when playout makes its next decision. Each decision moves it
	// on by packetDuration from where it was, not from when the decision ran.
	releaseAt time.Time
	// playout reports whether the initial buffering phase has completed.
	playout bool
	// resync reports that playout stopped with nothing queued. Until it starts
	// again, expectedSeq is the first sequence number not yet played, and
	// packets behind it are late.
	resync bool

	// upstreamEnded reports that the read loop stopped on an upstream error.
	upstreamEnded bool
	// readErr stores the terminal upstream error returned after queued packets drain.
	readErr error

	// stepSeq, stepTimestamp and stepPayloadType describe the newest packet
	// that arrived, which the next one is compared with to learn the packet
	// duration. stepSet reports whether they are set.
	stepSeq         uint16
	stepTimestamp   uint32
	stepPayloadType uint8
	stepSet         bool
	// pendingStep is the timestamp step between the last two consecutive
	// packets. A step becomes the packet duration when the next pair repeats it.
	pendingStep uint32

	// lastArrivalTime is used only for JITTER_DEBUG receive-side diagnostics.
	lastArrivalTime time.Time
	// lastArrivalSeq is the previous RTP sequence observed by readLoop.
	lastArrivalSeq uint16
	// lastArrivalSet reports whether receive-side debug arrival state is initialized.
	lastArrivalSet bool

	// arrived wakes ReadRTP once the read loop has filed a packet or ended.
	arrived chan struct{}
	// timer wakes ReadRTP for its next playout decision. Only ReadRTP arms it.
	timer *time.Timer
	// done is closed by Close to stop the read loop and delivery.
	done chan struct{}
	// closeOnce makes Close idempotent.
	closeOnce sync.Once
	// startOnce ensures that only one upstream reader goroutine is launched.
	startOnce sync.Once
	// readLoopDone is closed when readLoop returns, or by Close when readLoop
	// never started.
	readLoopDone chan struct{}
	// stats uses atomics so Statistics may run concurrently with ReadRTP.
	stats rtpJitterBufferStats
}

// NewRTPJitterBuffer creates a fixed jitter buffer over another RTPReader.
// It panics if packetDuration is not greater than zero.
func NewRTPJitterBuffer(reader RTPReader, packetDuration time.Duration, opts RTPJitterBufferOptions) *RTPJitterBuffer {
	if packetDuration <= 0 {
		panic("media: RTP jitter buffer packetDuration must be greater than zero")
	}
	if opts.DelayPackets <= 0 {
		opts.DelayPackets = defaultRTPJitterBufferDelayPackets
	}
	if opts.MaxPackets <= 0 {
		opts.MaxPackets = defaultRTPJitterBufferMaxPackets
	}
	if opts.MaxPackets < opts.DelayPackets {
		opts.MaxPackets = opts.DelayPackets
	}
	if opts.DelayPackets > opts.MaxPackets {
		opts.DelayPackets = opts.MaxPackets
	}

	// The window holds at most MaxPackets, so one extra slot is always free for
	// the read loop to read the next packet into. All packet storage is
	// allocated once here.
	slotCount := opts.MaxPackets + 1
	data := make([]byte, slotCount*RTPBufSize)
	slots := make([]rtpJitterSlot, slotCount)
	freeSlots := make([]int, slotCount)
	for i := range slots {
		slots[i].raw = data[i*RTPBufSize : (i+1)*RTPBufSize]
		freeSlots[i] = i
	}

	sequence := make([]int, opts.MaxPackets)
	for i := range sequence {
		sequence[i] = -1
	}

	timer := time.NewTimer(time.Hour)
	timer.Stop()

	return &RTPJitterBuffer{
		reader:         reader,
		packetDuration: packetDuration,
		clockRate:      opts.ClockRate,
		delayPackets:   opts.DelayPackets,
		maxPackets:     opts.MaxPackets,
		slots:          slots,
		freeSlots:      freeSlots,
		sequence:       sequence,
		arrived:        make(chan struct{}, 1),
		timer:          timer,
		done:           make(chan struct{}),
		readLoopDone:   make(chan struct{}),
	}
}

// ReadRTP implements RTPReader. Calls must not overlap.
func (j *RTPJitterBuffer) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	j.start()

	for {
		// Close wins over anything queued.
		select {
		case <-j.done:
			j.timer.Stop()
			return 0, io.ErrClosedPipe
		default:
		}

		// Every packet that has arrived is filed by now, so each decision sees
		// all of them.
		j.mu.Lock()
		now := time.Now()
		if !j.playout {
			if startDue := !j.startAt.IsZero() && !now.Before(j.startAt); startDue || j.queued >= j.delayPackets {
				if startDue {
					j.debugPlayoutStarted("initial_timer")
				} else {
					j.debugPlayoutStarted("delay_packets")
				}
				j.startPlayout(now)
			}
		}

		if j.playout && !now.Before(j.releaseAt) {
			position := int(j.expectedSeq) % j.maxPackets
			slotIndex := j.sequence[position]
			if slotIndex >= 0 && j.slots[slotIndex].seq == j.expectedSeq {
				slot := &j.slots[slotIndex]
				if len(buf) < slot.n {
					j.mu.Unlock()
					return 0, io.ErrShortBuffer
				}

				j.sequence[position] = -1
				j.queued--
				j.expectedSeq++
				j.releaseAt = j.releaseAt.Add(j.packetDuration)
				n := copy(buf, slot.raw[:slot.n])
				j.freeSlot(slotIndex)
				j.mu.Unlock()

				if err := RTPUnmarshal(buf[:n], p); err != nil {
					return 0, err
				}
				j.stats.packetsReleased.Add(1)
				j.stats.lastSequenceNumber.Store(uint32(p.SequenceNumber))
				return n, nil
			}

			if j.queued == 0 {
				j.stopPlayout()
			} else {
				j.stats.packetsLost.Add(1)
				j.expectedSeq++
				j.releaseAt = j.releaseAt.Add(j.packetDuration)
				j.mu.Unlock()
				continue
			}
		}

		if j.upstreamEnded && j.queued == 0 {
			err := j.readErr
			j.mu.Unlock()
			j.timer.Stop()
			if err != nil {
				return 0, err
			}
			return 0, io.EOF
		}

		// Nothing is due yet. Wait for the next decision, the next packet or
		// Close. Stopped with nothing queued, only a packet starts anything.
		var wait time.Duration
		switch {
		case j.playout:
			wait = j.releaseAt.Sub(now)
		case !j.startAt.IsZero():
			wait = j.startAt.Sub(now)
		}
		j.mu.Unlock()

		var timerC <-chan time.Time
		if wait > 0 {
			j.timer.Reset(wait)
			timerC = j.timer.C
		}
		select {
		case <-j.done:
			j.timer.Stop()
			return 0, io.ErrClosedPipe
		case <-j.arrived:
		case <-timerC:
		}
	}
}

func (j *RTPJitterBuffer) start() {
	j.startOnce.Do(func() {
		go j.readLoop()
	})
}

func (j *RTPJitterBuffer) readLoop() {
	defer close(j.readLoopDone)

	for {
		// Once the loop has seen Close it starts no further upstream read.
		select {
		case <-j.done:
			return
		default:
		}

		j.mu.Lock()
		slotIndex := j.freeSlots[len(j.freeSlots)-1]
		j.freeSlots = j.freeSlots[:len(j.freeSlots)-1]
		j.mu.Unlock()

		slot := &j.slots[slotIndex]
		n, err := j.readUpstream(slot)

		j.mu.Lock()
		switch {
		case err != nil:
			j.freeSlot(slotIndex)
			j.readErr = err
			j.upstreamEnded = true
		case n == 0:
			j.freeSlot(slotIndex)
		default:
			slot.n = n
			slot.ssrc = slot.packet.SSRC
			slot.seq = slot.packet.SequenceNumber
			j.debugArrival(slot)
			j.handleSlot(slotIndex)
		}
		j.mu.Unlock()

		if err != nil {
			j.notify()
			return
		}
		if n > 0 {
			j.notify()
		}
	}
}

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

// notify wakes ReadRTP. One pending wake covers any number of packets, since
// ReadRTP decides on everything filed when it wakes.
func (j *RTPJitterBuffer) notify() {
	select {
	case j.arrived <- struct{}{}:
	default:
	}
}

// handleSlot files the packet read into slotIndex. Called with mu held.
func (j *RTPJitterBuffer) handleSlot(slotIndex int) {
	slot := &j.slots[slotIndex]
	j.stats.packetsRead.Add(1)

	if !j.ssrcSet || j.ssrc != slot.ssrc {
		if j.ssrcSet {
			j.stats.ssrcResets.Add(1)
		}
		j.resetStream(slot.ssrc, slot.seq)
	}

	distance := int16(slot.seq - j.expectedSeq)
	// Stopped with nothing queued, a packet a whole window or more away from
	// where playout stopped comes from a sender that restarted its sequence
	// numbers, so the stream starts again from it as from a new source.
	if j.resync && j.queued == 0 && (int(distance) >= j.maxPackets || -int(distance) >= j.maxPackets) {
		j.debugPacketDecision("restart", slot, "outside_window")
		j.resetStream(slot.ssrc, slot.seq)
		distance = 0
	}
	j.learnPacketDuration(slot)
	if distance < 0 {
		if j.playout || j.resync {
			j.stats.packetsLate.Add(1)
			j.debugPacketDecision("late", slot, "behind_playout")
			j.freeSlot(slotIndex)
			return
		}
		if !j.canMoveExpectedBack(slot.seq) {
			j.stats.packetsDropped.Add(1)
			j.debugPacketDecision("dropped", slot, "before_window")
			j.freeSlot(slotIndex)
			return
		}
		j.expectedSeq = slot.seq
		distance = 0
	}

	if int(distance) >= j.maxPackets {
		// The packet is the newest audio and does not fit the window. The
		// window moves on to end at it with the playout delay, and what it
		// leaves behind is stale.
		j.dropBefore(slot.seq - uint16(j.delayPackets-1))
		distance = int16(slot.seq - j.expectedSeq)
	}

	position := int(slot.seq) % j.maxPackets
	existing := j.sequence[position]
	if existing >= 0 {
		if j.slots[existing].seq == slot.seq {
			j.stats.packetsDuplicate.Add(1)
			j.debugPacketDecision("duplicate", slot, "same_sequence")
		} else {
			j.stats.packetsDropped.Add(1)
			j.debugPacketDecision("dropped", slot, "slot_collision")
		}
		j.freeSlot(slotIndex)
		return
	}

	j.sequence[position] = slotIndex
	j.queued++
	if j.resync && j.queued == 1 {
		// The stream resumed, so buffer again as at its start.
		j.startAt = time.Now().Add(time.Duration(j.delayPackets) * j.packetDuration)
	}
}

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

func (j *RTPJitterBuffer) canMoveExpectedBack(seq uint16) bool {
	for _, slotIndex := range j.sequence {
		if slotIndex < 0 {
			continue
		}
		distance := int16(j.slots[slotIndex].seq - seq)
		if distance < 0 || int(distance) >= j.maxPackets {
			return false
		}
	}
	return true
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

func (j *RTPJitterBuffer) resetStream(ssrc uint32, seq uint16) {
	j.clearSequence()
	j.ssrc = ssrc
	j.ssrcSet = true
	j.expectedSeq = seq
	j.expectedSet = true
	j.playout = false
	j.resync = false
	j.stepSet = false
	j.pendingStep = 0
	j.startAt = time.Now().Add(time.Duration(j.delayPackets) * j.packetDuration)
}

func (j *RTPJitterBuffer) clearSequence() {
	for i, slotIndex := range j.sequence {
		if slotIndex < 0 {
			continue
		}
		j.sequence[i] = -1
		j.freeSlot(slotIndex)
	}
	j.queued = 0
}

// startPlayout starts playout with a release due at now.
func (j *RTPJitterBuffer) startPlayout(now time.Time) {
	j.startAt = time.Time{}
	// Starting again after a stop, the first packet queued can be ahead of
	// expectedSeq. The sequence numbers before it were lost, and playout starts
	// at that packet instead of spending a tick on each of them.
	for j.resync && j.queued > 0 && j.sequence[int(j.expectedSeq)%j.maxPackets] < 0 {
		j.stats.packetsLost.Add(1)
		j.expectedSeq++
	}
	j.resync = false
	j.playout = true
	j.releaseAt = now
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

// freeSlot returns a slot no longer queued. Called with mu held.
func (j *RTPJitterBuffer) freeSlot(slotIndex int) {
	j.freeSlots = append(j.freeSlots, slotIndex)
}

// Close stops jitter-buffer delivery. It does not close the injected reader,
// and it does not wait for the read loop, which may be blocked in the injected
// reader's ReadRTP where Close cannot interrupt it. See Done.
func (j *RTPJitterBuffer) Close() error {
	j.closeOnce.Do(func() {
		close(j.done)
		j.timer.Stop()
		// A read loop that has not started by now never starts.
		j.startOnce.Do(func() { close(j.readLoopDone) })
	})
	return nil
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

// Statistics returns a race-safe snapshot of jitter buffer counters.
func (j *RTPJitterBuffer) Statistics() RTPJitterBufferStatistics {
	j.mu.Lock()
	packetDuration := j.packetDuration
	j.mu.Unlock()
	return RTPJitterBufferStatistics{
		PacketsRead:        j.stats.packetsRead.Load(),
		PacketsReleased:    j.stats.packetsReleased.Load(),
		PacketsLost:        j.stats.packetsLost.Load(),
		PacketsLate:        j.stats.packetsLate.Load(),
		PacketsDuplicate:   j.stats.packetsDuplicate.Load(),
		PacketsDropped:     j.stats.packetsDropped.Load(),
		SSRCResets:         j.stats.ssrcResets.Load(),
		Underruns:          j.stats.underruns.Load(),
		LastSequenceNumber: uint16(j.stats.lastSequenceNumber.Load()),
		PacketDuration:     packetDuration,
	}
}

func (j *RTPJitterBuffer) debugArrival(slot *rtpJitterSlot) {
	if !rtpJitterDebug {
		return
	}

	now := time.Now()
	if j.lastArrivalSet {
		arrivalDelta := now.Sub(j.lastArrivalTime)
		if arrivalDelta > j.packetDuration+j.packetDuration/2 {
			jitterDebugf("event=delayed_arrival seq=%d prev_seq=%d arrival_delta=%s packet_duration=%s",
				slot.seq,
				j.lastArrivalSeq,
				arrivalDelta,
				j.packetDuration,
			)
		}

		expectedNext := j.lastArrivalSeq + 1
		if slot.seq != expectedNext {
			jitterDebugf("event=out_of_sequence seq=%d prev_seq=%d expected_next=%d arrival_delta=%s",
				slot.seq,
				j.lastArrivalSeq,
				expectedNext,
				arrivalDelta,
			)
		}
	}

	j.lastArrivalTime = now
	j.lastArrivalSeq = slot.seq
	j.lastArrivalSet = true
}

func (j *RTPJitterBuffer) debugPacketDecision(event string, slot *rtpJitterSlot, reason string) {
	if !rtpJitterDebug {
		return
	}

	jitterDebugf("event=%s seq=%d expected_seq=%d queued=%d delay_packets=%d max_packets=%d reason=%s",
		event,
		slot.seq,
		j.expectedSeq,
		j.queued,
		j.delayPackets,
		j.maxPackets,
		reason,
	)
}

func (j *RTPJitterBuffer) debugPlayoutStarted(reason string) {
	if !rtpJitterDebug {
		return
	}

	jitterDebugf("event=playout_started queued=%d delay_packets=%d max_packets=%d reason=%s",
		j.queued,
		j.delayPackets,
		j.maxPackets,
		reason,
	)
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

func jitterDebugf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "JITTER_DEBUG "+format+"\n", args...)
}

func envBool(name string) bool {
	switch strings.ToLower(os.Getenv(name)) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}

var _ RTPReader = (*RTPJitterBuffer)(nil)
var _ io.Closer = (*RTPJitterBuffer)(nil)
