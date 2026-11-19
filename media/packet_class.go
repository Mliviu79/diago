// SPDX-License-Identifier: MPL-2.0

package media

// IsRTCP reports whether buf carries RTCP rather than RTP.
//
// RTP and RTCP share one port under rtcp-mux (RFC 5761). They are told apart by
// the payload type field: section 4 of that RFC reserves 64-95 for RTCP, which
// cannot collide with an RTP payload type once the marker bit is masked off.
//
// The WebRTC packet mux does not classify with it: it compares the whole
// second byte with the RTCP packet types 192-223.
func IsRTCP(buf []byte) bool {
	if len(buf) < 2 {
		return false
	}
	pt := buf[1] & 0x7f
	return pt >= 64 && pt <= 95
}

// isDTLS reports whether buf carries a DTLS record.
//
// RFC 7983 section 7 assigns the value of the first byte: 20-63 is DTLS,
// 128-191 is RTP or RTCP, and 0-3 is STUN.
func isDTLS(buf []byte) bool {
	if len(buf) < 1 {
		return false
	}
	return buf[0] >= 20 && buf[0] <= 63
}
