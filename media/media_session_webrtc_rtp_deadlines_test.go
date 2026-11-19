// SPDX-License-Identifier: MPL-2.0

package media

import (
	"os"
	"testing"
	"time"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// newKeyedWebrtcSRTPPeers returns a WebRTC session that has its transport and
// its keys, as after Finalize, and the peer at the other end of it.
func newKeyedWebrtcSRTPPeers(t *testing.T) (*MediaSessionWebrtc, *webrtcSRTPPeer) {
	t.Helper()
	sess, peer := newWebrtcSRTPPeers(t)
	keyWebrtcSession(sess, peer)
	return sess, peer
}

// readWebrtcRTP reads one RTP packet of sess on a goroutine of its own, so
// that a read no deadline ends fails its test instead of hanging it.
func readWebrtcRTP(sess *MediaSessionWebrtc) <-chan error {
	done := make(chan error, 1)
	go func() {
		_, err := sess.ReadRTP(make([]byte, RTPBufSize), &rtp.Packet{})
		done <- err
	}()
	return done
}

// readEnded returns what a read started by readWebrtcRTP, or by a packet
// reader, returned, and fails the test when the read is still waiting.
func readEnded(t *testing.T, done <-chan error, waiting string) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal(waiting)
		return nil
	}
}

// requireWebrtcReadStopped requires a read of sess to return a timeout at
// once, although a packet of the peer is there to be read.
func requireWebrtcReadStopped(t *testing.T, sess *MediaSessionWebrtc, peer *webrtcSRTPPeer, msg string) {
	t.Helper()
	peer.send(t, encryptFor(t, peer.sendSRTP, webrtcRTPPacket(1, "not to be read")))
	err := readEnded(t, readWebrtcRTP(sess), "the read waited: "+msg)
	require.True(t, os.IsTimeout(err), "ReadRTP() = %v, want a timeout: %s", err, msg)
}

// requireWebrtcReadGoesOn requires a read of sess to return a packet the peer
// sends.
func requireWebrtcReadGoesOn(t *testing.T, sess *MediaSessionWebrtc, peer *webrtcSRTPPeer, msg string) {
	t.Helper()
	peer.send(t, encryptFor(t, peer.sendSRTP, webrtcRTPPacket(2, "to be read")))
	require.NoError(t, readEnded(t, readWebrtcRTP(sess), "the read did not return the packet: "+msg), msg)
}

// requireWebrtcWriteStopped requires a write of sess to fail on the write
// deadline.
func requireWebrtcWriteStopped(t *testing.T, sess *MediaSessionWebrtc, msg string) {
	t.Helper()
	require.ErrorIs(t, sess.WriteRTP(webrtcRTPPacket(3, "not to be sent")), os.ErrDeadlineExceeded, msg)
}

// TestWebrtcStopRTPStopsAtOnce pins that StopRTP with no duration stops the
// reads of a WebRTC session at once, as it does on a MediaSession: the
// deadline is the time of the call plus the duration, whatever the duration.
// It cleared the deadline instead, so a caller that stopped its reads, as
// Bridge does to end its proxy, left them running. StartRTP is what clears.
func TestWebrtcStopRTPStopsAtOnce(t *testing.T) {
	for name, rw := range map[string]int8{"Read": 1, "Both": 0} {
		t.Run(name, func(t *testing.T) {
			sess, peer := newKeyedWebrtcSRTPPeers(t)

			require.NoError(t, sess.StopRTP(rw, 0))
			requireWebrtcReadStopped(t, sess, peer, "StopRTP with no duration")

			require.NoError(t, sess.StartRTP(rw))
			requireWebrtcReadGoesOn(t, sess, peer, "StartRTP clears the stop")
		})
	}

	// The write side is not StopRTP's on this type, with or without a
	// duration: the session goes on sending.
	t.Run("LeavesWritesAlone", func(t *testing.T) {
		sess, _ := newKeyedWebrtcSRTPPeers(t)
		require.NoError(t, sess.StopRTP(0, 0))
		require.NoError(t, sess.StopRTP(2, 0))
		require.NoError(t, sess.WriteRTP(webrtcRTPPacket(3, "still sent")))
	})
}

