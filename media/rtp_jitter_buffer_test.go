// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

type sliceRTPReader struct {
	packets []rtp.Packet
	pos     int
}

func (r *sliceRTPReader) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	if r.pos >= len(r.packets) {
		return 0, io.EOF
	}

	pkt := r.packets[r.pos]
	r.pos++

	raw, err := pkt.Marshal()
	if err != nil {
		return 0, err
	}
	if len(buf) < len(raw) {
		return 0, io.ErrShortBuffer
	}

	n := copy(buf, raw)
	return n, RTPUnmarshal(buf[:n], p)
}

type chanRTPReader struct {
	packets chan rtp.Packet
}

type countingChanRTPReader struct {
	*chanRTPReader
	reads atomic.Uint64
}

func newChanRTPReader() *chanRTPReader {
	return &chanRTPReader{packets: make(chan rtp.Packet)}
}

func (r *chanRTPReader) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	pkt, ok := <-r.packets
	if !ok {
		return 0, io.EOF
	}

	raw, err := pkt.Marshal()
	if err != nil {
		return 0, err
	}
	if len(buf) < len(raw) {
		return 0, io.ErrShortBuffer
	}

	n := copy(buf, raw)
	return n, RTPUnmarshal(buf[:n], p)
}

// emptyRTPReader returns empty reads, as an RTP source does for a keepalive, so
// a read loop over it never blocks.
type emptyRTPReader struct{}

func (emptyRTPReader) ReadRTP([]byte, *rtp.Packet) (int, error) { return 0, nil }

func (r *countingChanRTPReader) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	n, err := r.chanRTPReader.ReadRTP(buf, p)
	if err == nil && n > 0 {
		r.reads.Add(1)
	}
	return n, err
}

// startSignalRTPReader reports on started each read that begins, before it
// blocks on packets.
type startSignalRTPReader struct {
	*chanRTPReader
	started chan struct{}
}

func newStartSignalRTPReader() *startSignalRTPReader {
	return &startSignalRTPReader{
		chanRTPReader: newChanRTPReader(),
		started:       make(chan struct{}, 8),
	}
}

func (r *startSignalRTPReader) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	r.started <- struct{}{}
	return r.chanRTPReader.ReadRTP(buf, p)
}

