// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/emiago/sipgo/sip"
)

// ErrEarlyMediaNotKeyed is returned by ProgressMedia when the caller did not key
// DTLS-SRTP early media within ProgressMediaOptions.KeyTimeout, and by
// ProgressMediaWebrtc within ProgressMediaWebrtcOptions.KeyTimeout, most likely
// because it ignores early media (RFC 3960) and takes part in the DTLS
// handshake only once the call is answered.
//
// The call stays answerable. The media ProgressMedia returned with it has no
// audio reader or writer, and its media session refuses to send with
// media.ErrDTLSNotKeyed; nothing may use it before the answer.
// AnswerEarlyMedia with that media, or AnswerContext, sends the 200 and then
// completes the handshake ProgressMedia started, bounded by the call and by
// media.DTLSHandshakeTimeout, before the media is set up. When that handshake
// has failed by then, the answer declines the call 488 instead. The media
// ProgressMediaWebrtc returned with it has no transport set up until
// AnswerWebrtcContext, or AnswerWebrtc, has answered the call the same way.
var ErrEarlyMediaNotKeyed = errors.New("early media not keyed: the caller has not completed the DTLS handshake")

// errEarlyMediaOtherType refuses an answer with media of one type to a call
// that ProgressMedia or ProgressMediaWebrtc gave early media of the other: the
// 2xx repeats the answer of the 183 (RFC 3261 section 13.2.1), and only the
// media that made that answer carries the session it describes.
var errEarlyMediaOtherType = errors.New("answer: the early media of the call is of another media type")

// keyEarlyMedia has keyer, the media whose answer the 183 just sent carried
// as answer, keyed by the caller, and records that answer for the 2xx to
// repeat. It returns nil once the early media is keyed, at once for media
// that needs no keys.
//
// A caller that treats the answer in the 183 as the answer (RFC 3261 section
// 13.2.1) keys the early media with us: we take setup:active and start the
// DTLS handshake at once (RFC 5763 section 6.2), and no media may go out
// before it completes (RFC 5764 section 5.1). A caller may also ignore early
// media (RFC 3960) and take part only after the 200, so the wait is bounded by
// wait, five seconds when it is zero, and then ends with
// ErrEarlyMediaNotKeyed. It ends earlier, with the handshake's error, when the
// caller gives up and the dialog ends.
//
// The handshake is not abandoned with the wait. The ClientHellos already sent
// wait on the caller's socket until its DTLS stack reads them after the 200:
// it completes this association from them, and fails a new one they are read
// ahead of. It runs on with the dialog, and the answer waits for it instead.
func (d *DialogServerSession) keyEarlyMedia(keyer mediaEarlyKeyer, answer []byte, wait time.Duration) error {
	if wait <= 0 {
		wait = defaultEarlyMediaKeyTimeout
	}
	// The dialog's ctx is the bound of the handshake, which outlives the wait.
	if err := keyer.onFinalizeWithin(d.Context(), wait); err != nil {
		if errors.Is(err, errMediaKeyingInProgress) {
			d.noteEarlyAnswer(answer, true)
			return ErrEarlyMediaNotKeyed
		}
		return err
	}
	d.noteEarlyAnswer(answer, false)
	return nil
}

// earlyAnswerOf returns the answer the 183 of ProgressMedia or
// ProgressMediaWebrtc carried when med is the media that it made, and false
// for any other media, which describes itself in the 2xx.
func (d *DialogServerSession) earlyAnswerOf(med mediaEarlyKeyer) (earlyAnswer, bool) {
	registered, ok := d.mediaHanshakerLocked().(mediaEarlyKeyer)
	if !ok || registered != med {
		return earlyAnswer{}, false
	}
	return d.earlyAnswer()
}

// earlyMedia returns the media ProgressMedia or ProgressMediaWebrtc gave the
// call, with the answer its 183 carried, and false for a call without early
// media.
func (d *DialogServerSession) earlyMedia() (mediaEarlyKeyer, earlyAnswer, bool) {
	med, ok := d.mediaHanshakerLocked().(mediaEarlyKeyer)
	if !ok {
		return nil, earlyAnswer{}, false
	}
	early, ok := d.earlyAnswer()
	return med, early, ok
}

// answerEarlyMediaWithin answers, within ctx, a call that has the early media
// med, as AnswerContext describes.
func (d *DialogServerSession) answerEarlyMediaWithin(ctx context.Context, med mediaEarlyKeyer, early earlyAnswer) error {
	if ctx.Err() != nil {
		return fmt.Errorf("answer: %w", context.Cause(ctx))
	}
	// RFC 4028: negotiate session timers before the 200 OK, as an answer
	// without early media does.
	if err := d.prepareSessionTimers(ctx); err != nil {
		return err
	}
	return d.answerEarlyWithin(ctx, med, early)
}

// answerEarlyWithin answers 200 a call whose 183 carried early as its answer,
// and waits for the ACK within ctx, see answerWithin. The 200 repeats that
// answer byte for byte: RFC 3261 section 13.2.1 lets the answer, which is in
// the 2xx, also be placed in a provisional response as "that same exact
// answer", and has the caller take the first session description it receives
// as the answer and ignore those in later responses. The media is not asked to
// describe itself again, which it may not be while a handshake is running on
// it.
//
// Early media the caller had not keyed, which ProgressMedia reported with
// ErrEarlyMediaNotKeyed, is keyed after the 200: a caller that ignored the 183
// takes part in the handshake only then. The reading of the ACK has keyer
// wait for the handshake ProgressMedia left running, bounded by the call and
// by media.DTLSHandshakeTimeout, and set the media up, see readInviteAck. An
// in-dialog request waits for all of it.
//
// A handshake that has already failed leaves the call no media to answer
// with, so the call is declined 488 rather than answered 200 and ended with a
// BYE: RFC 3261 section 21.4.26 gives 488 the meaning of 606, a session
// description the callee cannot support, and RFC 5763 section 5 has the media
// session torn down on a certificate that does not match its fingerprint. The
// error then joins why the handshake failed with how the refusal went.
func (d *DialogServerSession) answerEarlyWithin(ctx context.Context, keyer mediaEarlyKeyer, early earlyAnswer) error {
	if early.unkeyed {
		// Asked without waiting: the wait for the keys follows the 200.
		if err := keyer.onFinalizeWithin(d.Context(), 0); err != nil && !errors.Is(err, errMediaKeyingInProgress) {
			// The 488 waits for its ACK within ctx, as the 200 would.
			return errors.Join(err, d.answerWithin(ctx, d.markAnswering(), func() error {
				return d.Respond(d.Context(), sip.StatusNotAcceptableHere, "Not Acceptable Here", nil)
			}))
		}
	}
	return d.respondSDPWithin(ctx, early.sdp)
}
