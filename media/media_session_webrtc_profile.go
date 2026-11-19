// SPDX-License-Identifier: MPL-2.0

package media

// webrtcOfferProfile is the m= transport of an offer of ours: RFC 8829 section
// 5.2.1 has the proto field of an offer "set to UDP/TLS/RTP/SAVPF, as
// specified in RFC 5764, Section 8".
const webrtcOfferProfile = "UDP/TLS/RTP/SAVPF"

// offerAccepted records the m= transport proto of a remote description
// RemoteSDP accepted, when that description is an offer: offered says it
// answers an offer of ours instead, and the transport of our next answer is
// not taken from an answer. The caller holds m.mu.
func (m *MediaSessionWebrtc) offerAccepted(proto string, offered bool) {
	if offered {
		return
	}
	m.offeredProto = proto
}

// localRTPProfile returns the m= transport of the body LocalSDP builds, where
// answered says the body answers the offer RemoteSDP accepted. The caller
// holds m.mu.
//
// Only an offer's transport is ours to choose. An endpoint may offer WebRTC
// media under another name of the RTP profile family than the one RFC 5764
// section 8 registers, RTP/SAVPF with a=fingerprint for one, and RFC 8829
// section 5.1.2 has such an offer accepted and "the profile in any m= line in
// any generated answer MUST exactly match the profile provided in the offer".
// So an answer repeats the offer's transport as the peer wrote it; see
// MediaSession.localRTPProfile for the offer/answer rules that leave an
// answerer no other choice.
func (m *MediaSessionWebrtc) localRTPProfile(answered bool) string {
	if answered && m.offeredProto != "" {
		return m.offeredProto
	}
	return webrtcOfferProfile
}
