// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// challengedEarlyDialogCaller returns an outgoing dialog whose first INVITE
// is answered by a 183 from early dialog a with the answer of early, and,
// once release is closed, by a 401 challenge; the INVITE sent again with
// credentials is answered by resent.
func challengedEarlyDialogCaller(t *testing.T, early *media.MediaSession, release <-chan struct{}, resent func(req *sip.Request, deliver func(*sip.Response))) *DialogClientSession {
	t.Helper()
	return newStagedCallerDialog(t, Transport{}, func(req *sip.Request, deliver func(*sip.Response)) {
		if req.GetHeader("Authorization") != nil {
			resent(req, deliver)
			return
		}
		progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", answerOf(t, early, req))
		challenge := earlyDialogResponse(req, sip.StatusUnauthorized, "Unauthorized", "a", nil)
		challenge.AppendHeader(sip.NewHeader("WWW-Authenticate", `Digest realm="pbx", nonce="early-nonce", algorithm=MD5`))
		go func() {
			deliver(progress)
			select {
			case <-release:
			case <-t.Context().Done():
				return
			}
			deliver(challenge)
		}()
	})
}

// TestDialogClientChallengeEndsEarlyDialogs pins that a non-2xx final response
// to our INVITE, here a digest challenge, ends every early dialog of it with
// its answer (RFC 3261 section 12.3). The INVITE sent again with credentials
// is a new request (section 22.2), and the first session description of its
// responses is the answer (section 13.2.1), also from the To tag of an ended
// early dialog: the 200's, not that of the 183 before the challenge.
func TestDialogClientChallengeEndsEarlyDialogs(t *testing.T) {
	creds := sipgo.AnswerOptions{Username: "caller", Password: "secret"}

	t.Run("EarlyMediaDetect", func(t *testing.T) {
		early := newMediaSessionForTest(t)
		final := newMediaSessionForTest(t)
		release := make(chan struct{})
		d := challengedEarlyDialogCaller(t, early, release, func(req *sip.Request, deliver func(*sip.Response)) {
			ok := earlyDialogResponse(req, sip.StatusOK, "OK", "a", answerOf(t, final, req))
			go deliver(ok)
		})

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		med, err := d.Invite(ctx, InviteClientOptions{EarlyMediaDetect: true})
		require.ErrorIs(t, err, ErrClientEarlyMedia)
		requireRemote(t, med, early, "the early media is not the 183's")
		close(release)
		require.NoError(t, d.WaitAnswer(ctx, med, creds))
		requireRemote(t, med, final, "the call kept the answer of an early dialog the challenge ended")
	})
	t.Run("WithoutDetect", func(t *testing.T) {
		early := newMediaSessionForTest(t)
		final := newMediaSessionForTest(t)
		release := make(chan struct{})
		close(release)
		d := challengedEarlyDialogCaller(t, early, release, func(req *sip.Request, deliver func(*sip.Response)) {
			ok := earlyDialogResponse(req, sip.StatusOK, "OK", "a", answerOf(t, final, req))
			go deliver(ok)
		})

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		med, err := d.Invite(ctx, InviteClientOptions{Username: creds.Username, Password: creds.Password})
		require.NoError(t, err)
		requireRemote(t, med, final, "the call took the answer of an early dialog the challenge ended")
	})
	t.Run("NewEarlyDialogAnswer", func(t *testing.T) {
		early := newMediaSessionForTest(t)
		renewed := newMediaSessionForTest(t)
		final := newMediaSessionForTest(t)
		release := make(chan struct{})
		close(release)
		d := challengedEarlyDialogCaller(t, early, release, func(req *sip.Request, deliver func(*sip.Response)) {
			progress := earlyDialogResponse(req, sip.StatusSessionInProgress, "Session Progress", "a", answerOf(t, renewed, req))
			ok := earlyDialogResponse(req, sip.StatusOK, "OK", "a", answerOf(t, final, req))
			go func() {
				deliver(progress)
				deliver(ok)
			}()
		})

		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		med, err := d.Invite(ctx, InviteClientOptions{Username: creds.Username, Password: creds.Password})
		require.NoError(t, err)
		requireRemote(t, med, renewed, "the call did not take the first answer of the INVITE sent again")
	})
}
