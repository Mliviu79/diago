// SPDX-License-Identifier: MPL-2.0

package media

import (
	"crypto/rand"
	"errors"
	"fmt"
	"net"
	"slices"
	"strings"
)

// dtlsAssociationState is what a media session keeps about the DTLS
// association its media is keyed from, across the offer/answer exchanges of a
// call: a MediaSession and a MediaSessionWebrtc both embed it. It is guarded
// as the fields the session negotiates with are: on a MediaSession it is not
// safe for concurrent use, on a MediaSessionWebrtc the session's lock guards
// it.
type dtlsAssociationState struct {
	// dtlsAssoc describes the DTLS association of the session: the role,
	// fingerprints, tls-id and transport it was built with, which tell
	// whether a later offer or answer continues the association or asks for a
	// new one (RFC 8842 section 3.1). The record itself is never written
	// after it is set.
	//
	// On a MediaSession the handshake sets it once SRTP is keyed, and Fork
	// shares it together with the SRTP contexts with the renegotiating fork;
	// one pointer can be shared between a session and its forks because the
	// record is never written. On a MediaSessionWebrtc RemoteSDP sets it when
	// it negotiates the association of the session's ICE generation, the only
	// one that session has.
	dtlsAssoc *dtlsAssociation

	// localTLSID is our a=tls-id value for the association this session
	// continues or is negotiating, "" while none was drawn and for an
	// association whose offer carried none. Fork of a MediaSession carries
	// it.
	localTLSID string

	// dtlsRemoteTLSID is the peer's a=tls-id value as RemoteSDP last read it,
	// "" when that body carried none. An answer of ours carries a tls-id only
	// when the offer did (RFC 8842 section 5.3).
	dtlsRemoteTLSID string
}

// dtlsAssociation is what a session knows about the DTLS association its
// handshake completed: what RFC 8842 section 3.1 and section 4 compare a later
// offer or answer against to decide whether it continues the association.
type dtlsAssociation struct {
	// setup is the a=setup role the association was actually built with,
	// dtlsSetupActive or dtlsSetupPassive, never actpass.
	setup string
	// localFingerprints and remoteFingerprints are the fingerprint sets each
	// side advertised, as fingerprintSet renders them.
	localFingerprints  []string
	remoteFingerprints []string
	// remoteTLSID is the peer's a=tls-id, empty when it sent none, and
	// localTLSID ours, empty when we sent none.
	remoteTLSID string
	localTLSID  string
	// laddr and raddr are the transport addresses the association was
	// signalled over. Under ICE the candidates carry the transport, and they
	// stay zero.
	laddr net.UDPAddr
	raddr net.UDPAddr
}

// changedBy returns why the DTLS attributes RemoteSDP just read on s ask for a
// new association, or "" when they continue this one.
//
// RFC 8842 section 3.1 asks for a new association when the roles change, a
// fingerprint is modified, added or removed, or the tls-id changes. A peer
// sending no tls-id asks for one by changing its transport instead (section
// 4). We ask for one from a new local address (section 5.1), with a new
// tls-id, so a change of our own transport is a new association whatever the
// peer sends.
func (a *dtlsAssociation) changedBy(s *MediaSession, setup string, fingerprints []DTLSFingerprint, tlsID string) string {
	if role := dtlsRoleAgainst(setup); role != "" && role != a.setup {
		return "a change of DTLS roles"
	}
	if !slices.Equal(fingerprintSet(fingerprints), a.remoteFingerprints) {
		return "a change of the peer's fingerprints"
	}
	if !slices.Equal(fingerprintSet(s.localDTLSFingerprints()), a.localFingerprints) {
		return "a change of our fingerprints"
	}
	if a.tlsIDChanged(tlsID) {
		return "a new tls-id"
	}
	if !sameUDPAddr(s.Laddr, a.laddr) {
		return "a change of our transport"
	}
	if tlsID == "" && !sameUDPAddr(s.Raddr, a.raddr) {
		return "a change of the peer's transport"
	}
	return ""
}

// tlsIDChanged reports whether tlsID, the a=tls-id value of a peer's offer or
// answer, "" when it carried none, asks for a new DTLS association: a peer
// asks for one by changing the value (RFC 8842 section 4). A peer that sends
// none changes nothing by it.
func (a *dtlsAssociation) tlsIDChanged(tlsID string) bool {
	return tlsID != "" && tlsID != a.remoteTLSID
}

