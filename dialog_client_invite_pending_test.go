// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUDPCallee starts a callee with no SIP stack on a UDP socket of its own
// and returns its address and the method of each request it reads, in order.
// answer is called for each request, on the callee's goroutine, and the
// responses it returns are sent back in that order. The callee ends with the
// test.
func newUDPCallee(t *testing.T, answer func(req *sip.Request) []*sip.Response) (sip.Uri, <-chan sip.RequestMethod) {
	t.Helper()
	conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	require.True(t, ok, "LocalAddr() = %T, want *net.UDPAddr", conn.LocalAddr())

	methods := make(chan sip.RequestMethod, 16)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 65535)
		for {
			n, src, err := conn.ReadFrom(buf)
			if err != nil {
				return
			}
			msg, err := sip.ParseMessage(bytes.Clone(buf[:n]))
			if err != nil {
				continue
			}
			req, ok := msg.(*sip.Request)
			if !ok {
				continue
			}
			select {
			case methods <- req.Method:
			case <-stop:
				return
			}
			for _, res := range answer(req) {
				if _, err := conn.WriteTo([]byte(res.String()), src); err != nil {
					return
				}
			}
		}
	}()
	t.Cleanup(func() {
		close(stop)
		assert.NoError(t, conn.Close())
		<-done
	})
	return sip.Uri{User: "callee", Host: "127.0.0.1", Port: addr.Port}, methods
}

// calleeResponse returns the callee's response to req, with the callee's tag
// and, when body is not nil, with body as its session description.
func calleeResponse(req *sip.Request, status int, reason string, body []byte) *sip.Response {
	res := sip.NewResponseFromRequest(req, status, reason, body)
	if !res.To().Params.Has("tag") {
		res.To().Params.Add("tag", "callee-tag")
	}
	if body != nil {
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	}
	return res
}

// requireCalleeRead fails the test unless the callee reads requests of
// exactly the methods want next, in that order.
func requireCalleeRead(t *testing.T, methods <-chan sip.RequestMethod, want ...sip.RequestMethod) {
	t.Helper()
	for i, method := range want {
		select {
		case got := <-methods:
			require.Equal(t, method, got, "request %d the callee read", i)
		case <-time.After(5 * time.Second):
			t.Fatalf("the callee read no %s, request %d of %v", method, i, want)
		}
	}
}

// newDialogToCallee returns an outgoing dialog to callee, not invited yet,
// over a dialer of its own, and the allocator its media takes its one RTP
// port from, which records the release of that port: the port is released
// once the media is closed.
func newDialogToCallee(t *testing.T, callee sip.Uri) (*DialogClientSession, *funcPortAllocator) {
	t.Helper()
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, ua.Close()) })
	dg := newDialer(ua)
	require.NoError(t, serveBackground(t, dg, t.Context(), func(_ context.Context, d *DialogServerSession) {}))

	d, err := dg.NewDialog(callee, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, d.Close()) })

	port := freeEvenPort(t, net.IPv4(127, 0, 0, 1))
	alloc := &funcPortAllocator{alloc: func(context.Context) (int, error) { return port, nil }}
	d.mediaConfig.RTPPortAllocator = alloc
	return d, alloc
}

// giveUpAtRinging returns a ctx for an INVITE and the OnResponse that ends it
// with cause once the callee rings.
func giveUpAtRinging(t *testing.T, cause error) (context.Context, func(*sip.Response) error) {
	t.Helper()
	ctx, giveUp := context.WithCancelCause(t.Context())
	t.Cleanup(func() { giveUp(nil) })
	return ctx, func(res *sip.Response) error {
		if res.StatusCode == sip.StatusRinging {
			giveUp(cause)
		}
		return nil
	}
}

