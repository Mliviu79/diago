// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"io"

	"github.com/emiago/diago/media"
)

// rtpStatsReader passes the read statistics of the dialog's current RTP
// session to hook after each read. A media update replaces that session with
// one that carries the statistics on, so a reader bound to the session it was
// installed on would report counts that stopped at the update.
type rtpStatsReader struct {
	reader io.Reader
	// stats returns the read statistics of the dialog's current RTP session,
	// and false when the dialog has none.
	stats func() (media.RTPReadStats, bool)
	hook  media.OnRTPReadStats
}

func (r *rtpStatsReader) Read(b []byte) (int, error) {
	n, err := r.reader.Read(b)
	if err != nil {
		return n, err
	}
	if stats, ok := r.stats(); ok {
		r.hook(stats)
	}
	return n, nil
}

// rtpStatsWriter passes the write statistics of the dialog's current RTP
// session to hook after each write, as rtpStatsReader does for reads.
type rtpStatsWriter struct {
	writer io.Writer
	// stats returns the write statistics of the dialog's current RTP
	// session, and false when the dialog has none.
	stats func() (media.RTPWriteStats, bool)
	hook  media.OnRTPWriteStats
}

func (w *rtpStatsWriter) Write(b []byte) (int, error) {
	n, err := w.writer.Write(b)
	if err != nil {
		return n, err
	}
	if stats, ok := w.stats(); ok {
		w.hook(stats)
	}
	return n, nil
}

// rtpReadStats returns the read statistics of the dialog's current RTP
// session, and false when it has none.
func (d *DialogMedia) rtpReadStats() (media.RTPReadStats, bool) {
	sess := d.RTPSession()
	if sess == nil {
		return media.RTPReadStats{}, false
	}
	return sess.ReadStats(), true
}

// rtpWriteStats returns the write statistics of the dialog's current RTP
// session, and false when it has none.
func (d *DialogMedia) rtpWriteStats() (media.RTPWriteStats, bool) {
	sess := d.RTPSession()
	if sess == nil {
		return media.RTPWriteStats{}, false
	}
	return sess.WriteStats(), true
}
