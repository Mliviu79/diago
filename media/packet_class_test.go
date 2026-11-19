// SPDX-License-Identifier: MPL-2.0

package media

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIsRTCP(t *testing.T) {
	// RFC 5761 section 4 reserves payload types 64-95 for RTCP so that RTP and
	// RTCP can share one port without ambiguity.
	tests := []struct {
		name string
		buf  []byte
		want bool
	}{
		{"rtcp sender report 200", []byte{0x80, 200}, true},
		{"rtcp receiver report 201", []byte{0x80, 201}, true},
		{"rtcp bye 203", []byte{0x80, 203}, true},
		{"rtcp lower bound 64", []byte{0x80, 64}, true},
		{"rtcp upper bound 95", []byte{0x80, 95}, true},
		{"rtp pcmu payload 0", []byte{0x80, 0}, false},
		{"rtp pcma payload 8", []byte{0x80, 8}, false},
		{"rtp dynamic payload 96", []byte{0x80, 96}, false},
		{"rtp payload 63 below rtcp range", []byte{0x80, 63}, false},
		// The marker bit shares a byte with the payload type and must be
		// masked off before the range check.
		{"rtp payload 8 with marker bit set", []byte{0x80, 0x80 | 8}, false},
		{"rtcp 200 with top bit set", []byte{0x80, 0x80 | 200&0x7f}, true},
		{"too short", []byte{0x80}, false},
		{"empty", []byte{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsRTCP(tc.buf))
		})
	}
}

func TestIsDTLS(t *testing.T) {
	// RFC 5764 section 5.1.2 assigns the first byte: 20-63 DTLS, 128-191
	// RTP/RTCP, 0-3 STUN.
	tests := []struct {
		name string
		buf  []byte
		want bool
	}{
		{"dtls lower bound 20", []byte{20, 0xfe}, true},
		{"dtls handshake 22", []byte{22, 0xfe, 0xfd}, true},
		{"dtls upper bound 63", []byte{63, 0x00}, true},
		{"rtp 128", []byte{128, 0}, false},
		{"rtcp 128 pt200", []byte{128, 200}, false},
		{"stun binding request", []byte{0x00, 0x01}, false},
		{"below dtls range 19", []byte{19, 0}, false},
		{"above dtls range 64", []byte{64, 0}, false},
		{"empty", []byte{}, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isDTLS(tc.buf))
		})
	}
}
