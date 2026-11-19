// SPDX-License-Identifier: MPL-2.0

package media

import "fmt"

// audioInCommon returns nil when common, the formats an offer and our
// configuration share, holds an audio codec, and an error that wraps
// ErrNoCommonCodec when it holds only auxiliary formats or none.
//
// telephone-event (RFC 4733) and comfort noise (RFC 3389) travel beside the
// audio and are no audio themselves: RFC 4733 section 2.1 has the events
// "carried as part of the audio stream" and "use the same sequence number and
// timestamp base as the regular audio channel", and section 1.2 defines them
// as separate payload formats because "low-rate voice codecs cannot be
// guaranteed to reproduce these tone signals". A stream whose only formats in
// common are these has no format to carry the call's audio in, so it is
// treated as one with "no media formats in common" (RFC 3264 section 6.1),
// which the answerer rejects. Accepted, it would leave the session with no
// codec to send or pace audio with.
func audioInCommon(common []Codec) error {
	if _, ok := CodecAudioFromList(common); ok {
		return nil
	}
	names := make([]string, 0, len(common))
	for _, c := range common {
		names = append(names, c.Name)
	}
	return fmt.Errorf("%w: no audio codec in common, only auxiliary formats %q", ErrNoCommonCodec, names)
}
