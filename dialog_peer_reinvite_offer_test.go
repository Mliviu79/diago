// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDialogAckAnswerAppliedOnFork is a re-INVITE of the peer's without an
// offer, on either side, while our media is written. Our 2xx carries an offer,
// and the ACK carries the answer, which moves the peer's RTP to another port.
// The answer is applied to the fork that made the offer, which is installed,
// and the session that carried the media is left as it was: applied to it in
// place, the answer rewrote its codecs and remote address while the writer
// read them.
func TestDialogAckAnswerAppliedOnFork(t *testing.T) {
	sides := []struct {
		name string
		// setup returns a live dialog's media, a re-INVITE of the peer's
		// without an offer, and the handling of it and of the ACK.
		setup func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error, func(*sip.Request) error)
	}{
		{
			name: "inbound",
			setup: func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error, func(*sip.Request) error) {
				d, med := newAnsweredTestServerCall(t)
				return med, newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1), func(req *sip.Request, tx sip.ServerTransaction) error {
						return d.handleReInvite(t.Context(), req, tx)
					},
					func(ack *sip.Request) error { return d.ReadAck(t.Context(), ack, newByeServerTx()) }
			},
		},
		{
			name: "outbound",
			setup: func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error, func(*sip.Request) error) {
				d, med, _ := newAnsweredTestClientCall(t)
				return med, newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1), func(req *sip.Request, tx sip.ServerTransaction) error {
						return d.handleReInvite(t.Context(), req, tx)
					},
					func(ack *sip.Request) error { return d.handleReInviteACK(t.Context(), ack, newByeServerTx()) }
			},
		},
	}

	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			m, reInvite, handleReInvite, readAck := side.setup(t)
			orig := m.MediaSession()
			origPort := orig.Raddr.Port
			m.mu.Lock()
			w := m.RTPPacketWriter
			m.mu.Unlock()

			stop := make(chan struct{})
			written := make(chan struct{})
			go func() {
				defer close(written)
				payload := make([]byte, 160)
				for {
					select {
					case <-stop:
						return
					default:
					}
					_, _ = w.Write(payload)
				}
			}()
			stopWriter := func() {
				close(stop)
				select {
				case <-written:
				case <-time.After(5 * time.Second):
					t.Fatal("the writer did not stop")
				}
			}

			tx := newRespondedServerTx()
			handled := make(chan error, 1)
			go func() { handled <- handleReInvite(reInvite, tx) }()
			var res *sip.Response
			select {
			case res = <-tx.responded:
			case <-time.After(5 * time.Second):
				stopWriter()
				t.Fatal("the re-INVITE was not answered")
			}
			require.Equal(t, sip.StatusOK, res.StatusCode, "reason: %s", res.Reason)

			answerer := newMediaSessionForTest(t)
			require.NoError(t, answerer.RemoteSDP(res.Body(), false))
			err := readAck(newReInviteAck(reInvite, answerer.LocalSDP(true)))
			stopWriter()
			require.NoError(t, err)
			select {
			case err := <-handled:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the re-INVITE handler did not return")
			}

			cur := m.MediaSession()
			require.False(t, orig == cur, "the answer was not applied to a fork")
			assert.Equal(t, origPort, orig.Raddr.Port, "the answer rewrote the session carrying the media")
			assert.Equal(t, answerer.Laddr.Port, cur.Raddr.Port, "the answer was not applied")
		})
	}
}

// requireLocalSDP asserts the response carries our published media: the port
// and the codec identify it.
func requireLocalSDP(t *testing.T, d *DialogMedia, res *sip.Response) {
	t.Helper()
	require.Equal(t, "application/sdp", res.ContentType().Value())
	require.Contains(t, string(res.Body()),
		fmt.Sprintf("m=audio %d RTP/AVP 0", d.MediaSession().Laddr.Port))
}

// newUnsetMediaCall returns a confirmed inbound call whose registered media
// has no media session.
func newUnsetMediaCall(t *testing.T) *DialogServerSession {
	t.Helper()
	d, inviteTx := newAnswerTestDialog(t)
	t.Cleanup(func() { _ = d.Close() })
	(&DialogMedia{}).registerDialogCallbacks(&d.dialogCallbacks)
	answered := sendOK(t, d, inviteTx)
	confirm(t, d)
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not return")
	}
	return d
}

