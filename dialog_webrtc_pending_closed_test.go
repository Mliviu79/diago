// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"testing"

	"github.com/emiago/diago/media"
	"github.com/stretchr/testify/require"
)

// TestDialogWebrtcCloseBeforePendingStore pins what becomes of a transport an
// offer/answer exchange set up when the media is closed after the exchange
// found it open and before the transport is stored: the exchange is refused
// with errMediaClosed, a transport made for it has its sockets closed, and
// nothing is left pending. A peer's re-INVITE racing its BYE left one ICE and
// DTLS transport open for good this way.
func TestDialogWebrtcCloseBeforePendingStore(t *testing.T) {
	for _, tc := range []struct {
		name string
		// newTransport says the exchange makes a transport of its own.
		newTransport bool
		exchange     func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer) error
	}{
		{
			name:         "own_ice_restart_offer",
			newTransport: true,
			exchange: func(t *testing.T, d *DialogWebrtc, _ *webrtcHandlePeer) error {
				_, err := d.onLocalSDP(t.Context(), false, "")
				return err
			},
		},
		{
			name:         "peer_ice_restart_offer",
			newTransport: true,
			exchange: func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer) error {
				fork, err := p.sess.Fork(t.Context())
				require.NoError(t, err)
				t.Cleanup(func() { require.NoError(t, fork.Close()) })
				offer, err := fork.LocalSDP(t.Context(), false)
				require.NoError(t, err)
				return d.onRemoteSDP(t.Context(), offer, false)
			},
		},
		{
			name: "peer_offer_in_place",
			exchange: func(t *testing.T, d *DialogWebrtc, p *webrtcHandlePeer) error {
				offer, err := p.sess.ReOffer("")
				require.NoError(t, err)
				return d.onRemoteSDP(t.Context(), offer, false)
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, p := newWebrtcHandleCall(t, []media.Codec{media.CodecAudioUlaw})
			current := d.MediaSession()
			var stored *media.MediaSessionWebrtc
			var storedSDP []byte
			d.beforePendingStore = func(sess *media.MediaSessionWebrtc) {
				stored = sess
				var err error
				storedSDP, err = sess.LocalSDP(t.Context(), false)
				require.NoError(t, err)
				require.NoError(t, d.Close())
			}

			require.ErrorIs(t, tc.exchange(t, d, p), errMediaClosed)

			require.NotNil(t, stored, "the exchange stored no transport")
			require.Equal(t, tc.newTransport, stored != current)
			d.mu.Lock()
			pending := d.pendingMediaSession
			d.mu.Unlock()
			require.Nil(t, pending, "a transport was left pending on closed media")
			requirePortReleased(t, storedSDP)
		})
	}
}
