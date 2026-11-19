// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDialogReInviteOffersNegotiatedCodecs pins the codecs ReInvite offers, on
// either side, when the media is configured to renegotiate nothing with it:
// the ones the call runs on, as an offer made from the session in place did,
// so a session refresh does not ask the peer to move the call to another
// codec. The session keeps every codec it was set up with, for a later offer
// of the peer's. Without that configuration the offer carries every codec the
// media was set up with.
func TestDialogReInviteOffersNegotiatedCodecs(t *testing.T) {
	newSession := func(t *testing.T, codecs ...media.Codec) *media.MediaSession {
		s := &media.MediaSession{Codecs: codecs, Mode: sdp.ModeSendrecv, Laddr: net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}}
		require.NoError(t, s.Init())
		t.Cleanup(func() { _ = s.Close() })
		return s
	}
	// negotiated returns the session of a call set up with PCMU and PCMA
	// that runs on PCMA, the only codec its peer has.
	negotiated := func(t *testing.T, side string) *media.MediaSession {
		sess := newSession(t, media.CodecAudioUlaw, media.CodecAudioAlaw)
		peer := newSession(t, media.CodecAudioAlaw)
		if side == "inbound" {
			require.NoError(t, sess.RemoteSDP(peer.LocalSDP(false), false))
		} else {
			require.NoError(t, peer.RemoteSDP(sess.LocalSDP(false), false))
			require.NoError(t, sess.RemoteSDP(peer.LocalSDP(true), true))
		}
		require.Equal(t, []media.Codec{media.CodecAudioAlaw}, sess.CommonCodecs())
		return sess
	}
	// answering returns how a peer that has both codecs answers the call's
	// requests, and the offer it was sent.
	answering := func(t *testing.T) (func(*sip.Request) *sip.Response, func() []byte) {
		var offer []byte
		onRequest := func(req *sip.Request) *sip.Response {
			res := sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
			res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}})
			if req.IsInvite() {
				offer = req.Body()
				answerer := newSession(t, media.CodecAudioUlaw, media.CodecAudioAlaw)
				if err := answerer.RemoteSDP(offer, false); err == nil {
					res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
					res.SetBody(answerer.LocalSDP(true))
				}
			}
			return res
		}
		return onRequest, func() []byte { return offer }
	}

	for _, side := range reInviteSides {
		t.Run(side.name, func(t *testing.T) {
			sess := negotiated(t, side.name)
			onRequest, offer := answering(t)
			conf := MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}, ReInviteNegotiatedCodecs: true}
			call := side.setup(t, conf, sess, onRequest)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, call.reInvite(ctx))
			assert.Equal(t, fmt.Sprintf("m=audio %d RTP/AVP 8", sess.Laddr.Port), sdpLineWithPrefix(t, offer(), "m=audio "), "the offer must keep the call's codec")

			cur := call.med.MediaSession()
			assert.Equal(t, []media.Codec{media.CodecAudioAlaw}, cur.CommonCodecs())
			assert.Equal(t, []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw}, cur.Codecs, "the session lost codecs it was set up with")
		})

		t.Run(side.name+"/every_codec_by_default", func(t *testing.T) {
			sess := negotiated(t, side.name)
			onRequest, offer := answering(t)
			call := side.setup(t, MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}}, sess, onRequest)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, call.reInvite(ctx))
			assert.Equal(t, fmt.Sprintf("m=audio %d RTP/AVP 0 8", sess.Laddr.Port), sdpLineWithPrefix(t, offer(), "m=audio "))
		})

		t.Run(side.name+"/hold_offers_every_codec", func(t *testing.T) {
			sess := negotiated(t, side.name)
			onRequest, offer := answering(t)
			conf := MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}, ReInviteNegotiatedCodecs: true}
			call := side.setup(t, conf, sess, onRequest)
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			require.NoError(t, call.hold(ctx))
			assert.Equal(t, fmt.Sprintf("m=audio %d RTP/AVP 0 8", sess.Laddr.Port), sdpLineWithPrefix(t, offer(), "m=audio "))
			assert.Equal(t, "a=sendonly", sdpLineWithPrefix(t, offer(), "a=sendonly"))
		})
	}
}

// sdpLineWithPrefix returns the first line of body that starts with prefix.
func sdpLineWithPrefix(t *testing.T, body []byte, prefix string) string {
	t.Helper()
	for line := range strings.SplitSeq(string(body), "\r\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	t.Fatalf("no %q line in the session description:\n%s", prefix, body)
	return ""
}
