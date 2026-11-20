// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

// silence returns n zero bytes from a buffer the writer keeps across writes.
// The buffer is never written after it is made, so concurrent writes may
// share it; a longer write replaces it with a larger one.
func (c *AudioMuterWriter) silence(n int) []byte {
	if zeros := c.zeroBuf.Load(); zeros != nil && len(*zeros) >= n {
		return (*zeros)[:n:n]
	}
	zeros := make([]byte, n)
	c.zeroBuf.Store(&zeros)
	return zeros
}
