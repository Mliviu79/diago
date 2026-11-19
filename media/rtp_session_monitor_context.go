// SPDX-License-Identifier: MPL-2.0

package media

import "context"

// MonitorBackgroundContext is MonitorBackground, with its errors, whose
// monitors also end with ctx: when ctx ends they stop as MonitorClose stops
// them, which leaves the RTP and RTCP connections open and the session closed
// to a new monitor. A ctx that has ended already stops them as soon as they
// are started.
//
// The goroutine that watches ctx is one of the monitor's: MonitorClose ends it
// when ctx has not ended, and waits for it beside the reader and the writer.
func (s *RTPSession) MonitorBackgroundContext(ctx context.Context) error {
	if err := s.MonitorBackground(); err != nil {
		return err
	}

	s.rtcpMU.Lock()
	defer s.rtcpMU.Unlock()
	if s.closed {
		// Stopped since they were started, so ctx has nothing left to stop.
		return nil
	}
	// MonitorClose waits only after it has marked the session closed under
	// this lock, so a goroutine counted here, before that mark, is one it
	// waits for.
	s.monitorWG.Go(func() {
		select {
		case <-ctx.Done():
			// The stop fails only where the RTCP connection takes no
			// deadline any more. MonitorClose and Close set that deadline
			// again, and return that error to whoever joins the monitors.
			s.close(false)
		case <-s.rtcpClosed:
		}
	})
	return nil
}
