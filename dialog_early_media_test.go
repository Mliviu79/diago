// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAnsweringDTLSMedia returns DTLS-SRTP dialog media that has taken the
// offer of peer and described its answer, as ProgressMedia has it when its 183
// is out, with that answer.
func newAnsweringDTLSMedia(t *testing.T, peer *media.MediaSession) (*DialogMedia, []byte) {
	t.Helper()
	med := &DialogMedia{}
	med.InitMediaSession(newDTLSMediaSession(t, testdata.ServerCertificate()), nil, nil)
	t.Cleanup(func() { _ = med.Close() })
	require.NoError(t, med.setupRTPSession(peer.LocalSDP(false), media.NewRTPSession(med.mediaSession)))
	return med, med.mediaSession.LocalSDP(true)
}

// TestDialogMediaEarlyKeyingLeftRunning pins what the media does with a
// handshake the peer has not completed within the wait: it reports it in
// progress and leaves it running, has no RTP session, reader or writer
// meanwhile, and sets them up and starts the RTCP monitor once onFinalize has
// waited for the keys.
func TestDialogMediaEarlyKeyingLeftRunning(t *testing.T) {
	peer := newDTLSMediaSession(t, testdata.ClientCertificate())
	med, answer := newAnsweringDTLSMedia(t, peer)
	require.NotNil(t, med.RTPSession())

	require.ErrorIs(t, med.onFinalizeWithin(t.Context(), 100*time.Millisecond), errMediaKeyingInProgress)
	assert.Nil(t, med.RTPSession(), "media that is not keyed has no RTP session")
	_, err := med.AudioReader()
	assert.ErrorIs(t, err, ErrNoMediaSetup)
	_, err = med.AudioWriter()
	assert.ErrorIs(t, err, ErrNoMediaSetup)
	assert.ErrorIs(t, med.MediaSession().WriteRTP(&rtp.Packet{Header: rtp.Header{Version: 2}}), media.ErrDTLSNotKeyed)
	// Asked again without waiting, it still runs.
	require.ErrorIs(t, med.onFinalizeWithin(t.Context(), 0), errMediaKeyingInProgress)

	// The peer takes part only now.
	require.NoError(t, peer.RemoteSDP(answer, true))
	peerDone := make(chan error, 1)
	go func() { peerDone <- peer.Finalize() }()
	require.NoError(t, med.onFinalize(t.Context()))
	select {
	case err := <-peerDone:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the peer's handshake did not return")
	}

	rtpSess := med.RTPSession()
	require.NotNil(t, rtpSess, "the commit did not set the media up")
	assert.Error(t, rtpSess.MonitorBackground(), "the commit did not start the RTCP monitor")
	_, err = med.AudioWriter()
	assert.NoError(t, err)
	requireEncryptedMedia(t, med, &DialogMedia{mediaSession: peer}, 100)

	// A second commit sets nothing up again.
	require.NoError(t, med.onFinalize(t.Context()))
	assert.Same(t, rtpSess, med.RTPSession())
}

// TestDialogMediaEarlyKeyingKeyed pins that media the peer keys within the
// wait keeps the RTP session it was set up with, and that media which needs
// no keys is reported keyed at once.
func TestDialogMediaEarlyKeyingKeyed(t *testing.T) {
	t.Run("dtls", func(t *testing.T) {
		peer := newDTLSMediaSession(t, testdata.ClientCertificate())
		med, answer := newAnsweringDTLSMedia(t, peer)
		rtpSess := med.RTPSession()

		require.NoError(t, peer.RemoteSDP(answer, true))
		peerDone := make(chan error, 1)
		go func() { peerDone <- peer.Finalize() }()
		require.NoError(t, med.onFinalizeWithin(t.Context(), 10*time.Second))
		select {
		case err := <-peerDone:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("the peer's handshake did not return")
		}
		assert.Same(t, rtpSess, med.RTPSession())
		// Whoever waited for the keys starts the monitor, and the commit
		// that follows the answer starts none beside it.
		require.NoError(t, rtpSess.MonitorBackground())
		require.NoError(t, med.onFinalize(t.Context()))
		assert.Same(t, rtpSess, med.RTPSession())
	})

	t.Run("no_keys_needed", func(t *testing.T) {
		peer := newMediaSessionForTest(t)
		med := &DialogMedia{}
		med.InitMediaSession(newMediaSessionForTest(t), nil, nil)
		t.Cleanup(func() { _ = med.Close() })
		require.NoError(t, med.setupRTPSession(peer.LocalSDP(false), media.NewRTPSession(med.mediaSession)))
		rtpSess := med.RTPSession()

		require.NoError(t, med.onFinalizeWithin(t.Context(), time.Minute))
		assert.Same(t, rtpSess, med.RTPSession())
	})
}

