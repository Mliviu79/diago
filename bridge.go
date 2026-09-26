// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"sync"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
)

type Bridger interface {
	AddDialogSession(d DialogSession) error
}

// Bridge proxies the audio of two dialogs to each other. It is safe for
// concurrent use once initialized, and is not copied after that.
type Bridge struct {
	// Originator is dialog session that created bridge. AddDialogSession sets
	// it, under the bridge's lock, when the first dialog joins.
	Originator DialogSession
	// DTMFpass is also dtmf pipeline and proxy. By default only audio media is proxied
	// NOTE: this may not work if you are already processing DTMF with AudioReaderDTMF
	DTMFpass bool

	log *slog.Logger
	// TODO: RTPpass. RTP pass means that RTP will be proxied.
	// This gives high performance but you can not attach any pipeline in media processing
	// RTPpass bool

	// mu guards dialogs, Originator and proxy
	mu      sync.Mutex
	dialogs []DialogSession
	// proxy is the proxy AddDialogSession started, nil until it starts
	proxy *bridgeProxy

	// minDialogs is just helper flag when to start proxy
	WaitDialogsNum int
}

var bridgeReadPool = sync.Pool{
	New: func() any {
		b := make([]byte, media.RTPBufSize)
		return &b
	},
}

// NewBridge creates bridge with default settings.
func NewBridge() (b Bridge) {
	// Set up in the result itself: a Bridge holds a lock
	b.Init(media.DefaultLogger())
	return
}

func (b *Bridge) Init(log *slog.Logger) {
	b.log = log
	if b.log == nil {
		b.log = media.DefaultLogger()
	}

	if b.WaitDialogsNum == 0 {
		b.WaitDialogsNum = 2
	}
}

func (b *Bridge) GetDialogs() []DialogSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dialogs
}

// originator returns Originator, read under the lock AddDialogSession sets it
// under.
func (b *Bridge) originator() DialogSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Originator
}

func (b *Bridge) AddDialogSession(d DialogSession) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// The dialog's codec is read from its media session
	if d.Media().MediaSession() == nil {
		return fmt.Errorf("dialog session has no media %q", d.Id())
	}

	// Check can this dialog be added to bridge. NO TRANSCODING
	if b.Originator != nil {
		// This may look ugly but it is safe way of reading
		origM := b.Originator.Media()
		origProps := MediaProps{}
		_ = origM.audioWriterProps(&origProps)

		m := d.Media()
		mprops := MediaProps{}
		_ = m.audioWriterProps(&mprops)

		err := func() error {
			if origProps.Codec != mprops.Codec {
				return fmt.Errorf("no transcoding supported in bridge codec1=%+v codec2=%+v", origProps.Codec, mprops.Codec)
			}
			return nil
		}()
		if err != nil {
			return err
		}
	}

	// The dialog joins once the checks below pass, so a refused dialog is not
	// left in the bridge.
	dialogs := append(slices.Clone(b.dialogs), d)
	if len(dialogs) >= b.WaitDialogsNum {
		if len(dialogs) > 2 {
			return fmt.Errorf("currently bridge only support 2 party")
		}
		// Check are both answered
		for _, d := range dialogs {
			// TODO remove this double locking. Read once
			if d.Media().RTPPacketReader == nil || d.Media().RTPPacketWriter == nil {
				return fmt.Errorf("dialog session not answered %q", d.Id())
			}
		}
	}

	b.dialogs = dialogs
	if len(b.dialogs) == 1 {
		b.Originator = d
	}

	if len(b.dialogs) < b.WaitDialogsNum {
		return nil
	}

	proxy := newBridgeProxy()
	b.proxy = proxy
	go func() {
		defer close(proxy.done)
		defer func(start time.Time) {
			b.log.Debug("Proxy media setup", "dur", time.Since(start).String())
		}(time.Now())
		proxy.err = b.proxyMedia(dialogs, proxy.stopping)
		// A hangup closes a dialog's media, which ends the proxy's read of it
		// and fails a write to it
		if proxy.err == nil || errors.Is(proxy.err, io.EOF) || errors.Is(proxy.err, net.ErrClosed) {
			b.log.Debug("Proxy media stopped", "error", proxy.err)
			return
		}
		b.log.Error("Proxy media stopped", "error", proxy.err)
	}()
	return nil
}

