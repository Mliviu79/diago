// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// failingEndKeyer is media whose early media fails to stop.
type failingEndKeyer struct {
	err   error
	calls int
}

func (k *failingEndKeyer) onFinalizeWithin(context.Context, time.Duration) error { return nil }

func (k *failingEndKeyer) endEarlyMedia() error {
	k.calls++
	return k.err
}

// TestDialogClientEndEarlyMediaFailureKeepsFinalResponse pins that media
// failing to stop at a non-2xx final response to our INVITE does not hide that
// response: the wait returns the *sipgo.ErrDialogResponse of the final, with
// its status, and the media's error beside it. A challenge whose media fails
// to stop is returned the same way and not answered.
func TestDialogClientEndEarlyMediaFailureKeepsFinalResponse(t *testing.T) {
	for _, tc := range []struct {
		name   string
		final  func(req *sip.Request) *sip.Response
		status int
	}{
		{name: "busy", status: sip.StatusBusyHere, final: func(req *sip.Request) *sip.Response {
			return earlyDialogResponse(req, sip.StatusBusyHere, "Busy Here", "a", nil)
		}},
		{name: "challenge", status: sip.StatusUnauthorized, final: func(req *sip.Request) *sip.Response {
			res := earlyDialogResponse(req, sip.StatusUnauthorized, "Unauthorized", "a", nil)
			res.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="pbx", nonce="end-nonce", algorithm=MD5`))
			return res
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			invites := 0
			d := newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
				invites++
				final := tc.final(req)
				go deliver(final)
			})
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			require.NoError(t, d.doInvite(ctx, []byte("v=0\r\n")))

			mediaErr := errors.New("media did not stop")
			keyer := &failingEndKeyer{err: mediaErr}
			err := d.awaitInviteAnswer(ctx, keyer, sipgo.AnswerOptions{Username: "caller", Password: "secret"}, d.DialogClientSession.WaitAnswer)

			var res *sipgo.ErrDialogResponse
			require.ErrorAs(t, err, &res, "the final response is hidden")
			assert.Equal(t, tc.status, res.Res.StatusCode)
			require.ErrorIs(t, err, mediaErr)
			assert.Equal(t, 1, keyer.calls)
			assert.Equal(t, 1, invites, "the INVITE was sent again with the early media running")
		})
	}
}
