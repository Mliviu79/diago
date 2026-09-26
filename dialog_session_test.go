// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
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

// inDialogHandlers are the handlers diago runs for INFO, REFER and NOTIFY on a
// dialog found in the cache, on either side.
type inDialogHandlers interface {
	DialogSession
	readSIPInfoDTMF(req *sip.Request, tx sip.ServerTransaction) error
	handleRefer(dg *Diago, req *sip.Request, tx sip.ServerTransaction)
	handleReferNotify(req *sip.Request, tx sip.ServerTransaction)
}

// sentRequests records the methods of the requests a dialog sends through a
// client built by newRecordingDialogUA.
type sentRequests struct {
	mu      sync.Mutex
	methods []sip.RequestMethod
}

func (s *sentRequests) record(req *sip.Request) *sip.Response {
	s.mu.Lock()
	s.methods = append(s.methods, req.Method)
	s.mu.Unlock()
	return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
}

func (s *sentRequests) reset() {
	s.mu.Lock()
	s.methods = nil
	s.mu.Unlock()
}

func (s *sentRequests) sent() []sip.RequestMethod {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sip.RequestMethod(nil), s.methods...)
}

// newRecordingDialogUA returns a DialogUA whose client answers every request
// 200 without a transport, and what it was asked to send.
func newRecordingDialogUA(t *testing.T) (*sipgo.DialogUA, *sentRequests) {
	t.Helper()
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	require.NoError(t, err)
	sent := &sentRequests{}
	client.TxRequester = &clientTxRequester{onRequest: sent.record}
	return &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "dg", Host: "127.0.0.1", Port: 5060}}}, sent
}

// peerSDP is the session description the peer of newAnsweredTestClientDialog
// answers and offers with.
func peerSDP() []byte {
	return sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ULAW})
}

// newAnsweredTestClientDialog returns an outgoing call, invited, answered with
// peerSDP and acknowledged, over a client that answers every request 200
// without a transport, and what the dialog sent after the ACK.
func newAnsweredTestClientDialog(t *testing.T) (*DialogClientSession, *sentRequests) {
	t.Helper()
	sent := &sentRequests{}
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		res := sent.record(req)
		if req.IsInvite() {
			res.SetBody(peerSDP())
		}
		return res
	})
	ctx := context.Background()
	d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	require.NoError(t, d.Invite(ctx, InviteClientOptions{}))
	require.NoError(t, d.Ack(ctx))
	sent.reset()
	return d, sent
}

// newPeerInDialogRequest builds a request of the given method the peer sends
// inside a dialog.
func newPeerInDialogRequest(method sip.RequestMethod, seq uint32) *sip.Request {
	peer := sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}
	fromParams := sip.NewParams()
	fromParams.Add("tag", "peer-tag")
	toParams := sip.NewParams()
	toParams.Add("tag", "our-tag")
	req := sip.NewRequest(method, sip.Uri{User: "dg", Host: "127.0.0.1", Port: 5060})
	req.AppendHeader(&sip.FromHeader{Address: peer, Params: fromParams})
	req.AppendHeader(&sip.ToHeader{Address: sip.Uri{User: "dg", Host: "127.0.0.1"}, Params: toParams})
	req.AppendHeader(sip.NewHeader("Call-ID", "ended-dialog-request-test"))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: method})
	req.AppendHeader(&sip.ContactHeader{Address: peer})
	return req
}

