// SPDX-License-Identifier: MPL-2.0

package media

import "time"

// clockStart is clockReset that also starts a disabled clock.
func (w *RTPPacketWriter) clockStart() {
	w.clockReset()
	if w.clockTicker == nil {
		w.clockTicker = time.NewTicker(w.codec.SampleDur)
		w.clockStopped = make(chan struct{})
	}
}

// awaitSampleTime lets the duration of the frame just written pass and records
// when it ended, which ResetTimestamp measures a pause in the stream from.
// ticker and stopped are the clock as Write read it under the lock: a nil
// ticker is a disabled clock, on which nothing is waited for, and stopped
// closes when ClockDisable stops that ticker, which never ticks again. The
// time is recorded under the lock ResetTimestamp reads it under.
func (p *RTPPacketWriter) awaitSampleTime(ticker *time.Ticker, stopped <-chan struct{}) {
	sampleTime := time.Now()
	if ticker != nil {
		select {
		case sampleTime = <-ticker.C:
		case <-stopped:
			sampleTime = time.Now()
		}
	}
	p.mu.Lock()
	p.lastSampleTime = sampleTime
	p.mu.Unlock()
}
