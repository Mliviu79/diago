// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"io"
	"os"
	"testing"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/require"
)

// TestDialogMediaRTPStatsWithoutRTPSession pins that the RTP statistics
// options refuse a dialog that has a reader and a writer but no RTP session,
// as one set up with InitMediaSession has: there are no statistics to report.
// Installed all the same, the reader and the writer failed on their first use.
func TestDialogMediaRTPStatsWithoutRTPSession(t *testing.T) {
	d := &DialogMedia{}
	d.InitMediaSession(newMediaSessionForTest(t), nil, nil)
	d.SetAudioReader(bytes.NewBuffer(nil))
	d.SetAudioWriter(io.Discard)

	_, err := d.AudioReader(WithAudioReaderRTPStats(func(media.RTPReadStats) {}))
	require.ErrorIs(t, err, ErrNoMediaSetup)
	_, err = d.AudioWriter(WithAudioWriterRTPStats(func(media.RTPWriteStats) {}))
	require.ErrorIs(t, err, ErrNoMediaSetup)
}

// TestDialogMediaPlaybackWithoutMediaSession pins that playback, the ringtone
// and the recording are refused on a dialog that was given a reader and a
// writer but has no media session: the session is what names the codec they
// play and record in.
func TestDialogMediaPlaybackWithoutMediaSession(t *testing.T) {
	d := &DialogMedia{}
	d.SetAudioReader(bytes.NewBuffer(nil))
	d.SetAudioWriter(io.Discard)

	creators := map[string]func(t *testing.T) error{
		"PlaybackCreate": func(t *testing.T) error {
			_, err := d.PlaybackCreate()
			return err
		},
		"PlaybackControlCreate": func(t *testing.T) error {
			_, err := d.PlaybackControlCreate()
			return err
		},
		"PlaybackRingtoneCreate": func(t *testing.T) error {
			_, err := d.PlaybackRingtoneCreate()
			return err
		},
		"AudioStereoRecordingCreate": func(t *testing.T) error {
			f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
			require.NoError(t, err)
			defer f.Close()
			_, err = d.AudioStereoRecordingCreate(f)
			return err
		},
	}
	for name, create := range creators {
		t.Run(name, func(t *testing.T) {
			require.ErrorIs(t, create(t), ErrNoMediaSetup)
		})
	}
}

// TestDialogMediaPlaybackWithoutPacketWriter pins that a playback made on a
// dialog whose audio goes to a writer of the caller's, with no RTP packet
// writer under it, plays: there is no RTP timestamp to reset at each play.
// The playback reset it on the missing packet writer and panicked.
func TestDialogMediaPlaybackWithoutPacketWriter(t *testing.T) {
	d := &DialogMedia{
		mediaSession: &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
		audioWriter:  io.Discard,
	}

	playback, err := d.PlaybackCreate()
	require.NoError(t, err)
	written, err := playback.PlayFile("testdata/files/demo-echodone.wav")
	require.NoError(t, err)
	require.Positive(t, written)
}
