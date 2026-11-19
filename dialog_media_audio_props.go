// SPDX-License-Identifier: MPL-2.0

package diago

import "io"

// audioReaderWriterProps returns the media's audio reader and writer, and in p
// the props of the media session they run on, read in one hold of the lock, so
// a re-INVITE can not give the reader and the writer different codecs. Before
// the media is set up it returns ErrNoMediaSetup.
func (d *DialogMedia) audioReaderWriterProps(p *MediaProps) (io.Reader, io.Writer, error) {
	d.mu.Lock()
	defer d.mu.Unlock()

	if err := WithAudioReaderMediaProps(p)(d); err != nil {
		return nil, nil, err
	}
	r, w := d.getAudioReader(), d.getAudioWriter()
	if r == nil || w == nil {
		return nil, nil, ErrNoMediaSetup
	}
	return r, w, nil
}
