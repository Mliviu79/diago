// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newWebrtcDefaultsCallPair is newWebrtcCallPair with both parties given
// loopbackWebrtcConfig as the default WebRTC configuration of their media
// config, so neither call passes a configuration of its own.
func newWebrtcDefaultsCallPair(t *testing.T, handle ServeDialogFunc) *DialogClientSession {
	t.Helper()
	tcp := Transport{Transport: "tcp", BindHost: "127.0.0.1", BindPort: 0}
	conf := WithMediaConfig(MediaConfig{
		Codecs:       []media.Codec{media.CodecAudioUlaw},
		WebrtcConfig: loopbackWebrtcConfig(),
	})

	calleeUA, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, calleeUA.Close()) })
	callee := NewDiago(calleeUA, WithTransport(tcp), conf)
	require.NoError(t, serveBackground(t, callee, t.Context(), handle))

	callerUA, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, callerUA.Close()) })
	caller := NewDiago(callerUA, WithTransport(tcp), conf)
	serveRequests(t, caller)

	d, err := caller.NewDialog(sip.Uri{User: "callee", Host: "127.0.0.1", Port: callee.transports[0].BindPort}, NewDialogOptions{Transport: "tcp"})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, d.Close()) })
	return d
}

// requireWebrtcAudioBetween requires that a and b, both completed, carry
// audio to each other.
func requireWebrtcAudioBetween(t *testing.T, a, b *DialogWebrtc) {
	t.Helper()
	require.NoError(t, a.MediaSession().StopRTP(1, 5*time.Second))
	require.NoError(t, b.MediaSession().StopRTP(1, 5*time.Second))
	received := make([]byte, media.RTPBufSize)
	for _, pair := range []struct{ from, to *DialogWebrtc }{{from: a, to: b}, {from: b, to: a}} {
		payload := bytes.Repeat([]byte{0x31}, int(media.CodecAudioUlaw.SampleTimestamp()))
		writer, err := pair.from.AudioWriter()
		require.NoError(t, err)
		n, err := writer.Write(payload)
		require.NoError(t, err)
		require.Equal(t, len(payload), n)
		reader, err := pair.to.AudioReader()
		require.NoError(t, err)
		n, err = reader.Read(received)
		require.NoError(t, err)
		require.Equal(t, payload, received[:n])
	}
}

// TestDialogClientInviteWebrtcLeaveAck pins the WebRTC INVITE that leaves the
// ACK to the caller, against a callee that answers with AnswerWebrtcContext,
// both on the default WebRTC configuration of their media config: the INVITE
// returns the media with no transport set up, the callee's answer waits for the
// ACK, and Ack sends it and completes the media, which then carries audio both
// ways.
func TestDialogClientInviteWebrtcLeaveAck(t *testing.T) {
	type calleeAnswer struct {
		med *DialogWebrtc
		err error
	}
	answered := make(chan calleeAnswer, 1)
	d := newWebrtcDefaultsCallPair(t, func(ctx context.Context, callee *DialogServerSession) {
		med, err := callee.AnswerWebrtcContext(ctx, AnswerWebrtcOptions{})
		answered <- calleeAnswer{med: med, err: err}
		if err != nil {
			return
		}
		<-callee.Context().Done()
	})

	inviteCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	med, err := d.InviteWebrtcLeaveAck(inviteCtx, InviteWebrtcOptions{})
	require.NoError(t, err)
	require.NotNil(t, med)
	assert.Nil(t, med.MediaSession(), "the media is set up by Ack")
	_, err = med.AudioReader()
	assert.ErrorIs(t, err, ErrNoMediaSetup)
	select {
	case answer := <-answered:
		t.Fatalf("the callee's answer returned before the ACK: %v", answer.err)
	default:
	}

	require.NoError(t, d.Ack(inviteCtx))
	var calleeMed *DialogWebrtc
	select {
	case answer := <-answered:
		require.NoError(t, answer.err)
		calleeMed = answer.med
	case <-time.After(20 * time.Second):
		t.Fatal("the callee's answer did not return")
	}
	require.NotNil(t, med.MediaSession(), "Ack completed the media")
	require.NotNil(t, med.RTPSession())
	assert.Error(t, med.RTPSession().MonitorBackground(), "the RTCP monitor is already running")
	requireWebrtcAudioBetween(t, med, calleeMed)

	require.NoError(t, d.Hangup(t.Context()))
	require.NoError(t, d.Close())
	assert.True(t, webrtcMediaClosed(med), "the session's Close closes the media")
}

