// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
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

// originOf returns the session id and the session version of the o= line of
// body, and the rest of that line.
func originOf(t *testing.T, body []byte) (id, version uint64, rest string) {
	t.Helper()
	fields := strings.SplitN(sdpLineWithPrefix(t, body, "o="), " ", 4)
	require.Len(t, fields, 4, "the o= line of:\n%s", body)
	id, err := strconv.ParseUint(fields[1], 10, 64)
	require.NoError(t, err)
	version, err = strconv.ParseUint(fields[2], 10, 64)
	require.NoError(t, err)
	return id, version, fields[0] + " " + fields[3]
}

// directionOf returns the direction attribute of body.
func directionOf(t *testing.T, body []byte) string {
	t.Helper()
	for line := range strings.SplitSeq(string(body), "\r\n") {
		switch line {
		case "a=" + sdp.ModeSendrecv, "a=" + sdp.ModeSendonly, "a=" + sdp.ModeRecvonly, "a=" + sdp.ModeInactive:
			return strings.TrimPrefix(line, "a=")
		}
	}
	t.Fatalf("no direction attribute in the session description:\n%s", body)
	return ""
}

// peerOffering is the SDP the peer of these tests sends with a direction.
func peerOffering(mode string) []byte {
	return sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34800, mode, []string{sdp.FORMAT_TYPE_ULAW})
}

// TestSessionRefreshKeepsTheOriginOfAnUnchangedSession pins the o= line of a
// session refresh. RFC 4028 section 7.4: a re-INVITE that refreshes the
// session "SHOULD contain [an offer], even if the details of the session have
// not changed. In that case, the offer MUST indicate that it has not changed.
// In the case of SDP, this is accomplished by including the same value for the
// origin field as did previous SDP messages to its peer. The same is true for
// an answer exchanged as a result of a session refresh request". RFC 3264
// section 8 has an SDP whose origin version does not increment "be identical
// to the SDP with that version number", so the refresh of a call nothing has
// changed is the SDP we answered it with, and so is our answer to the peer's.
func TestSessionRefreshKeepsTheOriginOfAnUnchangedSession(t *testing.T) {
	t.Run("our_refresh", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, weRefreshCall...)

		for range 2 {
			refresh, from := p.await(t, sip.INVITE, refreshDue)
			assert.Equal(t, string(p.answerSDP), string(refresh.Body()), "the refresh is not the SDP sent before it")
			p.answerOK(t, refresh, from, "Session-Expires: 90;refresher=uac")
		}
	})

	t.Run("our_answer_to_the_peers_refresh", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, peerRefreshesCall...)

		for range 2 {
			res := p.reInvite(t, "Supported: timer", "Session-Expires: 90;refresher=uac")
			require.Equal(t, sip.StatusOK, res.StatusCode)
			assert.Equal(t, string(p.answerSDP), string(res.Body()), "the answer is not the SDP sent before it")
		}
	})
}

