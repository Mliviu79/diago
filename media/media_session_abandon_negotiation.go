// SPDX-License-Identifier: MPL-2.0

package media

// AbandonNegotiation ends the DTLS handshake of the answer the session took,
// the one FinalizeWithin left running or RemoteSDP armed, and returns once it
// has returned. The handshake is ended without a word to the peer, and the
// association it made, if it completed meanwhile, is dropped with its keys, so
// that nothing of it is carried to a fork. The sockets stay open.
//
// It is for an answer that another answer to the same offer replaces before
// the call is answered: a UAC takes the answer of each early dialog its offer
// creates (RFC 3261 sections 12.1 and 13.2.1), and the call goes on with the
// dialog the 2xx confirms. A fork of the session then takes that dialog's
// answer on the same sockets, which the offer named, and runs a handshake of
// its own; this session is done with and only its sockets are closed, by
// whoever closes the fork.
//
// The read loop of the ended DTLS stack may still be blocked on the RTP
// socket, and takes at most one more datagram off it before it sees the
// transport retired: a handshake that loses a flight to it sends it again
// (RFC 6347 section 4.2.4).
//
// It must not be called beside any other use of the session.
func (s *MediaSession) AbandonNegotiation() error {
	s.leftRunning.stop()
	s.onFinalize = nil
	var err error
	if s.dtlsTr != nil && s.dtlsConn != nil {
		err = s.retireDTLS()
	}
	s.dtlsAssoc = nil
	s.dtlsKeyed.Store(false)
	return err
}
