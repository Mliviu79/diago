// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"bytes"
	"io"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo/fakes"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fakeMediaSessionReader(lport int, rtpReader io.Reader) *MediaSession {
	sess := &MediaSession{
		Codecs: []Codec{CodecAudioAlaw, CodecAudioUlaw},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: lport},
	}

	conn := &fakes.UDPConn{
		Reader: rtpReader,
	}
	sess.rtpConn = conn
	return sess
}

func TestRTPReader(t *testing.T) {
	rtpConn := bytes.NewBuffer([]byte{})
	sess := fakeMediaSessionReader(0, rtpConn)
	rtpSess := NewRTPSession(sess)
	rtpReader := NewRTPPacketReaderSession(rtpSess)

	payload := []byte("12312313")
	N := 10
	buf := make([]byte, 3200)
	for i := 0; i < N; i++ {
		writePkt := rtp.Packet{
			Header: rtp.Header{
				SSRC:           1234,
				Version:        2,
				PayloadType:    8,
				SequenceNumber: uint16(i),
				Timestamp:      160 * uint32(i),
				Marker:         i == 0,
			},
			Payload: payload,
		}
		data, _ := writePkt.Marshal()
		rtpConn.Reset()
		rtpConn.Write(data)
		// conn.Reader = bytes.NewBuffer(data)

		n, err := rtpReader.Read(buf)
		require.NoError(t, err)

		pkt := rtpReader.PacketHeader
		require.Equal(t, writePkt.PayloadType, pkt.PayloadType)
		require.Equal(t, writePkt.SSRC, pkt.SSRC)
		require.Equal(t, i == 0, pkt.Marker)
		require.Equal(t, len(payload), n)
		require.Equal(t, rtpReader.seqReader.ReadExtendedSeq(), uint64(writePkt.SequenceNumber))
	}
}

// closedRTPReader stands for the reader of a replaced session whose socket is
// closed while a read is blocked on it: each ReadRTP reports that it started,
// blocks until closed is closed, and then fails with net.ErrClosed.
type closedRTPReader struct {
	started chan struct{}
	closed  chan struct{}
}

func newClosedRTPReader() *closedRTPReader {
	return &closedRTPReader{started: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (r *closedRTPReader) ReadRTP([]byte, *rtp.Packet) (int, error) {
	r.started <- struct{}{}
	<-r.closed
	return 0, net.ErrClosed
}

func waitReadStarted(t *testing.T, r *closedRTPReader) {
	t.Helper()
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not move to the new reader")
	}
}

// TestRTPPacketReaderFollowsReplacedReaders pins that a Read blocked while its
// reader is replaced continues on the new one, however many replacements
// happen before a packet arrives. Two re-INVITEs that each move the media to a
// new socket close the old one under the read twice; the application must
// read on, not see the stream end.
func TestRTPPacketReaderFollowsReplacedReaders(t *testing.T) {
	first := newClosedRTPReader()
	second := newClosedRTPReader()
	payload := []byte{1, 2, 3, 4}
	last := &sliceRTPReader{packets: []rtp.Packet{{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 1, SSRC: 1234},
		Payload: payload,
	}}}

	r := NewRTPPacketReader(first, CodecAudioUlaw)
	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, RTPBufSize)
	read := make(chan readResult, 1)
	go func() {
		n, err := r.Read(buf)
		read <- readResult{n: n, err: err}
	}()

	waitReadStarted(t, first)
	r.UpdateReader(second)
	close(first.closed)

	waitReadStarted(t, second)
	r.UpdateReader(last)
	close(second.closed)

	select {
	case res := <-read:
		require.NoError(t, res.err, "a replaced reader must not end the stream")
		assert.Equal(t, payload, buf[:res.n])
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not return")
	}
}

// TestRTPPacketReaderUpdateLeavesSharedSocketReadable pins that replacing the
// reader with one on the same socket, while no read is blocked, leaves the
// next read working. A fork of a media session shares its socket, so nothing
// done to the socket for the old reader may reach the new one.
func TestRTPPacketReaderUpdateLeavesSharedSocketReadable(t *testing.T) {
	sess := &MediaSession{
		Codecs: []Codec{CodecAudioUlaw},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
	}
	require.NoError(t, sess.createListeners(&sess.Laddr))
	t.Cleanup(func() { _ = sess.Close() })

	r := NewRTPPacketReader(sess, CodecAudioUlaw)
	r.UpdateReader(sess.Fork())

	peer, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	payload := []byte{1, 2, 3, 4}
	raw, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: 0, SequenceNumber: 1, SSRC: 1234},
		Payload: payload,
	}).Marshal()
	require.NoError(t, err)
	_, err = peer.WriteTo(raw, sess.rtpConn.LocalAddr())
	require.NoError(t, err)

	type readResult struct {
		n   int
		err error
	}
	buf := make([]byte, RTPBufSize)
	read := make(chan readResult, 1)
	go func() {
		n, err := r.Read(buf)
		read <- readResult{n: n, err: err}
	}()
	select {
	case res := <-read:
		require.NoError(t, res.err)
		assert.Equal(t, payload, buf[:res.n])
	case <-time.After(5 * time.Second):
		t.Fatal("the read did not return")
	}
}

func BenchmarkRTPPacketReader(b *testing.B) {
	rtpConn := bytes.NewBuffer([]byte{})
	sess := fakeMediaSessionReader(0, rtpConn)
	rtpSess := NewRTPSession(sess)
	rtpReader := NewRTPPacketReaderSession(rtpSess)

	payload := []byte("12312313")
	buf := make([]byte, 3200)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		writePkt := rtp.Packet{
			Header: rtp.Header{
				SSRC:           1234,
				Version:        2,
				PayloadType:    8,
				SequenceNumber: uint16(i % (1 << 16)),
				Timestamp:      160 * uint32(i),
				Marker:         i == 0,
			},
			Payload: payload,
		}
		data, _ := writePkt.Marshal()
		rtpConn.Write(data)

		_, err := rtpReader.Read(buf)
		require.NoError(b, err)
	}
	b.ReportMetric(float64(b.N)/b.Elapsed().Seconds(), "reads/s")
}
