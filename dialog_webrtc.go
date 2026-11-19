// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2026, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/emiago/diago/media"
)

const webrtcFailureCleanupTimeout = 5 * time.Second

// DialogWebrtc exposes the direct ICE + DTLS-SRTP media stack negotiated
// through SIP. It is independent from DialogWebrtcPion, which uses a Pion
// PeerConnection and remains available for browser-oriented calls.
type DialogWebrtc struct {
	mu sync.Mutex

	mediaSession        *media.MediaSessionWebrtc
	rtpSession          *media.RTPSessionWebrtc
	pendingMediaSession *media.MediaSessionWebrtc

	// RTPPacketReader is the default RTP payload reader. Prefer AudioReader when
	// an application may install media interceptors.
	RTPPacketReader *media.RTPPacketReader
	// RTPPacketWriter is the default RTP packetizer. Prefer AudioWriter when an
	// application may install media interceptors.
	RTPPacketWriter *media.RTPPacketWriter

	audioReader io.Reader
	audioWriter io.Writer
	onClose     func() error
	closed      bool

	dialogWebrtcState
}

func (d *DialogWebrtc) Init(sess *media.MediaSessionWebrtc, rtpSess *media.RTPSessionWebrtc) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.mediaSession = sess
	d.rtpSession = rtpSess
	codec := sess.Codec()
	d.RTPPacketReader = media.NewRTPPacketReader(rtpSess, codec)
	d.RTPPacketWriter = media.NewRTPPacketWriter(rtpSess, codec)
}

// MediaSession returns the direct WebRTC transport session.
func (d *DialogWebrtc) MediaSession() *media.MediaSessionWebrtc {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.mediaSession
}

// RTPSession returns the RTP/RTCP statistics session.
func (d *DialogWebrtc) RTPSession() *media.RTPSessionWebrtc {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.rtpSession
}

// OnClose adds a cleanup hook. Hooks run once when Close is first called.
func (d *DialogWebrtc) OnClose(f func() error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if f == nil {
		return
	}
	if d.onClose == nil {
		d.onClose = f
		return
	}
	previous := d.onClose
	d.onClose = func() error { return errors.Join(previous(), f()) }
}

// Close stops RTCP monitoring and closes ICE, DTLS and SRTP resources.
func (d *DialogWebrtc) Close() error {
	d.mu.Lock()
	if d.closed {
		done := d.closeDone
		d.mu.Unlock()
		// The first Close is still releasing the media: this one reports it
		// closed only once the release has run.
		<-done
		return nil
	}
	done := make(chan struct{})
	d.closeDone = done
	defer close(done)
	rtpSess := d.rtpSession
	sess := d.mediaSession
	pendingSess := d.pendingMediaSession
	jitter := d.jitterBuffer

	d.closed = true
	onClose := d.onClose
	d.onClose = nil

	d.pendingMediaSession = nil
	d.mu.Unlock()

	var hookErr, jitterErr, rtpErr, mediaErr, pendingErr error
	if onClose != nil {
		hookErr = onClose()
	}
	if jitter != nil {
		jitterErr = jitter.Close()
	}
	if rtpSess != nil {
		rtpErr = rtpSess.MonitorClose()
	}
	if sess != nil {
		mediaErr = sess.Close()
	}
	if pendingSess != nil && pendingSess != sess {
		pendingErr = pendingSess.Close()
	}
	// The sockets are closed, which fails the read the buffer's loop sits in.
	return errors.Join(hookErr, jitterErr, rtpErr, mediaErr, pendingErr, awaitJitterBufferStopped(jitter))
}

func (d *DialogWebrtc) registerDialogCallbacks(c *dialogCallbacks) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.mediaHanshaker = d
	c.onMediaFailure = d.abortPendingMediaSession
}

func (d *DialogWebrtc) onLocalSDP(ctx context.Context, answered bool, mode string) ([]byte, error) {
	d.mu.Lock()
	current := d.mediaSession
	pending := d.pendingMediaSession
	closed := d.closed
	d.mu.Unlock()
	if closed {
		// Nothing is described, and for an offer of ours nothing forked, on
		// media that is closed.
		return nil, errMediaClosed
	}
	if current == nil {
		return nil, fmt.Errorf("dialog WebRTC media is not initialized")
	}
	if answered {
		if pending == nil {
			return nil, fmt.Errorf("dialog WebRTC re-INVITE has no pending offer")
		}
		return pending.LocalSDP(ctx, true)
	}

	fork, err := current.Fork(ctx)
	if err != nil {
		return nil, err
	}
	if mode != "" {
		fork.Mode = mode
	}
	if mode == "" && d.offersNegotiatedCodecs() {
		offerNegotiatedCodecs(fork, current)
	}
	if err := d.setPendingMediaSession(fork); err != nil {
		return nil, err
	}
	localSDP, err := fork.LocalSDP(ctx, false)
	if err != nil {
		d.abortPendingMediaSession()
		return nil, err
	}
	return localSDP, nil
}

