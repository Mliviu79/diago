// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// TestDialogClientInviteKeepsCallerFromTag has a caller build the INVITE
// before sending it, which gives the From its tag, and key its dialog by that
// tag, as ai-runtime's outbound dial does: the INVITE the callee reads carries
// the same tag, so the caller's key names the dialog on the wire.
func TestDialogClientInviteKeepsCallerFromTag(t *testing.T) {
	tags := make(chan string, 1)
	callee, methods := newUDPCallee(t, func(req *sip.Request) []*sip.Response {
		if req.Method != sip.INVITE {
			return nil
		}
		tag, _ := req.From().Params.Get("tag")
		select {
		case tags <- tag:
		default:
		}
		return []*sip.Response{calleeResponse(req, sip.StatusBusyHere, "Busy Here", nil)}
	})
	d, _ := newDialogToCallee(t, callee)
	require.NoError(t, sipgo.ClientRequestBuild(d.UA.Client, d.InviteRequest))
	want, ok := d.InviteRequest.From().Params.Get("tag")
	require.True(t, ok, "the built INVITE's From has no tag")

	_, err := d.Invite(t.Context(), InviteClientOptions{})
	var resErr *sipgo.ErrDialogResponse
	require.True(t, errors.As(err, &resErr), "Invite() = %v, want *sipgo.ErrDialogResponse", err)
	requireCalleeRead(t, methods, sip.INVITE)

	select {
	case got := <-tags:
		require.Equal(t, want, got, "From tag the callee read")
	case <-time.After(5 * time.Second):
		t.Fatal("the callee read no INVITE")
	}
	fromTag, _ := d.InviteRequest.From().Params.Get("tag")
	require.Equal(t, want, fromTag, "From tag of the dialog's INVITE")
}