// An offer-less re-INVITE is a request for an offer (RFC 3261 14.2): it must be
// answered with the SDP we already published, not rejected.
func TestHandleMediaUpdateOfferless(t *testing.T) {
	// answered has d handle req, whose 2xx the peer acknowledges without an
	// answer, and returns the response.
	answered := func(t *testing.T, d *DialogServerSession, req *sip.Request) *sip.Response {
		t.Helper()
		inner := newByeServerTx()
		tx := newAckedServerTx(inner, func() {
			_ = d.ReadAck(t.Context(), newReInviteAck(req, nil), newByeServerTx())
		})
		require.NoError(t, d.handleReInvite(t.Context(), req, tx))
		require.Len(t, inner.responses, 1)
		return inner.responses[0]
	}

	t.Run("ReplaysLocalSDP", func(t *testing.T) {
		d, med := newAnsweredTestServerCall(t)

		res := answered(t, d, newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1))
		require.Equal(t, sip.StatusOK, res.StatusCode)
		requireLocalSDP(t, med, res)
	})

	// Content-Length: 0 can reach us as an empty but non nil body, which a nil
	// check lets through into the SDP parser. It finds no m= line and the peer is
	// told its legal request was rejected.
	t.Run("EmptyNonNilBodyReplaysLocalSDP", func(t *testing.T) {
		d, med := newAnsweredTestServerCall(t)
		req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
		req.SetBody([]byte{})
		require.NotNil(t, req.Body())

		res := answered(t, d, req)
		require.Equal(t, sip.StatusOK, res.StatusCode)
		requireLocalSDP(t, med, res)
	})

	// A request for an offer can not be answered with an SDP we never built. The
	// bodied path already rejects this; the offer-less path must not instead
	// dereference the nil session.
	t.Run("NoMediaSessionIsRejected", func(t *testing.T) {
		d := newUnsetMediaCall(t)
		tx := newByeServerTx()

		require.Error(t, d.handleReInvite(t.Context(), newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1), tx))
		require.Len(t, tx.responses, 1)
		require.Equal(t, sip.StatusBadRequest, tx.responses[0].StatusCode)
	})

	// Control: a re-INVITE that does carry an offer must still be negotiated,
	// never swallowed by the offer-less branch.
	t.Run("BodiedOfferIsStillNegotiated", func(t *testing.T) {
		d := newUnsetMediaCall(t)
		req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody([]byte("v=0\r\n"))
		tx := newByeServerTx()

		require.ErrorIs(t, d.handleReInvite(t.Context(), req, tx), errNoMediaSession)
		require.Len(t, tx.responses, 1)
		require.Equal(t, sip.StatusBadRequest, tx.responses[0].StatusCode)
	})
}

// TestDialogPeerReInviteWithoutOfferAckWithoutAnswer is a re-INVITE of the
// peer's without an offer whose 2xx, carrying ours, is acknowledged without an
// answer, on either side. The offer is dropped and the call goes on over the
// media session it had: nothing is installed and the call is not ended.
func TestDialogPeerReInviteWithoutOfferAckWithoutAnswer(t *testing.T) {
	for _, side := range reInviteOfferlessSides {
		t.Run(side.name, func(t *testing.T) {
			med, req, handleReInvite, readAck, sentBye := side.setup(t)
			before := med.MediaSession()

			tx := newRespondedServerTx()
			handled := make(chan error, 1)
			go func() { handled <- handleReInvite(req, tx) }()
			select {
			case res := <-tx.responded:
				require.Equal(t, sip.StatusOK, res.StatusCode)
			case <-time.After(5 * time.Second):
				t.Fatal("the re-INVITE was not answered")
			}
			med.mu.Lock()
			offering := med.pendingMediaSession
			med.mu.Unlock()
			require.NotNil(t, offering, "the offer in the 2xx was not made by a fork")
			require.NotSame(t, before, offering, "the offer in the 2xx was made by the session carrying the media")

			require.NoError(t, readAck(newReInviteAck(req, nil)))
			select {
			case err := <-handled:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the re-INVITE handler did not return")
			}

			med.mu.Lock()
			pending := med.pendingMediaSession
			med.mu.Unlock()
			assert.Nil(t, pending, "the offer was kept without its answer")
			assert.Same(t, before, med.MediaSession(), "an ACK without an answer replaced the media session")
			assert.False(t, sentBye(), "an ACK without an answer ended the call")
		})
	}
}