// TestDialogEndedAnswersInDialogRequests481 pins how INFO, REFER and NOTIFY are
// answered on a dialog that has ended, on either side. An ended dialog stays in
// the cache until the call handler returns, for an inbound call, or until it is
// closed, for an outbound one, so these requests still find it. INFO belongs to
// the INVITE usage the BYE ended, and a REFER asks to transfer a call that is
// gone, so both match no live dialog and are answered 481 (RFC 3261 section
// 12.2.2), without the REFER reaching the transfer. A BYE does not end the
// subscription usage a REFER created, and the dialog lives on until its last
// usage ends (RFC 5057 sections 2 and 4.1), so a NOTIFY for a REFER the dialog
// still tracks is taken as on a live dialog, and any other is answered 481.
func TestDialogEndedAnswersInDialogRequests481(t *testing.T) {
	sides := []struct {
		name string
		// newEnded returns an ended dialog with a transfer callback that
		// records its calls, and what the dialog sent.
		newEnded func(t *testing.T, onRefer OnReferDialogFunc) (inDialogHandlers, *sentRequests)
	}{
		{
			name: "inbound",
			newEnded: func(t *testing.T, onRefer OnReferDialogFunc) (inDialogHandlers, *sentRequests) {
				ua, sent := newRecordingDialogUA(t)
				d := newTestDialogOverUA(t, newByeServerTx(), ua)
				d.InviteResponse = sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
				confirm(t, d)
				d.onReferDialog = onRefer
				require.NoError(t, d.ReadBye(newBye(t, d, d.InviteRequest.CSeq().SeqNo+1), newByeServerTx()))
				require.Equal(t, sip.DialogStateEnded, d.LoadState())
				return d, sent
			},
		},
		{
			name: "outbound",
			newEnded: func(t *testing.T, onRefer OnReferDialogFunc) (inDialogHandlers, *sentRequests) {
				d, sent := newAnsweredTestClientDialog(t)
				d.onReferDialog = onRefer
				require.NoError(t, d.Hangup(context.Background()))
				require.Equal(t, sip.DialogStateEnded, d.LoadState())
				sent.reset()
				return d, sent
			},
		},
	}

	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			t.Run("INFO", func(t *testing.T) {
				d, _ := side.newEnded(t, nil)
				req := newPeerInDialogRequest(sip.INFO, 500)
				req.AppendHeader(sip.NewHeader("Content-Type", "application/dtmf-relay"))
				req.SetBody([]byte("Signal=1\r\nDuration=160\r\n"))

				tx := newByeServerTx()
				require.NoError(t, d.readSIPInfoDTMF(req, tx))
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
			})

			t.Run("REFER", func(t *testing.T) {
				var transfers atomic.Int32
				d, sent := side.newEnded(t, func(*DialogClientSession) error {
					transfers.Add(1)
					return nil
				})
				ua, err := sipgo.NewUA()
				require.NoError(t, err)
				t.Cleanup(func() { _ = ua.Close() })
				dg := NewDiago(ua)

				req := newPeerInDialogRequest(sip.REFER, 500)
				req.AppendHeader(sip.NewHeader("Refer-To", "<sip:carol@127.0.0.1:5080>"))

				tx := newByeServerTx()
				d.handleRefer(dg, req, tx)
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
				assert.Empty(t, sent.sent(), "the REFER on an ended dialog sent a NOTIFY")
				assert.Zero(t, transfers.Load(), "the REFER on an ended dialog reached the transfer")
			})

			t.Run("NOTIFY for no REFER", func(t *testing.T) {
				d, _ := side.newEnded(t, nil)
				req := newPeerInDialogRequest(sip.NOTIFY, 500)
				req.AppendHeader(sip.NewHeader("Event", "refer"))
				req.AppendHeader(sip.NewHeader("Subscription-State", "terminated;reason=noresource"))
				req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
				req.SetBody([]byte("SIP/2.0 200 OK"))

				tx := newByeServerTx()
				d.handleReferNotify(req, tx)
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, tx.responses[0].StatusCode)
			})

			t.Run("NOTIFY for a tracked REFER", func(t *testing.T) {
				d, _ := side.newEnded(t, nil)
				attempt := d.Media().beginReferAttempt(nil)
				d.Media().setReferAttemptCSeq(attempt, 7)

				req := newPeerInDialogRequest(sip.NOTIFY, 500)
				req.AppendHeader(sip.NewHeader("Event", "refer;id=7"))
				req.AppendHeader(sip.NewHeader("Subscription-State", "terminated;reason=noresource"))
				req.AppendHeader(sip.NewHeader("Content-Type", "message/sipfrag;version=2.0"))
				req.SetBody([]byte("SIP/2.0 200 OK"))

				tx := newByeServerTx()
				d.handleReferNotify(req, tx)
				require.Len(t, tx.responses, 1)
				assert.Equal(t, sip.StatusOK, tx.responses[0].StatusCode)
				d.Media().referMu.Lock()
				end := attempt.end
				d.Media().referMu.Unlock()
				assert.Equal(t, ReferEndFinalNotify, end, "the transfer outcome was not delivered")
			})
		})
	}
}

