// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"

	"github.com/emiago/diago/media"
)

// errMediaUpdateInProgress is how the media refuses an offer/answer exchange
// while an earlier one still waits for the peer to key it: the handshake of a
// new DTLS association runs with the media's lock released, and a second
// exchange beside it would fork the session that handshake is about to
// replace. A re-INVITE of the peer's that meets it is answered 491, see
// refusePeerReInvite.
var errMediaUpdateInProgress = errors.New("dialog media update in progress")

// errMediaKeyingFailed is how the media reports that an update which needed
// keys of its own, a new DTLS association, was not completed: its handshake
// failed or was cut short, or the session it keyed could not be put in place.
// The fork is discarded and the media session in place is left as it was. The
// offer/answer exchange was complete by then, so the peer has moved to a
// session this side cannot use.
var errMediaKeyingFailed = errors.New("dialog media update could not be keyed")

// updateFromForkUnsafe applies remoteSDP to a fork of the media session in
// place, offered saying whether it answers an offer of ours, and reports
// whether the fork was put in that session's place.
//
// A fork that continues what the session runs on is installed at once. One
// that RemoteSDP found to need a negotiation of its own, the handshake of a
// new DTLS association, has no SRTP keys until that has run, so it is not
// installed before: the session in place carries the media meanwhile. For an
// answer the exchange is complete, and installKeyedUnsafe runs the handshake
// here, within ctx. For an offer of the peer's it cannot run yet: it needs
// our answer, which tells the peer where the fork is and which certificate it
// presents (RFC 8842 section 5.3). The fork is staged instead, onLocalSDP
// answers from it, and onFinalize keys and installs it once that answer is
// out.
//
// Must be called with d.mu held, which is released while a handshake runs.
func (d *DialogMedia) updateFromForkUnsafe(ctx context.Context, remoteSDP []byte, offered bool) (installed bool, err error) {
	msess := d.mediaSession.Fork()
	if err := msess.RemoteSDP(remoteSDP, offered); err != nil {
		return false, fmt.Errorf("sdp update media remote SDP applying failed: %w", err)
	}
	if !offered && msess.FinalizePending() {
		d.stageMediaSessionUnsafe(msess)
		return false, nil
	}
	if err := d.installKeyedUnsafe(ctx, msess); err != nil {
		return false, err
	}
	return true, nil
}

// stageMediaSessionUnsafe keeps msess, the fork that answers an offer of the
// peer's with a new DTLS association, until onFinalize commits it or the
// exchange is aborted. A fork an earlier exchange left staged is discarded:
// the offer just applied replaces that exchange. Must be called with d.mu
// held.
func (d *DialogMedia) stageMediaSessionUnsafe(msess *media.MediaSession) {
	if prev := d.stagedMediaSession; prev != nil {
		d.discardErr = errors.Join(d.discardErr, d.discardForkUnsafe(prev))
	}
	d.stagedMediaSession = msess
}

// answeringSessionUnsafe returns the media session that answers the offer
// last applied: the fork that offer left staged, or the session in place,
// which a fork that needed no keys of its own has become by now. Must be
// called with d.mu held.
func (d *DialogMedia) answeringSessionUnsafe() *media.MediaSession {
	if d.stagedMediaSession != nil {
		return d.stagedMediaSession
	}
	return d.mediaSession
}

// commitStagedMediaSession keys and installs the fork an offer of the peer's
// left staged, now that our answer to it is out, and reports whether there
// was one. The handshake runs within ctx, see installKeyedUnsafe for its
// errors. The application is told of the update once the fork is in place.
func (d *DialogMedia) commitStagedMediaSession(ctx context.Context) (staged bool, err error) {
	d.mu.Lock()
	msess := d.stagedMediaSession
	if msess == nil {
		d.mu.Unlock()
		return false, nil
	}
	// Taken before the handshake, which runs with the lock released, so an
	// abort or a Close made meanwhile leaves the fork to this call.
	d.stagedMediaSession = nil
	err = d.installKeyedUnsafe(ctx, msess)
	onMediaUpdate := d.onMediaUpdate
	d.mu.Unlock()
	if err != nil {
		return true, err
	}
	if onMediaUpdate != nil {
		onMediaUpdate(d)
	}
	return true, nil
}

