// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"os"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emiago/diago/media"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// telephoneEventAt is telephone-event/8000 at payload type pt.
func telephoneEventAt(pt uint8) media.Codec {
	c := media.CodecTelephoneEvent8000
	c.PayloadType = pt
	return c
}

// webrtcHandlePeer is the far end of a DialogWebrtc: a WebRTC media session
// of its own on the loopback interface that sends the dialog RTP and reads
// what the dialog sends.
type webrtcHandlePeer struct {
	sess  *media.MediaSessionWebrtc
	laddr string
	seq   uint16
}

// newLoopbackWebrtcSession returns a WebRTC media session with codecs on the
// loopback interface, closed with the test.
func newLoopbackWebrtcSession(t *testing.T, codecs []media.Codec) *media.MediaSessionWebrtc {
	t.Helper()
	conf, err := prepareWebrtcConfig(loopbackWebrtcConfig())
	require.NoError(t, err)
	sess := &media.MediaSessionWebrtc{Codecs: codecs}
	require.NoError(t, sess.Init(t.Context(), conf))
	t.Cleanup(func() { assert.NoError(t, sess.Close()) })
	return sess
}

// finalizeWebrtcBoth runs the two finalizes at once, as each waits for the
// other side, and requires both to complete.
func finalizeWebrtcBoth(t *testing.T, a, b func(context.Context) error) {
	t.Helper()
	errs := make(chan error, 2)
	go func() { errs <- a(t.Context()) }()
	go func() { errs <- b(t.Context()) }()
	for range 2 {
		select {
		case err := <-errs:
			require.NoError(t, err)
		case <-time.After(30 * time.Second):
			t.Fatal("the ICE and DTLS negotiation did not complete")
		}
	}
}

// newWebrtcHandleCall negotiates a peer offering peerCodecs with a dialog
// whose media has PCMU and telephone-event at 101, and sets the media up in a
// DialogWebrtc the way an answer does. Both end with the test.
func newWebrtcHandleCall(t *testing.T, peerCodecs []media.Codec) (*DialogWebrtc, *webrtcHandlePeer) {
	t.Helper()
	ctx := t.Context()
	local := newLoopbackWebrtcSession(t, []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000})
	peer := &webrtcHandlePeer{sess: newLoopbackWebrtcSession(t, peerCodecs)}

	offer, err := peer.sess.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, local.RemoteSDP(ctx, offer, false))
	answer, err := local.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, peer.sess.RemoteSDP(ctx, answer, true))
	finalizeWebrtcBoth(t, local.Finalize, peer.sess.Finalize)
	peer.laddr = peer.sess.Laddr

	d := &DialogWebrtc{}
	rtpSess := media.NewRTPSessionWebrtc(local)
	d.Init(local, rtpSess)
	require.NoError(t, rtpSess.MonitorBackground())
	t.Cleanup(func() { assert.NoError(t, d.Close()) })
	return d, peer
}

// send sends one RTP packet to the dialog.
func (p *webrtcHandlePeer) send(t *testing.T, pt uint8, marker bool, timestamp uint32, payload []byte) {
	t.Helper()
	p.seq++
	require.NoError(t, p.sess.WriteRTP(&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    pt,
			SequenceNumber: p.seq,
			Timestamp:      timestamp,
			SSRC:           0x7e1e7e1e,
			Marker:         marker,
		},
		Payload: payload,
	}))
}

// sendAudio sends one 20 ms PCMU frame.
func (p *webrtcHandlePeer) sendAudio(t *testing.T) {
	t.Helper()
	p.send(t, 0, false, uint32(p.seq)*160, make([]byte, 160))
}

// recv reads the next RTP packet the dialog sends the peer, bounded.
func (p *webrtcHandlePeer) recv(t *testing.T) rtp.Packet {
	t.Helper()
	require.NoError(t, p.sess.StopRTP(1, 5*time.Second))
	pkt := rtp.Packet{}
	_, err := p.sess.ReadRTP(make([]byte, media.RTPBufSize), &pkt)
	require.NoError(t, err, "the dialog sent nothing")
	require.NoError(t, p.sess.StartRTP(1))
	return pkt
}

