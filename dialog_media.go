// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"
)

var (
	HTTPDebug = os.Getenv("HTTP_DEBUG") == "true"

	DefaultPlaybackHTTPClient = http.Client{
		Timeout: 20 * time.Second,
	}

	errNoRTPSession = errors.New("no rtp session")
)

func init() {
	if HTTPDebug {
		DefaultPlaybackHTTPClient.Transport = &loggingTransport{}
	}
}

// DialogMedia is common struct for server and client session and it shares same functionality
// which is mostly arround media
type DialogMedia struct {
	mu sync.Mutex

	// media session is RTP local and remote
	// it is forked on media changes and updated on writer and reader
	// must be mutex protected
	// It MUST be always created on Media Session Init
	// Only safe to use after dialog Answered (Completed state)
	mediaSession *media.MediaSession

	// rtp session is created for usage with RTPPacketReader and RTPPacketWriter
	// it adds RTCP layer and RTP monitoring before passing packets to MediaSession
	rtpSession *media.RTPSession

	// iceAgentOwner is the media session that created this dialog's ICE agent,
	// kept after a re-INVITE replaced it with a fork. The fork shares the ICE
	// pair but not the agent, and closing the pair does not release the UDP
	// mux or the socket it wraps; only the owner's Close does. Dropping the
	// owner on the swap would leak that socket and its mux goroutine for the
	// life of the process, and hand the port back to the allocator while it is
	// still bound. Nil until a swap, and for a dialog without ICE.
	iceAgentOwner *media.MediaSession
	// Packet reader is default reader for RTP audio stream
	// Use always AudioReader to get current Audio reader
	// Use this only as read only
	// It MUST be always created on Media Session Init
	// Only safe to use after dialog Answered (Completed state)
	RTPPacketReader *media.RTPPacketReader

	// Packet writer is default writer for RTP audio stream
	// Use always AudioWriter to get current Audio reader
	// Use this only as read only
	RTPPacketWriter *media.RTPPacketWriter

	// In case we are chaining audio readers
	audioReader io.Reader
	audioWriter io.Writer

	// remoteContactTarget is actual target changed caused by incomign or outgoing REINVITE
	// We do not use sipgo as this needs mutex but also keeping original invite
	remoteContactTarget *sip.ContactHeader

	onReferNotify func(statusCode int)

	// referMu guards referAttempts, referHighestCSeq, referLatest and the
	// attempts they hold. It is separate from mu so observing a REFER NOTIFY
	// never contends with media state.
	referMu sync.Mutex
	// referAttempts are the registered REFER attempts, oldest first: those
	// waiting for their outcome, and those whose wait ended on the deadline or
	// on cancellation and can still receive a late terminal NOTIFY.
	referAttempts []*referAttempt
	// referHighestCSeq is the highest CSeq recorded for a REFER sent on this
	// dialog, dropped attempts included. In-dialog CSeqs only grow, so an Event
	// id at or below it belongs to an earlier REFER.
	referHighestCSeq uint32
	// referLatest is the attempt begun most recently. It is kept after it is
	// dropped, so a NOTIFY without an Event id is never given to an older
	// attempt once a newer one has started.
	referLatest *referAttempt

	// releaseRTPPort returns this dialog RTP port to the allocator that handed
	// it out. Nil when no allocator is installed, as the OS takes the port back
	// when the socket closes. Set next to mediaSession and consumed by Close.
	releaseRTPPort func()

	onClose       func() error
	onMediaUpdate func(*DialogMedia)

	closed bool
}

// referAttempt is one REFER sent on the dialog and what has been observed for
// it. Every field is guarded by DialogMedia.referMu.
type referAttempt struct {
	// cseq is the CSeq of the sent REFER. RFC 3515 makes the NOTIFY's Event id
	// the CSeq of the REFER that created the subscription, so this is what an
	// id-carrying NOTIFY is matched against.
	cseq uint32
	// cseqKnown is false until setReferAttemptCSeq records the sent REFER's CSeq.
	// Until then an id-carrying NOTIFY cannot be told apart from this attempt's.
	cseqKnown bool
	// registered is true while the attempt is in referAttempts.
	registered bool
	// end is how the attempt ended, zero while its wait is open.
	end ReferEnd
	// notifies are the NOTIFYs recorded while the wait was open, at most
	// maxReferNotifies, and dropped counts the ones not kept.
	notifies []ReferNotify
	dropped  int
	// wake tells the waiting REFER that a NOTIFY ended the attempt. Capacity 1,
	// sent to without blocking.
	wake chan struct{}
	// onLate receives a terminal NOTIFY that arrives after the wait ended on the
	// deadline or on cancellation.
	onLate func(ReferLateNotify)
}