func TestRTPJitterBuffer(t *testing.T) {
	t.Run("packetDurationRequired", func(t *testing.T) {
		require.PanicsWithValue(t,
			"media: RTP jitter buffer packetDuration must be greater than zero",
			func() {
				NewRTPJitterBuffer(&sliceRTPReader{}, 0, RTPJitterBufferOptions{})
			},
		)
	})

	t.Run("defaults", func(t *testing.T) {
		// The values RTPJitterBufferOptions documents.
		jb := NewRTPJitterBuffer(&sliceRTPReader{}, time.Millisecond, RTPJitterBufferOptions{})
		require.Equal(t, 20, jb.delayPackets)
		require.Equal(t, 40, jb.maxPackets)

		jb = NewRTPJitterBuffer(&sliceRTPReader{}, time.Millisecond, RTPJitterBufferOptions{DelayPackets: 5, MaxPackets: 2})
		require.Equal(t, 5, jb.delayPackets)
		require.Equal(t, 5, jb.maxPackets, "MaxPackets below DelayPackets is raised to it")
	})

	t.Run("inOrder", func(t *testing.T) {
		jb := newTestRTPJitterBuffer(t, rtpPackets(1234, 0, 1, 2))

		require.Equal(t, uint16(0), readJitterSeq(t, jb))
		require.Equal(t, uint16(1), readJitterSeq(t, jb))
		require.Equal(t, uint16(2), readJitterSeq(t, jb))
		requireJitterEOF(t, jb)
	})

	t.Run("outOfOrder", func(t *testing.T) {
		jb := newTestRTPJitterBuffer(t, rtpPackets(1234, 0, 2, 1, 3))

		require.Equal(t, uint16(0), readJitterSeq(t, jb))
		require.Equal(t, uint16(1), readJitterSeq(t, jb))
		require.Equal(t, uint16(2), readJitterSeq(t, jb))
		require.Equal(t, uint16(3), readJitterSeq(t, jb))
	})

	t.Run("missingPacket", func(t *testing.T) {
		jb := newTestRTPJitterBuffer(t, rtpPackets(1234, 0, 2))

		require.Equal(t, uint16(0), readJitterSeq(t, jb))
		require.Equal(t, uint16(2), readJitterSeq(t, jb))
		require.Equal(t, uint64(1), jb.Statistics().PacketsLost)
	})

	t.Run("duplicatePacket", func(t *testing.T) {
		jb := newTestRTPJitterBuffer(t, rtpPackets(1234, 0, 1, 1, 2))

		require.Equal(t, uint16(0), readJitterSeq(t, jb))
		require.Equal(t, uint16(1), readJitterSeq(t, jb))
		require.Equal(t, uint16(2), readJitterSeq(t, jb))
		require.Equal(t, uint64(1), jb.Statistics().PacketsDuplicate)
	})

	t.Run("sequenceWrap", func(t *testing.T) {
		jb := newTestRTPJitterBuffer(t, rtpPackets(1234, 65534, 0, 65535, 1))

		require.Equal(t, uint16(65534), readJitterSeq(t, jb))
		require.Equal(t, uint16(65535), readJitterSeq(t, jb))
		require.Equal(t, uint16(0), readJitterSeq(t, jb))
		require.Equal(t, uint16(1), readJitterSeq(t, jb))
	})

	t.Run("latePacket", func(t *testing.T) {
		reader := newChanRTPReader()
		jb := NewRTPJitterBuffer(reader, time.Millisecond, RTPJitterBufferOptions{
			DelayPackets: 1,
			MaxPackets:   4,
		})

		done := make(chan uint16, 1)
		go func() {
			done <- readJitterSeq(t, jb)
		}()
		reader.packets <- rtpPacket(1234, 0)
		require.Equal(t, uint16(0), <-done)

		go func() {
			done <- readJitterSeq(t, jb)
		}()
		reader.packets <- rtpPacket(1234, 2)
		require.Equal(t, uint16(2), <-done)

		reader.packets <- rtpPacket(1234, 1)
		close(reader.packets)
		requireJitterEOF(t, jb)
		require.Equal(t, uint64(1), jb.Statistics().PacketsLate)
	})

	t.Run("ssrcReset", func(t *testing.T) {
		reader := newChanRTPReader()
		jb := NewRTPJitterBuffer(reader, time.Millisecond, RTPJitterBufferOptions{
			DelayPackets: 1,
			MaxPackets:   4,
		})

		done := make(chan uint16, 1)
		go func() {
			done <- readJitterSeq(t, jb)
		}()
		reader.packets <- rtpPacket(1111, 0)
		require.Equal(t, uint16(0), <-done)

		go func() {
			done <- readJitterSeq(t, jb)
		}()
		reader.packets <- rtpPacket(2222, 10)
		require.Equal(t, uint16(10), <-done)

		require.Equal(t, uint64(1), jb.Statistics().SSRCResets)
	})

	t.Run("startupEarlierPacket", func(t *testing.T) {
		jb := newTestRTPJitterBuffer(t, rtpPackets(1234, 2, 0, 1, 3))

		require.Equal(t, uint16(0), readJitterSeq(t, jb))
		require.Equal(t, uint16(1), readJitterSeq(t, jb))
		require.Equal(t, uint16(2), readJitterSeq(t, jb))
		require.Equal(t, uint16(3), readJitterSeq(t, jb))
	})

	t.Run("forwardWindowDrop", func(t *testing.T) {
		jb := NewRTPJitterBuffer(&sliceRTPReader{packets: rtpPackets(1234, 0, 4, 1)}, time.Millisecond, RTPJitterBufferOptions{
			DelayPackets: 2,
			MaxPackets:   4,
		})

		require.Equal(t, uint16(0), readJitterSeq(t, jb))
		require.Equal(t, uint16(1), readJitterSeq(t, jb))
		requireJitterEOF(t, jb)
		require.Equal(t, uint64(1), jb.Statistics().PacketsDropped)
	})

	t.Run("shortBufferRetry", func(t *testing.T) {
		jb := NewRTPJitterBuffer(&sliceRTPReader{packets: rtpPackets(1234, 7)}, time.Millisecond, RTPJitterBufferOptions{
			DelayPackets: 1,
			MaxPackets:   2,
		})

		var pkt rtp.Packet
		_, err := jb.ReadRTP(make([]byte, 1), &pkt)
		require.ErrorIs(t, err, io.ErrShortBuffer)
		require.Equal(t, uint16(7), readJitterSeq(t, jb))
	})

	t.Run("slotReuse", func(t *testing.T) {
		reader := &chanRTPReader{packets: make(chan rtp.Packet, 3)}
		jb := NewRTPJitterBuffer(reader, time.Millisecond, RTPJitterBufferOptions{
			DelayPackets: 2,
			MaxPackets:   4,
		})

		reader.packets <- rtpPacket(1234, 0)
		reader.packets <- rtpPacket(1234, 1)
		reader.packets <- rtpPacket(1234, 2)
		require.Equal(t, uint16(0), readJitterSeq(t, jb))
		for seq := uint16(3); seq < 100; seq++ {
			reader.packets <- rtpPacket(1234, seq)
			require.Equal(t, seq-2, readJitterSeq(t, jb))
		}
		require.Equal(t, uint16(98), readJitterSeq(t, jb))
		require.Equal(t, uint16(99), readJitterSeq(t, jb))
		close(reader.packets)
		requireJitterEOF(t, jb)
	})

	t.Run("close", func(t *testing.T) {
		reader := newChanRTPReader()
		jb := NewRTPJitterBuffer(reader, time.Millisecond, RTPJitterBufferOptions{})
		result := make(chan error, 1)
		go func() {
			var pkt rtp.Packet
			_, err := jb.ReadRTP(make([]byte, RTPBufSize), &pkt)
			result <- err
		}()

		require.NoError(t, jb.Close())
		require.NoError(t, jb.Close())
		require.ErrorIs(t, <-result, io.ErrClosedPipe)
		close(reader.packets)
	})

	t.Run("closeAfterReadLoopStopped", func(t *testing.T) {
		jb := NewRTPJitterBuffer(emptyRTPReader{}, time.Millisecond, RTPJitterBufferOptions{})
		jb.start()
		require.NoError(t, jb.Close())

		// The read loop closes input when Close stops it, which is also how it
		// reports the end of the upstream stream. Wait for that before reading.
		requireJitterDone(t, jb)

		// ReadRTP sees Close and the closed input at once, and a select picks
		// among ready cases at random, so a single read proves nothing.
		var pkt rtp.Packet
		for i := 0; i < 64; i++ {
			_, err := jb.ReadRTP(make([]byte, RTPBufSize), &pkt)
			require.ErrorIs(t, err, io.ErrClosedPipe, "read %d after Close", i+1)
		}
	})
}

