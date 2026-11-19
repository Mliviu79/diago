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
	// Pending fork used while a locally initiated SDP offer waits for its answer.
	pendingMediaSession *media.MediaSession

	// rtp session is created for usage with RTPPacketReader and RTPPacketWriter
	// it adds RTCP layer and RTP monitoring before passing packets to MediaSession
	rtpSession *media.RTPSession
	// unstartedRTPSession is the RTP session of a first offer/answer exchange
	// made through the handshaker, until onFinalize starts its RTCP monitor.
	unstartedRTPSession *media.RTPSession
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

	// jitterBuffer is the buffer WithAudioReaderJitterBuffer put between the
	// RTP session and RTPPacketReader, nil without one. A media update moves it
	// onto the new RTP session rather than taking it off the packet reader.
	jitterBuffer *media.RTPJitterBuffer

	// In case we are chaining audio readers
	audioReader io.Reader
	audioWriter io.Writer

	onClose       func() error
	onMediaUpdate func(*DialogMedia)

	// offerRequested is set while the peer's request has brought no session
	// description and so asks for our offer, until that offer is made. See
	// noteOfferRequest.
	offerRequested bool

	// stagedMediaSession is the fork that answers an offer of the peer's
	// which asks for a new DTLS association, from that offer until onFinalize
	// has keyed and installed it or the exchange is aborted. The media
	// session in place carries the media meanwhile.
	stagedMediaSession *media.MediaSession
	// mediaUpdating is set while an exchange waits, with mu released, for
	// the handshake of a new DTLS association. No other exchange is taken
	// meanwhile, see errMediaUpdateInProgress.
	mediaUpdating bool
	// discardErr is what closing a fork an abort discarded reported. The
	// abort has nowhere to return it, so Close does.
	discardErr error

	// offerNegotiatedCodecs is MediaConfig.ReInviteNegotiatedCodecs of the
	// config the media was built from. Set before the media is handed out
	// and not changed after.
	offerNegotiatedCodecs bool

	// releaseRTPPort returns this dialog RTP port to the allocator that handed
	// it out. Nil when no allocator is installed, as the OS takes the port back
	// when the socket closes. Set next to mediaSession and consumed by Close.
	releaseRTPPort func()

	// closed latches the first Close. closeDone is closed once that Close has
	// released the port, so a Close made after it returns only once the media
	// is released. Both are guarded by mu.
	closed    bool
	closeDone chan struct{}
}

var _ mediaHanshaker = (*DialogMedia)(nil)