// beginReferAttempt registers a new REFER attempt as the most recent one and
// returns it. Called before the REFER is sent so a NOTIFY that races ahead of
// the REFER's response is recorded rather than lost.
func (d *DialogMedia) beginReferAttempt(onLate func(ReferLateNotify)) *referAttempt {
	a := &referAttempt{registered: true, wake: make(chan struct{}, 1), onLate: onLate}
	d.referMu.Lock()
	d.referAttempts = append(d.referAttempts, a)
	d.referLatest = a
	d.referMu.Unlock()
	return a
}

// setReferAttemptCSeq records the sent REFER's CSeq: on the dialog's high-water
// mark whatever a's state, and on a only while a is registered.
func (d *DialogMedia) setReferAttemptCSeq(a *referAttempt, cseq uint32) {
	d.referMu.Lock()
	if cseq > d.referHighestCSeq {
		d.referHighestCSeq = cseq
	}
	if a.registered {
		a.cseq = cseq
		a.cseqKnown = true
	}
	d.referMu.Unlock()
}

// observeReferNotify routes one parsed NOTIFY to the REFER attempt it belongs
// to. With an Event id that is the registered attempt whose known CSeq equals
// it, or else the most recent attempt while its CSeq is still unknown, provided
// no other attempt awaits its CSeq and the id is above every REFER CSeq the
// dialog has recorded; without an id it is the most recent attempt. A NOTIFY
// with no such attempt is dropped.
//
// While the attempt's wait is open the NOTIFY is recorded, and a terminal one
// ends the wait. After the wait ended on the deadline or on cancellation a
// terminal NOTIFY is late: the attempt is dropped and its late callback is
// returned with the late NOTIFY, for the caller to run once the lock is
// released. The returned callback is nil when there is nothing to deliver.
func (d *DialogMedia) observeReferNotify(n ReferNotify) (onLate func(ReferLateNotify), late ReferLateNotify) {
	d.referMu.Lock()
	defer d.referMu.Unlock()

	a := d.referNotifyTargetLocked(n)
	if a == nil {
		return nil, ReferLateNotify{}
	}
	switch a.end {
	case 0:
		a.recordLocked(n)
	case ReferEndDeadline, ReferEndCancelled:
		if !n.endsSubscription() {
			return nil, ReferLateNotify{}
		}
		d.removeReferAttemptLocked(a)
		return a.onLate, ReferLateNotify{CSeq: a.cseq, CSeqKnown: a.cseqKnown, Notify: n}
	case ReferEndFinalNotify, ReferEndRefused, ReferEndSubscriptionEnded:
		// The attempt's outcome is already decided and about to be collected.
	}
	return nil, ReferLateNotify{}
}

// referNotifyTargetLocked returns the registered attempt n belongs to, or nil.
// Called with referMu held.
func (d *DialogMedia) referNotifyTargetLocked(n ReferNotify) *referAttempt {
	if n.HasEventID {
		awaitingCSeq := 0
		for _, a := range d.referAttempts {
			if !a.cseqKnown {
				awaitingCSeq++
				continue
			}
			if a.cseq == n.EventID {
				return a
			}
		}
		// The NOTIFY may have raced ahead of the REFER's response, while the
		// most recent attempt's CSeq is not known yet. In-dialog CSeqs only
		// grow, so an id at or below the highest REFER CSeq recorded belongs to
		// an earlier REFER, even one already dropped. With a second attempt
		// also awaiting its CSeq the id may be that attempt's, so it is given
		// to neither.
		if latest := d.referLatest; latest != nil && latest.registered && !latest.cseqKnown &&
			awaitingCSeq == 1 && n.EventID > d.referHighestCSeq {
			return latest
		}
		return nil
	}
	if latest := d.referLatest; latest != nil && latest.registered {
		return latest
	}
	return nil
}