// TestDialogMediaEarlyKeyingRefused pins the classes the media answers the
// keying with when it cannot run it.
func TestDialogMediaEarlyKeyingRefused(t *testing.T) {
	t.Run("closed", func(t *testing.T) {
		peer := newDTLSMediaSession(t, testdata.ClientCertificate())
		med, _ := newAnsweringDTLSMedia(t, peer)
		require.NoError(t, med.Close())
		assert.ErrorIs(t, med.onFinalizeWithin(t.Context(), time.Minute), errMediaClosed)
	})

	t.Run("no_media_session", func(t *testing.T) {
		med := &DialogMedia{}
		assert.ErrorIs(t, med.onFinalizeWithin(t.Context(), time.Minute), errNoMediaSession)
	})

	// Closing the media ends the handshake left running, and the commit that
	// follows reports it.
	t.Run("closed_while_left_running", func(t *testing.T) {
		peer := newDTLSMediaSession(t, testdata.ClientCertificate())
		med, _ := newAnsweringDTLSMedia(t, peer)
		require.ErrorIs(t, med.onFinalizeWithin(t.Context(), 50*time.Millisecond), errMediaKeyingInProgress)
		require.NoError(t, med.Close())
		require.Error(t, med.onFinalize(t.Context()))
		assert.Nil(t, med.RTPSession(), "closed media is not set up")
	})
}

// scriptedKeyer is media whose keying a test scripts: each call takes the next
// result, and the last one again once they are used up.
type scriptedKeyer struct {
	results []error
	waits   []time.Duration
}

func (k *scriptedKeyer) onFinalizeWithin(_ context.Context, wait time.Duration) error {
	k.waits = append(k.waits, wait)
	i := min(len(k.waits), len(k.results)) - 1
	return k.results[i]
}

// TestDialogServerKeyEarlyMedia pins what the session makes of each result of
// the keying: keyed, left running, failed.
func TestDialogServerKeyEarlyMedia(t *testing.T) {
	answer := []byte("v=0\r\n")
	failed := errors.New("handshake failed")

	t.Run("keyed", func(t *testing.T) {
		d := newTestDialogOver(t, newByeServerTx())
		keyer := &scriptedKeyer{results: []error{nil}}
		require.NoError(t, d.keyEarlyMedia(keyer, answer, 0))
		assert.Equal(t, []time.Duration{defaultEarlyMediaKeyTimeout}, keyer.waits)
		early, ok := d.earlyAnswer()
		require.True(t, ok)
		assert.Equal(t, earlyAnswer{sdp: answer}, early)
	})

	t.Run("left_running", func(t *testing.T) {
		d := newTestDialogOver(t, newByeServerTx())
		keyer := &scriptedKeyer{results: []error{errMediaKeyingInProgress}}
		require.ErrorIs(t, d.keyEarlyMedia(keyer, answer, time.Second), ErrEarlyMediaNotKeyed)
		assert.Equal(t, []time.Duration{time.Second}, keyer.waits)
		early, ok := d.earlyAnswer()
		require.True(t, ok)
		assert.Equal(t, earlyAnswer{sdp: answer, unkeyed: true}, early)
	})

	t.Run("failed", func(t *testing.T) {
		d := newTestDialogOver(t, newByeServerTx())
		keyer := &scriptedKeyer{results: []error{failed}}
		require.ErrorIs(t, d.keyEarlyMedia(keyer, answer, time.Second), failed)
		_, ok := d.earlyAnswer()
		assert.False(t, ok, "media that failed to key left no answer to repeat")
	})
}

