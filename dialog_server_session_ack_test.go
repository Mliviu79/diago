// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
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

// sendOK sends our 2xx through RespondSDP on another goroutine and returns
// once it is out. Like every answer, it marks itself in progress, which holds
// in-dialog requests until it has returned, and waits for the ACK; its result
// arrives on the channel.
func sendOK(t *testing.T, d *DialogServerSession, inviteTx *sentServerTx) <-chan error {
	t.Helper()

	answered := make(chan error, 1)
	go func() {
		answered <- d.RespondSDP(nil)
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
// Either way the 2xx send sees its ACK and returns without error.
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
			case err := <-answered:
				require.NoError(t, err, "the ACK was read, the 2xx send must not report it missing")
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

// TestDialogServerAnswerWithByeBehindAck pins that an answer whose 2xx is
// acknowledged returns without error when a BYE right behind the ACK ends the
// dialog. The ACK and the BYE are read on goroutines of their own, and the
// dialog notifies each state change to its observers one after another, so the
// BYE, woken by the ACK's notification, can end the dialog while that
// notification is still on its way to the answer. The answer then sees the
// dialog end before it sees the ACK, and reports "No ACK received". The
// observer registered here holds the ACK's notification at that point, which
// makes the window certain instead of rare.
func TestDialogServerAnswerWithByeBehindAck(t *testing.T) {
	answers := []struct {
		name   string
		answer func(d *DialogServerSession) error
	}{
		{name: "early media Answer", answer: (*DialogServerSession).Answer},
		{name: "early media AnswerOptions", answer: func(d *DialogServerSession) error {
			return d.AnswerOptions(AnswerOptions{})
		}},
		{name: "RespondSDP", answer: func(d *DialogServerSession) error {
			return d.RespondSDP(d.mediaSession.LocalSDP())
		}},
	}
	for _, tc := range answers {
		t.Run(tc.name, func(t *testing.T) {
			d, inviteTx := newAnswerTestDialog(t)
			sess := &media.MediaSession{
				Codecs: []media.Codec{media.CodecAudioUlaw},
				Mode:   sdp.ModeSendrecv,
				Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
			}
			require.NoError(t, sess.Init())
			t.Cleanup(func() { _ = sess.Close() })
			// Early media: the session exists before the answer.
			d.mediaSession = sess

			answered := make(chan error, 1)
			go func() { answered <- tc.answer(d) }()
			select {
			case <-inviteTx.sent:
			case <-time.After(5 * time.Second):
				t.Fatal("the 2xx was never sent")
			}

			// Registered after the answer's own observer and before the BYE's,
			// so it runs between the two. It holds the ACK's notification until
			// the dialog has ended, or for a bounded time when the BYE waits.
			d.OnState(func(state sip.DialogState) {
				if state != sip.DialogStateConfirmed {
					return
				}
				select {
				case <-d.Context().Done():
				case <-time.After(300 * time.Millisecond):
				}
			})

			bye := newBye(t, d, d.InviteRequest.CSeq().SeqNo+1)
			res := handleEarly(t, func(tx sip.ServerTransaction) error {
				return d.ReadBye(bye, tx)
			}, func() {
				readAck(t, d)
			})
			assert.Equal(t, sip.StatusOK, res.StatusCode)

			select {
			case err := <-answered:
				assert.NoError(t, err, "the ACK was read, the answer must not report it missing")
			case <-time.After(5 * time.Second):
				t.Fatal("the answer did not return")
			}
			assert.Equal(t, sip.DialogStateEnded, d.LoadState())
		})
	}
}

// inviteServerTx is an INVITE transaction that records the status of every
// response it is asked to send, and closes sent on the first.
type inviteServerTx struct {
	*byeServerTx
	mu       sync.Mutex
	statuses []int
	once     sync.Once
	sent     chan struct{}
}

func newInviteServerTx() *inviteServerTx {
	return &inviteServerTx{byeServerTx: newByeServerTx(), sent: make(chan struct{})}
}

func (tx *inviteServerTx) Respond(res *sip.Response) error {
	tx.mu.Lock()
	tx.statuses = append(tx.statuses, res.StatusCode)
	tx.mu.Unlock()
	tx.once.Do(func() { close(tx.sent) })
	return nil
}

// finalStatuses returns the final responses sent, retransmissions included.
func (tx *inviteServerTx) finalStatuses() []int {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	var final []int
	for _, status := range tx.statuses {
		if status >= 200 {
			final = append(final, status)
		}
	}
	return final
}

// waitingCtx is a context that closes waiting the first time something
// selects on it.
type waitingCtx struct {
	context.Context
	once    sync.Once
	waiting chan struct{}
}

func (c *waitingCtx) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

// TestDialogServerHangupAnswered pins how Hangup ends an inbound call whose
// 2xx has been sent. The INVITE transaction has accepted the call and sends no
// other final response, so the call cannot be declined any more: it is ended
// with a BYE, sent once the ACK is read or the transaction has timed out
// without one (RFC 3261 section 15). A call not answered yet is declined 480.
func TestDialogServerHangupAnswered(t *testing.T) {
	// answered returns a dialog whose 2xx is out and awaits its ACK, the
	// transaction it went out on, what the dialog sends, and the 2xx send's
	// result.
	answered := func(t *testing.T) (*DialogServerSession, *inviteServerTx, *sentRequests, <-chan error) {
		t.Helper()
		ua, sent := newRecordingDialogUA(t)
		inviteTx := newInviteServerTx()
		d := newTestDialogOverUA(t, inviteTx, ua)
		res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
		res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
		accepted := make(chan error, 1)
		go func() { accepted <- d.DialogServerSession.WriteResponse(res) }()
		select {
		case <-inviteTx.sent:
		case <-time.After(5 * time.Second):
			t.Fatal("the 2xx was never sent")
		}
		// Ends the transaction when the test does, so a Hangup still waiting
		// on it returns.
		t.Cleanup(func() {
			select {
			case <-inviteTx.done:
			default:
				close(inviteTx.done)
			}
		})
		return d, inviteTx, sent, accepted
	}
	hangup := func(ctx context.Context, d *DialogServerSession) <-chan error {
		hungUp := make(chan error, 1)
		go func() { hungUp <- d.Hangup(ctx) }()
		return hungUp
	}
	requireReturned := func(t *testing.T, ch <-chan error, what string) error {
		t.Helper()
		select {
		case err := <-ch:
			return err
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not return", what)
			return nil
		}
	}

	t.Run("ACK read", func(t *testing.T) {
		d, inviteTx, sent, accepted := answered(t)
		base, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ctx := &waitingCtx{Context: base, waiting: make(chan struct{})}

		hungUp := hangup(ctx, d)
		select {
		case <-ctx.waiting:
		case <-time.After(2 * time.Second):
			t.Errorf("Hangup did not wait for the ACK; the INVITE got final responses %v", inviteTx.finalStatuses())
		}
		readAck(t, d)

		assert.NoError(t, requireReturned(t, hungUp, "Hangup"))
		assert.NoError(t, requireReturned(t, accepted, "the 2xx send"), "the ACK was read")
		assert.Equal(t, []sip.RequestMethod{sip.BYE}, sent.sent(), "the answered call was not ended with a BYE")
		assert.NotContains(t, inviteTx.finalStatuses(), sip.StatusTemporarilyUnavailable, "an accepted INVITE was declined")
		assert.Equal(t, sip.DialogStateEnded, d.LoadState())
	})

	t.Run("transaction timed out", func(t *testing.T) {
		d, inviteTx, sent, accepted := answered(t)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		hungUp := hangup(ctx, d)
		// No ACK comes, and the transaction ends as Timer L ends it.
		close(inviteTx.done)

		assert.NoError(t, requireReturned(t, hungUp, "Hangup"))
		assert.Equal(t, []sip.RequestMethod{sip.BYE}, sent.sent(), "the answered call was not ended with a BYE")
		assert.NotContains(t, inviteTx.finalStatuses(), sip.StatusTemporarilyUnavailable, "an accepted INVITE was declined")
		assert.Equal(t, sip.DialogStateEnded, d.LoadState())
		requireReturned(t, accepted, "the 2xx send")
	})

	t.Run("late offer answer refused", func(t *testing.T) {
		// The ACK carries the answer to the offer in our 2xx, and one that
		// cannot be applied ends the call. The ACK is still read first, or the
		// BYE would wait for it until the 2xx gave up.
		d, inviteTx, sent, accepted := answered(t)
		newTestMediaSession(t, &d.DialogMedia)
		ack := sip.NewRequest(sip.ACK, d.InviteRequest.Contact().Address)
		ack.AppendHeader(&sip.CSeqHeader{SeqNo: d.InviteRequest.CSeq().SeqNo, MethodName: sip.ACK})
		ack.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		ack.SetBody([]byte("v=0\r\n"))

		read := make(chan error, 1)
		go func() { read <- d.ReadAck(ack, newByeServerTx()) }()

		assert.Error(t, requireReturned(t, read, "ReadAck"), "the answer could not be applied")
		assert.Equal(t, []sip.RequestMethod{sip.BYE}, sent.sent(), "the call was not ended with a BYE")
		assert.NotContains(t, inviteTx.finalStatuses(), sip.StatusTemporarilyUnavailable, "an accepted INVITE was declined")
		assert.Equal(t, sip.DialogStateEnded, d.LoadState())
		requireReturned(t, accepted, "the 2xx send")
	})

	t.Run("not answered", func(t *testing.T) {
		ua, sent := newRecordingDialogUA(t)
		inviteTx := newInviteServerTx()
		d := newTestDialogOverUA(t, inviteTx, ua)

		hungUp := hangup(context.Background(), d)
		select {
		case <-inviteTx.sent:
		case <-time.After(5 * time.Second):
			t.Fatal("the call was never declined")
		}
		// The transaction ends once the decline is acknowledged.
		close(inviteTx.done)

		assert.NoError(t, requireReturned(t, hungUp, "Hangup"))
		assert.Equal(t, []int{sip.StatusTemporarilyUnavailable}, inviteTx.finalStatuses())
		assert.Empty(t, sent.sent())
	})
}