// recordLocked records n on an attempt whose wait is open, and ends the wait
// when n is terminal. Past maxReferNotifies a non-terminal NOTIFY is only
// counted, and a terminal one takes the last slot so it is always last.
// Called with referMu held.
func (a *referAttempt) recordLocked(n ReferNotify) {
	switch {
	case len(a.notifies) < maxReferNotifies:
		a.notifies = append(a.notifies, n)
	case n.endsSubscription():
		a.notifies[len(a.notifies)-1] = n
		a.dropped++
	default:
		a.dropped++
	}

	switch {
	case n.isFinal():
		a.end = ReferEndFinalNotify
	case n.endsSubscription():
		a.end = ReferEndSubscriptionEnded
	default:
		return
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// finishReferAttempt ends a's wait with end unless a NOTIFY already ended it,
// or with ReferEndRefused regardless, and returns what a observed. An attempt
// that ended on a final NOTIFY, an ended subscription or a refused REFER is
// dropped; one that ended on the deadline or on cancellation stays registered
// for a late terminal NOTIFY.
func (d *DialogMedia) finishReferAttempt(a *referAttempt, end ReferEnd) ReferObservation {
	d.referMu.Lock()
	defer d.referMu.Unlock()

	// A refused REFER created no subscription, so its response decides the end
	// even if a NOTIFY claiming an outcome raced ahead of it.
	if a.end == 0 || end == ReferEndRefused {
		a.end = end
	}
	switch a.end {
	case ReferEndFinalNotify, ReferEndSubscriptionEnded, ReferEndRefused:
		d.removeReferAttemptLocked(a)
	case ReferEndDeadline, ReferEndCancelled:
		// Stays registered, so a late terminal NOTIFY finds this attempt.
	}
	return ReferObservation{
		End:             a.end,
		CSeq:            a.cseq,
		CSeqKnown:       a.cseqKnown,
		Notifies:        slices.Clone(a.notifies),
		NotifiesDropped: a.dropped,
	}
}

// dropReferAttempt unregisters a. Dropping an attempt never affects another.
func (d *DialogMedia) dropReferAttempt(a *referAttempt) {
	d.referMu.Lock()
	d.removeReferAttemptLocked(a)
	d.referMu.Unlock()
}

// removeReferAttemptLocked unregisters a, leaving referLatest as it is. Called
// with referMu held.
func (d *DialogMedia) removeReferAttemptLocked(a *referAttempt) {
	if !a.registered {
		return
	}
	a.registered = false
	d.referAttempts = slices.DeleteFunc(d.referAttempts, func(r *referAttempt) bool { return r == a })
}

func (d *DialogMedia) Close() error {
	// Any hook attached
	// Prevent double exec
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true

	onClose := d.onClose
	d.onClose = nil
	m := d.mediaSession
	rtpSess := d.rtpSession
	iceOwner := d.iceAgentOwner
	d.iceAgentOwner = nil
	releasePort := d.releaseRTPPort
	d.releaseRTPPort = nil

	d.mu.Unlock()

	// A NOTIFY after close finds no REFER attempt, late ones included.
	d.referMu.Lock()
	for _, a := range d.referAttempts {
		a.registered = false
	}
	d.referAttempts = nil
	d.referLatest = nil
	d.referMu.Unlock()

	var e1, e2, e3, e4 error
	if onClose != nil {
		e1 = onClose()
	}

	if rtpSess != nil {
		e2 = rtpSess.MonitorClose()
	}

	if m != nil {
		e3 = m.Close()
	}

	// The agent goes after the session using its pair, so nothing is still
	// reading or writing on the socket it releases, and before the port is
	// returned below.
	if iceOwner != nil && iceOwner != m {
		e4 = iceOwner.Close()
	}

	// After the sockets are closed, never before. An allocator may hold the port
	// for a drain window so late RTP from this call can not land on the next
	// call socket, and that window has to start when the wire is actually down.
	// The closed latch above makes this exactly once.
	if releasePort != nil {
		releasePort()
	}
	return errors.Join(e1, e2, e3, e4)
}

func (d *DialogMedia) OnClose(f func() error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onCloseUnsafe(f)
}

func (d *DialogMedia) onCloseUnsafe(f func() error) {
	if d.onClose != nil {
		prev := d.onClose
		d.onClose = func() error {
			return errors.Join(prev(), f())
		}
		return
	}
	d.onClose = f
}

func (d *DialogMedia) InitMediaSession(m *media.MediaSession, r *media.RTPPacketReader, w *media.RTPPacketWriter) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.initMediaSessionUnsafe(m, r, w)
}

func (d *DialogMedia) initMediaSessionUnsafe(m *media.MediaSession, r *media.RTPPacketReader, w *media.RTPPacketWriter) {
	d.mediaSession = m
	d.RTPPacketReader = r
	d.RTPPacketWriter = w
}

func (d *DialogMedia) initRTPSessionUnsafe(m *media.MediaSession, rtpSess *media.RTPSession) {
	d.mediaSession = m
	d.rtpSession = rtpSess
	d.RTPPacketReader = media.NewRTPPacketReaderSession(rtpSess)
	d.RTPPacketWriter = media.NewRTPPacketWriterSession(rtpSess)
}

func (d *DialogMedia) initMediaSessionFromConf(conf MediaConfig) error {
	if d.mediaSession != nil {
		// To allow testing or customizing current underhood session, this may be
		// precreated, so we want to return if already initialized.
		// Ex: To fake IO on RTP connection or different media stacks
		return nil
	}

	bindIP := conf.bindIP
	if bindIP == nil {
		var err error
		bindIP, _, err = sip.ResolveInterfacesIP("ip4", nil)
		if err != nil {
			return err
		}
	}

	// Port 0 leaves the choice to the OS or to the media package port range. An
	// allocator, when installed, makes it instead, so the port comes from a range
	// the operator has bounded and opened on the firewall.
	rtpPort := 0
	var releasePort func()
	if alloc := conf.RTPPortAllocator; alloc != nil {
		port, err := alloc.AllocateRTPPort()
		if err != nil {
			return err
		}
		rtpPort = port
		releasePort = func() { alloc.ReleaseRTPPort(port) }
	}

	// The session takes DTLS by value, so an unset config is the zero one. That
	// keeps a caller that never touched DTLS on the path it had before.
	var dtlsConf media.DTLSConfig
	if conf.DTLSConfig != nil {
		dtlsConf = *conf.DTLSConfig
	}

	sess := &media.MediaSession{
		Codecs:     slices.Clone(conf.Codecs),
		Laddr:      net.UDPAddr{IP: bindIP, Port: rtpPort},
		ExternalIP: conf.externalIP,
		Mode:       sdp.ModeSendrecv,
		SecureRTP:  conf.secureRTP,
		SRTPAlg:    conf.SecureRTPAlg,
		RTPNAT:     conf.rtpNAT,
		DTLSConf:   dtlsConf,
		ICEConf:    conf.ICEConfig,
	}

	if err := sess.Init(); err != nil {
		// Nothing was bound, so the port goes straight back rather than wait for
		// a Close that will not come: this dialog has no media session.
		if releasePort != nil {
			releasePort()
		}
		return err
	}
	d.mediaSession = sess
	d.releaseRTPPort = releasePort
	return nil
}

// RTPSession returns underhood rtp session
// NOTE: this can be nil
func (d *DialogMedia) RTPSession() *media.RTPSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rtpSession
}

