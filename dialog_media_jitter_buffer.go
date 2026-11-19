// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"
	"time"

	"github.com/emiago/diago/media"
)

// jitterBufferStopTimeout bounds how long Close waits for a jitter buffer's
// read loop once the sockets it reads are closed. Closing them fails its read
// at once, so the bound only matters for a read Close did not end.
const jitterBufferStopTimeout = 5 * time.Second

// moveRTPReadsUnsafe moves the dialog's RTP reads onto rtpSess, the RTP
// session a media update installs. A jitter buffer stays in front of the
// packet reader and reads the new session itself, so its queue and playout
// carry over and no read loop is left on the session replaced. Without one
// the packet reader is moved. Must be called with d.mu held.
func (d *DialogMedia) moveRTPReadsUnsafe(rtpSess *media.RTPSession) {
	if d.jitterBuffer != nil {
		d.jitterBuffer.UpdateRTPSession(rtpSess)
		return
	}
	d.RTPPacketReader.UpdateRTPSession(rtpSess)
}

// awaitJitterBufferStopped waits for the read loop of jitter, the dialog's
// jitter buffer or nil without one, to stop reading the dialog's session, for
// jitterBufferStopTimeout at most. Close calls it once the buffer and the
// sockets are closed, which fails the read the loop sits in, and before the
// RTP port is released: the loop no longer reads the session once Done is
// closed. An expired bound is an error, since the loop may still be reading.
func awaitJitterBufferStopped(jitter *media.RTPJitterBuffer) error {
	if jitter == nil {
		return nil
	}
	select {
	case <-jitter.Done():
		return nil
	case <-time.After(jitterBufferStopTimeout):
		return errors.New("jitter buffer read loop did not stop")
	}
}
