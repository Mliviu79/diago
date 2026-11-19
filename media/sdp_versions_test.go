// SPDX-License-Identifier: MPL-2.0

package media

import (
	"strings"
	"testing"

	"github.com/emiago/diago/media/sdp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// originVersionsAnswer is what the peer of the sessions below answers an offer
// of ours with. Its o= line is the peer's own.
var originVersionsAnswer = []byte(strings.Join([]string{
	"v=0", "o=- 777 5 IN IP4 192.0.2.10", "s=-", "c=IN IP4 192.0.2.10", "t=0 0",
	"m=audio 30000 RTP/AVP 8 0 101", "a=rtpmap:8 PCMA/8000", "a=rtpmap:0 PCMU/8000",
	"a=rtpmap:101 telephone-event/8000", "a=fmtp:101 0-16", "a=sendrecv",
}, "\r\n") + "\r\n")

// inPlaceOffer returns the offer a fork of m makes of the session as it runs:
// the codecs the call was negotiated with, and nothing else changed.
func inPlaceOffer(m *MediaSession) (fork *MediaSession, offer []byte) {
	fork = m.Fork()
	fork.Codecs = m.CommonCodecs()
	return fork, fork.LocalSDP(false)
}

// TestMediaSessionOriginVersion pins the o= line of the SDPs the media of one
// call sends. RFC 3264 section 8: "the "o=" line of the new SDP MUST be
// identical to that in the previous SDP, except that the version in the origin
// field MUST increment by one from the previous SDP. If the version in the
// origin line does not increment, the SDP MUST be identical to the SDP with
// that version number." The previous SDP is the last one provided to the peer,
// in an offer or in an answer. RFC 4028 section 7.4 has the offer of a session
// refresh that changes nothing, and the answer to one, carry "the same value
// for the origin field as did previous SDP messages to its peer".
func TestMediaSessionOriginVersion(t *testing.T) {
	t.Run("the_same_answer_again_is_the_same_description", func(t *testing.T) {
		m, answer := answerTo(t, streamsOffer(streamsAudio))
		id, version, _ := sdpOriginOf(t, answer)
		assert.EqualValues(t, 100, id)
		assert.EqualValues(t, 102, version)
		// As the answer of a 183 is repeated in the 200.
		assert.Equal(t, string(answer), string(m.LocalSDP(true)))
	})

	t.Run("an_offer_of_the_session_as_it_runs_keeps_the_origin", func(t *testing.T) {
		m, answer := answerTo(t, streamsOffer(streamsAudio))

		fork, refresh := inPlaceOffer(m)
		assert.Equal(t, string(answer), string(refresh), "a session refresh that changes nothing is the previous SDP")
		require.NoError(t, fork.RemoteSDP(originVersionsAnswer, true))

		_, again := inPlaceOffer(fork)
		assert.Equal(t, string(answer), string(again), "the refresh after it")
	})

	t.Run("an_answer_to_an_unchanged_offer_keeps_the_origin", func(t *testing.T) {
		m, answer := answerTo(t, streamsOffer(streamsAudio))

		fork := m.Fork()
		require.NoError(t, fork.RemoteSDP(streamsOffer(streamsAudio), false))
		assert.Equal(t, string(answer), string(fork.LocalSDP(true)))
	})

	t.Run("a_modified_description_takes_the_next_version", func(t *testing.T) {
		m, answer := answerTo(t, streamsOffer(streamsAudio))
		id, version, rest := sdpOriginOf(t, answer)
		requireVersion := func(t *testing.T, body []byte, want int64) {
			t.Helper()
			gotID, gotVersion, gotRest := sdpOriginOf(t, body)
			require.Equal(t, id, gotID, "the session id")
			require.Equal(t, rest, gotRest, "the rest of the o= line")
			require.Equal(t, want, gotVersion, "the session version of\n%s", body)
		}

		hold := m.Fork()
		hold.Mode = sdp.ModeSendonly
		held := hold.LocalSDP(false)
		requireVersion(t, held, version+1)

		// That offer came to nothing, as one refused or answered 491. The
		// one sent again is the same description.
		retry := m.Fork()
		retry.Mode = sdp.ModeSendonly
		assert.Equal(t, string(held), string(retry.LocalSDP(false)))

		// Another offer made in its place is not the SDP that version went
		// out with, so it takes the next one, whichever session makes it.
		other := m.Fork()
		other.Mode = sdp.ModeInactive
		requireVersion(t, other.LocalSDP(false), version+2)

		// And the session as it runs, offered after it, is modified from
		// the previous SDP too.
		_, refresh := inPlaceOffer(m)
		requireVersion(t, refresh, version+3)
	})

	t.Run("the_offerer_keeps_its_origin_once_answered", func(t *testing.T) {
		m := newStreamsSession()
		offer := m.LocalSDP(false)
		id, version, rest := sdpOriginOf(t, offer)
		require.NoError(t, m.RemoteSDP(originVersionsAnswer, true))

		hold := m.Fork()
		hold.Mode = sdp.ModeSendonly
		gotID, gotVersion, gotRest := sdpOriginOf(t, hold.LocalSDP(false))
		assert.Equal(t, id, gotID, "the session id of our SDPs changed to the peer's")
		assert.Equal(t, rest, gotRest)
		assert.Equal(t, version+1, gotVersion)
	})

	t.Run("a_peer_that_changes_its_session_id_does_not_change_ours", func(t *testing.T) {
		m, answer := answerTo(t, streamsOffer(streamsAudio))
		id, version, _ := sdpOriginOf(t, answer)

		fork := m.Fork()
		reoffer := strings.Replace(string(streamsOffer(streamsAudio)), "o=- 100 100 ", "o=- 900 7 ", 1)
		reoffer = strings.Replace(reoffer, "a=sendrecv", "a=sendonly", 1)
		require.NoError(t, fork.RemoteSDP([]byte(reoffer), false))
		gotID, gotVersion, _ := sdpOriginOf(t, fork.LocalSDP(true))
		assert.Equal(t, id, gotID)
		assert.Equal(t, version+1, gotVersion)
	})
}

// TestSDPVersions pins the numbering itself: the first description takes the
// version it is given, one that repeats the description before it takes that
// description's version, and any other the next.
func TestSDPVersions(t *testing.T) {
	var v sdpVersions
	text := "a"
	write := func(version uint64) []byte {
		return []byte(text + " " + string(rune('0'+version)))
	}

	body, version := v.numbered(3, write)
	assert.Equal(t, "a 3", string(body))
	assert.EqualValues(t, 3, version)

	body, version = v.numbered(9, write)
	assert.Equal(t, "a 3", string(body), "the same description again")
	assert.EqualValues(t, 3, version)

	text = "b"
	body, version = v.numbered(9, write)
	assert.Equal(t, "b 4", string(body))
	assert.EqualValues(t, 4, version)

	text = "a"
	body, version = v.numbered(9, write)
	assert.Equal(t, "a 5", string(body), "a description sent before is modified from the previous one")
	assert.EqualValues(t, 5, version)

	// What is handed out is the caller's to change.
	body[0] = 'x'
	body, _ = v.numbered(9, write)
	assert.Equal(t, "a 5", string(body))
}
