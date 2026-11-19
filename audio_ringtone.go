// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
)

// AudioRingtone is playback for ringtone
type AudioRingtone struct {
	writer     *audio.PCMEncoderWriter
	ringtone   []byte
	sampleSize int
	// dialog is the dialog the ringtone plays on. Its write deadline, which
	// stops the ringtone, is set on its current media session.
	dialog *DialogMedia
}

// PlayBackground plays the ringtone until the returned stop is called. The
// stop ends a write in progress with a write deadline, and clears it again.
func (a *AudioRingtone) PlayBackground() (func() error, error) {
	if err := a.dialog.StartRTP(2, 0); err != nil {
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())

	wg := sync.WaitGroup{}
	wg.Add(1)
	var playErr error
	go func() {
		defer wg.Done()
		playErr = a.play(ctx)
	}()

	return func() error {
		cancel()

		stopErr := a.dialog.StopRTP(2, 0)
		wg.Wait()

		// enable RTP again
		if err := errors.Join(stopErr, a.dialog.StartRTP(2, 0)); err != nil {
			return err
		}

		// The stop ends the ringtone between two rings, through the context,
		// or during one, through the write deadline.
		if errors.Is(playErr, context.Canceled) {
			return nil
		}
		if e, ok := playErr.(net.Error); ok && e.Timeout() {
			return nil
		}

		return playErr
	}, nil
}

func (a *AudioRingtone) Play(ctx context.Context) error {
	return a.play(ctx)
}

func (a *AudioRingtone) play(timerCtx context.Context) error {
	t := time.NewTimer(0)
	for {
		_, err := media.WriteAll(a.writer, a.ringtone, a.sampleSize)
		if err != nil {
			return err
		}

		t.Reset(4 * time.Second)
		select {
		case <-t.C:
		case <-timerCtx.Done():
			return timerCtx.Err()
		}
	}
}