// StopProxyMedia stops the proxy AddDialogSession started and waits until it
// has stopped reading and writing the dialogs. It returns what the proxy ended
// with, as ProxyMedia does, and nil when no proxy was started.
//
// The proxy stops on its own once either dialog ends. Until it has stopped it
// can take the next frame of the dialog left, so call StopProxyMedia before
// reading that dialog. It can be called any number of times.
func (b *Bridge) StopProxyMedia() error {
	b.mu.Lock()
	proxy := b.proxy
	b.mu.Unlock()
	if proxy == nil {
		return nil
	}
	return proxy.stop()
}

// bridgeProxy is a proxy running in the background, and its stop.
type bridgeProxy struct {
	// stopping is closed to stop the proxy
	stopping chan struct{}
	stopOnce sync.Once
	// done is closed once the proxy has stopped, and err set before
	done chan struct{}
	err  error
}

func newBridgeProxy() *bridgeProxy {
	return &bridgeProxy{stopping: make(chan struct{}), done: make(chan struct{})}
}

// stop stops the proxy, waits until it has stopped, and returns what it ended
// with.
func (p *bridgeProxy) stop() error {
	p.stopOnce.Do(func() { close(p.stopping) })
	<-p.done
	return p.err
}

// ProxyMedia is explicit starting proxy media.
// In some cases you want to control and be signaled when bridge terminates
// It returns once either dialog, or its media, has ended.
//
// NOTE: Should be only called if you want to start manually proxying.
// It is required to set WaitDialogsNum higher than 2
//
// Experimental
func (b *Bridge) ProxyMedia() error {
	dialogs, err := b.proxyDialogs()
	if err != nil {
		return err
	}
	return b.proxyMedia(dialogs, nil)
}

// proxyDialogs returns the dialogs a proxy started by hand runs on, or why it
// can not be started.
func (b *Bridge) proxyDialogs() ([]DialogSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.dialogs) < 2 {
		return nil, fmt.Errorf("number of dialogs must equal to 2")
	}

	if b.WaitDialogsNum < 3 {
		return nil, fmt.Errorf("you are already running proxy media. Increase WaitDialogsNum")
	}
	return b.dialogs, nil
}

// ProxyMediaControl starts proxy in background and allows to stop proxy at any time.
// The proxy stops on its own once either dialog ends. Stop waits until the proxy
// has stopped and returns what it ended with, and can be called any number of
// times.
// Like ProxyMedia, it requires WaitDialogsNum higher than 2, so no other proxy
// runs on the dialogs.
//
// Experimental
func (b *Bridge) ProxyMediaControl() (func() error, error) {
	dialogs, err := b.proxyDialogs()
	if err != nil {
		return nil, err
	}

	proxy := newBridgeProxy()
	go func() {
		defer close(proxy.done)
		proxy.err = b.proxyMedia(dialogs, proxy.stopping)
	}()
	return proxy.stop, nil
}

// proxyMedia proxies the audio of two dialogs to each other, one direction
// each way, until proxyWait ends them.
func (b *Bridge) proxyMedia(dialogs []DialogSession, stopping <-chan struct{}) error {
	log := b.log

	m1 := dialogs[0].Media()
	m2 := dialogs[1].Media()

	// Lets for now simplify proxy and later optimize

	errCh := make(chan error, 2)
	if b.DTMFpass {
		go func() {
			errCh <- b.proxyMediaWithDTMF(m1, m2)
		}()

		go func() {
			errCh <- b.proxyMediaWithDTMF(m2, m1)
		}()
		return proxyWait(dialogs, errCh, stopping)
	}
	func() {
		p1, p2 := MediaProps{}, MediaProps{}
		r := m1.audioReaderProps(&p1)
		w := m2.audioWriterProps(&p2)

		log := log.With("from", p1.Raddr+" > "+p1.Laddr, "to", p2.Laddr+" > "+p2.Raddr)
		log.Debug("Starting proxy media routine")
		go proxyMediaBackground(log, r, w, errCh)
	}()

	// Second
	func() {
		p1, p2 := MediaProps{}, MediaProps{}
		r := m2.audioReaderProps(&p1)
		w := m1.audioWriterProps(&p2)
		log := log.With("from", p1.Raddr+" > "+p1.Laddr, "to", p2.Laddr+" > "+p2.Raddr)
		log.Debug("Starting proxy media routine")
		go proxyMediaBackground(log, r, w, errCh)
	}()

	return proxyWait(dialogs, errCh, stopping)
}

