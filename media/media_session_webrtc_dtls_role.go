// SPDX-License-Identifier: MPL-2.0

package media

import (
	"fmt"
	"strings"
)

// localDTLSSetup returns the a=setup value of the body LocalSDP builds, where
// answered says the body answers the offer RemoteSDP accepted. An answer
// carries the side the session took when it read that offer, empty before
// any. An offer carries actpass, which RFC 5763 section 5 requires of the
// offerer and RFC 8842 section 5.5 of every subsequent offer, unless the
// application chose another value with DTLSConfig.SDPSetupRole, which is asked
// for an offer only. The caller holds m.mu.
func (m *MediaSessionWebrtc) localDTLSSetup(answered bool) string {
	if answered {
		return m.localSetup
	}
	return m.Config.DTLS.setupFor(true, "")
}

// dtlsSetupAgainst returns the side of the DTLS handshake this session takes
// against setup, the peer's a=setup value in a body that answers an offer of
// ours when offered is true and is an offer otherwise: active, and the session
// sends the ClientHello, or passive, and it waits for one (RFC 5763 section
// 5). The decision is DTLSConfig.setupFor's, the one every media type takes,
// so the value an answer of ours carries and the role the session plays are
// the same decision.
//
// An offer of active is answered passive, as one of passive is answered
// active: RFC 4145 section 4.1 leaves the answerer of either nothing else, and
// RFC 8842 section 5.3 has an answerer "able to receive initial and subsequent
// offers with other attribute values" than the actpass an offerer is required
// to send. An offer of actpass leaves the choice to us; DTLSConfig.SDPSetupRole
// makes that choice when it is set, and active is taken otherwise. An answer
// leaves no choice: one that repeats actpass instead of taking a side is
// refused with an error that wraps ErrNoCommonCrypto (checkAnswerDTLSSetup).
// Whatever the choice returns, the session ends on one side: anything but
// active makes it the server, and the value it then writes in an answer is
// passive, the role it plays, since RFC 5763 section 5 has an answerer use
// active or passive.
//
// A session whose DTLS association is negotiated already has its side, and a
// peer that offers actpass again does not open the choice again: the body
// continues the association, and RFC 8842 section 5.3 has the answer to such
// an offer carry a value "that does not change the previously negotiated DTLS
// roles".
//
// A value other than active, passive and actpass is refused: RFC 8842 section
// 5.1 leaves a DTLS association no other.
func (m *MediaSessionWebrtc) dtlsSetupAgainst(setup string, offered bool) (string, error) {
	if !knownDTLSSetup(setup) {
		if offered {
			return "", fmt.Errorf("invalid DTLS setup role %q in answer", setup)
		}
		return "", fmt.Errorf("invalid DTLS setup role %q in offer", setup)
	}
	if offered {
		if err := checkAnswerDTLSSetup(setup); err != nil {
			return "", err
		}
	}

	m.mu.Lock()
	conf := m.Config.DTLS
	negotiated := m.localSetup
	m.mu.Unlock()
	if negotiated != "" && strings.EqualFold(strings.TrimSpace(setup), dtlsSetupActPass) {
		return negotiated, nil
	}
	// offered says the body answers an offer of ours, which makes us the
	// offerer of the exchange.
	return dtlsSide(conf.setupFor(offered, setup)), nil
}
