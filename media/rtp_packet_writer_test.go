// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/fakes"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func fakeMediaSessionWriter(lport int, rport int, rtpWriter io.Writer) *MediaSession {
	sess := &MediaSession{
		Codecs: []Codec{CodecAudioAlaw, CodecAudioUlaw},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Raddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234},
	}

	conn := &fakes.UDPConn{
		Writers: map[string]io.Writer{
			sess.Raddr.String(): rtpWriter,
		},
	}
	sess.rtpConn = conn
	return sess
}

func TestRTPWriter(t *testing.T) {
	rtpConn := bytes.NewBuffer([]byte{})
	sess := fakeMediaSessionWriter(0, 1234, rtpConn)
	rtpSession := NewRTPSession(sess)
	rtpWriter := NewRTPPacketWriterSession(rtpSession)

	payload := []byte("12312313")
	N := 10
	for i := 0; i < N; i++ {
		_, err := rtpWriter.Write(payload)
		require.NoError(t, err)

		pkt := rtpWriter.PacketHeader

		require.Equal(t, rtpWriter.codec.PayloadType, pkt.PayloadType)
		require.Equal(t, rtpWriter.SSRC, pkt.SSRC)
		require.Equal(t, rtpWriter.seqWriter.ReadExtendedSeq(), uint64(pkt.SequenceNumber))
		require.Equal(t, rtpWriter.nextTimestamp, pkt.Timestamp+160, "%d vs %d", rtpWriter.nextTimestamp, pkt.Timestamp)
		require.Equal(t, i == 0, pkt.Marker)
	}
}

func BenchmarkRTPPacketWriter(b *testing.B) {
	reader, writer := io.Pipe()
	session := fakeMediaSessionWriter(0, 1234, writer)
	rtpSess := NewRTPSession(session)
	w := NewRTPPacketWriterSession(rtpSess)
	w.clockTicker.Reset(1 * time.Nanosecond)

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		_, _ = io.Copy(io.Discard, reader)
	}()
	// Closing the write side ends the reader, and the benchmark waits for it.
	defer func() {
		writer.Close()
		select {
		case <-readerDone:
		case <-time.After(5 * time.Second):
			b.Error("the pipe reader did not stop")
		}
	}()

	data := make([]byte, 160)

	for i := 0; i < b.N; i++ {
		_, err := w.Write(data)
		if err != nil {
			b.Error(err)
		}
	}

	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "writes/s")
}

// TestRTPPacketWriterClockDisable pins that with the clock disabled Write sends
// the packet and returns without waiting on the clock: after ClockDisable, when
// ClockDisable comes while a Write waits on the clock, and after a media update,
// which keeps the clock disabled. The clock here ticks once an hour, so a Write
// that waits on it does not return within the test.
func TestRTPPacketWriterClockDisable(t *testing.T) {
	newRTPSession := func() *RTPSession {
		sess := fakeMediaSessionWriter(0, 1234, bytes.NewBuffer(nil))
		sess.Codecs = []Codec{{PayloadType: 0, SampleRate: 8000, SampleDur: time.Hour, NumChannels: 1, Name: "PCMU"}}
		return NewRTPSession(sess)
	}

	t.Run("writeAfterDisable", func(t *testing.T) {
		rtpSess := newRTPSession()
		w := NewRTPPacketWriterSession(rtpSess)
		w.ClockDisable()
		requireRTPWriterWriteReturns(t, rtpWriterWriteAsync(w))
		requireRTPWriterWriteReturns(t, rtpWriterWriteAsync(w))
		require.Equal(t, uint64(2), rtpSess.WriteStats().PacketsCount)
	})

	t.Run("disableWhileWriteWaits", func(t *testing.T) {
		rtpSess := newRTPSession()
		w := NewRTPPacketWriterSession(rtpSess)
		requireDisableEndsWrite(t, w)
		require.Equal(t, uint64(1), rtpSess.WriteStats().PacketsCount)
	})

	t.Run("disableAfterEnable", func(t *testing.T) {
		w := NewRTPPacketWriterSession(newRTPSession())
		w.ClockDisable()
		w.ClockEnable()
		requireDisableEndsWrite(t, w)
	})

	t.Run("mediaUpdateKeepsClockDisabled", func(t *testing.T) {
		w := NewRTPPacketWriterSession(newRTPSession())
		w.ClockDisable()
		rtpSess := newRTPSession()
		w.UpdateRTPSession(rtpSess)
		requireRTPWriterWriteReturns(t, rtpWriterWriteAsync(w))
		require.Equal(t, uint64(1), rtpSess.WriteStats().PacketsCount)
	})
}

