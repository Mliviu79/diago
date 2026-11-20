// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"testing"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDialogWebrtcFinalizeClosedMedia pins that a transport whose ICE and
// DTLS complete after the media was closed, as the dialog's end closes it
// while an answer or an outbound call waits for them, is not set up as the
// media and gets no RTCP monitor: the Close found no transport to close, so
// a monitor started now would run with nothing left to stop it. The media
// reports itself closed and the transport is left to its caller to close.
func TestDialogWebrtcFinalizeClosedMedia(t *testing.T) {
	peer, offer := newWebrtcPeerOffer(t)
	sess := newWebrtcPeer(t)
	require.NoError(t, sess.RemoteSDP(t.Context(), offer, false))
	answer, err := sess.LocalSDP(t.Context(), true)
	require.NoError(t, err)
	require.NoError(t, peer.RemoteSDP(t.Context(), answer, true))

	med := &DialogWebrtc{}
	require.NoError(t, med.Close())

	peerDone := finalizeWebrtcPeer(t, peer)
	require.ErrorIs(t, finalizeWebrtcMedia(t.Context(), sess, med), errMediaClosed)
	requireWebrtcPeerFinalized(t, peerDone)
	assert.Nil(t, med.MediaSession(), "closed media sets no transport up")
	assert.Nil(t, med.RTPSession(), "closed media has no RTP session to monitor")

	// The transport is still its caller's, and its RTCP can still be
	// monitored by whoever takes it: nothing was started on it.
	rtpSess := media.NewRTPSessionWebrtc(sess)
	require.NoError(t, rtpSess.MonitorBackground())
	require.NoError(t, rtpSess.MonitorClose())
}
