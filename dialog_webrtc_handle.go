// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"io"
	"slices"
	"time"

	"github.com/emiago/diago/media"
)

// dialogWebrtcState is the state DialogWebrtc keeps beside its transport for
// the media handle: the close latch, the media update callbacks and the count
// of media installs. It is guarded by DialogWebrtc.mu.
type dialogWebrtcState struct {
	// closeDone is made by the first Close and closed once that Close has
	// released the media, so a Close made after it returns only then.
	closeDone chan struct{}

	// onMediaUpdate is the application's callback, see OnMediaUpdate.
	onMediaUpdate func(*DialogWebrtc)
	// mediaUpdateHooks are told of every media install before
	// onMediaUpdate, see addMediaUpdateHook.
	mediaUpdateHooks mediaUpdateHooks

	// installs counts the media updates installed since Init. A peer's offer
	// that changes no ICE credentials is applied to the media session in
	// place, so the session alone does not tell one install from the next.
	installs uint64
}

var _ MediaHandle = (*DialogWebrtc)(nil)

// StopRTP sets a deadline dur from now on the dialog's RTP reads (bit 1 of
// rw), its writes (bit 2 without bit 1) or both (neither bit), on the media
// session the dialog runs on, as DialogMedia.StopRTP does. dur 0 sets it at
// once, and so ends a read or write waiting on the transport. It holds the
// dialog's lock while it does, since a media update replaces the session under
// it, so the deadline is set either on the session replaced, which hands it to
// its replacement, or on the replacement.
//
// The write deadline is that of the candidate pair ICE selected, which also
// carries the session's RTCP, DTLS and ICE packets, see
// media.MediaSessionWebrtc.SetRTPDeadline. Without a media session it returns
// ErrNoMediaSetup.
func (d *DialogWebrtc) StopRTP(rw int8, dur time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mediaSession == nil {
		return ErrNoMediaSetup
	}
	return d.mediaSession.SetRTPDeadline(rw, time.Now().Add(dur))
}

// StartRTP clears the deadline StopRTP sets for rw, on the media session the
// dialog runs on. Without a media session it returns ErrNoMediaSetup.
func (d *DialogWebrtc) StartRTP(rw int8, _ time.Duration) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.mediaSession == nil {
		return ErrNoMediaSetup
	}
	return d.mediaSession.SetRTPDeadline(rw, time.Time{})
}

// NegotiatedProps returns what AudioReader fills with
// WithAudioReaderWebrtcProps: the codec of the media session in use and the
// addresses of the candidate pair ICE selected, read under the lock a media
// update takes. Without a media session it returns ErrNoMediaSetup.
func (d *DialogWebrtc) NegotiatedProps() (MediaProps, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := MediaProps{}
	if err := WithAudioReaderWebrtcProps(&p)(d); err != nil {
		return MediaProps{}, err
	}
	return p, nil
}

// Reader returns the audio reader AudioReader returns with no option.
func (d *DialogWebrtc) Reader() (io.Reader, error) {
	return d.AudioReader()
}

// Writer returns the audio writer AudioWriter returns with no option.
func (d *DialogWebrtc) Writer() (io.Writer, error) {
	return d.AudioWriter()
}

// Listen keeps reading the audio until a read fails, as DialogMedia.Listen
// does, and returns that failure.
func (d *DialogWebrtc) Listen() error {
	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}
	return listenReader(audioReader)
}

// ListenBackground reads the audio in the background, as
// DialogMedia.ListenBackground does. The returned stop ends the reading with a
// read deadline, waits for it to end and clears the deadline again.
func (d *DialogWebrtc) ListenBackground() (stop func() error, err error) {
	audioReader, err := d.AudioReader()
	if err != nil {
		return nil, err
	}
	return listenBackground(audioReader, d), nil
}

// ListenContext reads the audio until ctx ends, as DialogMedia.ListenContext
// does. The context's end stops the read with a read deadline, which is
// cleared again before ListenContext returns, and ListenContext then returns
// nil.
func (d *DialogWebrtc) ListenContext(ctx context.Context) error {
	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}
	return listenContext(ctx, audioReader, d)
}

// ListenUntil reads the audio for dur, and returns the read deadline's error
// when dur has passed. The read deadline is cleared again before it returns.
func (d *DialogWebrtc) ListenUntil(dur time.Duration) error {
	audioReader, err := d.AudioReader()
	if err != nil {
		return err
	}
	return listenUntil(audioReader, d, dur)
}

// PlaybackControlCreate creates playback with controls like mute and unmute,
// over PlaybackCreate.
func (d *DialogWebrtc) PlaybackControlCreate() (AudioPlaybackControl, error) {
	p, err := d.PlaybackCreate()
	if err != nil {
		return AudioPlaybackControl{}, err
	}
	return NewAudioPlaybackControl(p), nil
}

// PlaybackRingtoneCreate creates playback of a ringtone in the negotiated
// codec. Its stop sets and clears the dialog's write deadline, on the media
// session the dialog runs on then.
//
// Experimental
func (d *DialogWebrtc) PlaybackRingtoneCreate() (AudioRingtone, error) {
	props := MediaProps{}
	w, err := d.AudioWriter(WithAudioWriterWebrtcProps(&props))
	if err != nil {
		return AudioRingtone{}, err
	}
	return newAudioRingtone(w, props.Codec, d)
}

