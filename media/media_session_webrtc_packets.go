// SPDX-License-Identifier: MPL-2.0

package media

import "github.com/pion/srtp/v3"

// webrtcPacketPath is what one packet read or write needs of a
// MediaSessionWebrtc, as the session stood at one moment. Packets are read and
// written on the application's goroutines while the session is negotiated on
// others: RemoteSDP builds the mux, Finalize keys the session and commits the
// direction and the codecs of a subsequent description. All of that is written
// under MediaSessionWebrtc.mu, so a packet path copies it there and works on
// the copy.
type webrtcPacketPath struct {
	// mux is the session's packet mux, nil until a remote description has
	// been applied.
	mux *webRTCPacketMux

	// The SRTP and SRTCP contexts are nil until the DTLS handshake has keyed
	// the session. The media of a WebRTC session is SRTP alone, and "SRTP
	// processing MUST NOT take place before the DTLS handshake completes"
	// (RFC 5764 section 5.1): a path without them carries nothing.
	localSRTP   *srtp.Context
	remoteSRTP  *srtp.Context
	localSRTCP  *srtp.Context
	remoteSRTCP *srtp.Context

	// mode is the negotiated direction and codecs the negotiated codecs. The
	// session replaces its codec slice and never writes into it, so the copy
	// shares the elements.
	mode   string
	codecs []Codec

	// mid is the header extension that carries the identification-tag of the
	// audio stream in the packets written, as the exchange the session
	// committed to left it: see webrtcStreams.mid.
	mid midExtension
}

// packetPath returns what a packet read or write needs of the session as it
// stands now. It is taken per packet, after a read has returned and before a
// write, so that each packet meets the keys and the direction that hold when
// it is handled.
func (m *MediaSessionWebrtc) packetPath() webrtcPacketPath {
	m.mu.Lock()
	defer m.mu.Unlock()
	path := webrtcPacketPath{mux: m.mux, mode: m.Mode, codecs: m.Codecs, mid: m.streams.mid}
	if !m.ready {
		return path
	}
	path.localSRTP = m.localCtxSRTP
	path.remoteSRTP = m.remoteCtxSRTP
	path.localSRTCP = m.localCtxSRTCP
	path.remoteSRTCP = m.remoteCtxSRTCP
	return path
}
