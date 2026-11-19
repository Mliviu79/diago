// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestDialogMediaRTPControlReadsMediaUnderLock asserts that stopping and
// starting RTP reads, directly or through the Listen helpers, reads the media
// session under the dialog lock. A re-INVITE swaps the session for a fork of it
// under that lock, so an unguarded read races it. The race only shows under
// -race.
func TestDialogMediaRTPControlReadsMediaUnderLock(t *testing.T) {
	// swapWhile runs control while another goroutine swaps d's session for a
	// fork of it, as a re-INVITE does.
	swapWhile := func(t *testing.T, d *DialogMedia, control func() error) {
		t.Helper()
		d.mu.Lock()
		fork := d.mediaSession.Fork()
		d.mu.Unlock()

		swapped := make(chan struct{})
		go func() {
			defer close(swapped)
			d.mu.Lock()
			d.mediaSession = fork
			d.mu.Unlock()
		}()
		require.NoError(t, control())
		select {
		case <-swapped:
		case <-time.After(5 * time.Second):
			t.Fatal("the session was never swapped")
		}
	}
	// newAnswered returns dialog media negotiated with a peer that sends
	// nothing, with its RTP monitor running.
	newAnswered := func(t *testing.T) *DialogMedia {
		t.Helper()
		sess := newMediaSessionForTest(t)
		require.NoError(t, sess.RemoteSDP(newMediaSessionForTest(t).LocalSDP(false), false))
		return newICEDialogMedia(t, sess)
	}
	// timeoutIsEnd reports a read ended by its deadline as a clean end.
	timeoutIsEnd := func(err error) error {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			return nil
		}
		return err
	}

	t.Run("StopRTP and StartRTP", func(t *testing.T) {
		d := newAnswered(t)
		swapWhile(t, d, func() error {
			return errors.Join(d.StopRTP(1, 0), d.StartRTP(1, 0))
		})
	})

	t.Run("ListenBackground", func(t *testing.T) {
		d := newAnswered(t)
		stop, err := d.ListenBackground()
		require.NoError(t, err)
		swapWhile(t, d, stop)
	})

	t.Run("ListenContext", func(t *testing.T) {
		d := newAnswered(t)
		swapWhile(t, d, func() error {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			return d.ListenContext(ctx)
		})
	})

	t.Run("ListenUntil", func(t *testing.T) {
		d := newAnswered(t)
		swapWhile(t, d, func() error {
			return timeoutIsEnd(d.ListenUntil(20 * time.Millisecond))
		})
	})
}

// TestDialogMediaAudioBeforeSetup pins that asking a dialog for its audio
// before its media is set up is an error rather than a reader or writer that
// panics when used. The media is set up by the answer, or by ProgressMedia for
// early media, so it is missing before either: a dialog then has no media
// session, or one that has taken part in no offer/answer exchange yet and so
// has no RTP session. Before, AudioReader and AudioWriter returned the missing
// packet reader or writer as a non-nil interface holding a nil pointer, and
// the helpers built on them panicked.
func TestDialogMediaAudioBeforeSetup(t *testing.T) {
	// call runs f, turning a panic into a test failure so every row reports.
	call := func(t *testing.T, name string, f func() error) {
		t.Helper()
		var err error
		panicked := func() (p any) {
			defer func() { p = recover() }()
			err = f()
			return nil
		}()
		if panicked != nil {
			t.Errorf("%s panicked: %v", name, panicked)
			return
		}
		if !errors.Is(err, ErrNoMediaSetup) {
			t.Errorf("%s: want ErrNoMediaSetup, got %v", name, err)
		}
	}

	dialogs := map[string]func(t *testing.T) *DialogMedia{
		"no media session": func(t *testing.T) *DialogMedia { return &DialogMedia{} },
		"media session without RTP session": func(t *testing.T) *DialogMedia {
			return &DialogMedia{mediaSession: newMediaSessionForTest(t)}
		},
	}
	for name, newDialog := range dialogs {
		t.Run(name, func(t *testing.T) {
			d := newDialog(t)
			call(t, "AudioReader", func() error {
				r, err := d.AudioReader()
				require.Nil(t, r)
				return err
			})
			call(t, "AudioReader with media props", func() error {
				_, err := d.AudioReader(WithAudioReaderMediaProps(&MediaProps{}))
				return err
			})
			call(t, "AudioReader with DTMF", func() error {
				_, err := d.AudioReader(WithAudioReaderDTMF(&DTMFReader{}))
				return err
			})
			call(t, "AudioWriter", func() error {
				w, err := d.AudioWriter()
				require.Nil(t, w)
				return err
			})
			call(t, "AudioWriter with media props", func() error {
				_, err := d.AudioWriter(WithAudioWriterMediaProps(&MediaProps{}))
				return err
			})
			call(t, "AudioWriter with DTMF", func() error {
				_, err := d.AudioWriter(WithAudioWriterDTMF(&DTMFWriter{}))
				return err
			})
			call(t, "AudioReaderDTMF", func() error {
				_, err := d.AudioReaderDTMF()
				return err
			})
			call(t, "AudioWriterDTMF", func() error {
				_, err := d.AudioWriterDTMF()
				return err
			})
			call(t, "PlaybackCreate", func() error {
				_, err := d.PlaybackCreate()
				return err
			})
			call(t, "PlaybackControlCreate", func() error {
				_, err := d.PlaybackControlCreate()
				return err
			})
			call(t, "PlaybackRingtoneCreate", func() error {
				_, err := d.PlaybackRingtoneCreate()
				return err
			})
			call(t, "AudioStereoRecordingCreate", func() error {
				f, err := os.CreateTemp(t.TempDir(), "rec-*.wav")
				require.NoError(t, err)
				defer f.Close()
				_, err = d.AudioStereoRecordingCreate(f)
				return err
			})
			call(t, "Echo", d.Echo)
			call(t, "Listen", d.Listen)
			call(t, "ListenContext", func() error { return d.ListenContext(context.Background()) })
			call(t, "ListenUntil", func() error { return d.ListenUntil(time.Millisecond) })
			call(t, "ListenBackground", func() error {
				_, err := d.ListenBackground()
				return err
			})
			if d.mediaSession == nil {
				// With a media session its sockets take a deadline.
				call(t, "StopRTP", func() error { return d.StopRTP(1, 0) })
				call(t, "StartRTP", func() error { return d.StartRTP(1, 0) })
			}
		})
	}
}
