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
)

var rtpJitterDebug = envBool("JITTER_DEBUG")

// RTPJitterBufferOptions configures a fixed RTP jitter buffer.
type RTPJitterBufferOptions struct {
	// DelayPackets is the initial fixed playout delay in packets. If unset, 20 is used.
	DelayPackets int
	// MaxPackets caps buffered packets and the forward reordering window. If unset, 40 is used.
	// A value below DelayPackets is raised to DelayPackets.
	MaxPackets int
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

type rtpJitterInput struct {
	slot int
	err  error
}

// RTPJitterBuffer is a fixed-delay, single-consumer RTPReader wrapper.
//
// It sits after RTPSession and before RTPPacketReader, so RTPSession observes
// true network arrival while downstream readers get reordered packets.
//
// Playout releases one packet per packetDuration, and skips a missing packet as
// lost when later ones are queued. When nothing is queued at all, because the
// sender paused (hold, silence suppression, DTX) or the network stalled for
// longer than the playout delay, playout stops, counting an underrun and no
// packet lost, and buffers again, as at the start of the stream, from the next
// packet at or after where it stopped. Until it plays again, a packet behind
// that point is late, except that one MaxPackets or more away in either
// direction, arriving with nothing queued, starts the stream again, as from a
// sender that restarted its sequence numbers.
type RTPJitterBuffer struct {
	// mu guards reader and packetDuration, which UpdateRTPSession changes while
	// the read loop and ReadRTP use them.
	mu sync.Mutex
	// reader is the upstream network-facing RTP source.
	reader RTPReader
	// packetDuration controls the interval between playout decisions.
	packetDuration time.Duration

	// delayPackets is the number of queued packets required to start playout early.
	delayPackets int
	// maxPackets is both the queue capacity and accepted forward sequence window.
	maxPackets int

	// input transfers ownership of filled slot indexes from readLoop to ReadRTP.
	input chan rtpJitterInput
	// freeSlots transfers ownership of reusable slot indexes back to readLoop.
	freeSlots chan int
	// done is closed by Close to stop internal channel operations and delivery.
	done chan struct{}
	// closeOnce makes Close idempotent.
	closeOnce sync.Once
	// startOnce ensures that only one upstream reader goroutine is launched.
	startOnce sync.Once
	// readLoopDone is closed when readLoop returns, or by Close when readLoop
	// never started.
	readLoopDone chan struct{}

	// slots contains all reusable packet metadata and packet-sized byte regions.
	slots []rtpJitterSlot
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

	// initialTimer limits how long startup waits to collect delayPackets.
	initialTimer *time.Timer
	// playoutTimer schedules the next packet release or loss decision.
	playoutTimer *time.Timer
	// playout reports whether the initial buffering phase has completed.
	playout bool
	// releaseNow reports that the next expected sequence may be processed now.
	releaseNow bool
	// startNow reports that the initial timer expired, so playout starts at the
	// next decision.
	startNow bool
	// resync reports that playout stopped with nothing queued. Until it starts
	// again, expectedSeq is the first sequence number not yet played, and
	// packets behind it are late.
	resync bool

	// inputClosed reports that readLoop has stopped producing packet events.
	inputClosed bool
	// readErr stores the terminal upstream error returned after queued packets drain.
	readErr error
	// lastArrivalTime is used only for JITTER_DEBUG receive-side diagnostics.
	lastArrivalTime time.Time
	// lastArrivalSeq is the previous RTP sequence observed by readLoop.
	lastArrivalSeq uint16
	// lastArrivalSet reports whether receive-side debug arrival state is initialized.
	lastArrivalSet bool
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

	// One extra slot lets the network reader finish one read while the configured
	// jitter window is full. All packet storage is allocated once here.
	slotCount := opts.MaxPackets + 1
	data := make([]byte, slotCount*RTPBufSize)
	slots := make([]rtpJitterSlot, slotCount)
	freeSlots := make(chan int, slotCount)
	for i := range slots {
		slots[i].raw = data[i*RTPBufSize : (i+1)*RTPBufSize]
		freeSlots <- i
	}

	sequence := make([]int, opts.MaxPackets)
	for i := range sequence {
		sequence[i] = -1
	}

	initialTimer := time.NewTimer(time.Hour)
	initialTimer.Stop()
	playoutTimer := time.NewTimer(time.Hour)
	playoutTimer.Stop()

	return &RTPJitterBuffer{
		reader:         reader,
		packetDuration: packetDuration,
		delayPackets:   opts.DelayPackets,
		maxPackets:     opts.MaxPackets,
		input:          make(chan rtpJitterInput, slotCount),
		freeSlots:      freeSlots,
		done:           make(chan struct{}),
		readLoopDone:   make(chan struct{}),
		slots:          slots,
		sequence:       sequence,
		initialTimer:   initialTimer,
		playoutTimer:   playoutTimer,
	}
}

// ReadRTP implements RTPReader. Calls must not overlap.
func (j *RTPJitterBuffer) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	j.start()

	for {
		// Close wins over anything queued or in flight. The read loop closes input
		// when Close stops it, which is also how it reports the end of the
		// upstream stream, so input alone cannot tell the two apart.
		select {
		case <-j.done:
			j.stopTimers()
			return 0, io.ErrClosedPipe
		default:
		}

		// A due timer and arrived input are ready together, and a select picks
		// among them at random. Taking what has arrived first means no playout
		// decision misses a packet still waiting in input.
		j.drainInput()

		if !j.playout && (j.startNow || j.queued >= j.delayPackets) {
			if j.startNow {
				j.debugPlayoutStarted("initial_timer")
			} else {
				j.debugPlayoutStarted("delay_packets")
			}
			j.startPlayout()
		}

		if j.playout && j.releaseNow {
			position := int(j.expectedSeq) % j.maxPackets
			slotIndex := j.sequence[position]
			if slotIndex >= 0 && j.slots[slotIndex].seq == j.expectedSeq {
				slot := &j.slots[slotIndex]
				if len(buf) < slot.n {
					return 0, io.ErrShortBuffer
				}

				j.sequence[position] = -1
				j.queued--
				j.expectedSeq++
				j.releaseNow = false
				j.resetPlayoutTimer()

				n := copy(buf, slot.raw[:slot.n])
				err := RTPUnmarshal(buf[:n], p)
				j.recycleSlot(slotIndex)
				if err != nil {
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
				j.releaseNow = false
				j.resetPlayoutTimer()
			}
		}

		if j.inputClosed && j.queued == 0 {
			j.stopTimers()
			if j.readErr != nil {
				return 0, j.readErr
			}
			return 0, io.EOF
		}

		// Once the read loop has ended, input is closed and always ready, so it
		// is left out and only the timers wake the remaining playout.
		var inputC <-chan rtpJitterInput
		if !j.inputClosed {
			inputC = j.input
		}

		var initialC <-chan time.Time
		if !j.playout && j.expectedSet {
			initialC = j.initialTimer.C
		}

		var playoutC <-chan time.Time
		if j.playout && !j.releaseNow {
			playoutC = j.playoutTimer.C
		}

		select {
		case <-j.done:
			j.stopTimers()
			return 0, io.ErrClosedPipe

		case input, ok := <-inputC:
			j.handleInput(input, ok)

		case <-initialC:
			j.startNow = true

		case <-playoutC:
			j.releaseNow = true
		}
	}
}

// drainInput takes the input that has already arrived. It takes at most as
// many as input holds, so a source whose packets are all dropped cannot keep
// ReadRTP from its playout decisions.
func (j *RTPJitterBuffer) drainInput() {
	for i := 0; i < cap(j.input) && !j.inputClosed; i++ {
		select {
		case input, ok := <-j.input:
			j.handleInput(input, ok)
		default:
			return
		}
	}
}

func (j *RTPJitterBuffer) handleInput(input rtpJitterInput, ok bool) {
	if !ok {
		j.inputClosed = true
		return
	}
	if input.err != nil {
		j.readErr = input.err
		j.inputClosed = true
		return
	}
	j.handleSlot(input.slot)
}

func (j *RTPJitterBuffer) start() {
	j.startOnce.Do(func() {
		go j.readLoop()
	})
}

func (j *RTPJitterBuffer) readLoop() {
	defer close(j.readLoopDone)
	defer close(j.input)

	for {
		var slotIndex int
		select {
		case <-j.done:
			return
		case slotIndex = <-j.freeSlots:
		}
		// A select picks among ready cases at random, so a free slot can win over
		// Close. Once the loop has seen Close it starts no further upstream read.
		select {
		case <-j.done:
			return
		default:
		}

		slot := &j.slots[slotIndex]
		n, err := j.readUpstream(slot)
		if err != nil {
			j.sendInput(rtpJitterInput{err: err})
			return
		}
		if n == 0 {
			j.recycleSlot(slotIndex)
			continue
		}

		slot.n = n
		slot.ssrc = slot.packet.SSRC
		slot.seq = slot.packet.SequenceNumber
		j.debugArrival(slot)
		if !j.sendInput(rtpJitterInput{slot: slotIndex}) {
			return
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

func (j *RTPJitterBuffer) duration() time.Duration {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.packetDuration
}

// UpdateRTPSession moves the buffer onto rtpSess, the session a media update
// installed in place of the one it reads, and paces playout by the packet
// duration of its negotiated audio codec. What is queued and the playout state
// carry over. A read the read loop has in progress on the previous reader
// finishes there, or, if it fails, for example because the previous session
// was closed, continues on rtpSess.
func (j *RTPJitterBuffer) UpdateRTPSession(rtpSess *RTPSession) {
	packetDuration := CodecAudioFromSession(rtpSess.Sess).SampleDur
	j.mu.Lock()
	defer j.mu.Unlock()
	j.reader = rtpSess
	if packetDuration > 0 {
		j.packetDuration = packetDuration
	}
}

func (j *RTPJitterBuffer) sendInput(input rtpJitterInput) bool {
	select {
	case <-j.done:
		return false
	case j.input <- input:
		return true
	}
}

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
	if distance < 0 {
		if j.playout || j.resync {
			j.stats.packetsLate.Add(1)
			j.debugPacketDecision("late", slot, "behind_playout")
			j.recycleSlot(slotIndex)
			return
		}
		if !j.canMoveExpectedBack(slot.seq) {
			j.stats.packetsDropped.Add(1)
			j.debugPacketDecision("dropped", slot, "before_window")
			j.recycleSlot(slotIndex)
			return
		}
		j.expectedSeq = slot.seq
		distance = 0
	}

	if int(distance) >= j.maxPackets {
		j.stats.packetsDropped.Add(1)
		j.debugPacketDecision("dropped", slot, "beyond_window")
		j.recycleSlot(slotIndex)
		return
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
		j.recycleSlot(slotIndex)
		return
	}

	j.sequence[position] = slotIndex
	j.queued++
	if j.resync && j.queued == 1 {
		// The stream resumed, so buffer again as at its start.
		j.resetTimer(j.initialTimer, time.Duration(j.delayPackets)*j.duration())
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

func (j *RTPJitterBuffer) resetStream(ssrc uint32, seq uint16) {
	j.clearSequence()
	j.ssrc = ssrc
	j.ssrcSet = true
	j.expectedSeq = seq
	j.expectedSet = true
	j.playout = false
	j.releaseNow = false
	j.startNow = false
	j.resync = false
	j.stopAndDrainTimer(j.playoutTimer)
	j.resetTimer(j.initialTimer, time.Duration(j.delayPackets)*j.duration())
}

func (j *RTPJitterBuffer) clearSequence() {
	for i, slotIndex := range j.sequence {
		if slotIndex < 0 {
			continue
		}
		j.sequence[i] = -1
		j.recycleSlot(slotIndex)
	}
	j.queued = 0
}

func (j *RTPJitterBuffer) startPlayout() {
	j.stopAndDrainTimer(j.initialTimer)
	// Starting again after a stop, the first packet queued can be ahead of
	// expectedSeq. The sequence numbers before it were lost, and playout starts
	// at that packet instead of spending a tick on each of them.
	for j.resync && j.queued > 0 && j.sequence[int(j.expectedSeq)%j.maxPackets] < 0 {
		j.stats.packetsLost.Add(1)
		j.expectedSeq++
	}
	j.resync = false
	j.startNow = false
	j.playout = true
	j.releaseNow = true
}

// stopPlayout stops playout when it finds nothing queued. Counting every tick
// of a sender's pause as a lost packet would move expectedSeq onto sequence
// numbers the sender has not used yet, and every packet after the pause would
// then be late. Playout waits for the stream instead and keeps expectedSeq.
func (j *RTPJitterBuffer) stopPlayout() {
	j.stats.underruns.Add(1)
	j.debugPlayoutStopped()
	j.playout = false
	j.releaseNow = false
	j.resync = true
	j.stopAndDrainTimer(j.playoutTimer)
}

func (j *RTPJitterBuffer) resetPlayoutTimer() {
	j.resetTimer(j.playoutTimer, j.duration())
}

func (j *RTPJitterBuffer) resetTimer(timer *time.Timer, duration time.Duration) {
	j.stopAndDrainTimer(timer)
	timer.Reset(duration)
}

func (j *RTPJitterBuffer) stopAndDrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (j *RTPJitterBuffer) stopTimers() {
	j.stopAndDrainTimer(j.initialTimer)
	j.stopAndDrainTimer(j.playoutTimer)
}

func (j *RTPJitterBuffer) recycleSlot(slotIndex int) {
	select {
	case j.freeSlots <- slotIndex:
	case <-j.done:
	}
}

// Close stops jitter-buffer delivery. It does not close the injected reader,
// and it does not wait for the read loop, which may be blocked in the injected
// reader's ReadRTP where Close cannot interrupt it. See Done.
func (j *RTPJitterBuffer) Close() error {
	j.closeOnce.Do(func() {
		close(j.done)
		j.stopTimers()
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
	}
}

func (j *RTPJitterBuffer) debugArrival(slot *rtpJitterSlot) {
	if !rtpJitterDebug {
		return
	}

	now := time.Now()
	packetDuration := j.duration()
	if j.lastArrivalSet {
		arrivalDelta := now.Sub(j.lastArrivalTime)
		if arrivalDelta > packetDuration+packetDuration/2 {
			jitterDebugf("event=delayed_arrival seq=%d prev_seq=%d arrival_delta=%s packet_duration=%s",
				slot.seq,
				j.lastArrivalSeq,
				arrivalDelta,
				packetDuration,
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