// answerLog records, across transactions, the order in which requests are
// answered.
type answerLog struct {
	mu      sync.Mutex
	answers []string
}

func (l *answerLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.answers...)
}

// loggedServerTx is a byeServerTx that writes each response to a shared log
// under its request's name.
type loggedServerTx struct {
	*byeServerTx
	name string
	log  *answerLog
}

func newLoggedServerTx(name string, log *answerLog) *loggedServerTx {
	return &loggedServerTx{byeServerTx: newByeServerTx(), name: name, log: log}
}

func (tx *loggedServerTx) Respond(res *sip.Response) error {
	tx.log.mu.Lock()
	tx.log.answers = append(tx.log.answers, fmt.Sprintf("%s %d", tx.name, res.StatusCode))
	tx.log.mu.Unlock()
	return nil
}

// TestDialogReInviteRacingBye pins how the peer's BYE is handled while its
// re-INVITE is pending, on either side. sipgo hands each request to its handler
// on its own goroutine. The re-INVITE here carries an offer, and its media
// update callback reads the BYE and holds the re-INVITE until the BYE is
// answered. RFC 3261 section 15.1.2 has the BYE answered and every pending
// request answered too, 487 recommended. So the BYE is answered 200 while the
// re-INVITE is still pending, which gets 487, and never a 200 after the BYE's.
func TestDialogReInviteRacingBye(t *testing.T) {
	sides := []struct {
		name string
		// setup returns a live dialog's media and the handling of the
		// peer's re-INVITE, with an offer, and of its BYE.
		setup func(t *testing.T) (*DialogMedia, func(sip.ServerTransaction) error, func(sip.ServerTransaction) error)
	}{
		{
			name: "inbound",
			setup: func(t *testing.T) (*DialogMedia, func(sip.ServerTransaction) error, func(sip.ServerTransaction) error) {
				d, _ := newByeTestDialog(t)
				res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
				res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
				d.InviteResponse = res
				confirm(t, d)

				peer := newMediaSessionForTest(t)
				sess := newMediaSessionForTest(t)
				require.NoError(t, sess.RemoteSDP(peer.LocalSDP()))
				rtpSess := media.NewRTPSession(sess)
				d.mu.Lock()
				d.initRTPSessionUnsafe(sess, rtpSess)
				d.mu.Unlock()
				require.NoError(t, rtpSess.MonitorBackground())
				t.Cleanup(func() { _ = d.DialogMedia.Close() })

				seq := d.InviteRequest.CSeq().SeqNo
				reInvite := newInDialogReInvite(t, d, seq+1)
				reInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
				reInvite.SetBody(peer.LocalSDP())
				bye := newBye(t, d, seq+2)
				return &d.DialogMedia,
					func(tx sip.ServerTransaction) error { return d.handleReInvite(reInvite, tx) },
					func(tx sip.ServerTransaction) error { return d.ReadBye(bye, tx) }
			},
		},
		{
			name: "outbound",
			setup: func(t *testing.T) (*DialogMedia, func(sip.ServerTransaction) error, func(sip.ServerTransaction) error) {
				d, _ := newAnsweredTestClientDialog(t)

				seq := d.InviteRequest.CSeq().SeqNo
				reInvite := newPeerReInvite(d, seq+1)
				reInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
				reInvite.SetBody(peerSDP())
				bye := newPeerInDialogRequest(sip.BYE, seq+2)
				return &d.DialogMedia,
					func(tx sip.ServerTransaction) error { return d.handleReInvite(reInvite, tx) },
					func(tx sip.ServerTransaction) error { return d.ReadBye(bye, tx) }
			},
		},
	}

	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			m, handleReInvite, readBye := side.setup(t)
			log := &answerLog{}

			var byeErr error
			byeDone := make(chan struct{})
			byeAnswered := make(chan bool, 1)
			m.mu.Lock()
			m.onMediaUpdate = func(*DialogMedia) {
				go func() {
					defer close(byeDone)
					byeErr = readBye(newLoggedServerTx("BYE", log))
				}()
				select {
				case <-byeDone:
					byeAnswered <- true
				case <-time.After(5 * time.Second):
					byeAnswered <- false
				}
			}
			m.mu.Unlock()

			require.NoError(t, handleReInvite(newLoggedServerTx("re-INVITE", log)))
			require.True(t, <-byeAnswered, "the BYE waited for the re-INVITE")
			require.NoError(t, byeErr)
			assert.Equal(t, []string{"BYE 200", "re-INVITE 487"}, log.list())
		})
	}
}

