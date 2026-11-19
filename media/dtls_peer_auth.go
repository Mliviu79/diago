// SPDX-License-Identifier: MPL-2.0

package media

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"hash"
	"slices"
	"strings"

	"github.com/pion/dtls/v3"
)

// checkCertificates reports a configuration that cannot take part in DTLS-SRTP
// negotiation. RFC 5763 section 5 requires an a=fingerprint in the SDP, and it
// is computed from our certificate. Without one the SDP names a DTLS setup with
// nothing to check the handshake against, which a conformant peer refuses, and
// the handshake fails for want of a certificate to present.
func (conf *DTLSConfig) checkCertificates() error {
	if len(conf.Certificates) == 0 {
		return fmt.Errorf("dtls needs a certificate, none is configured")
	}
	for i, cert := range conf.Certificates {
		if _, err := dtlsSHA256Fingerprint(cert); err != nil {
			return fmt.Errorf("dtls certificate %d has no fingerprint: %w", i, err)
		}
	}
	return nil
}

// clientAuth is the policy the DTLS server applies to the client certificate.
// The peer is checked against its SDP fingerprints, which needs its
// certificate, so the policy requires one at least (RFC 8122 section 6.2). A
// server that sends no CertificateRequest receives no certificate and fails
// every handshake, and one that only requests it lets the client leave it out.
func (conf *DTLSConfig) clientAuth() dtls.ClientAuthType {
	auth := dtls.ClientAuthType(conf.ServerClientAuth)
	if auth < dtls.RequireAnyClientCert {
		return dtls.RequireAnyClientCert
	}
	return auth
}

// dtlsFingerprintHashes maps the a=fingerprint hash function names of RFC 8122
// section 5 to their hash, leaving out MD2 and MD5, which the same section
// forbids verifying with. The names are case insensitive and browsers write
// them in lower case, so they are looked up in lower case.
var dtlsFingerprintHashes = map[string]func() hash.Hash{
	"sha-1":   sha1.New,
	"sha-224": sha256.New224,
	"sha-256": sha256.New,
	"sha-384": sha512.New384,
	"sha-512": sha512.New,
}

// dtlsFingerprintHash returns the hash an a=fingerprint hash function name
// stands for, and false for a name fingerprints are not compared under.
func dtlsFingerprintHash(name string) (func() hash.Hash, bool) {
	newHash, ok := dtlsFingerprintHashes[strings.ToLower(name)]
	return newHash, ok
}

// hasCheckableDTLSFingerprint reports whether one of the fingerprints names a
// hash function the peer's certificate can be compared under.
func hasCheckableDTLSFingerprint(fingerprints []DTLSFingerprint) bool {
	return slices.ContainsFunc(fingerprints, func(fp DTLSFingerprint) bool {
		_, ok := dtlsFingerprintHash(fp.Algorithm)
		return ok
	})
}

// fingerprintSet renders fingerprints as a sorted set that compares equal for
// the same certificates however the SDP spelled them: hash function names and
// hex digits are case insensitive (RFC 8122 section 5), and the order of the
// attributes carries no meaning.
func fingerprintSet(fingerprints []DTLSFingerprint) []string {
	set := make([]string, 0, len(fingerprints))
	for _, fp := range fingerprints {
		if fp.Value == "" {
			continue
		}
		set = append(set, strings.ToLower(fp.Algorithm)+" "+strings.ToUpper(fp.Value))
	}
	slices.Sort(set)
	return slices.Compact(set)
}

// dtlsCertificateFingerprint returns the fingerprint of a DER certificate under
// the given hash, formatted as a=fingerprint carries it: upper case hex octets
// separated by colons.
func dtlsCertificateFingerprint(cert []byte, newHash func() hash.Hash) (string, error) {
	leaf, err := x509.ParseCertificate(cert)
	if err != nil {
		return "", fmt.Errorf("failed to parse certificate: %v", err)
	}

	h := newHash()
	h.Write(leaf.Raw)
	hexStr := strings.ToUpper(hex.EncodeToString(h.Sum(nil)))

	var fingerprint strings.Builder
	for i := 0; i < len(hexStr); i += 2 {
		if i > 0 {
			fingerprint.WriteString(":")
		}
		fingerprint.WriteString(hexStr[i : i+2])
	}
	return fingerprint.String(), nil
}
