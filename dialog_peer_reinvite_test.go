// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// answerLog records, across transactions, the order in which requests are
// answered.
type answerLog struct {
	mu      sync.Mutex
	answers []string
}

func (l *answerLog) list() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.answers...)
}

// loggedServerTx is a byeServerTx that writes each response to a shared log
// under its request's name.
type loggedServerTx struct {
	*byeServerTx
	name string
	log  *answerLog
}

func newLoggedServerTx(name string, log *answerLog) *loggedServerTx {
	return &loggedServerTx{byeServerTx: newByeServerTx(), name: name, log: log}
}

func (tx *loggedServerTx) Respond(_ context.Context, res *sip.Response) error {
	tx.log.mu.Lock()
	tx.log.answers = append(tx.log.answers, fmt.Sprintf("%s %d", tx.name, res.StatusCode))
	tx.log.mu.Unlock()
	return nil
}

// TestDialogReInviteRacingBye pins how the peer's BYE is handled while its
// re-INVITE is pending, on either side. sipgo hands each request to its handler
// on its own goroutine. The re-INVITE here carries an offer, and its media
// update callback reads the BYE and holds the re-INVITE until the BYE is
// answered. RFC 3261 section 15.1.2 has the BYE answered and every pending
// request answered too, 487 recommended. So the BYE is answered 200 while the
// re-INVITE is still pending, which gets 487, and never a 200 after the BYE's.
func TestDialogReInviteRacingBye(t *testing.T) {
	sides := []struct {
		name string
		// setup returns a live dialog's media and the handling of the
		// peer's re-INVITE, with an offer, and of its BYE.
		setup func(t *testing.T) (*DialogMedia, func(sip.ServerTransaction) error, func(sip.ServerTransaction) error)
	}{
		{
			name: "inbound",
			setup: func(t *testing.T) (*DialogMedia, func(sip.ServerTransaction) error, func(sip.ServerTransaction) error) {
				d, med := newAnsweredTestServerCall(t)

				seq := d.InviteRequest.CSeq().SeqNo
				reInvite := newInDialogReInvite(t, d, seq+1)
				reInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
				reInvite.SetBody(peerSDP())
				bye := newBye(t, d, seq+2)
				return med,
					func(tx sip.ServerTransaction) error { return d.handleReInvite(t.Context(), reInvite, tx) },
					func(tx sip.ServerTransaction) error { return d.ReadBye(t.Context(), bye, tx) }
			},
		},
		{
			name: "outbound",
			setup: func(t *testing.T) (*DialogMedia, func(sip.ServerTransaction) error, func(sip.ServerTransaction) error) {
				d, med, _ := newAnsweredTestClientCall(t)

				seq := d.InviteRequest.CSeq().SeqNo
				reInvite := newPeerReInvite(d, seq+1)
				reInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
				reInvite.SetBody(peerSDP())
				bye := newPeerInDialogRequest(sip.BYE, seq+2)
				return med,
					func(tx sip.ServerTransaction) error { return d.handleReInvite(t.Context(), reInvite, tx) },
					func(tx sip.ServerTransaction) error { return d.ReadBye(t.Context(), bye, tx) }
			},
		},
	}

	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			m, handleReInvite, readBye := side.setup(t)
			log := &answerLog{}

			var byeErr error
			byeDone := make(chan struct{})
			byeAnswered := make(chan bool, 1)
			m.mu.Lock()
			m.onMediaUpdate = func(*DialogMedia) {
				go func() {
					defer close(byeDone)
					byeErr = readBye(newLoggedServerTx("BYE", log))
				}()
				select {
				case <-byeDone:
					byeAnswered <- true
				case <-time.After(5 * time.Second):
					byeAnswered <- false
				}
			}
			m.mu.Unlock()

			require.NoError(t, handleReInvite(newLoggedServerTx("re-INVITE", log)))
			require.True(t, <-byeAnswered, "the BYE waited for the re-INVITE")
			require.NoError(t, byeErr)
			assert.Equal(t, []string{"BYE 200", "re-INVITE 487"}, log.list())
		})
	}
}

