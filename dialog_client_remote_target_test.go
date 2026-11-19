// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// sentRequestURIs records the method and the Request-URI of every request a
// call sends.
type sentRequestURIs struct {
	mu   sync.Mutex
	sent []string
}

func (s *sentRequestURIs) record(req *sip.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, req.Method.String()+" "+req.Recipient.String())
}

func (s *sentRequestURIs) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = nil
}

func (s *sentRequestURIs) requests() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

// TestDialogClientRequestsWithoutRemoteContact pins where an outbound call
// whose INVITE was answered 2xx without Contact sends its requests. RFC 3261
// section 12.1.2 sets the remote target from that Contact and gives none for
// an answer without one, which is accepted: its ACK and the BYE go to the
// Request-URI of the INVITE. Every other request of the dialog, a re-INVITE
// of ours, the ACK of its 2xx and a REFER, goes there too (section 12.2.1.1
// puts the remote target in the Request-URI). They dereferenced the missing
// Contact and panicked.
func TestDialogClientRequestsWithoutRemoteContact(t *testing.T) {
	peer := sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}
	sent := &sentRequestURIs{}
	answerPort := 34455
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		sent.record(req)
		if !req.IsInvite() {
			return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
		}
		// Neither the 2xx to the INVITE nor one to a re-INVITE carries a
		// Contact.
		answerPort++
		answer := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), answerPort, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ULAW})
		res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", answer)
		res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		return res
	})

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	d, err := dg.NewDialog(peer, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	_, err = d.Invite(ctx, InviteClientOptions{})
	require.NoError(t, err)
	require.Nil(t, d.InviteResponse.Contact(), "the 2xx to the INVITE must carry no Contact")
	require.Nil(t, d.RemoteContact())
	require.NoError(t, d.Ack(ctx))

	target := peer.String()
	referTo := sip.Uri{User: "transfer", Host: "127.0.0.3", Port: 5060}
	// observed bounds the REFERs that wait for a NOTIFY the peer never sends:
	// only where the REFER went is checked.
	observed := func(ctx context.Context) (context.Context, context.CancelFunc) {
		return context.WithTimeout(ctx, 100*time.Millisecond)
	}
	for _, tc := range []struct {
		name string
		send func(ctx context.Context) error
		// want is what the call sends.
		want []string
	}{
		{name: "ReInvite", send: d.ReInvite, want: []string{"INVITE " + target, "ACK " + target}},
		{name: "Hold", send: d.Hold, want: []string{"INVITE " + target, "ACK " + target}},
		{name: "Unhold", send: d.Unhold, want: []string{"INVITE " + target, "ACK " + target}},
		{name: "keepalive", send: d.reInviteKeepAlive, want: []string{"INVITE " + target, "ACK " + target}},
		{name: "ReferOptions", send: func(ctx context.Context) error {
			return d.ReferOptions(ctx, referTo, ReferClientOptions{OnNotify: func(int) {}})
		}, want: []string{"REFER " + target}},
		{name: "Refer", send: func(ctx context.Context) error {
			return d.Refer(ctx, referTo)
		}, want: []string{"REFER " + target}},
		{name: "ReferAndWait", send: func(ctx context.Context) error {
			ctx, cancel := observed(ctx)
			defer cancel()
			// The outcome never comes; the REFER is what is checked.
			if err := d.ReferAndWait(ctx, referTo); err == nil {
				t.Error("ReferAndWait returned no error without a NOTIFY")
			}
			return nil
		}, want: []string{"REFER " + target}},
		{name: "ReferAndObserve", send: func(ctx context.Context) error {
			ctx, cancel := observed(ctx)
			defer cancel()
			_, err := d.ReferAndObserve(ctx, referTo, ReferObserveOptions{})
			return err
		}, want: []string{"REFER " + target}},
		{name: "Bye", send: d.Bye, want: []string{"BYE " + target}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sent.reset()
			var recovered any
			var err error
			func() {
				defer func() { recovered = recover() }()
				err = tc.send(ctx)
			}()
			require.Nil(t, recovered, "%s panicked", tc.name)
			require.NoError(t, err)
			require.Equal(t, tc.want, sent.requests())
		})
	}
}
