// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// finalResponseTx is a byeServerTx that hands the final responses it is given
// to a channel, and drops the provisional ones.
type finalResponseTx struct {
	*byeServerTx
	finals chan *sip.Response
}

func (tx *finalResponseTx) Respond(_ context.Context, res *sip.Response) error {
	if res.IsProvisional() {
		return nil
	}
	select {
	case tx.finals <- res:
	default:
	}
	return nil
}

// TestDialogServerAnswersRefuseASessionIntervalBelowMinSE pins that every way
// of answering an INVITE that carries an offer negotiates the session timer
// before it answers: a Session-Expires below our Min-SE is refused 422 with
// Min-SE (RFC 4028 section 6), no 2xx is sent, no session timer starts, and
// the answer returns errSessionIntervalTooSmall.
func TestDialogServerAnswersRefuseASessionIntervalBelowMinSE(t *testing.T) {
	answers := []struct {
		name   string
		answer func(t *testing.T, d *DialogServerSession) error
	}{
		{name: "Answer", answer: func(_ *testing.T, d *DialogServerSession) error {
			_, err := d.Answer(AnswerOptions{})
			return err
		}},
		{name: "AnswerContext", answer: func(t *testing.T, d *DialogServerSession) error {
			_, err := d.AnswerContext(t.Context())
			return err
		}},
		{name: "AnswerOptions", answer: func(t *testing.T, d *DialogServerSession) error {
			_, err := d.AnswerOptions(t.Context(), AnswerOptions{})
			return err
		}},
		{name: "AnswerEarlyMedia", answer: func(t *testing.T, d *DialogServerSession) error {
			med, err := d.ProgressMedia(t.Context(), ProgressMediaOptions{})
			if err != nil {
				return err
			}
			return d.AnswerEarlyMedia(med, AnswerOptions{})
		}},
		{name: "AnswerWebrtc", answer: func(_ *testing.T, d *DialogServerSession) error {
			_, err := d.AnswerWebrtc(AnswerWebrtcOptions{})
			return err
		}},
	}
	for _, tc := range answers {
		t.Run(tc.name, func(t *testing.T) {
			peer := newMediaSessionForTest(t)
			inviteTx := &finalResponseTx{byeServerTx: newByeServerTx(), finals: make(chan *sip.Response, 4)}
			d, _ := newRecordingServerDialog(t, inviteTx, peer.LocalSDP(false), MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw},
				bindIP: net.IPv4(127, 0, 0, 1),
			})
			d.sessionTimers = SessionTimerPolicy{Enabled: true, PreferUASRefresher: true}
			d.InviteRequest.AppendHeader(sip.NewHeader("Supported", "timer"))
			d.InviteRequest.AppendHeader(sip.NewHeader("Session-Expires", "60"))

			answered := make(chan error, 1)
			go func() { answered <- tc.answer(t, d) }()
			select {
			case res := <-inviteTx.finals:
				require.Equal(t, statusSessionIntervalTooSmall, res.StatusCode, "the final response is the 422, no 2xx")
				assert.Equal(t, "90", headerValue(res, "Min-SE"), "the 422 carries our Min-SE")
			case err := <-answered:
				require.FailNow(t, "the answer returned before any final response", "error: %v", err)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "no final response was sent")
			}

			// The INVITE transaction ends, as it does once the peer
			// acknowledges the 422.
			close(inviteTx.done)
			select {
			case err := <-answered:
				assert.ErrorIs(t, err, errSessionIntervalTooSmall)
			case <-time.After(5 * time.Second):
				require.FailNow(t, "the answer did not return after the 422")
			}
			assert.Nil(t, d.timer.Load(), "a refused INVITE started a session timer")
			assert.Empty(t, inviteTx.finals, "another final response followed the 422")
		})
	}
}
