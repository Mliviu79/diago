// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"net"
	"testing"
	"testing/synctest"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errAnswerCut is the cause a test cuts an answer's context with.
var errAnswerCut = errors.New("the answer's owner stopped waiting")

// newAnswerContextDialog builds an inbound dialog whose INVITE carries the
// offer of a peer, over a fake transaction that hands its first response to a
// channel and drops its retransmissions. The requests the dialog sends, such as
// a BYE, are answered 200 and handed to the returned channel.
func newAnswerContextDialog(t *testing.T) (*DialogServerSession, *firstResponseTx, <-chan *sip.Request) {
	t.Helper()
	peer := newMediaSessionForTest(t)
	inviteTx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
	d, sent := newRecordingServerDialog(t, inviteTx, peer.LocalSDP(false), MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	})
	return d, inviteTx, sent
}

// answerContext is AnswerContext for a test that reads only its error.
func answerContext(d *DialogServerSession) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		_, err := d.AnswerContext(ctx)
		return err
	}
}

// cutWhileAwaitingAck runs answer on another goroutine with a context the test
// cuts once the 2xx is sent, and requires answer to return at that cut, with
// an error carrying its cause, while the 2xx still awaits its ACK.
func cutWhileAwaitingAck(t *testing.T, d *DialogServerSession, inviteTx *firstResponseTx, answer func(ctx context.Context) error) *sip.Response {
	t.Helper()
	ctx, cut := context.WithCancelCause(t.Context())
	defer cut(nil)
	answered := make(chan error, 1)
	go func() { answered <- answer(ctx) }()

	var res *sip.Response
	select {
	case res = <-inviteTx.first:
		require.Equal(t, sip.StatusOK, res.StatusCode)
	case <-time.After(5 * time.Second):
		t.Fatal("no 2xx was sent")
	}

	cutAt := time.Now()
	cut(errAnswerCut)
	select {
	case err := <-answered:
		require.ErrorIs(t, err, errAnswerCut, "the answer must report why its wait was cut")
		require.Less(t, time.Since(cutAt), sip.T1, "the answer must return at its context's end, not with the ACK wait")
	case <-time.After(5 * time.Second):
		t.Fatal("the answer outlived its context")
	}
	require.Equal(t, sip.DialogStateEstablished, d.LoadState(), "the 2xx still awaits its ACK")
	return res
}

// requireNoRequest requires that the dialog sent no request, such as a BYE,
// within window.
func requireNoRequest(t *testing.T, sent <-chan *sip.Request, window time.Duration, why string) {
	t.Helper()
	select {
	case req := <-sent:
		t.Fatalf("%s: %s was sent", why, req.Method)
	case <-time.After(window):
	}
}

// requireByeEndsDialog requires that the dialog itself sends a BYE within
// within and ends with it, and returns the instant the dialog ended.
func requireByeEndsDialog(t *testing.T, d *DialogServerSession, sent <-chan *sip.Request, within time.Duration) time.Time {
	t.Helper()
	select {
	case req := <-sent:
		require.Equal(t, sip.BYE, req.Method, "the dialog must end the call it answered for nobody")
	case <-time.After(within):
		t.Fatal("the dialog sent no BYE")
	}
	select {
	case <-d.Context().Done():
		return time.Now()
	case <-time.After(5 * time.Second):
		t.Fatal("the dialog did not end with its BYE")
	}
	return time.Time{}
}

// requireCloseJoins requires that Close returns, which joins the rest of an
// answer whose wait was cut.
func requireCloseJoins(t *testing.T, d *DialogServerSession) {
	t.Helper()
	closed := make(chan error, 1)
	go func() { closed <- d.Close() }()
	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not join the answer")
	}
}

