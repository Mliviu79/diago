// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// referNotifyDialog is a minimal DialogSession. Only Media and Hangup are
// reached by dialogHandleReferNotify, the rest stays unimplemented.
type referNotifyDialog struct {
	DialogSession
	media   *DialogMedia
	hangups int
}

func (d *referNotifyDialog) Media() *DialogMedia { return d.media }

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
			d := &referNotifyDialog{media: &DialogMedia{}}
			d.media.onReferNotify = func(statusCode int) { notified = statusCode }

			req := newReferNotifyRequest(t, tc.contentType, "SIP/2.0 200 OK")
			tx, conn := newReferNotifyTx(t, req)

			dialogHandleReferNotify(d, req, tx)

			require.Len(t, conn.msgs, 1)
			res, ok := conn.msgs[0].(*sip.Response)
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
	d := &referNotifyDialog{media: &DialogMedia{}}
	req := newReferNotifyRequest(t, "message/sipfrag", "SIP/2.0")
	tx, conn := newReferNotifyTx(t, req)

	dialogHandleReferNotify(d, req, tx)

	require.Len(t, conn.msgs, 1)
	res := conn.msgs[0].(*sip.Response)
	require.Equal(t, sip.StatusBadRequest, res.StatusCode)
}

// TestDialogHandleReferNotifyWithoutContentType checks an in-dialog NOTIFY that
// carries no Content-Type is rejected rather than panicking on the missing
// header, and that the rejection is never recorded as a transfer outcome.
func TestDialogHandleReferNotifyWithoutContentType(t *testing.T) {
	notified := -1
	d := &referNotifyDialog{media: &DialogMedia{}}
	d.media.onReferNotify = func(statusCode int) { notified = statusCode }
	attempt := d.media.beginReferAttempt(nil)

	req := newReferNotifyRequest(t, "", "SIP/2.0 200 OK")
	require.Nil(t, req.ContentType(), "the NOTIFY must reach the handler without Content-Type")
	tx, conn := newReferNotifyTx(t, req)

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		dialogHandleReferNotify(d, req, tx)
	}()
	require.Nil(t, recovered, "dialogHandleReferNotify panicked")

	require.Len(t, conn.msgs, 1)
	res, ok := conn.msgs[0].(*sip.Response)
	require.True(t, ok)
	require.Equal(t, sip.StatusBadRequest, res.StatusCode)
	require.Equal(t, -1, notified, "OnNotify must not fire on a rejected NOTIFY")

	d.media.referMu.Lock()
	notifies, end := len(attempt.notifies), attempt.end
	d.media.referMu.Unlock()
	require.Zero(t, notifies, "a rejected NOTIFY must not be recorded on the REFER attempt")
	require.Zero(t, end, "a rejected NOTIFY must not end the REFER attempt")
	require.Zero(t, d.hangups, "a REFER NOTIFY never ends the dialog")
}

// TestDialogHandleReferNotifyLeavesTheDialogAlone checks a REFER NOTIFY hands
// the result on and never ends the dialog, whoever sent it and whenever it
// arrives. Each row has no Refer waiting and no OnNotify callback, which is the
// shape of a NOTIFY arriving after Refer stopped waiting: what follows a
// transfer result is the dialog owner's decision.
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
			d := &referNotifyDialog{media: &DialogMedia{}}

			req := newReferNotifyRequest(t, "message/sipfrag", tc.body)
			for _, h := range tc.headers {
				req.AppendHeader(h)
			}
			tx, conn := newReferNotifyTx(t, req)

			dialogHandleReferNotify(d, req, tx)

			require.Len(t, conn.msgs, 1)
			res, ok := conn.msgs[0].(*sip.Response)
			require.True(t, ok)
			require.Equal(t, sip.StatusOK, res.StatusCode)
			require.Zero(t, d.hangups, "a %s NOTIFY ended the dialog", tc.name)
		})
	}
}

// referObserveDialog is a DialogSession that answers a REFER with a canned
// response and counts what the REFER path does to the dialog. Its SIP dialog is
// a bare sipgo dialog put in the confirmed state, unless a test changes it.
type referObserveDialog struct {
	DialogSession
	media     *DialogMedia
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
		media:     &DialogMedia{},
		sipDialog: sipDialog,
		status:    status,
		reason:    reason,
		nextCSeq:  1,
	}
}

func (d *referObserveDialog) Media() *DialogMedia { return d.media }

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
func sendReferNotify(t *testing.T, d DialogSession, body, event, subscriptionState string) int {
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

	dialogHandleReferNotify(d, req, tx)

	if len(conn.msgs) != 1 {
		t.Errorf("a REFER NOTIFY was answered %d times, want once", len(conn.msgs))
		return 0
	}
	res, ok := conn.msgs[0].(*sip.Response)
	if !ok {
		t.Errorf("a REFER NOTIFY was answered with %T, want a response", conn.msgs[0])
		return 0
	}
	return res.StatusCode
}