// TestDialogClientInviteCancelContextCut has a callee that rings and then
// answers neither our CANCEL nor the INVITE. The INVITE's context ends after
// the 180, which leaves the INVITE pending, and the context Cancel is given
// ends while the CANCEL waits: Cancel returns at that end, with its cause,
// instead of after the CANCEL's Timer F.
func TestDialogClientInviteCancelContextCut(t *testing.T) {
	peer, err := net.ListenPacket("udp4", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	cancels := make(chan struct{}, 4)
	peerDone := make(chan struct{})
	go func() {
		defer close(peerDone)
		buf := make([]byte, 65535)
		for {
			n, src, err := peer.ReadFrom(buf)
			if err != nil {
				return
			}
			msg, err := sip.ParseMessage(append([]byte(nil), buf[:n]...))
			if err != nil {
				continue
			}
			req, ok := msg.(*sip.Request)
			if !ok {
				continue
			}
			switch req.Method {
			case sip.INVITE:
				res := sip.NewResponseFromRequest(req, sip.StatusRinging, "Ringing", nil)
				res.To().Params.Add("tag", "callee-tag")
				_, _ = peer.WriteTo([]byte(res.String()), src)
			case sip.CANCEL:
				cancels <- struct{}{}
			}
		}
	}()
	t.Cleanup(func() { _ = peer.Close(); <-peerDone })

	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	dg := newDialer(ua)
	require.NoError(t, serveBackground(t, dg, t.Context(), func(_ context.Context, d *DialogServerSession) {}))

	d, err := dg.NewDialog(sip.Uri{User: "callee", Host: "127.0.0.1", Port: peer.LocalAddr().(*net.UDPAddr).Port}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })

	ctx, giveUp := context.WithCancel(t.Context())
	defer giveUp()
	cancelCtx, stop := context.WithCancelCause(t.Context())
	defer stop(nil)
	_, err = d.InviteLeavePending(ctx, InviteClientOptions{
		OnResponse: func(res *sip.Response) error {
			if res.StatusCode == sip.StatusRinging {
				giveUp()
			}
			return nil
		},
	})
	require.ErrorIs(t, err, sipgo.ErrInvitePending)
	require.ErrorIs(t, err, context.Canceled)
	withdrawn := make(chan error, 1)
	go func() {
		withdrawn <- d.Cancel(cancelCtx)
	}()

	select {
	case <-cancels:
	case <-time.After(5 * time.Second):
		t.Fatal("no CANCEL was sent after the 180")
	}
	errOwnerStopped := errors.New("the call's owner stopped")
	stopAt := time.Now()
	stop(errOwnerStopped)
	select {
	case err := <-withdrawn:
		require.ErrorIs(t, err, errOwnerStopped)
		assert.Less(t, time.Since(stopAt), sip.T1, "the CANCEL's wait must end with its context")
	case <-time.After(5 * time.Second):
		t.Fatal("the CANCEL's wait outlived its context")
	}
}

// TestDialogClientCancelWithdrawsPendingInvite has a callee that rings, and
// answers our CANCEL by ending the INVITE with a 487. The INVITE's context
// ends after the 180: InviteLeavePending returns with the INVITE pending and
// the media of its offer still bound, and Cancel then withdraws the INVITE and
// closes that media.
func TestDialogClientCancelWithdrawsPendingInvite(t *testing.T) {
	var invite *sip.Request
	callee, methods := newUDPCallee(t, func(req *sip.Request) []*sip.Response {
		switch req.Method {
		case sip.INVITE:
			invite = req
			return []*sip.Response{calleeResponse(req, sip.StatusRinging, "Ringing", nil)}
		case sip.CANCEL:
			return []*sip.Response{
				calleeResponse(req, sip.StatusOK, "OK", nil),
				calleeResponse(invite, sip.StatusRequestTerminated, "Request Terminated", nil),
			}
		}
		return nil
	})
	d, alloc := newDialogToCallee(t, callee)

	errGaveUp := errors.New("the caller gave up")
	ctx, onResponse := giveUpAtRinging(t, errGaveUp)
	med, err := d.InviteLeavePending(ctx, InviteClientOptions{OnResponse: onResponse})
	require.ErrorIs(t, err, sipgo.ErrInvitePending)
	require.ErrorIs(t, err, errGaveUp)
	require.Nil(t, med, "a pending INVITE's media is the session's to close")
	require.Empty(t, alloc.released, "the RTP port was released while the INVITE that offers it was pending")
	requireCalleeRead(t, methods, sip.INVITE)

	require.NoError(t, d.Cancel(t.Context()))
	require.Len(t, alloc.released, 1, "Cancel must close the media of the INVITE it withdrew")
	require.Equal(t, sip.StatusRequestTerminated, d.InviteResponse.StatusCode)
	// The ACK is the INVITE transaction's, for the 487.
	requireCalleeRead(t, methods, sip.CANCEL, sip.ACK)
}

// TestDialogClientCancelAnsweredRegardless has a callee that rings and answers
// the INVITE 200 when our CANCEL arrives, which it answers 481. Cancel returns
// the 2xx as *sipgo.ErrAnsweredAfterCancel and closes the media of the INVITE,
// and the dialog is established: Ack acknowledges the 2xx and Bye ends the
// call.
func TestDialogClientCancelAnsweredRegardless(t *testing.T) {
	var invite *sip.Request
	callee, methods := newUDPCallee(t, func(req *sip.Request) []*sip.Response {
		switch req.Method {
		case sip.INVITE:
			invite = req
			return []*sip.Response{calleeResponse(req, sip.StatusRinging, "Ringing", nil)}
		case sip.CANCEL:
			answer := calleeResponse(invite, sip.StatusOK, "OK", peerSDP())
			answer.AppendHeader(&sip.ContactHeader{Address: invite.Recipient})
			return []*sip.Response{
				answer,
				calleeResponse(req, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil),
			}
		case sip.BYE:
			return []*sip.Response{calleeResponse(req, sip.StatusOK, "OK", nil)}
		}
		return nil
	})
	d, alloc := newDialogToCallee(t, callee)

	ctx, onResponse := giveUpAtRinging(t, errors.New("the caller gave up"))
	_, err := d.InviteLeavePending(ctx, InviteClientOptions{OnResponse: onResponse})
	require.ErrorIs(t, err, sipgo.ErrInvitePending)

	err = d.Cancel(t.Context())
	var answered *sipgo.ErrAnsweredAfterCancel
	require.ErrorAs(t, err, &answered)
	require.Equal(t, sip.StatusOK, answered.Res.StatusCode)
	require.Len(t, alloc.released, 1, "Cancel must close the media of an INVITE answered regardless")
	require.Equal(t, sip.DialogStateEstablished, d.LoadState())

	require.NoError(t, d.Ack(t.Context()))
	require.NoError(t, d.Bye(t.Context()))
	require.Equal(t, sip.DialogStateEnded, d.LoadState())
	requireCalleeRead(t, methods, sip.INVITE, sip.CANCEL, sip.ACK, sip.BYE)
}