// TestDialogAckAnswerAppliedOnFork is a re-INVITE of the peer's without an
// offer, on either side, while our media is written. Our 2xx carries an offer,
// and the ACK carries the answer, which moves the peer's RTP to another port.
// The answer is applied to the fork that made the offer, which is installed,
// and the session that carried the media is left as it was: applied to it in
// place, the answer rewrote its codecs and remote address while the writer
// read them.
func TestDialogAckAnswerAppliedOnFork(t *testing.T) {
	sides := []struct {
		name string
		// setup returns a live dialog's media, a re-INVITE of the peer's
		// without an offer, and the handling of it and of the ACK.
		setup func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error, func(*sip.Request) error)
	}{
		{
			name: "inbound",
			setup: func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error, func(*sip.Request) error) {
				d, _ := newByeTestDialog(t)
				res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
				res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
				d.InviteResponse = res
				confirm(t, d)

				peer := newMediaSessionForTest(t)
				sess := newMediaSessionForTest(t)
				require.NoError(t, sess.RemoteSDP(peer.LocalSDP()))
				rtpSess := media.NewRTPSession(sess)
				d.mu.Lock()
				d.initRTPSessionUnsafe(sess, rtpSess)
				d.mu.Unlock()
				require.NoError(t, rtpSess.MonitorBackground())
				t.Cleanup(func() { _ = d.DialogMedia.Close() })

				return &d.DialogMedia, newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1), d.handleReInvite,
					func(ack *sip.Request) error { return d.ReadAck(ack, newByeServerTx()) }
			},
		},
		{
			name: "outbound",
			setup: func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error, func(*sip.Request) error) {
				d, _ := newAnsweredTestClientDialog(t)
				return &d.DialogMedia, newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1), d.handleReInvite,
					func(ack *sip.Request) error { return d.handleReInviteACK(ack, newByeServerTx()) }
			},
		},
	}

	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			m, reInvite, handleReInvite, readAck := side.setup(t)
			orig := m.MediaSession()
			origPort := orig.Raddr.Port
			m.mu.Lock()
			w := m.RTPPacketWriter
			m.mu.Unlock()

			stop := make(chan struct{})
			written := make(chan struct{})
			go func() {
				defer close(written)
				payload := make([]byte, 160)
				for {
					select {
					case <-stop:
						return
					default:
					}
					_, _ = w.Write(payload)
				}
			}()
			stopWriter := func() {
				close(stop)
				select {
				case <-written:
				case <-time.After(5 * time.Second):
					t.Fatal("the writer did not stop")
				}
			}

			tx := newRespondedServerTx()
			require.NoError(t, handleReInvite(reInvite, tx))
			var res *sip.Response
			select {
			case res = <-tx.responded:
			case <-time.After(5 * time.Second):
				stopWriter()
				t.Fatal("the re-INVITE was not answered")
			}
			require.Equal(t, sip.StatusOK, res.StatusCode, "reason: %s", res.Reason)

			answerer := newMediaSessionForTest(t)
			require.NoError(t, answerer.RemoteSDP(res.Body()))
			ack := sip.NewRequest(sip.ACK, reInvite.Recipient)
			ack.AppendHeader(&sip.CSeqHeader{SeqNo: reInvite.CSeq().SeqNo, MethodName: sip.ACK})
			ack.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			ack.SetBody(answerer.LocalSDP())
			err := readAck(ack)
			stopWriter()
			require.NoError(t, err)

			cur := m.MediaSession()
			require.False(t, orig == cur, "the answer was not applied to a fork")
			assert.Equal(t, origPort, orig.Raddr.Port, "the answer rewrote the session carrying the media")
			assert.Equal(t, answerer.Laddr.Port, cur.Raddr.Port, "the answer was not applied")
		})
	}
}

