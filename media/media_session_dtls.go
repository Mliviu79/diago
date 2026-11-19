// SPDX-License-Identifier: MPL-2.0

package media

import (
	"fmt"
	"strings"

	"github.com/pion/dtls/v3"
	"github.com/pion/srtp/v3"
)

// negotiateDTLS applies the DTLS part of the SDP RemoteSDP is reading, whose
// attributes are attrs: it takes the peer's a=setup and a=fingerprint values,
// builds the DTLS conn for the role dtlsSetupFor gives this session against
// them and arms Finalize to run the handshake and key SRTP from it. An a=setup
// value other than active, passive and actpass is refused: RFC 8842 section
// 5.1 leaves a DTLS association no other. A session with neither a DTLS
// certificate nor SecureRTP set to DTLS does nothing.
func (s *MediaSession) negotiateDTLS(attrs []string) error {
	if len(s.DTLSConf.Certificates) == 0 && s.SecureRTP != 2 {
		return nil
	}

	setup := ""
	fingerprints := make([]DTLSFingerprint, 0, 1) // at least must be 1
	for _, v := range attrs {
		if strings.HasPrefix(v, "setup:") {
			setup = strings.TrimSpace(v[len("setup:"):])
			continue
		}

		// This may only needed to be done when answering call
		if strings.HasPrefix(v, "fingerprint:") {
			vals := strings.Split(v[len("fingerprint:"):], " ")
			if len(vals) < 2 {
				return fmt.Errorf("sdp: bad fingerprint attribute attr=%q", v)
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

	// THIS may need external or after SIP ACK establishment
	dtlsConf := s.DTLSConf.ToLibConf(fingerprints)
	tr := s.dtlsTransport()
	var err error
	role := "server"
	if s.dtlsActsAsClient() {
		role = "client"
		s.dtlsConn, err = dtls.Client(tr, &s.Raddr, dtlsConf)
	} else {
		// NOTE: setup:active allows the answer and the DTLS handshake to occur in parallel.
		s.dtlsConn, err = dtls.Server(tr, &s.Raddr, dtlsConf)
	}
	if err != nil {
		return fmt.Errorf("failed to setup dlts %s conn: %w", role, err)
	}
	s.dtlsTr = tr

	s.onFinalize = func() error {
		DefaultLogger().Debug("Starting dtls handshake",
			"setup", setup,
			"role", role,
			"laddr", s.dtlsConn.LocalAddr().String(),
			"raddr", s.dtlsConn.RemoteAddr().String(),
		)
		if err := s.dtlsConn.Handshake(); err != nil {
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

		DefaultLogger().Debug("DTLS SRTP setuped")
		return nil
	}
	return nil
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