// reInviteOfferlessSides builds, for each side of a call, a confirmed dialog
// with media, a re-INVITE of the peer's without an offer, and the handling of
// it and of the ACK to our 2xx. sentBye reports whether the dialog has sent a
// BYE.
var reInviteOfferlessSides = []struct {
	name  string
	setup func(t *testing.T) (med *DialogMedia, req *sip.Request, handleReInvite func(*sip.Request, sip.ServerTransaction) error, readAck func(*sip.Request) error, sentBye func() bool)
}{
	{
		name: "inbound",
		setup: func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error, func(*sip.Request) error, func() bool) {
			ua, sent := newRecordingDialogUA(t)
			d, med := newAnsweredTestServerCallOverUA(t, ua)
			return med, newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1),
				func(req *sip.Request, tx sip.ServerTransaction) error { return d.handleReInvite(t.Context(), req, tx) },
				func(ack *sip.Request) error { return d.ReadAck(t.Context(), ack, newByeServerTx()) },
				func() bool { return slices.Contains(sent.sent(), sip.BYE) }
		},
	},
	{
		name: "outbound",
		setup: func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error, func(*sip.Request) error, func() bool) {
			d, med, sent := newAnsweredTestClientCall(t)
			return med, newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1),
				func(req *sip.Request, tx sip.ServerTransaction) error { return d.handleReInvite(t.Context(), req, tx) },
				func(ack *sip.Request) error { return d.handleReInviteACK(t.Context(), ack, newByeServerTx()) },
				func() bool { return slices.Contains(sent.sent(), sip.BYE) }
		},
	},
}

// TestDialogPeerReInviteWithoutOfferUnusableAnswerEndsTheCall is a re-INVITE
// of the peer's without an offer whose ACK carries an answer the media cannot
// apply, on either side. An ACK cannot be refused, so the offer is dropped,
// the media session in place is left as it was, and the call is ended with a
// BYE.
func TestDialogPeerReInviteWithoutOfferUnusableAnswerEndsTheCall(t *testing.T) {
	for _, side := range reInviteOfferlessSides {
		t.Run(side.name, func(t *testing.T) {
			med, req, handleReInvite, readAck, sentBye := side.setup(t)
			before := med.MediaSession()

			tx := newRespondedServerTx()
			handled := make(chan error, 1)
			go func() { handled <- handleReInvite(req, tx) }()
			select {
			case res := <-tx.responded:
				require.Equal(t, sip.StatusOK, res.StatusCode)
			case <-time.After(5 * time.Second):
				t.Fatal("the re-INVITE was not answered")
			}

			// The answer names a codec the media was never set up with, and
			// none the offer listed.
			answer := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34457, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_G729})
			require.ErrorIs(t, readAck(newReInviteAck(req, answer)), media.ErrNoCommonCodec)
			select {
			case err := <-handled:
				require.NoError(t, err)
			case <-time.After(5 * time.Second):
				t.Fatal("the re-INVITE handler did not return")
			}

			med.mu.Lock()
			pending := med.pendingMediaSession
			med.mu.Unlock()
			assert.Nil(t, pending, "the offer was kept after its answer failed")
			assert.Same(t, before, med.MediaSession(), "an answer that could not be applied replaced the media session")
			assert.True(t, sentBye(), "the call was not ended with a BYE")
		})
	}
}

