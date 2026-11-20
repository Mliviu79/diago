// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newSDESCallPair starts a callee over TCP that handles each call with
// handle, and returns an outgoing dialog to it, not invited yet, over a dialer
// of its own whose media offers PCMU and PCMA keyed with SDES. Both end with
// the test.
func newSDESCallPair(t *testing.T, handle ServeDialogFunc) *DialogClientSession {
	t.Helper()
	calleeUA, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, calleeUA.Close()) })
	callee := NewDiago(calleeUA, WithTransport(Transport{Transport: "tcp", BindHost: "127.0.0.1", BindPort: 0}))
	require.NoError(t, serveBackground(t, callee, t.Context(), handle))

	callerUA, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, callerUA.Close()) })
	caller := NewDiago(callerUA,
		WithTransport(Transport{Transport: "tcp", BindHost: "127.0.0.1", BindPort: 0, MediaSRTP: media.SecureRTPModeSDES}),
		WithMediaConfig(MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw}}),
	)
	serveRequests(t, caller)

	d, err := caller.NewDialog(sip.Uri{User: "callee", Host: "127.0.0.1", Port: callee.transports[0].BindPort}, NewDialogOptions{Transport: "tcp"})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, d.Close()) })
	return d
}

// newSDESAnswerer returns a media session on the loopback interface with
// codecs, keyed with SDES, that has taken offer and returns its answer.
func newSDESAnswerer(codecs []media.Codec, offer []byte) (*media.MediaSession, []byte, error) {
	sess := &media.MediaSession{
		Codecs:    codecs,
		Mode:      sdp.ModeSendrecv,
		Laddr:     net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
		SecureRTP: media.SecureRTPModeSDES,
	}
	if err := sess.Init(); err != nil {
		return nil, nil, err
	}
	if err := sess.RemoteSDP(offer, false); err != nil {
		return nil, nil, errors.Join(err, sess.Close())
	}
	return sess, sess.LocalSDP(true), nil
}

// TestDialogClientEarlyMediaFinalAnswerIgnored pins that the session
// description of the 200 after early media is not applied: the UAC "MUST
// ignore any session descriptions in subsequent responses to the initial
// INVITE" (RFC 3261 section 13.2.1), and does not switch to one that differs
// from the first (RFC 6337 section 3.1.1). The callee answers the 183 from
// one media session and the 200 from another, on another port, with another
// SDES key and another codec. The call is answered and keeps the media
// session, the remote address, the keys and the codec of the 183, on which
// audio flows both ways.
func TestDialogClientEarlyMediaFinalAnswerIgnored(t *testing.T) {
	calleeSetUp := make(chan *media.MediaSession, 1)
	calleeEnded := make(chan error, 1)
	answerNow := make(chan struct{})
	d := newSDESCallPair(t, func(serveCtx context.Context, callee *DialogServerSession) {
		ctx := callee.Context()
		calleeEnded <- func() error {
			offer := callee.InviteRequest.Body()
			early, earlyAnswer, err := newSDESAnswerer([]media.Codec{media.CodecAudioUlaw}, offer)
			if err != nil {
				return err
			}
			defer func() { _ = early.Close() }()
			other, finalAnswer, err := newSDESAnswerer([]media.Codec{media.CodecAudioAlaw}, offer)
			if err != nil {
				return err
			}
			defer func() { _ = other.Close() }()

			contentType := sip.NewHeader("Content-Type", "application/sdp")
			if err := callee.DialogServerSession.Respond(ctx, sip.StatusSessionInProgress, "Session Progress", earlyAnswer, contentType); err != nil {
				return err
			}
			calleeSetUp <- early
			// The handler ends with its run as well as with its dialog: a dialog
			// whose BYE never arrived would otherwise hold serving past the test.
			select {
			case <-answerNow:
			case <-serveCtx.Done():
				return context.Cause(serveCtx)
			case <-ctx.Done():
				return context.Cause(ctx)
			}
			if err := callee.DialogServerSession.Respond(ctx, sip.StatusOK, "OK", finalAnswer, contentType); err != nil {
				return err
			}
			select {
			case <-serveCtx.Done():
			case <-ctx.Done():
			}
			return nil
		}()
	})

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	med, err := d.Invite(ctx, InviteClientOptions{EarlyMediaDetect: true})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	var peer *media.MediaSession
	select {
	case peer = <-calleeSetUp:
	case err := <-calleeEnded:
		t.Fatalf("the callee's early media was not set up: %v", err)
	case <-time.After(20 * time.Second):
		t.Fatal("the callee's early media was not set up")
	}
	sess := med.MediaSession()
	require.NotNil(t, sess)
	rtpSess := med.RTPSession()
	require.Equal(t, peer.Laddr.String(), sess.Raddr.String())

	close(answerNow)
	require.NoError(t, d.WaitAnswer(ctx, med, sipgo.AnswerOptions{}))
	require.NoError(t, d.Ack(ctx))
	assert.Equal(t, sip.DialogStateConfirmed, d.LoadState(), "the call is answered and acknowledged")
	require.Same(t, sess, med.MediaSession(), "the 200's description replaced the media session")
	require.Same(t, rtpSess, med.RTPSession())
	assert.Equal(t, peer.Laddr.String(), sess.Raddr.String(), "the call left the remote address of the 183")
	assert.Equal(t, []media.Codec{media.CodecAudioUlaw}, sess.CommonCodecs(), "the call left the codec of the 183")

	// Our audio reaches the callee's media of the 183, keyed with our offer's
	// key, as PCMU.
	payload := bytes.Repeat([]byte{0x31}, int(media.CodecAudioUlaw.SampleTimestamp()))
	_, err = med.RTPPacketWriter.Write(payload)
	require.NoError(t, err)
	require.NoError(t, peer.StopRTP(1, 5*time.Second))
	pkt := rtp.Packet{}
	_, err = peer.ReadRTP(make([]byte, media.RTPBufSize), &pkt)
	require.NoError(t, err, "the callee's media of the 183 read nothing of ours")
	assert.Equal(t, media.CodecAudioUlaw.PayloadType, pkt.PayloadType)
	assert.Equal(t, payload, pkt.Payload)

	// The callee's audio, keyed with the 183's key, is read.
	require.NoError(t, peer.WriteRTP(&rtp.Packet{
		Header:  rtp.Header{Version: 2, PayloadType: media.CodecAudioUlaw.PayloadType, SequenceNumber: 1, Timestamp: 160, SSRC: 0x5eed},
		Payload: payload,
	}))
	require.NoError(t, sess.StopRTP(1, 5*time.Second))
	received := make([]byte, media.RTPBufSize)
	n, err := med.RTPPacketReader.Read(received)
	require.NoError(t, err, "the call read nothing of the callee's media of the 183")
	assert.Equal(t, payload, received[:n])
	require.NoError(t, sess.StartRTP(1))

	require.NoError(t, d.Hangup(t.Context()))
	select {
	case err := <-calleeEnded:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the callee did not return")
	}
}
