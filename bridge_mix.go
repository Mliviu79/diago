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
)

var bridgeReadPool = sync.Pool{
	New: func() any {
		b := make([]byte, media.RTPBufSize)
		return &b
	},
}

func proxyMediaBackground(log *slog.Logger, reader io.Reader, writer io.Writer, ch chan error) {
	buf := rtpBufPool.Get()
	defer rtpBufPool.Put(buf)

	written, err := copyWithBuf(reader, writer, buf.([]byte))
	log.Debug("Proxy media routine finished", "bytes", written)
	// A deadline is how the proxy is stopped. The error is assigned in this
	// scope: the one sent below is the one cleared.
	var timeout net.Error
	if errors.As(err, &timeout) && timeout.Timeout() {
		log.Debug("Proxy media stopped with timeout. RTP Deadline", "error", err)
		err = nil
	}
	ch <- err
}

// BridgeMix is mixing audio when having 2 or more parties.
//
// Experimental: not fully tested yet
type BridgeMix struct {
	mu     sync.Mutex
	medias []bridgeMixMedia

	mixWG    sync.WaitGroup
	mixState int
	// mixStopped is closed when the stop that set mixState to 2 has finished
	mixStopped chan struct{}
	// mixCodec is the audio of the last mix started, zero once the bridge is
	// empty
	mixCodec media.Codec
	// streamCodecs is the codec the running mix decodes and encodes each
	// media with, and nil while no mix runs
	streamCodecs map[bridgeMixMedia]media.Codec
	// unhooks removes the media update hook of each media in the bridge
	unhooks map[bridgeMixMedia]func()

	// WaitDialogsNum is just helper flag when to start proxy
	WaitDialogsNum int
	// RealtimeReader is almost always nesessary if you are delaying audio streaming(mixing) in bridge
	RealtimeReader bool
	// RealtimeReaderPerMix has each mix read a media through a realtime reader
	// of its own, which is never set on the media: the media leaves the bridge
	// with the audio reader it joined with, and a mix judges a frame late only
	// against frames it has read itself.
	//
	// By default the realtime reader is set on the media as its audio reader.
	// It stays there once the media has left the bridge, and every later mix
	// reads through it.
	RealtimeReaderPerMix bool
	// WriteDeadlineLosesFrame has a write to a media that fails on a write
	// deadline, while the mix runs, lose that frame for that media alone:
	// the media stays in the mix and is written the next frame.
	//
	// By default such a media is taken out of the running mix, and is
	// written nothing more until the next join, leave or codec change
	// restarts the mix.
	WriteDeadlineLosesFrame bool
	Poll                    bool
	log                     *slog.Logger
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
	str += " medias:["
	for _, m := range b.medias {
		str += fmt.Sprintf(" %d", m.packetWriterSSRC())
	}
	str += "]"
	return str
}

// DialogMediaList returns list of medias in bridge.
// It is not safe to use media until it is removed from bridge.
// A media that a re-INVITE moved to audio the bridge can not mix with the
// others is taken out of the bridge at the next join, leave or codec change.
//
// The WebRTC medias in the bridge are listed by DialogWebrtcList.
func (b *BridgeMix) DialogMediaList() []*DialogMedia {
	return bridgeMixList[*DialogMedia](b)
}

// AddDialogMedia adds m to the bridge and mixes its audio with that of the
// medias already in it. A media that can not join is refused with an error and
// the others keep being mixed: one without a media session, with
// ErrNoMediaSetup, one that is in the bridge already, and one whose audio
// differs from the mix in sample rate or frame duration.
func (b *BridgeMix) AddDialogMedia(m *DialogMedia) error {
	if m == nil {
		return fmt.Errorf("dialog media is nil")
	}
	return b.addMedia(m)
}

// addMedia is AddDialogMedia and AddDialogWebrtc for a media of either type.
func (b *BridgeMix) addMedia(m bridgeMixMedia) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// The mix reads the media's codec from its media session
	if _, err := m.NegotiatedProps(); err != nil {
		return fmt.Errorf("dialog media has no media session: %w", err)
	}

	// Stop any current mixing
	b.log.Debug("Stoping mix")
	if err := b.mixStopWait(); err != nil {
		// The join is refused, and the medias already here keep being mixed.
		b.mixStart()
		return fmt.Errorf("failed to stop current mixing: %w", err)
	}

	// A media is in the bridge once: twice, two streams would read it at
	// once. This is checked after the stop, which lets another join run
	// meanwhile.
	if slices.Contains(b.medias, m) {
		b.mixStart()
		return fmt.Errorf("dialog media is already in the bridge")
	}

	b.medias = append(b.medias, m)
	b.hookUnsafe(m)
	b.log.Debug("Added media", "total", len(b.medias))
	if err := b.mixStart()[m]; err != nil {
		// The media can not be mixed with the others. The join is refused: the
		// media is out of the bridge again, and the others are mixed.
		return err
	}
	return nil
}

// RemoveDialogMedia takes m out of the bridge and mixes the medias left. The
// mix has stopped reading m when it returns. m leaves even when stopping the
// mix reports an error, which is then returned.
func (b *BridgeMix) RemoveDialogMedia(m *DialogMedia) error {
	return b.removeMedia(m)
}

