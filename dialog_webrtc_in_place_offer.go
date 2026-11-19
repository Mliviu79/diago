// SPDX-License-Identifier: MPL-2.0

package diago

import "github.com/emiago/diago/media"

// noteOfferRequest records that the session handed over no session
// description, when it was not to answer an offer of ours (offered false): a
// re-INVITE of the peer's without an offer asks for ours, which our 2xx
// carries and its ACK answers (RFC 3261 section 14.2), and a session refresh
// of ours asks for an offer that indicates that the session has not changed
// (RFC 4028 section 7.4). The offer made next is that of the transport in
// place, see onLocalSDP.
func (d *DialogWebrtc) noteOfferRequest(offered bool) {
	if offered {
		return
	}
	d.mu.Lock()
	d.offerRequested = true
	d.mu.Unlock()
}

// offersInPlaceUnsafe reports whether an offer of ours asking for mode is made
// on the transport in place, and forgets an offer request: the request is
// good for the one description that follows it. Must be called with d.mu
// held.
//
// The offer the session asked for, see noteOfferRequest, and one that changes
// the direction, as Hold and Unhold do, continue the nominated ICE pair and
// the DTLS association: they change no ICE credentials, so RFC 8839 section
// 4.4.1.1.1 makes them no ICE restart, and they keep the fingerprints and
// a=tls-id, so RFC 8842 section 5.5 makes them no new DTLS association. The
// peer then runs no connectivity checks and no handshake, and media keeps
// flowing on the keys in use. Any other offer of ours, a plain re-INVITE,
// restarts ICE on a new transport.
func (d *DialogWebrtc) offersInPlaceUnsafe(mode string) bool {
	requested := d.offerRequested
	d.offerRequested = false
	return requested || mode != ""
}

// offerInPlace returns the offer of current, the transport in place, asking
// for mode, and stores current as the pending transport, which the answer to
// the offer then updates in place. An offer that cannot be made leaves
// current as it was.
func (d *DialogWebrtc) offerInPlace(current *media.MediaSessionWebrtc, mode string) ([]byte, error) {
	if err := d.setPendingMediaSession(current); err != nil {
		return nil, err
	}
	offer, err := current.ReOffer(mode)
	if err != nil {
		d.abortPendingMediaSession()
		return nil, err
	}
	return offer, nil
}