// stageNewTransport has the dialog take an offer of the peer's from a fork
// of its session with codecs, or the session's own when codecs is nil, which
// asks for a new ICE generation and so a new transport. It returns the fork,
// which the dialog has answered and staged, and the answer.
func (p *webrtcHandlePeer) stageNewTransport(t *testing.T, d *DialogWebrtc, codecs []media.Codec) (*media.MediaSessionWebrtc, []byte) {
	t.Helper()
	ctx := t.Context()
	fork, err := p.sess.Fork(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, fork.Close()) })
	if codecs != nil {
		fork.Codecs = codecs
	}
	offer, err := fork.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, d.onRemoteSDP(ctx, offer, false))
	answer, err := d.onLocalSDP(ctx, true, "")
	require.NoError(t, err)
	return fork, answer
}

// reinviteNewTransport moves the call to a new transport, as a re-INVITE of
// the peer's that restarts ICE does: the offer, our answer, and the commit
// once the answer is out, which negotiates the new transport.
func (p *webrtcHandlePeer) reinviteNewTransport(t *testing.T, d *DialogWebrtc, codecs []media.Codec) {
	t.Helper()
	fork, answer := p.stageNewTransport(t, d, codecs)
	require.NoError(t, fork.RemoteSDP(t.Context(), answer, true))
	finalizeWebrtcBoth(t, d.onFinalize, fork.Finalize)
	p.sess = fork
}

// reinviteInPlace applies a re-INVITE of the peer's that changes no ICE
// credentials, which the dialog applies to the transport in place.
func (p *webrtcHandlePeer) reinviteInPlace(t *testing.T, d *DialogWebrtc) {
	t.Helper()
	ctx := t.Context()
	offer, err := p.sess.ReOffer("")
	require.NoError(t, err)
	require.NoError(t, d.onRemoteSDP(ctx, offer, false))
	answer, err := d.onLocalSDP(ctx, true, "")
	require.NoError(t, err)
	require.NoError(t, p.sess.RemoteSDP(ctx, answer, true))
	require.NoError(t, p.sess.Finalize(ctx))
	require.NoError(t, d.onFinalize(ctx))
}

// TestDialogWebrtcDTMFWithoutTelephoneEvent pins that a WebRTC call whose
// peer negotiated no telephone-event still gets a DTMF reader and writer, on
// the default payload type 101, as a DialogMedia does, and that the reader
// takes the events the peer sends there for DTMF.
func TestDialogWebrtcDTMFWithoutTelephoneEvent(t *testing.T) {
	readers := map[string]func(d *DialogWebrtc) (*DTMFReader, error){
		"AudioReaderDTMF": func(d *DialogWebrtc) (*DTMFReader, error) { return d.AudioReaderDTMF() },
		"WithAudioReaderWebrtcDTMF": func(d *DialogWebrtc) (*DTMFReader, error) {
			r := &DTMFReader{}
			_, err := d.AudioReader(WithAudioReaderWebrtcDTMF(r))
			return r, err
		},
	}
	for name, newReader := range readers {
		t.Run(name, func(t *testing.T) {
			d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
			_, ok := media.CodecTelephoneEventFromList(d.MediaSession().CommonCodecs())
			require.False(t, ok, "telephone-event must not be negotiated for this fixture")

			r, err := newReader(d)
			require.NoError(t, err)
			digits := make(chan rune, 1)
			r.OnDTMF(func(dtmf rune) error {
				digits <- dtmf
				return nil
			})
			p.sendAudio(t)
			_, err = readWithin(t, r, 5*time.Second)
			require.NoError(t, err)
			p.send(t, 101, true, 8000, media.DTMFEncode(media.DTMFEvent{Event: 1, Volume: 10, Duration: 160}))
			p.send(t, 101, false, 8000, media.DTMFEncode(media.DTMFEvent{Event: 1, EndOfEvent: true, Volume: 10, Duration: 800}))
			for range 2 {
				_, err := readWithin(t, r, 5*time.Second)
				require.NoError(t, err)
			}
			select {
			case got := <-digits:
				require.Equal(t, '1', got)
			default:
				t.Fatal("the DTMF at 101 was not read as DTMF")
			}

			// The writer is set up on 101 too. The WebRTC RTP session sends
			// only the formats negotiated (RFC 3264 section 6.1), so a key
			// written on it is refused there rather than sent.
			w, err := d.AudioWriterDTMF()
			require.NoError(t, err)
			require.Error(t, w.WriteDTMF('7'))
		})
	}
}

