// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestMediaPropsWithoutMediaSession checks that asking a dialog with no media
// session for its media props returns ErrNoMediaSetup rather than panicking on
// the missing session, through AudioReader and AudioWriter and from the options
// themselves, which the bridges call directly.
func TestMediaPropsWithoutMediaSession(t *testing.T) {
	d := &DialogMedia{}
	p := MediaProps{}
	require.NotPanics(t, func() {
		_, err := d.AudioReader(WithAudioReaderMediaProps(&p))
		assert.ErrorIs(t, err, ErrNoMediaSetup)
		assert.ErrorIs(t, WithAudioReaderMediaProps(&p)(d), ErrNoMediaSetup)
	})
	require.NotPanics(t, func() {
		_, err := d.AudioWriter(WithAudioWriterMediaProps(&p))
		assert.ErrorIs(t, err, ErrNoMediaSetup)
		assert.ErrorIs(t, WithAudioWriterMediaProps(&p)(d), ErrNoMediaSetup)
	})
}