func (d *DialogWebrtc) onRemoteSDP(ctx context.Context, remoteSDP []byte, offered bool) error {
	if remoteSDP == nil {
		return nil
	}

	d.mu.Lock()
	current := d.mediaSession
	pending := d.pendingMediaSession
	closed := d.closed
	d.mu.Unlock()
	if closed {
		// Close has taken what it closes already, and would not close what
		// this exchange set up.
		return errMediaClosed
	}
	if current == nil {
		return fmt.Errorf("dialog WebRTC media is not initialized")
	}

	if offered {
		if pending == nil {
			return fmt.Errorf("dialog WebRTC re-INVITE has no pending local offer")
		}
		if err := pending.RemoteSDP(ctx, remoteSDP, true); err != nil {
			d.abortPendingMediaSession()
			return err
		}
		if err := pending.Finalize(ctx); err != nil {
			d.abortPendingMediaSession()
			return err
		}
		if pending == current {
			return d.commitCurrentMediaSession()
		}
		return d.replaceMediaSession(pending)
	}

	// A browser may re-offer codec or direction changes without restarting
	// ICE. MediaSessionWebrtc stages those changes on the current transport.
	// Changed credentials require a fresh ICE/DTLS/SRTP transport instead.
	if err := current.RemoteSDP(ctx, remoteSDP, false); err == nil {
		return d.setPendingMediaSession(current)
	} else if !errors.Is(err, media.ErrWebRTCICERestart) {
		return err
	}

	fork, err := current.Fork(ctx)
	if err != nil {
		return err
	}
	if err = fork.RemoteSDP(ctx, remoteSDP, false); err != nil {
		_ = fork.Close()
		return err
	}
	return d.setPendingMediaSession(fork)
}

func (d *DialogWebrtc) onFinalize(ctx context.Context) error {
	d.mu.Lock()
	pending := d.pendingMediaSession
	current := d.mediaSession
	d.mu.Unlock()
	if pending == nil {
		return nil
	}
	if err := pending.Finalize(ctx); err != nil {
		d.abortPendingMediaSession()
		return err
	}
	if pending == current {
		return d.commitCurrentMediaSession()
	}
	return d.replaceMediaSession(pending)
}

// setPendingMediaSession stores sess as the transport the exchange in progress
// commits. The store checks for Close under the same lock: a Close made since
// the exchange found the media open has taken what it closes already, so sess
// is refused with errMediaClosed and closed rather than left open for good.
func (d *DialogWebrtc) setPendingMediaSession(sess *media.MediaSessionWebrtc) error {
	d.mu.Lock()
	beforeStore := d.beforePendingStore
	d.mu.Unlock()
	if beforeStore != nil {
		beforeStore(sess)
	}
	d.mu.Lock()
	if d.closed {
		current := d.mediaSession
		d.mu.Unlock()
		return refusePendingOnClosed(sess, current)
	}
	previous := d.pendingMediaSession
	d.pendingMediaSession = sess
	current := d.mediaSession
	d.mu.Unlock()
	if previous != nil && previous != current && previous != sess {
		_ = previous.Close()
	}
	return nil
}

func (d *DialogWebrtc) abortPendingMediaSession() {
	d.mu.Lock()
	pending := d.pendingMediaSession
	current := d.mediaSession
	d.pendingMediaSession = nil
	d.mu.Unlock()
	if pending == current {
		current.Rollback()
	} else if pending != nil {
		_ = pending.Close()
	}
}

func (d *DialogWebrtc) commitCurrentMediaSession() error {
	d.mu.Lock()
	d.pendingMediaSession = nil
	if d.RTPPacketWriter != nil && d.rtpSession != nil && d.mediaSession != nil {
		if err := d.RTPPacketWriter.UpdateWriter(d.rtpSession, d.mediaSession.Codec()); err != nil {
			d.mu.Unlock()
			return fmt.Errorf("dialog WebRTC media update: %w", err)
		}
	}
	d.followInPlaceUpdateUnsafe()
	d.noteInstallUnsafe()
	d.mu.Unlock()
	d.mediaInstalled()
	return nil
}

