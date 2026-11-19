// SPDX-License-Identifier: MPL-2.0

package media

import "errors"

// errWebRTCSessionNotNegotiated is what ReOffer returns on a session that has
// no negotiated transport to describe again.
var errWebRTCSessionNotNegotiated = errors.New("webrtc media session is not negotiated")

// stagedModePreference is what a re-offer of ours changed on a session before
// its answer is known: the direction we ask for. The peer's answer is
// negotiated against that direction, so it has to be in place while the
// exchange runs, and the one it replaced has to come back when the exchange
// fails.
type stagedModePreference struct {
	// staged says a re-offer of ours is neither committed nor rolled back.
	staged bool
	// previous is the direction preference the session had before it.
	previous string
}

// ReOffer returns an offer of ours that modifies a negotiated session on the
// transport it runs on: the offer of a re-INVITE that refreshes the session,
// puts the call on hold or takes it off. mode is the direction the offer asks
// for, sendrecv, sendonly, recvonly or inactive, or empty for the direction we
// last asked for.
//
// The offer describes the ICE generation and the DTLS association in use
// again: the same ICE credentials, candidates, fingerprints and a=tls-id, and
// a=setup:actpass, which RFC 8842 section 5.5 has a subsequent offer carry.
// The peer therefore runs no connectivity checks and no handshake for it, and
// once RemoteSDP has taken its answer and Finalize has committed it, the
// session is on the candidate pair and the SRTP contexts it had. It is the
// other way to make an offer on a negotiated session beside
// [MediaSessionWebrtc.Fork], which starts a new ICE generation and with it a
// new DTLS association.
//
// Media keeps the direction it has until Finalize. Only the direction we ask
// for is put in place at once, since the answer is negotiated against it, and
// [MediaSessionWebrtc.Rollback], which drops what an exchange that failed had
// staged, puts back the one before.
//
// A session that is not negotiated or is closed has no transport to describe
// again: ReOffer returns an error and changes nothing. It is safe for
// concurrent use with the packet paths; the offer/answer exchanges of one
// session are for their caller to keep one at a time.
func (m *MediaSessionWebrtc) ReOffer(mode string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, errWebRTCSessionClosed
	}
	if !m.ready {
		return nil, errWebRTCSessionNotNegotiated
	}

	preference := m.stageModePreference(mode)
	codecs := m.Codecs
	if len(m.filterCodecs) > 0 {
		codecs = m.filterCodecs
	}
	offer, err := m.localSDPLocked(codecs, m.localDTLSSetup(false), true, preference, m.localRTPProfile(false), m.localTLSIDFor(false))
	if err != nil {
		m.restoreModePreference()
		return nil, err
	}
	return offer, nil
}

// stageModePreference makes mode the direction a remote description is
// negotiated against, and returns it. An empty mode keeps the preference in
// place, which is the negotiated direction on a session that never stated one.
// The preference it replaces is remembered once, so that a re-offer made again
// before the first is rolled back does not forget where the session came
// from. The caller holds m.mu.
func (m *MediaSessionWebrtc) stageModePreference(mode string) string {
	if !m.reOffer.staged {
		m.reOffer = stagedModePreference{staged: true, previous: m.modePreference}
	}
	if mode == "" {
		mode = m.modePreference
	}
	if mode == "" {
		mode = m.Mode
	}
	m.modePreference = mode
	return mode
}

// restoreModePreference puts back the direction preference a re-offer of ours
// replaced, when one is staged: its exchange failed, and the session asks for
// the direction it asked for before. The caller holds m.mu.
func (m *MediaSessionWebrtc) restoreModePreference() {
	if !m.reOffer.staged {
		return
	}
	m.modePreference = m.reOffer.previous
	m.reOffer = stagedModePreference{}
}

// keepModePreference makes the direction preference in place the session's
// own: the exchange that staged it is committed, and a later Rollback has
// nothing of it to put back. The caller holds m.mu.
func (m *MediaSessionWebrtc) keepModePreference() {
	m.reOffer = stagedModePreference{}
}
