// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAudioControl(t *testing.T) {
	receiver := bytes.NewBuffer([]byte{})
	p := AudioMuterWriter{
		Writer: receiver,
	}

	payload := []byte{1, 2, 3}
	original := bytes.Clone(payload)

	n, err := p.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	res := bytes.Clone(payload)
	require.Equal(t, res, receiver.Bytes())

	_, err = p.Write(payload)
	require.NoError(t, err)
	res = append(res, payload...)
	require.Equal(t, res, receiver.Bytes())

	p.Mute(true)
	n, err = p.Write(payload)
	require.NoError(t, err)
	require.Equal(t, len(payload), n)
	require.Equal(t, original, payload, "a muted write must leave the caller's buffer unchanged")
	res = append(res, []byte{0, 0, 0}...)
	require.Equal(t, res, receiver.Bytes())

	p.Mute(false)
	_, err = p.Write(payload)
	require.NoError(t, err)
	res = append(res, original...)
	require.Equal(t, res, receiver.Bytes())
}

func TestAudioMuterWriterSilenceFollowsWriteLength(t *testing.T) {
	receiver := bytes.NewBuffer([]byte{})
	p := AudioMuterWriter{Writer: receiver}
	p.Mute(true)

	for _, size := range []int{4, 160, 2, 320, 0} {
		payload := bytes.Repeat([]byte{0x7f}, size)
		receiver.Reset()

		n, err := p.Write(payload)
		require.NoError(t, err)
		assert.Equal(t, size, n)
		assert.Equal(t, make([]byte, size), receiver.Bytes())
		assert.Equal(t, bytes.Repeat([]byte{0x7f}, size), payload)
	}
}

func TestAudioMuterWriterReusesSilence(t *testing.T) {
	p := AudioMuterWriter{Writer: io.Discard}
	p.Mute(true)
	payload := make([]byte, 160)
	_, err := p.Write(payload)
	require.NoError(t, err)

	allocs := testing.AllocsPerRun(100, func() {
		if _, err := p.Write(payload[:80]); err != nil {
			t.Error(err)
		}
	})
	assert.Zero(t, allocs)
}

func TestAudioMuterWriterConcurrentWriteAndMute(t *testing.T) {
	p := AudioMuterWriter{Writer: io.Discard}

	var wg sync.WaitGroup
	for i := range 4 {
		wg.Go(func() {
			payload := bytes.Repeat([]byte{1}, 80*(i+1))
			for range 200 {
				if _, err := p.Write(payload); err != nil {
					t.Error(err)
					return
				}
				if !bytes.Equal(payload, bytes.Repeat([]byte{1}, 80*(i+1))) {
					t.Error("write modified the caller's buffer")
					return
				}
			}
		})
	}
	wg.Go(func() {
		for j := range 200 {
			p.Mute(j%2 == 0)
		}
	})
	wg.Wait()
}

func TestAudioMuterReader(t *testing.T) {
	r := AudioMuterReader{Reader: strings.NewReader("\x01\x02\x03\x04")}

	buf := make([]byte, 2)
	n, err := r.Read(buf)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 2}, buf[:n])

	r.Mute(true)
	n, err = r.Read(buf)
	require.NoError(t, err)
	require.Equal(t, []byte{0, 0}, buf[:n])
}

func TestAudioStopperWriter(t *testing.T) {
	receiver := bytes.NewBuffer([]byte{})
	w := AudioStopperWriter{Writer: receiver}

	n, err := w.Write([]byte{1, 2})
	require.NoError(t, err)
	require.Equal(t, 2, n)

	w.Stop()
	n, err = w.Write([]byte{3, 4})
	require.ErrorIs(t, err, io.EOF)
	require.Zero(t, n)
	require.Equal(t, []byte{1, 2}, receiver.Bytes(), "a stopped writer must not write")
}

func TestAudioStopperReader(t *testing.T) {
	src := strings.NewReader("\x01\x02\x03\x04")
	r := AudioStopperReader{Reader: src}

	buf := make([]byte, 2)
	n, err := r.Read(buf)
	require.NoError(t, err)
	require.Equal(t, []byte{1, 2}, buf[:n])

	r.Stop()
	n, err = r.Read(buf)
	require.ErrorIs(t, err, io.EOF)
	require.Zero(t, n)
	require.Equal(t, 2, src.Len(), "a stopped reader must not read")
}
