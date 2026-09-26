// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// respondedServerTx is a byeServerTx that hands each response to a channel, so
// a test can see when a request running on another goroutine is answered.
type respondedServerTx struct {
	*byeServerTx
	responded chan *sip.Response
}

func newRespondedServerTx() *respondedServerTx {
	return &respondedServerTx{byeServerTx: newByeServerTx(), responded: make(chan *sip.Response, 1)}
}

func (tx *respondedServerTx) Respond(res *sip.Response) error {
	tx.responded <- res
	return nil
}

// sentServerTx is a byeServerTx that closes sent when its first response goes
// out, so a test knows the INVITE's 2xx has been sent.
type sentServerTx struct {
	*byeServerTx
	once sync.Once
	sent chan struct{}
}

func (tx *sentServerTx) Respond(res *sip.Response) error {
	tx.once.Do(func() { close(tx.sent) })
	return nil
}

// newAnswerTestDialog builds the dialog of newByeTestDialog over an INVITE
// transaction that reports when our 2xx is sent.
func newAnswerTestDialog(t *testing.T) (*DialogServerSession, *sentServerTx) {
	t.Helper()
	inviteTx := &sentServerTx{byeServerTx: newByeServerTx(), sent: make(chan struct{})}
	return newTestDialogOver(t, inviteTx), inviteTx
}

// newInDialogReInvite builds a body-less re-INVITE inside the dialog.
func newInDialogReInvite(t *testing.T, d *DialogServerSession, seq uint32) *sip.Request {
	t.Helper()

	inv := d.InviteRequest
	req := sip.NewRequest(sip.INVITE, inv.Contact().Address)
	req.AppendHeader(sip.HeaderClone(inv.From()))
	req.AppendHeader(sip.HeaderClone(inv.To()))
	req.AppendHeader(sip.HeaderClone(inv.CallID()))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: sip.INVITE})
	req.AppendHeader(sip.HeaderClone(inv.Contact()))
	return req
}

// sendOK sends our 2xx on another goroutine and returns once it is out. Like
// Answer, the send then waits for the ACK; its result arrives on the channel.
func sendOK(t *testing.T, d *DialogServerSession, inviteTx *sentServerTx) <-chan error {
	t.Helper()

	answered := make(chan error, 1)
	go func() {
		res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
		answered <- d.DialogServerSession.WriteResponse(res)
	}()
	select {
	case <-inviteTx.sent:
	case <-time.After(5 * time.Second):
		t.Fatal("the 2xx was never sent")
	}
	return answered
}

// readAck reads the ACK to our 2xx. Unlike confirm it does not check the state
// afterwards: a request waiting for the ACK may already have moved it on.
func readAck(t *testing.T, d *DialogServerSession) {
	t.Helper()
	ack := sip.NewRequest(sip.ACK, d.InviteRequest.Contact().Address)
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: d.InviteRequest.CSeq().SeqNo, MethodName: sip.ACK})
	require.NoError(t, d.ReadAck(ack, newByeServerTx()))
}

// handleEarly runs handle on another goroutine, calls release once the request
// has had time to be answered, and returns the response the request got. It
// fails the test when the response came before release. A handler that does
// not wait answers within the window; one that does waits two T1 intervals,
// far longer.
func handleEarly(t *testing.T, handle func(tx sip.ServerTransaction) error, release func()) *sip.Response {
	t.Helper()

	tx := newRespondedServerTx()
	handled := make(chan error, 1)
	go func() {
		handled <- handle(tx)
	}()

	var early *sip.Response
	select {
	case early = <-tx.responded:
	case <-time.After(100 * time.Millisecond):
	}
	release()

	res := early
	if res == nil {
		select {
		case res = <-tx.responded:
		case <-time.After(5 * time.Second):
			t.Fatal("the request was never answered")
		}
	}
	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("the request handler did not return")
	}

	if early != nil {
		t.Fatalf("the request was answered %d before the answer was complete", early.StatusCode)
	}
	return res
}

// inDialogRequests are the in-dialog requests that must wait for our answer.
var inDialogRequests = []struct {
	name       string
	newRequest func(t *testing.T, d *DialogServerSession) *sip.Request
	handle     func(d *DialogServerSession, req *sip.Request, tx sip.ServerTransaction) error
	wantState  sip.DialogState
}{
	{
		name: "BYE",
		newRequest: func(t *testing.T, d *DialogServerSession) *sip.Request {
			return newBye(t, d, d.InviteRequest.CSeq().SeqNo+1)
		},
		handle:    (*DialogServerSession).ReadBye,
		wantState: sip.DialogStateEnded,
	},
	{
		name: "re-INVITE",
		newRequest: func(t *testing.T, d *DialogServerSession) *sip.Request {
			return newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
		},
		handle:    (*DialogServerSession).handleReInvite,
		wantState: sip.DialogStateConfirmed,
	},
}

// TestDialogServerRequestBeforeAck pins that an in-dialog request which reaches
// its handler while our 2xx still awaits its ACK is handled after the ACK. The
// peer sends the ACK first, but sipgo runs each request on its own goroutine,
// so the later one can be handled first. A re-INVITE handled then is refused
// 491 although the peer did nothing wrong. A BYE handled then ends the dialog
// ahead of the ACK, and the ACK read after it confirms the ended dialog again.
func TestDialogServerRequestBeforeAck(t *testing.T) {
	for _, tc := range inDialogRequests {
		t.Run(tc.name, func(t *testing.T) {
			d, inviteTx := newAnswerTestDialog(t)
			answered := sendOK(t, d, inviteTx)

			req := tc.newRequest(t, d)
			res := handleEarly(t, func(tx sip.ServerTransaction) error {
				return tc.handle(d, req, tx)
			}, func() {
				readAck(t, d)
			})
			select {
			case <-answered:
			case <-time.After(5 * time.Second):
				t.Fatal("the 2xx send did not return")
			}

			assert.NotEqual(t, sip.StatusRequestPending, res.StatusCode)
			assert.Equal(t, tc.wantState, d.LoadState())
		})
	}
}

// TestDialogServerRequestBeforeAnswerMedia pins that an in-dialog request on a
// confirmed dialog waits for an answer still setting up its media. Once the
// ACK is read the answer finalizes its media session and starts its RTP
// monitor. A re-INVITE handled in between replaces that session, so the answer
// fails to start its monitor, and a fork taken before the finalize misses what
// the finalize sets up, such as DTLS-SRTP keys. A BYE handled in between ends
// the dialog under the answer.
func TestDialogServerRequestBeforeAnswerMedia(t *testing.T) {
	for _, tc := range inDialogRequests {
		t.Run(tc.name, func(t *testing.T) {
			d, inviteTx := newAnswerTestDialog(t)
			answered := sendOK(t, d, inviteTx)
			confirm(t, d)
			select {
			case err := <-answered:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the 2xx send did not return")
			}

			// An answer that has read its ACK and not yet returned.
			answering := make(chan struct{})
			d.mu.Lock()
			d.answering = answering
			d.mu.Unlock()

			req := tc.newRequest(t, d)
			res := handleEarly(t, func(tx sip.ServerTransaction) error {
				return tc.handle(d, req, tx)
			}, func() {
				close(answering)
			})

			assert.NotEqual(t, sip.StatusRequestPending, res.StatusCode)
			assert.Equal(t, tc.wantState, d.LoadState())
		})
	}
}
