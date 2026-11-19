// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"fmt"
	"io"
	"time"

	"github.com/emiago/diago/media"
)

// bridgeMixMedia is the media of a call a BridgeMix mixes: a DialogMedia or a
// DialogWebrtc.
type bridgeMixMedia interface {
	bridgeDialog
	// StopRTP and StartRTP set and clear the read deadline the mix stops
	// and paces its reads with, on the media session in use.
	StopRTP(rw int8, dur time.Duration) error
	StartRTP(rw int8, dur time.Duration) error
	// SetAudioReader sets the media's audio reader.
	SetAudioReader(r io.Reader)
	// audioReaderWriterProps returns the audio reader and writer, and in p
	// the props of the media session they run on, read together.
	audioReaderWriterProps(p *MediaProps) (io.Reader, io.Writer, error)
	// packetReader returns the media's RTP packet reader.
	packetReader() *media.RTPPacketReader
	// packetWriterSSRC returns the SSRC of the media's RTP packet writer.
	packetWriterSSRC() uint32
}

// bridgeMixCodec returns the codec of the media session m runs on, and the
// zero codec when m has none.
func bridgeMixCodec(m bridgeMixMedia) media.Codec {
	p, err := m.NegotiatedProps()
	if err != nil {
		return media.Codec{}
	}
	return p.Codec
}

// bridgeMixList returns the medias of type T in the bridge, in the order they
// joined.
func bridgeMixList[T bridgeMixMedia](b *BridgeMix) []T {
	b.mu.Lock()
	defer b.mu.Unlock()
	var list []T
	for _, m := range b.medias {
		if t, ok := m.(T); ok {
			list = append(list, t)
		}
	}
	return list
}

// AddDialogWebrtc adds m, the media of a WebRTC call, to the bridge and mixes
// its audio with that of the medias already in it, as AddDialogMedia does for
// a DialogMedia: medias of both types are mixed together. It refuses the same
// medias AddDialogMedia refuses, a media without a media session with
// ErrNoMediaSetup.
func (b *BridgeMix) AddDialogWebrtc(m *DialogWebrtc) error {
	if m == nil {
		return fmt.Errorf("dialog media is nil")
	}
	return b.addMedia(m)
}

// RemoveDialogWebrtc takes m out of the bridge, as RemoveDialogMedia does for
// a DialogMedia.
func (b *BridgeMix) RemoveDialogWebrtc(m *DialogWebrtc) error {
	return b.removeMedia(m)
}

// DialogWebrtcList returns the WebRTC medias in the bridge. As with
// DialogMediaList, a media is not safe to use until it is removed from the
// bridge.
func (b *BridgeMix) DialogWebrtcList() []*DialogWebrtc {
	return bridgeMixList[*DialogWebrtc](b)
}

func (d *DialogMedia) packetReader() *media.RTPPacketReader {
	return d.RTPPacketReader
}

func (d *DialogMedia) packetWriterSSRC() uint32 {
	return d.RTPPacketWriter.SSRC
}

func (d *DialogWebrtc) packetReader() *media.RTPPacketReader {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.RTPPacketReader
}

func (d *DialogWebrtc) packetWriterSSRC() uint32 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.RTPPacketWriter.SSRC
}

// audioReaderWriterProps returns the media's audio reader and writer, and in
// p the props of the transport they run on, read in one hold of the lock, as
// DialogMedia.audioReaderWriterProps does. Before the media is set up it
// returns ErrNoMediaSetup.
func (d *DialogWebrtc) audioReaderWriterProps(p *MediaProps) (io.Reader, io.Writer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.RTPPacketReader == nil || d.RTPPacketWriter == nil {
		return nil, nil, ErrNoMediaSetup
	}
	if err := WithAudioReaderWebrtcProps(p)(d); err != nil {
		return nil, nil, err
	}
	return d.audioReaderUnsafe(), d.audioWriterUnsafe(), nil
}
