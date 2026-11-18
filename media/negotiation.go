// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"errors"
	"fmt"
)

// Negotiation failures returned, wrapped, by [MediaSession.RemoteSDP]. Each one
// means the remote SDP and our configuration have no overlap, which is a
// property of the peer's SDP and not a fault in this process. They are exported
// so a caller can tell the two apart with errors.Is: an offer we cannot meet
// should be answered with 488 Not Acceptable Here, while an internal error
// should not. The wrapping text carries the specifics.
var (
	// ErrNoCommonCodec means the audio formats do not intersect: the peer
	// offered no codec we support, or none that could be parsed.
	ErrNoCommonCodec = errors.New("no common codec")

	// ErrNoCommonCrypto means the media is negotiable but the keying is not.
	// The peer asked for secure RTP and no SRTP context could be built from what
	// it offered. Kept apart from ErrNoCommonCodec because a codec mismatch is a
	// capability gap while this is a security policy gap.
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
// Port 0 is not a parse accident, it has a meaning: the stream is rejected
// (RFC 3264 section 6). It also absorbs every unparseable port, because
// MediaDescription discards strconv's error and leaves the zero value. Both
// readings agree there is no audio to carry. Accepting it built a session
// pointed at nothing, which fails silently on first write, so the call answered
// and then sat mute.
func checkRTPPort(port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%w: audio media port is not usable port=%d", ErrNoCommonMedia, port)
	}
	return nil
}
