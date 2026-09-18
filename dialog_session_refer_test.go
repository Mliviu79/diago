// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseSipfragStatus(t *testing.T) {
	tests := []struct {
		name       string
		frag       string
		wantStatus int
		wantReason string
		wantOK     bool
	}{
		{name: "trying", frag: "SIP/2.0 100 Trying", wantStatus: 100, wantReason: "Trying", wantOK: true},
		{name: "ok", frag: "SIP/2.0 200 OK", wantStatus: 200, wantReason: "OK", wantOK: true},
		{name: "busy", frag: "SIP/2.0 486 Busy Here", wantStatus: 486, wantReason: "Busy Here", wantOK: true},
		{name: "only status line is read", frag: "SIP/2.0 486 Busy Here\r\nVia: SIP/2.0/UDP 10.0.0.1", wantStatus: 486, wantReason: "Busy Here", wantOK: true},
		{name: "no reason phrase", frag: "SIP/2.0 480", wantStatus: 480, wantOK: true},
		{name: "not a status line", frag: "GARBAGE 486 Busy", wantOK: false},
		{name: "non numeric code", frag: "SIP/2.0 4x6 Busy", wantOK: false},
		{name: "code out of range", frag: "SIP/2.0 999 Nope", wantOK: false},
		{name: "code below range", frag: "SIP/2.0 42 Nope", wantOK: false},
		{name: "lowest status accepted", frag: "SIP/2.0 100 Trying", wantStatus: 100, wantReason: "Trying", wantOK: true},
		{name: "highest status accepted", frag: "SIP/2.0 699 Custom", wantStatus: 699, wantReason: "Custom", wantOK: true},
		{name: "one below the lowest status", frag: "SIP/2.0 99 Nope", wantOK: false},
		{name: "one above the highest status class", frag: "SIP/2.0 700 Nope", wantOK: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, reason, ok := parseSipfragStatus(tt.frag)
			assert.Equal(t, tt.wantOK, ok)
			assert.Equal(t, tt.wantStatus, status)
			assert.Equal(t, tt.wantReason, reason)
		})
	}
}

func TestParseReferNotifyEventID(t *testing.T) {
	newReq := func(event string) *sip.Request {
		req := sip.NewRequest(sip.NOTIFY, sip.Uri{Host: "127.0.0.1"})
		if event != "" {
			req.AppendHeader(sip.NewHeader("Event", event))
		}
		return req
	}

	tests := []struct {
		name      string
		event     string
		wantID    uint32
		wantHasID bool
	}{
		{name: "id present", event: "refer;id=42", wantID: 42, wantHasID: true},
		{name: "id with spacing", event: "refer; id=7", wantID: 7, wantHasID: true},
		{name: "no id param", event: "refer", wantHasID: false},
		{name: "no event header", event: "", wantHasID: false},
		{name: "non numeric id", event: "refer;id=abc", wantHasID: false},
		{name: "largest 32 bit id", event: "refer;id=4294967295", wantID: 4294967295, wantHasID: true},
		{name: "id past 32 bits is not wrapped", event: "refer;id=4294967296", wantHasID: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, hasID := parseReferNotifyEventID(newReq(tt.event))
			assert.Equal(t, tt.wantHasID, hasID)
			assert.Equal(t, tt.wantID, id)
		})
	}
}

// TestParseReferSubscriptionState checks the Subscription-State header is kept
// exactly as received, that an absent header is its own fact, and that only
// the state token decides whether the subscription is terminated.
func TestParseReferSubscriptionState(t *testing.T) {
	tests := []struct {
		name           string
		header         string
		wantPresent    bool
		wantTerminated bool
	}{
		{name: "terminated with a reason", header: "terminated;reason=noresource", wantPresent: true, wantTerminated: true},
		{name: "terminated in another case", header: "Terminated", wantPresent: true, wantTerminated: true},
		{name: "terminated with spacing", header: " terminated ; reason=timeout", wantPresent: true, wantTerminated: true},
		{name: "active", header: "active;expires=60", wantPresent: true, wantTerminated: false},
		{name: "pending", header: "pending", wantPresent: true, wantTerminated: false},
		{name: "a reason that reads terminated is not the state", header: "active;reason=terminated", wantPresent: true, wantTerminated: false},
		{name: "header absent", wantPresent: false, wantTerminated: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := sip.NewRequest(sip.NOTIFY, sip.Uri{Host: "127.0.0.1"})
			if tt.wantPresent {
				req.AppendHeader(sip.NewHeader("Subscription-State", tt.header))
			}

			value, present := parseReferSubscriptionState(req)
			assert.Equal(t, tt.wantPresent, present)
			assert.Equal(t, tt.header, value, "the value is kept as received")
			assert.Equal(t, tt.wantTerminated, referSubscriptionTerminated(value))
		})
	}
}

func TestReferFailureErrorMessageHidesSIPInternals(t *testing.T) {
	err := &ReferFailureError{Status: 486, Reason: "Busy Here"}
	assert.Equal(t, "refer transfer failed: 486 Busy Here", err.Error())

	timeout := &ReferFailureError{Status: 0, Reason: "timeout"}
	assert.Equal(t, "refer transfer failed: timeout", timeout.Error())

	// The error reaches callers that surface it; it must never carry routing state.
	for _, banned := range []string{"Via", "Contact", "@", "SIP/2.0"} {
		assert.NotContains(t, err.Error(), banned)
	}
}

