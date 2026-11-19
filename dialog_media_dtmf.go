// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"sync"
	"time"

	"github.com/emiago/diago/media"
)

// dtmfCodecWithoutNegotiated8000 returns the codec DTMF is read on, when
// receive is set, or written on, for a media session that negotiated no
// telephone-event/8000.
//
// It is the telephone-event the session does have: one negotiated at another
// clock rate, or the one of its own list while nothing is negotiated yet. A
// session with none at all gets the package default, payload type 101, so a
// call whose peer offered no telephone-event still has a DTMF reader and
// writer. The reader then takes for DTMF only what arrives on that number.
func dtmfCodecWithoutNegotiated8000(s *media.MediaSession, receive bool) media.Codec {
	codec, ok := media.CodecTelephoneEventFromSession(s)
	if !ok {
		return media.CodecTelephoneEvent8000
	}
	if receive {
		codec.PayloadType = codec.PayloadTypeRecv()
	}
	return codec
}

// initDTMFReaderUnsafe sets r up over the dialog's audio reader. r reads DTMF
// on the telephone-event payload type of the media session in use and follows
// the sessions that replace it, and sets its read deadlines on the session in
// use at that moment. Before the media is set up it returns ErrNoMediaSetup.
// Must be called with d.mu held.
func (d *DialogMedia) initDTMFReaderUnsafe(r *DTMFReader) error {
	ar := d.getAudioReader()
	if ar == nil || d.RTPPacketReader == nil {
		return ErrNoMediaSetup
	}
	codec, err := d.dtmfCodec(true)
	if err != nil {
		return err
	}
	r.dtmfReader = media.NewRTPDTMFReader(codec, d.RTPPacketReader, ar)
	r.rtpDeadline = dialogRTPDeadline{dialog: d}
	r.follow = &dtmfMediaFollow{dialog: d, receive: true, target: r.dtmfReader, mediaSession: d.mediaSession}
	return nil
}

// initDTMFWriterUnsafe sets w up over the dialog's audio writer. w writes DTMF
// on the telephone-event payload type of the media session in use and follows
// the sessions that replace it. Before the media is set up it returns
// ErrNoMediaSetup. Must be called with d.mu held.
func (d *DialogMedia) initDTMFWriterUnsafe(w *DTMFWriter) error {
	aw := d.getAudioWriter()
	if aw == nil || d.RTPPacketWriter == nil {
		return ErrNoMediaSetup
	}
	codec, err := d.dtmfCodec(false)
	if err != nil {
		return err
	}
	w.dtmfWriter = media.NewRTPDTMFWriter(codec, d.RTPPacketWriter, aw)
	w.follow = &dtmfMediaFollow{dialog: d, target: w.dtmfWriter, mediaSession: d.mediaSession}
	return nil
}

// dtmfMediaFollow keeps a DTMF reader or writer on the telephone-event codec
// of the media session its dialog runs on. telephone-event is a dynamic
// format, and the offer behind a media update may carry it at another number
// and drop the old one, which the answerer then must not send (RFC 3264
// section 8.3.2).
type dtmfMediaFollow struct {
	dialog *DialogMedia
	// receive is set for a reader, which takes the number the peer sends on.
	receive bool
	// target is the RTP DTMF reader or writer kept on that codec.
	target interface{ UpdateCodec(codec media.Codec) }

	// mu guards mediaSession and keeps the codec changes made for it in the
	// order of the sessions they were made for.
	mu sync.Mutex
	// mediaSession is the media session the codec in use was taken from.
	mediaSession *media.MediaSession
}

// update gives the target the telephone-event codec of the dialog's current
// media session when a media update has replaced the one the codec in use was
// taken from.
func (f *dtmfMediaFollow) update() {
	f.mu.Lock()
	defer f.mu.Unlock()

	d := f.dialog
	d.mu.Lock()
	sess := d.mediaSession
	if sess == f.mediaSession {
		d.mu.Unlock()
		return
	}
	codec, err := d.dtmfCodec(f.receive)
	d.mu.Unlock()
	if err != nil {
		// The dialog has no media session to follow.
		return
	}
	f.mediaSession = sess
	f.target.UpdateCodec(codec)
}

// followMedia moves the reader to the telephone-event codec of the media
// session its dialog runs on now. It runs on the reading goroutine: the codec
// must not change beside a read.
func (d *DTMFReader) followMedia() {
	if d.follow == nil {
		return
	}
	d.follow.update()
}

// followMedia moves the writer to the telephone-event codec of the media
// session its dialog runs on now.
func (w *DTMFWriter) followMedia() {
	if w.follow == nil {
		return
	}
	w.follow.update()
}

// dialogRTPDeadline sets a DTMF reader's read deadline on the media session
// its dialog runs on at that moment, under the dialog's lock. A reader that
// kept the session it was made on set the deadline on one a media update had
// replaced since.
type dialogRTPDeadline struct {
	dialog *DialogMedia
}

func (a dialogRTPDeadline) StopRTP(rw int8, dur time.Duration) error {
	return a.dialog.StopRTP(rw, dur)
}

func (a dialogRTPDeadline) StartRTP(rw int8) error {
	return a.dialog.StartRTP(rw, 0)
}