// installKeyedUnsafe puts msess, a fork a session description was applied to,
// in place of the media session, after it has run the negotiation msess has
// pending, if any: the handshake of a new DTLS association. msess carries no
// media before it is keyed, so audio never runs on a session without keys.
//
// d.mu is released while the handshake waits for the peer, within ctx and
// media.DTLSHandshakeTimeout, and mediaUpdating keeps other exchanges out
// meanwhile. A fork that is not installed is discarded: its handshake failed,
// ctx ended, or the media was closed meanwhile. The error then wraps
// errMediaKeyingFailed, or is errMediaClosed for media closed meanwhile,
// whatever became of the handshake.
//
// Must be called with d.mu held.
func (d *DialogMedia) installKeyedUnsafe(ctx context.Context, msess *media.MediaSession) error {
	if !msess.FinalizePending() {
		if err := d.replaceMediaSessionUnsafe(msess); err != nil {
			return errors.Join(err, d.discardForkUnsafe(msess))
		}
		return nil
	}

	d.mediaUpdating = true
	d.mu.Unlock()
	err := msess.FinalizeContext(ctx)
	d.mu.Lock()
	d.mediaUpdating = false

	if d.closed {
		// Close has taken what it closes already, and would not close msess.
		return errors.Join(errMediaClosed, d.discardForkUnsafe(msess))
	}
	if err == nil && ctx.Err() != nil {
		// Keyed as the exchange it belongs to was given up: nothing is
		// installed for a caller that no longer waits, or on a call that
		// has ended.
		err = context.Cause(ctx)
	}
	if err == nil {
		err = d.replaceMediaSessionUnsafe(msess)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("%w: %w", errMediaKeyingFailed, err), d.discardForkUnsafe(msess))
	}
	return nil
}

// ownsSocketsUnsafe reports whether msess, a fork, runs on sockets of its own
// and not on those of the media session in place. Must be called with d.mu
// held.
func (d *DialogMedia) ownsSocketsUnsafe(msess *media.MediaSession) bool {
	cur := d.mediaSession
	return cur == nil || !cur.Laddr.IP.Equal(msess.Laddr.IP) || cur.Laddr.Port != msess.Laddr.Port
}

// discardForkUnsafe closes a fork that is not going to be installed, if it
// was moved to sockets of its own. A fork on the sockets of the media session
// in place is only dropped, since closing it would close them. Must be called
// with d.mu held.
func (d *DialogMedia) discardForkUnsafe(msess *media.MediaSession) error {
	if !d.ownsSocketsUnsafe(msess) {
		return nil
	}
	return msess.Close()
}

// takeUninstalledForksUnsafe takes, for the Close that latched the media
// closed, the forks an exchange left without installing them: the one that
// made an offer of ours and the one staged for an offer of the peer's. It
// returns what that Close runs to close those on sockets of their own, which
// also reports what the discards of an abort could not return. A fork whose
// handshake is running is not among them: the exchange that keys it discards
// it once it finds the media closed. Must be called with d.mu held.
func (d *DialogMedia) takeUninstalledForksUnsafe() func() error {
	var own []*media.MediaSession
	for _, fork := range []*media.MediaSession{d.pendingMediaSession, d.stagedMediaSession} {
		if fork != nil && d.ownsSocketsUnsafe(fork) {
			own = append(own, fork)
		}
	}
	d.pendingMediaSession, d.stagedMediaSession = nil, nil
	err := d.discardErr
	d.discardErr = nil
	return func() error {
		for _, fork := range own {
			err = errors.Join(err, fork.Close())
		}
		return err
	}
}

// closeReplacedSockets closes old, the media session msess has replaced, when
// msess runs on sockets of its own. The reader and the writer were moved off
// old by then, so its sockets carry nothing any more. Closing them releases
// the ports and fails a read still blocked on the old RTP socket, which the
// packet reader then takes up on the new session. Without it that read never
// returns, because the peer now sends to the new address.
func closeReplacedSockets(old, msess *media.MediaSession) error {
	if old == nil || old == msess {
		return nil
	}
	if old.Laddr.IP.Equal(msess.Laddr.IP) && old.Laddr.Port == msess.Laddr.Port {
		return nil
	}
	if err := old.Close(); err != nil {
		return fmt.Errorf("closing replaced media session: %w", err)
	}
	return nil
}
