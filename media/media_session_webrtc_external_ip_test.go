// SPDX-License-Identifier: MPL-2.0

package media

import (
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireConnectionAddress requires the c= line of body to give external, or
// the address of one of its candidates when external is nil, and the o= line
// and every candidate to keep an address that was gathered.
func requireConnectionAddress(t *testing.T, what string, body []byte, external net.IP) {
	t.Helper()
	sd := sdp.SessionDescription{}
	require.NoError(t, sdp.Unmarshal(body, &sd), what)
	connInfo, err := sd.ConnectionInformation()
	require.NoError(t, err, what)
	require.Len(t, sdpLines(body, "c="), 1, "%s:\n%s", what, body)

	candidates := sdpCandidates(t, body)
	require.NotEmpty(t, candidates, what)
	gathered := make([]string, 0, len(candidates))
	for _, c := range candidates {
		gathered = append(gathered, c.Address())
	}
	if external == nil {
		assert.Contains(t, gathered, connInfo.IP.String(), "%s: c= gives the address of a candidate", what)
		return
	}
	assert.True(t, external.Equal(connInfo.IP), "%s: c= gives %s, want %s", what, connInfo.IP, external)
	assert.NotContains(t, gathered, external.String(), "%s: the candidates keep the gathered addresses", what)

	origin := sdpLines(body, "o=")
	require.Len(t, origin, 1, what)
	originAddr := origin[0][strings.LastIndex(origin[0], " ")+1:]
	assert.Contains(t, gathered, originAddr, "%s: the o= line keeps a gathered address: %s", what, origin[0])
}

// TestMediaSessionWebrtcExternalIP pins that the c= line of a WebRTC session
// gives ExternalIP when one is configured, in an offer, in an answer, in a
// re-offer on the same ICE generation and in the offer of the generation a
// Fork starts. Every re-negotiation builds its SDP anew, so one that loses
// ExternalIP advertises the internal bind address in the middle of a call.
// Only the c= line takes it: the candidates are not rewritten.
func TestMediaSessionWebrtcExternalIP(t *testing.T) {
	ctx := t.Context()
	external := net.IPv4(1, 1, 1, 1)
	newSession := func(externalIP net.IP) *MediaSessionWebrtc {
		t.Helper()
		conf := iceLiteTestConfig(false, testdata.ServerCertificate())
		conf.ICE.ExternalIP = externalIP
		s := &MediaSessionWebrtc{Codecs: []Codec{CodecAudioUlaw}, Mode: sdp.ModeSendrecv}
		require.NoError(t, s.Init(ctx, conf))
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	offerer := newICELiteTestSession(t, false, testdata.ClientCertificate())
	answerer := newSession(external)

	offer, answer := negotiateWebrtc(t, offerer, answerer, nil)
	requireConnectionAddress(t, "offer without ExternalIP", offer, nil)
	requireConnectionAddress(t, "answer", answer, external)

	requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE and DTLS negotiation")
	reOffer, err := answerer.ReOffer("")
	require.NoError(t, err)
	requireConnectionAddress(t, "re-offer", reOffer, external)

	fork, err := answerer.Fork(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { _ = fork.Close() })
	forkOffer, err := fork.LocalSDP(ctx, false)
	require.NoError(t, err)
	requireConnectionAddress(t, "offer of the fork", forkOffer, external)

	firstOffer, err := newSession(external).LocalSDP(ctx, false)
	require.NoError(t, err)
	requireConnectionAddress(t, "offer", firstOffer, external)
	require.Equal(t, []string{"c=IN IP4 1.1.1.1"}, sdpLines(firstOffer, "c="))

	// The address family of the line is ExternalIP's own, whatever the
	// family of the candidates.
	v6Offer, err := newSession(net.ParseIP("2001:db8::1")).LocalSDP(ctx, false)
	require.NoError(t, err)
	require.Equal(t, []string{"c=IN IP6 2001:db8::1"}, sdpLines(v6Offer, "c="))
}
