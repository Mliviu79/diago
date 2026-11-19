// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/emiago/diago/media"
)

type BridgeAudioMedia struct {
	Reader      io.Reader
	ReaderProps MediaProps
	Writer      io.Writer
	WriterProps MediaProps
	StopRTP     func() error
	StartRTP    func() error

	// source is set for audio taken from a DialogMedia by AddDialogMedia.
	source bridgeMediaSource
}

type Bridge struct {
	// originator is dialog session that created bridge
	originator *BridgeAudioMedia
	// DTMFpass is also dtmf pipeline and proxy. By default only audio media is proxied
	// NOTE: this may not work if you are already processing DTMF with AudioReaderDTMF
	DTMFpass bool

	log *slog.Logger
	// TODO: RTPpass. RTP pass means that RTP will be proxied.
	// This gives high performance but you can not attach any pipeline in media processing
	// RTPpass bool

	dialogs []*BridgeAudioMedia
	// minDialogs is just helper flag when to start proxy
	WaitDialogsNum int

	wg sync.WaitGroup

	// mu guards dialogs, originator and proxy
	mu sync.Mutex
	// proxy is the proxy AddAudioMedia started, nil until it starts
	proxy *bridgeProxy
}

// NewBridge creates bridge with default settings.
func NewBridge() (b Bridge) {
	// Set up in the result itself: a Bridge holds a lock and a
	// sync.WaitGroup, which are not copied
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

func (b *Bridge) GetDialogs() []*BridgeAudioMedia {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dialogs
}

func (b *Bridge) AddDialogMedia(m *DialogMedia) error {
	med := BridgeAudioMedia{}
	var err error
	med.Reader, err = m.AudioReader(WithAudioReaderMediaProps(&med.ReaderProps))
	if err != nil {
		return err
	}

	med.Writer, err = m.AudioWriter(WithAudioWriterMediaProps(&med.WriterProps))
	if err != nil {
		return err
	}

	if b.DTMFpass {
		// The proxy reads and writes through DTMF interceptors of its own,
		// which are not set on the dialog: once the proxy has ended, a dialog
		// must not keep sending the keys read on it to the other party. The
		// proxy passes the keys read on one party to the writer of the other,
		// see passDTMF.
		dtmfReader := DTMFReader{}
		if _, err := m.AudioReader(WithAudioReaderMediaProps(&med.ReaderProps), bridgeDTMFReader(&dtmfReader)); err != nil {
			return err
		}
		// Until a proxy passes them on, the keys read have nowhere to go.
		dtmfReader.OnDTMF(func(rune) error { return nil })
		dtmfWriter := DTMFWriter{}
		if _, err := m.AudioWriter(WithAudioWriterMediaProps(&med.WriterProps), bridgeDTMFWriter(&dtmfWriter)); err != nil {
			return err
		}
		med.Reader, med.Writer = &dtmfReader, &dtmfWriter
		med.source.dtmfReader, med.source.dtmfWriter = &dtmfReader, &dtmfWriter
	}
	med.source.dialogMedia = m
	med.StopRTP = func() error {
		return m.StopRTP(0, 0)
	}

	med.StartRTP = func() error {
		return m.StartRTP(0, 0)
	}

	return b.AddAudioMedia(&med)
}

func (b *Bridge) AddMedia(m any) error {
	switch t := m.(type) {
	case *DialogMedia:
		return b.AddDialogMedia(t)
	case *DialogWebrtc:
		return b.AddDialogWebrtc(t)
	default:
		return fmt.Errorf("unsupporte media stack for bridge. Use AddAudioMedia")
	}
}

func (b *Bridge) AddDialogWebrtc(m *DialogWebrtc) error {
	med := BridgeAudioMedia{}
	var err error
	med.Reader, err = m.AudioReader(WithAudioReaderWebrtcProps(&med.ReaderProps))
	if err != nil {
		return err
	}
	med.Writer, err = m.AudioWriter(WithAudioWriterWebrtcProps(&med.WriterProps))
	if err != nil {
		return err
	}

	if b.DTMFpass {
		dtmfReader := DTMFReader{}
		med.Reader, err = m.AudioReader(WithAudioReaderWebrtcProps(&med.ReaderProps), WithAudioReaderWebrtcDTMF(&dtmfReader))
		if err != nil {
			return err
		}
		dtmfWriter := DTMFWriter{}
		med.Writer, err = m.AudioWriter(WithAudioWriterWebrtcProps(&med.WriterProps), WithAudioWriterWebrtcDTMF(&dtmfWriter))
		if err != nil {
			return err
		}

		dtmfReader.OnDTMF(func(dtmf rune) error {
			return dtmfWriter.WriteDTMF(dtmf)
		})
	}

	med.StopRTP = func() error {
		return m.mediaSession.StopRTP(0, 0)
	}

	med.StartRTP = func() error {
		return m.mediaSession.StartRTP(0)
	}

	return b.AddAudioMedia(&med)
}

func (b *Bridge) AddAudioMedia(m *BridgeAudioMedia) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Check can this dialog be added to bridge. NO TRANSCODING
	if b.originator != nil {
		// This may look ugly but it is safe way of reading
		origProps := b.originator.ReaderProps
		mprops := m.ReaderProps

		if err := bridgeCodecErr(origProps.Codec, mprops.Codec); err != nil {
			return err
		}
	}

	// The media joins once the checks below pass, so a refused one is not
	// left in the bridge.
	dialogs := append(slices.Clone(b.dialogs), m)
	if len(dialogs) >= b.WaitDialogsNum {
		if len(dialogs) > 2 {
			return fmt.Errorf("currently bridge only support 2 party")
		}
		// Check are both answered
		for _, m := range dialogs {
			// TODO remove this double locking. Read once
			if m.Reader == nil || m.Writer == nil {
				return fmt.Errorf("dialog session not answered?")
			}
		}
	}

	b.dialogs = dialogs
	if len(b.dialogs) == 1 {
		b.originator = m
	}

	if len(b.dialogs) < b.WaitDialogsNum {
		return nil
	}

	// Nothing hands this proxy a context: StopProxyMedia ends the one it
	// runs on.
	ctx, cancel := context.WithCancel(context.Background())
	proxy := newBridgeProxy(cancel)
	b.proxy = proxy
	go func() {
		defer close(proxy.done)
		defer cancel()
		defer func(start time.Time) {
			b.log.Debug("Proxy media setup", "dur", time.Since(start).String())
		}(time.Now())
		m1 := dialogs[0]
		m2 := dialogs[1]
		proxy.err = b.proxyMediaChannels(ctx, m1, m2)
		if proxyEndedWithMedia(proxy.err) {
			b.log.Debug("Proxy media stopped", "error", proxy.err)
			return
		}
		b.log.Error("Proxy media stopped", "error", proxy.err)
	}()
	return nil
}

