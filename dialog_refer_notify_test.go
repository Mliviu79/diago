// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referNotifyDialog is a minimal dialog session with the callbacks and the SIP
// state a session has. Only those, DialogSIP and Hangup are reached by
// dialogRouteReferNotify, the rest stays unimplemented. Its SIP dialog is a
// bare sipgo dialog put in the confirmed state.
type referNotifyDialog struct {
	DialogSession
	dialogCallbacks
	sessionState
	sipDialog *sipgo.Dialog
	hangups   int
}

func newReferNotifyDialog() *referNotifyDialog {
	invite := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "127.0.0.1", Port: 5060})
	sipDialog := &sipgo.Dialog{InviteRequest: invite}
	sipDialog.InitWithState(sip.DialogStateConfirmed)
	return &referNotifyDialog{sipDialog: sipDialog}
}

func (d *referNotifyDialog) DialogSIP() *sipgo.Dialog { return d.sipDialog }

func (d *referNotifyDialog) Hangup(ctx context.Context) error {
	d.hangups++
	return nil
}

func newReferNotifyRequest(t *testing.T, contentType string, body string) *sip.Request {
	t.Helper()

	viaParams := sip.NewParams()
	viaParams.Add("branch", sip.GenerateBranch())
	fromParams := sip.NewParams()
	fromParams.Add("tag", "fromtag")
	toParams := sip.NewParams()
	toParams.Add("tag", "totag")

	req := sip.NewRequest(sip.NOTIFY, sip.Uri{User: "bob", Host: "127.0.0.1", Port: 5060})
	req.AppendHeader(&sip.ViaHeader{
		ProtocolName:    "SIP",
		ProtocolVersion: "2.0",
		Transport:       "UDP",
		Host:            "127.0.0.1",
		Port:            5060,
		Params:          viaParams,
	})
	req.AppendHeader(&sip.FromHeader{
		Address: sip.Uri{User: "alice", Host: "127.0.0.1"},
		Params:  fromParams,
	})
	req.AppendHeader(&sip.ToHeader{
		Address: sip.Uri{User: "bob", Host: "127.0.0.1"},
		Params:  toParams,
	})
	callid := sip.CallIDHeader("refer-notify-test")
	req.AppendHeader(&callid)
	req.AppendHeader(&sip.CSeqHeader{SeqNo: 1, MethodName: sip.NOTIFY})
	if contentType != "" {
		req.AppendHeader(sip.NewHeader("Content-Type", contentType))
	}
	req.SetBody([]byte(body))
	return req
}

func newReferNotifyTx(t *testing.T, req *sip.Request) (*sip.ServerTx, *connRecorder) {
	t.Helper()

	tx, conn, err := buildReferNotifyTx(req)
	require.NoError(t, err)
	return tx, conn
}

// buildReferNotifyTx builds the server transaction newReferNotifyTx does and
// returns its error instead of asserting it, for helpers that also run off
// the test goroutine, where FailNow must not be called.
func buildReferNotifyTx(req *sip.Request) (*sip.ServerTx, *connRecorder, error) {
	key, err := sip.ServerTxKeyMake(req)
	if err != nil {
		return nil, nil, err
	}

	conn := NewConnRecorder()
	tx := sip.NewServerTx(key, req, conn, slog.Default())
	if err := tx.Init(); err != nil {
		return nil, nil, err
	}
	return tx, conn, nil
}

