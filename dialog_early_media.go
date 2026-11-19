// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"time"
)

// defaultEarlyMediaKeyTimeout is how long ProgressMedia waits for a caller to
// key DTLS-SRTP early media when ProgressMediaOptions leaves it unset, and
// how long a call placed with EarlyMediaDetect waits for the callee when
// InviteClientOptions leaves it unset.
//
// A peer that takes the answer in the 183 keys it within a few round trips: a
// DTLS 1.2 handshake with a cookie exchange is three, about a second on a 300
// ms path. Five seconds also covers two lost flights, whose retransmission
// timers start at one second and double (RFC 6347 section 4.2.4.1). A peer
// that ignores the 183 (RFC 3960) never keys it, and costs the application
// this long before it learns early media is not available. Too short a bound
// costs only the early media, never the call, since the handshake is not
// abandoned with the wait.
const defaultEarlyMediaKeyTimeout = 5 * time.Second

// A mediaEarlyKeyer is media that can be keyed before the answer it gave, or
// took, is final: the answer went out, or came in, in a provisional response,
// and the peer is free to act on it only once the call is answered (RFC 3960).
// It is the one call a session makes on its media beside those of
// mediaHanshaker, on the same media.
type mediaEarlyKeyer interface {
	// onFinalizeWithin has the media run the negotiation its session
	// description left it, as onFinalize does, and waits for it at most wait.
	// It returns nil once the media is keyed, or needs no keys, and the
	// negotiation's error when it failed, from then on at every call.
	//
	// When the peer has not completed the negotiation within wait, the error
	// is errMediaKeyingInProgress and the negotiation goes on: it cannot be
	// run a second time in its place. It is bounded by ctx, which is the
	// call's and so outlives this call, and by the media's close. A later
	// onFinalize waits for it, bounds it as it bounds any other, and sets up
	// what carries the media, which has nothing to carry it meanwhile. A wait
	// of zero or less does not wait: it reports how the negotiation stands.
	//
	// Nothing else may be asked of the media between a call that left the
	// negotiation running and the onFinalize that completes it.
	onFinalizeWithin(ctx context.Context, wait time.Duration) error
}

// earlyAnswer is what a session keeps of the answer the 183 of ProgressMedia
// carried.
type earlyAnswer struct {
	// sdp is that answer, as it was sent.
	sdp []byte
	// unkeyed says the caller had not keyed the early media when ProgressMedia
	// returned.
	unkeyed bool
}

// noteEarlyAnswer records sdp as the answer a 183 carried, and whether the
// peer had keyed the media when the wait for it ended.
func (s *sessionState) noteEarlyAnswer(sdp []byte, unkeyed bool) {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	s.earlyAnswerSDP = bytes.Clone(sdp)
	s.earlyUnkeyed = unkeyed
}

// earlyAnswer returns what noteEarlyAnswer recorded, and false when no 183
// carried an answer.
func (s *sessionState) earlyAnswer() (earlyAnswer, bool) {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	if s.earlyAnswerSDP == nil {
		return earlyAnswer{}, false
	}
	return earlyAnswer{sdp: s.earlyAnswerSDP, unkeyed: s.earlyUnkeyed}, true
}

// noteEarlyKeyingLeft records that the answer in a 183 was taken and the
// handshake it started was left running.
func (s *sessionState) noteEarlyKeyingLeft() {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	s.earlyUnkeyed = true
}

// earlyKeyingLeft reports whether the handshake of early media was left
// running.
func (s *sessionState) earlyKeyingLeft() bool {
	s.answeringMu.Lock()
	defer s.answeringMu.Unlock()
	return s.earlyUnkeyed
}
