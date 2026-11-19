// SPDX-License-Identifier: MPL-2.0

package media

import (
	"errors"
	"net"
	"os"
)

// webRTCReadTimeoutError is what an RTP read of a MediaSessionWebrtc returns
// when a read deadline passes. The transport reports it with an error of its
// own, which this keeps, and which a read of a MediaSession's socket reports
// as os.ErrDeadlineExceeded; it matches both, so a caller that ends a read
// with StopRTP tells that end apart the same way on both media types.
type webRTCReadTimeoutError struct {
	err error
}

func (e *webRTCReadTimeoutError) Error() string   { return e.err.Error() }
func (e *webRTCReadTimeoutError) Timeout() bool   { return true }
func (e *webRTCReadTimeoutError) Temporary() bool { return true }
func (e *webRTCReadTimeoutError) Unwrap() []error {
	return []error{e.err, os.ErrDeadlineExceeded}
}

// webRTCReadErr returns err, a read of the session's RTP stream failed with,
// as a webRTCReadTimeoutError when it is a timeout that does not already
// match os.ErrDeadlineExceeded.
func webRTCReadErr(err error) error {
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() || errors.Is(err, os.ErrDeadlineExceeded) {
		return err
	}
	return &webRTCReadTimeoutError{err: err}
}
