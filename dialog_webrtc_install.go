// SPDX-License-Identifier: MPL-2.0

package diago

import "github.com/emiago/diago/media"

// initMonitored sets sess up as the media of the call, with rtpSess, its
// reader and its writer, as Init does, and starts the RTCP monitor of rtpSess
// with monitor. Closed media sets up and starts nothing and reports
// errMediaClosed: sess and rtpSess stay the caller's to close.
//
// Both run under the lock Close takes, so a Close either finds the transport
// and its running monitor and stops them, or has run before and leaves
// nothing to start; a Close made before the transport was set up found
// nothing of it to close. monitor must not wait for the monitor it starts:
// MonitorBackground and MonitorBackgroundContext only start its goroutines.
func (d *DialogWebrtc) initMonitored(sess *media.MediaSessionWebrtc, rtpSess *media.RTPSessionWebrtc, monitor func() error) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return errMediaClosed
	}
	d.installUnsafe(sess, rtpSess)
	return monitor()
}

// installUnsafe makes sess and rtpSess the media of the call, with a packet
// reader and writer on rtpSess for the codec of sess. Must be called with
// d.mu held.
func (d *DialogWebrtc) installUnsafe(sess *media.MediaSessionWebrtc, rtpSess *media.RTPSessionWebrtc) {
	codec := sess.Codec()
	d.mediaSession = sess
	d.rtpSession = rtpSess
	d.RTPPacketReader = media.NewRTPPacketReader(rtpSess, codec)
	d.RTPPacketWriter = media.NewRTPPacketWriter(rtpSess, codec)
}