func (d *DialogMedia) MediaSession() *media.MediaSession {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mediaSession
}

func (d *DialogMedia) handleMediaUpdate(req *sip.Request, tx sip.ServerTransaction, contactHDR sip.Header) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.remoteContactTarget = req.Contact().Clone()

	// When body is not present this can mean client is doing keep alive
	// Still offer needs to be responded.
	// Content-Length: 0 can reach us as an empty but non nil body, so length is
	// checked rather than nilness.
	if len(req.Body()) > 0 {
		if err := d.sdpReInviteUnsafe(req.Body()); err != nil {
			// The offer is well formed but asks for an ICE restart, which this
			// session cannot perform. The fork was never installed, so the call
			// carries on over its current pair. The reason phrase is fixed and
			// carries no error text, so nothing internal reaches the peer.
			if errors.Is(err, media.ErrICERestartUnsupported) {
				return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptableHere, "Not Acceptable Here", nil))
			}
			return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusRequestTerminated, "Request Terminated - "+err.Error(), nil))
		}

		if d.onMediaUpdate != nil {
			d.mu.Unlock()
			d.onMediaUpdate(d)
			d.mu.Lock()
		}
	}

	// A request for an offer can not be answered with an SDP we never built.
	// The bodied path rejects this inside sdpReInviteUnsafe.
	if d.mediaSession == nil {
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusRequestTerminated, "Request Terminated - no media session present", nil))
	}

	// Reply with updated SDP
	sd := d.mediaSession.LocalSDP()
	res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", sd)
	res.AppendHeader(contactHDR)
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	return tx.Respond(res)
}

// Must be protected with lock
func (d *DialogMedia) sdpReInviteUnsafe(sdp []byte) error {
	if d.mediaSession == nil {
		return fmt.Errorf("no media session present")
	}

	// An inbound re-INVITE carries an offer, whichever side of the dialog we are.
	d.mediaSession.RemoteSDPIsAnswer = false
	if err := d.sdpUpdateUnsafe(sdp); err != nil {
		return err
	}
	return nil
}

func (d *DialogMedia) checkEarlyMedia(remoteSDP []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	// RTP Session is only created when negotiation is finished. We use this to detect existing media
	if d.rtpSession == nil {
		return errNoRTPSession
	}
	return d.sdpUpdateUnsafe(remoteSDP)
}

func (d *DialogMedia) sdpUpdateUnsafe(sdp []byte) error {
	msess := d.mediaSession.Fork()
	if err := msess.RemoteSDP(sdp); err != nil {
		return fmt.Errorf("sdp update media remote SDP applying failed: %w", err)
	}

	return d.replaceRTPSessionUnsafe(msess)
}

