// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"time"

	"github.com/emiago/sipgo/sip"
)

// markAcking marks Ack in progress and returns what ends the mark, which Ack
// calls when it returns: once the media it finalizes is keyed, or has failed.
// Ack sets the mark before it sends the ACK, so a re-INVITE the peer sends as
// soon as that ACK reaches it finds the mark in place, see awaitAck.
func (s *sessionState) markAcking() (acked func()) {
	acking := make(chan struct{})
	s.answeringMu.Lock()
	s.acking = acking
	s.answeringMu.Unlock()
	return func() { close(acking) }
}

// awaitAck waits until Ack has returned before a re-INVITE of the peer's is
// handled, and reports whether it has. The peer may re-INVITE as soon as our
// ACK reaches it, while Ack still runs the DTLS handshake of the media the
// ACK started. A fork made then has no association to continue and would arm
// a second handshake on the socket the first one runs on; once Ack has
// returned, the fork continues the association Ack keyed. The wait ends with
// tx, with ctx, or after two T1 intervals, which covers the handshake's last
// flight; RFC 5407 section 3.1.4 lets a re-INVITE crossing the ACK be answered
// 491, which asks the peer to retry.
func (d *DialogClientSession) awaitAck(ctx context.Context, tx sip.ServerTransaction) bool {
	d.answeringMu.Lock()
	acking := d.acking
	d.answeringMu.Unlock()
	if acking == nil {
		return true
	}

	timer := time.NewTimer(2 * sip.T1)
	defer timer.Stop()
	select {
	case <-acking:
		return true
	case <-tx.Done():
		return false
	case <-ctx.Done():
		return false
	case <-timer.C:
		return false
	}
}
