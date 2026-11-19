// SPDX-License-Identifier: MPL-2.0

package diago

// awaitsFirstAnswerUnsafe reports whether the media session in place made the
// call's first offer and has no answer to it yet: no update of ours is pending
// and the session has no remote description. The answer to that offer belongs
// to this session and not to a fork of it: the session holds what the offer
// committed it to, such as its DTLS role and certificate, a fork has none of
// it, and whoever set the session up starts its RTP session once it is
// answered. Must be called with d.mu held.
func (d *DialogMedia) awaitsFirstAnswerUnsafe() bool {
	return d.pendingMediaSession == nil && d.mediaSession != nil && d.mediaSession.Raddr.IP == nil
}
