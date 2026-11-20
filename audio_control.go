package diago

import (
	"io"
	"sync/atomic"
)

type AudioMuterReader struct {
	Reader io.Reader
	muted  atomic.Bool
}

func (c *AudioMuterReader) Read(b []byte) (n int, err error) {
	n, err = c.Reader.Read(b)
	if err != nil {
		return n, err
	}

	if c.muted.Load() {
		for i := range b[:n] {
			b[i] = 0
		}
	}

	return n, err
}

func (c *AudioMuterReader) Mute(mute bool) {
	c.muted.Store(mute)
}

type AudioMuterWriter struct {
	Writer  io.Writer
	muted   atomic.Bool
	zeroBuf atomic.Pointer[[]byte]
}

func (c *AudioMuterWriter) Write(b []byte) (n int, err error) {
	if c.muted.Load() {
		// io.Writer forbids modifying b, so the silence comes from a buffer of our own.
		return c.Writer.Write(c.silence(len(b)))
	}

	return c.Writer.Write(b)
}

func (c *AudioMuterWriter) Mute(mute bool) {
	c.muted.Store(mute)
}

type AudioStopperReader struct {
	Reader io.Reader
	stop   atomic.Bool
}

func (c *AudioStopperReader) Read(b []byte) (n int, err error) {
	if c.stop.Load() {
		return 0, io.EOF
	}

	n, err = c.Reader.Read(b)
	if err != nil {
		return n, err
	}

	return n, err
}

// Stop will stop and return io.Eof
func (c *AudioStopperReader) Stop() {
	c.stop.Store(true)
}

type AudioStopperWriter struct {
	Writer io.Writer
	stop   atomic.Bool
}

func (c *AudioStopperWriter) Write(b []byte) (n int, err error) {
	if c.stop.Load() {
		return 0, io.EOF
	}
	return c.Writer.Write(b)
}

// Stop will stop reader/writer and return io.Eof
func (c *AudioStopperWriter) Stop() {
	c.stop.Store(true)
}
