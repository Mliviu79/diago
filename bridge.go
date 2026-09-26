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

type Bridge struct {
	// Originator is dialog session that created bridge
	Originator DialogSession
	// DTMFpass is also dtmf pipeline and proxy. By default only audio media is proxied
	// NOTE: this may not work if you are already processing DTMF with AudioReaderDTMF
	DTMFpass bool

	log *slog.Logger
	// TODO: RTPpass. RTP pass means that RTP will be proxied.
	// This gives high performance but you can not attach any pipeline in media processing
	// RTPpass bool

	dialogs []DialogSession

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
func NewBridge() Bridge {
	b := Bridge{}
	b.Init(media.DefaultLogger())
	return b
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
	return b.dialogs
}

func (b *Bridge) AddDialogSession(d DialogSession) error {
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

	b.dialogs = append(b.dialogs, d)
	if len(b.dialogs) == 1 {
		b.Originator = d
	}

	if len(b.dialogs) < b.WaitDialogsNum {
		return nil
	}

	if len(b.dialogs) > 2 {
		return fmt.Errorf("currently bridge only support 2 party")
	}
	// Check are both answered
	for _, d := range b.dialogs {
		// TODO remove this double locking. Read once
		if d.Media().RTPPacketReader == nil || d.Media().RTPPacketWriter == nil {
			return fmt.Errorf("dialog session not answered %q", d.Id())
		}
	}

	go func() {
		defer func(start time.Time) {
			b.log.Debug("Proxy media setup", "dur", time.Since(start).String())
		}(time.Now())
		if err := b.proxyMedia(); err != nil {
			if errors.Is(err, io.EOF) {
				return
			}

			b.log.Error("Proxy media stopped", "error", err)
		}
	}()
	return nil
}

// ProxyMedia is explicit starting proxy media.
// In some cases you want to control and be signaled when bridge terminates
//
// NOTE: Should be only called if you want to start manually proxying.
// It is required to set WaitDialogsNum higher than 2
//
// Experimental
func (b *Bridge) ProxyMedia() error {
	if len(b.dialogs) < 2 {
		return fmt.Errorf("number of dialogs must equal to 2")
	}

	if b.WaitDialogsNum < 3 {
		return fmt.Errorf("you are already running proxy media. Increase WaitDialogsNum")
	}

	for _, d := range b.dialogs {
		d.Media().mediaSession.StopRTP(2, 0)
	}
	return b.proxyMedia()
}

// ProxyMediaControl starts proxy in background and allows to stop proxy at any time.
// Stop should be called once and it is not needed to be called if call is terminating
//
// Experimental
func (b *Bridge) ProxyMediaControl() (func() error, error) {
	proxyErr := make(chan error, 1)
	go func() {
		proxyErr <- b.proxyMedia()
	}()

	stopF := func() error {
		for _, d := range b.dialogs {
			d.Media().mediaSession.StopRTP(2, 0)
		}

		// Wait goroutine termination
		err := <-proxyErr
		for _, d := range b.dialogs {
			d.Media().mediaSession.StartRTP(2)
		}
		return err
	}

	return stopF, b.proxyMedia()
}

// proxyMedia starts routine to proxy media between
// Should be called after having 2 or more participants
func (b *Bridge) proxyMedia() error {
	var err error
	log := b.log

	m1 := b.dialogs[0].Media()
	m2 := b.dialogs[1].Media()

	// Lets for now simplify proxy and later optimize

	if b.DTMFpass {
		errCh := make(chan error, 4)
		go func() {
			errCh <- b.proxyMediaWithDTMF(m1, m2)
		}()

		go func() {
			errCh <- b.proxyMediaWithDTMF(m2, m1)
		}()

		// Wait for all to finish
		for i := 0; i < 2; i++ {
			err = errors.Join(err, <-errCh)
		}
		return err
	}
	errCh := make(chan error, 2)
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

	// Wait for all to finish
	for i := 0; i < 2; i++ {
		err = errors.Join(err, <-errCh)
	}
	return err
}

func proxyMediaBackground(log *slog.Logger, reader io.Reader, writer io.Writer, ch chan error) {
	buf := rtpBufPool.Get()
	defer rtpBufPool.Put(buf)

	written, err := copyWithBuf(reader, writer, buf.([]byte))
	log.Debug("Proxy media routine finished", "bytes", written)
	if err, ok := err.(net.Error); ok && err.Timeout() {
		log.Debug("Proxy media stopped with timeout. RTP Deadline", "error", err)
		err = nil
	}
	ch <- err
}

func (b *Bridge) proxyMediaWithDTMF(m1 *DialogMedia, m2 *DialogMedia) error {
	dtmfReader := DTMFReader{}
	p1, p2 := MediaProps{}, MediaProps{}
	r, err := m1.AudioReader(WithAudioReaderDTMF(&dtmfReader), WithAudioReaderMediaProps(&p1))
	if err != nil {
		return err
	}
	dtmfWriter := DTMFWriter{}
	w, err := m2.AudioWriter(WithAudioWriterDTMF(&dtmfWriter), WithAudioWriterMediaProps(&p2))
	if err != nil {
		return err
	}
	dtmfReader.OnDTMF(func(dtmf rune) error {
		return dtmfWriter.WriteDTMF(dtmf)
	})

	buf := rtpBufPool.Get()
	defer rtpBufPool.Put(buf)

	log := b.log.With("from", p1.Raddr+" > "+p1.Laddr, "to", p2.Laddr+" > "+p2.Raddr)
	log.Debug("Starting proxy media routine")
	written, err := copyWithBuf(r, w, buf.([]byte))
	log.Debug("Bridge proxy stream finished", "bytes", written)
	return err
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

	// Stop any current mixing
	b.log.Debug("Stoping mix", "dialog", d.Id())
	if err := b.mixStopWait(); err != nil {
		// The join is refused, and the dialogs already here keep being mixed.
		return errors.Join(fmt.Errorf("failed to stop current mixing: %w", err), b.mixStart())
	}

	b.dialogs = append(b.dialogs, d)
	b.log.Debug("Added dialog", "dialog", d.Id(), "total", len(b.dialogs))
	if err := b.mixStart(); err != nil {
		// The dialog can not be mixed with the others. The join is refused, and
		// the dialogs already here keep being mixed.
		b.dialogs = slices.Delete(b.dialogs, len(b.dialogs)-1, len(b.dialogs))
		return errors.Join(err, b.mixStart())
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
			break
		}
	}

	b.log.Debug("Removed dialog", "dialog", dialog.Id(), "total", len(b.dialogs))
	return errors.Join(stopErr, b.mixStart())
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

func (b *BridgeMix) mixStart() error {
	if b.mixState == 2 {
		// A stop is in progress (another goroutine is in mixStopWait).
		// Don't start a new mix to avoid WaitGroup Add/Wait race.
		return nil
	}
	if len(b.dialogs) < 1 {
		return nil
	}
	if len(b.dialogs) < b.WaitDialogsNum {
		return nil
	}

	ctx, cancelPoll := context.WithCancel(context.Background())
	// We could decide and optimize here, poll vs deadlines
	poll := b.Poll
	firstDialogCodec := media.Codec{}
	rwStreams, err := func() ([]*bridgePCMStream, error) {
		rwStreams := make([]*bridgePCMStream, len(b.dialogs))

		for i, d := range b.dialogs {
			rwStreams[i] = &bridgePCMStream{}
			if err := b.addDialogStream(d, rwStreams[i], &firstDialogCodec); err != nil {
				return nil, err
			}
		}
		return rwStreams, nil
	}()
	if err != nil {
		cancelPoll()
		return err
	}

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
		if err := b.mixLoop(rwStreams, poll, firstDialogCodec.SampleDur); err != nil {
			b.log.Info("Mix stopped with error", "error", err)
		}
	}(rwStreams)
	return nil
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
	id           uint32
	r            io.Reader
	w            io.Writer
	mediaSession *media.MediaSession
	// read buf
	buf []byte
	n   int

	pipeRead  chan int
	pipeWrite chan []byte
	markGone  bool
}

