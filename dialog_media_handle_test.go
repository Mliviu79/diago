// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"io"
	"os"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/require"
)

// mediaHandlePeer is the far end of a call whose media a MediaHandle drives:
// it sends the call RTP and reads what the call sends.
type mediaHandlePeer struct {
	// raddr is the peer's address as the call's negotiated props name it.
	raddr     string
	sendAudio func(t *testing.T)
	sendDTMF  func(t *testing.T, pt uint8, marker bool, payload []byte)
	recv      func(t *testing.T) uint8
}

// mediaHandleCase gives a MediaHandle of one media type, set up with PCMU and
// telephone-event at dtmfPT, and its peer; and one of that type with no
// media set up.
type mediaHandleCase struct {
	name   string
	dtmfPT uint8
	setUp  func(t *testing.T) (MediaHandle, *mediaHandlePeer)
	unset  func() MediaHandle
}

func mediaHandleCases() []mediaHandleCase {
	return []mediaHandleCase{
		{
			name:   "DialogMedia",
			dtmfPT: 101,
			setUp: func(t *testing.T) (MediaHandle, *mediaHandlePeer) {
				p := newFollowPeer(t)
				return p.d, &mediaHandlePeer{
					raddr:     p.conn.LocalAddr().String(),
					sendAudio: p.sendAudio,
					sendDTMF: func(t *testing.T, pt uint8, marker bool, payload []byte) {
						p.send(t, pt, marker, 8000, payload)
					},
					recv: func(t *testing.T) uint8 { return p.recv(t).PayloadType },
				}
			},
			unset: func() MediaHandle { return &DialogMedia{} },
		},
		{
			// telephone-event at a number other than the package default.
			name:   "DialogWebrtc",
			dtmfPT: 110,
			setUp: func(t *testing.T) (MediaHandle, *mediaHandlePeer) {
				d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw, telephoneEventAt(110)})
				return d, &mediaHandlePeer{
					raddr:     p.laddr,
					sendAudio: p.sendAudio,
					sendDTMF: func(t *testing.T, pt uint8, marker bool, payload []byte) {
						p.send(t, pt, marker, 8000, payload)
					},
					recv: func(t *testing.T) uint8 { return p.recv(t).PayloadType },
				}
			},
			unset: func() MediaHandle { return &DialogWebrtc{} },
		},
	}
}

// TestMediaHandleSequence drives each media type through its MediaHandle
// with the same sequence an application runs on a call: the negotiated
// props, a read and a write of audio, DTMF both ways, a read ended by
// StopRTP and resumed by StartRTP, and the close.
func TestMediaHandleSequence(t *testing.T) {
	for _, tc := range mediaHandleCases() {
		t.Run(tc.name, func(t *testing.T) {
			h, peer := tc.setUp(t)

			props, err := h.NegotiatedProps()
			require.NoError(t, err)
			require.True(t, props.Codec.Matches(media.CodecAudioUlaw), "NegotiatedProps() codec = %v, want PCMU", props.Codec)
			require.Equal(t, peer.raddr, props.Raddr)
			require.NotEmpty(t, props.Laddr)

			r, err := h.Reader()
			require.NoError(t, err)
			peer.sendAudio(t)
			n, err := readWithin(t, r, 5*time.Second)
			require.NoError(t, err)
			require.Equal(t, 160, n)

			w, err := h.Writer()
			require.NoError(t, err)
			_, err = w.Write(make([]byte, 160))
			require.NoError(t, err)
			require.EqualValues(t, props.Codec.PayloadType, peer.recv(t))

			dr, err := h.AudioReaderDTMF()
			require.NoError(t, err)
			digits := make(chan rune, 4)
			dr.OnDTMF(func(dtmf rune) error {
				digits <- dtmf
				return nil
			})
			peer.sendDTMF(t, tc.dtmfPT, true, media.DTMFEncode(media.DTMFEvent{Event: 5, Volume: 10, Duration: 160}))
			peer.sendDTMF(t, tc.dtmfPT, false, media.DTMFEncode(media.DTMFEvent{Event: 5, EndOfEvent: true, Volume: 10, Duration: 800}))
			for range 2 {
				_, err := readWithin(t, dr, 5*time.Second)
				require.NoError(t, err)
			}
			select {
			case d := <-digits:
				require.Equal(t, '5', d)
			default:
				t.Fatal("the DTMF the peer sent was not read as DTMF")
			}

			dw, err := h.AudioWriterDTMF()
			require.NoError(t, err)
			require.NoError(t, dw.WriteDTMF('7'))
			require.Equal(t, tc.dtmfPT, peer.recv(t))

			// A read waiting on the socket ends when StopRTP sets the read
			// deadline in the past, and reads go on once StartRTP clears it.
			reading := make(chan error, 1)
			go func() {
				buf := make([]byte, media.RTPBufSize)
				_, err := r.Read(buf)
				reading <- err
			}()
			require.NoError(t, h.StopRTP(1, 0))
			select {
			case err := <-reading:
				require.ErrorIs(t, err, os.ErrDeadlineExceeded)
			case <-time.After(5 * time.Second):
				t.Fatal("StopRTP did not end the waiting read")
			}
			require.NoError(t, h.StartRTP(1, 0))
			peer.sendAudio(t)
			_, err = readWithin(t, r, 5*time.Second)
			require.NoError(t, err, "a read deadline was left set")

			// The audio reader ends with io.EOF once the media is closed.
			require.NoError(t, h.Close())
			_, err = readWithin(t, r, 5*time.Second)
			require.ErrorIs(t, err, io.EOF)
		})
	}
}