func (d *DialogMedia) Close() error {
	// Any hook attached
	// Prevent double exec
	d.mu.Lock()
	if d.closed {
		done := d.closeDone
		d.mu.Unlock()
		// The first Close is still releasing the media. This one must not report
		// it closed before the release has run: a caller that acts on the close
		// (the port pool, the media's owner) would find the port still held.
		<-done
		return nil
	}
	d.closed = true
	// Deferred, so the port goes back once the sockets are closed below, never
	// before, and a later Close returns after that.
	releasePort := d.takeRTPPortReleaseUnsafe()
	defer releasePort()
	// A fork that was never installed and runs on sockets of its own is
	// closed with the rest.
	closeForks := d.takeUninstalledForksUnsafe()

	onClose := d.onClose
	d.onClose = nil
	m := d.mediaSession
	rtpSess := d.rtpSession
	jitter := d.jitterBuffer

	d.mu.Unlock()

	var e1, e2, e3 error
	if onClose != nil {
		e1 = onClose()
	}

	if rtpSess != nil {
		e2 = rtpSess.MonitorClose()
	}

	if m != nil {
		e3 = m.Close()
	}

	// The jitter buffer was closed with the hooks above, but its read loop may
	// still be in a read on the session. Closing the sockets fails that read,
	// and the loop has stopped reading them once this returns.
	e4 := awaitJitterBufferStopped(jitter)
	return errors.Join(e1, e2, e3, e4, closeForks())
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

func (d *DialogMedia) initMediaSessionFromConf(ctx context.Context, conf MediaConfig) error {
	d.offerNegotiatedCodecs = conf.ReInviteNegotiatedCodecs
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
	rtpPort, releasePort, err := conf.allocateRTPPort(ctx)
	if err != nil {
		return err
	}

	sess := &media.MediaSession{
		Codecs:     slices.Clone(conf.Codecs),
		Laddr:      net.UDPAddr{IP: bindIP, Port: rtpPort},
		ExternalIP: conf.externalIP,
		Mode:       sdp.ModeSendrecv,
		SecureRTP:  conf.secureRTP,
		SRTPAlg:    conf.SecureRTPAlg,
		RTPNAT:     conf.rtpNAT,
		DTLSConf:   conf.dtlsConfig(),
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

func (d *DialogMedia) setupRTPSession(sdp []byte, rtpSess *media.RTPSession) error {
	sess := rtpSess.Sess

	if err := sess.RemoteSDP(sdp, false); err != nil {
		return err
	}

	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	d.mu.Unlock()
	return nil
}

func (d *DialogMedia) handleMediaUpdate(remoteSDP []byte) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// When body is not present this can mean client is doing keep alive
	// Still offer needs to be responded
	if remoteSDP != nil {
		if err := d.sdpReInviteUnsafe(remoteSDP); err != nil {
			return nil, err
		}

		if d.onMediaUpdate != nil {
			d.mu.Unlock()
			d.onMediaUpdate(d)
			d.mu.Lock()
		}
	}

	// Reply with updated SDP
	return d.mediaSession.LocalSDP(true), nil
}

// Must be protected with lock
func (d *DialogMedia) sdpReInviteUnsafe(sdp []byte) error {
	if d.mediaSession == nil {
		return fmt.Errorf("no media session present")
	}

	if err := d.sdpUpdateUnsafe(sdp, false); err != nil {
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
	if d.mediaSession == nil {
		return fmt.Errorf("no media session present")
	}
	if err := d.mediaSession.RemoteSDP(remoteSDP, true); err != nil {
		return fmt.Errorf("sdp update media remote SDP applying failed: %w", err)
	}
	return nil
}

func (d *DialogMedia) sdpUpdateUnsafe(sdp []byte, offered bool) error {
	msess := d.mediaSession.Fork()
	if err := msess.RemoteSDP(sdp, offered); err != nil {
		return fmt.Errorf("sdp update media remote SDP applying failed: %w", err)
	}

	return d.replaceMediaSessionUnsafe(msess)
}

func (d *DialogMedia) mediaUpdateUnsafe(msess *media.MediaSession) error {
	return d.replaceMediaSessionUnsafe(msess)
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
	d.moveRTPReadsUnsafe(rtpSess)
	d.RTPPacketWriter.UpdateRTPSession(rtpSess)

	d.mediaSession = msess
	d.rtpSession = rtpSess
	return nil
}

func (d *DialogMedia) registerDialogCallbacks(c *dialogCallbacks) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.mediaHanshaker = d
	c.onMediaFailure = d.abortPendingMediaSession
	c.onClose = append(c.onClose, d.Close)
}

func (d *DialogMedia) onRemoteSDP(ctx context.Context, remoteSDP []byte, offered bool) error {
	if remoteSDP == nil {
		d.noteOfferRequest(offered)
		return nil
	}

	d.mu.Lock()
	if d.closed {
		// Close has taken what it closes already, and would not close what
		// this exchange installed.
		d.mu.Unlock()
		return errMediaClosed
	}
	if d.mediaUpdating {
		// An earlier exchange still waits for its keys.
		d.mu.Unlock()
		return errMediaUpdateInProgress
	}
	if d.mediaSession == nil {
		d.mu.Unlock()
		return errNoMediaSession
	}
	if !offered && d.awaitsFirstExchangeUnsafe() {
		// The call's first offer: there is nothing running to update.
		err := d.stageFirstOfferUnsafe(remoteSDP)
		d.mu.Unlock()
		return err
	}
	if offered && d.awaitsFirstAnswerUnsafe() {
		// Nothing is replaced and nothing was running, so this is no media
		// update.
		err := d.mediaSession.RemoteSDP(remoteSDP, true)
		d.mu.Unlock()
		return err
	}
	var err error
	// An offer that asks for a new DTLS association is only staged here, and
	// installed by onFinalize once it is keyed.
	installed := true
	if offered && d.pendingMediaSession != nil {
		pending := d.pendingMediaSession
		err = pending.RemoteSDP(remoteSDP, true)
		if err == nil {
			// Taken first: installing it may wait for a handshake with the
			// lock released.
			d.pendingMediaSession = nil
			err = d.installKeyedUnsafe(ctx, pending)
		}
	} else {
		installed, err = d.updateFromForkUnsafe(ctx, remoteSDP, offered)
	}
	onMediaUpdate := d.onMediaUpdate
	d.mu.Unlock()
	if err != nil {
		return err
	}
	if installed && onMediaUpdate != nil {
		onMediaUpdate(d)
	}
	return nil
}

func (d *DialogMedia) onLocalSDP(ctx context.Context, answered bool, mode string) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	requested := d.takeOfferRequestUnsafe()
	if d.closed {
		// Nothing is described, and for an offer of ours nothing forked, on
		// media that is closed.
		return nil, errMediaClosed
	}
	if d.mediaUpdating {
		// An earlier exchange still waits for its keys.
		return nil, errMediaUpdateInProgress
	}
	if d.mediaSession == nil {
		return nil, fmt.Errorf("dialog media is not initialized")
	}

	if answered {
		return d.answeringSessionUnsafe().LocalSDP(true), nil
	}

	if d.awaitsFirstExchangeUnsafe() {
		// The call's first offer is made by the session itself, not by a
		// fork of it, and its answer is applied there.
		return d.firstOfferUnsafe(mode), nil
	}

	ms := d.mediaSession.Fork()
	if mode != "" {
		ms.Mode = mode
	}
	d.pendingMediaSession = ms
	if requested {
		// This offer was asked for as the one of the session in place: it
		// renegotiates nothing.
		return negotiatedOffer(ms, d.mediaSession), nil
	}
	if mode == "" && d.offerNegotiatedCodecs {
		// An offer of ours for the session as it is, configured to
		// renegotiate nothing.
		return negotiatedOffer(ms, d.mediaSession), nil
	}
	return ms.LocalSDP(false), nil
}

func (d *DialogMedia) onFinalize(ctx context.Context) error {
	// A fork staged for a new DTLS association is keyed and installed now
	// that its answer is out.
	if staged, err := d.commitStagedMediaSession(ctx); staged {
		return err
	}

	d.mu.Lock()
	ms := d.mediaSession
	d.mu.Unlock()
	if ms == nil {
		return nil
	}
	if err := ms.FinalizeContext(ctx); err != nil {
		return err
	}
	return d.startFirstExchange()
}

type AudioReaderOption func(d *DialogMedia) error

type MediaProps struct {
	Codec media.Codec
	Laddr string
	Raddr string
}

func WithAudioReaderMediaProps(p *MediaProps) AudioReaderOption {
	return func(d *DialogMedia) error {
		if d.mediaSession == nil {
			return ErrNoMediaSetup
		}
		p.Codec = media.CodecAudioFromSession(d.mediaSession)
		p.Laddr = d.mediaSession.Laddr.String()
		p.Raddr = d.mediaSession.Raddr.String()
		return nil
	}
}

// WithAudioReaderJitterBuffer inserts an RTP jitter buffer before the payload reader.
// Playout starts at the packet duration of the negotiated audio codec and then
// follows the duration of the peer's packets, learned from their RTP timestamps
// in the codec's clock rate, or in opts.ClockRate when it is set.
// The buffer stays in place across media updates, reading each new RTP session.
// A dialog has at most one; asking for a second is an error.
// Before the media is set up the option returns ErrNoMediaSetup.
//
// A stop of the dialog's reads set with StopRTP stays in force when the buffer
// is put in.
func WithAudioReaderJitterBuffer(opts media.RTPJitterBufferOptions) AudioReaderOption {
	return func(d *DialogMedia) error {
		if d.mediaSession == nil || d.RTPPacketReader == nil {
			return ErrNoMediaSetup
		}
		if d.jitterBuffer != nil {
			return errors.New("jitter buffer already set up")
		}

		codec := media.CodecAudioFromSession(d.mediaSession)
		if codec.SampleDur <= 0 {
			return fmt.Errorf("invalid audio codec packet duration: %s", codec.SampleDur)
		}

		reader := d.RTPPacketReader.Reader()
		if reader == nil {
			return fmt.Errorf("no RTP reader setup")
		}

		if opts.ClockRate == 0 {
			opts.ClockRate = codec.SampleRate
		}
		jitter := media.NewRTPJitterBuffer(reader, codec.SampleDur, opts)
		d.RTPPacketReader.UpdateReader(jitter)

		// UpdateReader interrupts a potentially blocked RTPSession read. The new
		// jitter reader uses that same session, so restore normal network reading.
		// What is restored is the read deadline the application last asked for,
		// which is none unless it stopped the reads: that stop is not cleared.
		if session, ok := reader.(*media.RTPSession); ok {
			if err := session.Sess.RestoreRTPReadDeadline(); err != nil {
				d.RTPPacketReader.UpdateReader(reader)
				_ = jitter.Close()
				return fmt.Errorf("failed to start jitter buffer RTP reader: %w", err)
			}
		}

		d.jitterBuffer = jitter
		d.onCloseUnsafe(jitter.Close)
		return nil
	}
}

// WithAudioReaderRTPStats creates RTP Statistics interceptor on audio reader.
// The statistics are those of the dialog's current RTP session, which a media
// update replaces with one that carries them on. Without an RTP session the
// option returns ErrNoMediaSetup.
func WithAudioReaderRTPStats(hook media.OnRTPReadStats) AudioReaderOption {
	return func(d *DialogMedia) error {
		if d.rtpSession == nil {
			return ErrNoMediaSetup
		}
		d.audioReader = &rtpStatsReader{
			reader: d.getAudioReader(),
			dialog: d,
			hook:   hook,
		}
		return nil
	}
}

// WithAudioReaderDTMF creates DTMF interceptor
func WithAudioReaderDTMF(r *DTMFReader) AudioReaderOption {
	return func(d *DialogMedia) error {
		if err := d.initDTMFReaderUnsafe(r); err != nil {
			return err
		}
		d.audioReader = r
		return nil
	}
}

// WithAudioReaderMuter adds a controllable mute stage to the audio reader pipeline.
func WithAudioReaderMuter(muter *AudioMuterReader) AudioReaderOption {
	return func(d *DialogMedia) error {
		if muter == nil {
			return fmt.Errorf("audio reader muter is nil")
		}
		muter.Reader = d.getAudioReader()
		d.audioReader = muter
		return nil
	}
}

// WithAudioReaderStopper adds a controllable stop stage to the audio reader pipeline.
func WithAudioReaderStopper(stopper *AudioStopperReader) AudioReaderOption {
	return func(d *DialogMedia) error {
		if stopper == nil {
			return fmt.Errorf("audio reader stopper is nil")
		}
		stopper.Reader = d.getAudioReader()
		d.audioReader = stopper
		return nil
	}
}

func WithAudioReaderPCMMonitor(mon *audio.MonitorPCMReader, w io.Writer) AudioReaderOption {
	return func(d *DialogMedia) error {
		if d.mediaSession == nil {
			return ErrNoMediaSetup
		}
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
//
// Before the media is set up it returns ErrNoMediaSetup.
func (d *DialogMedia) AudioReader(opts ...AudioReaderOption) (io.Reader, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// An option works on the reader there is.
	if len(opts) > 0 && d.getAudioReader() == nil {
		return nil, ErrNoMediaSetup
	}
	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}
	r := d.getAudioReader()
	if r == nil {
		return nil, ErrNoMediaSetup
	}
	return r, nil
}

// getAudioReader returns the audio reader, or nil when there is none.
func (d *DialogMedia) getAudioReader() io.Reader {
	if d.audioReader != nil {
		return d.audioReader
	}
	if d.RTPPacketReader == nil {
		return nil
	}
	return d.RTPPacketReader
}

// audioReaderProps fills p from the media session and returns the audio
// reader. It returns nil when there is no reader, and without a media session,
// which describes the audio.
func (d *DialogMedia) audioReaderProps(p *MediaProps) io.Reader {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := WithAudioReaderMediaProps(p)(d); err != nil {
		return nil
	}
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
		if d.mediaSession == nil {
			return ErrNoMediaSetup
		}
		p.Codec = media.CodecAudioFromSession(d.mediaSession)
		p.Laddr = d.mediaSession.Laddr.String()
		p.Raddr = d.mediaSession.Raddr.String()
		return nil
	}
}

// WithAudioWriterRTPStats creates RTP Statistics interceptor on audio writer.
// The statistics are those of the dialog's current RTP session, which a media
// update replaces with one that carries them on. Without an RTP session the
// option returns ErrNoMediaSetup.
func WithAudioWriterRTPStats(hook media.OnRTPWriteStats) AudioWriterOption {
	return func(d *DialogMedia) error {
		if d.rtpSession == nil {
			return ErrNoMediaSetup
		}
		d.audioWriter = &rtpStatsWriter{
			writer: d.getAudioWriter(),
			dialog: d,
			hook:   hook,
		}
		return nil
	}
}

// WithAudioWriterDTMF adds DTMF into audio pipeline
func WithAudioWriterDTMF(r *DTMFWriter) AudioWriterOption {
	return func(d *DialogMedia) error {
		if err := d.initDTMFWriterUnsafe(r); err != nil {
			return err
		}
		d.audioWriter = r
		return nil
	}
}

// WithAudioWriterMuter adds a controllable mute stage to the audio writer pipeline.
func WithAudioWriterMuter(muter *AudioMuterWriter) AudioWriterOption {
	return func(d *DialogMedia) error {
		if muter == nil {
			return fmt.Errorf("audio writer muter is nil")
		}
		muter.Writer = d.getAudioWriter()
		d.audioWriter = muter
		return nil
	}
}

// WithAudioWriterStopper adds a controllable stop stage to the audio writer pipeline.
func WithAudioWriterStopper(stopper *AudioStopperWriter) AudioWriterOption {
	return func(d *DialogMedia) error {
		if stopper == nil {
			return fmt.Errorf("audio writer stopper is nil")
		}
		stopper.Writer = d.getAudioWriter()
		d.audioWriter = stopper
		return nil
	}
}

// WithAudioWriterComfortNoise adds explicit RFC 3389 comfort-noise writes to
// the audio pipeline. It does not advertise CN in SDP or schedule packets.
func WithAudioWriterComfortNoise(w *ComfortNoiseWriter) AudioWriterOption {
	return func(d *DialogMedia) error {
		if w == nil {
			return fmt.Errorf("comfort noise writer is nil")
		}
		w.comfortNoiseWriter = media.NewRTPComfortNoiseWriter(d.RTPPacketWriter, d.getAudioWriter())
		d.audioWriter = w
		return nil
	}
}

// WithAudioWriterMonitor initializes and adds PCM monitor in audio pipeline. It records and decodes stream into PCM.
func WithAudioWriterMonitor(mon *audio.MonitorPCMWriter, w io.Writer) AudioWriterOption {
	return func(d *DialogMedia) error {
		if d.mediaSession == nil {
			return ErrNoMediaSetup
		}
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
//
// Before the media is set up it returns ErrNoMediaSetup.
func (d *DialogMedia) AudioWriter(opts ...AudioWriterOption) (io.Writer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	// An option works on the writer there is.
	if len(opts) > 0 && d.getAudioWriter() == nil {
		return nil, ErrNoMediaSetup
	}
	for _, o := range opts {
		if err := o(d); err != nil {
			return nil, err
		}
	}

	w := d.getAudioWriter()
	if w == nil {
		return nil, ErrNoMediaSetup
	}
	return w, nil
}

// getAudioWriter returns the audio writer, or nil when there is none.
func (d *DialogMedia) getAudioWriter() io.Writer {
	if d.audioWriter != nil {
		return d.audioWriter
	}
	if d.RTPPacketWriter == nil {
		return nil
	}
	return d.RTPPacketWriter
}

// audioWriterProps fills p from the media session and returns the audio
// writer. It returns nil when there is no writer, and without a media session,
// which describes the audio.
func (d *DialogMedia) audioWriterProps(p *MediaProps) io.Writer {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := WithAudioWriterMediaProps(p)(d); err != nil {
		return nil
	}
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
		return AudioPlayback{}, ErrNoMediaSetup
	}
	p := NewAudioPlayback(w, mprops.Codec)
	// On each play it needs reset RTP timestamp
	d.mu.Lock()
	packetWriter := d.RTPPacketWriter
	d.mu.Unlock()
	if packetWriter != nil {
		p.onPlay = packetWriter.ResetTimestamp
	}
	return p, nil
}

// PlaybackControlCreate creates playback for audio with controls like mute unmute
func (d *DialogMedia) PlaybackControlCreate() (AudioPlaybackControl, error) {
	p, err := d.PlaybackCreate()
	if err != nil {
		return AudioPlaybackControl{}, err
	}
	// Audio is controled via audio reader/writer
	control := NewAudioPlaybackControl(p)
	return control, nil
}

// PlaybackRingtoneCreate is creating playback for ringtone
//
// Experimental
func (d *DialogMedia) PlaybackRingtoneCreate() (AudioRingtone, error) {
	mprops := MediaProps{}
	w := d.audioWriterProps(&mprops)
	if w == nil {
		return AudioRingtone{}, ErrNoMediaSetup
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
		writer:     &encoder,
		ringtone:   ringtone,
		sampleSize: mprops.Codec.Samples16(),
		dialog:     d,
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
		return AudioStereoRecordingWav{}, ErrNoMediaSetup
	}

	mpropsR := MediaProps{}
	ar := d.audioReaderProps(&mpropsR)
	if ar == nil {
		return AudioStereoRecordingWav{}, ErrNoMediaSetup
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
		if err := d.StopRTP(1, 0); err != nil {
			return err
		}
		wg.Wait() // This makes sure we have exited reading
		if err := d.StartRTP(1, 0); err != nil {
			return err
		}
		return readErr
	}, nil
}

// ListenContext listens until context is canceled.
//
// The context's end stops the read with a read deadline, which is cleared
// again before ListenContext returns, and ListenContext then returns nil.
func (d *DialogMedia) ListenContext(pctx context.Context) (err error) {
	buf := make([]byte, media.RTPBufSize)
	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}
	restoreReads := d.stopReadsAtEnd(pctx)
	defer func() {
		if restoreErr := restoreReads(); err == nil {
			err = restoreErr
		}
	}()
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

// ListenUntil listens for dur, and returns the read deadline's error when dur
// has passed. The read deadline is cleared again before it returns.
func (d *DialogMedia) ListenUntil(dur time.Duration) (err error) {
	buf := make([]byte, media.RTPBufSize)

	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}
	if err := d.StopRTP(1, dur); err != nil {
		return err
	}
	defer func() {
		if startErr := d.StartRTP(1, 0); startErr != nil && errors.Is(err, os.ErrDeadlineExceeded) {
			err = errors.Join(err, startErr)
		}
	}()
	for {
		_, err := audioReader.Read(buf)
		if err != nil {
			return err
		}
	}
}

// StopRTP sets a read or write deadline, as MediaSession.StopRTP does, on the
// dialog's current media session. It holds the dialog's lock while it does,
// since a media update replaces the session under it, so the deadline is set
// either on the session replaced, which hands it to its replacement, or on the
// replacement. Without a media session it returns ErrNoMediaSetup.
func (d *DialogMedia) StopRTP(rw int8, dur time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mediaSession == nil {
		return ErrNoMediaSetup
	}
	return d.mediaSession.StopRTP(rw, dur)
}

// StartRTP clears the deadline StopRTP sets, on the dialog's current media
// session. Without a media session it returns ErrNoMediaSetup.
func (d *DialogMedia) StartRTP(rw int8, dur time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mediaSession == nil {
		return ErrNoMediaSetup
	}
	return d.mediaSession.StartRTP(rw)
}

type rtpDTMFDeadline interface {
	StopRTP(rw int8, dur time.Duration) error
	StartRTP(rw int8) error
}

type DTMFReader struct {
	rtpDeadline rtpDTMFDeadline
	dtmfReader  *media.RTPDtmfReader
	onDTMF      func(dtmf rune) error
	// follow is set on a reader of a DialogMedia, which then reads DTMF on
	// the telephone-event payload type of the dialog's current media session.
	follow *dtmfMediaFollow
}

func (d *DialogMedia) dtmfCodec(receive bool) (media.Codec, error) {
	if d.mediaSession == nil {
		return media.Codec{}, ErrNoMediaSetup
	}
	for _, codec := range d.mediaSession.CommonCodecs() {
		if !codec.Matches(media.CodecTelephoneEvent8000) {
			continue
		}
		if receive {
			codec.PayloadType = codec.PayloadTypeRecv()
		}
		return codec, nil
	}
	return dtmfCodecWithoutNegotiated8000(d.mediaSession, receive), nil
}

// AudioReaderDTMF is DTMF over RTP. It reads audio and provides hook for dtmf while listening for audio
// Use Listen or OnDTMF after this call
func (m *DialogMedia) AudioReaderDTMF() (*DTMFReader, error) {
	// Under the lock: a media update replaces the media session under it.
	m.mu.Lock()
	defer m.mu.Unlock()
	r := &DTMFReader{}
	if err := m.initDTMFReaderUnsafe(r); err != nil {
		return nil, err
	}
	return r, nil
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

// readDeadline reads once, with a read deadline dur away on the media when dur
// is set, and clears that deadline again, so the next read of the media does
// not fail on it.
func (d *DTMFReader) readDeadline(buf []byte, dur time.Duration) (n int, err error) {
	mediaSession := d.rtpDeadline
	if dur > 0 {
		// Stop RTP
		if err := mediaSession.StopRTP(1, dur); err != nil {
			return 0, err
		}
		defer func() {
			if startErr := mediaSession.StartRTP(1); startErr != nil && err == nil {
				err = startErr
			}
		}()
	}
	return d.Read(buf)
}

// OnDTMF must be called before audio reading
func (d *DTMFReader) OnDTMF(onDTMF func(dtmf rune) error) {
	d.onDTMF = onDTMF
}

// Read exposes io.Reader that can be used as AudioReader
func (d *DTMFReader) Read(buf []byte) (n int, err error) {
	d.followMedia()
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
	dtmfWriter *media.RTPDtmfWriter
	// follow is set on a writer of a DialogMedia, which then writes DTMF on
	// the telephone-event payload type of the dialog's current media session.
	follow *dtmfMediaFollow
}

func (m *DialogMedia) AudioWriterDTMF() (*DTMFWriter, error) {
	// Under the lock: a media update replaces the media session under it.
	m.mu.Lock()
	defer m.mu.Unlock()
	w := &DTMFWriter{}
	if err := m.initDTMFWriterUnsafe(w); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *DTMFWriter) WriteDTMF(dtmf rune) error {
	w.followMedia()
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

// ComfortNoiseWriter exposes explicit comfort-noise writes while remaining an
// io.Writer for ordinary encoded audio.
type ComfortNoiseWriter struct {
	comfortNoiseWriter *media.RTPComfortNoiseWriter
}

// WriteComfortNoise sends one PT 13 comfort-noise packet. Level is attenuation
// in -dBov and must be between 0 and 127.
func (w *ComfortNoiseWriter) WriteComfortNoise(level uint8) error {
	if w.comfortNoiseWriter == nil {
		return fmt.Errorf("comfort noise writer is not initialized")
	}
	return w.comfortNoiseWriter.WriteComfortNoise(level)
}

// Write forwards ordinary encoded audio through the configured pipeline.
func (w *ComfortNoiseWriter) Write(buf []byte) (n int, err error) {
	if w.comfortNoiseWriter == nil {
		return 0, fmt.Errorf("comfort noise writer is not initialized")
	}
	return w.comfortNoiseWriter.Write(buf)
}