func (b *BridgeMix) addDialogStream(d DialogSession, stream *bridgePCMStream, firstDialogCodec *media.Codec) error {
	m := d.Media()

	p := MediaProps{}
	r, err := m.AudioReader(WithAudioReaderMediaProps(&p))
	if err != nil {
		return err
	}

	if firstDialogCodec.SampleRate == 0 {
		*firstDialogCodec = p.Codec
	}

	if firstDialogCodec.SampleRate != p.Codec.SampleRate || firstDialogCodec.SampleDur != p.Codec.SampleDur {
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
	p = MediaProps{}
	w, err := m.AudioWriter(WithAudioWriterMediaProps(&p))
	if err != nil {
		return err
	}

	pcmWriter := audio.PCMEncoderWriter{}
	if err := pcmWriter.Init(p.Codec, w); err != nil {
		return err
	}

	*stream = bridgePCMStream{
		r:            &pcmReader,
		w:            &pcmWriter,
		mediaSession: m.mediaSession,
		id:           m.RTPPacketWriter.SSRC,
		buf:          make([]byte, media.RTPBufSize),
		pipeRead:     make(chan int),
		pipeWrite:    make(chan []byte),
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
			for i, r := range rwStreams {
				r.mediaSession.StopRTP(1, 1*time.Millisecond)

				// Mostly PCM sample size should be same or less our sampling
				// but we should keep same sampling or deal this per writer?
				n, err := r.r.Read(r.buf)
				rwStreams[i].n = n
				if err != nil {
					if errors.Is(err, os.ErrDeadlineExceeded) {
						state := b.stateRead()
						if state != 1 {
							// We are stopped
							return err
						}
						continue
					}
					return err
				}
				maxN = max(maxN, n)
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

func unmixStream(buf []byte, mixedBuf []byte) []byte {
	n := len(mixedBuf)
	if len(buf) < len(mixedBuf) {
		// panic("stream buf is shorter than mixed buf")
	}

	readBuf := buf[:n]
	audio.PCMUnmix(readBuf, mixedBuf, readBuf)
	// NOTE: This can be higher than actual read bytes
	return readBuf
}
