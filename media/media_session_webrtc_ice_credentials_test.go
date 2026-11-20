// SPDX-License-Identifier: MPL-2.0

package media

import (
	"testing"

	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/require"
)

// TestWebrtcRemoteICERequiresCredentials asserts a peer that signals no ICE
// credentials is refused, in an offer and in an answer, rather than run
// connectivity checks no peer can answer. The description is otherwise the
// one the control case accepts, so the credentials are what it is refused
// for.
func TestWebrtcRemoteICERequiresCredentials(t *testing.T) {
	tests := []struct {
		name    string
		without []string
	}{
		{name: "no_ufrag_or_pwd", without: []string{"ice-ufrag", "ice-pwd"}},
		{name: "ufrag_without_pwd", without: []string{"ice-pwd"}},
		{name: "pwd_without_ufrag", without: []string{"ice-ufrag"}},
	}

	for _, offered := range []bool{false, true} {
		kind := "offer"
		if offered {
			kind = "answer"
		}
		t.Run("control_in_"+kind, func(t *testing.T) {
			ctx := t.Context()
			body := []byte(webrtcOffer(ctx, t, testdata.ClientCertificate()))
			m := newCodecsWebrtcSession(ctx, t, testdata.ServerCertificate(), CodecAudioUlaw)
			require.NoError(t, m.RemoteSDP(ctx, body, offered))
		})
		for _, tc := range tests {
			t.Run(tc.name+"_in_"+kind, func(t *testing.T) {
				ctx := t.Context()
				body := []byte(webrtcOffer(ctx, t, testdata.ClientCertificate()))
				for _, name := range tc.without {
					sdpLine(t, body, "a="+name+":")
					body = sdpWithoutAttribute(body, name)
				}
				m := newCodecsWebrtcSession(ctx, t, testdata.ServerCertificate(), CodecAudioUlaw)

				require.Error(t, m.RemoteSDP(ctx, body, offered))
			})
		}
	}
}