func (d *DialogMedia) mediaUpdateUnsafe(msess *media.MediaSession) error {
	return d.replaceRTPSessionUnsafe(msess)
}

// replaceRTPSessionUnsafe replaces the RTP session after the old monitor has
// fully stopped. A fork preserves statistics and shared connections; a new
// session is used when media connections were recreated.
func (d *DialogMedia) replaceRTPSessionUnsafe(msess *media.MediaSession) error {
	oldRTPSess := d.rtpSession
	if oldRTPSess == nil {
		return fmt.Errorf("RTP Session is nil while trying to update it")
	}
	// This will block until read  RTCP is closed fully, which we need before starting new one
	if err := oldRTPSess.MonitorClose(); err != nil {
		return err
	}

	// Same as media, we are forking RTP Session
	rtpSess := oldRTPSess.Fork(msess)
	if err := rtpSess.MonitorBackground(); err != nil {
		return errors.Join(err, rtpSess.Close())
	}

	// Make sure any current reader is not consuming old media session.
	d.RTPPacketReader.UpdateRTPSession(rtpSess)
	d.RTPPacketWriter.UpdateRTPSession(rtpSess)

	old := d.mediaSession
	// Every path forks from the session installed at setup, so exactly one
	// session ever owns an ICE agent, and it is the one replaced first. It is
	// kept for Close rather than closed here, because the fork's media still
	// runs on the pair that agent holds open.
	if old != nil && old != msess && old.OwnsICEAgent() && d.iceAgentOwner == nil {
		d.iceAgentOwner = old
	}
	d.mediaSession = msess
	d.rtpSession = rtpSess

	// A fork rebound to a new local address runs on sockets of its own, and the
	// reader and writer were moved off the replaced session above, so its
	// sockets carry nothing any more. Closing them releases the ports and fails
	// a read still blocked on the old RTP socket, which the packet reader then
	// retries on the new session. Without it that read never returns, because
	// the peer now sends to the new address.
	if old != nil && old != d.iceAgentOwner && (!old.Laddr.IP.Equal(msess.Laddr.IP) || old.Laddr.Port != msess.Laddr.Port) {
		if err := old.Close(); err != nil {
			return fmt.Errorf("closing replaced media session: %w", err)
		}
	}
	return nil
}

type AudioReaderOption func(d *DialogMedia) error

type MediaProps struct {
	Codec media.Codec
	Laddr string
	Raddr string
}

func WithAudioReaderMediaProps(p *MediaProps) AudioReaderOption {
	return func(d *DialogMedia) error {
		p.Codec = media.CodecAudioFromSession(d.mediaSession)
		p.Laddr = d.mediaSession.Laddr.String()
		p.Raddr = d.mediaSession.Raddr.String()
		return nil
	}
}

// WithAudioReaderJitterBuffer inserts an RTP jitter buffer before the payload reader.
// Packet duration is derived from the negotiated audio codec.
func WithAudioReaderJitterBuffer(opts media.RTPJitterBufferOptions) AudioReaderOption {
	return func(d *DialogMedia) error {
		if d.mediaSession == nil || d.RTPPacketReader == nil {
			return fmt.Errorf("no media setup")
		}

		codec := media.CodecAudioFromSession(d.mediaSession)
		if codec.SampleDur <= 0 {
			return fmt.Errorf("invalid audio codec packet duration: %s", codec.SampleDur)
		}

		reader := d.RTPPacketReader.Reader()
		if reader == nil {
			return fmt.Errorf("no RTP reader setup")
		}

		jitter := media.NewRTPJitterBuffer(reader, codec.SampleDur, opts)
		d.RTPPacketReader.UpdateReader(jitter)

		// UpdateReader interrupts a potentially blocked RTPSession read. The new
		// jitter reader uses that same session, so restore normal network reading.
		if session, ok := reader.(*media.RTPSession); ok {
			if err := session.Sess.StartRTP(1); err != nil {
				d.RTPPacketReader.UpdateReader(reader)
				_ = jitter.Close()
				return fmt.Errorf("failed to start jitter buffer RTP reader: %w", err)
			}
		}

		d.onCloseUnsafe(jitter.Close)
		return nil
	}
}

// WithAudioReaderRTPStats creates RTP Statistics interceptor on audio reader
func WithAudioReaderRTPStats(hook media.OnRTPReadStats) AudioReaderOption {
	return func(d *DialogMedia) error {
		r := &media.RTPStatsReader{
			Reader:         d.getAudioReader(),
			RTPSession:     d.rtpSession,
			OnRTPReadStats: hook,
		}
		d.audioReader = r
		return nil
	}
}

