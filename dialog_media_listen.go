// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"time"

	"github.com/emiago/diago/media"
)

// dialogRTPControl sets and clears the RTP read and write deadlines of a
// dialog's media, on the media session the dialog runs on when it is called,
// as DialogMedia.StopRTP and DialogMedia.StartRTP do. Both media types have it,
// so the helpers that stop a read or a write with a deadline serve both.
type dialogRTPControl interface {
	StopRTP(rw int8, dur time.Duration) error
	StartRTP(rw int8, dur time.Duration) error
}

// listenReader reads r until a read fails, and returns that failure.
func listenReader(r io.Reader) error {
	buf := make([]byte, media.RTPBufSize)
	for {
		if _, err := r.Read(buf); err != nil {
			return err
		}
	}
}

// listenBackground reads r on a goroutine of its own until a read fails. The
// returned stop ends the reading with a read deadline set through rtp, waits
// for the goroutine to return, clears the deadline again, and returns what
// ended the reading unless that was a timeout.
func listenBackground(r io.Reader, rtp dialogRTPControl) (stop func() error) {
	buf := make([]byte, media.RTPBufSize)
	wg := sync.WaitGroup{}
	var readErr error
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, err := r.Read(buf)
			if err != nil {
				if err, ok := err.(net.Error); ok && err.Timeout() {
					return
				}
				readErr = err
				return
			}
		}
	}()

	return func() error {
		if err := rtp.StopRTP(1, 0); err != nil {
			return err
		}
		wg.Wait() // This makes sure we have exited reading
		if err := rtp.StartRTP(1, 0); err != nil {
			return err
		}
		return readErr
	}
}

// listenContext reads r until ctx ends or a read fails. The ctx's end stops
// the read with a read deadline set through rtp, which is cleared again
// before listenContext returns, and listenContext then returns nil.
func listenContext(ctx context.Context, r io.Reader, rtp dialogRTPControl) (err error) {
	buf := make([]byte, media.RTPBufSize)
	restoreReads := stopReadsAtEnd(ctx, rtp)
	defer func() {
		if restoreErr := restoreReads(); err == nil {
			err = restoreErr
		}
	}()
	for {
		_, err := r.Read(buf)
		if err != nil {
			if err, ok := err.(net.Error); ok && err.Timeout() {
				return nil
			}
			return err
		}
	}
}

// listenUntil reads r for dur, and returns the read deadline's error when dur
// has passed. The read deadline, set through rtp, is cleared again before it
// returns.
func listenUntil(r io.Reader, rtp dialogRTPControl, dur time.Duration) (err error) {
	buf := make([]byte, media.RTPBufSize)
	if err := rtp.StopRTP(1, dur); err != nil {
		return err
	}
	defer func() {
		if startErr := rtp.StartRTP(1, 0); startErr != nil && errors.Is(err, os.ErrDeadlineExceeded) {
			err = errors.Join(err, startErr)
		}
	}()
	for {
		_, err := r.Read(buf)
		if err != nil {
			return err
		}
	}
}

// stopReadsAtEnd ends a read of the dialog's media when ctx ends, with a read
// deadline set through rtp at that moment on the media session in use.
//
// The caller calls the function returned once it reads no more. When ctx has
// ended, that waits for the deadline to be set and clears it again, so the
// next read of the dialog does not fail on it, and returns what setting or
// clearing it failed with. When ctx has not ended, no deadline was set, and
// none is set later.
func stopReadsAtEnd(ctx context.Context, rtp dialogRTPControl) (restore func() error) {
	stopped := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() { stopped <- rtp.StopRTP(1, 0) })
	return func() error {
		if stop() {
			return nil
		}
		return errors.Join(<-stopped, rtp.StartRTP(1, 0))
	}
}
