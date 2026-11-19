// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// loopbackWebrtcConfig is a WebRTC media configuration whose ICE agent takes
// and accepts loopback host candidates only.
func loopbackWebrtcConfig() media.MediaSessionWebrtcConfig {
	loopbackOnly := func(addr netip.Addr) bool { return addr.IsLoopback() }
	return media.MediaSessionWebrtcConfig{
		IPFamilies:      []media.ICEIPFamily{media.ICEIPFamilyIPv4},
		CandidateTypes:  []media.ICECandidateType{media.ICECandidateHost},
		IncludeLoopback: true,
		IPFilter:        loopbackOnly,
		RemoteIPFilter:  loopbackOnly,
	}
}

// newWebrtcCallPair starts a callee over TCP that handles each call with
// handle, and returns an outgoing dialog to it, not invited yet, over a dialer
// of its own. Both end with the test.
func newWebrtcCallPair(t *testing.T, handle ServeDialogFunc) *DialogClientSession {
	t.Helper()
	tcp := Transport{Transport: "tcp", BindHost: "127.0.0.1", BindPort: 0}

	calleeUA, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, calleeUA.Close()) })
	callee := NewDiago(calleeUA, WithTransport(tcp))
	require.NoError(t, serveBackground(t, callee, t.Context(), handle))

	callerUA, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, callerUA.Close()) })
	caller := NewDiago(callerUA, WithTransport(tcp))
	serveRequests(t, caller)

	d, err := caller.NewDialog(sip.Uri{User: "callee", Host: "127.0.0.1", Port: callee.transports[0].BindPort}, NewDialogOptions{Transport: "tcp"})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, d.Close()) })
	return d
}

// TestDialogClientInviteWebrtcLeavePending has a callee that rings and does
// not answer. The INVITE's context ends after the 180:
// InviteWebrtcLeavePending returns with the INVITE pending and no media, and
// Cancel then withdraws the INVITE.
func TestDialogClientInviteWebrtcLeavePending(t *testing.T) {
	d := newWebrtcCallPair(t, func(ctx context.Context, callee *DialogServerSession) {
		if err := callee.Ringing(ctx); err != nil {
			return
		}
		<-callee.Context().Done()
	})

	errGaveUp := errors.New("the caller gave up")
	ctx, onResponse := giveUpAtRinging(t, errGaveUp)
	med, err := d.InviteWebrtcLeavePending(ctx, InviteWebrtcOptions{
		OnResponse:   onResponse,
		WebrtcConfig: loopbackWebrtcConfig(),
	})
	require.ErrorIs(t, err, sipgo.ErrInvitePending)
	require.ErrorIs(t, err, errGaveUp)
	require.Nil(t, med)

	require.NoError(t, d.Cancel(t.Context()))
	require.Equal(t, sip.StatusRequestTerminated, d.InviteResponse.StatusCode)
}

// answerWebrtcEarlyMedia answers a call with WebRTC early media in a 183 and
// no final response, and holds it until the call ends. It closes setUp once
// its side of the early media is set up, and sends ended what it failed with,
// nil when the call ended with the early media set up.
func answerWebrtcEarlyMedia(callee *DialogServerSession, setUp chan<- struct{}, ended chan<- error) {
	ended <- func() (err error) {
		ctx := callee.Context()
		conf, err := prepareWebrtcConfig(loopbackWebrtcConfig())
		if err != nil {
			return err
		}
		sess := &media.MediaSessionWebrtc{Codecs: callee.mediaConf.Codecs}
		if err := sess.Init(ctx, conf); err != nil {
			return err
		}
		closeMedia := sess.Close
		defer func() { err = errors.Join(err, closeMedia()) }()

		if err := sess.RemoteSDP(ctx, callee.InviteRequest.Body(), false); err != nil {
			return err
		}
		answer, err := sess.LocalSDP(ctx, true)
		if err != nil {
			return err
		}
		contentType := sip.NewHeader("Content-Type", "application/sdp")
		if err := callee.DialogServerSession.Respond(ctx, sip.StatusSessionInProgress, "Session Progress", answer, contentType); err != nil {
			return err
		}
		if err := sess.Finalize(ctx); err != nil {
			return err
		}
		rtpSess := media.NewRTPSessionWebrtc(sess)
		med := &DialogWebrtc{}
		med.Init(sess, rtpSess)
		closeMedia = med.Close
		if err := rtpSess.MonitorBackground(); err != nil {
			return err
		}
		close(setUp)
		<-ctx.Done()
		return nil
	}()
}

// TestDialogClientWaitAnswerWebrtcLeavePendingEarlyMedia has a callee that
// sends WebRTC early media in a 183 and no final response.
// InviteWebrtcLeavePending returns the early media,
// WaitAnswerWebrtcLeavePending leaves the INVITE pending when its context ends
// and the early media running, and Cancel withdraws the INVITE and closes that
// media.
func TestDialogClientWaitAnswerWebrtcLeavePendingEarlyMedia(t *testing.T) {
	calleeSetUp := make(chan struct{})
	calleeEnded := make(chan error, 1)
	d := newWebrtcCallPair(t, func(_ context.Context, callee *DialogServerSession) {
		answerWebrtcEarlyMedia(callee, calleeSetUp, calleeEnded)
	})

	inviteCtx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	med, err := d.InviteWebrtcLeavePending(inviteCtx, InviteWebrtcOptions{
		EarlyMediaDetect: true,
		WebrtcConfig:     loopbackWebrtcConfig(),
	})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	require.NotNil(t, med)
	t.Cleanup(func() { assert.NoError(t, med.Close()) })
	// Our side of the handshake is complete before the callee's is, and a
	// CANCEL would end the callee's while it runs.
	select {
	case <-calleeSetUp:
	case err := <-calleeEnded:
		t.Fatalf("the callee's early media was not set up: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("the callee's early media was not set up")
	}

	// No final response is coming, so a wait whose context has ended finds
	// the INVITE pending.
	errGaveUp := errors.New("the caller gave up")
	ctx, giveUp := context.WithCancelCause(t.Context())
	giveUp(errGaveUp)
	err = d.WaitAnswerWebrtcLeavePending(ctx, med, sipgo.AnswerOptions{})
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

// webrtcMediaClosed reports whether med has been closed.
func webrtcMediaClosed(med *DialogWebrtc) bool {
	med.mu.Lock()
	defer med.mu.Unlock()
	return med.closed
}
