// SPDX-License-Identifier: MPL-2.0

package sdp

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestFormatsStringNamesG722(t *testing.T) {
	fmts := NewFormats(FORMAT_TYPE_ULAW, FORMAT_TYPE_ALAW, FORMAT_TYPE_G722, FORMAT_TYPE_OPUS, FORMAT_TYPE_TELEPHONE_EVENT)

	assert.Equal(t, "0(ulaw),8(alaw),9(g722),96(opus),101", fmts.String())
}

// G.722 is described on the 8000 clock RFC 3551 section 4.5.2 gives it, with no
// encoding parameters.
func TestGenerateForAudioDescribesG722(t *testing.T) {
	ip := net.IPv4(127, 0, 0, 1)
	out := string(GenerateForAudio(ip, ip, 10000, ModeSendrecv, NewFormats(FORMAT_TYPE_G722, FORMAT_TYPE_TELEPHONE_EVENT)))

	assert.Contains(t, out, "m=audio 10000 RTP/AVP 9 101\r\n")
	assert.Contains(t, out, "a=rtpmap:9 G722/8000\r\n")
}