// transmissionsTx is a server transaction that records the status of each
// response it is asked to send, as the peer never receives them.
type transmissionsTx struct {
	*byeServerTx
	sent chan int
}

func newTransmissionsTx() *transmissionsTx {
	return &transmissionsTx{byeServerTx: newByeServerTx(), sent: make(chan int, 256)}
}

func (tx *transmissionsTx) Respond(_ context.Context, res *sip.Response) error {
	tx.sent <- res.StatusCode
	return nil
}

// reInviteAckSides builds, for each side of a call, a confirmed dialog with
// media, a re-INVITE of the peer's carrying an offer, and the handling of it,
// of the ACK to our 2xx and of the peer's BYE. s is the dialog's own SIP
// state, where the timers of the 2xx's retransmission are set, and sentBye
// reports whether the dialog has sent a BYE.
var reInviteAckSides = []struct {
	name  string
	setup func(t *testing.T) (s *sessionState, req *sip.Request, handleReInvite func(sip.ServerTransaction) error, readAck func(*sip.Request) error, readBye func(sip.ServerTransaction) error, sentBye func() bool)
}{
	{
		name: "inbound",
		setup: func(t *testing.T) (*sessionState, *sip.Request, func(sip.ServerTransaction) error, func(*sip.Request) error, func(sip.ServerTransaction) error, func() bool) {
			ua, sent := newRecordingDialogUA(t)
			d, _ := newAnsweredTestServerCallOverUA(t, ua)

			req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
			req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			req.SetBody(peerSDP())
			sentBye := func() bool {
				return slices.Contains(sent.sent(), sip.BYE)
			}
			return d.sipState(), req,
				func(tx sip.ServerTransaction) error { return d.handleReInvite(t.Context(), req, tx) },
				func(ack *sip.Request) error { return d.ReadAck(t.Context(), ack, newByeServerTx()) },
				func(tx sip.ServerTransaction) error {
					return d.ReadBye(t.Context(), newBye(t, d, req.CSeq().SeqNo+1), tx)
				},
				sentBye
		},
	},
	{
		name: "outbound",
		setup: func(t *testing.T) (*sessionState, *sip.Request, func(sip.ServerTransaction) error, func(*sip.Request) error, func(sip.ServerTransaction) error, func() bool) {
			d, sent := newAnsweredTestClientDialog(t)
			req := newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1)
			req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			req.SetBody(peerSDP())
			sentBye := func() bool {
				return slices.Contains(sent.sent(), sip.BYE)
			}
			return d.sipState(), req,
				func(tx sip.ServerTransaction) error { return d.handleReInvite(t.Context(), req, tx) },
				func(ack *sip.Request) error { return d.handleReInviteACK(t.Context(), ack, newByeServerTx()) },
				func(tx sip.ServerTransaction) error {
					return d.ReadBye(t.Context(), newPeerInDialogRequest(sip.BYE, req.CSeq().SeqNo+1), tx)
				},
				sentBye
		},
	},
}

// nextTransmission returns the status of the next response tx sends, and
// fails the test when none comes within wait.
func nextTransmission(t *testing.T, tx *transmissionsTx, wait time.Duration) int {
	t.Helper()
	select {
	case status := <-tx.sent:
		return status
	case <-time.After(wait):
		t.Fatal("no response was sent")
		return 0
	}
}

