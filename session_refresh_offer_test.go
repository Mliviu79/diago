// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestSessionRefreshOffersTheSessionInPlace pins the offer of the session
// refresh we send: RFC 4028 section 7.4 has a re-INVITE that refreshes the
// session carry an offer that indicates the session has not changed, so it
// carries the codec the call runs on and no other, also on a Diago whose
// ReInvite offers every configured codec, as it does by default.
func TestSessionRefreshOffersTheSessionInPlace(t *testing.T) {
	p := newRefreshPeer(t, nil)
	d := p.call(t, weRefreshCall...)

	// The peer offered PCMU alone, so the call runs on it.
	refresh, from := p.await(t, sip.INVITE, refreshDue)
	refreshMedia := sdpLineWithPrefix(t, refresh.Body(), "m=audio ")
	assert.Regexp(t, `^m=audio \d+ RTP/AVP 0$`, refreshMedia, "the refresh asked to renegotiate the codecs")
	p.answerOK(t, refresh, from, "Session-Expires: 90;refresher=uac")

	// A ReInvite of the application's is no refresh: it offers every codec
	// the media was set up with.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	reinvited := make(chan error, 1)
	go func() { reinvited <- d.ReInvite(ctx) }()
	plain, from := p.await(t, sip.INVITE, 2*time.Second)
	assert.Regexp(t, `^m=audio \d+ RTP/AVP 0 8( \d+)*$`, sdpLineWithPrefix(t, plain.Body(), "m=audio "))
	assert.Equal(t, "90;refresher=uac", headerValue(plain, "Session-Expires"), "a re-INVITE of ours carries the session timer while it runs")
	p.answerOK(t, plain, from, "Session-Expires: 90;refresher=uac")
	select {
	case err := <-reinvited:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "ReInvite did not return")
	}
}