func (d *DialogWebrtc) replaceMediaSession(sess *media.MediaSessionWebrtc) error {
	d.mu.Lock()
	oldMediaSession := d.mediaSession
	oldRTPSession := d.rtpSession
	reader := d.RTPPacketReader
	writer := d.RTPPacketWriter
	closed := d.closed
	d.mu.Unlock()
	if closed || oldMediaSession == nil || oldRTPSession == nil || reader == nil || writer == nil {
		_ = sess.Close()
		return fmt.Errorf("dialog WebRTC media is not available for replacement")
	}

	rtpSession := oldRTPSession.Fork(sess)
	if err := rtpSession.MonitorBackground(); err != nil {
		_ = sess.Close()
		return err
	}

	d.mu.Lock()
	if d.closed || d.pendingMediaSession != sess {
		d.mu.Unlock()
		_ = rtpSession.MonitorClose()
		_ = sess.Close()
		return fmt.Errorf("dialog WebRTC media replacement was canceled")
	}
	// The deadlines StopRTP and StartRTP last set go first: moving the packet
	// reader ends a read waiting on the replaced transport, and that read is
	// taken up again on the new one with the deadline it holds by then.
	if err := sess.CarryRTPDeadlines(oldMediaSession); err != nil {
		d.pendingMediaSession = nil
		d.mu.Unlock()
		return errors.Join(fmt.Errorf("carrying RTP deadlines to the new media session: %w", err), rtpSession.MonitorClose(), sess.Close())
	}
	// The writer refuses a codec it cannot pace before anything has moved,
	// so the call stays on the session it runs on.
	if err := writer.UpdateWriter(rtpSession, sess.Codec()); err != nil {
		d.pendingMediaSession = nil
		d.mu.Unlock()
		return errors.Join(fmt.Errorf("dialog WebRTC media replacement: %w", err), rtpSession.MonitorClose(), sess.Close())
	}
	d.moveRTPReadsUnsafe(rtpSession)
	d.mediaSession = sess
	d.rtpSession = rtpSession
	d.pendingMediaSession = nil
	d.noteInstallUnsafe()
	d.mu.Unlock()

	err := errors.Join(oldRTPSession.MonitorClose(), oldMediaSession.Close())
	d.mediaInstalled()
	return err
}

type AudioReaderWebrtcOption func(*DialogWebrtc) error

// WithAudioReaderWebrtcProps returns the negotiated codec and ICE pair.
func WithAudioReaderWebrtcProps(props *MediaProps) AudioReaderWebrtcOption {
	return func(d *DialogWebrtc) error {
		if props == nil {
			return fmt.Errorf("media props are nil")
		}
		if d.mediaSession == nil {
			return ErrNoMediaSetup
		}
		props.Codec = d.mediaSession.Codec()
		props.Laddr = d.mediaSession.Laddr
		props.Raddr = d.mediaSession.Raddr
		return nil
	}
}

func WithAudioReaderWebrtcDTMF(reader *DTMFReader) AudioReaderWebrtcOption {
	return func(d *DialogWebrtc) error {
		if reader == nil {
			return fmt.Errorf("DTMF reader is nil")
		}
		if d.mediaSession == nil || d.RTPPacketReader == nil {
			return ErrNoMediaSetup
		}
		codec, err := d.dtmfCodecUnsafe(true)
		if err != nil {
			return err
		}
		reader.dtmfReader = media.NewRTPDTMFReader(
			codec,
			d.RTPPacketReader,
			d.audioReaderUnsafe(),
		)
		reader.rtpDeadline = dialogRTPDeadline{dialog: d}
		reader.follow = &dtmfMediaFollow[uint64]{dialog: d, receive: true, target: reader.dtmfReader, installed: d.installs}
		d.audioReader = reader
		return nil
	}
}

// AudioReader returns an encoded audio payload reader.
func (d *DialogWebrtc) AudioReader(opts ...AudioReaderWebrtcOption) (io.Reader, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mediaSession == nil || d.RTPPacketReader == nil {
		return nil, ErrNoMediaSetup
	}
	for _, opt := range opts {
		if err := opt(d); err != nil {
			return nil, err
		}
	}
	return d.audioReaderUnsafe(), nil
}

func (d *DialogWebrtc) audioReaderUnsafe() io.Reader {
	if d.audioReader != nil {
		return d.audioReader
	}
	return d.RTPPacketReader
}