// proxyWait waits for the two directions of a proxy between dialogs, which
// send their results to errCh, until either direction ends, either dialog ends
// or stopping is closed. A direction still reading a dialog would take the
// next frame from whoever reads that dialog next, so proxyWait then ends both.
// It returns once both have ended and the dialogs can be read again.
func proxyWait(dialogs []DialogSession, errCh chan error, stopping <-chan struct{}) error {
	var err error
	running := 2
	select {
	case err = <-errCh:
		running--
	case <-dialogs[0].Context().Done():
	case <-dialogs[1].Context().Done():
	case <-stopping:
	}

	// A read deadline ends a direction without an error, also while its dialog
	// is silent. It is cleared once both have ended.
	var stopErr error
	for _, d := range dialogs {
		stopErr = errors.Join(stopErr, bridgeRTPControlErr(d.Media().StopRTP(1, 0)))
	}
	for ; running > 0; running-- {
		err = errors.Join(err, <-errCh)
	}
	var startErr error
	for _, d := range dialogs {
		startErr = errors.Join(startErr, bridgeRTPControlErr(d.Media().StartRTP(1, 0)))
	}
	return errors.Join(err, stopErr, startErr)
}

func proxyMediaBackground(log *slog.Logger, reader io.Reader, writer io.Writer, ch chan error) {
	buf := rtpBufPool.Get()
	defer rtpBufPool.Put(buf)

	written, err := copyWithBuf(reader, writer, buf.([]byte))
	log.Debug("Proxy media routine finished", "bytes", written)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		log.Debug("Proxy media stopped with timeout. RTP Deadline", "error", err)
		err = nil
	}
	ch <- err
}

func (b *Bridge) proxyMediaWithDTMF(m1 *DialogMedia, m2 *DialogMedia) error {
	// The proxy reads and writes through DTMF interceptors of its own, which
	// are not set on the dialogs: once the proxy ends, a dialog must not keep
	// sending the keys read on it to the other one.
	dtmfReader := DTMFReader{}
	p1, p2 := MediaProps{}, MediaProps{}
	if _, err := m1.AudioReader(bridgeDTMFReader(&dtmfReader), WithAudioReaderMediaProps(&p1)); err != nil {
		return err
	}
	dtmfWriter := DTMFWriter{}
	if _, err := m2.AudioWriter(bridgeDTMFWriter(&dtmfWriter), WithAudioWriterMediaProps(&p2)); err != nil {
		return err
	}
	dtmfReader.OnDTMF(func(dtmf rune) error {
		return dtmfWriter.WriteDTMF(dtmf)
	})

	buf := rtpBufPool.Get()
	defer rtpBufPool.Put(buf)

	log := b.log.With("from", p1.Raddr+" > "+p1.Laddr, "to", p2.Laddr+" > "+p2.Raddr)
	log.Debug("Starting proxy media routine")
	written, err := copyWithBuf(&dtmfReader, &dtmfWriter, buf.([]byte))
	log.Debug("Bridge proxy stream finished", "bytes", written)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// A read deadline is how the proxy is stopped
		return nil
	}
	return err
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

// BridgeMix is mixing audio when having 2 or more parties.
//
// Experimental: not fully tested yet
type BridgeMix struct {
	mu      sync.Mutex
	dialogs []DialogSession

	mixWG    sync.WaitGroup
	mixState int
	// mixStopped is closed when the stop that set mixState to 2 has finished
	mixStopped chan struct{}
	// mixCodec is the audio of the last mix started, zero once the bridge is
	// empty
	mixCodec media.Codec
	// streamCodecs is the codec the running mix decodes and encodes each
	// dialog with, by dialog ID, and nil while no mix runs
	streamCodecs map[string]media.Codec
	// unhooks removes, by dialog ID, the media update hook of each dialog in
	// the bridge
	unhooks map[string]func()

	// WaitDialogsNum is just helper flag when to start proxy
	WaitDialogsNum int
	// RealtimeReader is almost always nesessary if you are delaying audio streaming(mixing) in bridge
	RealtimeReader bool
	Poll           bool
	log            *slog.Logger
}