// TestDialogClientInviteWebrtcLeaveAckPending has a callee that rings and does
// not answer. The INVITE's context ends after the 180: InviteWebrtcLeaveAck
// returns with the INVITE pending and no media, and Cancel then withdraws the
// INVITE.
func TestDialogClientInviteWebrtcLeaveAckPending(t *testing.T) {
	d := newWebrtcCallPair(t, func(ctx context.Context, callee *DialogServerSession) {
		if err := callee.Ringing(ctx); err != nil {
			return
		}
		<-callee.Context().Done()
	})

	errGaveUp := errors.New("the caller gave up")
	ctx, onResponse := giveUpAtRinging(t, errGaveUp)
	med, err := d.InviteWebrtcLeaveAck(ctx, InviteWebrtcOptions{
		OnResponse:   onResponse,
		WebrtcConfig: loopbackWebrtcConfig(),
	})
	require.ErrorIs(t, err, sipgo.ErrInvitePending)
	require.ErrorIs(t, err, errGaveUp)
	require.Nil(t, med)

	require.NoError(t, d.Cancel(t.Context()))
	require.Equal(t, sip.StatusRequestTerminated, d.InviteResponse.StatusCode)
}

// TestDialogClientWaitAnswerWebrtcLeaveAckEarlyMedia has a callee that sends
// WebRTC early media in a 183 and no final response. InviteWebrtcLeaveAck
// returns the early media, completed, WaitAnswerWebrtcLeaveAck leaves the
// INVITE pending when its context ends and the early media running, and
// Cancel withdraws the INVITE and closes that media.
func TestDialogClientWaitAnswerWebrtcLeaveAckEarlyMedia(t *testing.T) {
	calleeSetUp := make(chan struct{})
	calleeEnded := make(chan error, 1)
	d := newWebrtcCallPair(t, func(_ context.Context, callee *DialogServerSession) {
		answerWebrtcEarlyMedia(callee, calleeSetUp, calleeEnded)
	})

	inviteCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	med, err := d.InviteWebrtcLeaveAck(inviteCtx, InviteWebrtcOptions{
		EarlyMediaDetect: true,
		WebrtcConfig:     loopbackWebrtcConfig(),
	})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	require.NotNil(t, med)
	require.NotNil(t, med.MediaSession(), "the early media is set up")
	t.Cleanup(func() { assert.NoError(t, med.Close()) })
	select {
	case <-calleeSetUp:
	case err := <-calleeEnded:
		t.Fatalf("the callee's early media was not set up: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the callee's early media was not set up")
	}

	errGaveUp := errors.New("the caller gave up")
	ctx, giveUp := context.WithCancelCause(t.Context())
	giveUp(errGaveUp)
	err = d.WaitAnswerWebrtcLeaveAck(ctx, med, sipgo.AnswerOptions{})
	require.ErrorIs(t, err, sipgo.ErrInvitePending)
	require.ErrorIs(t, err, errGaveUp)
	require.False(t, webrtcMediaClosed(med), "the early media was closed while its INVITE was pending")

	require.NoError(t, d.Cancel(t.Context()))
	require.True(t, webrtcMediaClosed(med), "Cancel must close the early media of the INVITE it withdrew")
	select {
	case err := <-calleeEnded:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the callee's call did not end with the CANCEL")
	}
}

// TestDialogClientInviteWebrtcLeaveAckNoAnswer has a callee whose 2xx carries
// no answer to the offer of the INVITE. RFC 3261 section 13.2.1 has the answer
// in that 2xx, so the call has no media: InviteWebrtcLeaveAck acknowledges the
// 2xx, ends the call with a BYE (section 13.2.2.4) and returns why, with no
// media.
func TestDialogClientInviteWebrtcLeaveAckNoAnswer(t *testing.T) {
	calleeEnded := make(chan struct{})
	d := newWebrtcCallPair(t, func(ctx context.Context, callee *DialogServerSession) {
		defer close(calleeEnded)
		if err := callee.DialogServerSession.Respond(ctx, sip.StatusOK, "OK", nil); err != nil {
			return
		}
		<-callee.Context().Done()
	})

	inviteCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	med, err := d.InviteWebrtcLeaveAck(inviteCtx, InviteWebrtcOptions{WebrtcConfig: loopbackWebrtcConfig()})
	require.ErrorIs(t, err, errNoAnswerIn2xx)
	require.Nil(t, med)
	assert.Equal(t, sip.DialogStateEnded, d.LoadState(), "the call is ended")
	select {
	case <-calleeEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("the callee's call did not end with the BYE")
	}
}
