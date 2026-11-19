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
	dialog *DialogMedia
	hook   media.OnRTPReadStats
}

func (r *rtpStatsReader) Read(b []byte) (int, error) {
	n, err := r.reader.Read(b)
	if err != nil {
		return n, err
	}
	if sess := r.dialog.RTPSession(); sess != nil {
		r.hook(sess.ReadStats())
	}
	return n, nil
}

// rtpStatsWriter passes the write statistics of the dialog's current RTP
// session to hook after each write, as rtpStatsReader does for reads.
type rtpStatsWriter struct {
	writer io.Writer
	dialog *DialogMedia
	hook   media.OnRTPWriteStats
}

func (w *rtpStatsWriter) Write(b []byte) (int, error) {
	n, err := w.writer.Write(b)
	if err != nil {
		return n, err
	}
	if sess := w.dialog.RTPSession(); sess != nil {
		w.hook(sess.WriteStats())
	}
	return n, nil
}
