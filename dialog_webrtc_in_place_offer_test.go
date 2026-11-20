// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireWebrtcAudioBothWays requires a PCMU frame of the peer's to reach the
// dialog's packet reader, and one written to the dialog's packet writer to
// reach the peer, on the keys of the transport in place.
func requireWebrtcAudioBothWays(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer) {
	t.Helper()
	p.sendAudio(t)
	require.NoError(t, d.MediaSession().StopRTP(1, 5*time.Second))
	_, err := d.RTPPacketReader.Read(make([]byte, media.RTPBufSize))
	require.NoError(t, err, "the dialog read nothing of the peer's")
	require.NoError(t, d.MediaSession().StartRTP(1))
	requireWebrtcAudioToPeer(t, d, p)
}

// requireWebrtcAudioToPeer requires a frame written to the dialog's packet
// writer to reach the peer.
func requireWebrtcAudioToPeer(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer) {
	t.Helper()
	_, err := d.RTPPacketWriter.Write(make([]byte, 160))
	require.NoError(t, err)
	pkt := p.recv(t)
	assert.Equal(t, uint8(0), pkt.PayloadType)
}

// ownOfferInPlace runs an offer of ours asking for mode, after an offer request
// when requested, and the peer's answer from the transport it runs on. The
// peer takes the offer on its transport in place, which it refuses for an
// offer that changes the ICE credentials.
func ownOfferInPlace(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer, requested bool, mode string) []byte {
	t.Helper()
	ctx := t.Context()
	if requested {
		require.NoError(t, d.onRemoteSDP(ctx, nil, false))
	}
	offer, err := d.onLocalSDP(ctx, false, mode)
	require.NoError(t, err)
	require.NoError(t, p.sess.RemoteSDP(ctx, offer, false), "the offer is not one of the transport in place")
	answer, err := p.sess.LocalSDP(ctx, true)
	require.NoError(t, err)
	require.NoError(t, p.sess.Finalize(ctx))
	require.NoError(t, d.onRemoteSDP(ctx, answer, true))
	require.NoError(t, d.onFinalize(ctx))
	return offer
}

// TestDialogWebrtcOwnOfferInPlace pins that an offer of ours that refreshes
// the session, puts the call on hold or takes it off continues the nominated
// ICE pair and the DTLS association: the dialog keeps its transport and RTP
// session, the pair is the one in use, and media flows on the keys in use.
func TestDialogWebrtcOwnOfferInPlace(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested bool
		mode      string
		// held puts the call on hold first.
		held bool
		// wantOffered is the direction the offer asks for.
		wantOffered string
		// wantMode is the direction negotiated, unchecked when empty: the
		// peer here states no direction of its own, so it answers an offer
		// that takes the call off hold with the one in force.
		wantMode string
	}{
		{name: "refresh", requested: true, wantOffered: sdp.ModeSendrecv, wantMode: sdp.ModeSendrecv},
		{name: "hold", mode: sdp.ModeSendonly, wantOffered: sdp.ModeSendonly, wantMode: sdp.ModeSendonly},
		{name: "unhold", held: true, mode: sdp.ModeSendrecv, wantOffered: sdp.ModeSendrecv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
			before := d.MediaSession()
			beforeRTP := d.RTPSession()
			laddr, raddr := before.Laddr, before.Raddr
			if tc.held {
				ownOfferInPlace(t, d, p, false, sdp.ModeSendonly)
			}

			offer := ownOfferInPlace(t, d, p, tc.requested, tc.mode)

			assert.Contains(t, string(offer), "\r\na="+tc.wantOffered+"\r\n")
			require.Same(t, before, d.MediaSession(), "the offer moved the call to a new transport")
			require.Same(t, beforeRTP, d.RTPSession())
			assert.Equal(t, laddr, d.MediaSession().Laddr)
			assert.Equal(t, raddr, d.MediaSession().Raddr)
			if tc.wantMode != "" {
				assert.Equal(t, tc.wantMode, d.MediaSession().Mode)
			}
			d.mu.Lock()
			assert.Nil(t, d.pendingMediaSession, "the exchange left a transport pending")
			d.mu.Unlock()
			if tc.wantMode == sdp.ModeSendrecv {
				requireWebrtcAudioBothWays(t, d, p)
				return
			}
			requireWebrtcAudioToPeer(t, d, p)
		})
	}
}

