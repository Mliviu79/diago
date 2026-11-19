// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingMedia is the media of a call as a session sees it: it records what
// the session hands it and asks of it, and fails where it is told to.
type recordingMedia struct {
	// finalizeErr, when set, is what onFinalize returns, as media that cannot
	// be keyed.
	finalizeErr error

	mu        sync.Mutex
	answers   []string
	finalized int
	aborted   int
}

func (m *recordingMedia) onRemoteSDP(_ context.Context, remoteSDP []byte, offered bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if offered {
		m.answers = append(m.answers, string(remoteSDP))
	}
	return nil
}

func (m *recordingMedia) onLocalSDP(context.Context, bool, string) ([]byte, error) {
	return peerSDP(), nil
}

func (m *recordingMedia) onFinalize(context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.finalized++
	return m.finalizeErr
}

func (m *recordingMedia) abort() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.aborted++
}

// seen returns the answers handed over, and how often the media was asked to
// commit and to abort.
func (m *recordingMedia) seen() (answers []string, finalized, aborted int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.answers...), m.finalized, m.aborted
}

// register makes m the media of d.
func (m *recordingMedia) register(d *DialogServerSession) {
	d.dialogCallbacks.mu.Lock()
	defer d.dialogCallbacks.mu.Unlock()
	d.mediaHanshaker = m
	d.onMediaFailure = m.abort
}

// TestDialogServerAckMediaNotKeyedEndsTheCall pins what happens when the media
// of an answered call cannot be completed at the ACK, as when its keying
// fails. The ACK cannot be refused, so it is read, and the call is ended with
// one BYE. The answer waiting for that ACK sends the BYE and returns the
// reason, so the application does not take the call for answered; ReadAck
// returns the reason too.
func TestDialogServerAckMediaNotKeyedEndsTheCall(t *testing.T) {
	errNotKeyed := errors.New("media not keyed")
	ua, sent := newRecordingDialogUA(t)
	inviteTx := &sentServerTx{byeServerTx: newByeServerTx(), sent: make(chan struct{})}
	d := newTestDialogOverUA(t, inviteTx, ua)
	med := &recordingMedia{finalizeErr: errNotKeyed}
	med.register(d)
	answered := sendOK(t, d, inviteTx)

	ack := sip.NewRequest(sip.ACK, d.InviteRequest.Contact().Address)
	ack.AppendHeader(&sip.CSeqHeader{SeqNo: d.InviteRequest.CSeq().SeqNo, MethodName: sip.ACK})
	assert.ErrorIs(t, d.ReadAck(ack, newByeServerTx()), errNotKeyed)

	select {
	case err := <-answered:
		assert.ErrorIs(t, err, errNotKeyed, "the answer reports why the call has no media")
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not return")
	}
	assert.Equal(t, []sip.RequestMethod{sip.BYE}, sent.sent(), "the call is ended with one BYE")
	assert.Equal(t, sip.DialogStateEnded, d.LoadState())
	_, _, aborted := med.seen()
	assert.Equal(t, 1, aborted, "the media is told the exchange did not complete")
}

// TestDialogServerAckCommitsTheMedia pins which ACK the media is completed
// with. The ACK confirming the dialog commits the media the INVITE's exchange
// set up. A copy of that ACK, which the peer sends for each retransmission of
// the 2xx it receives, commits nothing more and hands over no answer, even
// when it carries a session description. The ACK to the 2xx to a re-INVITE of
// the peer's commits the update that re-INVITE staged.
func TestDialogServerAckCommitsTheMedia(t *testing.T) {
	d, inviteTx := newAnswerTestDialog(t)
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
	answers, finalized, aborted := med.seen()
	assert.Empty(t, answers)
	assert.Equal(t, 1, finalized, "the ACK confirming the dialog commits the media")
	assert.Zero(t, aborted)

	require.NoError(t, d.ReadAck(newLateAck(d, peerSDP()), newByeServerTx()))
	answers, finalized, _ = med.seen()
	assert.Empty(t, answers, "a copy of the ACK answers nothing")
	assert.Equal(t, 1, finalized, "a copy of the ACK commits nothing")

	reInvite := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
	reInvite.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	reInvite.SetBody(peerSDP())
	tx := newRespondedServerTx()
	require.NoError(t, d.handleReInvite(t.Context(), reInvite, tx))
	select {
	case res := <-tx.responded:
		require.Equal(t, sip.StatusOK, res.StatusCode)
	default:
		t.Fatal("the re-INVITE was not answered")
	}
	require.NoError(t, d.ReadAck(newReInviteAck(reInvite, nil), newByeServerTx()))
	_, finalized, _ = med.seen()
	assert.Equal(t, 2, finalized, "the ACK to the re-INVITE's 2xx commits its update")
	assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
}

// TestAckSessionDescription pins what in an ACK counts as a session
// description: a body declared as one, and a body with no declaration at all.
// A body declared as something else is not one, whatever it holds.
func TestAckSessionDescription(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        []byte
		want        []byte
	}{
		{name: "no_body"},
		{name: "declared_sdp", contentType: "application/sdp", body: peerSDP(), want: peerSDP()},
		{name: "undeclared", body: peerSDP(), want: peerSDP()},
		{name: "declared_otherwise", contentType: "application/isup", body: peerSDP()},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ack := sip.NewRequest(sip.ACK, sip.Uri{User: "alice", Host: "127.0.0.1"})
			if tt.contentType != "" {
				ack.AppendHeader(sip.NewHeader("Content-Type", tt.contentType))
			}
			if tt.body != nil {
				ack.SetBody(tt.body)
			}
			assert.Equal(t, tt.want, ackSessionDescription(ack))
		})
	}
}
