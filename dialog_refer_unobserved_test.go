// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referWithoutObserver sends the REFER of a Refer or ReferOptions call on d,
// which return at its acceptance.
func referWithoutObserver(ctx context.Context, d DialogSession) error {
	return dialogRefer(ctx, d, referTestRecipient, referTestTarget, referTestReferredBy)
}

// TestDialogReferWithoutObserver pins what Refer and ReferOptions do with a
// REFER nobody observes, on a dialog without the OnNotify callback: they return
// at the acceptance, the REFER's subscription is held until a NOTIFY ends it,
// with no deadline of ours, its NOTIFYs are answered 200 and its final one
// hangs the call up. A NOTIFY the subscription no longer holds matches nothing,
// as a NOTIFY for no REFER does (RFC 6665 section 4.1.3).
func TestDialogReferWithoutObserver(t *testing.T) {
	const terminated = "terminated;reason=noresource"

	t.Run("a final NOTIFY is answered 200 and hangs the call up", func(t *testing.T) {
		for _, tc := range []struct {
			name   string
			status int
			reason string
			final  string
		}{
			{name: "accepted 202 and transferred", status: 202, reason: "Accepted", final: "SIP/2.0 200 OK"},
			{name: "accepted 200 and transferred", status: 200, reason: "OK", final: "SIP/2.0 200 OK"},
			{name: "accepted and refused by the target", status: 202, reason: "Accepted", final: "SIP/2.0 486 Busy Here"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d := newReferObserveDialog(t, tc.status, tc.reason)
				var reports []ReferNotify
				d.OnReferNotifyUnmatched(func(n ReferNotify) { reports = append(reports, n) })

				require.NoError(t, referWithoutObserver(t.Context(), d))
				assert.Equal(t, int32(1), d.refersSent.Load(), "REFERs sent")
				assert.Equal(t, 1, registeredReferAttempts(&d.sessionState), "the REFER's subscription is not held")
				assert.Nil(t, d.onReferNotify, "the REFER installed the int-only callback")

				assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 100 Trying", "refer", "active;expires=60"))
				assert.Zero(t, d.hangups.Load(), "a progress NOTIFY ended the call")
				assert.Equal(t, 1, registeredReferAttempts(&d.sessionState))

				assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, tc.final, "refer", terminated))
				assert.Equal(t, int32(1), d.hangups.Load(), "the final NOTIFY did not hang the call up once")
				assert.Zero(t, registeredReferAttempts(&d.sessionState), "the ended subscription is still held")
				assert.Empty(t, reports)

				// The subscription ended with its final NOTIFY.
				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, sendReferNotify(t, d, tc.final, "refer", terminated))
				assert.Equal(t, int32(1), d.hangups.Load(), "a NOTIFY no subscription holds hung the call up")
				assert.Len(t, reports, 1, "the NOTIFY no subscription holds was not reported once")
			})
		}
	})

	t.Run("a NOTIFY that raced ahead of the REFER's response is the REFER's", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var answered int
		d.onDo = func(*sip.Request) { answered = sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=1", terminated) }

		require.NoError(t, referWithoutObserver(t.Context(), d))
		assert.Equal(t, sip.StatusOK, answered)
		assert.Equal(t, int32(1), d.hangups.Load())
		assert.Zero(t, registeredReferAttempts(&d.sessionState))
	})

	t.Run("a 1xx NOTIFY terminating the subscription ends it without a hang-up", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		require.NoError(t, referWithoutObserver(t.Context(), d))

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 100 Trying", "refer", terminated))
		assert.Zero(t, d.hangups.Load(), "a NOTIFY without a final status hung the call up")
		assert.Zero(t, registeredReferAttempts(&d.sessionState), "the terminated subscription is still held")
		assert.Equal(t, sip.StatusCallTransactionDoesNotExists, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer", terminated))
		assert.Zero(t, d.hangups.Load())
	})

	t.Run("a refused REFER holds no subscription", func(t *testing.T) {
		d := newReferObserveDialog(t, 403, "Forbidden")

		err := referWithoutObserver(t.Context(), d)
		var refused sipgo.ErrDialogResponse
		require.True(t, errors.As(err, &refused), "want sipgo.ErrDialogResponse, got %T: %v", err, err)
		assert.Equal(t, 403, refused.Res.StatusCode)
		assert.Zero(t, registeredReferAttempts(&d.sessionState))
		assert.Equal(t, sip.StatusCallTransactionDoesNotExists, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=1", terminated))
		assert.Zero(t, d.hangups.Load())
	})

	t.Run("a REFER that could not be sent holds no subscription and keeps its error", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		d.doErr = sipgoReferWriteFailure(nil)

		err := referWithoutObserver(t.Context(), d)
		assert.Same(t, d.doErr, err, "Refer changed the error of a REFER that was not sent")
		assert.Zero(t, registeredReferAttempts(&d.sessionState))
	})

	t.Run("an outcome that comes late still hangs the call up", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			d := newReferObserveDialog(t, 202, "Accepted")
			require.NoError(t, referWithoutObserver(t.Context(), d))

			// Past Timer N with no NOTIFY at all.
			time.Sleep(64*sip.T1 + time.Second)
			synctest.Wait()
			assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 100 Trying", "refer", "active;expires=1"))

			// Past the expires that NOTIFY gave, and past the longest an
			// observed attempt may run.
			time.Sleep(referSubscriptionMaxBound + time.Second)
			synctest.Wait()
			require.Equal(t, 1, registeredReferAttempts(&d.sessionState), "a deadline ended the subscription of a REFER nobody observes")

			assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer", terminated))
			assert.Equal(t, int32(1), d.hangups.Load(), "the late final NOTIFY did not hang the call up")
			assert.Zero(t, registeredReferAttempts(&d.sessionState))
		})
	})
}