// TestDialogWebrtcOwnReInviteRestartsICE pins that a plain re-INVITE of ours
// still restarts ICE on a new transport: its offer carries ICE credentials of
// its own, which the peer's transport in place refuses as a restart.
func TestDialogWebrtcOwnReInviteRestartsICE(t *testing.T) {
	d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
	offer, err := d.onLocalSDP(t.Context(), false, "")
	require.NoError(t, err)
	t.Cleanup(d.abortPendingMediaSession)

	d.mu.Lock()
	pending := d.pendingMediaSession
	d.mu.Unlock()
	require.NotNil(t, pending)
	assert.NotSame(t, d.MediaSession(), pending)
	require.ErrorIs(t, p.sess.RemoteSDP(t.Context(), offer, false), media.ErrWebRTCICERestart)
}

// TestDialogWebrtcFailedOwnOfferInPlace pins that an offer of ours on the
// transport in place whose exchange fails leaves the media running as it was:
// the answer is refused, or none comes. The transport, its direction and the
// keys stay, the next offer asks for the direction in force, and the answer
// to it is taken.
func TestDialogWebrtcFailedOwnOfferInPlace(t *testing.T) {
	for _, tc := range []struct {
		name string
		// fail ends the exchange of offer without its answer being taken.
		fail func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer, offer []byte)
	}{
		{
			// New ICE credentials in an answer to an offer that keeps ours:
			// the peer's checks would be made against the credentials the
			// offer carried, which no new transport of ours has.
			name: "answer_restarts_ice",
			fail: func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer, offer []byte) {
				fork, err := p.sess.Fork(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { assert.NoError(t, fork.Close()) })
				require.NoError(t, fork.RemoteSDP(t.Context(), offer, false))
				answer, err := fork.LocalSDP(t.Context(), true)
				require.NoError(t, err)
				require.ErrorIs(t, d.onRemoteSDP(t.Context(), answer, true), media.ErrWebRTCICERestart)
			},
		},
		{
			name: "answer_without_common_codec",
			fail: func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer, offer []byte) {
				answer := []byte(webrtcAudioFormats.ReplaceAllString(string(sdpAnswerFor(t, p, offer)), "$1 8\r"))
				answer = bytes.Replace(answer, []byte("a=rtpmap:0 pcmu/8000\r\n"), []byte("a=rtpmap:8 pcma/8000\r\n"), 1)
				require.ErrorIs(t, d.onRemoteSDP(t.Context(), answer, true), media.ErrNoCommonCodec)
			},
		},
		{
			name: "no_answer",
			fail: func(_ *testing.T, d *DialogWebrtc, _ *webrtcHandlePeer, _ []byte) {
				d.abortPendingMediaSession()
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
			before := d.MediaSession()
			offer, err := d.onLocalSDP(t.Context(), false, sdp.ModeSendonly)
			require.NoError(t, err)

			tc.fail(t, d, p, offer)

			require.Same(t, before, d.MediaSession())
			assert.Equal(t, sdp.ModeSendrecv, d.MediaSession().Mode)
			d.mu.Lock()
			assert.Nil(t, d.pendingMediaSession, "the failed exchange left a transport pending")
			d.mu.Unlock()
			requireWebrtcAudioBothWays(t, d, p)

			// The direction the failed offer asked for is not in force.
			ownOfferInPlace(t, d, p, true, "")
			assert.Equal(t, sdp.ModeSendrecv, d.MediaSession().Mode)
			require.Same(t, before, d.MediaSession())
			requireWebrtcAudioBothWays(t, d, p)
		})
	}
}

// sdpAnswerFor returns the answer the peer makes to offer on its transport in
// place, without committing it.
func sdpAnswerFor(t *testing.T, p *webrtcHandlePeer, offer []byte) []byte {
	t.Helper()
	require.NoError(t, p.sess.RemoteSDP(t.Context(), offer, false))
	answer, err := p.sess.LocalSDP(t.Context(), true)
	require.NoError(t, err)
	p.sess.Rollback()
	return answer
}