// WithAudioReaderDTMF creates DTMF interceptor
func WithAudioReaderDTMF(r *DTMFReader) AudioReaderOption {
	return func(d *DialogMedia) error {
		r.dtmfReader = media.NewRTPDTMFReader(dtmfCodec(d.mediaSession), d.RTPPacketReader, d.getAudioReader())
		r.mediaSession = d.mediaSession

		d.audioReader = r
		return nil
	}
}

func WithAudioReaderPCMMonitor(mon *audio.MonitorPCMReader, w io.Writer) AudioReaderOption {
	return func(d *DialogMedia) error {
		codec := media.CodecAudioFromSession(d.mediaSession)
		if err := mon.Init(w, codec, d.getAudioReader()); err != nil {
			return err
		}
		mon.FlushOnError = true // It will flush on writer stop
		d.audioReader = mon
		return nil
	}
}

// AudioReader returns io.Reader on which you can read your ENCODED audio.
// By default it is RTPPacketReader unless overwritten with SetAudioReader().
//
// NOTE: AudioReader must be called after negotiation is finished, like Answer()
// Reading buffer should be equal or bigger of media.RTPBufSize
// Use AuidioListen for optimized reading.
func (d *DialogMedia) AudioReader(opts ...AudioReaderOption) (io.Reader, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}
	return d.getAudioReader(), nil
}

func (d *DialogMedia) getAudioReader() io.Reader {
	if d.audioReader != nil {
		return d.audioReader
	}
	return d.RTPPacketReader
}

// audioReaderProps
func (d *DialogMedia) audioReaderProps(p *MediaProps) io.Reader {
	d.mu.Lock()
	defer d.mu.Unlock()

	WithAudioReaderMediaProps(p)(d)
	return d.getAudioReader()
}

// SetAudioReader adds/changes audio reader.
// Use this when you want to have interceptors of your audio
func (d *DialogMedia) SetAudioReader(r io.Reader) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audioReader = r
}

type AudioWriterOption func(d *DialogMedia) error

func WithAudioWriterMediaProps(p *MediaProps) AudioWriterOption {
	return func(d *DialogMedia) error {
		p.Codec = media.CodecAudioFromSession(d.mediaSession)
		p.Laddr = d.mediaSession.Laddr.String()
		p.Raddr = d.mediaSession.Raddr.String()
		return nil
	}
}

// WithAudioReaderRTPStats creates RTP Statistics interceptor on audio reader
func WithAudioWriterRTPStats(hook media.OnRTPWriteStats) AudioWriterOption {
	return func(d *DialogMedia) error {
		w := media.RTPStatsWriter{
			Writer:          d.getAudioWriter(),
			RTPSession:      d.rtpSession,
			OnRTPWriteStats: hook,
		}
		d.audioWriter = &w
		return nil
	}
}

// WithAudioWriterDTMF adds DTMF into audio pipeline
func WithAudioWriterDTMF(r *DTMFWriter) AudioWriterOption {
	return func(d *DialogMedia) error {
		r.dtmfWriter = media.NewRTPDTMFWriter(dtmfCodec(d.mediaSession), d.RTPPacketWriter, d.getAudioWriter())
		r.mediaSession = d.mediaSession
		d.audioWriter = r
		return nil
	}
}

// WithAudioWriterMonitor initializes and adds PCM monitor in audio pipeline. It records and decodes stream into PCM.
func WithAudioWriterMonitor(mon *audio.MonitorPCMWriter, w io.Writer) AudioWriterOption {
	return func(d *DialogMedia) error {
		codec := media.CodecAudioFromSession(d.mediaSession)
		if err := mon.Init(w, codec, d.getAudioWriter()); err != nil {
			return err
		}
		mon.FlushOnError = true // It will flush on writer stop
		d.audioWriter = mon
		return nil
	}
}

// AudioWriter returns io.Writer on which you can write your ENCODED audio.
// By default it is RTPPacketWriter unless overwritten with SetAudioWriter().
// NOTE: RTPPacketWriter has running sample clock, but it expects samples sent, match sample duration of codec.
func (d *DialogMedia) AudioWriter(opts ...AudioWriterOption) (io.Writer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}

	return d.getAudioWriter(), nil
}

func (d *DialogMedia) getAudioWriter() io.Writer {
	if d.audioWriter != nil {
		return d.audioWriter
	}
	return d.RTPPacketWriter
}

func (d *DialogMedia) audioWriterProps(p *MediaProps) io.Writer {
	d.mu.Lock()
	defer d.mu.Unlock()

	WithAudioWriterMediaProps(p)(d)
	return d.getAudioWriter()
}

// SetAudioWriter adds/changes audio reader.
// Use this when you want to have pipelines of your audio
func (d *DialogMedia) SetAudioWriter(r io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audioWriter = r
}

