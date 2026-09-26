package diago

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIntegrationRecordingStereoWav(t *testing.T) {
	fakePCMFrame := bytes.Repeat([]byte("0123456789"), 32)
	alawFrame := make([]byte, 160)
	_, err := audio.EncodeAlawTo(alawFrame, fakePCMFrame)
	require.NoError(t, err)
	encodedAudio := bytes.Repeat(alawFrame, 4)

	dialog := &DialogServerSession{
		DialogMedia: DialogMedia{
			mediaSession: &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
			// audioReader:  bytes.NewBuffer(make([]byte, 9999)),
			audioReader:     bytes.NewBuffer(encodedAudio),
			audioWriter:     bytes.NewBuffer([]byte{}),
			RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioUlaw),
		},
	}

	recordFile, err := os.OpenFile("/tmp/diago_test_record_stereo.wav", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0755)
	require.NoError(t, err)
	defer recordFile.Close()

	rec, err := dialog.AudioStereoRecordingCreate(recordFile)
	require.NoError(t, err)

	media.ReadAll(rec.AudioReader(), 160)
	media.WriteAll(rec.AudioWriter(), encodedAudio, 160)
	err = rec.Close()

	recordFile.Seek(0, 0)
	wav := audio.NewWavReader(recordFile)
	wav.ReadHeaders()
	// 2 channels, 4 frames Read, 4 frames Write
	assert.Equal(t, 2*4*320, wav.DataSize)
}

// TestRecordingStereoWavCopy pins that every copy of a recording drives one
// monitor. AudioStereoRecordingCreate returns the recording by value, so a
// caller can copy it, for example into a deferred Close. Each copy holding its
// own monitor would share the monitor's buffered writers but not the locks
// guarding them, and closing one copy would not stop writes through another.
func TestRecordingStereoWavCopy(t *testing.T) {
	dialog := &DialogServerSession{
		DialogMedia: DialogMedia{
			mediaSession:    &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
			audioReader:     bytes.NewBuffer(nil),
			audioWriter:     bytes.NewBuffer(nil),
			RTPPacketWriter: media.NewRTPPacketWriter(nil, media.CodecAudioUlaw),
		},
	}

	recordFile, err := os.Create(filepath.Join(t.TempDir(), "record_stereo.wav"))
	require.NoError(t, err)
	defer recordFile.Close()

	rec, err := dialog.AudioStereoRecordingCreate(recordFile)
	require.NoError(t, err)
	recCopy := rec

	assert.True(t, rec.AudioReader() == recCopy.AudioReader(), "the copies read through different monitors")
	assert.True(t, rec.AudioWriter() == recCopy.AudioWriter(), "the copies write through different monitors")
	require.NoError(t, recCopy.Close())
}
