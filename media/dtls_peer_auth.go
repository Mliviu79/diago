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

// dtlsFingerprintHashes lists the a=fingerprint hash functions of RFC 8122
// section 5 with their hash, leaving out MD2 and MD5, which the same section
// forbids verifying with. The names are case insensitive and browsers write
// them in lower case, so they are kept in lower case.
//
// The order is our preference among the hash functions a peer offers, the
// strongest first: see preferredDTLSFingerprintHash.
var dtlsFingerprintHashes = []struct {
	name    string
	newHash func() hash.Hash
}{
	{name: "sha-512", newHash: sha512.New},
	{name: "sha-384", newHash: sha512.New384},
	{name: "sha-256", newHash: sha256.New},
	{name: "sha-224", newHash: sha256.New224},
	{name: "sha-1", newHash: sha1.New},
}

// dtlsFingerprintHash returns the hash an a=fingerprint hash function name
// stands for, and false for a name fingerprints are not compared under.
func dtlsFingerprintHash(name string) (func() hash.Hash, bool) {
	for _, h := range dtlsFingerprintHashes {
		if strings.EqualFold(h.name, name) {
			return h.newHash, true
		}
	}
	return nil, false
}

// preferredDTLSFingerprintHash returns the name of the hash function the
// peer's certificate is verified under: the strongest one we compare among
// those the fingerprints use, and "" when they use none we compare.
//
// RFC 8122 section 5.1: "An endpoint MUST select the set of fingerprints that
// use its most preferred hash function (out of those offered by the peer) and
// verify that each certificate used matches one fingerprint out of that set."
// A certificate is therefore not accepted for matching a fingerprint under a
// weaker hash function while a stronger one was offered: whoever can forge a
// certificate for the weaker digest would otherwise choose which hash function
// authenticates it. A peer that offers sha-1 alone is verified under sha-1,
// which the section allows.
func preferredDTLSFingerprintHash(fingerprints []DTLSFingerprint) string {
	for _, h := range dtlsFingerprintHashes {
		offered := slices.ContainsFunc(fingerprints, func(fp DTLSFingerprint) bool {
			return strings.EqualFold(fp.Algorithm, h.name)
		})
		if offered {
			return h.name
		}
	}
	return ""
}

// mediaDTLSFingerprints returns the fingerprints that apply to a media
// description, given those of the session level and its own: its own when it
// has any, and those of the session level otherwise.
//
// RFC 8122 section 5: "The fingerprint attribute may be either a session-level
// or a media-level SDP attribute. If it is a session-level attribute, it
// applies to all TLS sessions for which no media-level fingerprint attribute
// is defined." A session-level fingerprint beside a media-level one names the
// certificate of other media, so it neither authenticates this media's peer
// nor belongs to the set its DTLS association was negotiated with.
func mediaDTLSFingerprints(session, media []DTLSFingerprint) []DTLSFingerprint {
	if len(media) > 0 {
		return media
	}
	return session
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