var (
	// BridgeDebug enables some traces
	BridgeDebug bool

	bridgeTrace = func(args ...any) {
		if BridgeDebug {
			fmt.Fprintln(os.Stderr, args...)
		}
	}
)

func NewBridgeMix() *BridgeMix {
	b := BridgeMix{
		RealtimeReader: true,
		Poll:           true,
	}
	b.Init()
	return &b
}

// Init initializes bridge struct. Use only if construct bridge with struct
// or use NewBridgeMix
func (b *BridgeMix) Init() {
	b.log = media.DefaultLogger().With("caller", "bridge_mix")
}

func (b *BridgeMix) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	str := fmt.Sprintf("state: %d", b.mixState)
	str += " dialogs:["
	for _, d := range b.dialogs {
		str += " " + d.Id()
	}
	str += "]"
	return str
}

// DialogSessionsList returns list of dialogs in bridge
// It is not safe to use dialogs for media until they are removed from bridge
// A dialog that a re-INVITE moved to audio the bridge can not mix with the
// others is taken out of the bridge at the next join or leave.
func (b *BridgeMix) DialogSessionsList() []DialogSession {
	b.mu.Lock()
	defer b.mu.Unlock()
	return slices.Clone(b.dialogs)
}

func (b *BridgeMix) AddDialogSession(d DialogSession) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if state := d.DialogSIP().LoadState(); state != sip.DialogStateConfirmed {
		return fmt.Errorf("dialog must be answered before adding into bridge")
	}
	// The mix reads the dialog's codec from its media session
	if d.Media().MediaSession() == nil {
		return fmt.Errorf("dialog session has no media %q", d.Id())
	}

	// Stop any current mixing
	b.log.Debug("Stoping mix", "dialog", d.Id())
	if err := b.mixStopWait(); err != nil {
		// The join is refused, and the dialogs already here keep being mixed.
		b.mixStart()
		return fmt.Errorf("failed to stop current mixing: %w", err)
	}

	// The bridge tells its dialogs apart by ID, so a dialog is in it once. This
	// is checked after the stop, which lets another join run meanwhile.
	if slices.ContainsFunc(b.dialogs, func(in DialogSession) bool { return in.Id() == d.Id() }) {
		b.mixStart()
		return fmt.Errorf("dialog %q is already in the bridge", d.Id())
	}

	b.dialogs = append(b.dialogs, d)
	if b.unhooks == nil {
		b.unhooks = map[string]func(){}
	}
	b.unhooks[d.Id()] = d.Media().addMediaUpdateHook(func() { b.mediaUpdated(d) })
	b.log.Debug("Added dialog", "dialog", d.Id(), "total", len(b.dialogs))
	if err := b.mixStart()[d.Id()]; err != nil {
		// The dialog can not be mixed with the others. The join is refused: the
		// dialog is out of the bridge again, and the others are mixed.
		return err
	}
	return nil
}

func (b *BridgeMix) RemoveDialogSession(d DialogSession) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	dialogID := d.Id()

	var dialog DialogSession
	for _, d := range b.dialogs {
		if d.Id() == dialogID {
			dialog = d
			break
		}
	}
	if dialog == nil {
		return nil
	}

	b.log.Debug("Stoping mix", "dialog", dialog.Id())

	// The dialog leaves even when stopping the mix reports an error. Kept in the
	// bridge, it would be in every later mix.
	stopErr := b.mixStopWait()
	if stopErr != nil {
		stopErr = fmt.Errorf("failed to stop current mixing: %w", stopErr)
	}

	// NOTE: mixStopWait unlocks so we can not do any update before
	for i, d := range b.dialogs {
		if d.Id() == dialogID {
			b.dialogs = append(b.dialogs[:i], b.dialogs[i+1:]...)
			b.unhookUnsafe(dialogID)
			break
		}
	}

	b.log.Debug("Removed dialog", "dialog", dialog.Id(), "total", len(b.dialogs))
	b.mixStart()
	return stopErr
}

// unhookUnsafe removes the media update hook of a dialog that has left the
// bridge.
func (b *BridgeMix) unhookUnsafe(dialogID string) {
	if unhook, ok := b.unhooks[dialogID]; ok {
		unhook()
		delete(b.unhooks, dialogID)
	}
}

