// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"errors"
	"fmt"
)

// Negotiation failures returned, wrapped, by [MediaSession.RemoteSDP] and
// [MediaSessionWebrtc.RemoteSDP]. Each one means the remote SDP and our
// configuration have no overlap, which is a property of the peer's SDP and not
// a fault in this process. They are exported so a caller can tell the two apart
// with errors.Is: an offer we cannot meet should be answered with 488 Not
// Acceptable Here, while an internal error should not. The wrapping text
// carries the specifics.
var (
	// ErrNoCommonCodec means the audio formats do not intersect: the peer
	// offered no codec we support, or none that could be parsed, or the only
	// formats it shares with us are telephone-event or comfort noise, which
	// carry no audio of their own.
	ErrNoCommonCodec = errors.New("no common codec")

	// ErrNoCommonCrypto means the media is negotiable but the keying is not.
	// The peer asked for secure RTP and no SRTP context could be built from what
	// it offered, or it answered an offer of DTLS-SRTP with a=setup:actpass,
	// which takes no side of the handshake. Kept apart from ErrNoCommonCodec
	// because a codec mismatch is a capability gap while this is a security
	// policy gap. [MediaSessionWebrtc.RemoteSDP] returns it for the answer of
	// actpass only.
	ErrNoCommonCrypto = errors.New("no common crypto")

	// ErrNoCommonMedia means the m= line itself is unusable before codecs are
	// reached: a transport we do not speak, or an address and port that cannot
	// carry RTP.
	ErrNoCommonMedia = errors.New("no common media")
)

// Secure RTP modes for [MediaSession.SecureRTP]. They are untyped so they also
// assign to the int fields that carry the mode outside this package.
const (
	// SecureRTPModeNone sends and expects plain RTP.
	SecureRTPModeNone = 0
	// SecureRTPModeSDES keys SRTP with the keys carried inline in the SDP
	// (RFC 4568).
	SecureRTPModeSDES = 1
	// SecureRTPModeDTLS keys SRTP from a DTLS handshake on the media path
	// (RFC 5764).
	SecureRTPModeDTLS = 2
)

// checkRTPPort refuses an audio port that cannot carry media, so that it is
// never latched as the remote address.
//
// Port 0 is not a parse accident, it has a meaning, and RFC 3264 gives it one
// in each direction. In an offer the stream "is offered but MUST NOT be used"
// (section 5.1), which is also how a stream is removed (section 8.2), and it
// "MUST be marked with port zero in the answer". In an answer the stream is
// rejected and neither side may generate media or RTCP for it (section 6).
// Port 0 also absorbs every unparseable port, because MediaDescription discards
// strconv's error and leaves the zero value. All readings agree there is no
// audio to carry, and this session carries nothing else, so the description is
// refused as a whole. Accepting it built a session pointed at nothing: an offer
// was answered with a live port, RTP was addressed to port 0 and RTCP to port
// 1, so the call answered and then sat mute.
func checkRTPPort(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%w: audio media port is not usable port=%d", ErrNoCommonMedia, port)
	}
	return nil
}