// TestDialogHandleReferNotifyContentType checks the Content-Type gate on REFER
// NOTIFY. RFC 3420 Section 5 makes the version parameter of message/sipfrag
// optional and defaulting to "2.0", so a NOTIFY that omits it must still be
// accepted and reach the OnNotify callback.
func TestDialogHandleReferNotifyContentType(t *testing.T) {
	for _, tc := range []struct {
		name        string
		contentType string
		expectCode  int
		expectFired bool
	}{
		{
			// Regression: 3CX and other PBXes omit the optional version param.
			name:        "sipfrag without version param",
			contentType: "message/sipfrag",
			expectCode:  sip.StatusOK,
			expectFired: true,
		},
		{
			name:        "sipfrag with version param",
			contentType: "message/sipfrag;version=2.0",
			expectCode:  sip.StatusOK,
			expectFired: true,
		},
		{
			name:        "unrelated content type is rejected",
			contentType: "application/sdp",
			expectCode:  sip.StatusBadRequest,
			expectFired: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notified := -1
			d := newReferNotifyDialog()
			d.onReferNotify = func(statusCode int) { notified = statusCode }

			req := newReferNotifyRequest(t, tc.contentType, "SIP/2.0 200 OK")
			tx, conn := newReferNotifyTx(t, req)

			dialogRouteReferNotify(t.Context(), d, req, tx, d.referNotifyCallback())

			require.Len(t, conn.messages(), 1)
			res, ok := conn.messages()[0].(*sip.Response)
			require.True(t, ok)
			require.Equal(t, tc.expectCode, res.StatusCode)

			if tc.expectFired {
				require.Equal(t, sip.StatusOK, notified, "OnNotify should receive the sipfrag status code")
			} else {
				require.Equal(t, -1, notified, "OnNotify must not fire on a rejected NOTIFY")
			}
			require.Zero(t, d.hangups, "a REFER NOTIFY never ends the dialog")
		})
	}
}

// TestDialogHandleReferNotifyShortBody checks a sipfrag body too short to carry
// a status line is rejected rather than panicking on the slice.
func TestDialogHandleReferNotifyShortBody(t *testing.T) {
	d := newReferNotifyDialog()
	req := newReferNotifyRequest(t, "message/sipfrag", "SIP/2.0")
	tx, conn := newReferNotifyTx(t, req)

	dialogRouteReferNotify(t.Context(), d, req, tx, d.referNotifyCallback())

	require.Len(t, conn.messages(), 1)
	res := conn.messages()[0].(*sip.Response)
	require.Equal(t, sip.StatusBadRequest, res.StatusCode)
}

// TestDialogHandleReferNotifyWithoutContentType checks an in-dialog NOTIFY that
// carries no Content-Type is rejected rather than panicking on the missing
// header, and that the rejection is never recorded as a transfer outcome.
func TestDialogHandleReferNotifyWithoutContentType(t *testing.T) {
	notified := -1
	d := newReferNotifyDialog()
	d.onReferNotify = func(statusCode int) { notified = statusCode }
	attempt := d.beginReferAttempt()

	req := newReferNotifyRequest(t, "", "SIP/2.0 200 OK")
	require.Nil(t, req.ContentType(), "the NOTIFY must reach the handler without Content-Type")
	tx, conn := newReferNotifyTx(t, req)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		dialogRouteReferNotify(t.Context(), d, req, tx, d.referNotifyCallback())
	}()
	require.Nil(t, recovered, "dialogRouteReferNotify panicked")

	require.Len(t, conn.messages(), 1)
	res, ok := conn.messages()[0].(*sip.Response)
	require.True(t, ok)
	require.Equal(t, sip.StatusBadRequest, res.StatusCode)
	require.Equal(t, -1, notified, "OnNotify must not fire on a rejected NOTIFY")

	d.referMu.Lock()
	notifies, end := len(attempt.notifies), attempt.end
	d.referMu.Unlock()
	require.Zero(t, notifies, "a rejected NOTIFY must not be recorded on the REFER attempt")
	require.Zero(t, end, "a rejected NOTIFY must not end the REFER attempt")
	require.Zero(t, d.hangups, "a REFER NOTIFY never ends the dialog")
}

