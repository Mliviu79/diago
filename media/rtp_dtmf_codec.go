// SPDX-License-Identifier: MPL-2.0

package media

import "strings"

// CodecTelephoneEventFromSession returns the telephone-event codec the session
// runs on, and whether it has one at all.
//
// DTMF must be sent and received on the payload type negotiation settled on.
// telephone-event is a dynamic format, so each side numbers it independently
// (RFC 3551 section 3) and an answer echoes the offerer's number (RFC 3264
// section 6.1). [CodecTelephoneEvent8000] therefore only describes our own
// default numbering, and is not what a peer necessarily puts on the wire.
//
// The codec is taken from what negotiation settled on once it has, and from
// the session's own list until then: a session forked for a media update starts
// with nothing negotiated and runs on its own list until the remote SDP is
// applied. Of a negotiated codec, PayloadType is the number we send on and
// [Codec.PayloadTypeRecv] the number the peer sends on.
//
// Encoding names are case insensitive (RFC 4566 section 6).
//
// It is not safe for concurrent use with the session's negotiation, like the
// rest of the codec accessors.
func CodecTelephoneEventFromSession(s *MediaSession) (Codec, bool) {
	codecs := s.CommonCodecs()
	if len(codecs) == 0 {
		codecs = s.Codecs
	}
	for _, codec := range codecs {
		if strings.EqualFold(codec.Name, CodecTelephoneEvent8000.Name) {
			return codec, true
		}
	}
	return Codec{}, false
}

// UpdateCodec sets the telephone-event codec DTMF is read on, as a media
// update renegotiates it. The reader takes a packet for DTMF when its payload
// type is codec's PayloadType, so that field carries the number the peer sends
// on. It must not be called while Read runs.
func (w *RTPDtmfReader) UpdateCodec(codec Codec) {
	w.codec = codec
}

// UpdateCodec sets the telephone-event codec DTMF is written on, as a media
// update renegotiates it. It is safe to call while Write or WriteDTMF runs, and
// takes effect on the next event.
func (w *RTPDtmfWriter) UpdateCodec(codec Codec) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.codec = codec
}
