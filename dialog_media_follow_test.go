// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

// followPeer is the far end of a dialog whose media follows its re-INVITEs: a
// plain UDP socket that sends RTP to the dialog and reads what it sends.
type followPeer struct {
	conn *net.UDPConn
	d    *DialogMedia
	seq  uint16
}

// teOffer is an offer from 127.0.0.1:port with PCMU and telephone-event at
// payload type te.
func teOffer(port int, te int, version int) []byte {
	return []byte(fmt.Sprintf("v=0\r\n"+
		"o=- 3948988145 %d IN IP4 127.0.0.1\r\n"+
		"s=Sip Go Media\r\n"+
		"c=IN IP4 127.0.0.1\r\n"+
		"t=0 0\r\n"+
		"m=audio %d RTP/AVP 0 %d\r\n"+
		"a=rtpmap:0 PCMU/8000\r\n"+
		"a=rtpmap:%d telephone-event/8000\r\n"+
		"a=fmtp:%d 0-16\r\n"+
		"a=ptime:20\r\n"+
		"a=sendrecv\r\n", version, port, te, te, te))
}

// newFollowPeer answers an offer with telephone-event at 101 from a peer
// socket, and installs the session in a dialog the way an answer does.
func newFollowPeer(t *testing.T) *followPeer {
	t.Helper()
	conn, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	sess := &media.MediaSession{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000},
		Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		Mode:   sdp.ModeSendrecv,
	}
	require.NoError(t, sess.Init())
	t.Cleanup(func() { _ = sess.Close() })
	require.NoError(t, sess.RemoteSDP(teOffer(conn.LocalAddr().(*net.UDPAddr).Port, 101, 1)))
	return &followPeer{conn: conn, d: newICEDialogMedia(t, sess)}
}

// reinvite applies a re-INVITE offer from the peer, with telephone-event at
// te, through the dialog's handler, and requires its 200.
func (p *followPeer) reinvite(t *testing.T, te int, version int) {
	t.Helper()
	tx := &fakeServerTransaction{}
	contact := &sip.ContactHeader{Address: sip.Uri{User: "us", Host: "127.0.0.1"}}
	body := teOffer(p.conn.LocalAddr().(*net.UDPAddr).Port, te, version)
	require.NoError(t, p.d.handleMediaUpdate(context.Background(), newReInvite(t, body), tx, contact))
	require.NotNil(t, tx.res)
	require.Equal(t, 200, tx.res.StatusCode)
}

// rebind moves the dialog's media to sockets of its own, as a re-INVITE whose
// answer moves our media does, and closes the ones it replaced.
func (p *followPeer) rebind(t *testing.T) {
	t.Helper()
	fork := p.d.MediaSession().Fork()
	fork.Laddr = net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}
	require.NoError(t, fork.Init())
	fork.RemoteSDPIsAnswer = false
	require.NoError(t, fork.RemoteSDP(teOffer(p.conn.LocalAddr().(*net.UDPAddr).Port, 101, 3)))
	p.d.mu.Lock()
	err := p.d.mediaUpdateUnsafe(fork)
	p.d.mu.Unlock()
	require.NoError(t, err)
}

// send sends one RTP packet to the dialog's current media address.
func (p *followPeer) send(t *testing.T, pt uint8, marker bool, timestamp uint32, payload []byte) {
	t.Helper()
	p.seq++
	raw, err := (&rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    pt,
			SequenceNumber: p.seq,
			Timestamp:      timestamp,
			SSRC:           0x7e1e7e1e,
			Marker:         marker,
		},
		Payload: payload,
	}).Marshal()
	require.NoError(t, err)
	laddr := p.d.MediaSession().Laddr
	_, err = p.conn.WriteTo(raw, &laddr)
	require.NoError(t, err)
}

// sendAudio sends one 20 ms PCMU frame.
func (p *followPeer) sendAudio(t *testing.T) {
	t.Helper()
	p.send(t, 0, false, uint32(p.seq)*160, make([]byte, 160))
}