// TestDialogServerAnswerEarlyWithin pins the answer the session gives a call
// with early media, over scripted media: the 183's bytes in a 200, without
// asking keyed media anything, and a 488 once the handshake left running has
// failed.
func TestDialogServerAnswerEarlyWithin(t *testing.T) {
	answer := []byte("v=0\r\ns=early\r\n")
	failed := errors.New("handshake failed")
	cases := []struct {
		name string
		// early is what the 183 left, and keying what the media says when it
		// is asked how its handshake stands.
		early  earlyAnswer
		keying error
		// wantAsked is whether the media is asked, and wantStatus what the
		// INVITE is answered with.
		wantAsked  bool
		wantStatus int
	}{
		{name: "keyed", early: earlyAnswer{sdp: answer}, keying: failed, wantStatus: sip.StatusOK},
		{name: "left_running", early: earlyAnswer{sdp: answer, unkeyed: true}, keying: errMediaKeyingInProgress, wantAsked: true, wantStatus: sip.StatusOK},
		{name: "keyed_meanwhile", early: earlyAnswer{sdp: answer, unkeyed: true}, wantAsked: true, wantStatus: sip.StatusOK},
		{name: "failed", early: earlyAnswer{sdp: answer, unkeyed: true}, keying: failed, wantAsked: true, wantStatus: sip.StatusNotAcceptableHere},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inviteTx := &respondedServerTx{byeServerTx: newByeServerTx(), responded: make(chan *sip.Response, 4)}
			d := newTestDialogOver(t, inviteTx)
			t.Cleanup(func() { _ = d.Close() })
			keyer := &scriptedKeyer{results: []error{tc.keying}}

			answered := make(chan error, 1)
			go func() { answered <- d.answerEarlyWithin(t.Context(), keyer, tc.early) }()
			var res *sip.Response
			select {
			case res = <-inviteTx.responded:
			case <-time.After(5 * time.Second):
				t.Fatal("the INVITE was not answered")
			}
			require.Equal(t, tc.wantStatus, res.StatusCode)

			if tc.wantStatus != sip.StatusOK {
				assert.Empty(t, res.Body(), "a refusal carries no answer")
				// The transaction ends, as the ACK to the refusal ends it.
				close(inviteTx.done)
				select {
				case err := <-answered:
					assert.ErrorIs(t, err, failed, "the answer says why the call was declined")
				case <-time.After(5 * time.Second):
					t.Fatal("the answer did not return")
				}
			} else {
				assert.Equal(t, string(answer), string(res.Body()))
				readAck(t, d)
				select {
				case err := <-answered:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("the answer did not return")
				}
			}
			// Read once the answer has returned: it asks on its own goroutine.
			if tc.wantAsked {
				assert.Equal(t, []time.Duration{0}, keyer.waits, "the media is asked once, without waiting")
			} else {
				assert.Empty(t, keyer.waits, "keyed media is asked nothing")
			}
		})
	}
}

// offerToSocket returns the offer of peer with the port of its m= line
// replaced by port.
func offerToSocket(t *testing.T, peer *media.MediaSession, port int) []byte {
	t.Helper()
	offer := peer.LocalSDP(false)
	line := sdpLineWithPrefix(t, offer, "m=audio ")
	fields := strings.Fields(line)
	require.GreaterOrEqual(t, len(fields), 2)
	fields[1] = strconv.Itoa(port)
	return bytes.Replace(offer, []byte(line), []byte(strings.Join(fields, " ")), 1)
}

// listenRTPPair binds a UDP socket on loopback and the one at the next port,
// where a session sends the RTCP of the RTP it sends to the first.
func listenRTPPair(t *testing.T) (rtpConn, rtcpConn *net.UDPConn) {
	t.Helper()
	for range 20 {
		rtpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		require.NoError(t, err)
		port := rtpConn.LocalAddr().(*net.UDPAddr).Port
		rtcpConn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port + 1})
		if err != nil {
			require.NoError(t, rtpConn.Close())
			continue
		}
		t.Cleanup(func() {
			_ = rtpConn.Close()
			_ = rtcpConn.Close()
		})
		return rtpConn, rtcpConn
	}
	t.Fatal("no two adjacent UDP ports are free")
	return nil, nil
}