// TestMediaHandleBeforeSetup pins that a MediaHandle asked for its audio, its
// DTMF or a deadline before the media is set up returns ErrNoMediaSetup.
func TestMediaHandleBeforeSetup(t *testing.T) {
	for _, tc := range mediaHandleCases() {
		t.Run(tc.name, func(t *testing.T) {
			h := tc.unset()
			_, err := h.NegotiatedProps()
			require.ErrorIs(t, err, ErrNoMediaSetup)
			_, err = h.Reader()
			require.ErrorIs(t, err, ErrNoMediaSetup)
			_, err = h.Writer()
			require.ErrorIs(t, err, ErrNoMediaSetup)
			_, err = h.AudioReaderDTMF()
			require.ErrorIs(t, err, ErrNoMediaSetup)
			_, err = h.AudioWriterDTMF()
			require.ErrorIs(t, err, ErrNoMediaSetup)
			require.ErrorIs(t, h.StopRTP(1, 0), ErrNoMediaSetup)
			require.ErrorIs(t, h.StartRTP(1, 0), ErrNoMediaSetup)
		})
	}
}

// mediaHandleAudioHelpers are the audio helpers both media types have beside
// MediaHandle.
type mediaHandleAudioHelpers interface {
	MediaHandle
	ListenContext(ctx context.Context) error
	ListenUntil(dur time.Duration) error
	PlaybackRingtoneCreate() (AudioRingtone, error)
}

// audioHelpersOf returns the audio helpers of h, failing the test when its
// type lacks them.
func audioHelpersOf(t *testing.T, h MediaHandle) mediaHandleAudioHelpers {
	t.Helper()
	a, ok := h.(mediaHandleAudioHelpers)
	require.True(t, ok, "%T lacks the audio helpers", h)
	return a
}

// requireHandleReadable requires a plain read of the call to get the next
// packet the peer sends, so no read deadline was left set.
func requireHandleReadable(t *testing.T, h MediaHandle, peer *mediaHandlePeer) {
	t.Helper()
	r, err := h.Reader()
	require.NoError(t, err)
	peer.sendAudio(t)
	_, err = readWithin(t, r, 5*time.Second)
	require.NoError(t, err, "a read deadline was left set")
}

// TestMediaHandleListenHelpers pins, on each media type, that the helpers
// that end a read with a read deadline end it on a silent peer and leave the
// call's reads as they found them: a deadline left set fails every later read
// at once.
func TestMediaHandleListenHelpers(t *testing.T) {
	for _, tc := range mediaHandleCases() {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("DTMFListen", func(t *testing.T) {
				h, peer := tc.setUp(t)
				r, err := h.AudioReaderDTMF()
				require.NoError(t, err)
				require.NoError(t, r.Listen(func(rune) error { return nil }, 50*time.Millisecond))
				requireHandleReadable(t, h, peer)
			})
			t.Run("ListenContext", func(t *testing.T) {
				h, peer := tc.setUp(t)
				ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
				defer cancel()
				require.NoError(t, audioHelpersOf(t, h).ListenContext(ctx))
				requireHandleReadable(t, h, peer)
			})
			t.Run("ListenUntil", func(t *testing.T) {
				h, peer := tc.setUp(t)
				require.ErrorIs(t, audioHelpersOf(t, h).ListenUntil(50*time.Millisecond), os.ErrDeadlineExceeded)
				requireHandleReadable(t, h, peer)
			})
		})
	}
}

// TestMediaHandleRingtone pins, on each media type, that a ringtone playing
// in the background goes out to the peer, keeps a read deadline another
// helper set, and stops cleanly between two rings, leaving the writes going
// out.
func TestMediaHandleRingtone(t *testing.T) {
	for _, tc := range mediaHandleCases() {
		t.Run(tc.name, func(t *testing.T) {
			h, peer := tc.setUp(t)
			ringtone, err := audioHelpersOf(t, h).PlaybackRingtoneCreate()
			require.NoError(t, err)

			r, err := h.Reader()
			require.NoError(t, err)
			require.NoError(t, h.StopRTP(1, 300*time.Millisecond))

			stop, err := ringtone.PlayBackground()
			require.NoError(t, err)

			_, err = readWithin(t, r, 5*time.Second)
			require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the ringtone cleared the read deadline")
			require.NoError(t, h.StartRTP(1, 0))

			// A ring is 2 s of PCMU in 20 ms frames.
			for range 100 {
				peer.recv(t)
			}
			require.NoError(t, stop(), "a stop between rings is a clean stop")

			w, err := h.Writer()
			require.NoError(t, err)
			_, err = w.Write(make([]byte, 160))
			require.NoError(t, err, "a write deadline was left set")
			peer.recv(t)
		})
	}
}