func TestRTPJitterBufferDone(t *testing.T) {
	t.Run("afterUpstreamEnd", func(t *testing.T) {
		jb := newTestRTPJitterBuffer(t, rtpPackets(1234, 0, 1))

		require.Equal(t, uint16(0), readJitterSeq(t, jb))
		require.Equal(t, uint16(1), readJitterSeq(t, jb))
		requireJitterEOF(t, jb)
		requireJitterDone(t, jb)
	})

	t.Run("closeBeforeRead", func(t *testing.T) {
		reader := newStartSignalRTPReader()
		jb := NewRTPJitterBuffer(reader, time.Millisecond, RTPJitterBufferOptions{})
		require.NoError(t, jb.Close())
		requireJitterDone(t, jb)

		var pkt rtp.Packet
		_, err := jb.ReadRTP(make([]byte, RTPBufSize), &pkt)
		require.ErrorIs(t, err, io.ErrClosedPipe)
		require.Empty(t, reader.started, "a buffer closed before its first read never reads upstream")
	})

	t.Run("closeWaitsForUpstreamRead", func(t *testing.T) {
		reader := newStartSignalRTPReader()
		jb := NewRTPJitterBuffer(reader, time.Millisecond, RTPJitterBufferOptions{})
		jb.start()
		requireSignal(t, reader.started, "the read loop never read upstream")

		// The read loop is blocked in the upstream ReadRTP, which Close cannot
		// interrupt, so it has not stopped yet.
		require.NoError(t, jb.Close())
		select {
		case <-jb.Done():
			t.Fatal("Done closed while the read loop is blocked upstream")
		default:
		}

		// Ending the upstream read is what the owner does to release it.
		close(reader.packets)
		requireJitterDone(t, jb)
	})

	t.Run("noUpstreamReadAfterClose", func(t *testing.T) {
		// The read loop returns from an upstream read after Close with a packet
		// and a free slot, and Close and the slot are both ready at once. A select
		// picks among them at random, so one trial proves nothing.
		for trial := 0; trial < 32; trial++ {
			reader := newStartSignalRTPReader()
			jb := NewRTPJitterBuffer(reader, time.Millisecond, RTPJitterBufferOptions{})
			jb.start()
			requireSignal(t, reader.started, "the read loop never read upstream")

			require.NoError(t, jb.Close())
			sendJitterPacket(t, reader.chanRTPReader, rtpPacket(1234, 0))

			select {
			case <-jb.Done():
			case <-reader.started:
				close(reader.packets)
				t.Fatalf("trial %d: the read loop read upstream again after Close", trial)
			case <-time.After(5 * time.Second):
				close(reader.packets)
				t.Fatalf("trial %d: the read loop did not stop", trial)
			}
			require.Empty(t, reader.started, "trial %d", trial)
			close(reader.packets)
		}
	})
}