// TestDialogServerAnswerContextLateAck is an answer whose context is cut while
// its 2xx awaits the ACK, and whose ACK arrives after the cut. AnswerContext
// returns at the cut. The dialog goes on retransmitting the 2xx and reads the
// late ACK, which cannot be refused (RFC 3261 section 13.3.1.4). Nobody is left
// to use the answer, so the dialog itself ends the call with a BYE as soon as
// the ACK has confirmed it, and never before (RFC 3261 section 15): the
// dialog's release completes at the ACK.
func TestDialogServerAnswerContextLateAck(t *testing.T) {
	d, inviteTx, sent := newAnswerContextDialog(t)
	cutWhileAwaitingAck(t, d, inviteTx, answerContext(d))

	requireNoRequest(t, sent, 2*sip.T1, "a BYE before the ACK")
	require.Equal(t, sip.DialogStateEstablished, d.LoadState())

	ackAt := time.Now()
	readAck(t, d)
	endedAt := requireByeEndsDialog(t, d, sent, 5*time.Second)
	assert.Less(t, endedAt.Sub(ackAt), sip.T1, "the release must complete at the late ACK")
	requireCloseJoins(t, d)
}

// TestDialogServerAnswerContextNoAck is an answer whose context is cut while
// its 2xx awaits the ACK, and whose ACK never comes. The dialog retransmits
// the 2xx for 64*T1, and then ends the call with a BYE (RFC 3261 section
// 13.3.1.4): the dialog's release completes at 64*T1 from the 2xx.
func TestDialogServerAnswerContextNoAck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, inviteTx, sent := newAnswerContextDialog(t)
		start := time.Now()
		cutWhileAwaitingAck(t, d, inviteTx, answerContext(d))

		synctest.Wait()
		select {
		case req := <-sent:
			t.Fatalf("a %s was sent before the ACK wait ended", req.Method)
		default:
		}

		endedAt := requireByeEndsDialog(t, d, sent, 64*sip.T1+5*time.Second)
		assert.Equal(t, 64*sip.T1, endedAt.Sub(start), "the release must complete when the ACK wait ends")
		requireCloseJoins(t, d)
	})
}

// TestDialogServerAnswerLateContextCut is AnswerLateContext, whose 2xx
// carries our offer, cut while that 2xx awaits the ACK that carries the
// answer: it returns at the cut, and the late ACK is still read, and followed
// by the dialog's BYE.
func TestDialogServerAnswerLateContextCut(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	})
	res := cutWhileAwaitingAck(t, d, inviteTx, func(ctx context.Context) error {
		_, err := d.AnswerLateContext(ctx)
		return err
	})

	requireNoRequest(t, sent, 2*sip.T1, "a BYE before the ACK")

	peer := newMediaSessionForTest(t)
	require.NoError(t, peer.RemoteSDP(res.Body(), false))
	require.NoError(t, d.ReadAck(newLateAck(d, peer.LocalSDP(true)), newByeServerTx()))
	requireByeEndsDialog(t, d, sent, 5*time.Second)
	requireCloseJoins(t, d)
}

// TestDialogServerAnswerWithinCutWhileRefusing is an answer that refuses the
// INVITE instead of answering it, cut while that final awaits its ACK: the
// answer returns at the cut, and the INVITE's transaction ends the refusal on
// its own, at the latest at Timer H (RFC 3261 section 17.2.1). No BYE follows
// a refusal.
func TestDialogServerAnswerWithinCutWhileRefusing(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, MediaConfig{})
	ctx, cut := context.WithCancelCause(t.Context())
	defer cut(nil)
	answered := make(chan error, 1)
	go func() {
		answered <- d.answerWithin(ctx, d.markAnswering(), func() error {
			return d.Respond(d.Context(), sip.StatusNotAcceptableHere, "Not Acceptable Here", nil)
		})
	}()
	select {
	case res := <-inviteTx.first:
		require.Equal(t, sip.StatusNotAcceptableHere, res.StatusCode)
	case <-time.After(5 * time.Second):
		t.Fatal("no refusal was sent")
	}

	cutAt := time.Now()
	cut(errAnswerCut)
	select {
	case err := <-answered:
		require.ErrorIs(t, err, errAnswerCut)
		require.Less(t, time.Since(cutAt), sip.T1, "the answer must return at its context's end")
	case <-time.After(5 * time.Second):
		t.Fatal("the answer outlived its context while its refusal awaited the ACK")
	}

	// The INVITE transaction ends, as it does once the peer acknowledges
	// the refusal.
	close(inviteTx.done)
	requireCloseJoins(t, d)
	assert.Equal(t, sip.DialogStateEnded, d.LoadState())
	select {
	case req := <-sent:
		t.Errorf("the dialog sent %s after its refusal", req.Method)
	default:
	}
}