// reInviteSides builds, for each side of a call, a confirmed dialog with media
// installed whose re-INVITEs the peer answers with onRequest, and its
// ReInvite.
var reInviteSides = []struct {
	name string
	// setup returns the dialog's media, with sess installed, and its
	// ReInvite.
	setup func(t *testing.T, sess *media.MediaSession, onRequest func(*sip.Request) *sip.Response) (*DialogMedia, func(context.Context) error)
}{
	{
		name: "inbound",
		setup: func(t *testing.T, sess *media.MediaSession, onRequest func(*sip.Request) *sip.Response) (*DialogMedia, func(context.Context) error) {
			ua, err := sipgo.NewUA()
			require.NoError(t, err)
			t.Cleanup(func() { _ = ua.Close() })
			client, err := sipgo.NewClient(ua)
			require.NoError(t, err)
			client.TxRequester = &clientTxRequester{onRequest: onRequest}
			d := newTestDialogOverUA(t, newByeServerTx(), &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5060}}})
			res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
			res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
			d.InviteResponse = res
			confirm(t, d)
			installTestMedia(t, &d.DialogMedia, sess)
			return &d.DialogMedia, d.ReInvite
		},
	},
	{
		name: "outbound",
		setup: func(t *testing.T, sess *media.MediaSession, onRequest func(*sip.Request) *sip.Response) (*DialogMedia, func(context.Context) error) {
			reInviting := false
			dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
				if reInviting {
					return onRequest(req)
				}
				res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				if req.IsInvite() {
					res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
					res.SetBody(peerSDP())
				}
				return res
			})
			ctx := context.Background()
			d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = d.Close() })
			require.NoError(t, d.Invite(ctx, InviteClientOptions{}))
			require.NoError(t, d.Ack(ctx))
			reInviting = true

			installTestMedia(t, &d.DialogMedia, sess)
			return &d.DialogMedia, d.ReInvite
		},
	},
}

// installTestMedia installs sess in m in place of the media it has, with an RTP
// session whose monitor runs.
func installTestMedia(t *testing.T, m *DialogMedia, sess *media.MediaSession) {
	t.Helper()
	rtpSess := media.NewRTPSession(sess)
	m.mu.Lock()
	old := m.rtpSession
	m.initRTPSessionUnsafe(sess, rtpSess)
	m.mu.Unlock()
	if old != nil {
		require.NoError(t, old.MonitorClose())
	}
	require.NoError(t, rtpSess.MonitorBackground())
	t.Cleanup(func() { _ = m.Close() })
}

