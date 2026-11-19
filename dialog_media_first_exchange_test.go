// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"net"
	"testing"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newUnnegotiatedDialogMedia returns media bound to loopback that has taken
// part in no exchange yet, closed with the test.
func newUnnegotiatedDialogMedia(t *testing.T) *DialogMedia {
	t.Helper()
	med, err := newDialogMedia(MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = med.Close() })
	return med
}

// TestDialogMediaFirstExchangeOffered pins the first exchange of a call's
// media when the peer makes the offer and the session drives it through the
// handshaker: the offer is applied to the media session in place, the answer
// describes that session, and the RTCP monitor is started by the commit, once.
func TestDialogMediaFirstExchangeOffered(t *testing.T) {
	updates := 0
	med := newUnnegotiatedDialogMedia(t)
	med.onMediaUpdate = func(*DialogMedia) { updates++ }
	sess := med.MediaSession()
	peer := newMediaSessionForTest(t)

	require.NoError(t, med.onRemoteSDP(t.Context(), peer.LocalSDP(false), false))
	assert.Same(t, sess, med.MediaSession(), "the first offer is applied to the session in place, not to a fork")
	require.NotNil(t, med.RTPSession())
	assert.NotNil(t, med.RTPPacketReader)
	assert.NotNil(t, med.RTPPacketWriter)

	answer, err := med.onLocalSDP(t.Context(), true, "")
	require.NoError(t, err)
	require.NoError(t, peer.RemoteSDP(answer, true))
	assert.Equal(t, sess.Laddr.Port, peer.Raddr.Port)

	rtpSess := med.RTPSession()
	require.NoError(t, med.onFinalize(t.Context()))
	assert.Error(t, rtpSess.MonitorBackground(), "the commit started the RTCP monitor")
	require.NoError(t, med.onFinalize(t.Context()), "a second commit starts nothing")
	assert.Same(t, rtpSess, med.RTPSession())
	assert.Zero(t, updates, "a first exchange is no media update")
}

// TestDialogMediaFirstExchangeOffering pins the first exchange of a call's
// media when we make the offer through the handshaker: the offer comes from
// the media session in place, the answer is applied to that session, and the
// RTCP monitor is started by the commit that follows the answer.
func TestDialogMediaFirstExchangeOffering(t *testing.T) {
	med := newUnnegotiatedDialogMedia(t)
	sess := med.MediaSession()
	peer := newMediaSessionForTest(t)

	offer, err := med.onLocalSDP(t.Context(), false, "")
	require.NoError(t, err)
	assert.Same(t, sess, med.MediaSession(), "the first offer is made by the session in place, not by a fork")
	require.NotNil(t, med.RTPSession())

	require.NoError(t, peer.RemoteSDP(offer, false))
	assert.Equal(t, sess.Laddr.Port, peer.Raddr.Port)
	require.NoError(t, med.onRemoteSDP(t.Context(), peer.LocalSDP(true), true))
	assert.Same(t, sess, med.MediaSession(), "the answer is applied to the session that made the offer")
	assert.Equal(t, peer.Laddr.Port, sess.Raddr.Port)

	rtpSess := med.RTPSession()
	require.NoError(t, med.onFinalize(t.Context()))
	assert.Error(t, rtpSess.MonitorBackground(), "the commit started the RTCP monitor")
}

// TestDialogMediaLaterExchangeForks pins that only the first exchange is made
// on the media session in place: once media runs, an offer of the peer's is
// applied to a fork that replaces it, as before.
func TestDialogMediaLaterExchangeForks(t *testing.T) {
	updates := 0
	med := newUnnegotiatedDialogMedia(t)
	med.onMediaUpdate = func(*DialogMedia) { updates++ }
	peer := newMediaSessionForTest(t)
	require.NoError(t, med.onRemoteSDP(t.Context(), peer.LocalSDP(false), false))
	require.NoError(t, med.onFinalize(t.Context()))
	sess := med.MediaSession()

	require.NoError(t, med.onRemoteSDP(t.Context(), peer.LocalSDP(false), false))
	assert.NotSame(t, sess, med.MediaSession(), "an update replaces the media session")
	assert.Equal(t, 1, updates)
}
