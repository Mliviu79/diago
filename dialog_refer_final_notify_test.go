// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIntegrationDialogReferFinalNotifyOnce runs a transfer over real UAs and
// checks the transferee sends the NOTIFY that terminates the refer
// subscription once: after it the subscription is over and the notifier sends
// nothing more for it (RFC 6665 section 4.1.3; RFC 3515 section 2.4.5). The
// referred call is confirmed and then hung up, and its end after the
// confirmation sends no second final NOTIFY; a referred call the target refuses
// gets its failure NOTIFY once.
func TestIntegrationDialogReferFinalNotifyOnce(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Refer target: refuses the busy user and answers any other.
	var targetPort int
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))
		require.NoError(t, serveBackground(t, dg, ctx, func(ctx context.Context, d *DialogServerSession) {
			if d.ToUser() == "busy" {
				d.Respond(ctx, sip.StatusBusyHere, "Busy Here", nil)
				return
			}
			_, _ = d.Answer(AnswerOptions{})
			select {
			case <-ctx.Done():
			case <-d.Context().Done():
			}
		}))
		tran, _ := dg.getTransport("udp")
		targetPort = tran.BindPort
	}

	// Transferee: answers our INVITE and acts on the REFER, hanging the
	// referred call up once it is confirmed. referDone takes a value once
	// each REFER has been acted on: every NOTIFY the referred call's end
	// sends has been answered by then.
	referDone := make(chan struct{}, 2)
	var transfereePort int
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0, ID: "udp"}))
		require.NoError(t, serveBackground(t, dg, ctx, func(serveCtx context.Context, d *DialogServerSession) {
			_, _ = d.Answer(AnswerOptions{
				OnRefer: func(referDialog *DialogClientSession) error {
					defer func() { referDone <- struct{}{} }()
					if _, err := referDialog.Invite(referDialog.Context(), InviteClientOptions{}); err != nil {
						return err
					}
					if err := referDialog.Ack(ctx); err != nil {
						return err
					}
					return referDialog.Hangup(ctx)
				},
			})
			select {
			case <-serveCtx.Done():
			case <-d.Context().Done():
			}
		}))
		tran, _ := dg.getTransport("udp")
		transfereePort = tran.BindPort
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	require.NoError(t, serveBackground(t, dg, ctx, nil))

	for _, tc := range []struct {
		name string
		user string
		want []int
	}{
		{name: "ConfirmedThenEnded", user: "carol", want: []int{100, 200}},
		{name: "RefusedByTheTarget", user: "busy", want: []int{100, 486}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, _, err := dg.Invite(ctx, sip.Uri{Host: "127.0.0.1", Port: transfereePort}, InviteOptions{})
			require.NoError(t, err)
			defer d.Close()
			defer d.Hangup(ctx)

			unmatched := make(chan ReferNotify, 4)
			d.OnReferNotifyUnmatched(func(n ReferNotify) { unmatched <- n })
			// Buffered beyond the NOTIFYs expected, so a NOTIFY sent after
			// the final one is counted rather than holding its handler.
			referState := make(chan int, 4)
			require.NoError(t, d.ReferOptions(d.Context(), sip.Uri{User: tc.user, Host: "127.0.0.1", Port: targetPort}, ReferClientOptions{
				OnNotify: func(statusCode int) { referState <- statusCode },
			}))

			select {
			case <-referDone:
			case <-time.After(5 * time.Second):
				t.Fatal("the transferee did not act on the REFER")
			}

			var got []int
			for len(got) < len(tc.want) {
				select {
				case code := <-referState:
					got = append(got, code)
				case <-time.After(5 * time.Second):
					t.Fatalf("NOTIFYs = %v, want %v", got, tc.want)
				}
			}
			assert.Equal(t, tc.want, got)

			// The transferee has acted on the REFER, so any NOTIFY it sent
			// has been answered: none may have come after the final one.
			select {
			case code := <-referState:
				t.Errorf("a NOTIFY %d reached OnNotify after the final one", code)
			default:
			}
			select {
			case n := <-unmatched:
				t.Errorf("a NOTIFY %d came after the final one", n.Status)
			default:
			}
		})
	}
}
