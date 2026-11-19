// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/pion/dtls/v3"
	"github.com/pion/dtls/v3/pkg/crypto/elliptic"
	"github.com/pion/logging"
)

var (
	DTLSDebug bool
)

const (
	// ServerClientAuthNoCert is pion's NoClientCert: the DTLS server sends no
	// CertificateRequest. It is the DTLSConfig.ServerClientAuth default, which
	// DTLSConfig raises to requiring a certificate.
	ServerClientAuthNoCert = int(dtls.NoClientCert)
	// ServerClientAuthRequireCert is pion's RequestClientCert: the DTLS server
	// asks the client for a certificate but lets it leave one out, so it
	// requests a certificate and does not require one. DTLSConfig raises it to
	// requiring one, as DTLSConfig.ServerClientAuth says.
	ServerClientAuthRequireCert = int(dtls.RequestClientCert)

	EllipticCurveP256   uint16 = uint16(elliptic.P256)
	EllipticCurveP384   uint16 = uint16(elliptic.P384)
	EllipticCurveX25519 uint16 = uint16(elliptic.X25519)
)

type DTLSConfig struct {
	Certificates []tls.Certificate
	// If used as client this would verify server certificate
	ServerName string

	// ServerClientAuth is the client certificate policy when this endpoint is
	// the DTLS server. The server always requires the client's certificate,
	// because it checks it against the peer's a=fingerprint (RFC 5763 section
	// 5), so a policy that lets the client leave it out is raised to requiring
	// one. That covers ServerClientAuthNoCert, the default, and
	// ServerClientAuthRequireCert, which requests a certificate without
	// requiring it. A stricter policy is kept.
	ServerClientAuth int

	// SRTPProfiles to use in exchange. Check constant vars with media.SRTPProfile...
	SRTPProfiles []uint16

	// SDPSetupRole chooses the a=setup value this endpoint advertises, and
	// with it the DTLS role it plays: active is the client, passive the server
	// (RFC 5763 section 5). offer reports whether this endpoint is the offerer
	// of the exchange, as MediaSession.DTLSRole or the arguments of LocalSDP
	// and RemoteSDP say. It is asked where the exchange leaves a choice: for
	// an offer, and for the answer to an offer of actpass. A peer that said
	// active or passive is given the other value without asking (RFC 4145
	// section 4.1).
	// Values: active,passive,actpass
	// Default: an offerer advertises actpass (RFC 5763 section 5), and
	// actpass is answered active.
	SDPSetupRole func(offer bool) string `json:"-"`

	// List of Elliptic Curves to use
	//
	// If an ECC ciphersuite is configured and EllipticCurves is empty
	// it will default to X25519, P-256, P-384 in this specific order.
	// Check values with media.Eliptic<name>
	EllipticCurves []uint16
}

// DTLSFingerprint identifies a peer certificate fingerprint advertised in SDP.
type DTLSFingerprint struct {
	Value     string
	Algorithm string
}