// TestReferAttemptRegistry covers how a REFER NOTIFY is routed to the attempt
// it belongs to, one invariant per subtest. The locked invariant for a
// NOTIFY that arrives after an attempt stopped waiting is delivery to that
// attempt as a late notify, and to no other attempt.
func TestReferAttemptRegistry(t *testing.T) {
	final := ReferNotify{Status: 486, Reason: "Busy Here"}
	withID := func(n ReferNotify, id uint32) ReferNotify {
		n.EventID = id
		n.HasEventID = true
		return n
	}

	t.Run("a NOTIFY without an id goes to the most recent attempt", func(t *testing.T) {
		m := &DialogMedia{}
		older := m.beginReferAttempt(nil)
		m.setReferAttemptCSeq(older, 1)
		newer := m.beginReferAttempt(nil)
		m.setReferAttemptCSeq(newer, 2)

		onLate, _ := m.observeReferNotify(final)
		assert.Nil(t, onLate)
		assert.Equal(t, []ReferNotify{final}, newer.notifies)
		assert.Equal(t, ReferEndFinalNotify, newer.end)
		assert.Empty(t, older.notifies)
		assert.Zero(t, older.end)
	})

	t.Run("a matching id delivers", func(t *testing.T) {
		m := &DialogMedia{}
		a := m.beginReferAttempt(nil)
		m.setReferAttemptCSeq(a, 5)

		m.observeReferNotify(withID(final, 5))
		assert.Equal(t, []ReferNotify{withID(final, 5)}, a.notifies)
		assert.Equal(t, ReferEndFinalNotify, a.end)
		assert.Len(t, a.wake, 1, "a final NOTIFY wakes the waiting REFER")
	})

	t.Run("a stale id is never given to a newer attempt", func(t *testing.T) {
		m := &DialogMedia{}
		older := m.beginReferAttempt(nil)
		m.setReferAttemptCSeq(older, 4)
		m.finishReferAttempt(older, ReferEndRefused)
		newer := m.beginReferAttempt(nil)
		m.setReferAttemptCSeq(newer, 5)

		onLate, _ := m.observeReferNotify(withID(final, 4))
		assert.Nil(t, onLate)
		assert.Empty(t, newer.notifies)
		assert.Zero(t, newer.end)
	})

	t.Run("an id arriving before the CSeq is known goes to the most recent attempt", func(t *testing.T) {
		m := &DialogMedia{}
		a := m.beginReferAttempt(nil)

		// A NOTIFY racing ahead of the REFER's response cannot be judged stale.
		m.observeReferNotify(withID(final, 9))
		assert.Equal(t, []ReferNotify{withID(final, 9)}, a.notifies)
		assert.Equal(t, ReferEndFinalNotify, a.end)
	})

	t.Run("a terminal NOTIFY after the wait ended is delivered late to its own attempt", func(t *testing.T) {
		m := &DialogMedia{}
		var lates lateRecorder
		older := m.beginReferAttempt(lates.record)
		m.setReferAttemptCSeq(older, 3)
		obs := m.finishReferAttempt(older, ReferEndDeadline)
		require.Equal(t, ReferEndDeadline, obs.End)
		newer := m.beginReferAttempt(nil)
		m.setReferAttemptCSeq(newer, 4)

		onLate, late := m.observeReferNotify(withID(final, 3))
		require.NotNil(t, onLate, "the late NOTIFY is handed back for its own attempt")
		onLate(late)
		assert.Equal(t, []ReferLateNotify{{CSeq: 3, CSeqKnown: true, Notify: withID(final, 3)}}, lates.calls())
		assert.Empty(t, newer.notifies, "a late NOTIFY is never given to a newer attempt")
		assert.Zero(t, newer.end)

		again, _ := m.observeReferNotify(withID(final, 3))
		assert.Nil(t, again, "a late NOTIFY is delivered once")
	})

	t.Run("a refused REFER decides the end over a NOTIFY that raced ahead of it", func(t *testing.T) {
		m := &DialogMedia{}
		a := m.beginReferAttempt(nil)
		m.observeReferNotify(final)

		obs := m.finishReferAttempt(a, ReferEndRefused)
		assert.Equal(t, ReferEndRefused, obs.End)
		assert.Equal(t, []ReferNotify{final}, obs.Notifies, "the NOTIFY is still recorded")
		assert.Zero(t, registeredReferAttempts(m), "a refused attempt is dropped")
	})

	t.Run("an older attempt never clears a newer one", func(t *testing.T) {
		m := &DialogMedia{}
		older := m.beginReferAttempt(nil)
		newer := m.beginReferAttempt(nil)
		m.dropReferAttempt(older)
		m.finishReferAttempt(older, ReferEndRefused)
		m.setReferAttemptCSeq(older, 8)

		m.observeReferNotify(final)
		assert.Equal(t, []ReferNotify{final}, newer.notifies)
		assert.False(t, older.cseqKnown, "a dropped attempt's CSeq is not recorded")
	})

	t.Run("delivery never blocks", func(t *testing.T) {
		m := &DialogMedia{}
		a := m.beginReferAttempt(nil)
		m.setReferAttemptCSeq(a, 1)

		done := make(chan struct{})
		go func() {
			defer close(done)
			for range 3 {
				m.observeReferNotify(final)
			}
		}()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("observeReferNotify blocked on an attempt nobody is waiting for")
		}
		assert.Len(t, a.wake, 1)
		assert.Equal(t, []ReferNotify{final}, a.notifies, "an ended attempt records nothing more")
	})
}

// lateRecorder collects the late notifies an attempt's OnLate receives.
type lateRecorder struct {
	mu  sync.Mutex
	got []ReferLateNotify
}

func (r *lateRecorder) record(n ReferLateNotify) {
	r.mu.Lock()
	r.got = append(r.got, n)
	r.mu.Unlock()
}

func (r *lateRecorder) calls() []ReferLateNotify {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ReferLateNotify(nil), r.got...)
}

// referNotifySpec is one NOTIFY the far end sends for a REFER.
type referNotifySpec struct {
	body  string
	event string
	state string
}

// notifyAfterRefer has d receive notifies, in order, from a goroutine started
// once the REFER goes out. The returned wait blocks, bounded, until every one
// has been answered, and returns the response statuses.
func notifyAfterRefer(t *testing.T, d *referObserveDialog, notifies ...referNotifySpec) (wait func() []int) {
	t.Helper()

	done := make(chan []int, 1)
	d.onDo = func(*sip.Request) {
		go func() {
			codes := make([]int, 0, len(notifies))
			for _, n := range notifies {
				codes = append(codes, sendReferNotify(t, d, n.body, n.event, n.state))
			}
			done <- codes
		}()
	}
	return func() []int {
		t.Helper()
		select {
		case codes := <-done:
			return codes
		case <-time.After(5 * time.Second):
			t.Fatal("the far end's NOTIFYs were not all answered")
			return nil
		}
	}
}

// referURIs are the recipient, Refer-To and Referred-By the engine tests send.
var (
	referTestRecipient  = sip.Uri{User: "bob", Host: "127.0.0.1", Port: 5060}
	referTestTarget     = sip.Uri{User: "carol", Host: "127.0.0.1", Port: 5070}
	referTestReferredBy = sip.Uri{User: "alice", Host: "127.0.0.1", Port: 5080}
)

func observeRefer(ctx context.Context, d DialogSession, opts ReferObserveOptions) (ReferObservation, *sip.Response, error) {
	return dialogReferObserve(ctx, d, referTestRecipient, referTestTarget, referTestReferredBy, opts)
}

func notifyStatuses(obs ReferObservation) []int {
	var statuses []int
	for _, n := range obs.Notifies {
		statuses = append(statuses, n.Status)
	}
	return statuses
}

func registeredReferAttempts(m *DialogMedia) int {
	m.referMu.Lock()
	defer m.referMu.Unlock()
	return len(m.referAttempts)
}

