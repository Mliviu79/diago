// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"testing"
	"testing/synctest"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// TestReferSubscriptionBoundEndsTheAttempt checks that the deadline the NOTIFYs
// of a REFER attempt set ends it on our side, at that instant and no later. The
// REFER's 2xx starts the subscription with Timer N, 64×T1. The first NOTIFY that
// does not end it replaces that wait with its own duration, even a longer one, and
// a later NOTIFY can only bring the deadline earlier. A NOTIFY after the end is
// refused.
func TestReferSubscriptionBoundEndsTheAttempt(t *testing.T) {
	type notifyAt struct {
		at    time.Duration
		state string
	}
	const huge = "active;expires=4294967295"
	timerN := 64 * sip.T1
	for _, tt := range []struct {
		name     string
		notifies []notifyAt
		endsAt   time.Duration
		want     ReferEnd
	}{
		{
			name:     "an expires above the cap ends the attempt at the cap",
			notifies: []notifyAt{{0, huge}},
			endsAt:   time.Hour,
			want:     ReferEndSubscriptionExpired,
		},
		{
			name:     "an expires below the cap ends the attempt when it passes",
			notifies: []notifyAt{{0, "active;expires=60"}},
			endsAt:   time.Minute,
			want:     ReferEndSubscriptionExpired,
		},
		{
			// A peer that re-sends a huge expires every 59 minutes must not hold
			// the attempt past the first deadline.
			name:     "a huge expires repeated every 59 minutes does not extend the attempt",
			notifies: []notifyAt{{0, huge}, {59 * time.Minute, huge}},
			endsAt:   time.Hour,
			want:     ReferEndSubscriptionExpired,
		},
		{
			name:     "a later NOTIFY with a larger expires cannot lengthen the attempt",
			notifies: []notifyAt{{0, "active;expires=60"}, {30 * time.Second, "active;expires=3600"}},
			endsAt:   time.Minute,
			want:     ReferEndSubscriptionExpired,
		},
		{
			name:     "a later NOTIFY with a smaller expires shortens the attempt",
			notifies: []notifyAt{{0, "active;expires=3600"}, {10 * time.Minute, "active;expires=60"}},
			endsAt:   11 * time.Minute,
			want:     ReferEndSubscriptionExpired,
		},
		{
			// The first NOTIFY arrives at 10s, before Timer N would end the attempt
			// at 32s, and gives a longer expires: its deadline must replace the
			// 64×T1 one, not be cut short by it.
			name:     "a first NOTIFY with a longer expires replaces Timer N",
			notifies: []notifyAt{{10 * time.Second, "active;expires=60"}},
			endsAt:   70 * time.Second,
			want:     ReferEndSubscriptionExpired,
		},
		{
			name:     "a first NOTIFY without expires counts 64×T1 from its arrival",
			notifies: []notifyAt{{10 * time.Second, "active"}},
			endsAt:   10*time.Second + timerN,
			want:     ReferEndSubscriptionExpired,
		},
		{
			name:   "no NOTIFY ends the attempt at Timer N",
			endsAt: timerN,
			want:   ReferEndNoNotify,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				m := &sessionState{}
				a := m.beginReferAttempt()
				// The REFER's 2xx starts the subscription before any NOTIFY arrives.
				m.startReferSubscription(a)

				// state reads the attempt under referMu, which the deadline timer also takes.
				state := func() (ReferEnd, bool) {
					m.referMu.Lock()
					defer m.referMu.Unlock()
					select {
					case <-a.done:
						return a.end, true
					default:
						return a.end, false
					}
				}

				var elapsed time.Duration
				advanceTo := func(at time.Duration) {
					time.Sleep(at - elapsed)
					synctest.Wait()
					elapsed = at
				}
				notify := func(state string) *referAttempt {
					return m.recordReferNotify(ReferNotify{Status: 100, SubscriptionState: state, HasSubscriptionState: true})
				}

				for _, n := range tt.notifies {
					advanceTo(n.at)
					require.NotNil(t, notify(n.state), "the NOTIFY at %v was refused", n.at)
				}

				advanceTo(tt.endsAt - time.Nanosecond)
				end, retired := state()
				require.False(t, retired, "the attempt ended before its deadline")
				require.Zero(t, end)

				advanceTo(tt.endsAt)
				end, retired = state()
				require.True(t, retired, "the attempt was still open at its deadline")
				require.Equal(t, tt.want, end)
				require.Nil(t, notify("active;expires=60"), "a NOTIFY after the end was accepted")
			})
		})
	}
}

// TestReferNotifyAfterDeadlineIsRefused checks a NOTIFY that arrives after the
// attempt's deadline passed, before the deadline's timer ran, is refused as one
// after the end is: the attempt ends as that timer would have ended it, and the
// NOTIFY is not recorded.
func TestReferNotifyAfterDeadlineIsRefused(t *testing.T) {
	for _, tt := range []struct {
		name     string
		notified bool
		want     ReferEnd
	}{
		{name: "after a NOTIFY set the deadline", notified: true, want: ReferEndSubscriptionExpired},
		{name: "after Timer N with no NOTIFY", notified: false, want: ReferEndNoNotify},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &sessionState{}
			a := m.beginReferAttempt()
			// Set the deadline in the past with no timer armed: an attempt whose
			// deadline has passed but whose timer has not ended it yet.
			m.referMu.Lock()
			a.notified = tt.notified
			a.deadline = time.Now().Add(-time.Second)
			m.referMu.Unlock()

			got := m.recordReferNotify(ReferNotify{Status: 100, SubscriptionState: "active;expires=60", HasSubscriptionState: true})
			require.Nil(t, got, "a NOTIFY after the deadline was recorded")

			m.referMu.Lock()
			defer m.referMu.Unlock()
			require.Equal(t, tt.want, a.end)
			require.Empty(t, a.notifies, "a NOTIFY after the deadline was kept")
			require.False(t, a.registered, "the attempt was not retired")
		})
	}
}