// TestDialogServerAnswerContextCutWhileRefusing is an answer that refuses the
// INVITE 422, its Session-Expires being below our Min-SE (RFC 4028 section 6),
// cut while that final awaits its ACK: AnswerLateContext returns at the cut,
// and the INVITE's transaction ends the refusal on its own, at the latest at
// Timer H (RFC 3261 section 17.2.1).
func TestDialogServerAnswerContextCutWhileRefusing(t *testing.T) {
	d, inviteTx, _ := newLateOfferTimerDialog(t,
		sip.NewHeader("Supported", "timer"),
		sip.NewHeader("Session-Expires", "60"),
	)
	ctx, cut := context.WithCancelCause(t.Context())
	defer cut(nil)
	answered := make(chan error, 1)
	go func() {
		_, err := d.AnswerLateContext(ctx)
		answered <- err
	}()
	select {
	case res := <-inviteTx.first:
		require.Equal(t, statusSessionIntervalTooSmall, res.StatusCode)
	case <-time.After(5 * time.Second):
		t.Fatal("no 422 was sent")
	}

	cutAt := time.Now()
	cut(errAnswerCut)
	select {
	case err := <-answered:
		require.ErrorIs(t, err, errAnswerCut)
		require.Less(t, time.Since(cutAt), sip.T1, "the answer must return at its context's end")
	case <-time.After(5 * time.Second):
		t.Fatal("the answer outlived its context while its 422 awaited the ACK")
	}

	// The INVITE transaction ends, as it does once the peer acknowledges
	// the 422.
	close(inviteTx.done)
	requireCloseJoins(t, d)
}

// TestEndWhileAckWithheld is an end requested, with Hangup, while our 2xx
// awaits an ACK that never comes. The BYE cannot leave before the ACK or the end
// of the ACK wait (RFC 3261 section 15), so the release completes when the wait
// ends, 64*T1 from the 2xx (the same figure as Timer H, section 17.2.1), plus the
// BYE's own transaction: the fake peer answers it at once, so nothing else
// delays it. The end does not wait on the answer's goroutine, the media or the
// INVITE transaction beyond the ACK wait.
func TestEndWhileAckWithheld(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		d, inviteTx, sent := newAnswerContextDialog(t)
		start := time.Now()

		answered := make(chan error, 1)
		go func() { answered <- answerContext(d)(t.Context()) }()
		select {
		case res := <-inviteTx.first:
			require.Equal(t, sip.StatusOK, res.StatusCode)
		case <-time.After(5 * time.Second):
			t.Fatal("no 2xx was sent")
		}

		endCtx, cancel := context.WithTimeout(t.Context(), 64*sip.T1+sip.Timer_F)
		defer cancel()
		ended := make(chan error, 1)
		go func() { ended <- d.Hangup(endCtx) }()

		synctest.Wait()
		select {
		case req := <-sent:
			t.Fatalf("a %s was sent while the ACK was still awaited", req.Method)
		default:
		}

		select {
		case err := <-ended:
			require.NoError(t, err)
		case <-time.After(64*sip.T1 + sip.Timer_F + time.Second):
			t.Fatal("the end outlived the ACK wait and the BYE's Timer F")
		}
		assert.Equal(t, 64*sip.T1, time.Since(start), "the release must complete when the ACK wait ends, waiting on nothing else")
		select {
		case req := <-sent:
			require.Equal(t, sip.BYE, req.Method)
		default:
			t.Fatal("no BYE was sent")
		}
		<-answered
		requireCloseJoins(t, d)
	})
}

// awaitFirstResponse returns the first response sent on inviteTx.
func awaitFirstResponse(t *testing.T, inviteTx *firstResponseTx) *sip.Response {
	t.Helper()
	select {
	case res := <-inviteTx.first:
		return res
	case <-time.After(5 * time.Second):
		t.Fatal("no response was sent")
	}
	return nil
}

// contextAnswer is what an answer whose wait ends with a ctx returned.
type contextAnswer struct {
	med *DialogMedia
	err error
}