// WithAudioReaderWebrtcRTPStats creates an RTP statistics interceptor on the
// audio reader. The statistics are those of the dialog's current RTP session,
// which a media update replaces with one that carries them on. Without an RTP
// session the option returns ErrNoMediaSetup.
func WithAudioReaderWebrtcRTPStats(hook media.OnRTPReadStats) AudioReaderWebrtcOption {
	return func(d *DialogWebrtc) error {
		if d.rtpSession == nil {
			return ErrNoMediaSetup
		}
		d.audioReader = &rtpStatsReader{
			reader: d.audioReaderUnsafe(),
			stats:  d.rtpReadStats,
			hook:   hook,
		}
		return nil
	}
}

// WithAudioWriterWebrtcRTPStats creates an RTP statistics interceptor on the
// audio writer, as WithAudioReaderWebrtcRTPStats does on the reader.
func WithAudioWriterWebrtcRTPStats(hook media.OnRTPWriteStats) AudioWriterWebrtcOption {
	return func(d *DialogWebrtc) error {
		if d.rtpSession == nil {
			return ErrNoMediaSetup
		}
		d.audioWriter = &rtpStatsWriter{
			writer: d.audioWriterUnsafe(),
			stats:  d.rtpWriteStats,
			hook:   hook,
		}
		return nil
	}
}

// rtpReadStats returns the read statistics of the dialog's current RTP
// session, and false when it has none.
func (d *DialogWebrtc) rtpReadStats() (media.RTPReadStats, bool) {
	sess := d.RTPSession()
	if sess == nil {
		return media.RTPReadStats{}, false
	}
	return sess.ReadStats(), true
}

// rtpWriteStats returns the write statistics of the dialog's current RTP
// session, and false when it has none.
func (d *DialogWebrtc) rtpWriteStats() (media.RTPWriteStats, bool) {
	sess := d.RTPSession()
	if sess == nil {
		return media.RTPWriteStats{}, false
	}
	return sess.WriteStats(), true
}

// OnMediaUpdate has f called after each media update the dialog installs:
// a peer's offer applied to the transport in place, a new transport put in
// place of the old one, and the answer to an offer of ours. f runs once the
// update is in place, after the media update hooks, with the media's lock
// released, on the goroutine that handled the update, which a re-INVITE may
// hold until f returns. An update that is only staged, waiting for its
// exchange to complete, and one that is aborted, call nothing. f replaces
// whatever an earlier call set; a nil f calls nothing.
func (d *DialogWebrtc) OnMediaUpdate(f func(*DialogWebrtc)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.onMediaUpdate = f
}

// addMediaUpdateHook has f called after every media update the dialog
// installs, before OnMediaUpdate, as DialogMedia.addMediaUpdateHook does. It
// returns the function that removes f again.
func (d *DialogWebrtc) addMediaUpdateHook(f func()) (remove func()) {
	d.mu.Lock()
	h := d.mediaUpdateHooks.add(f)
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.mediaUpdateHooks.remove(h)
		d.mu.Unlock()
	}
}

// noteInstallUnsafe counts a media update that has just been put in place.
// Must be called with d.mu held, in the same critical section as the install.
func (d *DialogWebrtc) noteInstallUnsafe() {
	d.installs++
}

// mediaInstalled tells the media update hooks and then OnMediaUpdate of a
// media update put in place, with the media's lock released. It is called
// from the two places a media update is installed, commitCurrentMediaSession
// and replaceMediaSession, once each per install.
func (d *DialogWebrtc) mediaInstalled() {
	d.mu.Lock()
	hooks := slices.Clone(d.mediaUpdateHooks)
	onMediaUpdate := d.onMediaUpdate
	d.mu.Unlock()
	hooks.run()
	if onMediaUpdate != nil {
		onMediaUpdate(d)
	}
}

// dtmfCodecUnsafe returns the telephone-event codec DTMF is read on, when
// receive is set, or written on, for the media session in use, as
// DialogMedia.dtmfCodec does. Must be called with d.mu held.
func (d *DialogWebrtc) dtmfCodecUnsafe(receive bool) (media.Codec, error) {
	if d.mediaSession == nil {
		return media.Codec{}, ErrNoMediaSetup
	}
	return negotiatedDTMFCodec(d.mediaSession.CommonCodecs(), d.mediaSession.Codecs, receive), nil
}

// dtmfCodecSince returns the count of media installs and the telephone-event
// codec of the media in use, when an install was made after since. ok is false
// when none was, or when the dialog has no media session.
func (d *DialogWebrtc) dtmfCodecSince(since uint64, receive bool) (installs uint64, codec media.Codec, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.installs == since {
		return 0, media.Codec{}, false
	}
	codec, err := d.dtmfCodecUnsafe(receive)
	if err != nil {
		return 0, media.Codec{}, false
	}
	return d.installs, codec, true
}
