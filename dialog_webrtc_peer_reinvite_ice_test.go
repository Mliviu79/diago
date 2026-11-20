// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// sdpLinesWithPrefix returns every line of body that starts with prefix.
func sdpLinesWithPrefix(body []byte, prefix string) []string {
	var lines []string
	for line := range strings.SplitSeq(string(body), "\r\n") {
		if strings.HasPrefix(line, prefix) {
			lines = append(lines, line)
		}
	}
	return lines
}

// newWebrtcICECall negotiates a WebRTC call between a dialog's media on a
// loopback ICE socket of its own and a peer, and sets the media up in a
// DialogWebrtc the way an answer or an ACK does. localOffers says which side
// made the offer. It returns the dialog, the peer, the dialog's own SDP of
// that exchange and the address of its ICE socket. Both end with the test.
func newWebrtcICECall(t *testing.T, localOffers bool) (*DialogWebrtc, *webrtcHandlePeer, []byte, *net.UDPAddr) {
	t.Helper()
	ctx := t.Context()
	conf := loopbackWebrtcConfig()
	conf.ICE.Socket = func(context.Context) (net.PacketConn, error) {
		return media.ListenICESocket(&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	}
	conf, err := prepareWebrtcConfig(conf)
	require.NoError(t, err)
	local := &media.MediaSessionWebrtc{Codecs: []media.Codec{media.CodecAudioUlaw}}
	require.NoError(t, local.Init(ctx, conf))
	t.Cleanup(func() { assert.NoError(t, local.Close()) })
	socket := local.ICESocketAddr()
	require.NotNil(t, socket)
	peer := &webrtcHandlePeer{sess: newLoopbackWebrtcSession(t, []media.Codec{media.CodecAudioUlaw})}

	offerer, answerer := peer.sess, local
	if localOffers {
		offerer, answerer = local, peer.sess
	}
	offer, err := offerer.LocalSDP(ctx, false)
	require.NoError(t, err)
	require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
	answer, err := answerer.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, offerer.RemoteSDP(ctx, answer, true))
	finalizeWebrtcBoth(t, local.Finalize, peer.sess.Finalize)
	peer.laddr = peer.sess.Laddr

	d := &DialogWebrtc{}
	rtpSess := media.NewRTPSessionWebrtc(local)
	d.Init(local, rtpSess)
	require.NoError(t, rtpSess.MonitorBackground())
	t.Cleanup(func() { assert.NoError(t, d.Close()) })
	own := answer
	if localOffers {
		own = offer
	}
	return d, peer, own, socket
}

// TestDialogWebrtcPeerReInviteSameCredentials is a peer's re-INVITE with an
// offer on an established WebRTC call, its ICE credentials unchanged. It is
// answered with the ICE attributes the call was set up with, on the
// transport in place, and closing the dialog releases the ICE socket.
func TestDialogWebrtcPeerReInviteSameCredentials(t *testing.T) {
	ctx := t.Context()
	d, p, firstAnswer, socket := newWebrtcICECall(t, false)
	before := d.MediaSession()

	reoffer, err := p.sess.ReOffer("")
	require.NoError(t, err)
	require.NoError(t, d.onRemoteSDP(ctx, reoffer, false))
	answer, err := d.onLocalSDP(ctx, true, "")
	require.NoError(t, err)
	for _, prefix := range []string{"a=ice-ufrag:", "a=ice-pwd:", "a=fingerprint:"} {
		assert.Equal(t, sdpLineWithPrefix(t, firstAnswer, prefix), sdpLineWithPrefix(t, answer, prefix))
	}
	candidates := sdpLinesWithPrefix(firstAnswer, "a=candidate:")
	require.NotEmpty(t, candidates)
	assert.Equal(t, candidates, sdpLinesWithPrefix(answer, "a=candidate:"))
	require.NoError(t, p.sess.RemoteSDP(ctx, answer, true))
	require.NoError(t, p.sess.Finalize(ctx))
	require.NoError(t, d.onFinalize(ctx))

	require.Same(t, before, d.MediaSession(), "the re-INVITE moved the call to a new transport")
	requireWebrtcAudioBothWays(t, d, p)

	require.NoError(t, d.Close())
	reuse, err := net.ListenUDP("udp", socket)
	require.NoError(t, err, "the ICE socket must be released when the dialog closes")
	require.NoError(t, reuse.Close())
}

// TestDialogWebrtcAnswerRestartingICESurfaced is an answer to an offer the
// transport in place made, as the 2xx of another early dialog than the 183
// that set the transport up carries it, whose ICE credentials are new. The
// offer asked for no ICE restart, so no transport of ours can meet the
// answer: the restart error comes back, the call keeps its transport and
// media flows on it.
func TestDialogWebrtcAnswerRestartingICESurfaced(t *testing.T) {
	ctx := t.Context()
	d, p, offer, _ := newWebrtcICECall(t, true)
	before := d.MediaSession()

	fork, err := p.sess.Fork(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, fork.Close()) })
	require.NoError(t, fork.RemoteSDP(ctx, offer, false))
	restartAnswer, err := fork.LocalSDP(ctx, true)
	require.NoError(t, err)

	require.ErrorIs(t, d.onRemoteSDP(ctx, restartAnswer, true), media.ErrWebRTCICERestart)
	require.Same(t, before, d.MediaSession(), "a refused answer must leave the transport in place")
	d.mu.Lock()
	assert.Nil(t, d.pendingMediaSession, "the refused answer left a transport pending")
	d.mu.Unlock()
	requireWebrtcAudioBothWays(t, d, p)
}
