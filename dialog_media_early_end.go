// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"
	"fmt"

	"github.com/emiago/diago/media"
	"github.com/pion/rtp"
)

// errEarlyMediaEnded is what the packet reader and writer of early media
// return once the early dialog that gave it has ended, see
// mediaEarlyKeyer.endEarlyMedia.
var errEarlyMediaEnded = errors.New("early media ended with its early dialog")

// endedEarlyMedia is what the packet reader and writer of ended early media
// read and write: nothing.
type endedEarlyMedia struct{}

func (endedEarlyMedia) ReadRTP([]byte, *rtp.Packet) (int, error) { return 0, errEarlyMediaEnded }

func (endedEarlyMedia) WriteRTP(*rtp.Packet) error { return errEarlyMediaEnded }

// retireEarlyIO has reader and writer, those of early media, refuse from now
// on, and ends a read reader has waiting on the session. codec is the codec
// writer paces. Either may be nil.
func retireEarlyIO(reader *media.RTPPacketReader, writer *media.RTPPacketWriter, codec media.Codec) error {
	if reader != nil {
		reader.UpdateReader(endedEarlyMedia{})
	}
	if writer == nil {
		return nil
	}
	if err := writer.UpdateWriter(endedEarlyMedia{}, codec); err != nil {
		return fmt.Errorf("retire the early media writer: %w", err)
	}
	return nil
}

// endEarlyMedia stops the early media of an early dialog a non-2xx final
// response ended, as mediaEarlyKeyer describes. The RTP session of keyed
// early media is stopped and its monitor joined, a handshake onFinalizeWithin
// left running is ended, and a jitter buffer in front of the reader is closed
// and its read loop joined. All of it waits with the lock released, and
// mediaUpdating keeps other exchanges out meanwhile.
//
// A fork of the session in the state of our offer, on the same sockets, takes
// its place (media.MediaSession.ForkOffer): it carries the DTLS association
// keyed early media had, which an answer with the same tls-id continues, and
// nothing of a handshake that was ended.
func (d *DialogMedia) endEarlyMedia() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	if d.mediaUpdating {
		d.mu.Unlock()
		return errMediaUpdateInProgress
	}
	old := d.mediaSession
	if old == nil || (old.Raddr.IP == nil && d.rtpSession == nil && !d.earlyKeysOwed) {
		// No answer was taken.
		d.mu.Unlock()
		return nil
	}
	oldRTP := d.rtpSession
	owed := d.earlyKeysOwed
	jitter := d.jitterBuffer
	retireErr := retireEarlyIO(d.RTPPacketReader, d.RTPPacketWriter, media.CodecAudioFromSession(old))
	if jitter != nil {
		// Closed first, so the read the move below ends is its last.
		retireErr = errors.Join(retireErr, jitter.Close())
		if oldRTP != nil {
			jitter.UpdateRTPSession(oldRTP)
		}
	}
	d.rtpSession = nil
	d.unstartedRTPSession = nil
	d.RTPPacketReader = nil
	d.RTPPacketWriter = nil
	d.jitterBuffer = nil
	d.earlyKeysOwed = false
	d.mediaUpdating = true
	d.mu.Unlock()

	var monitorErr, abandonErr error
	if oldRTP != nil {
		monitorErr = oldRTP.MonitorClose()
	}
	if owed {
		abandonErr = old.AbandonNegotiation()
	}
	jitterErr := awaitJitterBufferStopped(jitter)

	d.mu.Lock()
	defer d.mu.Unlock()
	d.mediaUpdating = false
	err := errors.Join(retireErr, monitorErr, abandonErr, jitterErr)
	if d.closed {
		// Close has closed the sockets the fork would run on.
		return err
	}
	fork := old.ForkOffer()
	if carryErr := fork.CarryRTPDeadlines(old); carryErr != nil {
		return errors.Join(err, fmt.Errorf("carrying RTP deadlines to the offer's media session: %w", carryErr))
	}
	d.mediaSession = fork
	// The read the retired reader had waiting was ended with a deadline on
	// the socket the fork shares: the fork reads under the one the
	// application last asked for.
	return errors.Join(err, fork.RestoreRTPReadDeadline())
}
