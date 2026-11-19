// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"
	"fmt"
	"testing"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// warnings returns the values of the Warning header fields of res.
func warnings(res *sip.Response) []string {
	var values []string
	for _, h := range res.GetHeaders("Warning") {
		values = append(values, h.Value())
	}
	return values
}

// TestDialogPeerReInviteOfferNotAcceptable is a re-INVITE of the peer's whose
// offer the media of the call reads and cannot take, on either side. RFC 3261
// section 14.2: "If the new session description is not acceptable, the UAS can
// reject it by returning a 488 (Not Acceptable Here) response for the
// re-INVITE. This response SHOULD include a Warning header field." The session
// stays as it was (RFC 3264 section 4: "the session reverts to the state prior
// to the offer"). A body that is no session description at all could not be
// understood, which is what 400 says (RFC 3261 section 21.4.1).
func TestDialogPeerReInviteOfferNotAcceptable(t *testing.T) {
	// us is where the peer addresses its re-INVITE: the Warning names it.
	us := sip.Uri{User: "dg", Host: "192.0.2.10", Port: 5080}
	const sdpHead = "v=0\r\no=- 3976000000 3976000001 IN IP4 127.0.0.1\r\ns=-\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n"

	sides := []struct {
		name  string
		setup func(t *testing.T) (med *DialogMedia, req *sip.Request, handleReInvite func(*sip.Request, sip.ServerTransaction) error)
	}{
		{
			name: "inbound",
			setup: func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error) {
				d, med := newAnsweredTestServerCall(t)
				return med, newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1),
					func(req *sip.Request, tx sip.ServerTransaction) error { return d.handleReInvite(t.Context(), req, tx) }
			},
		},
		{
			name: "outbound",
			setup: func(t *testing.T) (*DialogMedia, *sip.Request, func(*sip.Request, sip.ServerTransaction) error) {
				d, med, _ := newAnsweredTestClientCall(t)
				return med, newPeerReInvite(d, d.InviteRequest.CSeq().SeqNo+1),
					func(req *sip.Request, tx sip.ServerTransaction) error { return d.handleReInvite(t.Context(), req, tx) }
			},
		},
	}
	offers := []struct {
		name        string
		body        string
		wantErr     error
		wantStatus  int
		wantWarning []string
	}{
		{
			name:        "no_common_codec",
			body:        sdpHead + "m=audio 34455 RTP/AVP 18\r\na=rtpmap:18 G729/8000\r\n",
			wantErr:     media.ErrNoCommonCodec,
			wantStatus:  sip.StatusNotAcceptableHere,
			wantWarning: []string{`305 192.0.2.10:5080 "Incompatible media format"`},
		},
		{
			name:        "no_audio",
			body:        sdpHead + "m=video 34455 RTP/AVP 96\r\na=rtpmap:96 VP8/90000\r\n",
			wantErr:     media.ErrNoCommonMedia,
			wantStatus:  sip.StatusNotAcceptableHere,
			wantWarning: []string{`399 192.0.2.10:5080 "No usable audio media description"`},
		},
		{
			name:        "audio_port_zero",
			body:        sdpHead + "m=audio 0 RTP/AVP 0\r\na=rtpmap:0 PCMU/8000\r\n",
			wantErr:     media.ErrNoCommonMedia,
			wantStatus:  sip.StatusNotAcceptableHere,
			wantWarning: []string{`399 192.0.2.10:5080 "No usable audio media description"`},
		},
		{
			name:       "not_a_session_description",
			body:       "this is no session description\r\n",
			wantStatus: sip.StatusBadRequest,
		},
	}

	for _, side := range sides {
		t.Run(side.name, func(t *testing.T) {
			for _, offer := range offers {
				t.Run(offer.name, func(t *testing.T) {
					med, req, handleReInvite := side.setup(t)
					sess := med.MediaSession()
					req.Recipient = us
					req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
					req.SetBody([]byte(offer.body))

					tx := newByeServerTx()
					err := handleReInvite(req, tx)
					require.Error(t, err)
					if offer.wantErr != nil {
						require.ErrorIs(t, err, offer.wantErr)
					}
					require.Len(t, tx.responses, 1)
					res := tx.responses[0]
					assert.Equal(t, offer.wantStatus, res.StatusCode)
					assert.Equal(t, offer.wantWarning, warnings(res))
					assert.Empty(t, res.Body(), "a refusal carries no session description")

					assert.True(t, sess == med.MediaSession(), "a refused offer replaced the media session")
					med.mu.Lock()
					pending, staged := med.pendingMediaSession, med.stagedMediaSession
					med.mu.Unlock()
					assert.Nil(t, pending, "a refused offer left a fork pending")
					assert.Nil(t, staged, "a refused offer left a fork staged")
				})
			}
		})
	}
}

