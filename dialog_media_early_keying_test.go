// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"crypto/tls"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/pion/dtls/v3/pkg/crypto/selfsign"
	"github.com/stretchr/testify/require"
)

// heldDeadlineConn is a packet conn whose SetReadDeadline to an instant past,
// which is how the DTLS stack interrupts the read of a handshake it ends,
// waits for release once hold is called. It tells entered when such a call
// first waits.
type heldDeadlineConn struct {
	net.PacketConn

	mu      sync.Mutex
	release chan struct{}
	entered chan struct{}
	once    sync.Once
}

func newHeldDeadlineConn(t *testing.T) *heldDeadlineConn {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	return &heldDeadlineConn{PacketConn: conn, entered: make(chan struct{})}
}

// hold makes the SetReadDeadline calls to an instant past wait from now on,
// until the returned release is called.
func (c *heldDeadlineConn) hold() (release func()) {
	ch := make(chan struct{})
	c.mu.Lock()
	c.release = ch
	c.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { close(ch) }) }
}

func (c *heldDeadlineConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	release := c.release
	c.mu.Unlock()
	if release != nil && !t.IsZero() && t.Before(time.Now()) {
		c.once.Do(func() { close(c.entered) })
		<-release
	}
	return c.PacketConn.SetReadDeadline(t)
}

// newHeldEarlyKeyingMedia returns DTLS-SRTP media whose offer was answered by
// a peer that never keys it, with the handshake of that answer left running
// on an RTP socket whose reads it ends can be held, and that socket. The
// media is closed with the test.
func newHeldEarlyKeyingMedia(t *testing.T) (*DialogMedia, *heldDeadlineConn, []byte) {
	t.Helper()
	rtp := newHeldDeadlineConn(t)
	rtcp, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	caller := &media.MediaSession{
		Codecs:    []media.Codec{media.CodecAudioUlaw},
		SecureRTP: media.SecureRTPModeDTLS,
		DTLSConf:  media.DTLSConfig{Certificates: []tls.Certificate{testdata.ClientCertificate()}},
	}
	caller.InitWithListeners(rtp, rtcp, &net.UDPAddr{})
	d := &DialogMedia{mediaSession: caller}
	t.Cleanup(func() { _ = d.Close() })

	offer := caller.LocalSDP(false)
	early := newDTLSMediaSession(t, testdata.ServerCertificate())
	require.NoError(t, early.RemoteSDP(offer, false))
	require.NoError(t, d.onRemoteSDP(t.Context(), early.LocalSDP(true), true))
	// The callee of the 183 runs no handshake.
	require.ErrorIs(t, d.onFinalizeWithin(t.Context(), 200*time.Millisecond), errMediaKeyingInProgress)
	return d, rtp, offer
}

// TestDialogMediaOtherEarlyAnswerJoinsUnlocked pins that the media takes the
// answer of another early dialog without holding its lock while it ends the
// handshake the first answer left running: another call on the media is
// answered meanwhile, a second exchange is refused as one in progress, and
// the new answer is put in place only once that handshake has returned.
func TestDialogMediaOtherEarlyAnswerJoinsUnlocked(t *testing.T) {
	d, rtp, offer := newHeldEarlyKeyingMedia(t)
	replaced := d.MediaSession()
	cert, err := selfsign.GenerateSelfSigned()
	require.NoError(t, err)
	answering := newDTLSMediaSession(t, cert)
	require.NoError(t, answering.RemoteSDP(offer, false))
	answer := answering.LocalSDP(true)

	release := rtp.hold()
	defer release()
	replacedErr := make(chan error, 1)
	go func() { replacedErr <- d.onRemoteSDP(t.Context(), answer, true) }()
	select {
	case <-rtp.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the handshake left running was not ended")
	}

	got := make(chan *media.MediaSession, 1)
	go func() { got <- d.MediaSession() }()
	select {
	case sess := <-got:
		require.Same(t, replaced, sess, "the new answer was put in place before the handshake returned")
	case <-time.After(5 * time.Second):
		t.Fatal("the media's lock is held while the handshake is joined")
	}
	require.ErrorIs(t, d.onRemoteSDP(t.Context(), answer, true), errMediaUpdateInProgress)
	select {
	case err := <-replacedErr:
		t.Fatalf("the answer was taken before the handshake returned: %v", err)
	default:
	}

	release()
	select {
	case err := <-replacedErr:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the answer was not taken once the handshake returned")
	}
	sess := d.MediaSession()
	require.NotSame(t, replaced, sess, "the answer of the other early dialog was not taken")
	require.Equal(t, answering.Laddr.Port, sess.Raddr.Port)
}

// TestDialogMediaOtherEarlyAnswerClosedMeanwhile pins that media closed while
// it ends the handshake an answer left running takes no other answer: Close
// is not held by the media's lock, returns once the handshake has, and the
// answer is refused as one for closed media.
func TestDialogMediaOtherEarlyAnswerClosedMeanwhile(t *testing.T) {
	d, rtp, offer := newHeldEarlyKeyingMedia(t)
	replaced := d.MediaSession()
	answering := newDTLSMediaSession(t, testdata.ServerCertificate())
	require.NoError(t, answering.RemoteSDP(offer, false))
	answer := answering.LocalSDP(true)

	release := rtp.hold()
	defer release()
	replacedErr := make(chan error, 1)
	go func() { replacedErr <- d.onRemoteSDP(t.Context(), answer, true) }()
	select {
	case <-rtp.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the handshake left running was not ended")
	}

	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	// Close latches the media closed under its lock before it waits for the
	// handshake: a later exchange is then refused as one for closed media.
	require.Eventually(t, func() bool {
		return errors.Is(d.onRemoteSDP(t.Context(), answer, true), errMediaClosed)
	}, 5*time.Second, 10*time.Millisecond, "Close did not take the media's lock while the handshake was joined")

	release()
	select {
	case err := <-replacedErr:
		require.ErrorIs(t, err, errMediaClosed)
	case <-time.After(10 * time.Second):
		t.Fatal("the answer was not refused once the handshake returned")
	}
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return once the handshake had")
	}
	require.Same(t, replaced, d.MediaSession(), "an answer was put in place on closed media")
}