// TestIntegrationDialogWebrtcInPlaceReInvites runs, on one WebRTC call, a Hold
// of the caller's and a re-INVITE of the caller's without an offer. The hold
// keeps both transports. The re-INVITE without an offer is answered 200 with
// an offer of the callee's transport in place, the ACK carries the answer,
// and the callee commits it on that transport. Media flows on the keys in use
// after each.
func TestIntegrationDialogWebrtcInPlaceReInvites(t *testing.T) {
	ctx := t.Context()
	type serverCall struct {
		dialog *DialogServerSession
		media  *DialogWebrtc
	}
	calls := make(chan serverCall, 1)
	answerErr := make(chan error, 1)
	dialog := newWebrtcCallPair(t, func(serveCtx context.Context, d *DialogServerSession) {
		med, err := d.AnswerWebrtc(AnswerWebrtcOptions{WebrtcConfig: loopbackWebrtcConfig()})
		if err != nil {
			answerErr <- err
			return
		}
		calls <- serverCall{dialog: d, media: med}
		// The handler ends with its run as well as with its dialog: a dialog
		// whose BYE never arrived would otherwise hold serving past the test.
		select {
		case <-serveCtx.Done():
		case <-d.Context().Done():
		}
	})
	callCtx, callCancel := context.WithTimeout(ctx, 30*time.Second)
	defer callCancel()
	clientMed, err := dialog.InviteWebrtc(callCtx, InviteWebrtcOptions{WebrtcConfig: loopbackWebrtcConfig()})
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, clientMed.Close()) })
	var call serverCall
	select {
	case call = <-calls:
	case err := <-answerErr:
		t.Fatalf("callee did not answer: %v", err)
	case <-callCtx.Done():
		t.Fatal("timed out waiting for the callee's media")
	}
	clientBefore := clientMed.MediaSession()
	serverBefore := call.media.MediaSession()
	serverUpdates := make(chan string, 2)
	call.media.OnMediaUpdate(func(m *DialogWebrtc) { serverUpdates <- m.MediaSession().Mode })
	awaitServerUpdate := func(want string) {
		t.Helper()
		select {
		case mode := <-serverUpdates:
			require.Equal(t, want, mode)
		case <-callCtx.Done():
			t.Fatal("the callee did not commit the update")
		}
	}

	require.NoError(t, dialog.Hold(callCtx))
	awaitServerUpdate(sdp.ModeRecvonly)
	require.Same(t, clientBefore, clientMed.MediaSession(), "Hold moved the caller to a new transport")
	require.Same(t, serverBefore, call.media.MediaSession(), "Hold moved the callee to a new transport")
	require.Equal(t, sdp.ModeSendonly, clientMed.MediaSession().Mode)
	requireWebrtcDialogAudio(t, clientMed, call.media)

	req := sip.NewRequest(sip.INVITE, dialog.remoteTarget())
	req.AppendHeader(dialog.InviteRequest.Contact())
	res, err := dialog.Do(callCtx, req)
	require.NoError(t, err)
	require.Equal(t, sip.StatusOK, res.StatusCode)
	offer := res.Body()
	require.NotEmpty(t, offer, "the 200 to a re-INVITE without an offer carries none")
	require.NoError(t, clientMed.onRemoteSDP(callCtx, offer, false), "the callee's offer is not one of its transport in place")
	answer, err := clientMed.onLocalSDP(callCtx, true, "")
	require.NoError(t, err)
	require.NoError(t, dialog.ack(callCtx, res.Contact().Address, answer))
	require.NoError(t, clientMed.onFinalize(callCtx))
	// The caller's answer keeps the call on hold.
	awaitServerUpdate(sdp.ModeRecvonly)
	require.Same(t, clientBefore, clientMed.MediaSession())
	require.Same(t, serverBefore, call.media.MediaSession())
	requireWebrtcDialogAudio(t, clientMed, call.media)

	require.NoError(t, dialog.Hangup(ctx))
}

// requireWebrtcDialogAudio requires a frame written to from's packet writer
// to reach to's packet reader.
func requireWebrtcDialogAudio(t *testing.T, from, to *DialogWebrtc) {
	t.Helper()
	payload := bytes.Repeat([]byte{0x5a}, int(media.CodecAudioUlaw.SampleTimestamp()))
	_, err := from.RTPPacketWriter.Write(payload)
	require.NoError(t, err)
	require.NoError(t, to.MediaSession().StopRTP(1, 5*time.Second))
	buf := make([]byte, media.RTPBufSize)
	n, err := to.RTPPacketReader.Read(buf)
	require.NoError(t, err)
	require.NoError(t, to.MediaSession().StartRTP(1))
	require.Equal(t, payload, buf[:n])
}
