// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

type AudioPlaybackControl struct {
	AudioPlayback

	stopper *AudioStopperWriter
	mutter  *AudioMuterWriter
}

func NewAudioPlaybackControl(a AudioPlayback) AudioPlaybackControl {
	// Replace audio playback writer with control
	writer := a.writer

	mutter := &AudioMuterWriter{
		Writer: writer,
	}

	stopper := &AudioStopperWriter{
		Writer: mutter,
	}

	a.writer = stopper
	return AudioPlaybackControl{AudioPlayback: a, mutter: mutter, stopper: stopper}
}

func (p *AudioPlaybackControl) Mute(mute bool) {
	p.mutter.Mute(mute)
}

func (p *AudioPlaybackControl) Stop() {
	p.stopper.Stop()
}

/*
	Playback control should provide functionality like Mute Unmute over audio.
*/
