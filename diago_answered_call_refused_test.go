// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newAnsweredCallPeer builds a diago whose calls go over a fake transaction
// layer, which also carries the ACK, to a peer that answers the INVITE 200 with
// what answer makes of the offer, and every other request 200. It returns the
// methods of the requests the calls sent.
func newAnsweredCallPeer(t *testing.T, answer func(offer []byte) []byte, opts ...DiagoOption) (*Diago, *sentRequests) {
	t.Helper()
	sent := &sentRequests{}
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		res := sent.record(req)
		if req.IsInvite() {
			res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5099}})
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			res.SetBody(answer(req.Body()))
		}
		return res
	}, opts...)
	return dg, sent
}

// recordCallMediaPort has every call dg makes take its RTP port from the
// allocator returned, which records the release of that port: the port is
// released once the media of the call is closed.
func recordCallMediaPort(t *testing.T, dg *Diago) *funcPortAllocator {
	t.Helper()
	port := freeEvenPort(t, net.IPv4(127, 0, 0, 1))
	alloc := &funcPortAllocator{alloc: func(context.Context) (int, error) { return port, nil }}
	dg.mediaConf.RTPPortAllocator = alloc
	return alloc
}

// newBridgedTestMedia returns media a bridge takes as an answered PCMU call,
// without a socket.
func newBridgedTestMedia() *DialogMedia {
	return &DialogMedia{
		mediaSession: &media.MediaSession{Codecs: []media.Codec{media.CodecAudioUlaw}},
		audioReader:  bytes.NewBuffer(nil),
		audioWriter:  bytes.NewBuffer(nil),
	}
}

// errAckNotSent is the failure of firstAckRefused.
var errAckNotSent = errors.New("test: ACK not sent")

// firstAckRefused is a transaction layer that fails to send the first ACK it
// is given, and hands every other request to next.
type firstAckRefused struct {
	next    sipgo.ClientTransactionRequester
	refused atomic.Bool
}

func (r *firstAckRefused) Request(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
	if req.IsAck() && r.refused.CompareAndSwap(false, true) {
		return nil, errAckNotSent
	}
	return r.next.Request(ctx, req)
}

// TestDiagoAnsweredCallRefusedAfter2xx pins what becomes of a call whose 2xx
// arrived when it is then given up: InviteBridge whose bridge refuses the
// call, and InviteBridge or Invite whose Ack fails, on a DTLS handshake the
// callee never completes or on an ACK that could not be sent. RFC 3261 section
// 13.2.2.4 has the UAC acknowledge every 2xx and end a dialog it does not want
// with a BYE. The dialog was only closed, so the callee kept the call up, and
// a refused call went unacknowledged, its 2xx retransmitted until the callee
// gave up.
//
// The caller is handed no media for such a call, so the media made for it is
// closed by then, which the release of its RTP port shows.
func TestDiagoAnsweredCallRefusedAfter2xx(t *testing.T) {
	plainAnswer := func([]byte) []byte { return peerSDP() }
	silentDTLS := func(t *testing.T) (func([]byte) []byte, DiagoOption) {
		peer := newDTLSMediaSession(t, testdata.ServerCertificate())
		return func(offer []byte) []byte {
				if err := peer.RemoteSDP(offer, false); err != nil {
					return nil
				}
				// Passive, so our side starts the handshake the callee ignores.
				return withSetup(peer.LocalSDP(true), "passive")
			}, WithTransport(Transport{
				Transport: "udp",
				BindHost:  "127.0.0.1",
				MediaSRTP: media.SecureRTPModeDTLS,
				MediaDTLSConf: media.DTLSConfig{
					Certificates: []tls.Certificate{testdata.ClientCertificate()},
				},
			})
	}
	recipient := sip.Uri{User: "callee", Host: "127.0.0.1", Port: 5099}

	t.Run("InviteBridge refused by the bridge", func(t *testing.T) {
		dg, sent := newAnsweredCallPeer(t, plainAnswer)
		alloc := recordCallMediaPort(t, dg)
		b := NewBridge()
		b.WaitDialogsNum = 3
		require.NoError(t, b.AddDialogMedia(newBridgedTestMedia()))
		require.NoError(t, b.AddDialogMedia(newBridgedTestMedia()))

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _, err := dg.InviteBridge(ctx, recipient, &b, InviteOptions{})
		require.Error(t, err)
		assert.Equal(t, []sip.RequestMethod{sip.INVITE, sip.ACK, sip.BYE}, sent.sent())
		assert.Len(t, alloc.released, 1, "the media of the refused call was left open")
	})

	t.Run("InviteBridge whose Ack fails", func(t *testing.T) {
		answer, transport := silentDTLS(t)
		dg, sent := newAnsweredCallPeer(t, answer, transport)
		alloc := recordCallMediaPort(t, dg)
		b := NewBridge()

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, err := dg.InviteBridge(ctx, recipient, &b, InviteOptions{})
		require.Error(t, err)
		assert.Equal(t, []sip.RequestMethod{sip.INVITE, sip.ACK, sip.BYE}, sent.sent())
		assert.Len(t, alloc.released, 1, "the media of the call given up was left open")
	})

	t.Run("Invite whose Ack fails", func(t *testing.T) {
		answer, transport := silentDTLS(t)
		dg, sent := newAnsweredCallPeer(t, answer, transport)
		alloc := recordCallMediaPort(t, dg)

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_, _, err := dg.Invite(ctx, recipient, InviteOptions{})
		require.Error(t, err)
		assert.Equal(t, []sip.RequestMethod{sip.INVITE, sip.ACK, sip.BYE}, sent.sent())
		assert.Len(t, alloc.released, 1, "the media of the call given up was left open")
	})

	// An Ack that could not send its ACK leaves the 2xx unacknowledged, so
	// giving the call up sends the ACK before the BYE. The ACK recorded is
	// that one: the refused one never reached the peer.
	ackNotSent := map[string]func(ctx context.Context, dg *Diago) error{
		"InviteBridgeAckNotSent": func(ctx context.Context, dg *Diago) error {
			b := NewBridge()
			_, _, err := dg.InviteBridge(ctx, recipient, &b, InviteOptions{})
			return err
		},
		"InviteAckNotSent": func(ctx context.Context, dg *Diago) error {
			_, _, err := dg.Invite(ctx, recipient, InviteOptions{})
			return err
		},
	}
	for name, call := range ackNotSent {
		t.Run(name, func(t *testing.T) {
			dg, sent := newAnsweredCallPeer(t, plainAnswer)
			alloc := recordCallMediaPort(t, dg)
			dg.client.TxRequester = &firstAckRefused{next: dg.client.TxRequester}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			err := call(ctx, dg)
			require.ErrorIs(t, err, errAckNotSent)
			assert.Equal(t, []sip.RequestMethod{sip.INVITE, sip.ACK, sip.BYE}, sent.sent())
			assert.Len(t, alloc.released, 1, "the media of the call given up was left open")
		})
	}
}
