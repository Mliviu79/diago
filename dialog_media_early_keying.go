// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
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