// mediaUpdated runs after a media update of a dialog in the bridge. The mix
// decodes and encodes each dialog with the codec the dialog had when the mix
// started, so once the dialog's codec has changed the mix is restarted, as a
// join or leave restarts it: the dialog is mixed with its new codec, or taken
// out of the bridge when its audio no longer matches the mix.
func (b *BridgeMix) mediaUpdated(d DialogSession) {
	b.mu.Lock()
	defer b.mu.Unlock()
	mixedWith, mixed := b.streamCodecs[d.Id()]
	if !mixed {
		// No mix runs with this dialog. The next one reads its codec anew.
		return
	}
	p := MediaProps{}
	d.Media().audioReaderProps(&p)
	if p.Codec == mixedWith {
		return
	}

	b.log.Debug("Dialog codec changed, restarting mix", "dialog", d.Id(), "codec", p.Codec.String())
	if err := b.mixStopWait(); err != nil {
		// As at a leave, the mix restarts on every dialog regardless.
		b.log.Warn("Stopping the mix for a media update failed", "dialog", d.Id(), "error", err)
	}
	b.mixStart()
}

// mixEnded runs when a mix loop exits. A loop that ended on its own goes from
// running to idle, and its readers may still be reading the dialogs until the
// next join or leave stops them. A loop stopped by mixStopWait stays in the
// stopping state, which the stopping caller clears once mixWG has drained, so
// no other caller starts a mix and adds to mixWG while that caller is still
// waiting.
func (b *BridgeMix) mixEnded() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mixState == 1 {
		b.stateWriteUnsafe(0)
	}
}

func (b *BridgeMix) stateWriteUnsafe(s int) {
	b.mixState = s
}

func (b *BridgeMix) stateRead() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.mixState
}

func (b *BridgeMix) mixStopWait() error {
	// DO NOT CALL THIS INSIDE LOOP of b.dialogs. This Unlocks
	// A stop another caller started is waited for. Until it has finished, the
	// mix it stops may still be reading the dialogs.
	for b.mixState == 2 {
		stopped := b.mixStopped
		b.mu.Unlock()
		<-stopped
		b.mu.Lock()
	}

	// This caller sets the stopping state, so it clears it once the stopped
	// mix has drained, even when stopping reported an error.
	stopErr := b.mixStop()
	b.mu.Unlock()
	b.mixWG.Wait()
	b.mu.Lock()
	b.stateWriteUnsafe(0)
	close(b.mixStopped)

	// Enable RTP again, on every dialog even when stopping reported an error,
	// so no dialog is left with its reads stopped.
	var startErr error
	for _, d := range b.dialogs {
		err := d.Media().StartRTP(1, 0) // Start reading
		startErr = errors.Join(startErr, bridgeRTPControlErr(err))
	}
	return errors.Join(stopErr, startErr)
}

// mixStop stops reads on every dialog, whether or not a mix loop is running:
// a loop that ended on its own may have left its readers reading them.
func (b *BridgeMix) mixStop() error {
	b.mixState = 2 // Set it stoping in progress
	b.mixStopped = make(chan struct{})
	var allErros error
	for _, d := range b.dialogs {
		err := d.Media().StopRTP(1, 0) // Stop reading
		allErros = errors.Join(allErros, bridgeRTPControlErr(err))
	}
	return allErros
}

