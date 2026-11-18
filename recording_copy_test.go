// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRecordingStereoWavCopy pins that every copy of a recording drives one
// monitor. AudioStereoRecordingCreate returns the recording by value, so a
// caller can copy it, for example into a deferred Close. Each copy holding its
// own monitor would share the monitor's buffered writers but not the locks
// guarding them, and closing one copy would not stop writes through another.
func TestRecordingStereoWavCopy(t *testing.T) {
	med := &DialogMedia{
		mediaSession:    &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
		audioReader:     bytes.NewBuffer(nil),
		audioWriter:     bytes.NewBuffer(nil),
		RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioUlaw),
	}

	recordFile, err := os.Create(filepath.Join(t.TempDir(), "record_stereo.wav"))
	require.NoError(t, err)
	defer recordFile.Close()

	rec, err := med.AudioStereoRecordingCreate(recordFile)
	require.NoError(t, err)
	recCopy := rec

	assert.True(t, rec.AudioReader() == recCopy.AudioReader(), "the copies read through different monitors")
	assert.True(t, rec.AudioWriter() == recCopy.AudioWriter(), "the copies write through different monitors")
	require.NoError(t, recCopy.Close())
}
