// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"time"

	"github.com/emiago/sipgo/sip"
)

// takeEarlyWebrtcAnswer is what the response callback of a WebRTC INVITE
// with EarlyMediaDetect does with res: a 183 with a session description has
// that description applied to med, the media of the INVITE, as the answer to
// its offer, and the early media keyed by the callee within wait, five
// seconds when it is zero, see keyEarlyAnswer. It returns ErrClientEarlyMedia
// once that media is keyed, which then carries audio.
//
// Early media the callee has not keyed by then is not reported: it returns
// nil, the wait for the INVITE's answer goes on, the answer of the 183 stands
// and the negotiation it started runs on, for the ACK to complete. Any other
// response, and a 183 after the one taken, brings nothing: the first session
// description is the answer (RFC 3261 section 13.2.1).
func (d *DialogClientSession) takeEarlyWebrtcAnswer(ctx context.Context, med *DialogWebrtc, res *sip.Response, wait time.Duration) error {
	answer := earlySessionDescription(res)
	if answer == nil || med.MediaSession() != nil || d.earlyKeyingLeft() {
		return nil
	}
	if err := med.onRemoteSDP(ctx, answer, true); err != nil {
		return err
	}
	keyed, err := d.keyEarlyAnswer(ctx, med, wait)
	if !keyed {
		return err
	}
	return ErrClientEarlyMedia
}

// earlySessionDescription returns the session description res carries when
// it is a 183 Session Progress with one, and nil for any other response.
func earlySessionDescription(res *sip.Response) []byte {
	if res.StatusCode != sip.StatusSessionInProgress {
		return nil
	}
	contentType := res.ContentType()
	if contentType == nil || contentType.Value() != "application/sdp" {
		return nil
	}
	return res.Body()
}

// applyEarlyWebrtcFinalAnswer applies the session description of the 2xx to
// an INVITE whose 183 set med up to med, in place, as the 2xx after early
// media of DialogMedia is applied: the answer of the 183 stood for the call
// so far, and the 2xx describes the session it continues. A 2xx without a
// session description leaves the media as the 183 set it up. When the
// negotiation of the 183 was left running, the answer of the 183 stands
// (RFC 3261 section 13.2.1) and nothing is applied: the ACK completes it.
func (d *DialogClientSession) applyEarlyWebrtcFinalAnswer(ctx context.Context, med *DialogWebrtc) error {
	if d.earlyKeyingLeft() {
		return nil
	}
	answer := d.InviteResponse.Body()
	if answer == nil {
		return nil
	}
	return med.onRemoteSDP(ctx, answer, true)
}