// removeMedia is RemoveDialogMedia and RemoveDialogWebrtc for a media of
// either type.
func (b *BridgeMix) removeMedia(m bridgeMixMedia) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	var media bridgeMixMedia
	for _, bm := range b.medias {
		if bm == m {
			media = bm
			break
		}
	}
	if media == nil {
		return nil
	}

	b.log.Debug("Stoping mix")

	// The media leaves even when stopping the mix reports an error. Kept in
	// the bridge, it would be in every later mix.
	stopErr := b.mixStopWait()
	if stopErr != nil {
		stopErr = fmt.Errorf("failed to stop current mixing: %w", stopErr)
	}

	// NOTE: mixStopWait unlocks so we can not do any update before
	for i, bm := range b.medias {
		if bm == m {
			b.medias = append(b.medias[:i], b.medias[i+1:]...)
			b.unhookUnsafe(m)
			break
		}
	}

	b.log.Debug("Removed media", "total", len(b.medias))
	b.mixStart()
	return stopErr
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
	// mix it stops may still be reading the medias.
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

	// Enable RTP again, on every media even when stopping reported an error,
	// so no media is left with its reads stopped.
	var startErr error
	for _, m := range b.medias {
		err := m.StartRTP(1, 0) // Start reading
		startErr = errors.Join(startErr, bridgeRTPControlErr(err))
	}
	return errors.Join(stopErr, startErr)
}

// mixStop stops reads on every media, whether or not a mix loop is running:
// a loop that ended on its own may have left its readers reading them.
func (b *BridgeMix) mixStop() error {
	b.mixState = 2 // Set it stoping in progress
	b.mixStopped = make(chan struct{})
	var allErros error
	for _, m := range b.medias {
		err := m.StopRTP(1, 0) // Stop reading
		allErros = errors.Join(allErros, bridgeRTPControlErr(err))
	}
	return allErros
}

// mixStart starts mixing the medias in the bridge. The mix neither resamples
// nor reframes, so it runs at one sample rate and frame duration: those of the
// last mix while a media still has them, since a re-INVITE can move a media
// to other audio, and otherwise those of the first media. A media whose audio
// differs, or can not be mixed at all, is taken out of the bridge and the
// others are mixed. mixStart returns why each media it took out could not be
// mixed.
func (b *BridgeMix) mixStart() (notMixed map[bridgeMixMedia]error) {
	if b.mixState == 2 {
		// A stop is in progress (another goroutine is in mixStopWait).
		// Don't start a new mix to avoid WaitGroup Add/Wait race.
		return nil
	}
	b.streamCodecs = nil
	if len(b.medias) < 1 {
		b.mixCodec = media.Codec{}
		return nil
	}
	if len(b.medias) < b.WaitDialogsNum {
		return nil
	}

	codecs := make([]media.Codec, len(b.medias))
	for i, m := range b.medias {
		codecs[i] = bridgeMixCodec(m)
	}
	mixCodec := b.mixCodec
	if !slices.ContainsFunc(codecs, func(c media.Codec) bool { return bridgeSameAudio(c, mixCodec) }) {
		mixCodec = codecs[0]
	}

	notMixed = map[bridgeMixMedia]error{}
	streamCodecs := map[bridgeMixMedia]media.Codec{}
	var rwStreams []*bridgePCMStream
	for _, m := range b.medias {
		stream := &bridgePCMStream{}
		if err := b.addDialogStream(m, stream, mixCodec); err != nil {
			notMixed[m] = err
			continue
		}
		rwStreams = append(rwStreams, stream)
		streamCodecs[m] = stream.codec
	}
	for m, err := range notMixed {
		b.log.Warn("Dialog can not be mixed, taking it out of the bridge", "error", err)
		b.unhookUnsafe(m)
	}
	b.medias = slices.DeleteFunc(b.medias, func(m bridgeMixMedia) bool {
		_, out := notMixed[m]
		return out
	})
	if len(b.medias) < 1 {
		b.mixCodec = media.Codec{}
		return notMixed
	}
	if len(b.medias) < b.WaitDialogsNum {
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
	// frame hands it over within a frame duration. That stays inside what the
	// realtime reader allows before it drops a frame as late. A round that the
	// writers pace past its tick is followed at once by the next.
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
				// Detect is this Deadline or EOF error caused by stream exiting
				if errors.Is(err, os.ErrDeadlineExceeded) {
					state := b.stateRead()
					if state != 1 {
						// We are stopped
						return err
					}

					// Mixing has been stopped or network problem
					if b.WriteDeadlineLosesFrame {
						b.log.Debug("Writing stream failed", "stream.id", w.id, "error", err)
						continue
					}
					w.markGone = true
					continue

				}
				// A media that is closed, as a BYE closes it before its
				// handler leaves the bridge, is dropped from the mix. Any other
				// failure loses this frame for this media alone. Either way the
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
	// codec is the media's codec the stream decodes and encodes
	codec media.Codec
	// media is the dialog's media, whose current session a direct read sets its
	// deadline on
	media bridgeMixMedia
	// read buf
	buf []byte
	n   int

	pipeRead  chan int
	pipeWrite chan []byte
	markGone  bool
}

func (b *BridgeMix) addDialogStream(m bridgeMixMedia, stream *bridgePCMStream, mixCodec media.Codec) error {
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

	rtr := func() io.Reader {
		if !b.RealtimeReader {
			return r
		}

		if rtr, ok := r.(*media.RTPRealTimeReader); ok {
			return rtr
		}

		rtr := media.NewRTPRealTimeReader(r, m.packetReader(), p.Codec)
		if !b.RealtimeReaderPerMix {
			m.SetAudioReader(rtr)
		}
		return rtr
	}()

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
		id:        m.packetWriterSSRC(),
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
					// closed the dialog's media. The media is dropped from the
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