// bridgeRTPControlErr returns the error of starting or stopping reads on a
// dialog, or nil when the dialog's RTP connection is closed. A BYE closes the
// dialog's media before its handler leaves the bridge, so that is the usual
// state of a dialog that hung up: its reads already fail, there is no read to
// start or stop, and it must still be able to leave.
func bridgeRTPControlErr(err error) error {
	if errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

// mixStart starts mixing the dialogs in the bridge. The mix neither resamples
// nor reframes, so it runs at one sample rate and frame duration: those of the
// last mix while a dialog still has them, since a re-INVITE can move a dialog
// to other audio, and otherwise those of the first dialog. A dialog whose audio
// differs, or can not be mixed at all, is taken out of the bridge and the
// others are mixed. mixStart returns why each dialog it took out could not be
// mixed, by dialog ID.
func (b *BridgeMix) mixStart() (notMixed map[string]error) {
	if b.mixState == 2 {
		// A stop is in progress (another goroutine is in mixStopWait).
		// Don't start a new mix to avoid WaitGroup Add/Wait race.
		return nil
	}
	b.streamCodecs = nil
	if len(b.dialogs) < 1 {
		b.mixCodec = media.Codec{}
		return nil
	}
	if len(b.dialogs) < b.WaitDialogsNum {
		return nil
	}

	codecs := make([]media.Codec, len(b.dialogs))
	for i, d := range b.dialogs {
		p := MediaProps{}
		d.Media().audioReaderProps(&p)
		codecs[i] = p.Codec
	}
	mixCodec := b.mixCodec
	if !slices.ContainsFunc(codecs, func(c media.Codec) bool { return bridgeSameAudio(c, mixCodec) }) {
		mixCodec = codecs[0]
	}

	notMixed = map[string]error{}
	streamCodecs := map[string]media.Codec{}
	var rwStreams []*bridgePCMStream
	for _, d := range b.dialogs {
		stream := &bridgePCMStream{}
		if err := b.addDialogStream(d, stream, mixCodec); err != nil {
			notMixed[d.Id()] = err
			continue
		}
		rwStreams = append(rwStreams, stream)
		streamCodecs[d.Id()] = stream.codec
	}
	for id, err := range notMixed {
		b.log.Warn("Dialog can not be mixed, taking it out of the bridge", "dialog", id, "error", err)
		b.unhookUnsafe(id)
	}
	b.dialogs = slices.DeleteFunc(b.dialogs, func(d DialogSession) bool {
		_, out := notMixed[d.Id()]
		return out
	})
	if len(b.dialogs) < 1 {
		b.mixCodec = media.Codec{}
		return notMixed
	}
	if len(b.dialogs) < b.WaitDialogsNum {
		return notMixed
	}
	b.mixCodec = mixCodec
	b.streamCodecs = streamCodecs

	ctx, cancelPoll := context.WithCancel(context.Background())
	// We could decide and optimize here, poll vs deadlines
	poll := b.Poll

	// Readers start once every stream is set up, so a stream that can not be
	// mixed leaves no reader behind.
	if poll {
		for _, stream := range rwStreams {
			// We do buffering because initial packet can be read oner than actual mixing has started
			b.mixWG.Add(1)
			bridgeTrace("poll: starting stream", "stream.id", stream.id)
			go func(s *bridgePCMStream) {
				defer b.mixWG.Done()

				bufPtr := bridgeReadPool.Get().(*[]byte)
				defer bridgeReadPool.Put(bufPtr)

				defer close(s.pipeWrite)

				buf := *bufPtr
				for {
					n, err := s.r.Read(buf)
					if err != nil {
						bridgeTrace("poll: stopped with error", "error", err, "stream.id", s.id)
						return
					}

					select {
					case s.pipeWrite <- buf[:n]:
						nw := <-s.pipeRead
						if nw != n {
							// there is no reason this to happen, so lets panic
							panic("reading from pipe was not full")
						}
					case <-ctx.Done():
						bridgeTrace("poll: stream context canceled", "stream.id", s.id)
						return
					}
				}
			}(stream)
		}
	}

	// Start new mix
	b.mixWG.Add(1)
	b.stateWriteUnsafe(1)
	go func(rwStreams []*bridgePCMStream) {
		defer cancelPoll()
		defer b.mixWG.Done()
		defer b.mixEnded()
		b.log.Debug("Starting mix loop", "streams.len", len(rwStreams))
		if err := b.mixLoop(rwStreams, poll, mixCodec.SampleDur); err != nil {
			b.log.Info("Mix stopped with error", "error", err)
		}
	}(rwStreams)
	return notMixed
}

// bridgeSameAudio reports whether audio in codec a can be mixed with audio in
// codec b: same sample rate and frame duration.
func bridgeSameAudio(a, b media.Codec) bool {
	return a.SampleRate == b.SampleRate && a.SampleDur == b.SampleDur
}

func (b *BridgeMix) mixLoop(rwStreams []*bridgePCMStream, poll bool, frameDur time.Duration) error {
	mixBuf := make([]byte, media.RTPBufSize)

	if len(rwStreams) == 1 {
		b.log.Debug("Only single stream in bridge, reading bufffers...")
		// Just keep streaming
		r := rwStreams[0]
		if !poll {
			_, err := media.ReadAll(r.r, media.RTPBufSize)
			return err
		}

		for {
			bw, more := <-r.pipeWrite
			if !more {
				break
			}
			n := copy(r.buf, bw)
			r.pipeRead <- n
		}
		return nil
	}

	// A round starts every frame duration, on one ticker, so a reader holding a
	// frame hands it over within a frame duration. That stays inside the two
	// frame durations the realtime reader allows before it drops a frame as
	// late. A round that the writers pace past its tick is followed at once by
	// the next.
	ticker := time.NewTicker(frameDur)
	defer ticker.Stop()
	for ; ; <-ticker.C {
		n, err := b.mixAllStreams(rwStreams, mixBuf, poll)
		if err != nil {
			return err
		}
		if n == 0 {
			bridgeTrace("Nothing read, waiting for the next round")
			continue
		}

		// broadcast to all, except the streams dropped from the mix
		for i, w := range rwStreams {
			if w.markGone {
				continue
			}
			streamBuf := mixBuf[:n]
			if w.n > 0 {
				readBuf := w.buf
				streamBuf = unmixStream(readBuf[:w.n], mixBuf[:n])
			}

			n, err := w.w.Write(streamBuf)
			bridgeTrace("Writing stream", "i", i, "stream", w.id, "n", n, "err", err)
			if err != nil {
				// A dialog whose media is closed, as a BYE closes it before its
				// handler leaves the bridge, is dropped from the mix. Any other
				// failure loses this frame for this dialog alone. Either way the
				// others keep being mixed.
				if errors.Is(err, net.ErrClosed) {
					b.log.Debug("Dropping stream with closed media from mix", "stream.id", w.id, "error", err)
					w.markGone = true
					continue
				}
				b.log.Debug("Writing stream failed", "stream.id", w.id, "error", err)
			}
		}
	}
}

type bridgePCMStream struct {
	id uint32
	r  io.Reader
	w  io.Writer
	// codec is the dialog's codec the stream decodes and encodes
	codec media.Codec
	// media is the dialog's media, whose current session a direct read sets its
	// deadline on
	media *DialogMedia
	// read buf
	buf []byte
	n   int

	pipeRead  chan int
	pipeWrite chan []byte
	markGone  bool
}

func (b *BridgeMix) addDialogStream(d DialogSession, stream *bridgePCMStream, mixCodec media.Codec) error {
	m := d.Media()

	// The reader, the writer and their codec are read together, so a re-INVITE
	// can not give the stream's decoder and encoder different codecs.
	p := MediaProps{}
	r, w, err := m.audioReaderWriterProps(&p)
	if err != nil {
		return err
	}

	if !bridgeSameAudio(p.Codec, mixCodec) {
		return fmt.Errorf("Codec missmatch. Resampling or transcoding is not supported")
	}

	// The realtime reader belongs to this mix and is never set on the dialog. It
	// judges frames late against the first frame this mix reads, and the dialog
	// leaves the bridge with the reader it joined with.
	rtr := r
	if _, ok := r.(*media.RTPRealTimeReader); b.RealtimeReader && !ok {
		rtr = media.NewRTPRealTimeReader(r, m.RTPPacketReader, p.Codec)
	}

	// Attach PCM decoder
	pcmReader := audio.PCMDecoderReader{}
	if err := pcmReader.Init(p.Codec, rtr); err != nil {
		return err
	}

	// Now do write stream
	pcmWriter := audio.PCMEncoderWriter{}
	if err := pcmWriter.Init(p.Codec, w); err != nil {
		return err
	}

	*stream = bridgePCMStream{
		r:         &pcmReader,
		w:         &pcmWriter,
		codec:     p.Codec,
		media:     m,
		id:        m.RTPPacketWriter.SSRC,
		buf:       make([]byte, media.RTPBufSize),
		pipeRead:  make(chan int),
		pipeWrite: make(chan []byte),
	}
	return nil
}

func (b *BridgeMix) mixAllStreams(rwStreams []*bridgePCMStream, mixedBuf []byte, poll bool) (int, error) {
	maxN := 0
	// zero mixed buf
	for i := 0; i < len(mixedBuf); i++ {
		mixedBuf[i] = 0
		// binary.LittleEndian.PutUint16(mixedBuf[i:], uint16(0))
	}

	if !poll {
		// If are not polling data then we need todo direct read
		err := func() error {
			handledStreams := len(rwStreams)
			for _, r := range rwStreams {
				r.n = 0
				if r.markGone {
					handledStreams--
					continue
				}
				if !b.armDirectRead(r) {
					return fmt.Errorf("reading is stopped")
				}

				// Mostly PCM sample size should be same or less our sampling
				// but we should keep same sampling or deal this per writer?
				n, err := b.readDirect(r)
				if err != nil {
					if errors.Is(err, os.ErrDeadlineExceeded) {
						state := b.stateRead()
						if state != 1 {
							// We are stopped
							return err
						}
						continue
					}
					// The stream's reader has ended, as it does once a BYE has
					// closed the dialog's media. The dialog is dropped from the
					// mix, and the others keep being mixed.
					r.markGone = true
					handledStreams--
					continue
				}
				r.n = n
				mixN := audio.PCMMix(mixedBuf, mixedBuf, r.buf[:n])
				maxN = max(maxN, mixN)
			}
			if handledStreams == 0 {
				return fmt.Errorf("all streams are gones")
			}
			return nil
		}()
		return maxN, err
	}

	err := func() error {
		handledStreams := len(rwStreams)
		for _, r := range rwStreams {
			if r.markGone {
				handledStreams--
				continue
			}
			r.n = 0 // Make sure it is zero

			select {
			case bw, more := <-r.pipeWrite:
				if !more {
					r.markGone = true
					continue
				}
				n := copy(r.buf, bw)
				r.n = n
				r.pipeRead <- n

				readBuf := r.buf[:n]
				mixN := audio.PCMMix(mixedBuf, mixedBuf, readBuf)
				maxN = max(maxN, mixN)

			default:
				// Do not block
				b.log.Debug("poll: no packet on stream", "stream.id", r.id)
			}
		}

		if handledStreams == 0 {
			return fmt.Errorf("all streams are gones")
		}

		if handledStreams < len(rwStreams) || maxN == 0 {
			state := b.stateRead()
			if state != 1 {
				// We are stopped
				return fmt.Errorf("reading is stopped")
			}
		}

		return nil
	}()

	b.log.Debug("Mixing done", "streams.len", len(rwStreams), "maxN", maxN)
	return maxN, err
}

// armDirectRead gives the next direct read of a stream a deadline one
// millisecond away, on its dialog's current media session. It sets it under the
// bridge lock and only while the mix runs, so it never replaces the deadline
// mixStop sets to stop the mix, and returns false once a stop has begun. A
// deadline that can not be set leaves the read to report what is wrong with the
// connection.
func (b *BridgeMix) armDirectRead(s *bridgePCMStream) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mixState != 1 {
		return false
	}
	_ = s.media.MediaSession().StopRTP(1, time.Millisecond)
	return true
}