func (conf *DTLSConfig) ToLibConf(fingerprints []DTLSFingerprint) *dtls.Config {
	// The peer is authenticated with the certificate fingerprint carried in
	// SDP. A DTLS server must require the client's certificate, otherwise
	// VerifyConnection has nothing to compare.
	clientAuth := conf.clientAuth()

	config := &dtls.Config{
		// Use appropriate certificate or generate self-signed
		Certificates:     conf.Certificates,
		SignatureSchemes: []tls.SignatureScheme{},

		// CipherSuites: []dtls.CipherSuiteID{
		// 	dtls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		// },
		SRTPProtectionProfiles: []dtls.SRTPProtectionProfile{
			dtls.SRTP_AEAD_AES_128_GCM,
			dtls.SRTP_AES128_CM_HMAC_SHA1_80,
			// dtls.SRTP_AES128_CM_HMAC_SHA1_32,
		},

		// SignatureSchemes: []tls.SignatureScheme{
		// 	tls.ECDSAWithP256AndSHA256
		// },

		// If you're acting as the server
		// We are verifying Connection fingerprints so we require client cert
		ClientAuth:           clientAuth,
		ExtendedMasterSecret: dtls.RequireExtendedMasterSecret,

		InsecureSkipVerify: conf.ServerName == "", // Accept self-signed certs (for dev)
		ServerName:         conf.ServerName,       // If insecure is false

		// IT IS STILL UNCLEAR WHY WE CAN NOT READ CERTIFICATE HERE
		// The peer is checked against its SDP fingerprints, and with none there
		// is nothing it can match, so the handshake fails.
		VerifyConnection: func(state *dtls.State) error {
			return dtlsVerifyConnection(state, fingerprints)
		},
	}

	if conf.SRTPProfiles != nil {
		srtpProfs := make([]dtls.SRTPProtectionProfile, len(conf.SRTPProfiles))
		for i, p := range conf.SRTPProfiles {
			srtpProfs[i] = dtls.SRTPProtectionProfile(p)
		}
		config.SRTPProtectionProfiles = srtpProfs
	}

	if conf.EllipticCurves != nil {
		config.EllipticCurves = make([]elliptic.Curve, len(conf.EllipticCurves))
		for i, c := range conf.EllipticCurves {
			config.EllipticCurves[i] = elliptic.Curve(c)
		}
	}

	if DTLSDebug {
		loggerFactory := logging.NewDefaultLoggerFactory()
		loggerFactory.DefaultLogLevel = logging.LogLevelTrace
		config.LoggerFactory = loggerFactory
	}
	return config
}

// dtlsVerifyConnection accepts the peer only when its certificate matches one
// of the a=fingerprint values from its SDP (RFC 5763 section 5). The
// certificates are self signed, so that match is the only thing binding the
// DTLS peer to the signalling, and a certificate matching none of them is
// rejected. The grammar asks for upper case hex; lower case is accepted as
// well. A fingerprint whose hash function is not supported is skipped, and of
// the others only those under the strongest hash function the peer offered
// are compared (RFC 8122 section 5.1, see preferredDTLSFingerprintHash).
func dtlsVerifyConnection(state *dtls.State, fingerprints []DTLSFingerprint) error {
	if len(state.PeerCertificates) == 0 {
		return fmt.Errorf("no certificate found in dtls")
	}

	remoteCert := state.PeerCertificates[0]
	preferred := preferredDTLSFingerprintHash(fingerprints)
	for _, fp := range fingerprints {
		newHash, ok := dtlsFingerprintHash(fp.Algorithm)
		if !ok {
			DefaultLogger().Debug("Skiping fingerprint due to unsuported alg", "alg", fp.Algorithm)
			continue
		}
		if !strings.EqualFold(fp.Algorithm, preferred) {
			continue
		}

		remoteFP, err := dtlsCertificateFingerprint(remoteCert, newHash)
		if err != nil {
			return err
		}

		DefaultLogger().Debug("Comparing fingerprint", "alg", fp.Algorithm, "fp", fp.Value, "rfp", remoteFP)
		if strings.EqualFold(fp.Value, remoteFP) {
			return nil
		}
	}

	return fmt.Errorf("dtls certificate does not match any SDP fingerprint")
}

func dtlsSHA256Fingerprint(cert tls.Certificate) (string, error) {
	if len(cert.Certificate) == 0 {
		return "", fmt.Errorf("no certificate data found")
	}
	return dtlsSHA256CertificateFingerprint(cert.Certificate[0])
}

func dtlsSHA256CertificateFingerprint(cert []byte) (string, error) {

	// Parse the leaf certificate
	leaf, err := x509.ParseCertificate(cert)
	if err != nil {
		return "", fmt.Errorf("failed to parse certificate: %v", err)
	}

	// Calculate SHA-256 fingerprint
	hash := sha256.Sum256(leaf.Raw)

	// Format as colon-separated hex string
	hexStr := strings.ToUpper(hex.EncodeToString(hash[:]))
	var fingerprint strings.Builder
	for i := 0; i < len(hexStr); i += 2 {
		if i > 0 {
			fingerprint.WriteString(":")
		}
		fingerprint.WriteString(hexStr[i : i+2])
	}

	return fingerprint.String(), nil
}
