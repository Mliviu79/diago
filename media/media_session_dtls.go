// SPDX-License-Identifier: MPL-2.0

package media

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/pion/dtls/v3"
	"github.com/pion/srtp/v3"
)

// DTLSHandshakeTimeout caps how long the DTLS handshake Finalize and
// FinalizeContext run may wait for the peer, on a MediaSession and on a
// MediaSessionWebrtc. The DTLS stack retransmits its flights for as long as
// its context lives, so without a cap a peer that never completes the
// handshake would hold it, the caller of Finalize and the socket forever. A
// handshake MediaSessionWebrtc.FinalizeWithin left running is capped from the
// moment Finalize waits for it.
var DTLSHandshakeTimeout = 30 * time.Second

// negotiateDTLS applies the DTLS part of the SDP RemoteSDP is reading, whose
// attributes for the audio stream are attrs: it takes the peer's a=setup,
// a=fingerprint and a=tls-id values and hands them to
// negotiateDTLSAssociation, which continues the DTLS association the session
// has or arms the handshake of a new one. The fingerprints are those of the
// stream's own section, or those of the session level when it has none (RFC
// 8122 section 5, see mediaDTLSFingerprints). An a=setup value other than
// active, passive and actpass is refused: RFC 8842 section 5.1 leaves a DTLS
// association no other. A session with neither a DTLS certificate nor
// SecureRTP set to DTLS does nothing.
func (s *MediaSession) negotiateDTLS(attrs streamAttributes) error {
	if !s.dtlsSRTP() {
		return nil
	}

	setup := ""
	tlsID := ""
	for _, v := range attrs.all() {
		if strings.HasPrefix(v, "setup:") {
			setup = strings.TrimSpace(v[len("setup:"):])
			continue
		}

		// RFC 8842 section 4: the peer changes it to ask for a new DTLS
		// association.
		if strings.HasPrefix(v, "tls-id:") {
			tlsID = strings.TrimSpace(v[len("tls-id:"):])
			continue
		}
	}

	sessionLevel, err := dtlsFingerprintAttributes(attrs.session)
	if err != nil {
		return err
	}
	mediaLevel, err := dtlsFingerprintAttributes(attrs.media)
	if err != nil {
		return err
	}
	fingerprints := mediaDTLSFingerprints(sessionLevel, mediaLevel)

	// RFC 5763 section 5 makes a=fingerprint mandatory: the certificates are
	// self signed, and the fingerprint is the only thing the peer's one can
	// be checked against. Without one we could check, any certificate would
	// complete the handshake, so the media is refused here instead.
	if !hasCheckableDTLSFingerprint(fingerprints) {
		return fmt.Errorf("sdp: dtls media has no a=fingerprint with a supported hash function fingerprints=%d", len(fingerprints))
	}

	if setup == "" {
		return fmt.Errorf("Empty a=setup value attribute for dtls")
	}
	if !knownDTLSSetup(setup) {
		return fmt.Errorf("unknown setup value %q", setup)
	}
	// The peer's value is recorded and not acted on directly. Our own role
	// governs the handshake, and dtlsSetupFor derives it from this value for
	// LocalSDP too, so the role we advertise and the role we play are one
	// decision.
	s.dtlsRemoteSetup = setup
	s.dtlsRemoteTLSID = tlsID

	return s.negotiateDTLSAssociation(setup, fingerprints, tlsID)
}

// dtlsFingerprintAttributes returns the a=fingerprint values among attrs, the
// attribute values of one level of an SDP.
func dtlsFingerprintAttributes(attrs []string) ([]DTLSFingerprint, error) {
	var fingerprints []DTLSFingerprint
	for _, v := range attrs {
		// This may only needed to be done when answering call
		if strings.HasPrefix(v, "fingerprint:") {
			vals := strings.Split(v[len("fingerprint:"):], " ")
			if len(vals) < 2 {
				return nil, fmt.Errorf("sdp: bad fingerprint attribute attr=%q", v)
			}
			alg := vals[0]
			fp := vals[1]
			// TODO fingerprint validation
			fingerprints = append(fingerprints, DTLSFingerprint{
				Algorithm: alg,
				Value:     fp,
			})
		}
	}
	return fingerprints, nil
}