// TestReferObserveEnds covers each way an observed REFER ends. Every kind is its
// own value, none is a SIP status, and nothing on the path ends the dialog.
func TestReferObserveEnds(t *testing.T) {
	t.Run("the five kinds are distinct, non-zero and named", func(t *testing.T) {
		kinds := map[ReferEnd]string{
			ReferEndFinalNotify:       "final_notify",
			ReferEndRefused:           "refused",
			ReferEndDeadline:          "deadline",
			ReferEndCancelled:         "cancelled",
			ReferEndSubscriptionEnded: "subscription_ended",
		}
		require.Len(t, kinds, 5, "two kinds share a value")
		for kind, slug := range kinds {
			assert.NotZero(t, kind)
			assert.Equal(t, slug, kind.String())
		}
		assert.Equal(t, "ReferEnd(0)", ReferEnd(0).String())
	})

	const long = 5 * time.Second
	const (
		cancelNone = iota
		cancelBefore
		cancelDuringWait
		cancelBeforeResponse
	)
	trying := referNotifySpec{body: "SIP/2.0 100 Trying", event: "refer", state: "active;expires=60"}

	tests := []struct {
		name                 string
		status               int
		reason               string
		deadline             time.Duration
		notifies             []referNotifySpec
		cancel               int
		notConfirmed         bool
		doErr                error
		wantErr              bool
		wantEnd              ReferEnd
		wantResponse         ReferResponse
		wantResponseReceived bool
		wantStatuses         []int
		wantSent             int32
		within               time.Duration
	}{
		{
			name: "a final NOTIFY in time ends as final_notify", status: 202, reason: "Accepted", deadline: long,
			notifies: []referNotifySpec{trying, {body: "SIP/2.0 486 Busy Here", event: "refer", state: "terminated;reason=noresource"}},
			wantEnd:  ReferEndFinalNotify, wantResponse: ReferResponse{202, "Accepted"}, wantResponseReceived: true,
			wantStatuses: []int{100, 486}, wantSent: 1, within: time.Second,
		},
		{
			name: "a REFER answered 403 is refused at once", status: 403, reason: "Forbidden", deadline: long,
			wantEnd: ReferEndRefused, wantResponse: ReferResponse{403, "Forbidden"}, wantResponseReceived: true,
			wantSent: 1, within: time.Second,
		},
		{
			name: "a REFER answered 300 is refused", status: 300, reason: "Multiple Choices", deadline: long,
			wantEnd: ReferEndRefused, wantResponse: ReferResponse{300, "Multiple Choices"}, wantResponseReceived: true,
			wantSent: 1, within: time.Second,
		},
		{
			name: "an accepted REFER with no NOTIFY ends on the deadline", status: 202, reason: "Accepted", deadline: 50 * time.Millisecond,
			wantEnd: ReferEndDeadline, wantResponse: ReferResponse{202, "Accepted"}, wantResponseReceived: true, wantSent: 1,
		},
		{
			name: "a REFER answered 299 opens the wait", status: 299, reason: "Custom", deadline: 50 * time.Millisecond,
			wantEnd: ReferEndDeadline, wantResponse: ReferResponse{299, "Custom"}, wantResponseReceived: true, wantSent: 1,
		},
		{
			name: "a REFER answered 200 waits like a 202", status: 200, reason: "OK", deadline: long,
			notifies: []referNotifySpec{{body: "SIP/2.0 200 OK", event: "refer", state: "terminated;reason=noresource"}},
			wantEnd:  ReferEndFinalNotify, wantResponse: ReferResponse{200, "OK"}, wantResponseReceived: true,
			wantStatuses: []int{200}, wantSent: 1, within: time.Second,
		},
		{
			name: "a 199 sipfrag is progress and a 200 sipfrag ends the wait", status: 202, reason: "Accepted", deadline: long,
			notifies: []referNotifySpec{{body: "SIP/2.0 199 Early Dialog Terminated", event: "refer", state: "active"}, {body: "SIP/2.0 200 OK", event: "refer", state: "active"}},
			wantEnd:  ReferEndFinalNotify, wantResponse: ReferResponse{202, "Accepted"}, wantResponseReceived: true,
			wantStatuses: []int{199, 200}, wantSent: 1, within: time.Second,
		},
		{
			name: "a 1xx with the subscription terminated ends the subscription", status: 202, reason: "Accepted", deadline: long,
			notifies: []referNotifySpec{trying, {body: "SIP/2.0 100 Trying", event: "refer", state: "terminated;reason=noresource"}},
			wantEnd:  ReferEndSubscriptionEnded, wantResponse: ReferResponse{202, "Accepted"}, wantResponseReceived: true,
			wantStatuses: []int{100, 100}, wantSent: 1, within: time.Second,
		},
		{
			name: "a context cancelled during the wait ends as cancelled", status: 202, reason: "Accepted", deadline: long,
			cancel: cancelDuringWait, wantEnd: ReferEndCancelled, wantResponse: ReferResponse{202, "Accepted"},
			wantResponseReceived: true, wantSent: 1, within: time.Second,
		},
		{
			name: "an already cancelled context sends nothing", status: 202, reason: "Accepted", deadline: long,
			cancel: cancelBefore, wantEnd: ReferEndCancelled, wantSent: 0, within: time.Second,
		},
		{
			name: "a context cancelled before the response ends as cancelled", status: 202, reason: "Accepted", deadline: long,
			cancel: cancelBeforeResponse, doErr: context.Canceled, wantEnd: ReferEndCancelled, wantSent: 1, within: time.Second,
		},
		{name: "a zero deadline is an error", status: 202, reason: "Accepted", deadline: 0, wantErr: true},
		{name: "a negative deadline is an error", status: 202, reason: "Accepted", deadline: -time.Nanosecond, wantErr: true},
		{
			name: "a 1 ms deadline is accepted", status: 202, reason: "Accepted", deadline: time.Millisecond,
			wantEnd: ReferEndDeadline, wantResponse: ReferResponse{202, "Accepted"}, wantResponseReceived: true, wantSent: 1,
		},
		{name: "a dialog that is not confirmed is an error", status: 202, reason: "Accepted", deadline: long, notConfirmed: true, wantErr: true},
		{
			name: "a transport failure is an error", status: 202, reason: "Accepted", deadline: long,
			doErr: errors.New("transport closed"), wantErr: true, wantSent: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newReferObserveDialog(t, tt.status, tt.reason)
			d.doErr = tt.doErr
			if tt.notConfirmed {
				d.sipDialog.InitWithState(sip.DialogStateEstablished)
			}

			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			var waitNotifies func() []int
			switch tt.cancel {
			case cancelBefore:
				cancel()
			case cancelDuringWait:
				d.onDo = func(*sip.Request) { time.AfterFunc(20*time.Millisecond, cancel) }
			case cancelBeforeResponse:
				d.onDo = func(*sip.Request) { cancel() }
			}
			if len(tt.notifies) > 0 {
				waitNotifies = notifyAfterRefer(t, d, tt.notifies...)
			}

			started := time.Now()
			obs, _, err := observeRefer(ctx, d, ReferObserveOptions{Deadline: tt.deadline})
			elapsed := time.Since(started)

			if waitNotifies != nil && d.refersSent.Load() > 0 {
				for _, code := range waitNotifies() {
					assert.Equal(t, sip.StatusOK, code, "every NOTIFY is answered 200")
				}
			}
			assert.Equal(t, tt.wantSent, d.refersSent.Load(), "REFERs sent")
			assert.Zero(t, d.hangups.Load(), "the REFER path never ends the dialog")
			assert.Nil(t, d.media.onReferNotify, "the observing REFER never installs the int-only callback")

			if tt.wantErr {
				require.Error(t, err)
				var failure *ReferFailureError
				assert.False(t, errors.As(err, &failure), "the observing REFER never reports its outcome as a ReferFailureError")
				assert.Equal(t, ReferObservation{}, obs)
				assert.Zero(t, registeredReferAttempts(d.media), "a REFER that could not be carried leaves no attempt")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantEnd, obs.End)
			assert.Equal(t, tt.wantResponse, obs.Response)
			assert.Equal(t, tt.wantResponseReceived, obs.ResponseReceived)
			assert.Equal(t, tt.wantStatuses, notifyStatuses(obs))
			_, hasLast := obs.LastNotify()
			assert.Equal(t, len(tt.wantStatuses) > 0, hasLast)
			if tt.wantSent > 0 {
				assert.True(t, obs.CSeqKnown, "the sent REFER's CSeq is recorded")
				assert.Equal(t, uint32(1), obs.CSeq)
			}
			if tt.within > 0 {
				assert.Less(t, elapsed, tt.within, "the wait ended on its kind, not on the deadline")
			}
		})
	}
}

// TestReferObserveRecordsEveryNotify checks every NOTIFY is recorded in the
// order it was handled with its Subscription-State and Event id as received,
// duplicates kept, and that the record is bounded with the terminal NOTIFY
// always last.
func TestReferObserveRecordsEveryNotify(t *testing.T) {
	run := func(t *testing.T, notifies ...referNotifySpec) ReferObservation {
		t.Helper()
		d := newReferObserveDialog(t, 202, "Accepted")
		wait := notifyAfterRefer(t, d, notifies...)
		obs, _, err := observeRefer(t.Context(), d, ReferObserveOptions{Deadline: 5 * time.Second})
		require.NoError(t, err)
		wait()
		assert.Zero(t, d.hangups.Load(), "the REFER path never ends the dialog")
		return obs
	}

	t.Run("subscription state and event id are kept as received", func(t *testing.T) {
		obs := run(t,
			referNotifySpec{body: "SIP/2.0 100 Trying", event: "refer;id=1", state: "active;expires=60"},
			referNotifySpec{body: "SIP/2.0 200 OK", event: "refer;id=1", state: "terminated;reason=noresource"},
		)
		assert.Equal(t, ReferEndFinalNotify, obs.End)
		assert.Equal(t, []ReferNotify{
			{Status: 100, Reason: "Trying", SubscriptionState: "active;expires=60", HasSubscriptionState: true, EventID: 1, HasEventID: true},
			{Status: 200, Reason: "OK", SubscriptionState: "terminated;reason=noresource", HasSubscriptionState: true, EventID: 1, HasEventID: true},
		}, obs.Notifies)
		assert.Zero(t, obs.NotifiesDropped)
	})

	t.Run("an absent Subscription-State and Event id are recorded as absent", func(t *testing.T) {
		obs := run(t, referNotifySpec{body: "SIP/2.0 200 OK"})
		assert.Equal(t, ReferEndFinalNotify, obs.End)
		assert.Equal(t, []ReferNotify{{Status: 200, Reason: "OK"}}, obs.Notifies)
	})

	t.Run("identical NOTIFYs are both kept", func(t *testing.T) {
		ringing := referNotifySpec{body: "SIP/2.0 180 Ringing", event: "refer", state: "active"}
		obs := run(t, ringing, ringing, referNotifySpec{body: "SIP/2.0 200 OK", event: "refer", state: "terminated"})
		assert.Equal(t, []int{180, 180, 200}, notifyStatuses(obs))
		require.Len(t, obs.Notifies, 3)
		assert.Equal(t, obs.Notifies[0], obs.Notifies[1])
	})

	t.Run("past the bound the terminal NOTIFY is still last", func(t *testing.T) {
		var notifies []referNotifySpec
		for range 37 {
			notifies = append(notifies, referNotifySpec{body: "SIP/2.0 100 Trying", event: "refer", state: "active"})
		}
		notifies = append(notifies, referNotifySpec{body: "SIP/2.0 486 Busy Here", event: "refer", state: "terminated"})

		obs := run(t, notifies...)
		assert.Equal(t, ReferEndFinalNotify, obs.End)
		require.Len(t, obs.Notifies, maxReferNotifies)
		last, ok := obs.LastNotify()
		require.True(t, ok)
		assert.Equal(t, 486, last.Status)
		for _, n := range obs.Notifies[:maxReferNotifies-1] {
			assert.Equal(t, 100, n.Status)
		}
		assert.Equal(t, 6, obs.NotifiesDropped)
	})
}

