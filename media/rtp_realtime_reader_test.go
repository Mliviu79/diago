package media

import (
	"io"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// realtimeFrame is one frame a realtimeFeed hands out: its RTP timestamp and
// SSRC, and how long the read waits for it to arrive.
type realtimeFrame struct {
	ts   uint32
	ssrc uint32
	wait time.Duration
}

// realtimeFeed is an RTP reader on a fake clock. Reading a frame moves the
// clock on by the frame's wait, as a read blocked until the frame arrived.
type realtimeFeed struct {
	clock  time.Time
	frames []realtimeFrame
	seq    uint16
}

func (f *realtimeFeed) now() time.Time { return f.clock }

func (f *realtimeFeed) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	if len(f.frames) == 0 {
		return 0, io.EOF
	}
	fr := f.frames[0]
	f.frames = f.frames[1:]
	f.clock = f.clock.Add(fr.wait)
	f.seq++
	raw, err := (&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: CodecAudioUlaw.PayloadType, SequenceNumber: f.seq, Timestamp: fr.ts, SSRC: fr.ssrc},
		Payload: make([]byte, 160),
	}).Marshal()
	if err != nil {
		return 0, err
	}
	n := copy(buf, raw)
	return n, p.Unmarshal(buf[:n])
}

// newRealtimeFeedReader returns an RTPRealTimeReader over feed, on its clock.
func newRealtimeFeedReader(feed *realtimeFeed) (*RTPRealTimeReader, *RTPPacketReader) {
	feed.clock = time.Unix(1000, 0)
	pr := NewRTPPacketReader(feed, CodecAudioUlaw)
	r := NewRTPRealTimeReader(pr, pr, CodecAudioUlaw)
	r.now = feed.now
	return r, pr
}

// readAllTimestamps reads r until the feed ends and returns the timestamps of
// the frames it delivered.
func readAllTimestamps(t *testing.T, r *RTPRealTimeReader, pr *RTPPacketReader) []uint32 {
	t.Helper()
	var got []uint32
	buf := make([]byte, RTPBufSize)
	for {
		_, err := r.Read(buf)
		if err == io.EOF {
			return got
		}
		require.NoError(t, err)
		got = append(got, pr.PacketHeader.Timestamp)
	}
}

// TestRTPRealTimeReaderFollowsSenderClock is a peer whose sample clock runs 50
// ppm slower than ours, for 16 minutes: every frame arrives on its own
// schedule, and the reader waits for each. Judged against the first frame
// alone, with nothing to re-anchor on, each frame looked 50 us per second
// later than the one before; past two frame durations, after about 13
// minutes, every frame was dropped as late.
func TestRTPRealTimeReaderFollowsSenderClock(t *testing.T) {
	const frames = 16 * 60 * 50
	feed := &realtimeFeed{}
	// 20 ms of the peer's samples take 20.001 ms of ours.
	perFrame := 20*time.Millisecond + time.Microsecond
	for i := range frames {
		feed.frames = append(feed.frames, realtimeFrame{ts: uint32(i) * 160, ssrc: 1, wait: perFrame})
	}
	r, pr := newRealtimeFeedReader(feed)
	got := readAllTimestamps(t, r, pr)
	require.Len(t, got, frames, "frames were dropped as late")
}

// TestRTPRealTimeReaderResumesAfterPause is a peer that stops sending for two
// seconds and resumes its RTP timestamps where it left off, as some do across
// hold. Every frame after the pause arrived two seconds after its place in the
// stream the first frame set, and was dropped as late, for the rest of the
// call.
func TestRTPRealTimeReaderResumesAfterPause(t *testing.T) {
	feed := &realtimeFeed{}
	for i := range 100 {
		wait := 20 * time.Millisecond
		if i == 50 {
			wait = 2 * time.Second
		}
		feed.frames = append(feed.frames, realtimeFrame{ts: uint32(i) * 160, ssrc: 1, wait: wait})
	}
	r, pr := newRealtimeFeedReader(feed)
	got := readAllTimestamps(t, r, pr)
	require.Len(t, got, 100, "frames after the pause were dropped as late")
}