func (d *DialogMedia) Media() *DialogMedia {
	return d
}

// Echo does audio echo for you
func (d *DialogMedia) Echo() error {
	audioR, err := d.AudioReader()
	if err != nil {
		return err
	}
	audioW, err := d.AudioWriter()
	if err != nil {
		return err
	}

	_, err = media.Copy(audioR, audioW)
	return err
}

// PlaybackCreate creates playback for audio
func (d *DialogMedia) PlaybackCreate() (AudioPlayback, error) {
	mprops := MediaProps{}
	w := d.audioWriterProps(&mprops)
	if w == nil {
		return AudioPlayback{}, fmt.Errorf("no media setup")
	}
	p := NewAudioPlayback(w, mprops.Codec)
	// On each play it needs reset RTP timestamp
	p.onPlay = d.RTPPacketWriter.ResetTimestamp
	return p, nil
}

// PlaybackControlCreate creates playback for audio with controls like mute unmute
func (d *DialogMedia) PlaybackControlCreate() (AudioPlaybackControl, error) {
	// NOTE we should avoid returning pointers for any IN dialplan to avoid heap
	mprops := MediaProps{}
	w := d.audioWriterProps(&mprops)

	if w == nil {
		return AudioPlaybackControl{}, fmt.Errorf("no media setup")
	}
	// Audio is controled via audio reader/writer
	control := &audioControl{
		Writer: w,
	}

	p := AudioPlaybackControl{
		AudioPlayback: NewAudioPlayback(control, mprops.Codec),
		control:       control,
	}
	return p, nil
}

// PlaybackRingtoneCreate is creating playback for ringtone
//
// Experimental
func (d *DialogMedia) PlaybackRingtoneCreate() (AudioRingtone, error) {
	mprops := MediaProps{}
	w := d.audioWriterProps(&mprops)
	if w == nil {
		return AudioRingtone{}, fmt.Errorf("no media setup")
	}

	ringtone, err := audio.RingtoneLoadPCM(mprops.Codec)
	if err != nil {
		return AudioRingtone{}, err
	}

	encoder := audio.PCMEncoderWriter{}
	if err := encoder.Init(mprops.Codec, w); err != nil {
		return AudioRingtone{}, err
	}

	ar := AudioRingtone{
		writer:       &encoder,
		ringtone:     ringtone,
		sampleSize:   mprops.Codec.Samples16(),
		mediaSession: d.mediaSession,
	}
	return ar, nil
}

// AudioStereoRecordingCreate creates Stereo Recording audio Pipeline and stores as Wav file format
// For audio to be recorded use AudioReader and AudioWriter from Recording
//
// Tips:
// If you want to make permanent in audio pipeline use SetAudioReader, SetAudioWriter
//
// NOTE: API WILL change
func (d *DialogMedia) AudioStereoRecordingCreate(wavFile *os.File) (AudioStereoRecordingWav, error) {
	mpropsW := MediaProps{}
	aw := d.audioWriterProps(&mpropsW)
	if aw == nil {
		return AudioStereoRecordingWav{}, fmt.Errorf("no media setup")
	}

	mpropsR := MediaProps{}
	ar := d.audioReaderProps(&mpropsR)
	if ar == nil {
		return AudioStereoRecordingWav{}, fmt.Errorf("no media setup")
	}

	return newDialogRecordingWav(wavFile, ar, mpropsR, aw, mpropsW)
}

// Listen keeps reading stream until it gets closed or deadlined
// Use ListenBackground or ListenContext for better control
func (d *DialogMedia) Listen() (err error) {
	buf := make([]byte, media.RTPBufSize)
	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}

	for {
		_, err := audioReader.Read(buf)
		if err != nil {
			return err
		}
	}
}

// ListenBackground listens on stream in background and allows correct stoping of stream on network layer
func (d *DialogMedia) ListenBackground() (stop func() error, err error) {
	buf := make([]byte, media.RTPBufSize)
	audioReader, err := d.AudioReader()
	if err != nil {
		return nil, err
	}

	wg := sync.WaitGroup{}
	var readErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, err := audioReader.Read(buf)
			if err != nil {
				if err, ok := err.(net.Error); ok && err.Timeout() {
					return
				}
				readErr = err
				return
			}
		}
	}()

	return func() error {
		if err := d.mediaSession.StopRTP(1, 0); err != nil {
			return err
		}
		wg.Wait() // This makes sure we have exited reading
		if err := d.mediaSession.StartRTP(1); err != nil {
			return err
		}
		return readErr
	}, nil
}

