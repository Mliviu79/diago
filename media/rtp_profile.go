// SPDX-License-Identifier: MPL-2.0

package media

import "strings"

// rtpProfile is an m= transport protocol decomposed into the axes it is built
// from, so callers reason about properties rather than string equality.
type rtpProfile struct {
	// secure is the S in SAVP: media is protected with SRTP (RFC 3711).
	secure bool
	// feedback is the F in AVPF: the profile carries RTCP feedback (RFC 4585).
	// It is orthogonal to secure and to dtls, and carries no keying meaning.
	feedback bool
	// dtls is the "UDP/TLS/" prefix: keys come from a DTLS association
	// (RFC 5764 section 8). Its absence does not mean the keys come from
	// elsewhere: a peer may signal DTLS-SRTP under the plain RTP/SAVP(F) name
	// with a=fingerprint, so the fingerprint attribute is authoritative for
	// keying, not this bit.
	dtls bool
}

// parseRTPProfile decomposes an m= transport protocol into its axes. It reports
// false for a protocol outside the RTP profile family, which is the only thing
// this media stack can answer.
//
// The transport is not an opaque token to match against a list: it is two
// orthogonal axes over an optional DTLS association. S is SRTP (RFC 3711), F
// is the feedback profile (RFC 4585, and RFC 5124 for the two together), and
// the UDP/TLS/ prefix says the keys come from a DTLS association (RFC 5764
// section 8). Reading the axes classifies a spelling that was never met
// before instead of refusing it.
//
// Matching is case-insensitive and tolerates surrounding whitespace: the tokens
// are registered in upper case, but refusing a call over a peer's casing is a
// worse failure than accepting it, and being liberal here costs nothing.
func parseRTPProfile(proto string) (rtpProfile, bool) {
	p := strings.ToUpper(strings.TrimSpace(proto))

	var prof rtpProfile
	if rest, found := strings.CutPrefix(p, "UDP/TLS/"); found {
		prof.dtls = true
		p = rest
	}

	switch p {
	case "RTP/AVP":
	case "RTP/AVPF":
		prof.feedback = true
	case "RTP/SAVP":
		prof.secure = true
	case "RTP/SAVPF":
		prof.secure = true
		prof.feedback = true
	default:
		return rtpProfile{}, false
	}

	// UDP/TLS/ is the DTLS-SRTP keying prefix, so it is only meaningful over a
	// secured profile; UDP/TLS/RTP/AVP is not a thing any peer may offer.
	if prof.dtls && !prof.secure {
		return rtpProfile{}, false
	}
	return prof, true
}

// sdpHasFingerprint reports whether the SDP attributes attrs carry a DTLS
// certificate fingerprint, which is what marks a description as DTLS-keyed
// (RFC 5763 section 5) regardless of the profile name it came under.
func sdpHasFingerprint(attrs []string) bool {
	for _, a := range attrs {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(a)), "fingerprint:") {
			return true
		}
	}
	return false
}

// requiresInlineKeys reports whether a description under this profile, whose
// attributes are attrs, must carry its SRTP keys inline as a=crypto (SDES, RFC
// 4568), so that one without a usable a=crypto is refused.
//
// A secured profile says the media must be protected. It does not say how the
// keys arrive, which the attributes signal: a=fingerprint with a=setup for
// DTLS-SRTP, where the handshake brings them (RFC 5763 section 5), and
// a=crypto for SDES. RFC 5764 section 8 names the DTLS profiles
// UDP/TLS/RTP/SAVP(F), but endpoints also signal DTLS-SRTP under the plain
// RTP/SAVP(F) name with a=fingerprint, which RFC 8829 section 5.1.2 has a
// WebRTC endpoint accept: "support for DTLS-SRTP is determined by the presence
// of one or more a=fingerprint attributes". Deciding the keying from the
// profile name alone reads such an offer as SDES and refuses it for having no
// a=crypto.
func (p rtpProfile) requiresInlineKeys(attrs []string) bool {
	return p.secure && !p.dtls && !sdpHasFingerprint(attrs)
}

// localRTPProfile returns the m= transport of the body LocalSDP builds, where
// chosen is the one this session offers and answered says the body answers
// the offer RemoteSDP applied.
//
// Only an offer's transport is ours to choose. Offer/answer gives an answerer
// no way to pick another: RFC 3264 section 6.1 lets an answer differ from its
// offer in direction, formats, address and port, and section 8.3.1 has a
// stream's transport changed by an offer. RFC 5124 section 3.3.1 makes
// the four RTP profiles mutually exclusive within a negotiation and leaves an
// answerer that wants another one only the rejection of the stream, RFC 8643
// section 3.2 accepts SRTP offered under RTP/AVP by answering with the keying
// attributes, and RFC 8829 section 5.1.2 states the rule outright: "The
// profile in any m= line in any generated answer MUST exactly match the
// profile provided in the offer". So an answer repeats the offer's transport
// as the peer wrote it. Choosing afresh answers an RTP/SAVPF offer keyed by
// DTLS with UDP/TLS/RTP/SAVP, and an RTP/AVP offer carrying a=crypto with
// RTP/SAVP: each a different protocol than the one offered.
func (s *MediaSession) localRTPProfile(answered bool, chosen string) string {
	if answered && s.answeringOffer() {
		return s.remoteProto
	}
	return chosen
}
