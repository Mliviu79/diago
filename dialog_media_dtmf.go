// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"sync"
	"time"

	"github.com/emiago/diago/media"
)

// negotiatedDTMFCodec returns the codec DTMF is read on, when receive is set,
// or written on, for a media session that negotiated common and has own as
// its own codec list.
//
// It is telephone-event/8000 as negotiated, at the payload type the call runs
// it on. Without it, it is the telephone-event the session does have: one
// negotiated at another clock rate, or the one of its own list while nothing
// is negotiated yet. A session with none at all gets the package default,
// payload type 101, so a call whose peer offered no telephone-event still has
// a DTMF reader and writer. The reader then takes for DTMF only what arrives
// on that number.
func negotiatedDTMFCodec(common, own []media.Codec, receive bool) media.Codec {
	for _, codec := range common {
		if !codec.Matches(media.CodecTelephoneEvent8000) {
			continue
		}
		if receive {
			codec.PayloadType = codec.PayloadTypeRecv()
		}
		return codec
	}
	listed := common
	if len(listed) == 0 {
		listed = own
	}
	codec, ok := media.CodecTelephoneEventFromList(listed)
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
	r.follow = &dtmfMediaFollow[*media.MediaSession]{dialog: d, receive: true, target: r.dtmfReader, installed: d.mediaSession}
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
	w.follow = &dtmfMediaFollow[*media.MediaSession]{dialog: d, target: w.dtmfWriter, installed: d.mediaSession}
	return nil
}

// dtmfCodecSince returns the media session the dialog runs on and the
// telephone-event codec on it, when that session is not since. ok is false
// when it is since, or when the dialog has no media session.
func (d *DialogMedia) dtmfCodecSince(since *media.MediaSession, receive bool) (sess *media.MediaSession, codec media.Codec, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	sess = d.mediaSession
	if sess == since {
		return nil, media.Codec{}, false
	}
	codec, err := d.dtmfCodec(receive)
	if err != nil {
		// The dialog has no media session to follow.
		return nil, media.Codec{}, false
	}
	return sess, codec, true
}

// dtmfMedia is the media of a dialog as a DTMF reader or writer follows it.
// S tells apart the media installs a codec is taken from: the media session
// itself where every update installs a new one, a count of installs where an
// update may change the session in place.
type dtmfMedia[S comparable] interface {
	// dtmfCodecSince returns the install in place and the telephone-event
	// codec on it, receive selecting the number the peer sends on, when that
	// install is not since. ok is false when it is since, or when there is no
	// media.
	dtmfCodecSince(since S, receive bool) (installed S, codec media.Codec, ok bool)
}

// dtmfFollower moves a DTMF reader or writer to the telephone-event codec of
// the media its dialog runs on.
type dtmfFollower interface {
	update()
}

// dtmfMediaFollow keeps a DTMF reader or writer on the telephone-event codec
// of the media its dialog runs on. telephone-event is a dynamic format, and
// the offer behind a media update may carry it at another number and drop the
// old one, which the answerer then must not send (RFC 3264 section 8.3.2).
type dtmfMediaFollow[S comparable] struct {
	dialog dtmfMedia[S]
	// receive is set for a reader, which takes the number the peer sends on.
	receive bool
	// target is the RTP DTMF reader or writer kept on that codec.
	target interface{ UpdateCodec(codec media.Codec) }

	// mu guards installed and keeps the codec changes made for it in the
	// order of the installs they were made for.
	mu sync.Mutex
	// installed is the media install the codec in use was taken from.
	installed S
}

// update gives the target the telephone-event codec of the dialog's current
// media when a media update has replaced the install the codec in use was
// taken from.
func (f *dtmfMediaFollow[S]) update() {
	f.mu.Lock()
	defer f.mu.Unlock()

	installed, codec, ok := f.dialog.dtmfCodecSince(f.installed, f.receive)
	if !ok {
		return
	}
	f.installed = installed
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
	dialog dialogRTPControl
}

func (a dialogRTPDeadline) StopRTP(rw int8, dur time.Duration) error {
	return a.dialog.StopRTP(rw, dur)
}

func (a dialogRTPDeadline) StartRTP(rw int8) error {
	return a.dialog.StartRTP(rw, 0)
}