// awaitContextAnswer waits for an answer to report.
func awaitContextAnswer(t *testing.T, answered <-chan contextAnswer) contextAnswer {
	t.Helper()
	select {
	case answer := <-answered:
		return answer
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not return")
	}
	return contextAnswer{}
}

// TestDialogServerAnswerContextAnswers pins the call AnswerContext and
// AnswerOptions hand back once the ACK is read: the media the offer in the
// INVITE and the answer in the 2xx agreed on, started, on a confirmed dialog.
// The RTCP monitor of that media is started by the reading of the ACK, once.
func TestDialogServerAnswerContextAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(ctx context.Context, d *DialogServerSession) (*DialogMedia, error)
	}{
		{name: "AnswerContext", answer: func(ctx context.Context, d *DialogServerSession) (*DialogMedia, error) {
			return d.AnswerContext(ctx)
		}},
		{name: "AnswerOptions", answer: func(ctx context.Context, d *DialogServerSession) (*DialogMedia, error) {
			return d.AnswerOptions(ctx, AnswerOptions{Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw}})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := newMediaSessionForTest(t)
			inviteTx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
			d, sent := newRecordingServerDialog(t, inviteTx, peer.LocalSDP(false), MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw},
				bindIP: net.IPv4(127, 0, 0, 1),
			})

			answered := make(chan contextAnswer, 1)
			go func() {
				med, err := tc.answer(t.Context(), d)
				answered <- contextAnswer{med: med, err: err}
			}()
			res := awaitFirstResponse(t, inviteTx)
			require.Equal(t, sip.StatusOK, res.StatusCode)
			require.NoError(t, peer.RemoteSDP(res.Body(), true), "the 2xx carries the answer to the offer")

			readAck(t, d)
			answer := awaitContextAnswer(t, answered)
			require.NoError(t, answer.err)
			require.NotNil(t, answer.med)
			assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())

			sess := answer.med.MediaSession()
			assert.Equal(t, peer.Laddr.Port, sess.Raddr.Port, "the media goes where the offer says")
			assert.Equal(t, sess.Laddr.Port, peer.Raddr.Port, "the answer describes the media handed back")
			require.NotNil(t, answer.med.RTPSession())
			assert.Error(t, answer.med.RTPSession().MonitorBackground(), "the RTCP monitor is already running")
			_, err := answer.med.AudioReader()
			assert.NoError(t, err)
			_, err = answer.med.AudioWriter()
			assert.NoError(t, err)

			// A copy of the ACK starts nothing again and the call goes on.
			require.NoError(t, d.ReadAck(newLateAck(d, nil), newByeServerTx()))
			assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
			select {
			case req := <-sent:
				t.Errorf("the dialog sent %s on an answered call", req.Method)
			default:
			}

			// The session closes the media it was answered with.
			mediaClosed := false
			answer.med.OnClose(func() error {
				mediaClosed = true
				return nil
			})
			require.NoError(t, d.Close())
			assert.True(t, mediaClosed, "the media was not closed with the session")
		})
	}
}

// TestDialogServerAnswerLateContextAnswers pins the call AnswerLateContext
// hands back: its 2xx carries our offer, the ACK the answer (RFC 3261 section
// 13.2.1), applied to the media session that made the offer, and the media is
// started by the reading of that ACK.
func TestDialogServerAnswerLateContextAnswers(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	})
	answered := make(chan contextAnswer, 1)
	go func() {
		med, err := d.AnswerLateContext(t.Context())
		answered <- contextAnswer{med: med, err: err}
	}()
	res := awaitFirstResponse(t, inviteTx)
	require.Equal(t, sip.StatusOK, res.StatusCode)

	peer := newMediaSessionForTest(t)
	require.NoError(t, peer.RemoteSDP(res.Body(), false))
	require.NoError(t, d.ReadAck(newLateAck(d, peer.LocalSDP(true)), newByeServerTx()))

	answer := awaitContextAnswer(t, answered)
	require.NoError(t, answer.err)
	require.NotNil(t, answer.med)
	assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())

	sess := answer.med.MediaSession()
	assert.Equal(t, peer.Laddr.Port, sess.Raddr.Port, "the media goes where the answer says")
	assert.Equal(t, sess.Laddr.Port, peer.Raddr.Port, "the answer was applied to the session that made the offer")
	require.NotNil(t, answer.med.RTPSession())
	assert.Error(t, answer.med.RTPSession().MonitorBackground(), "the RTCP monitor is already running")
	select {
	case req := <-sent:
		t.Errorf("the dialog sent %s on an answered call", req.Method)
	default:
	}
}

