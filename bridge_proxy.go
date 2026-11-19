// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/emiago/diago/media"
)

// errBridgeNoTranscoding is the error of two medias a Bridge can not proxy to
// each other because their codecs differ: it passes payload on as it is.
var errBridgeNoTranscoding = errors.New("no transcoding supported in bridge")

// bridgeCodecErr returns why a Bridge can not proxy audio in codec c1 to media
// in codec c2, and back, or nil when it can.
func bridgeCodecErr(c1, c2 media.Codec) error {
	if c1 != c2 {
		return fmt.Errorf("%w codec1=%+v codec2=%+v", errBridgeNoTranscoding, c1, c2)
	}
	return nil
}

// bridgeMediaSource is what a Bridge keeps of the DialogMedia a
// BridgeAudioMedia was taken from by AddDialogMedia. It is zero for audio
// added any other way.
type bridgeMediaSource struct {
	// dialogMedia is told of media updates, and holds the codec in use.
	dialogMedia *DialogMedia
	// dtmfReader and dtmfWriter are the DTMF interceptors of a bridge that
	// passes DTMF. They read and write the dialog's audio and are not set on
	// the dialog.
	dtmfReader *DTMFReader
	dtmfWriter *DTMFWriter
}

// bridgeDTMFReader sets r up as WithAudioReaderDTMF does, over the dialog's
// audio reader, and leaves the dialog that reader.
func bridgeDTMFReader(r *DTMFReader) AudioReaderOption {
	return func(d *DialogMedia) error {
		reader := d.audioReader
		defer func() { d.audioReader = reader }()
		return WithAudioReaderDTMF(r)(d)
	}
}

// bridgeDTMFWriter sets w up as WithAudioWriterDTMF does, over the dialog's
// audio writer, and leaves the dialog that writer.
func bridgeDTMFWriter(w *DTMFWriter) AudioWriterOption {
	return func(d *DialogMedia) error {
		writer := d.audioWriter
		defer func() { d.audioWriter = writer }()
		return WithAudioWriterDTMF(w)(d)
	}
}

// passDTMF has the keys read on from sent to the DTMF writer of to, for the
// proxy about to start between them. A party without a DTMF writer of the
// bridge's is sent none.
func passDTMF(from, to *BridgeAudioMedia) {
	r := from.source.dtmfReader
	if r == nil {
		return
	}
	w := to.source.dtmfWriter
	if w == nil {
		r.OnDTMF(func(rune) error { return nil })
		return
	}
	r.OnDTMF(w.WriteDTMF)
}

// codec returns the codec the media runs on: the one of its dialog's current
// media session, which a media update replaces, and for audio added any other
// way the one it joined with.
func (m *BridgeAudioMedia) codec() media.Codec {
	dm := m.source.dialogMedia
	if dm == nil {
		return m.ReaderProps.Codec
	}
	p := MediaProps{}
	dm.audioReaderProps(&p)
	return p.Codec
}

// watchBridgeCodecs reports on mismatch, once, when the codecs of m1 and m2
// differ: payload is passed on as it is, so a proxy between them ends once a
// re-INVITE moves either to a codec the other does not have. The codecs are
// checked after every media update of either, and once here, for a re-INVITE
// since they joined. The caller calls unwatch when its proxy has ended.
func watchBridgeCodecs(m1, m2 *BridgeAudioMedia) (mismatch <-chan error, unwatch func()) {
	found := make(chan error, 1)
	check := func() {
		if err := bridgeCodecErr(m1.codec(), m2.codec()); err != nil {
			select {
			case found <- err:
			default:
			}
		}
	}
	var removes []func()
	for _, m := range []*BridgeAudioMedia{m1, m2} {
		if dm := m.source.dialogMedia; dm != nil {
			removes = append(removes, dm.addMediaUpdateHook(check))
		}
	}
	check()
	return found, func() {
		for _, remove := range removes {
			remove()
		}
	}
}

// proxyWait waits for the two directions of a proxy between medias, which
// send their results to errCh, until either direction ends, as it does once
// the media it reads or writes is closed, ctx ends or mismatch reports that
// the codecs differ. A direction still reading would take the next frame from
// whoever reads that media next, so proxyWait then ends both. It returns once
// both have ended and the medias can be read again.
//
// The end of ctx is how a proxy is stopped and is not reported as an error.
func proxyWait(ctx context.Context, medias []*BridgeAudioMedia, errCh <-chan error, mismatch <-chan error) error {
	var err error
	running := 2
	select {
	case err = <-errCh:
		running--
	case err = <-mismatch:
	case <-ctx.Done():
	}

	// A deadline ends a direction without an error, also while its media is
	// silent. It is cleared once both have ended.
	var stopErr error
	for _, m := range medias {
		stopErr = errors.Join(stopErr, bridgeRTPControl(m.StopRTP))
	}
	for ; running > 0; running-- {
		err = errors.Join(err, <-errCh)
	}
	var startErr error
	for _, m := range medias {
		startErr = errors.Join(startErr, bridgeRTPControl(m.StartRTP))
	}
	return errors.Join(err, stopErr, startErr)
}