// TestDialogReferNotifyCallbackBesideAttempts pins how a refer NOTIFY is routed
// on a dialog that has both the OnNotify callback of ReferOptions and REFER
// attempts: the callback takes every NOTIFY, whichever REFER it is for, but one
// for a subscription an earlier NOTIFY ended, which is answered 481 and
// reported as unmatched (RFC 6665 section 4.1.3), an attempt records the
// NOTIFYs it holds as it does without the callback, and nothing ends the
// dialog.
func TestDialogReferNotifyCallbackBesideAttempts(t *testing.T) {
	const terminated = "terminated;reason=noresource"

	t.Run("NotifyAfterTheFinalOfACallbackReferIs481", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			event string
			last  string
			state string
			want  []int
		}{
			{name: "WithoutEventID", event: "refer", last: "SIP/2.0 486 Busy Here", state: terminated, want: []int{100, 486}},
			{name: "WithTheRefersEventID", event: "refer;id=1", last: "SIP/2.0 486 Busy Here", state: terminated, want: []int{100, 486}},
			{name: "AfterA1xxTerminatingTheSubscription", event: "refer;id=1", last: "SIP/2.0 100 Trying", state: terminated, want: []int{100, 100}},
			{name: "AfterAFinalStatusInAnActiveSubscription", event: "refer;id=1", last: "SIP/2.0 200 OK", state: "active;expires=60", want: []int{100, 200}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				d := newReferObserveDialog(t, 202, "Accepted")
				var got []int
				d.onReferNotify = func(statusCode int) { got = append(got, statusCode) }
				var reports []ReferNotify
				d.OnReferNotifyUnmatched(func(n ReferNotify) { reports = append(reports, n) })

				require.NoError(t, referWithoutObserver(t.Context(), d))
				assert.Equal(t, 1, registeredReferAttempts(&d.sessionState), "the REFER's subscription is not held")

				assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 100 Trying", tc.event, "active;expires=60"))
				assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, tc.last, tc.event, tc.state))
				assert.Zero(t, registeredReferAttempts(&d.sessionState), "the ended subscription is still held")
				assert.Empty(t, reports)

				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, sendReferNotify(t, d, "SIP/2.0 486 Busy Here", tc.event, terminated),
					"a NOTIFY for a subscription that had ended was accepted")
				assert.Equal(t, tc.want, got, "a NOTIFY after the subscription ended reached the callback")
				assert.Len(t, reports, 1, "the NOTIFY after the subscription ended was not reported once")
				assert.Zero(t, d.hangups.Load(), "a NOTIFY ended the dialog")
			})
		}
	})

	t.Run("NotifyAfterTheFinalOnAnEndedDialogIs481", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var got []int
		d.onReferNotify = func(statusCode int) { got = append(got, statusCode) }
		var reports []ReferNotify
		d.OnReferNotifyUnmatched(func(n ReferNotify) { reports = append(reports, n) })

		require.NoError(t, referWithoutObserver(t.Context(), d))
		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=1", terminated))
		d.sipDialog.InitWithState(sip.DialogStateEnded)

		assert.Equal(t, sip.StatusCallTransactionDoesNotExists, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=1", terminated))
		assert.Equal(t, []int{200}, got)
		assert.Len(t, reports, 1)
	})

	t.Run("NotifyAfterTheFinalOfAnObservedAttemptIs481", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var got []int
		d.onReferNotify = func(statusCode int) { got = append(got, statusCode) }
		wait := notifyAfterRefer(t, d,
			referNotifySpec{body: "SIP/2.0 486 Busy Here", event: "refer;id=1", state: terminated},
		)

		obs, _, err := observeRefer(t.Context(), d)
		require.NoError(t, err)
		assert.Equal(t, []int{sip.StatusOK}, wait())
		assert.Equal(t, ReferEndFinalNotify, obs.End)

		assert.Equal(t, sip.StatusCallTransactionDoesNotExists, sendReferNotify(t, d, "SIP/2.0 486 Busy Here", "refer;id=1", terminated))
		assert.Equal(t, []int{486}, got, "a NOTIFY after the subscription ended reached the callback")
	})

	t.Run("NotifyForNoReferGoesToTheCallback", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var got []int
		d.onReferNotify = func(statusCode int) { got = append(got, statusCode) }
		var reports []ReferNotify
		d.OnReferNotifyUnmatched(func(n ReferNotify) { reports = append(reports, n) })

		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=7", terminated))
		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer", terminated))
		assert.Equal(t, []int{200, 200}, got)
		assert.Empty(t, reports)
	})

	t.Run("an observed attempt records its NOTIFYs and the callback is told of them", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		var got []int
		d.onReferNotify = func(statusCode int) { got = append(got, statusCode) }
		wait := notifyAfterRefer(t, d,
			referNotifySpec{body: "SIP/2.0 100 Trying", event: "refer;id=1", state: "active;expires=60"},
			referNotifySpec{body: "SIP/2.0 200 OK", event: "refer;id=1", state: terminated},
		)

		obs, _, err := observeRefer(t.Context(), d)
		require.NoError(t, err)
		assert.Equal(t, []int{sip.StatusOK, sip.StatusOK}, wait())
		assert.Equal(t, ReferEndFinalNotify, obs.End)
		assert.Equal(t, []int{100, 200}, notifyStatuses(obs))
		assert.Equal(t, []int{100, 200}, got)
		assert.Zero(t, d.hangups.Load())
		assert.Zero(t, registeredReferAttempts(&d.sessionState))
	})

	t.Run("a callback set after an unobserved REFER takes its outcome instead of the hang-up", func(t *testing.T) {
		d := newReferObserveDialog(t, 202, "Accepted")
		require.NoError(t, referWithoutObserver(t.Context(), d))
		require.Equal(t, 1, registeredReferAttempts(&d.sessionState))

		var got []int
		d.onReferNotify = func(statusCode int) { got = append(got, statusCode) }
		assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=1", terminated))
		assert.Equal(t, []int{200}, got)
		assert.Zero(t, d.hangups.Load(), "the callback and the hang-up both took the outcome")
		assert.Zero(t, registeredReferAttempts(&d.sessionState))
	})
}