// TestDialogWebrtcDTMFFollowsNewTransport pins that the DTMF reader and
// writer of a WebRTC call move to the telephone-event number of the media a
// re-INVITE installs. A later offer may carry telephone-event at another
// number and drop the old one, which the answerer then must not send (RFC
// 3264 section 8.3.2).
func TestDialogWebrtcDTMFFollowsNewTransport(t *testing.T) {
	d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw, telephoneEventAt(110)})
	r, err := d.AudioReaderDTMF()
	require.NoError(t, err)
	w, err := d.AudioWriterDTMF()
	require.NoError(t, err)
	digits := make(chan rune, 4)
	r.OnDTMF(func(dtmf rune) error {
		digits <- dtmf
		return nil
	})

	p.reinviteNewTransport(t, d, []media.Codec{media.CodecAudioUlaw, telephoneEventAt(111)})

	p.sendAudio(t)
	_, err = readWithin(t, r, 5*time.Second)
	require.NoError(t, err)
	p.send(t, 111, true, 8000, media.DTMFEncode(media.DTMFEvent{Event: 5, Volume: 10, Duration: 160}))
	p.send(t, 111, false, 8000, media.DTMFEncode(media.DTMFEvent{Event: 5, EndOfEvent: true, Volume: 10, Duration: 800}))
	for range 2 {
		_, err := readWithin(t, r, 5*time.Second)
		require.NoError(t, err)
	}
	select {
	case got := <-digits:
		require.Equal(t, '5', got)
	default:
		t.Fatal("the DTMF at the re-INVITE's telephone-event number was not read as DTMF")
	}

	require.NoError(t, w.WriteDTMF('7'))
	require.EqualValues(t, 111, p.recv(t).PayloadType, "the DTMF went out at the number of the media before the re-INVITE")
}

// TestDialogWebrtcStatsFollowNewTransport pins that the RTP statistics the
// WebRTC AudioReader and AudioWriter options report are those of the call's
// current RTP session, which a new transport replaces with one that carries
// the statistics on.
func TestDialogWebrtcStatsFollowNewTransport(t *testing.T) {
	d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw, telephoneEventAt(110)})

	var mu sync.Mutex
	var readCount, writeCount uint64
	ar, err := d.AudioReader(WithAudioReaderWebrtcRTPStats(func(stats media.RTPReadStats) {
		mu.Lock()
		readCount = stats.PacketsCount
		mu.Unlock()
	}))
	require.NoError(t, err)
	aw, err := d.AudioWriter(WithAudioWriterWebrtcRTPStats(func(stats media.RTPWriteStats) {
		mu.Lock()
		writeCount = stats.PacketsCount
		mu.Unlock()
	}))
	require.NoError(t, err)
	counts := func() (uint64, uint64) {
		mu.Lock()
		defer mu.Unlock()
		return readCount, writeCount
	}

	exchange := func() {
		p.sendAudio(t)
		_, err := readWithin(t, ar, 5*time.Second)
		require.NoError(t, err)
		_, err = aw.Write(make([]byte, 160))
		require.NoError(t, err)
		p.recv(t)
	}
	exchange()
	read, written := counts()
	require.EqualValues(t, 1, read)
	require.EqualValues(t, 1, written)

	p.reinviteNewTransport(t, d, nil)

	exchange()
	read, written = counts()
	require.EqualValues(t, 2, read, "the read statistics stopped at the new transport")
	require.EqualValues(t, 2, written, "the write statistics stopped at the new transport")
}