// TestWebrtcSetRTPDeadline pins the deadline set by time on a WebRTC session:
// on the read side, on the write side, and on both, with the zero time
// clearing what was set.
func TestWebrtcSetRTPDeadline(t *testing.T) {
	t.Run("EndsAWaitingRead", func(t *testing.T) {
		sess, _ := newKeyedWebrtcSRTPPeers(t)
		done := readWebrtcRTP(sess)
		require.NoError(t, sess.SetRTPDeadline(1, time.Now()))
		err := readEnded(t, done, "the read deadline did not end the read")
		require.True(t, os.IsTimeout(err), "ReadRTP() = %v, want a timeout", err)

		// The read side alone was set.
		require.NoError(t, sess.WriteRTP(webrtcRTPPacket(3, "still sent")))
	})

	t.Run("Read", func(t *testing.T) {
		sess, peer := newKeyedWebrtcSRTPPeers(t)
		require.NoError(t, sess.SetRTPDeadline(1, time.Now()))
		requireWebrtcReadStopped(t, sess, peer, "a read deadline that has passed")

		require.NoError(t, sess.SetRTPDeadline(1, time.Time{}))
		requireWebrtcReadGoesOn(t, sess, peer, "the zero time clears the read deadline")
	})

	t.Run("Write", func(t *testing.T) {
		sess, peer := newKeyedWebrtcSRTPPeers(t)
		require.NoError(t, sess.SetRTPDeadline(2, time.Now()))
		requireWebrtcWriteStopped(t, sess, "a write deadline that has passed")
		requireWebrtcReadGoesOn(t, sess, peer, "the write side alone was set")

		// StartRTP does not reach the write side.
		require.NoError(t, sess.StartRTP(2))
		require.NoError(t, sess.StartRTP(0))
		requireWebrtcWriteStopped(t, sess, "StartRTP clears a read deadline only")

		require.NoError(t, sess.SetRTPDeadline(2, time.Time{}))
		require.NoError(t, sess.WriteRTP(webrtcRTPPacket(3, "sent again")))
	})

	t.Run("Both", func(t *testing.T) {
		sess, peer := newKeyedWebrtcSRTPPeers(t)
		require.NoError(t, sess.SetRTPDeadline(0, time.Now()))
		requireWebrtcReadStopped(t, sess, peer, "a deadline on both sides")
		requireWebrtcWriteStopped(t, sess, "a deadline on both sides")

		require.NoError(t, sess.SetRTPDeadline(0, time.Time{}))
		requireWebrtcReadGoesOn(t, sess, peer, "the zero time clears both sides")
		require.NoError(t, sess.WriteRTP(webrtcRTPPacket(3, "sent again")))
	})

	// A session without a remote description has no transport to set a
	// deadline on, and says so, as StopRTP does.
	t.Run("NoTransport", func(t *testing.T) {
		sess := &MediaSessionWebrtc{}
		require.Error(t, sess.StopRTP(1, 0))
		require.Error(t, sess.StartRTP(1))
		require.Error(t, sess.SetRTPDeadline(1, time.Now()))
		from, _ := newKeyedWebrtcSRTPPeers(t)
		require.Error(t, sess.CarryRTPDeadlines(from))
	})
}