// recv reads the next RTP packet the dialog sends the peer, bounded.
func (p *followPeer) recv(t *testing.T) rtp.Packet {
	t.Helper()
	buf := make([]byte, media.RTPBufSize)
	require.NoError(t, p.conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	n, _, err := p.conn.ReadFrom(buf)
	require.NoError(t, err, "the dialog sent nothing")
	pkt := rtp.Packet{}
	require.NoError(t, pkt.Unmarshal(buf[:n]))
	return pkt
}

// readWithin reads r once, and fails the test when the read does not return
// within d. A read left waiting is ended by the dialog's Close at cleanup.
func readWithin(t *testing.T, r io.Reader, d time.Duration) (int, error) {
	t.Helper()
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := r.Read(make([]byte, media.RTPBufSize))
		done <- result{n, err}
	}()
	select {
	case res := <-done:
		return res.n, res.err
	case <-time.After(d):
		t.Fatalf("the read did not return within %s", d)
		return 0, nil
	}
}

// enteringRTPReader closes entered when a read enters it, and reads on.
type enteringRTPReader struct {
	media.RTPReader
	entered chan struct{}
	once    sync.Once
}

func (r *enteringRTPReader) ReadRTP(buf []byte, p *rtp.Packet) (int, error) {
	r.once.Do(func() { close(r.entered) })
	return r.RTPReader.ReadRTP(buf, p)
}