// TestDialogHandleReferNotifyLeavesTheDialogAlone checks a REFER NOTIFY hands
// the result on and never ends the dialog, whoever sent it. Each row has an
// open REFER attempt and no OnNotify callback: what follows a transfer result
// is the dialog owner's decision.
func TestDialogHandleReferNotifyLeavesTheDialogAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		body    string
		headers []sip.Header
	}{
		{name: "final failure", body: "SIP/2.0 486 Busy Here"},
		{name: "final success", body: "SIP/2.0 200 OK"},
		{
			name:    "progress with the subscription terminated",
			body:    "SIP/2.0 100 Trying",
			headers: []sip.Header{sip.NewHeader("Subscription-State", "terminated;reason=noresource")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newReferNotifyDialog()
			d.beginReferAttempt()

			req := newReferNotifyRequest(t, "message/sipfrag", tc.body)
			for _, h := range tc.headers {
				req.AppendHeader(h)
			}
			tx, conn := newReferNotifyTx(t, req)

			dialogRouteReferNotify(t.Context(), d, req, tx, d.referNotifyCallback())

			require.Len(t, conn.messages(), 1)
			res, ok := conn.messages()[0].(*sip.Response)
			require.True(t, ok)
			require.Equal(t, sip.StatusOK, res.StatusCode)
			require.Zero(t, d.hangups, "a %s NOTIFY ended the dialog", tc.name)
		})
	}
}

// referObserveDialog is a dialog session, with the callbacks and the SIP state
// a session has, that answers a REFER with a canned response and counts what
// the REFER path does to the dialog. Its SIP dialog is a bare sipgo dialog put
// in the confirmed state, unless a test changes it.
type referObserveDialog struct {
	DialogSession
	dialogCallbacks
	sessionState
	sipDialog *sipgo.Dialog

	// status and reason are the REFER's final response.
	status int
	reason string
	// nextCSeq is stamped on a REFER sent without a CSeq.
	nextCSeq uint32
	// doErr, when set, is returned by Do instead of a response.
	doErr error
	// onDo runs once the REFER is counted as sent, before it is answered.
	onDo func(req *sip.Request)

	hangups    atomic.Int32
	refersSent atomic.Int32
}

func newReferObserveDialog(t *testing.T, status int, reason string) *referObserveDialog {
	t.Helper()

	invite := sip.NewRequest(sip.INVITE, sip.Uri{User: "bob", Host: "127.0.0.1", Port: 5060})
	sipDialog := &sipgo.Dialog{InviteRequest: invite}
	sipDialog.InitWithState(sip.DialogStateConfirmed)
	return &referObserveDialog{
		sipDialog: sipDialog,
		status:    status,
		reason:    reason,
		nextCSeq:  1,
	}
}

func (d *referObserveDialog) DialogSIP() *sipgo.Dialog { return d.sipDialog }

func (d *referObserveDialog) Hangup(ctx context.Context) error {
	d.hangups.Add(1)
	return nil
}

func (d *referObserveDialog) Do(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if req.CSeq() == nil {
		req.AppendHeader(&sip.CSeqHeader{SeqNo: d.nextCSeq, MethodName: sip.REFER})
	}
	d.refersSent.Add(1)
	if d.onDo != nil {
		d.onDo(req)
	}
	if d.doErr != nil {
		return nil, d.doErr
	}
	return sip.NewResponseFromRequest(req, d.status, d.reason, nil), nil
}

// sendReferNotify hands d a REFER NOTIFY carrying the sipfrag body, plus the
// Event and Subscription-State headers when they are not empty, and returns the
// status of the response it was answered with.
func sendReferNotify(t *testing.T, d referDialog, body, event, subscriptionState string) int {
	t.Helper()

	req := newReferNotifyRequest(t, "message/sipfrag", body)
	if event != "" {
		req.AppendHeader(sip.NewHeader("Event", event))
	}
	if subscriptionState != "" {
		req.AppendHeader(sip.NewHeader("Subscription-State", subscriptionState))
	}
	// Tests also send NOTIFYs from their own goroutines, so an error is
	// reported with Errorf rather than asserted.
	tx, conn, err := buildReferNotifyTx(req)
	if err != nil {
		t.Errorf("building a REFER NOTIFY transaction: %v", err)
		return 0
	}

	dialogRouteReferNotify(t.Context(), d, req, tx, d.referNotifyCallback())

	msgs := conn.messages()
	if len(msgs) != 1 {
		t.Errorf("a REFER NOTIFY was answered %d times, want once", len(msgs))
		return 0
	}
	res, ok := msgs[0].(*sip.Response)
	if !ok {
		t.Errorf("a REFER NOTIFY was answered with %T, want a response", msgs[0])
		return 0
	}
	return res.StatusCode
}