// TestDialogReferObservedBesideUnobserved pins that only the final NOTIFY of a
// REFER nobody observes hangs the call up: the final NOTIFY of an observed
// REFER on the same dialog leaves the dialog to its caller, whatever else the
// dialog holds.
func TestDialogReferObservedBesideUnobserved(t *testing.T) {
	const terminated = "terminated;reason=noresource"
	d := newReferObserveDialog(t, 202, "Accepted")

	d.nextCSeq = 1
	require.NoError(t, referWithoutObserver(t.Context(), d))

	d.nextCSeq = 2
	sent := make(chan struct{})
	d.onDo = func(*sip.Request) { close(sent) }
	done := make(chan referRun, 1)
	go func() {
		obs, _, err := observeRefer(t.Context(), d)
		done <- referRun{obs, err}
	}()
	select {
	case <-sent:
	case <-time.After(time.Second):
		t.Fatal("the observed REFER was not sent")
	}

	assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 486 Busy Here", "refer;id=2", terminated))
	select {
	case run := <-done:
		require.NoError(t, run.err)
		assert.Equal(t, ReferEndFinalNotify, run.obs.End)
		assert.Equal(t, []int{486}, notifyStatuses(run.obs))
	case <-time.After(time.Second):
		t.Fatal("the observed attempt did not end on its final NOTIFY")
	}
	assert.Zero(t, d.hangups.Load(), "the observed REFER's final NOTIFY hung the call up")
	assert.Equal(t, 1, registeredReferAttempts(&d.sessionState), "the unobserved REFER's subscription was ended by another REFER's NOTIFY")

	assert.Equal(t, sip.StatusOK, sendReferNotify(t, d, "SIP/2.0 200 OK", "refer;id=1", terminated))
	assert.Equal(t, int32(1), d.hangups.Load(), "the unobserved REFER's final NOTIFY did not hang the call up")
	assert.Zero(t, registeredReferAttempts(&d.sessionState))
}