// TestDialogServerEarlyMediaUnkeyedSendsOnlyDTLS pins what early media sends
// while its caller has not keyed it: the DTLS handshake and nothing else.
// The caller's RTP and RTCP addresses are sockets of the test, which never
// answer, so ProgressMedia gives up after its KeyTimeout. A write made then
// is refused, the media has no audio, and everything that reached the caller
// is a DTLS record: no RTP, and no RTCP report of a monitor.
func TestDialogServerEarlyMediaUnkeyedSendsOnlyDTLS(t *testing.T) {
	d, peer, inviteTx := newProgressMediaDTLSDialog(t)
	callerRTP, callerRTCP := listenRTPPair(t)
	d.InviteRequest.SetBody(offerToSocket(t, peer, callerRTP.LocalAddr().(*net.UDPAddr).Port))

	start := time.Now()
	med, err := d.ProgressMedia(t.Context(), ProgressMediaOptions{KeyTimeout: 300 * time.Millisecond})
	require.ErrorIs(t, err, ErrEarlyMediaNotKeyed)
	require.NotNil(t, med)
	assert.Less(t, time.Since(start), 5*time.Second, "ProgressMedia waited past its KeyTimeout")
	select {
	case res := <-inviteTx.responded:
		require.Equal(t, sip.StatusSessionInProgress, res.StatusCode)
	case <-time.After(5 * time.Second):
		t.Fatal("no 183 was sent")
	}

	assert.Nil(t, med.RTPSession())
	_, err = med.AudioWriter()
	assert.ErrorIs(t, err, ErrNoMediaSetup)
	require.ErrorIs(t, med.MediaSession().WriteRTP(&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: media.CodecAudioUlaw.PayloadType, SequenceNumber: 1, Timestamp: 160, SSRC: 0x5eed},
		Payload: bytes.Repeat([]byte{0x5a}, 160),
	}), media.ErrDTLSNotKeyed)

	// The ClientHello is sent again a second after the first, so this window
	// holds every datagram of the first 1.5 s.
	buf := make([]byte, media.RTPBufSize)
	require.NoError(t, callerRTP.SetReadDeadline(time.Now().Add(1500*time.Millisecond)))
	datagrams := 0
	for {
		n, _, err := callerRTP.ReadFromUDP(buf)
		if err != nil {
			require.ErrorIs(t, err, os.ErrDeadlineExceeded)
			break
		}
		datagrams++
		// RFC 7983 section 7: a first byte of 20 to 63 is a DTLS record.
		require.True(t, n > 0 && buf[0] >= 20 && buf[0] <= 63, "datagram %d sent before the media was keyed is not DTLS: first byte %d", datagrams, buf[0])
	}
	assert.NotZero(t, datagrams, "the handshake was never started")

	require.NoError(t, callerRTCP.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, _, err = callerRTCP.ReadFromUDP(buf)
	assert.ErrorIs(t, err, os.ErrDeadlineExceeded, "RTCP was sent before the media was keyed")
}