// endReferOnDeadline runs an observed REFER on d that is accepted and ends on
// its deadline, and returns the observation.
func endReferOnDeadline(t *testing.T, d *referObserveDialog, onLate func(ReferLateNotify)) ReferObservation {
	t.Helper()
	obs, _, err := observeRefer(t.Context(), d, ReferObserveOptions{Deadline: 20 * time.Millisecond, OnLate: onLate})
	require.NoError(t, err)
	require.Equal(t, ReferEndDeadline, obs.End)
	return obs
}

// TestReferObserveLateNotify checks a terminal NOTIFY that arrives after the
// wait ended is answered 200, delivered once to that attempt's OnLate with the
// attempt's CSeq, and never touches the dialog.
func TestReferObserveLateNotify(t *testing.T) {
	t.Run("a final NOTIFY after the deadline is delivered once", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		d.nextCSeq = 7
		var lates lateRecorder
		endReferOnDeadline(t, d, lates.record)

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=7", "terminated;reason=noresource"))
		assert.Equal(t, []ReferLateNotify{{CSeq: 7, CSeqKnown: true, Notify: ReferNotify{
			Status: 200, Reason: "OK", SubscriptionState: "terminated;reason=noresource", HasSubscriptionState: true, EventID: 7, HasEventID: true,
		}}}, lates.calls())

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=7", "terminated;reason=noresource"))
		assert.Len(t, lates.calls(), 1, "a duplicate late NOTIFY is not delivered again")
		assert.Zero(t, d.hangups.Load(), "a late NOTIFY never ends the dialog")
	})

	t.Run("a late 1xx is not delivered", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var lates lateRecorder
		endReferOnDeadline(t, d, lates.record)

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 180 Ringing", "refer;id=1", "active"))
		assert.Empty(t, lates.calls())

		// The attempt still takes its terminal NOTIFY.
		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 486 Busy Here", "refer;id=1", "terminated"))
		require.Len(t, lates.calls(), 1)
		assert.Equal(t, 486, lates.calls()[0].Notify.Status)
		assert.Zero(t, d.hangups.Load())
	})

	t.Run("a late 1xx that terminates the subscription is delivered", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var lates lateRecorder
		endReferOnDeadline(t, d, lates.record)

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 100 Trying", "", "terminated;reason=timeout"))
		require.Len(t, lates.calls(), 1)
		late := lates.calls()[0]
		assert.Equal(t, 100, late.Notify.Status)
		assert.Equal(t, "terminated;reason=timeout", late.Notify.SubscriptionState)
		assert.Equal(t, uint32(1), late.CSeq)
		assert.Zero(t, d.hangups.Load())
	})

	t.Run("a final NOTIFY after cancellation is delivered", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var lates lateRecorder
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		d.onDo = func(*sip.Request) { time.AfterFunc(20*time.Millisecond, cancel) }
		obs, _, err := observeRefer(ctx, d, ReferObserveOptions{Deadline: 5 * time.Second, OnLate: lates.record})
		require.NoError(t, err)
		require.Equal(t, ReferEndCancelled, obs.End)

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=1", "terminated"))
		require.Len(t, lates.calls(), 1)
		assert.Equal(t, 200, lates.calls()[0].Notify.Status)
		assert.Zero(t, d.hangups.Load())
	})

	t.Run("nothing is delivered after the media is closed", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var lates lateRecorder
		endReferOnDeadline(t, d, lates.record)
		require.NoError(t, d.media.Close())

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=1", "terminated"))
		assert.Empty(t, lates.calls())
		assert.Zero(t, registeredReferAttempts(d.media))
		assert.Zero(t, d.hangups.Load())
	})

	t.Run("without OnLate a late NOTIFY is answered and dropped", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		endReferOnDeadline(t, d, nil)
		require.Equal(t, 1, registeredReferAttempts(d.media), "an attempt that ended on the deadline waits for a late NOTIFY")

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=1", "terminated"))
		assert.Zero(t, registeredReferAttempts(d.media))
		assert.Zero(t, d.hangups.Load())
	})
}

// referRun is the result of an observed REFER run on its own goroutine.
type referRun struct {
	obs ReferObservation
	err error
}

// TestReferObserveStaleEventID checks a NOTIFY for an older REFER attempt on
// the same dialog is never attributed to a newer attempt, and that a NOTIFY
// without an id goes to the newest attempt only.
func TestReferObserveStaleEventID(t *testing.T) {
	// startNewer ends attempt A (CSeq 1) on its deadline, then starts attempt B
	// (CSeq 2) waiting on its own goroutine, and returns once B's REFER is out.
	startNewer := func(t *testing.T, d *referObserveDialog, lates *lateRecorder) <-chan referRun {
		t.Helper()
		endReferOnDeadline(t, d, lates.record)

		d.nextCSeq = 2
		sent := make(chan struct{})
		d.onDo = func(*sip.Request) { close(sent) }
		done := make(chan referRun, 1)
		go func() {
			obs, _, err := observeRefer(t.Context(), d, ReferObserveOptions{Deadline: 5 * time.Second})
			done <- referRun{obs, err}
		}()
		select {
		case <-sent:
		case <-time.After(time.Second):
			t.Fatal("the newer REFER was not sent")
		}
		return done
	}
	awaitRun := func(t *testing.T, done <-chan referRun) referRun {
		t.Helper()
		select {
		case run := <-done:
			require.NoError(t, run.err)
			return run
		case <-time.After(time.Second):
			t.Fatal("the newer attempt did not end on its final NOTIFY")
			return referRun{}
		}
	}

	t.Run("an older attempt's id goes to that attempt and the newer keeps waiting", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var lates lateRecorder
		done := startNewer(t, d, &lates)

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 486 Busy Here", "refer;id=1", "terminated"))
		require.Len(t, lates.calls(), 1)
		assert.Equal(t, uint32(1), lates.calls()[0].CSeq)

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=2", "terminated"))
		run := awaitRun(t, done)
		assert.Equal(t, ReferEndFinalNotify, run.obs.End)
		require.Len(t, run.obs.Notifies, 1, "the older attempt's NOTIFY was recorded on the newer one")
		assert.Equal(t, uint32(2), run.obs.Notifies[0].EventID)
		assert.Equal(t, 200, run.obs.Notifies[0].Status)
		assert.Zero(t, d.hangups.Load())
	})

	t.Run("a NOTIFY without an id goes to the newer attempt, never the older", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var lates lateRecorder
		done := startNewer(t, d, &lates)

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 486 Busy Here", "refer", "terminated"))
		run := awaitRun(t, done)
		assert.Equal(t, ReferEndFinalNotify, run.obs.End)
		assert.Equal(t, []int{486}, notifyStatuses(run.obs))
		assert.Empty(t, lates.calls(), "the older attempt received a NOTIFY without an id")
		assert.Zero(t, d.hangups.Load())
	})
}