// TestRefusePeerReInviteStatus pins the status each class of refusal by the
// media gives a re-INVITE of the peer's, whichever media the call has: 488
// with a Warning for a session description that is not acceptable, 487 for
// media that is closed, 491 for media still completing an earlier update, and
// 400 for anything else.
func TestRefusePeerReInviteStatus(t *testing.T) {
	errOther := errors.New("offer refused")
	cases := []struct {
		name        string
		mediaErr    error
		wantStatus  int
		wantWarning []string
		wantErr     error
	}{
		{
			name:        "no_common_codec",
			mediaErr:    fmt.Errorf("sdp update: %w", media.ErrNoCommonCodec),
			wantStatus:  sip.StatusNotAcceptableHere,
			wantWarning: []string{`305 127.0.0.2:5060 "Incompatible media format"`},
			wantErr:     media.ErrNoCommonCodec,
		},
		{
			name:        "no_common_media",
			mediaErr:    fmt.Errorf("sdp update: %w", media.ErrNoCommonMedia),
			wantStatus:  sip.StatusNotAcceptableHere,
			wantWarning: []string{`399 127.0.0.2:5060 "No usable audio media description"`},
			wantErr:     media.ErrNoCommonMedia,
		},
		{
			name:        "no_common_crypto",
			mediaErr:    fmt.Errorf("sdp update: %w", media.ErrNoCommonCrypto),
			wantStatus:  sip.StatusNotAcceptableHere,
			wantWarning: []string{`399 127.0.0.2:5060 "No common SRTP keying"`},
			wantErr:     media.ErrNoCommonCrypto,
		},
		{
			name:        "ice_restart_required",
			mediaErr:    fmt.Errorf("remote DTLS role changed: %w", media.ErrWebRTCICERestartRequired),
			wantStatus:  sip.StatusNotAcceptableHere,
			wantWarning: []string{`399 127.0.0.2:5060 "A new DTLS association needs an ICE restart"`},
			wantErr:     media.ErrWebRTCICERestartRequired,
		},
		{
			name:       "media_closed",
			mediaErr:   fmt.Errorf("offer: %w", errMediaClosed),
			wantStatus: sip.StatusRequestTerminated,
		},
		{
			name:       "media_update_in_progress",
			mediaErr:   fmt.Errorf("offer: %w", errMediaUpdateInProgress),
			wantStatus: sip.StatusRequestPending,
		},
		{
			name:       "other",
			mediaErr:   errOther,
			wantStatus: sip.StatusBadRequest,
			wantErr:    errOther,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, med, _ := newRecordedMediaCall(t)
			med.refuseOffers(tc.mediaErr)
			req := newInDialogReInvite(t, d, d.InviteRequest.CSeq().SeqNo+1)
			req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
			req.SetBody(peerSDP())

			tx := newByeServerTx()
			err := d.handleReInvite(t.Context(), req, tx)
			if tc.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, tc.wantErr)
			}
			require.Len(t, tx.responses, 1)
			assert.Equal(t, tc.wantStatus, tx.responses[0].StatusCode)
			assert.Equal(t, tc.wantWarning, warnings(tx.responses[0]))
			_, _, aborted := med.seen()
			assert.Equal(t, 1, aborted, "the media was not told to drop the offer")
		})
	}
}