// TestRTPRealTimeReaderDropsBacklog pins what the reader is for: a consumer
// that stalled finds frames waiting, read without a wait, and the ones later
// than the reader allows are dropped so it catches up with the stream. It
// allows two frame durations, and at least 40 ms.
func TestRTPRealTimeReaderDropsBacklog(t *testing.T) {
	feed := &realtimeFeed{}
	for i := range 10 {
		feed.frames = append(feed.frames, realtimeFrame{ts: uint32(i) * 160, ssrc: 1, wait: 20 * time.Millisecond})
	}
	// Frames 10 to 14 arrive while the consumer is stalled, and wait for it.
	for i := 10; i < 15; i++ {
		feed.frames = append(feed.frames, realtimeFrame{ts: uint32(i) * 160, ssrc: 1})
	}
	for i := 15; i < 20; i++ {
		feed.frames = append(feed.frames, realtimeFrame{ts: uint32(i) * 160, ssrc: 1, wait: 20 * time.Millisecond})
	}
	r, pr := newRealtimeFeedReader(feed)

	buf := make([]byte, RTPBufSize)
	var got []uint32
	for range 10 {
		_, err := r.Read(buf)
		require.NoError(t, err)
		got = append(got, pr.PacketHeader.Timestamp)
	}
	// The consumer stalls for 100 ms: frame 10 is due 20 ms after frame 9 was
	// read, so it is read 80 ms late, frame 11 60 ms and frame 12 40 ms.
	feed.clock = feed.clock.Add(100 * time.Millisecond)
	got = append(got, readAllTimestamps(t, r, pr)...)

	var want []uint32
	for i := range 20 {
		if i == 10 || i == 11 {
			continue
		}
		want = append(want, uint32(i)*160)
	}
	require.Equal(t, want, got)
}

// TestRTPRealTimeReaderAllowance pins the lateness a frame waiting to be read
// may have: two frame durations, and at least 40 ms. A frame waits up to a
// frame duration to be handed over in a mix round, and the rest absorbs
// scheduling and network jitter, which is no smaller for short frames. At 10
// ms frames two frame durations left 10 ms for it, and a stall of 20 ms
// dropped a frame.
func TestRTPRealTimeReaderAllowance(t *testing.T) {
	for _, tc := range []struct {
		frameDur time.Duration
		stall    time.Duration
		dropped  bool
	}{
		{10 * time.Millisecond, 40 * time.Millisecond, false},
		{10 * time.Millisecond, 45 * time.Millisecond, true},
		{20 * time.Millisecond, 40 * time.Millisecond, false},
		{20 * time.Millisecond, 45 * time.Millisecond, true},
		{30 * time.Millisecond, 60 * time.Millisecond, false},
		{30 * time.Millisecond, 65 * time.Millisecond, true},
	} {
		t.Run(tc.frameDur.String()+"/"+tc.stall.String(), func(t *testing.T) {
			codec := CodecAudioUlaw
			codec.SampleDur = tc.frameDur
			step := codec.SampleTimestamp()
			feed := &realtimeFeed{clock: time.Unix(1000, 0), frames: []realtimeFrame{
				{ts: 0, ssrc: 1},
				// Due one frame duration after the first, read stall later
				// than that, without a wait.
				{ts: step, ssrc: 1},
				{ts: 2 * step, ssrc: 1, wait: tc.frameDur},
			}}
			pr := NewRTPPacketReader(feed, codec)
			r := NewRTPRealTimeReader(pr, pr, codec)
			r.now = feed.now

			buf := make([]byte, RTPBufSize)
			_, err := r.Read(buf)
			require.NoError(t, err)
			feed.clock = feed.clock.Add(tc.frameDur + tc.stall)
			_, err = r.Read(buf)
			require.NoError(t, err)
			if tc.dropped {
				require.Equal(t, 2*step, pr.PacketHeader.Timestamp, "the late frame was delivered")
			} else {
				require.Equal(t, step, pr.PacketHeader.Timestamp, "the frame was dropped as late")
			}
		})
	}
}

// TestRTPRealTimeReaderNewSource pins that a frame of another SSRC, whose
// timestamps have no relation to the stream read so far, is not judged
// against it.
func TestRTPRealTimeReaderNewSource(t *testing.T) {
	feed := &realtimeFeed{}
	for i := range 10 {
		feed.frames = append(feed.frames, realtimeFrame{ts: 1_000_000 + uint32(i)*160, ssrc: 1, wait: 20 * time.Millisecond})
	}
	// The new source starts with timestamps far behind, and its first frames
	// wait for the reader.
	for i := range 3 {
		feed.frames = append(feed.frames, realtimeFrame{ts: uint32(i) * 160, ssrc: 2})
	}
	r, pr := newRealtimeFeedReader(feed)
	got := readAllTimestamps(t, r, pr)
	require.Len(t, got, 13)
}