// TestReferSubscriptionBound checks the duration referSubscriptionBound gives a
// NOTIFY that did not end the subscription: its Subscription-State's expires (RFC
// 6665 section 4.1.3), capped at one hour, or 64×T1 when it carries none that
// parses as delta-seconds, so a missing or malformed value never leaves an
// attempt unbounded.
func TestReferSubscriptionBound(t *testing.T) {
	timerN := 64 * sip.T1
	for _, tt := range []struct {
		name  string
		state string
		has   bool
		want  time.Duration
	}{
		{name: "active with expires", state: "active;expires=60", has: true, want: 60 * time.Second},
		{name: "pending with expires", state: "pending;expires=5", has: true, want: 5 * time.Second},
		{name: "expires among other parameters", state: "active;reason=probation;expires=30", has: true, want: 30 * time.Second},
		{name: "expires in capitals with spaces", state: "active; EXPIRES = 30", has: true, want: 30 * time.Second},
		{name: "an expires of zero", state: "active;expires=0", has: true, want: 0},
		{name: "an expires one second below the cap", state: "active;expires=3599", has: true, want: 3599 * time.Second},
		{name: "an expires at the cap", state: "active;expires=3600", has: true, want: time.Hour},
		{name: "an expires one second above the cap", state: "active;expires=3601", has: true, want: time.Hour},
		{name: "the largest 32-bit expires", state: "active;expires=4294967295", has: true, want: time.Hour},
		{name: "no expires", state: "active", has: true, want: timerN},
		{name: "an empty expires", state: "active;expires=", has: true, want: timerN},
		{name: "a negative expires", state: "active;expires=-1", has: true, want: timerN},
		{name: "a non-numeric expires", state: "active;expires=soon", has: true, want: timerN},
		{name: "an expires past 32 bits", state: "active;expires=4294967296", has: true, want: timerN},
		{name: "an unknown state without expires", state: "waiting", has: true, want: timerN},
		{name: "no Subscription-State", has: false, want: timerN},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			n := ReferNotify{Status: 100, SubscriptionState: tt.state, HasSubscriptionState: tt.has}
			assert.Equal(t, tt.want, referSubscriptionBound(n))
		})
	}
}

// TestDialogNotifyForNoRefer pins how a NOTIFY on a live dialog is answered
// when the dialog sent no REFER it belongs to, on either side. It matches none
// of the dialog's subscriptions, and RFC 6665 section 4.1.3 has it answered
// 481. It was answered 200. A NOTIFY for a REFER the dialog tracks is still
// answered 200.
func TestDialogNotifyForNoRefer(t *testing.T) {
	sides := []struct {
		name    string
		newLive func(t *testing.T) inDialogHandlers
	}{
		{
			name: "inbound",
			newLive: func(t *testing.T) inDialogHandlers {
				d := newTestDialogOverUA(t, newByeServerTx(), &sipgo.DialogUA{})
				d.InviteResponse = sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
				confirm(t, d)
				return d
			},
		},
		{
			name: "outbound",
			newLive: func(t *testing.T) inDialogHandlers {
				d, _ := newAnsweredTestClientDialog(t)
				return d
			},
		},
	}
	newNotify := func() *sip.Request {
		req := newPeerInDialogRequest(sip.NOTIFY, 500)
		req.AppendHeader(sip.NewHeader("Event", "refer;id=7"))
		req.AppendHeader(sip.NewHeader("Subscription-State", "terminated;reason=noresource"))
		req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
		req.SetBody([]byte("SIP/2.0 200 OK"))
		return req
	}
	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			t.Run("no REFER", func(t *testing.T) {
				d := side.newLive(t)
				tx := newByeServerTx()
				d.handleReferNotify(t.Context(), newNotify(), tx)
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
			})

			t.Run("tracked REFER", func(t *testing.T) {
				d := side.newLive(t)
				attempt := d.sipState().beginReferAttempt()
				d.sipState().setReferAttemptCSeq(attempt, 7)
				tx := newByeServerTx()
				d.handleReferNotify(t.Context(), newNotify(), tx)
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusOK, tx.responses[0].StatusCode)
			})
		})
	}
}