// TestWebrtcCarryRTPDeadlines is TestMediaSessionCarryRTPDeadlines on the
// WebRTC session, which a new ICE generation replaces with a session on a
// transport of its own: the replacement takes over the read and write
// deadlines last set on the session it replaces, however they were set, and
// has them on its transport, so a read that was stopped stays stopped and a
// deadline that was to end a read or a write still ends it.
func TestWebrtcCarryRTPDeadlines(t *testing.T) {
	old, _ := newKeyedWebrtcSRTPPeers(t)
	require.NoError(t, old.StopRTP(1, 0))
	require.NoError(t, old.SetRTPDeadline(2, time.Now()))

	moved, movedPeer := newKeyedWebrtcSRTPPeers(t)
	require.NoError(t, moved.CarryRTPDeadlines(old))
	requireWebrtcReadStopped(t, moved, movedPeer, "the read deadline was not carried")
	requireWebrtcWriteStopped(t, moved, "the write deadline was not carried")

	// A second generation takes them from the first replacement.
	next, nextPeer := newKeyedWebrtcSRTPPeers(t)
	require.NoError(t, next.CarryRTPDeadlines(moved))
	requireWebrtcReadStopped(t, next, nextPeer, "the read deadline was not carried twice")
	requireWebrtcWriteStopped(t, next, "the write deadline was not carried twice")

	// Carrying a session's deadlines to itself changes nothing.
	require.NoError(t, next.CarryRTPDeadlines(next))
	requireWebrtcReadStopped(t, next, nextPeer, "a session lost its own read deadline")
	requireWebrtcWriteStopped(t, next, "a session lost its own write deadline")

	// Cleared deadlines are carried as cleared.
	require.NoError(t, moved.StartRTP(1))
	require.NoError(t, moved.SetRTPDeadline(2, time.Time{}))
	cleared, clearedPeer := newKeyedWebrtcSRTPPeers(t)
	require.NoError(t, cleared.SetRTPDeadline(0, time.Now()))
	require.NoError(t, cleared.CarryRTPDeadlines(moved))
	requireWebrtcReadGoesOn(t, cleared, clearedPeer, "the cleared read deadline was not carried")
	require.NoError(t, cleared.WriteRTP(webrtcRTPPacket(3, "sent")), "the cleared write deadline was not carried")
}

// TestWebrtcRTPDeadlinesFollowICERestart carries the deadlines across a real
// ICE restart: a Fork, negotiated and keyed against the peer's, takes over the
// read stop and the write deadline of the generation it replaces, on the
// candidate pair of its own agent, and passes media again once they are
// cleared on it.
func TestWebrtcRTPDeadlinesFollowICERestart(t *testing.T) {
	ctx := t.Context()
	offerer, answerer, _, _ := newEstablishedWebrtcPair(t, nil)

	require.NoError(t, answerer.StopRTP(1, 0))
	require.NoError(t, answerer.SetRTPDeadline(2, time.Now()))

	offerFork, err := offerer.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, offerFork)
	restartOffer, err := offerFork.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.ErrorIs(t, answerer.RemoteSDP(ctx, restartOffer, false), ErrWebRTCICERestart)
	answerFork, err := answerer.Fork(ctx)
	require.NoError(t, err)
	closeWebrtcWithTest(t, answerFork)
	require.NoError(t, answerFork.RemoteSDP(ctx, restartOffer, false))
	restartAnswer, err := answerFork.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, offerFork.RemoteSDP(ctx, restartAnswer, true))
	requireBothFinalize(t, offerFork, answerFork, 40*time.Second, "the new ICE generation")

	require.NoError(t, answerFork.CarryRTPDeadlines(answerer))

	require.NoError(t, offerFork.WriteRTP(dtlsTestPacket(1)))
	err = readEnded(t, readWebrtcRTP(answerFork), "the read stop did not follow the ICE restart")
	require.True(t, os.IsTimeout(err), "ReadRTP() = %v, want a timeout", err)
	// The ICE agent does not hand on the timeout of its socket, so the write
	// is told from one that went out by its error and by the peer, which
	// reads packet 4 below and not this one.
	require.Error(t, answerFork.WriteRTP(dtlsTestPacket(2)), "the write deadline did not follow the ICE restart")

	// Cleared, the packet the stopped read left is read, and media passes
	// both ways.
	require.NoError(t, answerFork.SetRTPDeadline(0, time.Time{}))
	require.NoError(t, readEnded(t, readWebrtcRTP(answerFork), "the read did not go on once the stop was cleared"))
	requireWebrtcSRTPBetween(t, offerFork, answerFork, 3)
	requireWebrtcSRTPBetween(t, answerFork, offerFork, 4)
}

