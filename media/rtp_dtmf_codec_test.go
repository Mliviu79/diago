// SPDX-License-Identifier: MPL-2.0

package media

import (
	"bytes"
	"io"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The telephone-event codec of a session is the negotiated one once there is
// one, and the session's own until then. A session that negotiated none reports
// that, so the caller decides what DTMF runs on.
func TestCodecTelephoneEventFromSession(t *testing.T) {
	te100 := telephoneEventAt(100)
	upper := telephoneEventAt(100)
	upper.Name = "TELEPHONE-EVENT"

	tests := []struct {
		name       string
		local      []Codec
		negotiated []Codec
		want       Codec
		wantOK     bool
	}{
		{
			name:       "negotiated_number",
			local:      []Codec{CodecAudioAlaw, CodecTelephoneEvent8000},
			negotiated: []Codec{CodecAudioAlaw, te100},
			want:       te100,
			wantOK:     true,
		},
		{
			name:   "local_list_before_negotiation",
			local:  []Codec{CodecAudioAlaw, CodecTelephoneEvent8000},
			want:   CodecTelephoneEvent8000,
			wantOK: true,
		},
		{
			name:       "not_negotiated",
			local:      []Codec{CodecAudioAlaw, CodecTelephoneEvent8000},
			negotiated: []Codec{CodecAudioAlaw},
		},
		{
			name:  "not_in_local_list",
			local: []Codec{CodecAudioAlaw},
		},
		{
			name:       "name_in_another_case",
			local:      []Codec{CodecAudioAlaw, CodecTelephoneEvent8000},
			negotiated: []Codec{CodecAudioAlaw, upper},
			want:       upper,
			wantOK:     true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := negotiateSession(tc.local...)
			m.filterCodecs = tc.negotiated

			got, ok := CodecTelephoneEventFromSession(m)
			assert.Equal(t, tc.wantOK, ok)
			assert.Equal(t, tc.want, got)
		})
	}
}

// An answer may number telephone-event differently from our offer. We then send
// on the peer's number and the peer sends on ours (RFC 3264 section 5.1), and
// the codec carries both.
func TestCodecTelephoneEventFromSessionAnswerRenumbers(t *testing.T) {
	m := negotiateSession(CodecAudioAlaw, CodecTelephoneEvent8000)
	require.Equal(t, 2, m.updateRemoteCodecs([]Codec{CodecAudioAlaw, telephoneEventAt(100)}, false))

	te, ok := CodecTelephoneEventFromSession(m)
	require.True(t, ok)
	assert.EqualValues(t, 100, te.PayloadType, "we send on the number the answer gave")
	assert.Equal(t, CodecTelephoneEvent8000.PayloadType, te.PayloadTypeRecv(), "the peer sends on the number we offered")
}

// A media update can move telephone-event to another number. The reader takes
// events at the old number until it is told, and at the new one after.
func TestRTPDtmfReaderUpdateCodec(t *testing.T) {
	var payload bytes.Buffer
	packetReader := &RTPPacketReader{}
	r := NewRTPDTMFReader(CodecTelephoneEvent8000, packetReader, &payload)

	// readDigit1 reads one whole event for digit 1 carried at payload type pt
	// and returns what the reader made of it.
	readDigit1 := func(pt uint8) (rune, bool) {
		events := []DTMFEvent{
			{Event: 1, EndOfEvent: false, Volume: 10, Duration: 160},
			{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800},
		}
		buf := make([]byte, RTPBufSize)
		for i, ev := range events {
			payload.Reset()
			payload.Write(DTMFEncode(ev))
			packetReader.PacketHeader = rtp.Header{PayloadType: pt, Marker: i == 0, Timestamp: 160}
			_, err := r.Read(buf)
			require.NoError(t, err)
		}
		return r.ReadDTMF()
	}

	_, ok := readDigit1(100)
	require.False(t, ok, "100 is not the reader's telephone-event number yet")

	r.UpdateCodec(telephoneEventAt(100))

	dtmf, ok := readDigit1(100)
	require.True(t, ok, "an event at the updated number must be read as DTMF")
	assert.Equal(t, '1', dtmf)

	_, ok = readDigit1(CodecTelephoneEvent8000.PayloadType)
	assert.False(t, ok, "the number the update replaced is no longer DTMF")
}

// The writer stamps events with the number it was last given.
func TestRTPDtmfWriterUpdateCodec(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wire := &rtpBuffer{}
		packetWriter := NewRTPPacketWriter(wire, CodecAudioUlaw)
		defer packetWriter.ClockDisable()
		w := NewRTPDTMFWriter(CodecTelephoneEvent8000, packetWriter, io.Discard)

		require.NoError(t, w.WriteDTMF('1'))
		w.UpdateCodec(telephoneEventAt(100))
		require.NoError(t, w.WriteDTMF('1'))

		got := make([]uint8, 0, len(wire.buf))
		for _, pkt := range wire.buf {
			got = append(got, pkt.PayloadType)
		}
		perDigit := len(RTPDTMFEncode8000('1'))
		want := slices.Concat(
			slices.Repeat([]uint8{CodecTelephoneEvent8000.PayloadType}, perDigit),
			slices.Repeat([]uint8{100}, perDigit),
		)
		assert.Equal(t, want, got)
	})
}