// TestDialogReInvite2xxRetransmission pins that our 2xx to a re-INVITE of the
// peer's is passed to its transaction again until the ACK is read, on either
// side. The server transaction does not retransmit a 2xx; the UAS core does,
// at T1, doubling up to T2, for 64*T1, and then ends the call with a BYE (RFC
// 3261 section 13.3.1.4, RFC 6026 section 7.1). Before, the 2xx was sent once,
// so a lost 2xx left the peer's re-INVITE unanswered.
func TestDialogReInvite2xxRetransmission(t *testing.T) {
	for _, side := range reInviteAckSides {
		t.Run(side.name, func(t *testing.T) {
			t.Run("first 2xx lost", func(t *testing.T) {
				_, req, handleReInvite, readAck, _, sentBye := side.setup(t)
				tx := newTransmissionsTx()
				handled := make(chan error, 1)
				go func() { handled <- handleReInvite(tx) }()

				require.Equal(t, sip.StatusOK, nextTransmission(t, tx, 5*time.Second))
				// The peer never got it: the 2xx must go out again.
				require.Equal(t, sip.StatusOK, nextTransmission(t, tx, 4*sip.T1), "the 2xx was not retransmitted")
				select {
				case err := <-handled:
					t.Fatalf("the re-INVITE ended before its ACK: %v", err)
				default:
				}

				require.NoError(t, readAck(newReInviteAck(req, nil)))
				select {
				case err := <-handled:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("the ACK did not end the retransmission")
				}
				assert.False(t, sentBye(), "an acknowledged re-INVITE ended the call")
			})

			t.Run("no ACK", func(t *testing.T) {
				s, _, handleReInvite, _, _, sentBye := side.setup(t)
				s.ackTimers = ackTimers{t1: 10 * time.Millisecond, t2: 40 * time.Millisecond, timeout: 400 * time.Millisecond}
				tx := newTransmissionsTx()
				handled := make(chan error, 1)
				go func() { handled <- handleReInvite(tx) }()
				select {
				case err := <-handled:
					require.ErrorIs(t, err, errReInviteAckTimeout)
				case <-time.After(5 * time.Second):
					t.Fatal("the retransmission did not give up")
				}
				// 0, 10, 30, 70, then every 40 ms up to 400 ms.
				assert.GreaterOrEqual(t, len(tx.sent), 3)
				assert.LessOrEqual(t, len(tx.sent), 13)
				assert.True(t, sentBye(), "the call was not ended with a BYE")
			})

			t.Run("BYE", func(t *testing.T) {
				_, _, handleReInvite, _, readBye, _ := side.setup(t)
				log := &answerLog{}
				tx := newLoggedServerTx("re-INVITE", log)
				handled := make(chan error, 1)
				go func() { handled <- handleReInvite(tx) }()
				require.Eventually(t, func() bool { return len(log.list()) == 1 }, 5*time.Second, time.Millisecond)

				require.NoError(t, readBye(newLoggedServerTx("BYE", log)))
				select {
				case err := <-handled:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					t.Fatal("the end of the dialog did not end the retransmission")
				}
				assert.Equal(t, []string{"re-INVITE 200", "BYE 200"}, log.list(), "a 2xx went out after the BYE")
			})
		})
	}
}

// TestDialogClientReInviteACKReadsMediaUnderLock asserts the ACK to a peer's
// re-INVITE reads the media session under the lock of the dialog's media.
// Handling another in-dialog offer swaps the session under that lock, so an
// unguarded read races it. The race only shows under -race. The dialog is a
// real one, since the ACK's handshake is bounded by its context.
func TestDialogClientReInviteACKReadsMediaUnderLock(t *testing.T) {
	d := newTestClientDialog(t, sip.DialogStateConfirmed)
	med := &DialogMedia{}
	med.InitMediaSession(&media.MediaSession{}, nil, nil)
	med.registerDialogCallbacks(&d.dialogCallbacks)

	swapped := make(chan struct{})
	go func() {
		defer close(swapped)
		med.mu.Lock()
		med.mediaSession = &media.MediaSession{}
		med.mu.Unlock()
	}()

	ack := sip.NewRequest(sip.ACK, sip.Uri{User: "dg", Host: "127.0.0.1"})
	require.NoError(t, d.handleReInviteACK(t.Context(), ack, nil))
	<-swapped
}

// newRecordedMediaCall returns a confirmed inbound call whose media is a
// recording one, registered before the answer, and what the dialog sends.
func newRecordedMediaCall(t *testing.T) (*DialogServerSession, *recordingMedia, *sentRequests) {
	t.Helper()
	ua, sent := newRecordingDialogUA(t)
	inviteTx := &sentServerTx{byeServerTx: newByeServerTx(), sent: make(chan struct{})}
	d := newTestDialogOverUA(t, inviteTx, ua)
	med := &recordingMedia{}
	med.register(d)
	answered := sendOK(t, d, inviteTx)
	confirm(t, d)
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not return")
	}
	return d, med, sent
}

