// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"

	"github.com/emiago/diago/media"
)

// newDialogMedia returns the media of a call that has taken part in no
// offer/answer exchange yet: a media session bound as conf says and nothing
// else. Whoever answers or places the call drives its first exchange through
// the handshaker, as it drives every later one. onMediaUpdate, when not nil,
// is told of each later update of the media.
//
// ctx is the context of the call the media is for, on which its RTP port is
// chosen when conf has an allocator. It bounds nothing else: the media runs
// until it is closed.
//
// The caller closes the media, or registers it with a session that does.
func newDialogMedia(ctx context.Context, conf MediaConfig, onMediaUpdate func(*DialogMedia)) (*DialogMedia, error) {
	med := &DialogMedia{onMediaUpdate: onMediaUpdate}
	if err := med.initMediaSessionFromConf(ctx, conf); err != nil {
		return nil, err
	}
	return med, nil
}

// awaitsFirstExchangeUnsafe reports whether the media has taken part in no
// offer/answer exchange yet: it has a media session with no remote
// description, no RTP session, and no offer of its own pending. The first
// offer, the peer's or ours, belongs to that media session and not to a fork
// of it: there is no running media a fork would keep carrying meanwhile, and
// the answer to an offer of ours is applied to the session that made it (see
// awaitsFirstAnswerUnsafe). Must be called with d.mu held.
func (d *DialogMedia) awaitsFirstExchangeUnsafe() bool {
	return d.rtpSession == nil && d.pendingMediaSession == nil && d.mediaSession != nil && d.mediaSession.Raddr.IP == nil
}

// stageFirstOfferUnsafe applies offer, the peer's first, to the media session
// and sets up the RTP session, its reader and its writer for it. The RTCP
// monitor is left to onFinalize: nothing is sent to the peer before the
// exchange is complete. Must be called with d.mu held.
func (d *DialogMedia) stageFirstOfferUnsafe(offer []byte) error {
	if err := d.mediaSession.RemoteSDP(offer, false); err != nil {
		return err
	}
	d.stageFirstRTPSessionUnsafe()
	return nil
}

// firstOfferUnsafe returns the call's first offer when we make it, with the
// direction mode when that is not empty, and sets up the RTP session, its
// reader and its writer for it. The answer is applied to the same media
// session, and the RTCP monitor is left to onFinalize, which runs once that
// answer is in. Must be called with d.mu held.
func (d *DialogMedia) firstOfferUnsafe(mode string) []byte {
	if mode != "" {
		d.mediaSession.Mode = mode
	}
	offer := d.mediaSession.LocalSDP(false)
	d.stageFirstRTPSessionUnsafe()
	return offer
}

// stageFirstRTPSessionUnsafe gives the media session its RTP session, reader
// and writer, and records that the RTCP monitor of that RTP session has yet to
// be started. Must be called with d.mu held.
func (d *DialogMedia) stageFirstRTPSessionUnsafe() {
	rtpSess := media.NewRTPSession(d.mediaSession)
	d.initRTPSessionUnsafe(d.mediaSession, rtpSess)
	d.unstartedRTPSession = rtpSess
}

// startFirstExchange starts the RTCP monitor of the RTP session a first
// exchange made through the handshaker set up, once: a later call, and a call
// on media set up any other way, starts nothing. It needs the remote
// description, so it runs once both the offer and the answer are in.
//
// Closed media starts nothing and reports errMediaClosed. The monitor is
// started under the lock Close takes, so a Close either finds it running and
// stops it, or has run before and leaves nothing to start: MonitorBackground
// only starts the monitor's goroutines and does not wait for them.
func (d *DialogMedia) startFirstExchange() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	rtpSess := d.unstartedRTPSession
	d.unstartedRTPSession = nil
	if rtpSess == nil {
		return nil
	}
	if d.closed {
		return errMediaClosed
	}
	return rtpSess.MonitorBackground()
}