// TestReferObserveLateCallbackRunsOutsideTheLock checks OnLate is called after
// every lock the NOTIFY path takes has been released: an OnLate that takes
// those locks itself must complete.
func TestReferObserveLateCallbackRunsOutsideTheLock(t *testing.T) {
	d := newReferObserveDialog(t, 202, "Accepted")
	var calls, registeredDuringLate atomic.Int32
	endReferOnDeadline(t, d, func(ReferLateNotify) {
		d.media.referMu.Lock()
		registeredDuringLate.Store(int32(len(d.media.referAttempts)))
		d.media.referMu.Unlock()
		d.media.mu.Lock()
		closed := d.media.closed
		d.media.mu.Unlock()
		assert.False(t, closed)
		calls.Add(1)
	})

	done := make(chan int, 1)
	go func() { done <- sendReferNotify(t, d, "SIP/2.0 200 OK", "", "terminated") }()
	select {
	case code := <-done:
		assert.Equal(t, sip.StatusOK, code)
	case <-time.After(time.Second):
		t.Fatal("the NOTIFY handler blocked: OnLate ran while a lock it needs was held")
	}
	assert.Equal(t, int32(1), calls.Load())
	assert.Zero(t, registeredDuringLate.Load(), "the attempt is dropped before its late callback runs")
	assert.Zero(t, d.hangups.Load())
}

// TestReferObserveDeadlineRace checks a final NOTIFY arriving as the deadline
// fires is either in the observation or delivered once as late, never both and
// never neither. The NOTIFY leaves between 0 and 2 ms after the REFER, around
// the 1 ms deadline, so both halves occur.
func TestReferObserveDeadlineRace(t *testing.T) {
	var inTime, late int
	for i := range 200 {
		d := newReferObserveDialog(t, 202, "Accepted")
		sent := make(chan struct{})
		d.onDo = func(*sip.Request) { close(sent) }
		notified := make(chan struct{})
		delay := time.Duration(i%5) * 500 * time.Microsecond
		go func() {
			defer close(notified)
			select {
			case <-sent:
			case <-time.After(5 * time.Second):
				return
			}
			time.Sleep(delay)
			sendReferNotify(t, d, "SIP/2.0 200 OK", "", "terminated;reason=noresource")
		}()

		var lates atomic.Int32
		obs, _, err := observeRefer(t.Context(), d, ReferObserveOptions{
			Deadline: time.Millisecond,
			OnLate:   func(ReferLateNotify) { lates.Add(1) },
		})
		require.NoError(t, err)
		select {
		case <-notified:
		case <-time.After(5 * time.Second):
			t.Fatalf("iteration %d: the NOTIFY was not answered", i)
		}

		inObservation := obs.End == ReferEndFinalNotify
		delivered := lates.Load()
		switch {
		case inObservation && delivered == 0:
			inTime++
		case obs.End == ReferEndDeadline && delivered == 1:
			late++
		default:
			t.Fatalf("iteration %d: end %s with %d late deliveries, want the final in the observation or delivered once as late", i, obs.End, delivered)
		}
		require.Zero(t, d.hangups.Load())
	}
	t.Logf("in time %d, late %d", inTime, late)
}

// TestReferObservationCarriesNoRoutingInternals checks neither an observation,
// a late notify nor an error from the observing REFER carries a host, a URI,
// Via or Contact, even when every NOTIFY carries them.
func TestReferObservationCarriesNoRoutingInternals(t *testing.T) {
	secret := sip.Uri{User: "secret", Host: "10.9.8.7", Port: 5099}
	sendWithRouting := func(t *testing.T, d DialogSession, body, event, state string) {
		req := newReferNotifyRequest(t, "message/sipfrag", body)
		req.PrependHeader(&sip.ViaHeader{
			ProtocolName: "SIP", ProtocolVersion: "2.0", Transport: "UDP",
			Host: secret.Host, Port: secret.Port, Params: sip.NewParams(),
		})
		req.AppendHeader(&sip.ContactHeader{Address: secret})
		req.AppendHeader(sip.NewHeader("Event", event))
		req.AppendHeader(sip.NewHeader("Subscription-State", state))
		tx, _ := newReferNotifyTx(t, req)
		dialogHandleReferNotify(d, req, tx)
	}
	banned := []string{"10.9.8.7", "sip:", "@", "Via", "Contact", "secret"}
	requireClean := func(t *testing.T, what, rendered string) {
		t.Helper()
		for _, b := range banned {
			assert.NotContains(t, rendered, b, "%s carries %q: %s", what, b, rendered)
		}
	}

	d := newReferObserveDialog(t, 202, "Accepted")
	done := make(chan struct{})
	d.onDo = func(*sip.Request) {
		go func() {
			defer close(done)
			sendWithRouting(t, d, "SIP/2.0 100 Trying", "refer;id=1", "active;expires=60")
			sendWithRouting(t, d, "SIP/2.0 486 Busy Here", "refer;id=1", "terminated;reason=noresource")
		}()
	}
	obs, _, err := observeRefer(t.Context(), d, ReferObserveOptions{Deadline: 5 * time.Second})
	require.NoError(t, err)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the NOTIFYs were not answered")
	}
	require.Len(t, obs.Notifies, 2)
	requireClean(t, "the observation", fmt.Sprintf("%+v", obs))

	var lates lateRecorder
	d.onDo = nil
	d.nextCSeq = 2
	endReferOnDeadline(t, d, lates.record)
	sendWithRouting(t, d, "SIP/2.0 200 OK", "refer;id=2", "terminated;reason=noresource")
	require.Len(t, lates.calls(), 1)
	requireClean(t, "the late notify", fmt.Sprintf("%+v", lates.calls()[0]))

	_, _, err = observeRefer(t.Context(), d, ReferObserveOptions{Deadline: 0})
	require.Error(t, err)
	requireClean(t, "the deadline error", err.Error())
	d.sipDialog.InitWithState(sip.DialogStateEstablished)
	_, _, err = observeRefer(t.Context(), d, ReferObserveOptions{Deadline: time.Second})
	require.Error(t, err)
	requireClean(t, "the dialog state error", err.Error())

	fresh := newReferObserveDialog(t, 202, "Accepted")
	fresh.doErr = sipgoReferWriteFailure(nil)
	_, _, err = observeRefer(t.Context(), fresh, ReferObserveOptions{Deadline: time.Second})
	require.Error(t, err)
	requireClean(t, "the carry error", err.Error())
	requireClean(t, "the carry error", fmt.Sprintf("%+v", err))
}

// sipgoReferWriteFailure builds the error sipgo returns when a REFER's first
// write fails: its text carries the request line, which names the far end's
// Contact with its user part, and the socket error with the far end's address.
// notSent, when set, is wrapped beside the socket error.
func sipgoReferWriteFailure(notSent error) error {
	line := "REFER sip:secret@10.9.8.7:5099 SIP/2.0"
	writeErr := &net.OpError{
		Op: "write", Net: "udp",
		Addr: &net.UDPAddr{IP: net.IPv4(10, 9, 8, 7), Port: 5099},
		Err:  errors.New("network is unreachable"),
	}
	if notSent != nil {
		return fmt.Errorf("%w. %w", fmt.Errorf("fail to write request on init req=%q: %w: %w", line, notSent, writeErr), sip.ErrTransactionTransport)
	}
	return fmt.Errorf("%w. %w", fmt.Errorf("fail to write request on init req=%q: %w", line, writeErr), sip.ErrTransactionTransport)
}