// TestRTPPacketReaderSwapKeepsWebrtcStopDeadline is
// TestRTPPacketReaderSwapKeepsStopDeadline on the WebRTC session. Moving the
// packet reader ends a read waiting on the session it reads with a read
// deadline of the move's own. That deadline is not the application's: on a
// session the reader keeps reading it gives way to the one the application
// asked for, which is none when it asked for none, and on a session of a new
// ICE generation the carried one is in force.
func TestRTPPacketReaderSwapKeepsWebrtcStopDeadline(t *testing.T) {
	type webrtcEnd struct {
		sess *MediaSessionWebrtc
		peer *webrtcSRTPPeer
	}
	newEnd := func(t *testing.T) webrtcEnd {
		t.Helper()
		sess, peer := newKeyedWebrtcSRTPPeers(t)
		return webrtcEnd{sess: sess, peer: peer}
	}
	// replace moves reader to next the way a media update does: the
	// deadlines first, then the reader.
	replace := func(t *testing.T, reader *RTPPacketReader, cur, next webrtcEnd) {
		t.Helper()
		require.NoError(t, next.sess.CarryRTPDeadlines(cur.sess))
		reader.UpdateReader(NewRTPSessionWebrtc(next.sess))
	}
	read := func(reader *RTPPacketReader) <-chan error {
		done := make(chan error, 1)
		go func() {
			_, err := reader.Read(make([]byte, RTPBufSize))
			done <- err
		}()
		return done
	}
	requireDeadline := func(t *testing.T, done <-chan error) {
		t.Helper()
		err := readEnded(t, done, "the read went on without the deadline StopRTP set")
		require.True(t, os.IsTimeout(err), "Read() = %v, want a timeout", err)
	}
	requireGoesOn := func(t *testing.T, reader *RTPPacketReader, end webrtcEnd, msg string) {
		t.Helper()
		done := read(reader)
		end.peer.send(t, encryptFor(t, end.peer.sendSRTP, webrtcRTPPacket(2, "to be read")))
		require.NoError(t, readEnded(t, done, "the read did not return the packet: "+msg), msg)
	}

	replacements := map[string]func(t *testing.T, cur webrtcEnd) webrtcEnd{
		"SameSession":  func(_ *testing.T, cur webrtcEnd) webrtcEnd { return cur },
		"OtherSession": func(t *testing.T, _ webrtcEnd) webrtcEnd { return newEnd(t) },
	}
	for name, replacement := range replacements {
		t.Run(name, func(t *testing.T) {
			t.Run("NotStopped", func(t *testing.T) {
				cur := newEnd(t)
				reader := NewRTPPacketReader(NewRTPSessionWebrtc(cur.sess), CodecAudioUlaw)
				done := read(reader)
				next := replacement(t, cur)
				replace(t, reader, cur, next)

				// The read the move ended is taken up on next.
				next.peer.send(t, encryptFor(t, next.peer.sendSRTP, webrtcRTPPacket(1, "first")))
				require.NoError(t, readEnded(t, done, "the moved read did not return the packet"), "the move's own deadline ended the read")
				requireGoesOn(t, reader, next, "the move's own deadline stayed on the session")
			})

			t.Run("StoppedBeforeTheSwap", func(t *testing.T) {
				cur := newEnd(t)
				reader := NewRTPPacketReader(NewRTPSessionWebrtc(cur.sess), CodecAudioUlaw)
				require.NoError(t, cur.sess.StopRTP(1, 0))
				replace(t, reader, cur, replacement(t, cur))
				requireDeadline(t, read(reader))
			})

			t.Run("SwappedWhileWaiting", func(t *testing.T) {
				cur := newEnd(t)
				reader := NewRTPPacketReader(NewRTPSessionWebrtc(cur.sess), CodecAudioUlaw)
				require.NoError(t, cur.sess.StopRTP(1, 300*time.Millisecond))
				done := read(reader)
				replace(t, reader, cur, replacement(t, cur))
				requireDeadline(t, done)
			})

			t.Run("ClearedAfterTheSwap", func(t *testing.T) {
				cur := newEnd(t)
				reader := NewRTPPacketReader(NewRTPSessionWebrtc(cur.sess), CodecAudioUlaw)
				require.NoError(t, cur.sess.StopRTP(1, 0))
				next := replacement(t, cur)
				replace(t, reader, cur, next)
				requireDeadline(t, read(reader))

				// Cleared on the session in place, the reads go on.
				require.NoError(t, next.sess.StartRTP(1))
				requireGoesOn(t, reader, next, "the deadline was cleared")
			})
		})
	}
}
