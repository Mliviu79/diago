// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/require"
)

// fakePortAllocator hands out ports from a fixed list and records releases.
type fakePortAllocator struct {
	ports    []int
	next     int
	err      error
	released []int
}

func (a *fakePortAllocator) AllocateRTPPort(ctx context.Context) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, context.Cause(ctx)
	}
	if a.err != nil {
		return 0, a.err
	}
	p := a.ports[a.next]
	a.next++
	return p, nil
}

func (a *fakePortAllocator) ReleaseRTPPort(port int) { a.released = append(a.released, port) }

// freeEvenPort returns an even port that is free together with its RTCP companion.
func freeEvenPort(t *testing.T, ip net.IP) int {
	t.Helper()
	for i := 0; i < 50; i++ {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: 0})
		require.NoError(t, err)
		port := c.LocalAddr().(*net.UDPAddr).Port
		c.Close()
		if port%2 != 0 {
			continue
		}
		// The RTCP companion must be free too, as MediaSession binds the pair.
		c2, err := net.ListenUDP("udp4", &net.UDPAddr{IP: ip, Port: port + 1})
		if err != nil {
			continue
		}
		c2.Close()
		return port
	}
	t.Fatal("no free even port pair found")
	return 0
}

// funcPortAllocator hands each allocation's ctx to alloc and records every port
// released to it. It keeps no ctx itself: a test that needs the ctx of an
// allocation captures it inside alloc.
type funcPortAllocator struct {
	alloc    func(ctx context.Context) (int, error)
	released []int
}

func (a *funcPortAllocator) AllocateRTPPort(ctx context.Context) (int, error) {
	return a.alloc(ctx)
}

func (a *funcPortAllocator) ReleaseRTPPort(port int) { a.released = append(a.released, port) }

func TestInitMediaSessionRTPPortAllocator(t *testing.T) {
	bindIP := net.IPv4(127, 0, 0, 1)

	t.Run("BindsAllocatedPortAndReleasesOnClose", func(t *testing.T) {
		// A probed port can be taken by anything else before Init binds it, so a
		// lost race is retried rather than reported as a failure.
		var d *DialogMedia
		var alloc *fakePortAllocator
		var port int
		for attempt := 0; ; attempt++ {
			port = freeEvenPort(t, bindIP)
			alloc = &fakePortAllocator{ports: []int{port}}
			d = &DialogMedia{}
			err := d.initMediaSessionFromConf(t.Context(), MediaConfig{
				Codecs:           []media.Codec{media.CodecAudioUlaw},
				bindIP:           bindIP,
				RTPPortAllocator: alloc,
			})
			if err == nil {
				break
			}
			require.Less(t, attempt, 10, "could not bind a probed free port: %v", err)
		}
		// The allocated port is what actually got bound, not an OS ephemeral one.
		require.Equal(t, port, d.mediaSession.Laddr.Port)
		require.Empty(t, alloc.released, "port released before Close")

		require.NoError(t, d.Close())
		require.Equal(t, []int{port}, alloc.released)

		// Close is latched, so the allocator is never handed the port twice.
		require.NoError(t, d.Close())
		require.Equal(t, []int{port}, alloc.released)
	})

	// A failed Init never reaches Close, so the port has to go back right away
	// or it leaks out of the pool for the life of the process.
	t.Run("ReleasesWhenInitFails", func(t *testing.T) {
		// The test holds the port open, so the session's bind fails on every
		// host. An address that is not local is refused on some hosts and
		// accepted on others, so it cannot be the failing bind.
		var taken *net.UDPConn
		for taken == nil {
			c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: bindIP})
			require.NoError(t, err)
			if c.LocalAddr().(*net.UDPAddr).Port%2 == 0 {
				taken = c
			} else {
				require.NoError(t, c.Close())
			}
		}
		t.Cleanup(func() { _ = taken.Close() })
		port := taken.LocalAddr().(*net.UDPAddr).Port

		alloc := &fakePortAllocator{ports: []int{port}}
		d := &DialogMedia{}
		err := d.initMediaSessionFromConf(t.Context(), MediaConfig{
			Codecs:           []media.Codec{media.CodecAudioUlaw},
			bindIP:           bindIP,
			RTPPortAllocator: alloc,
		})
		require.Error(t, err)
		require.Equal(t, []int{port}, alloc.released)
	})

	// An allocator with nothing left fails the call rather than silently falling
	// back to an unbounded ephemeral port.
	t.Run("AllocationErrorFailsInit", func(t *testing.T) {
		alloc := &fakePortAllocator{err: errors.New("pool exhausted")}
		d := &DialogMedia{}
		err := d.initMediaSessionFromConf(t.Context(), MediaConfig{
			Codecs:           []media.Codec{media.CodecAudioUlaw},
			bindIP:           bindIP,
			RTPPortAllocator: alloc,
		})
		require.ErrorContains(t, err, "pool exhausted")
		require.Nil(t, d.mediaSession)
		require.Empty(t, alloc.released, "nothing was allocated, so nothing to release")
	})

	// The allocation is made on the ctx the caller passed, so a probe or a bind
	// the allocator makes is bounded by the call that asked for the port.
	t.Run("the caller's ctx reaches the allocator", func(t *testing.T) {
		type ctxKey struct{}
		ctx := context.WithValue(t.Context(), ctxKey{}, "caller")
		var got any
		alloc := &funcPortAllocator{alloc: func(allocCtx context.Context) (int, error) {
			got = allocCtx.Value(ctxKey{})
			return 0, nil // 0 lets the OS pick, so the session still binds
		}}
		d := &DialogMedia{}
		require.NoError(t, d.initMediaSessionFromConf(ctx, MediaConfig{
			Codecs:           []media.Codec{media.CodecAudioUlaw},
			bindIP:           bindIP,
			RTPPortAllocator: alloc,
		}))
		t.Cleanup(func() { _ = d.Close() })
		require.Equal(t, "caller", got, "the allocator was not handed the caller's ctx")
	})

	// An allocator that honours an ended ctx fails the init with its cause, and
	// nothing is bound or released.
	t.Run("an allocation on an ended ctx fails the init", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		alloc := &funcPortAllocator{alloc: func(allocCtx context.Context) (int, error) {
			if err := allocCtx.Err(); err != nil {
				return 0, context.Cause(allocCtx)
			}
			return 0, nil
		}}
		d := &DialogMedia{}
		err := d.initMediaSessionFromConf(ctx, MediaConfig{
			Codecs:           []media.Codec{media.CodecAudioUlaw},
			bindIP:           bindIP,
			RTPPortAllocator: alloc,
		})
		t.Cleanup(func() { _ = d.Close() })
		require.ErrorIs(t, err, context.Canceled)
		require.Nil(t, d.mediaSession)
		require.Empty(t, alloc.released, "nothing was allocated, so nothing to release")
	})

	// Nil allocator keeps the historical OS/globals behaviour.
	t.Run("NilAllocatorLeavesPortToOS", func(t *testing.T) {
		d := &DialogMedia{}
		require.NoError(t, d.initMediaSessionFromConf(t.Context(), MediaConfig{
			Codecs: []media.Codec{media.CodecAudioUlaw},
			bindIP: bindIP,
		}))
		defer d.Close()
		require.Nil(t, d.releaseRTPPort)
		require.NotZero(t, d.mediaSession.Laddr.Port)
	})
}