// TestDialogClientWaitAnswerLeavePendingEarlyMedia has a callee that sends
// early media in a 183 and no final response. InviteLeavePending returns the
// early media, WaitAnswerLeavePending leaves the INVITE pending when its
// context ends and the early media running, and Cancel withdraws the INVITE
// and closes that media.
func TestDialogClientWaitAnswerLeavePendingEarlyMedia(t *testing.T) {
	var invite *sip.Request
	callee, methods := newUDPCallee(t, func(req *sip.Request) []*sip.Response {
		switch req.Method {
		case sip.INVITE:
			invite = req
			return []*sip.Response{calleeResponse(req, sip.StatusSessionInProgress, "Session Progress", peerSDP())}
		case sip.CANCEL:
			return []*sip.Response{
				calleeResponse(req, sip.StatusOK, "OK", nil),
				calleeResponse(invite, sip.StatusRequestTerminated, "Request Terminated", nil),
			}
		}
		return nil
	})
	d, alloc := newDialogToCallee(t, callee)

	med, err := d.InviteLeavePending(t.Context(), InviteClientOptions{EarlyMediaDetect: true})
	require.ErrorIs(t, err, ErrClientEarlyMedia)
	require.NotNil(t, med)

	// No final response is coming, so a wait whose context has ended finds
	// the INVITE pending.
	errGaveUp := errors.New("the caller gave up")
	ctx, giveUp := context.WithCancelCause(t.Context())
	giveUp(errGaveUp)
	err = d.WaitAnswerLeavePending(ctx, med, sipgo.AnswerOptions{})
	require.ErrorIs(t, err, sipgo.ErrInvitePending)
	require.ErrorIs(t, err, errGaveUp)
	require.Empty(t, alloc.released, "the early media was closed while its INVITE was pending")

	require.NoError(t, d.Cancel(t.Context()))
	require.Len(t, alloc.released, 1, "Cancel must close the early media of the INVITE it withdrew")
	requireCalleeRead(t, methods, sip.INVITE, sip.CANCEL, sip.ACK)
}

// TestDialogClientInviteLeavePendingFinalResponse pins that InviteLeavePending
// ends as Invite does when the INVITE gets its final response within the
// context: a 2xx returns the media of the call, which Ack and Bye then carry
// on with and end, and a refusal returns *sipgo.ErrDialogResponse, with the
// media closed.
func TestDialogClientInviteLeavePendingFinalResponse(t *testing.T) {
	t.Run("answered", func(t *testing.T) {
		callee, methods := newUDPCallee(t, func(req *sip.Request) []*sip.Response {
			switch req.Method {
			case sip.INVITE:
				answer := calleeResponse(req, sip.StatusOK, "OK", peerSDP())
				answer.AppendHeader(&sip.ContactHeader{Address: req.Recipient})
				return []*sip.Response{answer}
			case sip.BYE:
				return []*sip.Response{calleeResponse(req, sip.StatusOK, "OK", nil)}
			}
			return nil
		})
		d, alloc := newDialogToCallee(t, callee)

		med, err := d.InviteLeavePending(t.Context(), InviteClientOptions{})
		require.NoError(t, err)
		require.NotNil(t, med)
		require.NotNil(t, med.RTPSession(), "the answer was not applied to the media")
		require.NoError(t, d.Ack(t.Context()))
		require.Empty(t, alloc.released, "the media of an answered call was closed")

		require.NoError(t, d.Bye(t.Context()))
		requireCalleeRead(t, methods, sip.INVITE, sip.ACK, sip.BYE)
		require.NoError(t, d.Close())
		require.Len(t, alloc.released, 1, "Close must close the media of the call")
	})

	t.Run("refused", func(t *testing.T) {
		callee, methods := newUDPCallee(t, func(req *sip.Request) []*sip.Response {
			if req.Method != sip.INVITE {
				return nil
			}
			return []*sip.Response{calleeResponse(req, sip.StatusBusyHere, "Busy Here", nil)}
		})
		d, alloc := newDialogToCallee(t, callee)

		med, err := d.InviteLeavePending(t.Context(), InviteClientOptions{})
		var refused *sipgo.ErrDialogResponse
		require.ErrorAs(t, err, &refused)
		require.Equal(t, sip.StatusBusyHere, refused.Res.StatusCode)
		require.Nil(t, med)
		require.Len(t, alloc.released, 1, "the media of a refused INVITE must be closed")
		requireCalleeRead(t, methods, sip.INVITE, sip.ACK)
	})
}
