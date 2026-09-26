package media

import (
	"fmt"
	"io"
	"time"
)

// realTimeMinAllowance is the least lateness RTPRealTimeReader allows a frame
// waiting to be read. A frame may wait up to a frame duration to be handed on,
// as a mix round takes one frame per stream, and the rest absorbs scheduling
// and network jitter, which is no smaller for short frames.
const realTimeMinAllowance = 40 * time.Millisecond

// RTPRealTimeReader drops frames read later than real time allows, so a
// consumer that fell behind the stream, and found frames waiting for it,
// catches up instead of staying behind.
//
// It judges each frame against an anchor: a frame of the stream and the time
// it was read, from which every later frame's due time follows by its RTP
// timestamp. A frame read more than two frame durations, and at least 40 ms,
// after it was due is dropped when it was already waiting to be read. A frame
// the read waited for has just arrived, so the reader is not behind: the
// sender's sample clock drifted from ours, or it paused and resumed its
// timestamps where they left off. Such a frame becomes the anchor instead. So
// does a frame read no later than it was due, which keeps the anchor at the
// earliest the stream has arrived, and the first frame of a new SSRC.
type RTPRealTimeReader struct {
	r         io.Reader
	rtpReader *RTPPacketReader
	codec     Codec

	anchored   bool
	anchorTime time.Time
	anchorTS   uint32
	anchorSSRC uint32

	// now is the reader's clock, time.Now outside tests.
	now func() time.Time
}

func NewRTPRealTimeReader(reader io.Reader, rtpReader *RTPPacketReader, codec Codec) *RTPRealTimeReader {
	r := &RTPRealTimeReader{}
	r.Init(reader, rtpReader, codec)
	return r
}

func (r *RTPRealTimeReader) Init(reader io.Reader, rtpReader *RTPPacketReader, codec Codec) {
	if codec.SampleDur == 0 {
		panic("Codec sample dur not defined " + fmt.Sprintf("%+v", codec))
	}

	r.r = reader
	r.rtpReader = rtpReader
	r.codec = codec
	r.now = time.Now
}

// Reset forgets the anchor, so the next frame read becomes it.
func (r *RTPRealTimeReader) Reset() {
	r.anchored = false
}

func (r *RTPRealTimeReader) Read(b []byte) (int, error) {
	for {
		start := r.now()
		n, err := r.r.Read(b)
		if err != nil {
			return n, err
		}
		now := r.now()

		rtpHeader := r.rtpReader.PacketHeader
		if !r.anchored || rtpHeader.SSRC != r.anchorSSRC {
			r.anchor(now, rtpHeader.Timestamp, rtpHeader.SSRC)
			return n, nil
		}

		late := now.Sub(r.anchorTime) - r.sinceAnchor(rtpHeader.Timestamp)
		switch {
		case late <= 0:
			r.anchor(now, rtpHeader.Timestamp, rtpHeader.SSRC)
		case late <= r.allowance():
		case now.Sub(start) >= r.codec.SampleDur/2:
			// The read waited for the frame, so it was not waiting to be read
			r.anchor(now, rtpHeader.Timestamp, rtpHeader.SSRC)
		default:
			DefaultLogger().Debug("Skipping non realtime packet", "ssrc", rtpHeader.SSRC, "seq", rtpHeader.SequenceNumber, "ts", rtpHeader.Timestamp, "late", late)
			continue
		}
		return n, nil
	}
}

func (r *RTPRealTimeReader) anchor(now time.Time, ts uint32, ssrc uint32) {
	r.anchored = true
	r.anchorTime = now
	r.anchorTS = ts
	r.anchorSSRC = ssrc
}

// sinceAnchor returns how much later in the stream than the anchor a frame
// with timestamp ts is, negative for an earlier frame.
func (r *RTPRealTimeReader) sinceAnchor(ts uint32) time.Duration {
	samples := int64(int32(ts - r.anchorTS))
	return time.Duration(samples) * time.Second / time.Duration(r.codec.SampleRate)
}

// allowance returns how late a frame waiting to be read may be: two frame
// durations, and at least realTimeMinAllowance.
func (r *RTPRealTimeReader) allowance() time.Duration {
	return max(2*r.codec.SampleDur, realTimeMinAllowance)
}