// TestDialogServerAnswerCompletesEarlyKeying is the answer to a call whose
// caller ignored the 183, over a fake transaction: ProgressMedia gave up on
// the keys, the 200 repeats the 183's answer, the caller takes part in the
// handshake only then, and the reading of the ACK waits for it. The answer
// returns the early media with its audio set up and encrypted.
func TestDialogServerAnswerCompletesEarlyKeying(t *testing.T) {
	answers := []struct {
		name   string
		answer func(t *testing.T, d *DialogServerSession, early *DialogMedia) (*DialogMedia, error)
	}{
		{name: "AnswerEarlyMedia", answer: func(_ *testing.T, d *DialogServerSession, early *DialogMedia) (*DialogMedia, error) {
			return early, d.AnswerEarlyMedia(early, AnswerOptions{})
		}},
		{name: "AnswerContext", answer: func(t *testing.T, d *DialogServerSession, _ *DialogMedia) (*DialogMedia, error) {
			return d.AnswerContext(t.Context())
		}},
		{name: "AnswerOptions", answer: func(t *testing.T, d *DialogServerSession, _ *DialogMedia) (*DialogMedia, error) {
			return d.AnswerOptions(t.Context(), AnswerOptions{})
		}},
	}
	for _, tc := range answers {
		t.Run(tc.name, func(t *testing.T) {
			d, peer, inviteTx := newProgressMediaDTLSDialog(t)

			early, err := d.ProgressMedia(t.Context(), ProgressMediaOptions{KeyTimeout: 100 * time.Millisecond})
			require.ErrorIs(t, err, ErrEarlyMediaNotKeyed)
			var progress *sip.Response
			select {
			case progress = <-inviteTx.responded:
				require.Equal(t, sip.StatusSessionInProgress, progress.StatusCode)
			case <-time.After(5 * time.Second):
				t.Fatal("no 183 was sent")
			}

			type answerResult struct {
				med *DialogMedia
				err error
			}
			answered := make(chan answerResult, 1)
			go func() {
				med, err := tc.answer(t, d, early)
				answered <- answerResult{med: med, err: err}
			}()
			select {
			case res := <-inviteTx.responded:
				require.Equal(t, sip.StatusOK, res.StatusCode)
				require.Equal(t, string(progress.Body()), string(res.Body()), "the 200 must repeat the answer of the 183")
			case <-time.After(5 * time.Second):
				t.Fatal("no 200 was sent")
			}

			// The caller acts on the answer only now, and acknowledges it.
			require.NoError(t, peer.RemoteSDP(progress.Body(), true))
			peerDone := make(chan error, 1)
			go func() { peerDone <- peer.Finalize() }()
			readAck(t, d)
			select {
			case err := <-peerDone:
				require.NoError(t, err)
			case <-time.After(10 * time.Second):
				t.Fatal("the peer's handshake did not return")
			}

			select {
			case r := <-answered:
				require.NoError(t, r.err)
				require.Same(t, early, r.med, "the answer hands back the early media")
			case <-time.After(10 * time.Second):
				t.Fatal("the answer did not return")
			}
			require.NotNil(t, early.RTPSession(), "the answer did not set the media up")
			_, err = early.AudioWriter()
			require.NoError(t, err)
			requireEncryptedMedia(t, early, &DialogMedia{mediaSession: peer}, 100)
		})
	}
}

// TestDialogServerAnswerOptionsSetsEarlyMediaUpdate pins that AnswerOptions
// gives its OnMediaUpdate to the early media it answers with: the media tells
// it of the update a later offer makes.
func TestDialogServerAnswerOptionsSetsEarlyMediaUpdate(t *testing.T) {
	peer := newMediaSessionForTest(t)
	inviteTx := &respondedServerTx{byeServerTx: newByeServerTx(), responded: make(chan *sip.Response, 4)}
	d := newTestDialogOver(t, inviteTx)
	d.InviteRequest.SetBody(peer.LocalSDP(false))
	d.mediaConf = MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}, bindIP: net.IPv4(127, 0, 0, 1)}
	t.Cleanup(func() { _ = d.Close() })

	early, err := d.ProgressMedia(t.Context(), ProgressMediaOptions{})
	require.NoError(t, err)

	updated := make(chan *DialogMedia, 1)
	answered := make(chan error, 1)
	go func() {
		_, err := d.AnswerOptions(t.Context(), AnswerOptions{OnMediaUpdate: func(m *DialogMedia) { updated <- m }})
		answered <- err
	}()
	for _, status := range []int{sip.StatusSessionInProgress, sip.StatusOK} {
		select {
		case res := <-inviteTx.responded:
			require.Equal(t, status, res.StatusCode)
		case <-time.After(5 * time.Second):
			t.Fatalf("no %d was sent", status)
		}
	}
	readAck(t, d)
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not return")
	}

	require.NoError(t, early.onRemoteSDP(t.Context(), peer.LocalSDP(false), false))
	select {
	case m := <-updated:
		assert.Same(t, early, m)
	case <-time.After(5 * time.Second):
		t.Fatal("the early media did not tell of its update")
	}
}