// ProxyMedia is explicit call to start proxing media.
//
// It returns once the media of either party is closed, as a call's end closes
// it, once ctx ends, and with an error once a re-INVITE moves either party to
// a codec the other does not have. The medias can be read again by then. A
// proxy that ctx ended returns nil.
func (b *Bridge) ProxyMedia(ctx context.Context) error {
	m1, m2, err := b.proxyParties()
	if err != nil {
		return err
	}

	defer func(start time.Time) {
		b.log.Debug("Proxy media setup", "dur", time.Since(start).String())
	}(time.Now())
	if err := b.proxyMediaChannels(ctx, m1, m2); err != nil {
		if errors.Is(err, io.EOF) {
			return nil
		}

		return err
	}
	return nil
}

func (b *Bridge) proxyMediaChannels(ctx context.Context, m1, m2 *BridgeAudioMedia) error {
	log := b.log

	// The codecs are watched before the proxy starts, so a media update made
	// meanwhile is not missed.
	mismatch, unwatch := watchBridgeCodecs(m1, m2)
	defer unwatch()
	select {
	case err := <-mismatch:
		return err
	default:
	}
	passDTMF(m1, m2)
	passDTMF(m2, m1)

	// Lets for now simplify proxy and later optimize
	errCh := make(chan error, 2)
	func() {
		p1, p2 := m1.ReaderProps, m2.WriterProps
		r := m1.Reader
		w := m2.Writer

		log := log.With("from", p1.Raddr+" > "+p1.Laddr, "to", p2.Laddr+" > "+p2.Raddr)
		log.Debug("Starting proxy media routine")
		go proxyMediaBackground(log, r, w, errCh)
	}()

	// Second
	func() {
		p1, p2 := m2.ReaderProps, m1.WriterProps
		r := m2.Reader
		w := m1.Writer

		log := log.With("from", p1.Raddr+" > "+p1.Laddr, "to", p2.Laddr+" > "+p2.Raddr)
		log.Debug("Starting proxy media routine")
		go proxyMediaBackground(log, r, w, errCh)
	}()

	// Wait for all to finish, ending both once either ends or on cancelation
	return proxyWait(ctx, []*BridgeAudioMedia{m1, m2}, errCh, mismatch)
}