// TestDialogWebrtcReadDeadlinesAcrossNewTransport pins that the read
// deadline StopRTP sets follows the call's media when a re-INVITE moves it to
// a new transport: a read stopped before stays stopped, and a read waiting
// on the old transport with a deadline still ends at that deadline once it is
// carried to the new one, although the peer sends nothing.
func TestDialogWebrtcReadDeadlinesAcrossNewTransport(t *testing.T) {
	requireReadable := func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer) {
		t.Helper()
		ar, err := d.AudioReader()
		require.NoError(t, err)
		p.sendAudio(t)
		_, err = readWithin(t, ar, 5*time.Second)
		require.NoError(t, err, "a read deadline was left set")
	}

	t.Run("StoppedBefore", func(t *testing.T) {
		d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
		require.NoError(t, d.StopRTP(1, 0))
		p.reinviteNewTransport(t, d, nil)

		ar, err := d.AudioReader()
		require.NoError(t, err)
		_, err = readWithin(t, ar, 5*time.Second)
		require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the new transport dropped the stop")

		require.NoError(t, d.StartRTP(1, 0))
		requireReadable(t, d, p)
	})

	t.Run("ReadWaitingAcross", func(t *testing.T) {
		d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
		ar, err := d.AudioReader()
		require.NoError(t, err)
		// Far enough out that the new transport is in place before it passes.
		require.NoError(t, d.StopRTP(1, 8*time.Second))
		reading := make(chan error, 1)
		go func() {
			_, err := ar.Read(make([]byte, media.RTPBufSize))
			reading <- err
		}()
		p.reinviteNewTransport(t, d, nil)

		select {
		case err := <-reading:
			require.ErrorIs(t, err, os.ErrDeadlineExceeded)
		case <-time.After(15 * time.Second):
			t.Fatal("the read did not end with its deadline after the new transport was installed")
		}
		require.NoError(t, d.StartRTP(1, 0))
		requireReadable(t, d, p)
	})
}

// TestDialogWebrtcMediaUpdates pins that every media update a WebRTC call
// installs runs the media update hooks once and then OnMediaUpdate, with the
// update in place and the media's lock released, and that an update only
// staged or aborted runs none.
func TestDialogWebrtcMediaUpdates(t *testing.T) {
	type observed struct {
		mu      sync.Mutex
		hooks   []*media.MediaSessionWebrtc
		updates []*media.MediaSessionWebrtc
	}
	watch := func(t *testing.T, d *DialogWebrtc) *observed {
		t.Helper()
		o := &observed{}
		// Both read the session through the media's lock, which a callback
		// run with the lock held would wait on forever.
		remove := d.addMediaUpdateHook(func() {
			sess := d.MediaSession()
			o.mu.Lock()
			o.hooks = append(o.hooks, sess)
			o.mu.Unlock()
		})
		t.Cleanup(remove)
		d.addMediaUpdateHook(func() { t.Error("a removed hook ran") })()
		d.OnMediaUpdate(func(m *DialogWebrtc) {
			sess := m.MediaSession()
			o.mu.Lock()
			defer o.mu.Unlock()
			if len(o.updates) == len(o.hooks) {
				t.Error("OnMediaUpdate ran before the media update hooks")
			}
			o.updates = append(o.updates, sess)
		})
		return o
	}
	seen := func(o *observed) ([]*media.MediaSessionWebrtc, []*media.MediaSessionWebrtc) {
		o.mu.Lock()
		defer o.mu.Unlock()
		return o.hooks, o.updates
	}

	t.Run("NewTransport", func(t *testing.T) {
		d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
		before := d.MediaSession()
		o := watch(t, d)

		fork, answer := p.stageNewTransport(t, d, nil)
		hooks, updates := seen(o)
		require.Empty(t, hooks, "a staged transport ran the hooks")
		require.Empty(t, updates, "a staged transport ran OnMediaUpdate")

		require.NoError(t, fork.RemoteSDP(t.Context(), answer, true))
		finalizeWebrtcBoth(t, d.onFinalize, fork.Finalize)
		hooks, updates = seen(o)
		require.Len(t, hooks, 1, "the install must run the hooks once")
		require.Len(t, updates, 1, "the install must run OnMediaUpdate once")
		assert.NotSame(t, before, hooks[0], "the hook ran before the new transport was in place")
		assert.Same(t, d.MediaSession(), hooks[0])
		assert.Same(t, d.MediaSession(), updates[0])
	})

	t.Run("InPlace", func(t *testing.T) {
		d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
		before := d.MediaSession()
		o := watch(t, d)

		p.reinviteInPlace(t, d)
		hooks, updates := seen(o)
		require.Len(t, hooks, 1, "the install must run the hooks once")
		require.Len(t, updates, 1, "the install must run OnMediaUpdate once")
		assert.Same(t, before, hooks[0], "an update in place changed the transport")
	})

	t.Run("Aborted", func(t *testing.T) {
		d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
		o := watch(t, d)

		p.stageNewTransport(t, d, nil)
		d.abortPendingMediaSession()
		hooks, updates := seen(o)
		require.Empty(t, hooks, "an aborted update ran the hooks")
		require.Empty(t, updates, "an aborted update ran OnMediaUpdate")
	})
}

