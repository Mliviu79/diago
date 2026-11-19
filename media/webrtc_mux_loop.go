// SPDX-License-Identifier: MPL-2.0

package media

import (
	"errors"
	"sync"
)

// webRTCMuxLoop is what a webRTCPacketMux keeps about its read loop, so that
// Close can wait for the loop and report what the loop closed.
type webRTCMuxLoop struct {
	// wg counts the read loop.
	wg sync.WaitGroup
	// closeErr is what release returned to the read loop. The loop writes it
	// before it ends, and Close reads it only after waiting for the loop.
	closeErr error
}

// Close closes the three endpoints and the connection, and returns once the
// read loop has ended. The loop reads the connection and writes the endpoint
// buffers, so an owner that goes on to close the ICE agent under the
// connection, or to run another mux over it, must know that nothing of this
// one still runs. Closing the connection is what ends the loop's parked read.
//
// Readers parked on an endpoint return with an error. Close may be called more
// than once and from several goroutines; every call waits for the loop.
func (m *webRTCPacketMux) Close() error {
	err := m.release()
	m.loop.wg.Wait()
	// The loop closes the mux itself when its connection dies first; what
	// that close returned would otherwise be lost, since release closes once.
	return errors.Join(err, m.loop.closeErr)
}

// readEnded is the read loop's exit: the connection can no longer be read, so
// nothing more will reach the endpoints. Any read error is final. Carrying on
// would spin the loop on a failed connection for the rest of the call, so the
// mux is closed, which releases every reader parked on an endpoint.
func (m *webRTCPacketMux) readEnded(err error) {
	DefaultLogger().Debug("WebRTC packet mux read loop stopped", "error", err)
	m.loop.closeErr = m.release()
}