// TestDialogPeerReInviteWithoutOfferTellsTheMedia pins what the media of a
// call is told when a re-INVITE of the peer's carries no offer. The answer in
// the ACK to our 2xx is handed over as the answer it is and the update
// committed, once. An ACK without an answer has the media drop the offer and
// commits nothing. Media that cannot commit drops the offer and the call is
// ended, and media that is closed by then drops it without ending anything.
func TestDialogPeerReInviteWithoutOfferTellsTheMedia(t *testing.T) {
	// acknowledged has d handle a re-INVITE without an offer and reads ack,
	// built for that re-INVITE, once the 2xx is out. It returns what reading
	// the ACK returned.
	acknowledged := func(t *testing.T, d *DialogServerSession, ack func(req *sip.Request) *sip.Request) error {
		t.Helper()
		req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
		tx := newTransmissionsTx()
		handled := make(chan error, 1)
		go func() { handled <- d.handleReInvite(t.Context(), req, tx) }()
		require.Equal(t, sip.StatusOK, nextTransmission(t, tx, 5*time.Second))

		ackErr := d.ReadAck(t.Context(), ack(req), newByeServerTx())
		select {
		case err := <-handled:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the re-INVITE did not end once its ACK was handled")
		}
		return ackErr
	}
	withAnswer := func(req *sip.Request) *sip.Request { return newReInviteAck(req, peerSDP()) }

	t.Run("answer", func(t *testing.T) {
		d, med, sent := newRecordedMediaCall(t)
		_, committed, _ := med.seen()

		require.NoError(t, acknowledged(t, d, withAnswer))
		answers, finalized, aborted := med.seen()
		assert.Equal(t, []string{string(peerSDP())}, answers)
		assert.Equal(t, committed+1, finalized, "the update is committed once")
		assert.Zero(t, aborted, "a committed update was dropped")
		assert.NotContains(t, sent.sent(), sip.BYE)
	})

	t.Run("no_answer", func(t *testing.T) {
		d, med, sent := newRecordedMediaCall(t)
		_, committed, _ := med.seen()

		require.NoError(t, acknowledged(t, d, func(req *sip.Request) *sip.Request { return newReInviteAck(req, nil) }))
		answers, finalized, aborted := med.seen()
		assert.Empty(t, answers)
		assert.Equal(t, committed, finalized, "an offer without an answer was committed")
		assert.Equal(t, 1, aborted)
		assert.NotContains(t, sent.sent(), sip.BYE, "an ACK without an answer ended the call")
	})

	t.Run("answer_declared_otherwise", func(t *testing.T) {
		d, med, sent := newRecordedMediaCall(t)

		require.NoError(t, acknowledged(t, d, func(req *sip.Request) *sip.Request {
			ack := newReInviteAck(req, nil)
			ack.AppendHeader(sip.NewHeader("Content-Type", "application/isup"))
			ack.SetBody(peerSDP())
			return ack
		}))
		answers, _, aborted := med.seen()
		assert.Empty(t, answers, "a body declared as something else was taken for the answer")
		assert.Equal(t, 1, aborted)
		assert.NotContains(t, sent.sent(), sip.BYE)
	})

	t.Run("not_committed", func(t *testing.T) {
		d, med, sent := newRecordedMediaCall(t)
		errNotKeyed := errors.New("media not keyed")
		med.finalizeErr = errNotKeyed

		require.ErrorIs(t, acknowledged(t, d, withAnswer), errNotKeyed)
		_, _, aborted := med.seen()
		assert.Equal(t, 1, aborted)
		assert.Contains(t, sent.sent(), sip.BYE, "the call was not ended with a BYE")
	})

	t.Run("media_closed", func(t *testing.T) {
		d, med, sent := newRecordedMediaCall(t)
		med.finalizeErr = fmt.Errorf("commit: %w", errMediaClosed)

		require.NoError(t, acknowledged(t, d, withAnswer))
		_, _, aborted := med.seen()
		assert.Equal(t, 1, aborted)
		assert.NotContains(t, sent.sent(), sip.BYE, "media closed meanwhile ended the call")
	})
}

// audioFormats returns the formats of the audio stream description lists, in
// its order.
func audioFormats(t *testing.T, description []byte) []string {
	t.Helper()
	sd := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(description, &sd))
	md, err := sd.MediaDescription("audio")
	require.NoError(t, err)
	return md.Formats
}