// TestDialogReInviteOffersActpass pins the offer of ReInvite on a DTLS-SRTP
// call, on either side: a subsequent offer carries a=setup:actpass (RFC 8842
// section 5.5), whatever role the endpoint plays. The media session in place
// has its role set, passive for the offerer the peer answered active, active
// for the answerer of an actpass offer, and offering from it signalled that
// role.
func TestDialogReInviteOffersActpass(t *testing.T) {
	for _, side := range reInviteSides {
		t.Run(side.name, func(t *testing.T) {
			sess := newDTLSMediaSession(t, testdata.ClientCertificate())
			peer := newDTLSMediaSession(t, testdata.ServerCertificate())
			if side.name == "inbound" {
				require.NoError(t, sess.RemoteSDP(peer.LocalSDP()))
			} else {
				require.NoError(t, peer.RemoteSDP(sess.LocalSDP()))
				sess.RemoteSDPIsAnswer = true
				require.NoError(t, sess.RemoteSDP(peer.LocalSDP()))
			}

			offers := make(chan []byte, 1)
			_, reInvite := side.setup(t, sess, func(req *sip.Request) *sip.Response {
				offers <- req.Body()
				return sip.NewResponseFromRequest(req, sip.StatusNotAcceptableHere, "Not Acceptable Here", nil)
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			err := reInvite(ctx)
			var resErr sipgo.ErrDialogResponse
			require.ErrorAs(t, err, &resErr)
			require.Equal(t, "a=setup:actpass", iceSDPLine(t, <-offers, "a=setup:"))
		})
	}
}

// TestDialogReInviteAppliesAnswer pins that ReInvite applies the answer it
// gets, on either side: here the peer answers from another port, and the
// media then goes there. Before, the answer was dropped and the media kept
// going to the old port.
func TestDialogReInviteAppliesAnswer(t *testing.T) {
	for _, side := range reInviteSides {
		t.Run(side.name, func(t *testing.T) {
			peer := newMediaSessionForTest(t)
			sess := newMediaSessionForTest(t)
			require.NoError(t, sess.RemoteSDP(peer.LocalSDP()))

			moved := newMediaSessionForTest(t)
			m, reInvite := side.setup(t, sess, func(req *sip.Request) *sip.Response {
				res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
				if req.IsInvite() {
					if err := moved.RemoteSDP(req.Body()); err == nil {
						res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
						res.SetBody(moved.LocalSDP())
					}
				}
				return res
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, reInvite(ctx))
			require.Equal(t, moved.Laddr.Port, m.MediaSession().Raddr.Port, "the answer was not applied")
		})
	}
}

// TestDialogReInviteOffersNegotiatedCodecs pins the codecs ReInvite offers, on
// either side: the ones the call runs on, as an offer made from the session in
// place did, so a session refresh does not ask the peer to move the call to
// another codec. The session keeps every codec it was set up with, for a later
// offer of the peer's.
func TestDialogReInviteOffersNegotiatedCodecs(t *testing.T) {
	newSession := func(t *testing.T, codecs ...media.Codec) *media.MediaSession {
		s := &media.MediaSession{Codecs: codecs, Mode: sdp.ModeSendrecv, Laddr: net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}}
		require.NoError(t, s.Init())
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	for _, side := range reInviteSides {
		t.Run(side.name, func(t *testing.T) {
			sess := newSession(t, media.CodecAudioUlaw, media.CodecAudioAlaw)
			peer := newSession(t, media.CodecAudioAlaw)
			if side.name == "inbound" {
				require.NoError(t, sess.RemoteSDP(peer.LocalSDP()))
			} else {
				require.NoError(t, peer.RemoteSDP(sess.LocalSDP()))
				sess.RemoteSDPIsAnswer = true
				require.NoError(t, sess.RemoteSDP(peer.LocalSDP()))
			}
			require.Equal(t, []media.Codec{media.CodecAudioAlaw}, sess.CommonCodecs())

			var offer []byte
			m, reInvite := side.setup(t, sess, func(req *sip.Request) *sip.Response {
				res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
				res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
				if req.IsInvite() {
					offer = req.Body()
					answerer := newSession(t, media.CodecAudioUlaw, media.CodecAudioAlaw)
					if err := answerer.RemoteSDP(offer); err == nil {
						res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
						res.SetBody(answerer.LocalSDP())
					}
				}
				return res
			})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			require.NoError(t, reInvite(ctx))
			assert.Equal(t, fmt.Sprintf("m=audio %d RTP/AVP 8", sess.Laddr.Port), iceSDPLine(t, offer, "m=audio "), "the offer must keep the call's codec")

			cur := m.MediaSession()
			assert.Equal(t, []media.Codec{media.CodecAudioAlaw}, cur.CommonCodecs())
			assert.Equal(t, []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw}, cur.Codecs, "the session lost codecs it was set up with")
		})
	}
}