// TestIntegrationDialogReferWithoutObserver runs Refer, with no OnNotify
// callback, over real UAs: it returns once the transferee has accepted the
// REFER, and the transferee's final NOTIFY, whatever the transfer came to,
// hangs the referring call up.
func TestIntegrationDialogReferWithoutObserver(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Transferee: answers our INVITE and acts on the REFER. transfereeEnded
	// takes a value each time one of its calls has ended.
	transfereeEnded := make(chan struct{}, 2)
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15096,
				ID:        "udp",
			},
		))
		require.NoError(t, serveBackground(t, dg, ctx, func(serveCtx context.Context, d *DialogServerSession) {
			d.Answer(AnswerOptions{
				OnRefer: func(referDialog *DialogClientSession) error {
					if _, err := referDialog.Invite(referDialog.Context(), InviteClientOptions{}); err != nil {
						return err
					}
					if err := referDialog.Ack(ctx); err != nil {
						return err
					}
					return referDialog.Hangup(ctx)
				},
			})
			// The handler ends with its run as well as with its dialog: a dialog
			// whose BYE never arrived would otherwise hold serving past the test.
			select {
			case <-serveCtx.Done():
			case <-d.Context().Done():
			}
			transfereeEnded <- struct{}{}
		}))
	}

	// Refer target: refuses the busy user and answers any other.
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(
			Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				BindPort:  15097,
			},
		))
		require.NoError(t, serveBackground(t, dg, ctx, func(ctx context.Context, d *DialogServerSession) {
			if d.ToUser() == "busy" {
				d.Respond(ctx, sip.StatusBusyHere, "Busy Here", nil)
				return
			}
			d.Answer(AnswerOptions{})
			// The handler ends with its run as well as with its dialog: a dialog
			// whose BYE never arrived would otherwise hold serving past the test.
			select {
			case <-ctx.Done():
			case <-d.Context().Done():
			}
		}))
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(
		Transport{
			Transport: "udp",
			BindHost:  "127.0.0.1",
			BindPort:  15095,
		},
	))
	require.NoError(t, serveBackground(t, dg, ctx, nil))

	for _, tc := range []struct {
		name string
		user string
	}{
		{name: "a transfer that succeeds", user: "carol"},
		{name: "a transfer the target refuses", user: "busy"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: 15096}, InviteOptions{})
			require.NoError(t, err)
			defer d.Close()

			require.NoError(t, d.Refer(d.Context(), sip.Uri{User: tc.user, Host: "127.0.0.1", Port: 15097}))
			select {
			case <-d.Context().Done():
			case <-time.After(5 * time.Second):
				t.Fatal("the transfer's final NOTIFY did not hang the call up")
			}
			assert.Equal(t, sip.DialogStateEnded, d.DialogSIP().LoadState())
			assert.Nil(t, d.TerminatingBye(), "the call was ended by the far end, not by the final NOTIFY")
			// Our dialog ends as its BYE goes out. The transferee's ends when
			// the BYE reaches it, and the test leaves its UA up until then.
			select {
			case <-transfereeEnded:
			case <-time.After(5 * time.Second):
				t.Fatal("our BYE did not end the call at the transferee")
			}
		})
	}
}