// TestSessionRefreshOnAHeldCall pins what a session refresh offers on a call
// that is on hold, and that the hold survives it: a refresh does not change
// the session (RFC 4028 section 7.4). RFC 6337 section 5.3 has a UA that
// sends an offer "without any desire to change the hold status (e.g., [...]
// when sending a re-INVITE to refresh the session timer)" offer its own
// state: "If it previously initiated a "hold" by sending "a=sendonly"
// attribute or "a=inactive" attribute, then it should offer that again. If it
// had not previously initiated "hold", then it should offer "a=sendrecv"
// attribute, even if it had previously been forced to answer something else."
// The peer that holds the call says so again in its answer.
//
// Each description that differs from the one sent before it takes the next
// origin version, and the refresh that repeats the one before keeps it.
func TestSessionRefreshOnAHeldCall(t *testing.T) {
	t.Run("held_by_us", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		d := p.call(t, weRefreshCall...)
		id, version, rest := originOf(t, p.answerSDP)

		refresh, from := p.await(t, sip.INVITE, refreshDue)
		p.answerOK(t, refresh, from, "Session-Expires: 90;refresher=uac")

		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		held := make(chan error, 1)
		go func() { held <- d.Hold(ctx) }()
		hold, from := p.await(t, sip.INVITE, 2*time.Second)
		assert.Equal(t, sdp.ModeSendonly, directionOf(t, hold.Body()))
		holdID, holdVersion, holdRest := originOf(t, hold.Body())
		assert.Equal(t, id, holdID)
		assert.Equal(t, rest, holdRest)
		assert.Equal(t, version+1, holdVersion, "an offer that modifies the session increments the version by one")
		p.answer(t, hold, from, sip.StatusOK, "OK", peerOffering(sdp.ModeRecvonly), "Session-Expires: 90;refresher=uac")
		select {
		case err := <-held:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "Hold did not return")
		}

		// The hold offered every codec the media was set up with, and the
		// refresh offers the one the call runs on: it differs in that, and
		// so takes the next version. The refresh after it changes nothing.
		first, from := p.await(t, sip.INVITE, refreshDue)
		assert.Equal(t, sdp.ModeSendonly, directionOf(t, first.Body()), "the refresh did not offer our hold again")
		firstID, firstVersion, firstRest := originOf(t, first.Body())
		assert.Equal(t, id, firstID)
		assert.Equal(t, rest, firstRest)
		assert.Equal(t, holdVersion+1, firstVersion)
		p.answer(t, first, from, sip.StatusOK, "OK", peerOffering(sdp.ModeRecvonly), "Session-Expires: 90;refresher=uac")

		second, from := p.await(t, sip.INVITE, refreshDue)
		assert.Equal(t, string(first.Body()), string(second.Body()), "the refresh is not the SDP sent before it")
		p.answer(t, second, from, sip.StatusOK, "OK", peerOffering(sdp.ModeRecvonly), "Session-Expires: 90;refresher=uac")
	})

	t.Run("held_by_the_peer", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, weRefreshCall...)
		id, version, rest := originOf(t, p.answerSDP)

		refresh, from := p.await(t, sip.INVITE, refreshDue)
		p.answerOK(t, refresh, from, "Session-Expires: 90;refresher=uac")
		p.await(t, sip.ACK, 2*time.Second)

		res := p.reInviteOffering(t, peerOffering(sdp.ModeSendonly), "Supported: timer", "Session-Expires: 90;refresher=uas")
		require.Equal(t, sip.StatusOK, res.StatusCode)
		assert.Equal(t, sdp.ModeRecvonly, directionOf(t, res.Body()))
		heldID, heldVersion, heldRest := originOf(t, res.Body())
		assert.Equal(t, id, heldID)
		assert.Equal(t, rest, heldRest)
		assert.Equal(t, version+1, heldVersion)

		// We did not hold the call, so the refresh offers sendrecv, which
		// is not the SDP sent before it. The peer answers with its hold.
		first, from := p.await(t, sip.INVITE, refreshDue)
		assert.Equal(t, sdp.ModeSendrecv, directionOf(t, first.Body()))
		firstID, firstVersion, firstRest := originOf(t, first.Body())
		assert.Equal(t, id, firstID)
		assert.Equal(t, rest, firstRest)
		assert.Equal(t, heldVersion+1, firstVersion)
		p.answer(t, first, from, sip.StatusOK, "OK", peerOffering(sdp.ModeSendonly), "Session-Expires: 90;refresher=uac")

		second, from := p.await(t, sip.INVITE, refreshDue)
		assert.Equal(t, string(first.Body()), string(second.Body()), "the refresh is not the SDP sent before it")
		p.answer(t, second, from, sip.StatusOK, "OK", peerOffering(sdp.ModeSendonly), "Session-Expires: 90;refresher=uac")
		p.await(t, sip.ACK, 2*time.Second)

		// The call is still held by the peer: asked again, we answer as
		// we did when it put the call on hold, with that same SDP's
		// direction.
		res = p.reInviteOffering(t, peerOffering(sdp.ModeSendonly), "Supported: timer", "Session-Expires: 90;refresher=uas")
		require.Equal(t, sip.StatusOK, res.StatusCode)
		assert.Equal(t, sdp.ModeRecvonly, directionOf(t, res.Body()))
	})
}