func TestRTPJitterBufferEndedUpstreamWaitBlocks(t *testing.T) {
	// The upstream ends with packets still queued and the next release is an
	// hour away. ReadRTP has to wait for it blocked. The read loop has closed
	// input, and a select on a closed channel is always ready, so a ReadRTP that
	// still selects on it spins instead and never parks.
	jb := NewRTPJitterBuffer(&sliceRTPReader{packets: rtpPackets(1234, 0, 1, 2)}, time.Hour, RTPJitterBufferOptions{
		DelayPackets: 2,
		MaxPackets:   8,
	})
	require.Equal(t, uint16(0), readJitterSeq(t, jb))
	requireJitterDone(t, jb)

	result := make(chan error, 1)
	go func() {
		result <- jitterEndedUpstreamRead(jb)
	}()

	var status string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status = goroutineStatus("media.jitterEndedUpstreamRead(")
		if strings.HasPrefix(status, "select") {
			break
		}
		time.Sleep(time.Millisecond)
	}

	require.NoError(t, jb.Close())
	select {
	case err := <-result:
		require.ErrorIs(t, err, io.ErrClosedPipe)
	case <-time.After(5 * time.Second):
		t.Fatal("ReadRTP did not return after Close")
	}
	require.True(t, strings.HasPrefix(status, "select"),
		"ReadRTP never blocked waiting for its next release, last seen %q", status)
}

// jitterEndedUpstreamRead is a named frame, so its goroutine can be found in a
// stack dump.
func jitterEndedUpstreamRead(jb *RTPJitterBuffer) error {
	var pkt rtp.Packet
	_, err := jb.ReadRTP(make([]byte, RTPBufSize), &pkt)
	return err
}

// goroutineStatus returns the status a stack dump prints for the goroutine
// whose stack contains fn, such as "select" or "runnable", or "" if no
// goroutine's does.
func goroutineStatus(fn string) string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, fn) {
			continue
		}
		header, _, _ := strings.Cut(g, "\n")
		_, status, _ := strings.Cut(header, "[")
		status, _, _ = strings.Cut(status, "]")
		return status
	}
	return ""
}

func TestRTPJitterBufferOverflow(t *testing.T) {
	t.Skip("Jitter Buffer Overflow needs WORK")
	t.Run("continuesReading", func(t *testing.T) {
		reader := &countingChanRTPReader{
			chanRTPReader: &chanRTPReader{packets: make(chan rtp.Packet, 32)},
		}
		jb := NewRTPJitterBuffer(reader, time.Millisecond, RTPJitterBufferOptions{
			DelayPackets: 3,
			MaxPackets:   5,
		})
		t.Cleanup(func() {
			_ = jb.Close()
			close(reader.packets)
		})

		first := make(chan uint16, 1)
		go func() {
			first <- readJitterSeq(t, jb)
		}()
		for seq := uint16(0); seq < 3; seq++ {
			reader.packets <- rtpPacket(1234, seq)
		}
		require.Equal(t, uint16(0), <-first)

		for seq := uint16(3); seq <= 20; seq++ {
			reader.packets <- rtpPacket(1234, seq)
		}
		require.Eventually(t, func() bool {
			return reader.reads.Load() == 21
		}, 100*time.Millisecond, time.Millisecond,
			"upstream reading must continue when all playout slots are occupied")
	})

	t.Run("discardsStalePackets", func(t *testing.T) {
		reader := &countingChanRTPReader{
			chanRTPReader: &chanRTPReader{packets: make(chan rtp.Packet, 32)},
		}
		jb := NewRTPJitterBuffer(reader, time.Millisecond, RTPJitterBufferOptions{
			DelayPackets: 3,
			MaxPackets:   5,
		})
		t.Cleanup(func() {
			_ = jb.Close()
			close(reader.packets)
		})

		first := make(chan uint16, 1)
		go func() {
			first <- readJitterSeq(t, jb)
		}()
		for seq := uint16(0); seq < 3; seq++ {
			reader.packets <- rtpPacket(1234, seq)
		}
		require.Equal(t, uint16(0), <-first)

		for seq := uint16(3); seq <= 20; seq++ {
			reader.packets <- rtpPacket(1234, seq)
		}
		require.Eventually(t, func() bool {
			return reader.reads.Load() >= 6
		}, 100*time.Millisecond, time.Millisecond)

		seq := readJitterSeq(t, jb)
		require.GreaterOrEqual(t, seq, uint16(18),
			"playout should resume near the newest packet instead of returning stale audio")
	})
}

