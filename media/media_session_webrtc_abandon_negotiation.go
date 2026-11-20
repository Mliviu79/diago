// SPDX-License-Identifier: MPL-2.0

package media

import (
	"fmt"

	"github.com/pion/dtls/v3"
)

// AbandonNegotiation ends the ICE and DTLS negotiation FinalizeWithin left
// running, and returns once it has returned. A session that left none
// running is left as it is.
//
// It is for an answer that another answer to the same offer replaces before
// the call is answered: a UAC takes the answer of each early dialog its offer
// creates (RFC 3261 sections 12.1 and 13.2.1), and the call goes on with the
// dialog the 2xx confirms. The negotiation of the replaced answer reads and
// writes what RemoteSDP reads and writes, so it ends before that dialog's
// answer is applied.
//
// What the ended negotiation came to is not kept: the next Finalize
// negotiates afresh for the answer applied after this call. A negotiation
// that keyed the session before it was ended leaves it keyed, and an answer
// with the same ICE credentials and fingerprints continues it. One that did
// not leaves the ICE agent checking, as a Finalize that gave up leaves it,
// and the DTLS conn it ended is replaced by a fresh one on the same transport
// and with the same configuration, which the next Finalize runs a handshake
// on, and the answer applied next is matched against every codec of the
// offer, as the first answer was. An answer with other ICE credentials needs
// an ICE restart, which RemoteSDP refuses.
//
// It must not be called beside a Finalize or a FinalizeWithin of the session.
func (m *MediaSessionWebrtc) AbandonNegotiation() error {
	if !m.leftRunning.abandon() {
		return nil
	}

	m.mu.Lock()
	if m.closed || m.ready || m.dtlsConn == nil {
		m.mu.Unlock()
		return nil
	}
	m.restoreSetupCodecsLocked()
	ended, endedTr := m.dtlsConn, m.dtlsTr
	tr := m.dtlsTransport()
	var conn *dtls.Conn
	var err error
	if m.localSetup == dtlsSetupActive {
		conn, err = dtls.Client(tr, m.iceConn.RemoteAddr(), m.dtlsLibConf)
	} else {
		conn, err = dtls.Server(tr, m.iceConn.RemoteAddr(), m.dtlsLibConf)
	}
	if err != nil {
		m.dtlsTr = endedTr
		m.mu.Unlock()
		return fmt.Errorf("create DTLS transport: %w", err)
	}
	m.dtlsConn = conn
	m.mu.Unlock()

	// The ended conn read and wrote the DTLS stream of the mux the fresh one
	// now holds: it is retired without a word to the peer, as a conn whose
	// handshake completed is.
	return endedTr.retire(ended)
}