// armDTLSHandshake prepares the DTLS association RemoteSDP negotiated: it
// builds the DTLS conn for the role dtlsSetupFor gives this session against
// the peer's a=setup value, setup, and arms Finalize to run the handshake and
// key SRTP from it. fingerprints and tlsID are the certificate fingerprints
// and the tls-id the peer sent.
func (s *MediaSession) armDTLSHandshake(setup string, fingerprints []DTLSFingerprint, tlsID string) error {
	// THIS may need external or after SIP ACK establishment
	dtlsConf := s.DTLSConf.ToLibConf(fingerprints)
	tr := s.dtlsTransport()
	// What a later offer or answer is compared against, once the handshake
	// has completed: the transport is the one signalled in this exchange.
	assoc := &dtlsAssociation{
		setup:              dtlsSetupPassive,
		localFingerprints:  fingerprintSet(s.localDTLSFingerprints()),
		remoteFingerprints: fingerprintSet(fingerprints),
		remoteTLSID:        tlsID,
		localTLSID:         s.localTLSID,
		laddr:              s.Laddr,
		raddr:              s.Raddr,
	}
	var err error
	role := "server"
	if s.dtlsActsAsClient() {
		role = "client"
		assoc.setup = dtlsSetupActive
		s.dtlsConn, err = dtls.Client(tr, &s.Raddr, dtlsConf)
	} else {
		// NOTE: setup:active allows the answer and the DTLS handshake to occur in parallel.
		s.dtlsConn, err = dtls.Server(tr, &s.Raddr, dtlsConf)
	}
	if err != nil {
		return fmt.Errorf("failed to setup dlts %s conn: %w", role, err)
	}
	s.dtlsTr = tr

	s.onFinalize = func(ctx context.Context) error {
		DefaultLogger().Debug("Starting dtls handshake",
			"setup", setup,
			"role", role,
			"laddr", s.dtlsConn.LocalAddr().String(),
			"raddr", s.dtlsConn.RemoteAddr().String(),
		)
		hctx, cancel := context.WithTimeout(ctx, DTLSHandshakeTimeout)
		defer cancel()
		if err := s.dtlsConn.HandshakeContext(hctx); err != nil {
			return fmt.Errorf("dtls conn handshake: %w", err)
		}

		// The exchange is over, so the stack must let the socket go before it
		// can take a single SRTP packet off it. This is the first statement
		// after the handshake for that reason: nothing may come between.
		if err := s.retireDTLS(); err != nil {
			return err
		}

		DefaultLogger().Debug("Handshake finished. Checking DTLS State")

		state, ok := s.dtlsConn.ConnectionState()
		if !ok {
			return fmt.Errorf("failed to get dtls client state")
		}

		// Setup now SRTP for encryption
		prof, _ := s.dtlsConn.SelectedSRTPProtectionProfile()
		p := srtp.ProtectionProfile(prof)
		masterKeyLen, err := p.KeyLen()
		if err != nil {
			return fmt.Errorf("dtls - failed to get master keylen: %w", err)
		}
		masterSaltLen, err := p.SaltLen()
		if err != nil {
			return fmt.Errorf("dtls - failed to get master saltlen: %w", err)
		}
		keyingMaterial, err := state.ExportKeyingMaterial("EXTRACTOR-dtls_srtp", nil, 2*(masterKeyLen+masterSaltLen))
		if err != nil {
			return fmt.Errorf("dtls - failed to export keying material: %w", err)
		}

		clientKey := keyingMaterial[:masterKeyLen]
		serverKey := keyingMaterial[masterKeyLen : 2*masterKeyLen]
		clientSalt := keyingMaterial[2*masterKeyLen : 2*masterKeyLen+masterSaltLen]
		serverSalt := keyingMaterial[2*masterKeyLen+masterSaltLen:]

		if role == "server" {
			// Change order
			clientKey, serverKey = serverKey, clientKey
			clientSalt, serverSalt = serverSalt, clientSalt
		}

		s.localCtxSRTP, err = srtp.CreateContext(clientKey, clientSalt, p)
		if err != nil {
			return fmt.Errorf("failed to create SRTP context: %w", err)
		}

		// s.localCtxSRTP, err = srtp.CreateContext(serverKey, serverSalt, p)
		remoteContext, err := srtp.CreateContext(serverKey, serverSalt, p)
		if err != nil {
			return fmt.Errorf("failed to create SRTP context: %w", err)
		}

		s.remoteCtxSRTP = newSRTPReceiver(remoteContext)

		// Recorded only now, with the handshake done and both directions
		// keyed: a fork that finds the record continues the association
		// without a handshake of its own, so it must never see one that is
		// not yet usable.
		s.dtlsAssoc = assoc

		// Last, once the contexts above are set: see srtpReadState.dtlsKeyed.
		s.dtlsKeyed.Store(true)

		DefaultLogger().Debug("DTLS SRTP setuped")
		return nil
	}
	return nil
}

// FinalizeContext is Finalize bounded by ctx. The DTLS handshake it runs waits
// for the peer, and ends with an error when ctx is done first, or at the
// latest after DTLSHandshakeTimeout. A handshake is run once: after it, failed
// or not, FinalizeContext has nothing left to run and returns nil.
func (s *MediaSession) FinalizeContext(ctx context.Context) error {
	if s.onFinalize != nil {
		err := s.onFinalize(ctx)
		s.onFinalize = nil
		return err
	}
	return nil
}

// FinalizePending reports whether FinalizeContext has negotiation left to run:
// the DTLS handshake RemoteSDP armed. A session that needs none, and one whose
// handshake has been run, has nothing pending.
func (s *MediaSession) FinalizePending() bool {
	return s.onFinalize != nil
}

// dtlsTransport returns the packet conn the DTLS handshake runs over, which is
// the RTP socket: RFC 5764 section 5.1.2 puts the handshake on the transport
// that carries SRTP afterwards.
//
// The socket is only lent to the DTLS stack: see dtlsKeyExchangeConn.
func (s *MediaSession) dtlsTransport() *dtlsKeyExchangeConn {
	return newDTLSKeyExchangeConn(s.rtpConn)
}

// retireDTLS releases the media socket once the handshake is done with it: see
// dtlsKeyExchangeConn.retire.
func (s *MediaSession) retireDTLS() error {
	return s.dtlsTr.retire(s.dtlsConn)
}

// refuseUnkeyedWrite returns ErrDTLSNotKeyed for a DTLS-SRTP session its
// handshake has not keyed, and nil for every other session. WriteRTP,
// WriteRTCP and WriteRTCPs send nothing for such a session: its media is SRTP
// and there is nothing yet to protect it with, so a packet sent now would
// carry the payload in the clear, to whatever address the SDP named. The
// refusal does not depend on who holds the session or when it is put in
// place of another: a fork that starts a new association is unkeyed from the
// moment RemoteSDP finds it asked for until its own handshake has completed.
func (s *MediaSession) refuseUnkeyedWrite() error {
	if s.awaitsDTLSKeys() {
		return ErrDTLSNotKeyed
	}
	return nil
}