func TestRTPJitterBufferRealtimeSimulation(t *testing.T) {
	const (
		ssrc        = 1234
		packetCount = 300
	)
	packetDuration := 20 * time.Millisecond
	reader := &chanRTPReader{packets: make(chan rtp.Packet, packetCount)}
	jb := NewRTPJitterBuffer(reader, packetDuration, RTPJitterBufferOptions{
		DelayPackets: 25,
		MaxPackets:   32,
	})
	t.Cleanup(func() {
		_ = jb.Close()
	})

	wantPayloads := make(map[uint16][]byte, packetCount)
	packets := make([]rtp.Packet, 0, packetCount)
	for seq := uint16(0); seq < packetCount; seq++ {
		pkt := realtimeJitterRTPPacket(ssrc, seq)
		wantPayloads[seq] = append([]byte(nil), pkt.Payload...)
		packets = append(packets, pkt)
	}

	readResult := make(chan error, 1)
	go func() {
		readResult <- readRealtimeJitterPackets(jb, packetCount, wantPayloads)
	}()

	timedProducerDone := make(chan struct{})
	go func() {
		defer close(timedProducerDone)
		defer close(reader.packets)
		writeTimedRTPPackets(reader.packets, packets, packetDuration)
	}()

	select {
	case err := <-readResult:
		require.NoError(t, err)
	case <-time.After(8 * time.Second):
		_ = jb.Close()
		t.Fatal("timed out waiting for realtime jitter buffer simulation")
	}

	<-timedProducerDone
	stats := jb.Statistics()
	require.Equal(t, uint64(packetCount), stats.PacketsRead)
	require.Equal(t, uint64(packetCount), stats.PacketsReleased)
	require.Equal(t, uint64(0), stats.PacketsLost)
	require.Equal(t, uint64(0), stats.PacketsLate)
	require.Equal(t, uint64(0), stats.PacketsDropped)
	require.Equal(t, uint64(0), stats.PacketsDuplicate)
}

type benchmarkRTPReader struct {
	raw  []byte
	next chan uint16
}

func newBenchmarkRTPReader() *benchmarkRTPReader {
	raw, err := rtpPacket(1234, 0).Marshal()
	if err != nil {
		panic(err)
	}
	return &benchmarkRTPReader{
		raw:  raw,
		next: make(chan uint16, 3),
	}
}

func (r *benchmarkRTPReader) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	seq, ok := <-r.next
	if !ok {
		return 0, io.EOF
	}
	n := copy(buf, r.raw)
	binary.BigEndian.PutUint16(buf[2:4], seq)
	return n, RTPUnmarshal(buf[:n], p)
}

func BenchmarkRTPJitterBuffer(b *testing.B) {
	reader := newBenchmarkRTPReader()
	jb := NewRTPJitterBuffer(reader, 10*time.Microsecond, RTPJitterBufferOptions{
		DelayPackets: 2,
		MaxPackets:   8,
	})
	b.Cleanup(func() {
		_ = jb.Close()
		close(reader.next)
	})

	buf := make([]byte, RTPBufSize)
	var pkt rtp.Packet
	reader.next <- 0
	reader.next <- 1
	if _, err := jb.ReadRTP(buf, &pkt); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		reader.next <- uint16(i + 2)
		if _, err := jb.ReadRTP(buf, &pkt); err != nil {
			b.Fatal(err)
		}
	}
}

func newTestRTPJitterBuffer(t *testing.T, packets []rtp.Packet) *RTPJitterBuffer {
	t.Helper()

	return NewRTPJitterBuffer(&sliceRTPReader{packets: packets}, time.Millisecond, RTPJitterBufferOptions{
		DelayPackets: 2,
		MaxPackets:   8,
	})
}

func readJitterSeq(t *testing.T, jb *RTPJitterBuffer) uint16 {
	t.Helper()

	buf := make([]byte, RTPBufSize)
	var pkt rtp.Packet
	_, err := jb.ReadRTP(buf, &pkt)
	require.NoError(t, err)
	return pkt.SequenceNumber
}

