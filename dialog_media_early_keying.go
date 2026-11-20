// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emiago/diago/media"
)

// errMediaKeyingInProgress is how the media reports that the peer has not
// completed, within the wait it was given, the negotiation that keys it: the
// handshake of DTLS-SRTP early media. The negotiation is still running, and
// onFinalize waits for it. See mediaEarlyKeyer.
var errMediaKeyingInProgress = errors.New("dialog media keying in progress")

var _ mediaEarlyKeyer = (*DialogMedia)(nil)

// onFinalizeWithin keys the media session in place within wait, or leaves its
// handshake running, as mediaEarlyKeyer describes.
//
// Media whose handshake is left running has no RTP session, packet reader or
// packet writer: what was set up for it is taken away, so the dialog has no
// audio to read or write, and the media session refuses to send with
// media.ErrDTLSNotKeyed. onFinalize sets them up once it has waited for the
// keys.
func (d *DialogMedia) onFinalizeWithin(ctx context.Context, wait time.Duration) error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return errMediaClosed
	}
	ms := d.mediaSession
	d.mu.Unlock()
	if ms == nil {
		return errNoMediaSession
	}

	err := ms.FinalizeWithin(ctx, wait)
	if !errors.Is(err, media.ErrFinalizeInProgress) {
		return err
	}

	d.mu.Lock()
	d.earlyKeysOwed = true
	d.rtpSession = nil
	d.unstartedRTPSession = nil
	d.RTPPacketReader = nil
	d.RTPPacketWriter = nil
	d.mu.Unlock()
	return errMediaKeyingInProgress
}

// setUpEarlyKeyed gives the media session its RTP session, reader and writer
// once the handshake onFinalizeWithin left running has keyed it, and leaves
// the RTCP monitor of that RTP session to startFirstExchange. It does nothing
// on media that left no handshake running, and nothing where whoever waited
// for the keys has set the RTP session up itself.
func (d *DialogMedia) setUpEarlyKeyed() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if !d.earlyKeysOwed {
		return
	}
	d.earlyKeysOwed = false
	if d.closed || d.rtpSession != nil || d.mediaSession == nil {
		return
	}
	d.stageFirstRTPSessionUnsafe()
}

// setOnMediaUpdate has f told of each later update of the media, in place of
// whoever was told before. A nil f tells nobody.
func (d *DialogMedia) setOnMediaUpdate(f func(*DialogMedia)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onMediaUpdate = f
}

// replaceEarlyAnswerUnsafe applies remoteSDP, an answer to our offer, in place
// of the answer whose handshake onFinalizeWithin left running: the answer of
// another early dialog of the offer, whose 2xx confirms it (RFC 3261 sections
// 12.1 and 13.2.1). That handshake is ended first, as nothing else may use
// the media session while it runs, and a fork of the session, on the sockets
// the offer named, takes remoteSDP. The fork is put in the session's place
// once the handshake has returned, so nothing of it runs beside the fork:
// nothing carries media meanwhile, and onFinalize keys the fork and sets it
// up, as it does the session it replaces. A fork that refuses remoteSDP is
// dropped and leaves the session with no handshake to complete.
//
// Must be called with d.mu held. The lock is released while the handshake is
// ended and joined, which waits for its goroutine, and mediaUpdating keeps
// other exchanges out meanwhile. Media closed meanwhile is left as Close left
// it, with errMediaClosed.
func (d *DialogMedia) replaceEarlyAnswerUnsafe(remoteSDP []byte) error {
	replaced := d.mediaSession
	d.mediaUpdating = true
	d.mu.Unlock()
	abandonErr := replaced.AbandonNegotiation()
	d.mu.Lock()
	d.mediaUpdating = false

	if d.closed {
		// Close has closed the sockets the fork would run on.
		return errors.Join(errMediaClosed, abandonErr)
	}
	if abandonErr != nil {
		return abandonErr
	}
	msess := replaced.Fork()
	if err := msess.RemoteSDP(remoteSDP, true); err != nil {
		return err
	}
	if err := msess.CarryRTPDeadlines(replaced); err != nil {
		return fmt.Errorf("carrying RTP deadlines to the new media session: %w", err)
	}
	d.mediaSession = msess
	return nil
}
