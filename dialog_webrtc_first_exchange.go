// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"slices"

	"github.com/emiago/diago/media"
	"github.com/pion/ice/v4"
)

// newDialogWebrtc returns the WebRTC media of a call that has taken part in
// no offer/answer exchange yet: a transport whose candidates are gathered,
// with the configuration conf gives a call that passed call, and nothing
// else. Whoever answers or places the call drives its first exchange through
// the handshaker, as it drives every later one, and the transport is set up
// as the media of the call once that exchange is complete (onFinalize).
// codecs are the codecs the media offers and answers with, conf.Codecs when
// empty. onICEStateChange, when not nil, observes the transport's ICE state
// and must not block.
//
// ctx is the context of the call the media is for, on which its socket is
// bound. It bounds nothing else: the media runs until it is closed.
//
// The caller closes the media, or registers it with a session that does.
func newDialogWebrtc(ctx context.Context, conf MediaConfig, codecs []media.Codec, call media.MediaSessionWebrtcConfig, onICEStateChange func(ice.ConnectionState)) (*DialogWebrtc, error) {
	webrtcConf, err := prepareWebrtcConfig(conf.webrtcConfig(call))
	if err != nil {
		return nil, err
	}
	if len(codecs) == 0 {
		codecs = conf.Codecs
	}
	sess := &media.MediaSessionWebrtc{Codecs: slices.Clone(codecs)}
	if err := sess.Init(ctx, webrtcConf, onICEStateChange); err != nil {
		return nil, err
	}
	return &DialogWebrtc{
		// The transport is the exchange's until it completes: Close closes it
		// as it closes any transport of an exchange in progress.
		pendingMediaSession: sess,
		dialogWebrtcState:   dialogWebrtcState{offerNegotiatedCodecs: conf.ReInviteNegotiatedCodecs},
	}, nil
}

// firstExchangeSession returns the transport of the media's first
// offer/answer exchange while that exchange is in progress: the media has no
// transport set up yet, and the one newDialogWebrtc made is not closed. The
// first offer, ours or the peer's, and its answer belong to that transport
// and not to a fork of it: there is no running media a fork would keep
// carrying meanwhile.
func (d *DialogWebrtc) firstExchangeSession() (*media.MediaSessionWebrtc, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.mediaSession != nil || d.pendingMediaSession == nil {
		return nil, false
	}
	return d.pendingMediaSession, true
}

// firstLocalSDP is onLocalSDP during the first exchange: the offer of sess,
// with the direction mode when that is not empty, or its answer to the
// peer's offer.
func firstLocalSDP(ctx context.Context, sess *media.MediaSessionWebrtc, answered bool, mode string) ([]byte, error) {
	if !answered && mode != "" {
		// Nothing reads the transport's mode before its first description.
		sess.Mode = mode
	}
	return sess.LocalSDP(ctx, answered)
}

// firstRemoteSDP is onRemoteSDP during the first exchange: the peer's offer,
// or its answer to ours, applied to sess. A session asking for an offer of
// ours brings nothing to apply: the first offer is ours either way.
func firstRemoteSDP(ctx context.Context, sess *media.MediaSessionWebrtc, remoteSDP []byte, offered bool) error {
	if remoteSDP == nil {
		return nil
	}
	return sess.RemoteSDP(ctx, remoteSDP, offered)
}

// finalizeFirstExchange is onFinalize during the first exchange: it completes
// ICE and DTLS on sess within ctx and then sets sess up as the media of the
// call, with its RTP session, reader and writer, and starts its RTCP monitor.
// A transport that cannot be completed is closed, and the media is left
// without one.
func (d *DialogWebrtc) finalizeFirstExchange(ctx context.Context, sess *media.MediaSessionWebrtc) error {
	if err := sess.Finalize(ctx); err != nil {
		d.abortPendingMediaSession()
		return err
	}
	return d.installFirstExchange(sess)
}

// installFirstExchange sets sess, the completed transport of the media's
// first exchange, up as the media of the call, with its RTP session, reader
// and writer, and starts its RTCP monitor.
func (d *DialogWebrtc) installFirstExchange(sess *media.MediaSessionWebrtc) error {
	rtpSess := media.NewRTPSessionWebrtc(sess)
	d.mu.Lock()
	if d.closed || d.pendingMediaSession != sess {
		// Close took the transport and closed it.
		d.mu.Unlock()
		return errMediaClosed
	}
	codec := sess.Codec()
	d.pendingMediaSession = nil
	d.mediaSession = sess
	d.rtpSession = rtpSess
	d.RTPPacketReader = media.NewRTPPacketReader(rtpSess, codec)
	d.RTPPacketWriter = media.NewRTPPacketWriter(rtpSess, codec)
	d.mu.Unlock()
	return rtpSess.MonitorBackground()
}
