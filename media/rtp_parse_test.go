// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"io"
	"testing"
	"time"

	"github.com/emiago/sipgo/fakes"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
)

func BenchmarkRTCPUnmarshal(b *testing.B) {
	reader, writer := io.Pipe()
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			sr := rtcp.SenderReport{}
			data, err := sr.Marshal()
			if err != nil {
				return
			}

			if _, err := writer.Write(data); err != nil {
				return
			}
		}
	}()
	defer stopPipeWriter(b, reader, writerDone)

	b.Run("pionRTCP", func(b *testing.B) {
		buf := make([]byte, 1500)
		for i := 0; i < b.N; i++ {
			n, err := reader.Read(buf)
			if err != nil {
				b.Fatal(err)
			}
			pkts, err := rtcp.Unmarshal(buf[:n])
			if err != nil {
				b.Fatal(err)
			}
			if len(pkts) == 0 {
				b.Fatal("no packet read")
			}
		}
	})

	b.Run("RTCPImproved", func(b *testing.B) {
		buf := make([]byte, 1500)
		pkts := make([]rtcp.Packet, 5)
		for i := 0; i < b.N; i++ {
			n, err := reader.Read(buf)
			if err != nil {
				b.Fatal(err)
			}
			n, err = RTCPUnmarshal(buf[:n], pkts)
			if err != nil {
				b.Fatal(err)
			}
			if n < 0 {
				b.Fatal("no read RTCP")
			}
		}
	})
}

func BenchmarkReadRTP(b *testing.B) {
	session := &MediaSession{}
	reader, writer := io.Pipe()
	session.rtpConn = &fakes.UDPConn{
		Reader: reader,
	}

	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			pkt := rtp.Packet{
				Payload: make([]byte, 160),
			}
			data, err := pkt.Marshal()
			if err != nil {
				return
			}
			if _, err := writer.Write(data); err != nil {
				return
			}
		}
	}()
	defer stopPipeWriter(b, reader, writerDone)

	// A media session is read by one goroutine at a time, so each parallel body
	// reads through a session of its own over the shared connection. A body
	// runs off the benchmark goroutine, so it reports a failure with Error and
	// returns rather than calling Fatal.
	b.Run("return", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		b.RunParallel(func(p *testing.PB) {
			sess := &MediaSession{rtpConn: session.rtpConn}
			for p.Next() {
				pkt, err := sess.readRTPParsed()
				if err != nil {
					b.Error(err)
					return
				}
				if len(pkt.Payload) != 160 {
					b.Error("payload not parsed")
					return
				}
			}
		})

	})

	b.Run("pass", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		b.RunParallel(func(p *testing.PB) {
			sess := &MediaSession{rtpConn: session.rtpConn}
			buf := make([]byte, RTPBufSize)
			for p.Next() {
				pkt := rtp.Packet{}
				_, err := sess.ReadRTP(buf, &pkt)
				if err != nil {
					b.Error(err)
					return
				}
				if len(pkt.Payload) != 160 {
					b.Error("payload not parsed")
					return
				}
			}
		})
	})

	b.Run("withPayloadBuf", func(b *testing.B) {
		b.ResetTimer()
		b.ReportAllocs()

		b.RunParallel(func(p *testing.PB) {
			sess := &MediaSession{rtpConn: session.rtpConn}
			buf := make([]byte, RTPBufSize)
			for p.Next() {
				pkt := rtp.Packet{
					Payload: buf,
				}
				_, err := sess.ReadRTP(buf, &pkt)
				if err != nil {
					b.Error(err)
					return
				}
				if len(pkt.Payload) == 0 {
					b.Error("payload not parsed")
					return
				}
			}
		})
	})
}

// stopPipeWriter closes the read side of a pipe whose writer runs until its
// write fails, and waits for the writer, which done reports, to return.
func stopPipeWriter(b *testing.B, reader *io.PipeReader, done <-chan struct{}) {
	b.Helper()
	reader.Close()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		b.Error("the pipe writer did not stop")
	}
}