// TestReferObserveCarryErrorKeepsOnlyItsClass checks a REFER that could not be
// carried comes back with its class and none of sipgo's text, which carries the
// REFER's request line and socket addresses.
func TestReferObserveCarryErrorKeepsOnlyItsClass(t *testing.T) {
	dialErr := &net.OpError{
		Op: "dial", Net: "tcp",
		Addr: &net.TCPAddr{IP: net.IPv4(10, 9, 8, 7), Port: 5099},
		Err:  errors.New("connect: connection refused"),
	}
	// errNotNamed plays a sentinel a newer sipgo defines and this module does
	// not name.
	errNotNamed := errors.New("transaction request not sent")
	banned := []string{"10.9.8.7", "5099", "sip:", "@", "secret", "Via", "Contact"}

	tests := []struct {
		name      string
		doErr     error
		wantText  string
		wantIs    []error
		wantNotIs []error
	}{
		{
			name:      "a failed write keeps the transport class",
			doErr:     sipgoReferWriteFailure(nil),
			wantText:  "refer: the REFER could not be carried: transaction transport error",
			wantIs:    []error{sip.ErrTransactionTransport},
			wantNotIs: []error{sip.ErrTransactionTimeout},
		},
		{
			name:      "a failed connection request names no class",
			doErr:     fmt.Errorf("client transcation failed to request connection: %w", dialErr),
			wantText:  "refer: the REFER could not be carried",
			wantNotIs: []error{sip.ErrTransactionTransport},
		},
		{
			name:     "timer B keeps the timeout class",
			doErr:    fmt.Errorf("Timer_B timed out. %w", sip.ErrTransactionTimeout),
			wantText: "refer: the REFER could not be carried: transaction timeout",
			wantIs:   []error{sip.ErrTransactionTimeout},
		},
		{
			name:     "a context error from the dialog under a live context is an error",
			doErr:    fmt.Errorf("dialog request: %w", context.DeadlineExceeded),
			wantText: "refer: the REFER could not be carried: context deadline exceeded",
			wantIs:   []error{context.DeadlineExceeded},
		},
		{
			name:     "a class this package does not name stays matchable",
			doErr:    sipgoReferWriteFailure(errNotNamed),
			wantText: "refer: the REFER could not be carried: transaction transport error",
			wantIs:   []error{errNotNamed, sip.ErrTransactionTransport},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newReferObserveDialog(t, 202, "Accepted")
			d.doErr = tt.doErr

			obs, _, err := observeRefer(t.Context(), d, ReferObserveOptions{Deadline: time.Second})
			require.Error(t, err)
			assert.Equal(t, tt.wantText, err.Error())
			for _, class := range tt.wantIs {
				assert.ErrorIs(t, err, class)
			}
			for _, class := range tt.wantNotIs {
				assert.NotErrorIs(t, err, class)
			}
			for _, rendered := range []string{err.Error(), fmt.Sprintf("%v", err), fmt.Sprintf("%+v", err)} {
				for _, b := range banned {
					assert.NotContains(t, rendered, b, "the carry error carries %q: %s", b, rendered)
				}
			}
			var op *net.OpError
			assert.False(t, errors.As(err, &op), "the socket error is reachable: %v", op)
			var failure *ReferFailureError
			assert.False(t, errors.As(err, &failure), "a REFER that could not be carried is not a ReferFailureError")

			assert.Equal(t, ReferObservation{}, obs)
			assert.Zero(t, registeredReferAttempts(d.media), "a REFER that could not be carried leaves no attempt")
			assert.Equal(t, int32(1), d.refersSent.Load(), "REFERs sent")
			assert.Zero(t, d.hangups.Load(), "the REFER path never ends the dialog")
		})
	}
}

// TestDialogReferKeepsTheFailureContract checks the upstream Refer and
// ReferOptions, which run over the observing REFER, keep their return contract:
// nil on a successful final NOTIFY, *ReferFailureError for a failed or missing
// outcome, sipgo.ErrDialogResponse for a refused REFER, and a return at the
// acceptance when the int-only callback is in use.
func TestDialogReferKeepsTheFailureContract(t *testing.T) {
	orig := referAnswerDeadline
	referAnswerDeadline = 50 * time.Millisecond
	defer func() { referAnswerDeadline = orig }()

	terminated := "terminated;reason=noresource"
	tests := []struct {
		name        string
		status      int
		reason      string
		wait        bool
		notifies    []referNotifySpec
		cancel      bool
		wantNil     bool
		wantFailure *ReferFailureError
		wantRefused int
	}{
		{name: "a successful final NOTIFY returns nil", status: 202, reason: "Accepted", wait: true,
			notifies: []referNotifySpec{{body: "SIP/2.0 200 OK", event: "refer", state: terminated}}, wantNil: true},
		{name: "a busy final NOTIFY is a failure with its status", status: 202, reason: "Accepted", wait: true,
			notifies:    []referNotifySpec{{body: "SIP/2.0 100 Trying", event: "refer", state: "active"}, {body: "SIP/2.0 486 Busy Here", event: "refer", state: terminated}},
			wantFailure: &ReferFailureError{Status: 486, Reason: "Busy Here"}},
		{name: "no final NOTIFY is a timeout", status: 202, reason: "Accepted", wait: true,
			wantFailure: &ReferFailureError{Status: 0, Reason: "timeout"}},
		{name: "an ended subscription is a failure without a status", status: 202, reason: "Accepted", wait: true,
			notifies:    []referNotifySpec{{body: "SIP/2.0 100 Trying", event: "refer", state: terminated}},
			wantFailure: &ReferFailureError{Status: 0, Reason: "subscription ended"}},
		{name: "a cancelled context is a failure without a status", status: 202, reason: "Accepted", wait: true, cancel: true,
			wantFailure: &ReferFailureError{Status: 0, Reason: "cancelled"}},
		{name: "a refused REFER is the dialog response error", status: 403, reason: "Forbidden", wait: true, wantRefused: 403},
		{name: "with the callback an accepted REFER returns at once", status: 200, reason: "OK", wantNil: true},
		{name: "with the callback a refused REFER is the dialog response error", status: 403, reason: "Forbidden", wantRefused: 403},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := newReferObserveDialog(t, tt.status, tt.reason)
			var waitNotifies func() []int
			if len(tt.notifies) > 0 {
				waitNotifies = notifyAfterRefer(t, d, tt.notifies...)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if tt.cancel {
				cancel()
			}

			err := dialogRefer(ctx, d, referTestRecipient, referTestTarget, referTestReferredBy, tt.wait)
			if waitNotifies != nil {
				waitNotifies()
			}
			assert.Zero(t, d.hangups.Load(), "the REFER path never ends the dialog")

			switch {
			case tt.wantNil:
				require.NoError(t, err)
			case tt.wantFailure != nil:
				var failure *ReferFailureError
				require.True(t, errors.As(err, &failure), "want *ReferFailureError, got %T: %v", err, err)
				assert.Equal(t, tt.wantFailure, failure)
			default:
				var refused sipgo.ErrDialogResponse
				require.True(t, errors.As(err, &refused), "want sipgo.ErrDialogResponse, got %T: %v", err, err)
				assert.Equal(t, tt.wantRefused, refused.Res.StatusCode)
			}
		})
	}
}