type rtpWriterWriteResult struct {
	err      error
	panicked any
}

// rtpWriterWriteAsync writes one payload in its own goroutine and returns where
// its result arrives, so a Write that blocks or panics fails the test instead of
// hanging or ending it.
func rtpWriterWriteAsync(w *RTPPacketWriter) <-chan rtpWriterWriteResult {
	result := make(chan rtpWriterWriteResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				result <- rtpWriterWriteResult{panicked: r}
			}
		}()
		result <- rtpWriterWriteResult{err: rtpWriterClockWrite(w)}
	}()
	return result
}

// rtpWriterClockWrite is a named frame, so the goroutine writing can be found in
// a stack dump.
func rtpWriterClockWrite(w *RTPPacketWriter) error {
	_, err := w.Write(make([]byte, 160))
	return err
}

func requireRTPWriterWriteReturns(t *testing.T, result <-chan rtpWriterWriteResult) {
	t.Helper()
	select {
	case r := <-result:
		require.Nil(t, r.panicked, "Write panicked")
		require.NoError(t, r.err)
	case <-time.After(5 * time.Second):
		t.Fatal("Write did not return")
	}
}

// requireDisableEndsWrite starts a Write, waits until it blocks on the clock,
// and checks that ClockDisable lets it return.
func requireDisableEndsWrite(t *testing.T, w *RTPPacketWriter) {
	t.Helper()
	result := rtpWriterWriteAsync(w)
	var status string
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		status = goroutineStatus("media.rtpWriterClockWrite(")
		if status == "select" || status == "chan receive" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	require.Contains(t, []string{"select", "chan receive"}, status, "Write never waited on the clock")

	w.ClockDisable()
	requireRTPWriterWriteReturns(t, result)
}

// seqRecordingRTPWriter records the sequence number of every packet written.
type seqRecordingRTPWriter struct {
	mu   sync.Mutex
	seqs []uint16
}

func (w *seqRecordingRTPWriter) WriteRTP(p *rtp.Packet) error {
	w.mu.Lock()
	w.seqs = append(w.seqs, p.SequenceNumber)
	w.mu.Unlock()
	return nil
}

// TestRTPPacketWriterOverlappingPlaybacks is two playbacks on one dialog that
// overlap: each resets the timestamp when it starts, as PlaybackCreate's
// onPlay does, and writes its frames, while the other writes. Write recorded
// the time of its last frame without the lock ResetTimestamp reads it under,
// and wrote the packet under a read lock that two writers share, so -race
// reported both. Every packet must also get a sequence number of its own.
func TestRTPPacketWriterOverlappingPlaybacks(t *testing.T) {
	for _, clock := range []bool{false, true} {
		t.Run(fmt.Sprintf("clock=%t", clock), func(t *testing.T) {
			rec := &seqRecordingRTPWriter{}
			codec := CodecAudioUlaw
			codec.SampleDur = time.Millisecond
			w := NewRTPPacketWriter(rec, codec)
			if !clock {
				w.ClockDisable()
			}
			t.Cleanup(w.ClockDisable)

			const playbacks, frames = 2, 50
			var wg sync.WaitGroup
			errs := make(chan error, playbacks)
			for i := 0; i < playbacks; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					w.ResetTimestamp()
					payload := make([]byte, 160)
					for j := 0; j < frames; j++ {
						if _, err := w.Write(payload); err != nil {
							errs <- err
							return
						}
					}
				}()
			}
			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(10 * time.Second):
				t.Fatal("the playbacks did not finish")
			}
			close(errs)
			for err := range errs {
				require.NoError(t, err)
			}

			rec.mu.Lock()
			defer rec.mu.Unlock()
			require.Len(t, rec.seqs, playbacks*frames)
			seen := make(map[uint16]bool, len(rec.seqs))
			for _, seq := range rec.seqs {
				require.False(t, seen[seq], "sequence number %d was written twice", seq)
				seen[seq] = true
			}
		})
	}
}