// ListenContext listens until context is canceled.
func (d *DialogMedia) ListenContext(pctx context.Context) error {
	buf := make([]byte, media.RTPBufSize)
	ctx, cancel := context.WithCancel(pctx)
	defer cancel()

	go func() {
		<-ctx.Done()
		if pctx.Err() != nil {
			d.mediaSession.StopRTP(1, 0)
		}
	}()
	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}
	for {
		_, err := audioReader.Read(buf)
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Timeout() {
				return nil
			}
			return err
		}
	}
}

func (d *DialogMedia) ListenUntil(dur time.Duration) error {
	buf := make([]byte, media.RTPBufSize)

	d.mediaSession.StopRTP(1, dur)
	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}
	for {
		_, err := audioReader.Read(buf)
		if err != nil {
			return err
		}
	}
}

func (d *DialogMedia) StopRTP(rw int8, dur time.Duration) error {
	return d.mediaSession.StopRTP(rw, dur)
}

func (d *DialogMedia) StartRTP(rw int8, dur time.Duration) error {
	return d.mediaSession.StartRTP(rw)
}

// dtmfCodec returns the telephone-event codec DTMF is carried on for this
// session. It is the negotiated one: telephone-event is a dynamic format, so the
// number on the wire is the peer's rather than our default (RFC 3264 section
// 6.1).
//
// A session that negotiated no telephone-event keeps the package default, which
// is the behaviour every caller had before.
func dtmfCodec(s *media.MediaSession) media.Codec {
	if codec, ok := media.CodecTelephoneEventFromSession(s); ok {
		return codec
	}
	return media.CodecTelephoneEvent8000
}

type DTMFReader struct {
	mediaSession *media.MediaSession
	dtmfReader   *media.RTPDtmfReader
	onDTMF       func(dtmf rune) error
}

// AudioReaderDTMF is DTMF over RTP. It reads audio and provides hook for dtmf while listening for audio
// Use Listen or OnDTMF after this call
func (m *DialogMedia) AudioReaderDTMF() (*DTMFReader, error) {
	ar, err := m.AudioReader()
	if err != nil {
		return nil, err
	}
	return &DTMFReader{
		dtmfReader:   media.NewRTPDTMFReader(dtmfCodec(m.mediaSession), m.RTPPacketReader, ar),
		mediaSession: m.mediaSession,
	}, nil
}

func (d *DTMFReader) Listen(onDTMF func(dtmf rune) error, dur time.Duration) error {
	d.onDTMF = onDTMF
	buf := make([]byte, media.RTPBufSize)
	for {
		if _, err := d.readDeadline(buf, dur); err != nil {
			if errors.Is(err, os.ErrDeadlineExceeded) {
				return nil
			}
			return err
		}
	}
}

// readDeadline(reads RTP until
func (d *DTMFReader) readDeadline(buf []byte, dur time.Duration) (n int, err error) {
	mediaSession := d.mediaSession
	if dur > 0 {
		// Stop RTP
		mediaSession.StopRTP(1, dur)
		defer mediaSession.StartRTP(2)
	}
	return d.Read(buf)
}

// OnDTMF must be called before audio reading
func (d *DTMFReader) OnDTMF(onDTMF func(dtmf rune) error) {
	d.onDTMF = onDTMF
}

// Read exposes io.Reader that can be used as AudioReader
func (d *DTMFReader) Read(buf []byte) (n int, err error) {
	// This is optimal way of reading audio and DTMF
	dtmfReader := d.dtmfReader
	n, err = dtmfReader.Read(buf)
	if err != nil {
		return n, err
	}

	if dtmf, ok := dtmfReader.ReadDTMF(); ok {
		if err := d.onDTMF(dtmf); err != nil {
			return n, err
		}
	}
	return n, nil
}

type DTMFWriter struct {
	mediaSession *media.MediaSession
	dtmfWriter   *media.RTPDtmfWriter
}

func (m *DialogMedia) AudioWriterDTMF() (*DTMFWriter, error) {
	aw, err := m.AudioWriter()
	if err != nil {
		return nil, err
	}

	return &DTMFWriter{
		dtmfWriter:   media.NewRTPDTMFWriter(dtmfCodec(m.mediaSession), m.RTPPacketWriter, aw),
		mediaSession: m.mediaSession,
	}, nil
}

func (w *DTMFWriter) WriteDTMF(dtmf rune) error {
	return w.dtmfWriter.WriteDTMF(dtmf)
}

// AudioReader exposes DTMF audio writer. You should use this for parallel audio processing
func (w *DTMFWriter) AudioWriter() *media.RTPDtmfWriter {
	return w.dtmfWriter
}

// Write exposes as io.Writer that can be used as AudioWriter
func (w *DTMFWriter) Write(buf []byte) (n int, err error) {
	return w.dtmfWriter.Write(buf)
}