// localChangedBy reports whether s no longer presents our side of the
// association: our fingerprints or our transport changed.
func (a *dtlsAssociation) localChangedBy(s *MediaSession) bool {
	if !slices.Equal(fingerprintSet(s.localDTLSFingerprints()), a.localFingerprints) {
		return true
	}
	return !sameUDPAddr(s.Laddr, a.laddr)
}

// dtlsRoleAgainst returns the a=setup role this endpoint plays against the
// peer's setup value, or "" when the peer leaves the choice to us. A peer
// offering actpass on a session that has an association gets the role we
// have, as RFC 8842 section 5.3 has the answerer keep it.
func dtlsRoleAgainst(setup string) string {
	switch strings.ToLower(strings.TrimSpace(setup)) {
	case dtlsSetupActive:
		return dtlsSetupPassive
	case dtlsSetupPassive:
		return dtlsSetupActive
	}
	return ""
}

// sameUDPAddr reports whether a and b are the same IP, port and zone.
func sameUDPAddr(a, b net.UDPAddr) bool {
	return a.IP.Equal(b.IP) && a.Port == b.Port && a.Zone == b.Zone
}

// localDTLSFingerprints returns the a=fingerprint values of our certificates
// as LocalSDP advertises them, which a DTLS association records. A certificate
// that has no fingerprint leaves an empty value, which fingerprintSet skips as
// LocalSDP skips its line.
func (s *MediaSession) localDTLSFingerprints() []DTLSFingerprint {
	fingerprints := make([]DTLSFingerprint, len(s.DTLSConf.Certificates))
	for i, cert := range s.DTLSConf.Certificates {
		fingerprint, err := dtlsSHA256Fingerprint(cert)
		if err != nil {
			continue
		}
		fingerprints[i] = DTLSFingerprint{Algorithm: "SHA-256", Value: fingerprint}
	}
	return fingerprints
}

// carryDTLSAssociation hands fork, a fork of s, what it continues of the DTLS
// association of s.
//
// A fork of a keyed DTLS session continues its DTLS association unless
// RemoteSDP finds a new one asked for, so it keeps the record of it and the
// SRTP contexts, and reads and writes with them at once. The contexts are
// handed over rather than derived again from the same keying material,
// because a context is more than its keys: it holds the rollover counter and
// highest sequence number of every stream already in flight (RFC 3711 section
// 3.3.1). The peer continues its own contexts, so a fresh one here would put a
// stream that had already wrapped its sequence number back at a rollover
// counter of zero while the peer's stayed where it was, and that stream would
// stop authenticating. The session forked from stops using them when the fork
// is put in its place.
//
// Our tls-id belongs to the association too, and to an offer already made for
// one, so the fork keeps it whether or not s is keyed.
func (s *MediaSession) carryDTLSAssociation(fork *MediaSession) {
	fork.localTLSID = s.localTLSID
	if s.dtlsAssoc == nil {
		return
	}
	fork.dtlsAssoc = s.dtlsAssoc
	fork.localCtxSRTP = s.localCtxSRTP
	fork.remoteCtxSRTP = s.remoteCtxSRTP
	// Last, once the contexts above are set: see srtpReadState.dtlsKeyed.
	fork.dtlsKeyed.Store(true)
}

