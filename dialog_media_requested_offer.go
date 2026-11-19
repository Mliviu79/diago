// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"
	"slices"

	"github.com/emiago/diago/media"
)

// errNoMediaSession is how a session description of the peer's is refused by
// media that has no media session: there is nothing to apply it to, and
// nothing a fork could be made of.
var errNoMediaSession = errors.New("no media session present")

// noteOfferRequest records that the peer's request brought no session
// description, when it was not to answer an offer of ours (offered false): a
// re-INVITE without an offer asks for ours, which our 2xx carries and its ACK
// answers (RFC 3261 section 14.2). The offer made next is that one, see
// onLocalSDP. The session asks in the same way, with no description to hand
// over, for the offer of a re-INVITE of its own that is to leave the session
// as it runs.
func (d *DialogMedia) noteOfferRequest(offered bool) {
	if offered {
		return
	}
	d.mu.Lock()
	d.offerRequested = true
	d.mu.Unlock()
}

// takeOfferRequestUnsafe reports whether the description asked for now is the
// offer the peer's request asked for, and forgets that request: it is good for
// the one description that follows it. Must be called with d.mu held.
func (d *DialogMedia) takeOfferRequestUnsafe() bool {
	requested := d.offerRequested
	d.offerRequested = false
	return requested
}

// abortPendingMediaSession drops the fork that made an offer of ours when the
// exchange ends without its answer: the request carrying the offer failed, or
// the ACK to the 2xx carrying it brought no answer. The fork runs on the
// sockets of the media session in place, which carries the media as before,
// so there is nothing of the fork's own to close. An answer that comes later
// then finds no offer of ours to be the answer to.
func (d *DialogMedia) abortPendingMediaSession() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.pendingMediaSession = nil
	d.offerRequested = false
}

// negotiatedOffer returns the offer of fork, a fork of the installed session
// cur, made to renegotiate nothing. It carries the codecs cur runs on, with the
// payload types they were negotiated with, as the description of cur itself
// would, so the peer is not asked to move the call to another codec. RFC 3261
// section 14.2 has the offer in a 2xx overlap the session in place, which
// these codecs do. fork keeps every codec it was set up with for the answer
// and what follows.
//
// The answer is matched against the codecs fork holds, and a codec whose
// number there differs from the one in the answer is taken to be received on
// the number fork holds. So fork holds the codecs it offered under the numbers
// it offered them with, which are the numbers the peer sends them on.
func negotiatedOffer(fork, cur *media.MediaSession) []byte {
	negotiated := cur.CommonCodecs()
	if len(negotiated) == 0 {
		return fork.LocalSDP(false)
	}
	configured := fork.Codecs
	fork.Codecs = slices.Clone(negotiated)
	offer := fork.LocalSDP(false)
	fork.Codecs = withOfferedPayloadTypes(configured, negotiated)
	return offer
}

// withOfferedPayloadTypes returns configured with each codec that is among
// offered numbered as it is there. A codec that held such a number without
// being offered under it takes the number the offered codec gave up, so no
// two codecs of the result share one.
func withOfferedPayloadTypes(configured, offered []media.Codec) []media.Codec {
	codecs := slices.Clone(configured)
	for _, o := range offered {
		i := slices.IndexFunc(codecs, func(c media.Codec) bool { return c.Matches(o) })
		if i < 0 || codecs[i].PayloadType == o.PayloadType {
			continue
		}
		if j := slices.IndexFunc(codecs, func(c media.Codec) bool { return c.PayloadType == o.PayloadType }); j >= 0 {
			codecs[j].PayloadType = codecs[i].PayloadType
		}
		codecs[i].PayloadType = o.PayloadType
	}
	return codecs
}