// TestDialogMediaRequestedOfferRenegotiatesNothing pins the offer the media
// makes when the peer's request asked for one. The media is set up with PCMU,
// PCMA and telephone-event on 101, and the call runs on PCMA and
// telephone-event on 96, the number the peer gave it. The offer carries the
// codecs the call runs on, under the numbers they were negotiated with, so the
// peer is not asked to move the call. Once answered, the installed session
// receives each codec on the number it offered it with, and still holds every
// codec it was set up with. An offer nobody asked for, as a re-INVITE of ours
// makes, carries every codec the media was set up with.
func TestDialogMediaRequestedOfferRenegotiatesNothing(t *testing.T) {
	telephoneEvent96 := media.CodecTelephoneEvent8000
	telephoneEvent96.PayloadType = 96
	newSession := func(t *testing.T, codecs ...media.Codec) *media.MediaSession {
		t.Helper()
		sess := &media.MediaSession{
			Codecs: codecs,
			Laddr:  net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 0},
			Mode:   sdp.ModeSendrecv,
		}
		require.NoError(t, sess.Init())
		t.Cleanup(func() { sess.Close() })
		return sess
	}
	configured := []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000}
	// newCall returns the media of a call answered to a peer that offered
	// PCMA and telephone-event on 96.
	newCall := func(t *testing.T) *DialogMedia {
		t.Helper()
		peer := newSession(t, media.CodecAudioAlaw, telephoneEvent96)
		sess := newSession(t, configured...)
		require.NoError(t, sess.RemoteSDP(peer.LocalSDP(false), false))
		return newICEDialogMedia(t, sess)
	}

	t.Run("requested", func(t *testing.T) {
		med := newCall(t)
		ctx := t.Context()

		require.NoError(t, med.onRemoteSDP(ctx, nil, false))
		offer, err := med.onLocalSDP(ctx, false, "")
		require.NoError(t, err)
		assert.Equal(t, []string{"8", "96"}, audioFormats(t, offer))

		answerer := newSession(t, media.CodecAudioAlaw, telephoneEvent96)
		require.NoError(t, answerer.RemoteSDP(offer, false))
		require.NoError(t, med.onRemoteSDP(ctx, answerer.LocalSDP(true), true))
		require.NoError(t, med.onFinalize(ctx))

		installed := med.MediaSession()
		assert.Equal(t, answerer.Laddr.Port, installed.Raddr.Port, "the answer was not applied")
		type numbers struct {
			name       string
			send, recv uint8
		}
		var runsOn []numbers
		for _, c := range installed.CommonCodecs() {
			runsOn = append(runsOn, numbers{name: c.Name, send: c.PayloadType, recv: c.PayloadTypeRecv()})
		}
		assert.Equal(t, []numbers{
			{name: media.CodecAudioAlaw.Name, send: 8, recv: 8},
			{name: media.CodecTelephoneEvent8000.Name, send: 96, recv: 96},
		}, runsOn)
		var holds []string
		for _, c := range installed.Codecs {
			holds = append(holds, c.Name)
		}
		assert.Equal(t, []string{media.CodecAudioUlaw.Name, media.CodecAudioAlaw.Name, media.CodecTelephoneEvent8000.Name}, holds,
			"the installed session lost codecs it was set up with")
	})

	t.Run("not_requested", func(t *testing.T) {
		med := newCall(t)

		offer, err := med.onLocalSDP(t.Context(), false, "")
		require.NoError(t, err)
		assert.Equal(t, []string{"0", "8", "101"}, audioFormats(t, offer))
	})

	// The request is good for the one description that follows it: an offer
	// made after the requested one was dropped is an ordinary one again.
	t.Run("request_dropped", func(t *testing.T) {
		med := newCall(t)
		ctx := t.Context()

		require.NoError(t, med.onRemoteSDP(ctx, nil, false))
		med.abortPendingMediaSession()
		offer, err := med.onLocalSDP(ctx, false, "")
		require.NoError(t, err)
		assert.Equal(t, []string{"0", "8", "101"}, audioFormats(t, offer))
	})
}

// TestWithOfferedPayloadTypes pins the numbers a fork holds its codecs under
// once it has offered some of them under the numbers the call runs on: the
// offered ones take those numbers, and a codec that held one of them takes
// the number given up, so no number names two codecs.
func TestWithOfferedPayloadTypes(t *testing.T) {
	numbered := func(c media.Codec, payloadType uint8) media.Codec {
		c.PayloadType = payloadType
		return c
	}
	tests := []struct {
		name       string
		configured []media.Codec
		offered    []media.Codec
		want       []media.Codec
	}{
		{
			name:       "same_numbers",
			configured: []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000},
			offered:    []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000},
			want:       []media.Codec{media.CodecAudioUlaw, media.CodecTelephoneEvent8000},
		},
		{
			name:       "renumbered",
			configured: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, media.CodecTelephoneEvent8000},
			offered:    []media.Codec{media.CodecAudioAlaw, numbered(media.CodecTelephoneEvent8000, 96)},
			want:       []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw, numbered(media.CodecTelephoneEvent8000, 96)},
		},
		{
			name:       "number_held_by_another",
			configured: []media.Codec{media.CodecAudioOpus, media.CodecTelephoneEvent8000},
			offered:    []media.Codec{numbered(media.CodecTelephoneEvent8000, 96)},
			want:       []media.Codec{numbered(media.CodecAudioOpus, 101), numbered(media.CodecTelephoneEvent8000, 96)},
		},
		{
			name:       "numbers_exchanged",
			configured: []media.Codec{media.CodecAudioOpus, media.CodecTelephoneEvent8000},
			offered:    []media.Codec{numbered(media.CodecAudioOpus, 101), numbered(media.CodecTelephoneEvent8000, 96)},
			want:       []media.Codec{numbered(media.CodecAudioOpus, 101), numbered(media.CodecTelephoneEvent8000, 96)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configured := append([]media.Codec(nil), tt.configured...)
			assert.Equal(t, tt.want, withOfferedPayloadTypes(tt.configured, tt.offered))
			assert.Equal(t, configured, tt.configured, "the configured list was changed in place")
		})
	}
}
