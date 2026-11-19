// SPDX-License-Identifier: MPL-2.0

package media

import "time"

// realTimeMinAllowance is the least lateness RTPRealTimeReader allows a frame
// waiting to be read. A frame may wait up to a frame duration to be handed on,
// as a mix round takes one frame per stream, and the rest absorbs scheduling
// and network jitter, which is no smaller for short frames.
const realTimeMinAllowance = 40 * time.Millisecond

// realTimeAnchor is the frame an RTPRealTimeReader judges lateness against:
// its RTP timestamp, its SSRC and the time it was read. It is not safe for
// concurrent use, as the reader it belongs to is not.
type realTimeAnchor struct {
	anchored   bool
	anchorTime time.Time
	anchorTS   uint32
	anchorSSRC uint32

	// now is the reader's clock, time.Now outside tests.
	now func() time.Time
}

// anchor makes the frame with timestamp ts of source ssrc, read at now, the
// frame later ones are judged against.
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
