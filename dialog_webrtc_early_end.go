// SPDX-License-Identifier: MPL-2.0

package diago

import "errors"

// endEarlyMedia stops the early media of an early dialog a non-2xx final
// response ended, as mediaEarlyKeyer describes.
//
// Keyed early media is taken down to the transport: its RTP session is
// stopped and its monitor joined, a jitter buffer in front of its reader is
// closed and its read loop joined, and the transport goes back to being the
// one of the media's first exchange, which the answer to our offer sent again
// is applied to and keyed through, as the first one was. The transport keeps
// its ICE agent and DTLS association: the offer sent again carries the same
// ICE credentials and fingerprint, and an answer with the same ones continues
// them (media.MediaSessionWebrtc.AbandonNegotiation).
//
// A negotiation onFinalizeWithin left running is ended, and the transport,
// which carried no media, stays the first exchange's.
func (d *DialogWebrtc) endEarlyMedia() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	if d.mediaSession == nil {
		pending := d.pendingMediaSession
		d.mu.Unlock()
		if pending == nil {
			// No answer was taken.
			return nil
		}
		return pending.AbandonNegotiation()
	}
	sess := d.mediaSession
	rtpSess := d.rtpSession
	jitter := d.jitterBuffer
	err := retireEarlyIO(d.RTPPacketReader, d.RTPPacketWriter, sess.Codec())
	if jitter != nil {
		// Closed first, so the read the move below ends is its last.
		err = errors.Join(err, jitter.Close())
		if rtpSess != nil {
			jitter.UpdateRTPSession(rtpSess)
		}
	}
	d.mediaSession = nil
	d.rtpSession = nil
	d.RTPPacketReader = nil
	d.RTPPacketWriter = nil
	d.jitterBuffer = nil
	// Close closes it as the transport of an exchange in progress.
	d.pendingMediaSession = sess
	d.mu.Unlock()

	if rtpSess != nil {
		err = errors.Join(err, rtpSess.MonitorClose())
	}
	err = errors.Join(err, awaitJitterBufferStopped(jitter))
	// Under the lock Close marks the media closed with, before it closes the
	// transport.
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return err
	}
	// The read the retired reader had waiting was ended with a deadline on
	// the transport: the reads of the next answer go on under the one the
	// application last asked for.
	return errors.Join(err, sess.RestoreRTPReadDeadline())
}