// rebindingPortAllocator hands out one port. When that port is released it
// binds the port and its RTCP companion itself, which succeeds only when the
// sockets of the media that held them are closed by then.
type rebindingPortAllocator struct {
	ip        net.IP
	port      int
	released  []int
	rebindErr error
}

func (a *rebindingPortAllocator) AllocateRTPPort(context.Context) (int, error) {
	return a.port, nil
}

func (a *rebindingPortAllocator) ReleaseRTPPort(port int) {
	a.released = append(a.released, port)
	for _, p := range []int{port, port + 1} {
		c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: a.ip, Port: p})
		if err != nil {
			a.rebindErr = errors.Join(a.rebindErr, err)
			continue
		}
		a.rebindErr = errors.Join(a.rebindErr, c.Close())
	}
}

// TestDialogMediaReleasesThePortAfterItsSockets pins the order Close keeps: the
// port goes back to the allocator once the RTP and RTCP sockets are closed. An
// allocator that starts a drain window at the release, or hands the port to
// the next call, must find the wire down.
func TestDialogMediaReleasesThePortAfterItsSockets(t *testing.T) {
	bindIP := net.IPv4(127, 0, 0, 1)

	// A probed port can be taken by anything else before Init binds it, so a
	// lost race is retried rather than reported as a failure.
	var d *DialogMedia
	var alloc *rebindingPortAllocator
	for attempt := 0; ; attempt++ {
		alloc = &rebindingPortAllocator{ip: bindIP, port: freeEvenPort(t, bindIP)}
		d = &DialogMedia{}
		err := d.initMediaSessionFromConf(t.Context(), MediaConfig{
			Codecs:           []media.Codec{media.CodecAudioUlaw},
			bindIP:           bindIP,
			RTPPortAllocator: alloc,
		})
		if err == nil {
			break
		}
		require.Less(t, attempt, 10, "could not bind a probed free port: %v", err)
	}

	require.NoError(t, d.Close())
	require.Equal(t, []int{alloc.port}, alloc.released)
	require.NoError(t, alloc.rebindErr, "the port was released while its sockets were still bound")
}

// TestDialogMediaSecondCloseWaitsForTheRelease pins that a Close made while an
// earlier Close is still releasing the media returns only after that release
// has run. A caller the peer's end reaches through the second Close is then
// told the media is closed only once its port is back in the pool.
func TestDialogMediaSecondCloseWaitsForTheRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &DialogMedia{}
		inRelease := make(chan struct{})
		unblock := make(chan struct{})
		unblockOnce := sync.OnceFunc(func() { close(unblock) })
		defer unblockOnce() // a failing run must not leave the first Close blocked
		released := false
		d.releaseRTPPort = func() {
			close(inRelease)
			<-unblock
			released = true
		}

		first := make(chan error, 1)
		go func() { first <- d.Close() }()
		<-inRelease

		// released is written before the first Close ends and is read only after
		// the second Close has returned, so the read is ordered after the write.
		second := make(chan bool, 1)
		go func() {
			_ = d.Close()
			second <- released
		}()
		synctest.Wait()
		select {
		case <-second:
			t.Fatal("second Close returned while the first was still releasing the port")
		default:
		}

		unblockOnce()
		require.NoError(t, <-first)
		require.True(t, <-second, "second Close returned before the port was released")
	})
}

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
