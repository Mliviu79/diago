// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"io"
	"time"
)

// MediaHandle is the media of a call as an application drives it: the
// negotiated audio, the encoded audio reader and writer, DTMF over RTP, the
// read and write deadlines that stop and resume the RTP sockets, and the
// close. It holds no SIP: an application gets it from the answer or the
// INVITE that set the media up, and drives every call leg through it the
// same way, whichever media type serves the leg.
//
// Each method acts on the media session the call runs on when it is called,
// so it follows the media updates a re-INVITE makes. Before the media is set
// up the getters and StopRTP and StartRTP return ErrNoMediaSetup.
type MediaHandle interface {
	// NegotiatedProps returns the negotiated audio codec and the local and
	// remote media addresses.
	NegotiatedProps() (MediaProps, error)
	// Reader returns the encoded audio reader.
	Reader() (io.Reader, error)
	// Writer returns the encoded audio writer.
	Writer() (io.Writer, error)
	// AudioReaderDTMF returns a reader of the audio that hands the DTMF it
	// reads to the callback set with OnDTMF.
	AudioReaderDTMF() (*DTMFReader, error)
	// AudioWriterDTMF returns a writer of the audio that also writes DTMF.
	AudioWriterDTMF() (*DTMFWriter, error)
	// StopRTP sets a deadline dur from now on the reads (bit 1 of rw), the
	// writes (bit 2) or both; dur 0 sets it in the past and so ends a read or
	// write waiting on the socket.
	StopRTP(rw int8, dur time.Duration) error
	// StartRTP clears the deadline StopRTP set.
	StartRTP(rw int8, dur time.Duration) error
	// Close closes the media and releases its sockets.
	Close() error
}

var _ MediaHandle = (*DialogMedia)(nil)

// NegotiatedProps returns what AudioReader fills with
// WithAudioReaderMediaProps: the codec of the media session in use and its
// local and remote addresses, read under the lock a media update takes.
// Without a media session it returns ErrNoMediaSetup.
func (d *DialogMedia) NegotiatedProps() (MediaProps, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p := MediaProps{}
	if err := WithAudioReaderMediaProps(&p)(d); err != nil {
		return MediaProps{}, err
	}
	return p, nil
}

// Reader returns the audio reader AudioReader returns with no option.
func (d *DialogMedia) Reader() (io.Reader, error) {
	return d.AudioReader()
}

// Writer returns the audio writer AudioWriter returns with no option.
func (d *DialogMedia) Writer() (io.Writer, error) {
	return d.AudioWriter()
}