// TestIntegrationDialogReferWaitsForOutcome asserts the whole loop: a plain
// Refer (no OnNotify) reports the real transfer outcome rather than the 202, and
// a failed transfer surfaces as a typed *ReferFailureError carrying the status.
func TestIntegrationDialogReferWaitsForOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// UAS that receives our REFER and drives the referred INVITE.
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15081,
				ID:        "udp",
			},
		))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.AnswerOptions(AnswerOptions{
				OnRefer: func(referDialog *DialogClientSession) error {
					if err := referDialog.Invite(referDialog.Context(), InviteClientOptions{}); err != nil {
						return err
					}
					if err := referDialog.Ack(ctx); err != nil {
						return err
					}
					return referDialog.Hangup(referDialog.Context())
				},
			})
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	// Refer target.
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15082,
			},
		))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			switch d.ToUser() {
			case "busy":
				d.Respond(sip.StatusBusyHere, "Busy Here", nil)
				return
			default:
				d.Answer()
			}
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	// UAS that accepts the REFER (so 202 + 100 Trying are sent) but never lets the
	// referred dialog reach a terminal state, so no terminal sipfrag is ever sent.
	// Released at test end so the handler goroutine does not linger.
	stuckRefer := make(chan struct{})
	defer close(stuckRefer)
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15083,
				ID:        "udp",
			},
		))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.AnswerOptions(AnswerOptions{
				OnRefer: func(referDialog *DialogClientSession) error {
					<-stuckRefer
					return nil
				},
			})
			<-d.Context().Done()
		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  15080,
		},
	))
	require.NoError(t, dg.ServeBackground(ctx, nil))

	t.Run("Successful", func(t *testing.T) {
		d, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15081}, InviteOptions{})
		require.NoError(t, err)
		defer d.Close()
		defer d.Hangup(d.Context())

		// Returns only once the terminal sipfrag says the transfer completed.
		require.NoError(t, d.Refer(d.Context(), sip.Uri{Host: "127.0.0.1", Port: 15082}))
	})

	t.Run("BusyReturnsTypedFailure", func(t *testing.T) {
		d, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15081}, InviteOptions{})
		require.NoError(t, err)
		defer d.Close()
		defer d.Hangup(d.Context())

		err = d.Refer(d.Context(), sip.Uri{User: "busy", Host: "127.0.0.1", Port: 15082})
		require.Error(t, err, "refer to a busy target must not report success")

		var referErr *ReferFailureError
		require.True(t, errors.As(err, &referErr), "want *ReferFailureError, got %T: %v", err, err)
		assert.Equal(t, sip.StatusBusyHere, referErr.Status)
		assert.NotContains(t, referErr.Error(), "127.0.0.1")

		// A refused transfer leaves the referring dialog to its owner.
		requireDialogStaysConfirmed(t, d, 750*time.Millisecond)
	})

	t.Run("NoTerminalNotifyTimesOut", func(t *testing.T) {
		// Shrink the answer-supervision window so the deadline path is testable.
		orig := referAnswerDeadline
		referAnswerDeadline = 200 * time.Millisecond
		defer func() { referAnswerDeadline = orig }()

		// Talk to the peer that accepts the REFER but never completes it.
		d, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15083}, InviteOptions{})
		require.NoError(t, err)
		defer d.Close()
		defer d.Hangup(d.Context())

		// The REFER is accepted (202) and 100 Trying arrives, but no terminal
		// sipfrag ever does, so the deadline must decide the outcome.
		err = d.Refer(d.Context(), sip.Uri{Host: "127.0.0.1", Port: 15082})
		require.Error(t, err, "a refer with no terminal sipfrag must not report success")

		var referErr *ReferFailureError
		require.True(t, errors.As(err, &referErr), "want *ReferFailureError, got %T: %v", err, err)
		assert.Equal(t, 0, referErr.Status, "a timeout carries no SIP status")
		assert.True(t, strings.Contains(referErr.Reason, "timeout") || strings.Contains(referErr.Reason, "cancelled"))
	})
}

// requireDialogStaysConfirmed fails if d ends within window, then requires it
// is still confirmed. The bounded wait is what gives the negative assertion a
// meaning: an ending dialog cancels its context, and the state check covers a
// dialog that has left confirmed without cancelling yet.
func requireDialogStaysConfirmed(t *testing.T, d DialogSession, window time.Duration) {
	t.Helper()

	timer := time.NewTimer(window)
	defer timer.Stop()
	select {
	case <-d.Context().Done():
		t.Fatalf("the dialog ended within %s of the REFER outcome", window)
	case <-timer.C:
	}
	require.Equal(t, sip.DialogStateConfirmed, d.DialogSIP().LoadState(), "the dialog left the confirmed state")
}

// observingReferrer is a dialog session that can send an observed REFER. Both
// DialogClientSession and DialogServerSession satisfy it with one signature.
type observingReferrer interface {
	DialogSession
	ReferAndObserve(ctx context.Context, referTo sip.Uri, opts ReferObserveOptions) (ReferObservation, error)
}

var (
	_ observingReferrer = (*DialogClientSession)(nil)
	_ observingReferrer = (*DialogServerSession)(nil)
)

// observedReferCall is one answered call whose dialog refers the far end, with
// the Contact each side put on the INVITE transaction.
type observedReferCall struct {
	dialog        observingReferrer
	ourContact    sip.Uri
	farEndContact sip.Uri
}

// observedReferTransferee is the far end of an observed REFER. For each referral
// it records the Referred-By the referred INVITE carries, keyed by the Refer-To
// user, then invites the target, acks and hangs up the referred leg. A referral
// that finds a hold set waits for its release before inviting the target.
type observedReferTransferee struct {
	ctx      context.Context
	handlers sync.WaitGroup

	mu         sync.Mutex
	hold       chan struct{}
	referredBy map[string]string
}

func newObservedReferTransferee(ctx context.Context) *observedReferTransferee {
	return &observedReferTransferee{ctx: ctx, referredBy: map[string]string{}}
}

func (tr *observedReferTransferee) onRefer(referDialog *DialogClientSession) error {
	tr.handlers.Add(1)
	defer tr.handlers.Done()

	var referredBy string
	if h := referDialog.InviteRequest.GetHeader("Referred-By"); h != nil {
		referredBy = h.Value()
	}
	tr.mu.Lock()
	tr.referredBy[referDialog.InviteRequest.Recipient.User] = referredBy
	hold := tr.hold
	tr.hold = nil
	tr.mu.Unlock()

	if hold != nil {
		select {
		case <-hold:
		case <-tr.ctx.Done():
			return tr.ctx.Err()
		}
	}
	if err := referDialog.Invite(referDialog.Context(), InviteClientOptions{}); err != nil {
		return err
	}
	if err := referDialog.Ack(tr.ctx); err != nil {
		return err
	}
	return referDialog.Hangup(referDialog.Context())
}

// holdNextReferral makes the next referral wait until release is called.
// release is idempotent and also runs when t ends, so no referral stays held
// past the test that set it.
func (tr *observedReferTransferee) holdNextReferral(t *testing.T) (release func()) {
	t.Helper()

	hold := make(chan struct{})
	tr.mu.Lock()
	tr.hold = hold
	tr.mu.Unlock()

	var once sync.Once
	release = func() { once.Do(func() { close(hold) }) }
	t.Cleanup(release)
	return release
}

// referredByFor returns the Referred-By value recorded for the referral to user.
func (tr *observedReferTransferee) referredByFor(user string) (string, bool) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	value, ok := tr.referredBy[user]
	return value, ok
}

// awaitHandlers fails t unless every referral handler returned within 5s.
func (tr *observedReferTransferee) awaitHandlers(t *testing.T) {
	t.Helper()

	done := make(chan struct{})
	go func() {
		tr.handlers.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Errorf("a referral handler was still running 5s after the test")
	}
}

// requireNoIntOnlyNotifyCallback fails if d carries the int-only NOTIFY
// callback of ReferOptions: ReferAndObserve must not install it, since it is
// sticky on the dialog and later REFERs would report to it.
func requireNoIntOnlyNotifyCallback(t *testing.T, d DialogSession) {
	t.Helper()

	med := d.Media()
	med.mu.Lock()
	installed := med.onReferNotify != nil
	med.mu.Unlock()
	require.False(t, installed, "ReferAndObserve installed the int-only NOTIFY callback")
}

