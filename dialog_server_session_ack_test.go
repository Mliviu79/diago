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
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readAck reads the ACK to our 2xx. Unlike confirm it does not check the state
// afterwards: a request waiting for the ACK may already have moved it on.
func readAck(t *testing.T, d *DialogServerSession) {
	t.Helper()
	ack := sip.NewRequest(sip.ACK, d.InviteRequest.Contact().Address)
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: d.InviteRequest.CSeq().SeqNo, MethodName: sip.ACK})
	require.NoError(t, d.ReadAck(ack, newByeServerTx()))
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

func (tx *inviteServerTx) Respond(_ context.Context, res *sip.Response) error {
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
	// answeredWith returns a dialog whose 2xx, which answer sends, is out and
	// awaits its ACK, the transaction it went out on, what the dialog sends,
	// and answer's result.
	answeredWith := func(t *testing.T, answer func(d *DialogServerSession) error) (*DialogServerSession, *inviteServerTx, *sentRequests, <-chan error) {
		t.Helper()
		ua, sent := newRecordingDialogUA(t)
		inviteTx := newInviteServerTx()
		d := newTestDialogOverUA(t, inviteTx, ua)
		accepted := make(chan error, 1)
		go func() { accepted <- answer(d) }()
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
	// answered returns what answeredWith does for a 2xx without a body.
	answered := func(t *testing.T) (*DialogServerSession, *inviteServerTx, *sentRequests, <-chan error) {
		t.Helper()
		return answeredWith(t, func(d *DialogServerSession) error {
			res := sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
			res.AppendHeader(&sip.ContactHeader{Address: d.InviteRequest.Recipient})
			return d.DialogServerSession.WriteResponse(t.Context(), res)
		})
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
		d, inviteTx, sent, accepted := answeredWith(t, func(d *DialogServerSession) error {
			d.mediaConf = MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}, bindIP: net.IPv4(127, 0, 0, 1)}
			_, err := d.AnswerLate()
			return err
		})
		t.Cleanup(func() { _ = d.Close() })
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