func (d *DialogWebrtc) SetAudioReader(reader io.Reader) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audioReader = reader
}

type AudioWriterWebrtcOption func(*DialogWebrtc) error

// WithAudioWriterWebrtcProps returns the negotiated codec and ICE pair.
func WithAudioWriterWebrtcProps(props *MediaProps) AudioWriterWebrtcOption {
	return func(d *DialogWebrtc) error {
		if props == nil {
			return fmt.Errorf("media props are nil")
		}
		if d.mediaSession == nil {
			return ErrNoMediaSetup
		}
		props.Codec = d.mediaSession.Codec()
		props.Laddr = d.mediaSession.Laddr
		props.Raddr = d.mediaSession.Raddr
		return nil
	}
}

func WithAudioWriterWebrtcDTMF(writer *DTMFWriter) AudioWriterWebrtcOption {
	return func(d *DialogWebrtc) error {
		if writer == nil {
			return fmt.Errorf("DTMF writer is nil")
		}
		if d.RTPPacketWriter == nil {
			return ErrNoMediaSetup
		}
		codec, err := d.dtmfCodecUnsafe(false)
		if err != nil {
			return err
		}
		writer.dtmfWriter = media.NewRTPDTMFWriter(
			codec,
			d.RTPPacketWriter,
			d.audioWriterUnsafe(),
		)
		writer.follow = &dtmfMediaFollow[uint64]{dialog: d, target: writer.dtmfWriter, installed: d.installs}
		d.audioWriter = writer
		return nil
	}
}

// AudioWriter returns an encoded audio payload writer.
func (d *DialogWebrtc) AudioWriter(opts ...AudioWriterWebrtcOption) (io.Writer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mediaSession == nil || d.RTPPacketWriter == nil {
		return nil, ErrNoMediaSetup
	}
	for _, opt := range opts {
		if err := opt(d); err != nil {
			return nil, err
		}
	}
	return d.audioWriterUnsafe(), nil
}

func (d *DialogWebrtc) audioWriterUnsafe() io.Writer {
	if d.audioWriter != nil {
		return d.audioWriter
	}
	return d.RTPPacketWriter
}

func (d *DialogWebrtc) SetAudioWriter(writer io.Writer) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.audioWriter = writer
}

func (d *DialogWebrtc) AudioReaderDTMF() (*DTMFReader, error) {
	reader := &DTMFReader{}
	_, err := d.AudioReader(WithAudioReaderWebrtcDTMF(reader))
	return reader, err
}

func (d *DialogWebrtc) AudioWriterDTMF() (*DTMFWriter, error) {
	writer := &DTMFWriter{}
	_, err := d.AudioWriter(WithAudioWriterWebrtcDTMF(writer))
	return writer, err
}

// Echo copies received encoded audio back to the remote peer.
func (d *DialogWebrtc) Echo() error {
	reader, err := d.AudioReader()
	if err != nil {
		return err
	}
	writer, err := d.AudioWriter()
	if err != nil {
		return err
	}
	_, err = media.Copy(reader, writer)
	return err
}

// PlaybackCreate creates playback over the negotiated WebRTC RTP writer.
func (d *DialogWebrtc) PlaybackCreate() (AudioPlayback, error) {
	props := MediaProps{}
	writer, err := d.AudioWriter(WithAudioWriterWebrtcProps(&props))
	if err != nil {
		return AudioPlayback{}, err
	}
	if props.Codec.SampleRate == 0 {
		return AudioPlayback{}, fmt.Errorf("no codec defined")
	}
	playback := NewAudioPlayback(writer, props.Codec)
	playback.onPlay = d.RTPPacketWriter.ResetTimestamp
	return playback, nil
}

// AudioStereoRecordingCreate creates a stereo WAV recording pipeline. The
// caller must use the returned recording's reader and writer and call Close.
func (d *DialogWebrtc) AudioStereoRecordingCreate(wavFile *os.File) (AudioStereoRecordingWav, error) {
	readerProps, writerProps := MediaProps{}, MediaProps{}
	reader, err := d.AudioReader(WithAudioReaderWebrtcProps(&readerProps))
	if err != nil {
		return AudioStereoRecordingWav{}, err
	}
	writer, err := d.AudioWriter(WithAudioWriterWebrtcProps(&writerProps))
	if err != nil {
		return AudioStereoRecordingWav{}, err
	}
	return newDialogRecordingWav(wavFile, reader, readerProps, writer, writerProps)
}
