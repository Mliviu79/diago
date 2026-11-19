// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"
	"fmt"

	"github.com/emiago/diago/media"
)

// WithAudioReaderWebrtcJitterBuffer puts a jitter buffer between the dialog's
// RTP session and its packet reader, as WithAudioReaderJitterBuffer does on a
// DialogMedia. A dialog has one buffer: a second one is refused. A ClockRate
// left 0 is the negotiated codec's. The buffer reads every transport the
// dialog runs on, so its queue and playout carry over a media update, and
// Close waits for its read loop to stop reading.
//
// A read the application stopped with StopRTP stays stopped. Without a media
// session the option returns ErrNoMediaSetup.
func WithAudioReaderWebrtcJitterBuffer(opts media.RTPJitterBufferOptions) AudioReaderWebrtcOption {
	return func(d *DialogWebrtc) error {
		if d.mediaSession == nil || d.RTPPacketReader == nil {
			return ErrNoMediaSetup
		}
		if d.jitterBuffer != nil {
			return errors.New("jitter buffer already set up")
		}

		codec := d.mediaSession.Codec()
		if codec.SampleDur <= 0 {
			return fmt.Errorf("invalid audio codec packet duration: %s", codec.SampleDur)
		}
		reader := d.RTPPacketReader.Reader()
		if reader == nil {
			return errors.New("no RTP reader setup")
		}
		if opts.ClockRate == 0 {
			opts.ClockRate = codec.SampleRate
		}
		jitter := media.NewRTPJitterBuffer(reader, codec.SampleDur, opts)
		d.RTPPacketReader.UpdateReader(jitter)

		// UpdateReader ends a read waiting on the session with a deadline of
		// its own. The buffer reads that same session, so the deadline the
		// application last asked for is put back: none unless it stopped the
		// reads, and that stop is kept.
		if session, ok := reader.(*media.RTPSessionWebrtc); ok {
			if err := session.Sess.RestoreRTPReadDeadline(); err != nil {
				d.RTPPacketReader.UpdateReader(reader)
				return errors.Join(fmt.Errorf("start the jitter buffer's RTP reader: %w", err), jitter.Close())
			}
		}
		d.jitterBuffer = jitter
		return nil
	}
}

// moveRTPReadsUnsafe moves the dialog's RTP reads onto rtpSess, the RTP
// session of the transport a media update installs, as DialogMedia does. A
// jitter buffer stays in front of the packet reader and reads the new session
// itself; without one the packet reader is moved. Must be called with d.mu
// held.
func (d *DialogWebrtc) moveRTPReadsUnsafe(rtpSess *media.RTPSessionWebrtc) {
	if d.jitterBuffer != nil {
		d.jitterBuffer.UpdateRTPSession(rtpSess)
		return
	}
	d.RTPPacketReader.UpdateReader(rtpSess)
}

// followInPlaceUpdateUnsafe has the jitter buffer take up a peer's offer the
// transport applied in place, which can move the audio to another codec on
// the same RTP session: the buffer paces playout by the codec's packet
// duration. Must be called with d.mu held.
func (d *DialogWebrtc) followInPlaceUpdateUnsafe() {
	if d.jitterBuffer == nil || d.rtpSession == nil {
		return
	}
	d.jitterBuffer.UpdateRTPSession(d.rtpSession)
}