// TestDialogWebrtcClosedRefusesExchange pins that the media of a WebRTC call
// that is closed refuses an offer/answer exchange with errMediaClosed, as a
// DialogMedia does: nothing would close what the exchange set up.
func TestDialogWebrtcClosedRefusesExchange(t *testing.T) {
	d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
	offer, err := p.sess.ReOffer("")
	require.NoError(t, err)
	require.NoError(t, d.Close())

	require.ErrorIs(t, d.onRemoteSDP(t.Context(), offer, false), errMediaClosed)
	_, err = d.onLocalSDP(t.Context(), false, "")
	require.ErrorIs(t, err, errMediaClosed)
}

// TestDialogWebrtcBeforeSetup pins that the helpers of a WebRTC call asked
// for before its media is set up return ErrNoMediaSetup, as those of a
// DialogMedia do.
func TestDialogWebrtcBeforeSetup(t *testing.T) {
	d := &DialogWebrtc{}
	_, err := d.AudioReader(WithAudioReaderWebrtcRTPStats(func(media.RTPReadStats) {}))
	require.ErrorIs(t, err, ErrNoMediaSetup)
	_, err = d.AudioWriter(WithAudioWriterWebrtcRTPStats(func(media.RTPWriteStats) {}))
	require.ErrorIs(t, err, ErrNoMediaSetup)
	_, err = d.PlaybackCreate()
	require.ErrorIs(t, err, ErrNoMediaSetup)
	_, err = d.PlaybackControlCreate()
	require.ErrorIs(t, err, ErrNoMediaSetup)
	_, err = d.PlaybackRingtoneCreate()
	require.ErrorIs(t, err, ErrNoMediaSetup)
	require.ErrorIs(t, d.Listen(), ErrNoMediaSetup)
	_, err = d.ListenBackground()
	require.ErrorIs(t, err, ErrNoMediaSetup)
	require.ErrorIs(t, d.ListenContext(t.Context()), ErrNoMediaSetup)
	require.ErrorIs(t, d.ListenUntil(time.Millisecond), ErrNoMediaSetup)
}

// TestDialogWebrtcSecondCloseWaits pins that a Close of a WebRTC call made
// while an earlier Close is still releasing the media returns only after that
// release has run, as a DialogMedia's does.
func TestDialogWebrtcSecondCloseWaits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d := &DialogWebrtc{}
		inRelease := make(chan struct{})
		unblock := make(chan struct{})
		unblockOnce := sync.OnceFunc(func() { close(unblock) })
		defer unblockOnce() // a failing run must not leave the first Close blocked
		released := false
		d.OnClose(func() error {
			close(inRelease)
			<-unblock
			released = true
			return nil
		})

		first := make(chan error, 1)
		go func() { first <- d.Close() }()
		<-inRelease

		// released is written before the first Close ends and is read only
		// after the second Close has returned, so the read is ordered after
		// the write.
		second := make(chan bool, 1)
		go func() {
			_ = d.Close()
			second <- released
		}()
		synctest.Wait()
		select {
		case <-second:
			t.Fatal("second Close returned while the first was still releasing the media")
		default:
		}

		unblockOnce()
		require.NoError(t, <-first)
		require.True(t, <-second, "second Close returned before the media was released")
	})
}