// bridgeRTPControl runs control, the StopRTP or StartRTP of a media in a
// bridge, and returns its error, see bridgeRTPControlErr. Audio added without
// one has no read to stop or start.
func bridgeRTPControl(control func() error) error {
	if control == nil {
		return nil
	}
	return bridgeRTPControlErr(control())
}

// bridgeRTPControlErr returns the error of starting or stopping reads on a
// media, or nil when its RTP connection is closed. A BYE closes a dialog's
// media before its handler leaves the bridge, so that is the usual state of a
// dialog that hung up: its reads already fail, there is no read to start or
// stop, and it must still be able to leave.
func bridgeRTPControlErr(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// bridgeProxy is a proxy running in the background, and its stop.
type bridgeProxy struct {
	// cancel ends the context the proxy runs on, which stops it
	cancel context.CancelFunc
	// done is closed once the proxy has stopped, and err set before
	done chan struct{}
	err  error
}

func newBridgeProxy(cancel context.CancelFunc) *bridgeProxy {
	return &bridgeProxy{cancel: cancel, done: make(chan struct{})}
}

// stop stops the proxy, waits until it has stopped, and returns what it ended
// with.
func (p *bridgeProxy) stop() error {
	p.cancel()
	<-p.done
	return p.err
}

// proxyEndedWithMedia reports whether err is how a proxy ends when a call
// does: a hangup closes a dialog's media, which ends the proxy's read of it
// and fails a write to it. A proxy that was stopped ends without an error.
func proxyEndedWithMedia(err error) bool {
	if errors.Is(err, errBridgeNoTranscoding) {
		return false
	}
	return err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed)
}

// StopProxyMedia stops the proxy the bridge started when its second media
// joined and waits until it has stopped reading and writing the medias. It
// returns what the proxy ended with, and nil when no proxy was started.
//
// The proxy stops on its own once the media of either party is closed, as a
// call's end closes it. Until it has stopped it can take the next frame of the
// party left, so call StopProxyMedia before reading that party. It can be
// called any number of times.
func (b *Bridge) StopProxyMedia() error {
	b.mu.Lock()
	proxy := b.proxy
	b.mu.Unlock()
	if proxy == nil {
		return nil
	}
	return proxy.stop()
}

// proxyParties returns the two medias a proxy started by hand runs between, or
// why it can not be started.
func (b *Bridge) proxyParties() (m1, m2 *BridgeAudioMedia, err error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.dialogs) < 2 {
		return nil, nil, fmt.Errorf("number of dialogs must equal to 2")
	}
	if len(b.dialogs) > 2 {
		return nil, nil, fmt.Errorf("proxy media requieres minimum 2 parties")
	}
	// Check are both answered
	for _, m := range b.dialogs {
		if m.Reader == nil || m.Writer == nil {
			return nil, nil, fmt.Errorf("dialog session not answered?")
		}
	}
	return b.dialogs[0], b.dialogs[1], nil
}

// ProxyMediaControl starts the proxy in the background and returns the
// function that stops it. The proxy also stops on its own once the media of
// either party is closed, once ctx ends, and once a re-INVITE moves either
// party to a codec the other does not have. The stop waits until the proxy has
// stopped reading and writing the medias and returns what it ended with: nil
// for a proxy that was stopped or whose ctx ended. It can be called any number
// of times.
//
// It requires WaitDialogsNum higher than 2, so the bridge starts no proxy of
// its own on the same medias, which the stop would not stop.
//
// Experimental
func (b *Bridge) ProxyMediaControl(ctx context.Context) (stop func() error, err error) {
	m1, m2, err := b.proxyParties()
	if err != nil {
		return nil, err
	}
	if b.WaitDialogsNum < 3 {
		return nil, fmt.Errorf("you are already running proxy media. Increase WaitDialogsNum")
	}

	ctx, cancel := context.WithCancel(ctx)
	proxy := newBridgeProxy(cancel)
	go func() {
		defer close(proxy.done)
		defer cancel()
		proxy.err = b.proxyMediaChannels(ctx, m1, m2)
	}()
	return proxy.stop, nil
}
