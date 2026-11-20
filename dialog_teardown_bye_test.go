// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// heldByeAnswer is a transaction layer that hands a BYE to its client
// transaction at once and answers it 200 only once release is closed,
// reporting on inFlight that the BYE was handed over. Every other request goes
// to next.
type heldByeAnswer struct {
	next     sipgo.ClientTransactionRequester
	inFlight chan struct{}
	release  chan struct{}
}

func (r *heldByeAnswer) Request(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
	if req.Method != sip.BYE {
		return r.next.Request(ctx, req)
	}
	key, err := sip.ClientTxKeyMake(req)
	if err != nil {
		return nil, err
	}
	tx := sip.NewClientTx(key, req, NewConnRecorder(), slog.Default())
	if err := tx.Init(); err != nil {
		return nil, err
	}
	close(r.inFlight)
	go func() {
		<-r.release
		tx.Receive(sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil))
	}()
	return tx, nil
}

// TestDialogHangupNoMediaWaitsForByeAnswer ends an answered call left without
// usable media, and holds the 200 to its BYE. The BYE ends the dialog, and its
// context, as it is handed to its transaction (RFC 3261 section 15.1.1); the
// wait for the 200 runs on the ctx of the work that found the media unusable,
// which outlives the dialog, so it goes on past that end and hangupNoMedia
// returns the 200's outcome.
func TestDialogHangupNoMediaWaitsForByeAnswer(t *testing.T) {
	d, _, _ := newAnsweredTestClientCall(t)
	held := &heldByeAnswer{
		next:     d.UA.Client.TxRequester,
		inFlight: make(chan struct{}),
		release:  make(chan struct{}),
	}
	d.UA.Client.TxRequester = held

	hungUp := make(chan error, 1)
	go func() { hungUp <- d.hangupNoMedia(t.Context()) }()
	released := false
	defer func() {
		if !released {
			close(held.release)
		}
	}()

	select {
	case <-held.inFlight:
	case <-time.After(5 * time.Second):
		t.Fatal("the BYE was not sent")
	}
	select {
	case <-d.Context().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the dialog goes on while its BYE is in flight")
	}
	assert.Equal(t, sip.DialogStateEnded, d.LoadState())

	// A wait cut short by the end of the dialog returns before the 200, with
	// that end's error; one that goes on returns the 200's outcome.
	close(held.release)
	released = true
	select {
	case err := <-hungUp:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("hangupNoMedia did not return after the 200")
	}
}