// TestDialogPeerReInviteCommitsOrAbortsTheMedia pins what the media of a call
// is told about the update a re-INVITE of the peer's stages with its offer.
// The ACK to our 2xx commits it, once, and the handling of the re-INVITE ends
// only after that commit. Every other end of the re-INVITE leaves the update
// uncommitted, and the media is told to drop it: a refusal of the offer, a BYE
// that ends the dialog before the answer, and a 2xx whose ACK never comes.
func TestDialogPeerReInviteCommitsOrAbortsTheMedia(t *testing.T) {
	offer := func(t *testing.T, d *DialogServerSession) *sip.Request {
		req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
		req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		req.SetBody(peerSDP())
		return req
	}

	t.Run("acknowledged", func(t *testing.T) {
		d, med, _ := newRecordedMediaCall(t)
		_, committed, _ := med.seen()
		req := offer(t, d)

		committing := make(chan struct{})
		release := make(chan struct{})
		var first, released sync.Once
		releaseCommit := func() { released.Do(func() { close(release) }) }
		// A commit still held when the test fails is let go.
		t.Cleanup(releaseCommit)
		med.hook(nil, func() {
			first.Do(func() { close(committing) })
			<-release
		})

		tx := newTransmissionsTx()
		handled := make(chan error, 1)
		go func() { handled <- d.handleReInvite(t.Context(), req, tx) }()
		require.Equal(t, sip.StatusOK, nextTransmission(t, tx, 5*time.Second))

		ackRead := make(chan error, 1)
		go func() { ackRead <- d.ReadAck(t.Context(), newReInviteAck(req, nil), newByeServerTx()) }()
		select {
		case <-committing:
		case <-time.After(5 * time.Second):
			t.Fatal("the ACK did not have the media commit the update")
		}
		// A copy of the ACK, sent for a retransmission of the 2xx, arrives
		// while the first is handled: it has the media commit nothing.
		copyRead := make(chan error, 1)
		go func() { copyRead <- d.ReadAck(t.Context(), newReInviteAck(req, nil), newByeServerTx()) }()
		select {
		case err := <-copyRead:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("a copy of the ACK had the media commit again")
		}
		select {
		case err := <-handled:
			t.Fatalf("the re-INVITE ended before the media had committed its update: %v", err)
		default:
		}

		releaseCommit()
		require.NoError(t, <-ackRead)
		select {
		case err := <-handled:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the re-INVITE did not end once its ACK was handled")
		}
		_, finalized, aborted := med.seen()
		assert.Equal(t, committed+1, finalized, "the update is committed once")
		assert.Zero(t, aborted, "a committed update was dropped")
	})

	t.Run("refused", func(t *testing.T) {
		d, med, _ := newRecordedMediaCall(t)
		errRefused := errors.New("offer refused")
		med.refuseOffers(errRefused)

		tx := newByeServerTx()
		require.ErrorIs(t, d.handleReInvite(t.Context(), offer(t, d), tx), errRefused)
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusBadRequest, tx.responses[0].StatusCode)
		_, _, aborted := med.seen()
		assert.Equal(t, 1, aborted)
	})

	t.Run("media_closed", func(t *testing.T) {
		d, med, _ := newRecordedMediaCall(t)
		med.refuseOffers(fmt.Errorf("offer: %w", errMediaClosed))

		tx := newByeServerTx()
		require.NoError(t, d.handleReInvite(t.Context(), offer(t, d), tx))
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusRequestTerminated, tx.responses[0].StatusCode)
		_, _, aborted := med.seen()
		assert.Equal(t, 1, aborted)
	})

	t.Run("media_update_in_progress", func(t *testing.T) {
		d, med, _ := newRecordedMediaCall(t)
		med.refuseOffers(fmt.Errorf("offer: %w", errMediaUpdateInProgress))

		tx := newByeServerTx()
		require.NoError(t, d.handleReInvite(t.Context(), offer(t, d), tx))
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusRequestPending, tx.responses[0].StatusCode)
		_, _, aborted := med.seen()
		assert.Equal(t, 1, aborted)
	})

	t.Run("commit_failed", func(t *testing.T) {
		d, med, sent := newRecordedMediaCall(t)
		errNotKeyed := errors.New("media not keyed")
		med.finalizeErr = errNotKeyed
		req := offer(t, d)

		tx := newTransmissionsTx()
		handled := make(chan error, 1)
		go func() { handled <- d.handleReInvite(t.Context(), req, tx) }()
		require.Equal(t, sip.StatusOK, nextTransmission(t, tx, 5*time.Second))

		// The answer is out, so the peer has moved to a session the media
		// could not complete: the reading of the ACK says so and ends the
		// call.
		err := d.ReadAck(t.Context(), newReInviteAck(req, nil), newByeServerTx())
		require.ErrorIs(t, err, errMediaUpdateAfterAnswer)
		require.ErrorIs(t, err, errNotKeyed)
		select {
		case err := <-handled:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the re-INVITE did not end once its ACK was handled")
		}
		_, _, aborted := med.seen()
		assert.Equal(t, 1, aborted)
		assert.Contains(t, sent.sent(), sip.BYE, "the call was not ended with a BYE")
	})

	t.Run("dialog_ended_during_commit", func(t *testing.T) {
		d, med, sent := newRecordedMediaCall(t)
		req := offer(t, d)
		// The peer's BYE is read while the media commits the update, which
		// the end of the dialog then cuts short.
		var byeErr error
		med.hook(nil, func() {
			byeErr = d.ReadBye(t.Context(), newBye(t, d, req.CSeq().SeqNo+1), newByeServerTx())
		})
		med.finalizeErr = errors.New("commit cut short")

		tx := newTransmissionsTx()
		handled := make(chan error, 1)
		go func() { handled <- d.handleReInvite(t.Context(), req, tx) }()
		require.Equal(t, sip.StatusOK, nextTransmission(t, tx, 5*time.Second))

		require.NoError(t, d.ReadAck(t.Context(), newReInviteAck(req, nil), newByeServerTx()), "the end of the dialog is no failure of the update")
		require.NoError(t, byeErr)
		select {
		case err := <-handled:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("the re-INVITE did not end once its ACK was handled")
		}
		_, _, aborted := med.seen()
		assert.Equal(t, 1, aborted)
		assert.NotContains(t, sent.sent(), sip.BYE, "a call the peer had ended was ended again")
	})

	t.Run("bye_before_the_answer", func(t *testing.T) {
		d, med, _ := newRecordedMediaCall(t)
		req := offer(t, d)
		var byeErr error
		med.hook(func() {
			byeErr = d.ReadBye(t.Context(), newBye(t, d, req.CSeq().SeqNo+1), newByeServerTx())
		}, nil)

		tx := newByeServerTx()
		require.NoError(t, d.handleReInvite(t.Context(), req, tx))
		require.NoError(t, byeErr)
		require.Len(t, tx.responses, 1)
		assert.Equal(t, sip.StatusRequestTerminated, tx.responses[0].StatusCode)
		_, _, aborted := med.seen()
		assert.Equal(t, 1, aborted)
	})

	t.Run("no_ack", func(t *testing.T) {
		d, med, sent := newRecordedMediaCall(t)
		_, committed, _ := med.seen()
		d.ackTimers = ackTimers{t1: 10 * time.Millisecond, t2: 40 * time.Millisecond, timeout: 200 * time.Millisecond}

		require.ErrorIs(t, d.handleReInvite(t.Context(), offer(t, d), newTransmissionsTx()), errReInviteAckTimeout)
		_, finalized, aborted := med.seen()
		assert.Equal(t, committed, finalized, "an update whose 2xx was never acknowledged was committed")
		assert.Equal(t, 1, aborted)
		assert.Contains(t, sent.sent(), sip.BYE, "the call was not ended with a BYE")
	})
}