// readDirect reads the stream once armDirectRead has set its deadline. A
// re-INVITE that rebinds the dialog's media closes the socket that deadline is
// on, and the packet reader carries a read waiting there over to the new
// socket, which has no deadline. A read still waiting once its deadline has
// passed is therefore armed again, on the dialog's current media session, every
// two milliseconds until it returns. The arming has stopped when readDirect
// returns.
func (b *BridgeMix) readDirect(s *bridgePCMStream) (int, error) {
	readDone := make(chan struct{})
	rearmed := make(chan struct{})
	rearm := time.AfterFunc(2*time.Millisecond, func() {
		defer close(rearmed)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			b.armDirectRead(s)
			select {
			case <-readDone:
				return
			case <-ticker.C:
			}
		}
	})
	n, err := s.r.Read(s.buf)
	close(readDone)
	if !rearm.Stop() {
		<-rearmed
	}
	return n, err
}

// unmixStream returns what a stream hears of a round's mix: mixedBuf without
// buf, the frame the stream read that round. A frame shorter than the mix put
// nothing into the rest of it, so there the stream hears the mix as it is. The
// result is written over buf's backing array, whose capacity holds the mix.
func unmixStream(buf []byte, mixedBuf []byte) []byte {
	heard := buf[:len(mixedBuf)]
	audio.PCMUnmix(heard, mixedBuf, buf)
	copy(heard[len(buf):], mixedBuf[len(buf):])
	return heard
}