// TestIntegrationDialogReferAndObserve runs ReferAndObserve over real UAs with
// each session type as the referrer. A refused transfer and a late outcome both
// leave the referring dialog confirmed, the late outcome reaches OnLate for its
// own attempt, and the Referred-By the transferee receives names our Contact.
func TestIntegrationDialogReferAndObserve(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	transferee := newObservedReferTransferee(ctx)
	target := sip.Uri{Host: "127.0.0.1", Port: 15090}

	// Refer target: refuses the busy user and answers any other.
	{
		ua, _ := sipgo.NewUA()
		t.Cleanup(func() { ua.Close() })

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15090,
			},
		))
		require.NoError(t, dg.ServeBackground(ctx, func(d *DialogServerSession) {
			if d.ToUser() == "busy" {
				d.Respond(sip.StatusBusyHere, "Busy Here", nil)
				return
			}
			d.Answer()
			<-d.Context().Done()
		}))
	}

	t.Run("client session", func(t *testing.T) {
		// Transferee: answers our INVITE and acts on the REFER.
		{
			ua, _ := sipgo.NewUA()
			t.Cleanup(func() { ua.Close() })

			dg := NewDiago(ua, WithTransport(
				Transport{
					Transport: "udp",
					BindHost:  "127.0.0.1",
					BindPort:  15091,
					ID:        "udp",
				},
			))
			require.NoError(t, dg.ServeBackground(ctx, func(d *DialogServerSession) {
				d.AnswerOptions(AnswerOptions{OnRefer: transferee.onRefer})
				<-d.Context().Done()
			}))
		}

		ua, _ := sipgo.NewUA()
		t.Cleanup(func() { ua.Close() })

		referrer := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15092,
			},
		))
		require.NoError(t, referrer.ServeBackground(ctx, nil))
		t.Cleanup(func() { transferee.awaitHandlers(t) })

		dial := func(t *testing.T) observedReferCall {
			d, err := referrer.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15091}, InviteOptions{})
			require.NoError(t, err)
			t.Cleanup(func() {
				d.Hangup(ctx)
				d.Close()
			})
			return observedReferCall{
				dialog:        d,
				ourContact:    d.InviteRequest.Contact().Address,
				farEndContact: d.InviteResponse.Contact().Address,
			}
		}
		runObservedReferCases(t, dial, transferee, target, "client")
	})

	t.Run("server session", func(t *testing.T) {
		// Dialer: calls the referrer and is the transferee of its REFER.
		var dialer *Diago
		{
			ua, _ := sipgo.NewUA()
			t.Cleanup(func() { ua.Close() })

			dialer = NewDiago(ua, WithTransport(
				Transport{
					Transport: "udp",
					BindHost:  "127.0.0.1",
					BindPort:  15094,
					ID:        "udp",
				},
			))
			require.NoError(t, dialer.ServeBackground(ctx, nil))
		}

		ua, _ := sipgo.NewUA()
		t.Cleanup(func() { ua.Close() })

		referrer := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15093,
			},
		))
		received := make(chan *DialogServerSession)
		require.NoError(t, referrer.ServeBackground(ctx, func(d *DialogServerSession) {
			select {
			case received <- d:
			case <-ctx.Done():
				return
			}
			<-d.Context().Done()
		}))
		t.Cleanup(func() { transferee.awaitHandlers(t) })

		dial := func(t *testing.T) observedReferCall {
			dialog, err := dialer.NewDialog(sip.Uri{User: "referrer", Host: "127.0.0.1", Port: 15093}, NewDialogOptions{})
			require.NoError(t, err)
			t.Cleanup(func() { dialog.Close() })

			dialed := make(chan error, 1)
			go func() {
				err := dialog.Invite(ctx, InviteClientOptions{OnRefer: transferee.onRefer})
				if err == nil {
					err = dialog.Ack(ctx)
				}
				dialed <- err
			}()

			var d *DialogServerSession
			select {
			case d = <-received:
			case <-time.After(5 * time.Second):
				t.Fatal("the referrer did not receive the INVITE within 5s")
			}
			t.Cleanup(func() {
				d.Hangup(ctx)
				d.Close()
			})
			require.NoError(t, d.Answer())

			select {
			case err := <-dialed:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the dialer's INVITE did not complete within 5s")
			}
			return observedReferCall{
				dialog:        d,
				ourContact:    d.InviteResponse.Contact().Address,
				farEndContact: d.InviteRequest.Contact().Address,
			}
		}
		runObservedReferCases(t, dial, transferee, target, "server")
	})
}

// runObservedReferCases runs the ReferAndObserve cases on calls from dial.
// userSuffix keeps each group's Refer-To users apart in the transferee's record.
func runObservedReferCases(t *testing.T, dial func(t *testing.T) observedReferCall, transferee *observedReferTransferee, target sip.Uri, userSuffix string) {
	t.Run("a refusal keeps the dialog", func(t *testing.T) {
		call := dial(t)

		busy := target
		busy.User = "busy"
		obs, err := call.dialog.ReferAndObserve(call.dialog.Context(), busy, ReferObserveOptions{Deadline: 5 * time.Second})
		require.NoError(t, err)

		assert.Equal(t, ReferEndFinalNotify, obs.End)
		assert.True(t, obs.ResponseReceived, "the REFER's final response is recorded")
		assert.True(t, obs.Response.Status >= 200 && obs.Response.Status < 300, "the REFER was accepted, got %d", obs.Response.Status)
		require.NotEmpty(t, obs.Notifies, "the far end's NOTIFYs are recorded")
		assert.Equal(t, 100, obs.Notifies[0].Status)
		last, ok := obs.LastNotify()
		require.True(t, ok)
		assert.Equal(t, sip.StatusBusyHere, last.Status)
		requireNoIntOnlyNotifyCallback(t, call.dialog)

		requireDialogStaysConfirmed(t, call.dialog, 750*time.Millisecond)
	})

	t.Run("a late outcome reaches OnLate and keeps the dialog", func(t *testing.T) {
		call := dial(t)

		late := make(chan ReferLateNotify, 4)
		release := transferee.holdNextReferral(t)
		answering := target
		answering.User = "late-" + userSuffix
		obs, err := call.dialog.ReferAndObserve(call.dialog.Context(), answering, ReferObserveOptions{
			Deadline: 300 * time.Millisecond,
			OnLate: func(n ReferLateNotify) {
				select {
				case late <- n:
				default:
				}
			},
		})
		require.NoError(t, err)
		require.Equal(t, ReferEndDeadline, obs.End, "a held referral ends the wait on the deadline")

		release()
		select {
		case n := <-late:
			assert.Equal(t, obs.CSeq, n.CSeq, "the late NOTIFY is delivered for this attempt")
			assert.True(t, n.CSeqKnown)
			assert.Equal(t, 200, n.Notify.Status)
		case <-time.After(5 * time.Second):
			t.Fatal("no terminal NOTIFY reached OnLate within 5s of releasing the referral")
		}
		requireNoIntOnlyNotifyCallback(t, call.dialog)

		requireDialogStaysConfirmed(t, call.dialog, 750*time.Millisecond)
	})

	t.Run("Referred-By names our own contact", func(t *testing.T) {
		call := dial(t)

		answering := target
		answering.User = "referred-by-" + userSuffix
		obs, err := call.dialog.ReferAndObserve(call.dialog.Context(), answering, ReferObserveOptions{Deadline: 5 * time.Second})
		require.NoError(t, err)
		require.Equal(t, ReferEndFinalNotify, obs.End)
		requireNoIntOnlyNotifyCallback(t, call.dialog)

		value, ok := transferee.referredByFor(answering.User)
		require.True(t, ok, "the transferee received no referral")
		require.NotEmpty(t, value, "the referred INVITE carried no Referred-By")

		var referrer sip.Uri
		params := sip.NewParams()
		_, err = sip.ParseAddressValue(value, &referrer, &params)
		require.NoError(t, err)

		assert.Equal(t, call.ourContact.User, referrer.User)
		assert.Equal(t, call.ourContact.Host, referrer.Host)
		assert.Equal(t, call.ourContact.Port, referrer.Port)
		farEnd := call.farEndContact
		assert.False(t, referrer.User == farEnd.User && referrer.Host == farEnd.Host && referrer.Port == farEnd.Port,
			"Referred-By names the far end %s", farEnd.String())
	})
}