// TestDialogMediaStatsFollowReinvite pins that the RTP statistics the
// AudioReader and AudioWriter options report are those of the dialog's
// current RTP session. A re-INVITE replaces that session with a fork, which
// carries the statistics on and counts what is read and written from then on.
// The options kept the session they were installed on, whose counts stopped at
// the re-INVITE.
func TestDialogMediaStatsFollowReinvite(t *testing.T) {
	p := newFollowPeer(t)

	var mu sync.Mutex
	var readCount, writeCount uint64
	ar, err := p.d.AudioReader(WithAudioReaderRTPStats(func(stats media.RTPReadStats) {
		mu.Lock()
		readCount = stats.PacketsCount
		mu.Unlock()
	}))
	require.NoError(t, err)
	aw, err := p.d.AudioWriter(WithAudioWriterRTPStats(func(stats media.RTPWriteStats) {
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

	p.sendAudio(t)
	_, err = readWithin(t, ar, 5*time.Second)
	require.NoError(t, err)
	_, err = aw.Write(make([]byte, 160))
	require.NoError(t, err)
	p.recv(t)
	read, written := counts()
	require.EqualValues(t, 1, read)
	require.EqualValues(t, 1, written)

	p.reinvite(t, 101, 2)

	p.sendAudio(t)
	_, err = readWithin(t, ar, 5*time.Second)
	require.NoError(t, err)
	_, err = aw.Write(make([]byte, 160))
	require.NoError(t, err)
	p.recv(t)
	read, written = counts()
	require.EqualValues(t, 2, read, "the read statistics stopped at the re-INVITE")
	require.EqualValues(t, 2, written, "the write statistics stopped at the re-INVITE")
}

// TestDialogMediaDTMFFollowsReinvite pins that DTMF read and written through a
// dialog's DTMF reader and writer is carried on the telephone-event payload
// type of the dialog's current media session. telephone-event is a dynamic
// format, and a later offer may carry it at another number and drop the old
// one; the answerer then must not send a format that is not in the offer (RFC
// 3264 section 8.3.2). The reader and writer kept the number of the session
// they were made on, so the reader took the events at the new number for
// audio and the writer sent events at the number the offer dropped.
func TestDialogMediaDTMFFollowsReinvite(t *testing.T) {
	for _, installed := range []bool{false, true} {
		t.Run(fmt.Sprintf("options=%t", installed), func(t *testing.T) {
			p := newFollowPeer(t)

			var r *DTMFReader
			var w *DTMFWriter
			if installed {
				r, w = &DTMFReader{}, &DTMFWriter{}
				_, err := p.d.AudioReader(WithAudioReaderDTMF(r))
				require.NoError(t, err)
				_, err = p.d.AudioWriter(WithAudioWriterDTMF(w))
				require.NoError(t, err)
			} else {
				var err error
				r, err = p.d.AudioReaderDTMF()
				require.NoError(t, err)
				w, err = p.d.AudioWriterDTMF()
				require.NoError(t, err)
			}
			digits := make(chan rune, 4)
			r.OnDTMF(func(dtmf rune) error {
				digits <- dtmf
				return nil
			})

			p.reinvite(t, 96, 2)

			// Digit 5 from the peer at its new number: a start with the
			// marker, then the end of the event.
			start := media.DTMFEncode(media.DTMFEvent{Event: 5, Volume: 10, Duration: 160})
			end := media.DTMFEncode(media.DTMFEvent{Event: 5, EndOfEvent: true, Volume: 10, Duration: 800})
			p.send(t, 96, true, 8000, start)
			p.send(t, 96, false, 8000, end)
			for i := 0; i < 2; i++ {
				_, err := readWithin(t, r, 5*time.Second)
				require.NoError(t, err)
			}
			select {
			case d := <-digits:
				require.Equal(t, '5', d)
			default:
				t.Fatal("the DTMF at the re-INVITE's telephone-event number was not read as DTMF")
			}

			require.NoError(t, w.WriteDTMF('7'))
			pkt := p.recv(t)
			require.EqualValues(t, 96, pkt.PayloadType, "the DTMF went out at the number of the session before the re-INVITE")
		})
	}
}

// TestDialogMediaReadDeadlines pins that the helpers that end a read with a
// read deadline leave the dialog's reads as they found them, and that the
// deadline follows the dialog's media when a re-INVITE moves it to new
// sockets. A deadline left set fails every later read of the dialog at once,
// and a deadline left on a socket the re-INVITE closed leaves the read
// carried to the new socket without one, waiting for a packet that may never
// come.
func TestDialogMediaReadDeadlines(t *testing.T) {
	// requireReadable requires a plain read of the dialog to get the next
	// packet the peer sends.
	requireReadable := func(t *testing.T, p *followPeer) {
		t.Helper()
		ar, err := p.d.AudioReader()
		require.NoError(t, err)
		p.sendAudio(t)
		_, err = readWithin(t, ar, 5*time.Second)
		require.NoError(t, err, "a read deadline was left set")
	}

	t.Run("DTMF Listen", func(t *testing.T) {
		p := newFollowPeer(t)
		r, err := p.d.AudioReaderDTMF()
		require.NoError(t, err)
		require.NoError(t, r.Listen(func(rune) error { return nil }, 50*time.Millisecond))
		requireReadable(t, p)
	})

	t.Run("ListenContext", func(t *testing.T) {
		p := newFollowPeer(t)
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		require.NoError(t, p.d.ListenContext(ctx))
		requireReadable(t, p)
	})

	t.Run("ListenUntil", func(t *testing.T) {
		p := newFollowPeer(t)
		require.ErrorIs(t, p.d.ListenUntil(50*time.Millisecond), os.ErrDeadlineExceeded)
		requireReadable(t, p)
	})

	t.Run("DTMF Listen across a rebind", func(t *testing.T) {
		p := newFollowPeer(t)
		r, err := p.d.AudioReaderDTMF()
		require.NoError(t, err)

		// The first Listen ends with its deadline; the rebind comes between
		// it and the next one, and while the next one reads.
		require.NoError(t, r.Listen(func(rune) error { return nil }, 50*time.Millisecond))
		p.rebind(t)

		reading := make(chan struct{})
		p.d.RTPPacketReader.UpdateReader(&enteringRTPReader{RTPReader: p.d.RTPSession(), entered: reading})
		done := make(chan error, 1)
		go func() { done <- r.Listen(func(rune) error { return nil }, 300*time.Millisecond) }()
		select {
		case <-reading:
		case <-time.After(5 * time.Second):
			t.Fatal("Listen did not read")
		}
		p.rebind(t)
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("Listen did not end with its deadline after the re-INVITE moved the media")
		}
		requireReadable(t, p)
	})
}

// TestDialogMediaRingtoneKeepsReads pins that a ringtone playing in the
// background changes only the write deadline it stops its writes with, and
// restores it: a read deadline another helper set, as a listener waiting for
// DTMF with a timeout does, still ends that read. It also pins that stopping
// the ringtone between two rings is a clean stop, as stopping it during one
// is.
func TestDialogMediaRingtoneKeepsReads(t *testing.T) {
	p := newFollowPeer(t)
	ringtone, err := p.d.PlaybackRingtoneCreate()
	require.NoError(t, err)
	// The ring goes out at once, so the ringtone is between rings as soon as
	// the peer has it.
	p.d.RTPPacketWriter.ClockDisable()

	ar, err := p.d.AudioReader()
	require.NoError(t, err)
	require.NoError(t, p.d.StopRTP(1, 300*time.Millisecond))

	stop, err := ringtone.PlayBackground()
	require.NoError(t, err)

	_, err = readWithin(t, ar, 5*time.Second)
	require.ErrorIs(t, err, os.ErrDeadlineExceeded, "the ringtone cleared the read deadline")
	require.NoError(t, p.d.StartRTP(1, 0))

	// A ring is 2 s of PCMU in 20 ms frames.
	for i := 0; i < 100; i++ {
		p.recv(t)
	}
	require.NoError(t, stop(), "a stop between rings is a clean stop")
	requireSendsAudio(t, p)
}

// requireSendsAudio requires the dialog's writes to go out, so no write
// deadline was left set.
func requireSendsAudio(t *testing.T, p *followPeer) {
	t.Helper()
	aw, err := p.d.AudioWriter()
	require.NoError(t, err)
	_, err = aw.Write(make([]byte, 160))
	require.NoError(t, err, "a write deadline was left set")
	p.recv(t)
}

// TestDialogMediaAudioHelpersReadMediaUnderLock pins that the DTMF reader and
// writer helpers take the dialog's media session under its lock, since a
// re-INVITE swaps it for a fork under that lock. The race only shows under
// -race.
func TestDialogMediaAudioHelpersReadMediaUnderLock(t *testing.T) {
	swapWhile := func(t *testing.T, d *DialogMedia, f func() error) {
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
		require.NoError(t, f())
		select {
		case <-swapped:
		case <-time.After(5 * time.Second):
			t.Fatal("the session was never swapped")
		}
	}

	t.Run("AudioReaderDTMF", func(t *testing.T) {
		p := newFollowPeer(t)
		swapWhile(t, p.d, func() error {
			_, err := p.d.AudioReaderDTMF()
			return err
		})
	})
	t.Run("AudioWriterDTMF", func(t *testing.T) {
		p := newFollowPeer(t)
		swapWhile(t, p.d, func() error {
			_, err := p.d.AudioWriterDTMF()
			return err
		})
	})
}

// TestDialogMediaRingtoneFollowsRebind pins that a ringtone made before a
// re-INVITE moved the dialog's media to new sockets plays and stops on the
// dialog's current media. It kept the media session it was made on, whose
// sockets the re-INVITE closed, so starting and stopping it failed there.
func TestDialogMediaRingtoneFollowsRebind(t *testing.T) {
	p := newFollowPeer(t)
	ringtone, err := p.d.PlaybackRingtoneCreate()
	require.NoError(t, err)

	p.rebind(t)

	stop, err := ringtone.PlayBackground()
	require.NoError(t, err)
	p.recv(t)
	require.NoError(t, stop())
	requireSendsAudio(t, p)
}