func requireJitterEOF(t *testing.T, jb *RTPJitterBuffer) {
	t.Helper()

	buf := make([]byte, RTPBufSize)
	var pkt rtp.Packet
	_, err := jb.ReadRTP(buf, &pkt)
	require.ErrorIs(t, err, io.EOF)
}

func requireJitterDone(t *testing.T, jb *RTPJitterBuffer) {
	t.Helper()

	select {
	case <-jb.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the read loop did not stop")
	}
}

func requireSignal(t *testing.T, signal <-chan struct{}, msg string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(5 * time.Second):
		t.Fatal(msg)
	}
}

func sendJitterPacket(t *testing.T, reader *chanRTPReader, pkt rtp.Packet) {
	t.Helper()

	select {
	case reader.packets <- pkt:
	case <-time.After(5 * time.Second):
		t.Fatalf("the read loop did not take packet %d", pkt.SequenceNumber)
	}
}

func rtpPackets(ssrc uint32, seqs ...uint16) []rtp.Packet {
	packets := make([]rtp.Packet, 0, len(seqs))
	for _, seq := range seqs {
		packets = append(packets, rtpPacket(ssrc, seq))
	}
	return packets
}

func rtpPacket(ssrc uint32, seq uint16) rtp.Packet {
	return rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    8,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 160,
			SSRC:           ssrc,
		},
		Payload: []byte{byte(seq), byte(seq >> 8)},
	}
}

func realtimeJitterRTPPacket(ssrc uint32, seq uint16) rtp.Packet {
	payload := make([]byte, 12)
	binary.BigEndian.PutUint32(payload[:4], uint32(seq))
	for i := 4; i < len(payload); i++ {
		payload[i] = byte((uint32(seq)*31 + uint32(i)) & 0xff)
	}

	return rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    8,
			SequenceNumber: seq,
			Timestamp:      uint32(seq) * 160,
			SSRC:           ssrc,
		},
		Payload: payload,
	}
}

func readRealtimeJitterPackets(jb *RTPJitterBuffer, packetCount int, wantPayloads map[uint16][]byte) error {
	buf := make([]byte, RTPBufSize)
	var pkt rtp.Packet
	for wantSeq := uint16(0); wantSeq < uint16(packetCount); wantSeq++ {
		if _, err := jb.ReadRTP(buf, &pkt); err != nil {
			return fmt.Errorf("read sequence %d: %w", wantSeq, err)
		}
		if pkt.SequenceNumber != wantSeq {
			return fmt.Errorf("sequence mismatch: got %d, want %d", pkt.SequenceNumber, wantSeq)
		}
		if len(pkt.Payload) < 4 {
			return fmt.Errorf("payload for sequence %d is too short: %d", wantSeq, len(pkt.Payload))
		}
		payloadSeq := binary.BigEndian.Uint32(pkt.Payload[:4])
		if payloadSeq != uint32(pkt.SequenceNumber) {
			return fmt.Errorf("payload sequence mismatch: got %d, packet sequence %d", payloadSeq, pkt.SequenceNumber)
		}
		if !bytes.Equal(pkt.Payload, wantPayloads[wantSeq]) {
			return fmt.Errorf("payload bytes mismatch for sequence %d", wantSeq)
		}
	}
	return nil
}

type timedRTPPacket struct {
	packet  rtp.Packet
	arrival time.Duration
}

func writeTimedRTPPackets(dst chan<- rtp.Packet, packets []rtp.Packet, packetDuration time.Duration) {
	scheduled := make([]timedRTPPacket, 0, len(packets))
	for _, pkt := range packets {
		seq := pkt.SequenceNumber
		scheduled = append(scheduled, timedRTPPacket{
			packet:  pkt,
			arrival: time.Duration(seq)*packetDuration + realtimeJitterDelay(seq),
		})
	}
	sort.SliceStable(scheduled, func(i, j int) bool {
		return scheduled[i].arrival < scheduled[j].arrival
	})

	start := time.Now()
	for _, item := range scheduled {
		time.Sleep(time.Until(start.Add(item.arrival)))
		dst <- item.packet
	}
}

func realtimeJitterDelay(seq uint16) time.Duration {
	switch {
	case seq < 25:
		return 0
	case seq < 55:
		return time.Duration(50+int(seq%8)*20) * time.Millisecond
	default:
		return time.Duration(100+int(seq%11)*30) * time.Millisecond
	}
}