// TestDialogServerAnswerContextEnded pins that an answer whose ctx has ended
// already answers nothing: no final response is sent, the INVITE stays
// unanswered, and the error carries the cause.
func TestDialogServerAnswerContextEnded(t *testing.T) {
	for _, tc := range []struct {
		name   string
		late   bool
		answer func(ctx context.Context, d *DialogServerSession) (*DialogMedia, error)
	}{
		{name: "AnswerContext", answer: func(ctx context.Context, d *DialogServerSession) (*DialogMedia, error) {
			return d.AnswerContext(ctx)
		}},
		{name: "AnswerOptions", answer: func(ctx context.Context, d *DialogServerSession) (*DialogMedia, error) {
			return d.AnswerOptions(ctx, AnswerOptions{})
		}},
		{name: "AnswerLateContext", late: true, answer: func(ctx context.Context, d *DialogServerSession) (*DialogMedia, error) {
			return d.AnswerLateContext(ctx)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var offer []byte
			if !tc.late {
				offer = newMediaSessionForTest(t).LocalSDP(false)
			}
			inviteTx := &firstResponseTx{byeServerTx: newByeServerTx(), first: make(chan *sip.Response, 1)}
			d, _ := newRecordingServerDialog(t, inviteTx, offer, MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw},
				bindIP: net.IPv4(127, 0, 0, 1),
			})
			ctx, cut := context.WithCancelCause(t.Context())
			cut(errAnswerCut)

			med, err := tc.answer(ctx, d)
			require.ErrorIs(t, err, errAnswerCut)
			assert.Nil(t, med)
			select {
			case res := <-inviteTx.first:
				t.Errorf("a %d was sent on an ended ctx", res.StatusCode)
			default:
			}
			assert.Less(t, d.LoadState(), sip.DialogStateEstablished, "the INVITE stays unanswered")
		})
	}
}

// TestDialogServerAnswerContextWithoutOffer pins that AnswerContext refuses to
// answer an INVITE that carried no offer, and sends nothing: such an INVITE
// is answered with AnswerLateContext, whose 2xx carries our offer.
func TestDialogServerAnswerContextWithoutOffer(t *testing.T) {
	d, inviteTx, _ := newLateOfferDialog(t, MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	})
	med, err := d.AnswerContext(t.Context())
	require.ErrorIs(t, err, errNoOfferInInvite)
	assert.Nil(t, med)
	select {
	case res := <-inviteTx.first:
		t.Errorf("a %d was sent", res.StatusCode)
	default:
	}
}

// TestDialogServerAnswerLateContextCutAckWithoutAnswer is AnswerLateContext
// cut while its 2xx awaits the ACK, whose ACK then brings no answer to the
// offer in that 2xx. Nobody waits for the answer any more, and the ACK cannot
// be refused, so the dialog itself ends the call, with one BYE, once the ACK
// has confirmed it.
func TestDialogServerAnswerLateContextCutAckWithoutAnswer(t *testing.T) {
	d, inviteTx, sent := newLateOfferDialog(t, MediaConfig{
		Codecs: []media.Codec{media.CodecAudioUlaw},
		bindIP: net.IPv4(127, 0, 0, 1),
	})
	cutWhileAwaitingAck(t, d, inviteTx, func(ctx context.Context) error {
		_, err := d.AnswerLateContext(ctx)
		return err
	})

	ack := sip.NewRequest(sip.ACK, d.InviteRequest.Contact().Address)
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: d.InviteRequest.CSeq().SeqNo, MethodName: sip.ACK})
	assert.ErrorIs(t, d.ReadAck(ack, newByeServerTx()), errAckCarriesNoAnswer)

	requireByeEndsDialog(t, d, sent, 5*time.Second)
	requireCloseJoins(t, d)
	select {
	case req := <-sent:
		t.Errorf("the dialog sent a second request, %s, to end the call", req.Method)
	default:
	}
}