// negotiateDTLSAssociation decides, once RemoteSDP has read the peer's DTLS
// attributes, whether this session continues the DTLS association it has or
// needs a new one, and arms the handshake of a new one. setup, fingerprints and
// tlsID are the peer's a=setup, a=fingerprint and a=tls-id values.
//
// A session continuing an association is renegotiating codecs or direction
// over it: it keeps the association and the SRTP contexts Fork handed over, and
// arms no handshake. The attributes are still validated by the caller, and the
// peer's a=setup is still recorded. A body applied again to the session the
// handshake keyed, the 2xx after an early answer for one, continues its
// association in the same way and leaves the contexts its reader and writer
// use untouched.
//
// A new association has to be told apart from the current one, whose media
// keeps arriving until the peer switches over, and over UDP only the transport
// can do that (RFC 8842 section 5.1). A session answering an offer moves to
// sockets of its own, as section 5.1 has the answerer do, before it arms the
// handshake. A session applying an answer to its own offer can no longer move,
// since the peer is already sending to the transport in that offer; it arms
// the handshake only if the offer was made from sockets of its own, and
// otherwise refuses the answer.
func (s *MediaSession) negotiateDTLSAssociation(setup string, fingerprints []DTLSFingerprint, tlsID string) error {
	assoc := s.dtlsAssoc
	if assoc == nil {
		s.newAnswerTLSID(tlsID)
		return s.armDTLSHandshake(setup, fingerprints, tlsID)
	}

	reason := assoc.changedBy(s, setup, fingerprints, tlsID)
	if reason == "" {
		return nil
	}

	rebound := false
	if sameUDPAddr(s.Laddr, assoc.laddr) {
		// Still on the sockets of the session this one was forked from, whose
		// media runs on them and whose reader would take the handshake's
		// packets.
		if s.remoteSDPIsAnswer {
			return fmt.Errorf("sdp: %s asks for a new DTLS association over the transport of the current one", reason)
		}
		// Dropped first, so that a failed bind cannot leave this session
		// holding sockets it does not own.
		s.rtpConn, s.rtcpConn = nil, nil
		laddr := net.UDPAddr{IP: s.Laddr.IP, Zone: s.Laddr.Zone}
		if err := s.createListeners(&laddr); err != nil {
			return fmt.Errorf("media session: new DTLS association: %w", err)
		}
		rebound = true
	}

	DefaultLogger().Debug("Starting a new DTLS association", "reason", reason, "laddr", s.Laddr.String())
	// The keys of the current association are not the new one's. The session
	// reads nothing and sends nothing until its own handshake has keyed it:
	// see srtpReadState.dtlsKeyed.
	s.dtlsKeyed.Store(false)
	s.dtlsAssoc = nil
	s.localCtxSRTP = nil
	s.remoteCtxSRTP = nil
	s.newAnswerTLSID(tlsID)
	if err := s.armDTLSHandshake(setup, fingerprints, tlsID); err != nil {
		if rebound {
			// Nothing but this session refers to the sockets just bound, and
			// a session RemoteSDP refused is not installed.
			return errors.Join(err, s.Close())
		}
		return err
	}
	return nil
}

// newAnswerTLSID gives a session answering an offer with a new DTLS
// association our tls-id for it: see dtlsAssociationState.answerTLSID.
func (s *MediaSession) newAnswerTLSID(remoteTLSID string) {
	s.answerTLSID(s.remoteSDPIsAnswer, remoteTLSID)
}

// localSDPTLSID returns the a=tls-id value LocalSDP writes, or "" for none;
// answered is the argument of LocalSDP. See dtlsAssociationState.tlsIDFor: our
// side of the association has changed when the session offers from other
// sockets or with other certificates than the association was built with.
func (s *MediaSession) localSDPTLSID(answered bool) string {
	return s.tlsIDFor(answered, s.dtlsAssoc != nil && s.dtlsAssoc.localChangedBy(s))
}

// answerTLSID gives a session answering an offer with a new DTLS association
// our tls-id for it: a new value when the offer carries a tls-id, remoteTLSID,
// which the answer then carries, and none when it does not (RFC 8842 section
// 5.3). answersOurOffer says the body being applied answers an offer of ours
// instead: the session then keeps the value its offer carried.
func (st *dtlsAssociationState) answerTLSID(answersOurOffer bool, remoteTLSID string) {
	if answersOurOffer {
		return
	}
	st.localTLSID = ""
	if remoteTLSID != "" {
		st.localTLSID = newTLSID()
	}
}

// tlsIDFor returns the a=tls-id value of the body a session builds, or "" for
// none (RFC 8842 section 5); answered says the body answers the offer the
// session last read. An answer carries the value the session settled on when
// it read that offer, and only when the offer carried a tls-id. An offer
// carries the value of the association the session continues, or a new one
// when it has no association, or asks for a new one because our side of the
// association has changed, which localChanged says.
func (st *dtlsAssociationState) tlsIDFor(answered bool, localChanged bool) string {
	if answered {
		if st.dtlsRemoteTLSID == "" {
			return ""
		}
		return st.localTLSID
	}

	assoc := st.dtlsAssoc
	switch {
	case assoc != nil && !localChanged:
		st.localTLSID = assoc.localTLSID
	case assoc != nil && st.localTLSID == assoc.localTLSID, assoc == nil && st.localTLSID == "":
		st.localTLSID = newTLSID()
	}
	return st.localTLSID
}

// newTLSID draws a tls-id value: 26 characters of the base32 alphabet, which
// the tls-id-value grammar allows, carrying 128 bits from a cryptographically
// secure source, where RFC 8842 section 4 asks for at least 120.
func newTLSID() string {
	return rand.Text()
}
