// SPDX-License-Identifier: MPL-2.0

package media

import (
	"errors"
	"fmt"
)

// ErrCodecNotPaceable is returned, wrapped, by [RTPPacketWriter.UpdateWriter]
// for a codec with no sample rate or no sample duration, such as the zero
// Codec: the writer steps its timestamps and paces its writes by those two,
// and has neither to go by.
var ErrCodecNotPaceable = errors.New("media: codec has no sample rate or sample duration")

// checkPaceable returns an error that wraps ErrCodecNotPaceable when codec
// cannot pace an RTPPacketWriter.
func checkPaceable(codec Codec) error {
	if codec.SampleRate == 0 || codec.SampleDur <= 0 {
		return fmt.Errorf("%w: codec=%q rate=%d duration=%s", ErrCodecNotPaceable, codec.Name, codec.SampleRate, codec.SampleDur)
	}
	return nil
}
