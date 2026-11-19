// SPDX-License-Identifier: MPL-2.0

package media

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/testdata"
	"github.com/pion/ice/v4"
	"github.com/stretchr/testify/require"
)

// unreadableCandidates are a=candidate values the ICE stack cannot read, by
// what is wrong with them. The first is the case RFC 8839 section 5.1 names:
// "An agent processing remote candidates MUST ignore "candidate" lines that
// include candidates with FQDNs".
var unreadableCandidates = []struct {
	name  string
	value string
}{
	{name: "FQDN_address", value: "1 1 udp 2130706431 host.example.com 5000 typ host"},
	{name: "unknown_candidate_type", value: "1 1 udp 2130706431 192.0.2.1 5000 typ future"},
	{name: "port_that_is_no_number", value: "1 1 udp 2130706431 192.0.2.1 port typ host"},
	{name: "cut_short", value: "1 1 udp 2130706431"},
	{name: "no_fields", value: "candidate"},
}

// withCandidateLines returns body with its a=candidate lines replaced by
// lines, at the place of the first one.
func withCandidateLines(t *testing.T, body []byte, lines ...string) []byte {
	t.Helper()
	var out []string
	replaced := false
	for l := range strings.SplitSeq(string(body), "\r\n") {
		if !strings.HasPrefix(l, "a=candidate:") {
			out = append(out, l)
			continue
		}
		if !replaced {
			out = append(out, lines...)
			replaced = true
		}
	}
	require.True(t, replaced, "the SDP has no a=candidate line:\n%s", body)
	return []byte(strings.Join(out, "\r\n"))
}

// withCandidatesAfter returns body with lines written ahead of its own
// a=candidate lines, which are kept.
func withCandidatesAfter(t *testing.T, body []byte, lines ...string) []byte {
	t.Helper()
	return withCandidateLines(t, body, append(lines, sdpLines(body, "a=candidate:")...)...)
}

// unreadableCandidateLines returns one a=candidate line for each of
// unreadableCandidates, and requires the ICE stack to refuse every one.
func unreadableCandidateLines(t *testing.T) []string {
	t.Helper()
	lines := make([]string, 0, len(unreadableCandidates))
	for _, c := range unreadableCandidates {
		_, err := ice.UnmarshalCandidate(c.value)
		require.Error(t, err, "the ICE stack reads %q, so it is no case of this test", c.value)
		lines = append(lines, "a=candidate:"+c.value)
	}
	return lines
}

// silentCandidateLine returns an a=candidate line for a loopback UDP port
// that is bound for the length of the test and never answers a connectivity
// check.
func silentCandidateLine(t *testing.T) string {
	t.Helper()
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	require.NoError(t, err)
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close the silent socket: %v", err)
		}
	})
	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	require.True(t, ok, "the silent socket's address is %T", conn.LocalAddr())
	return fmt.Sprintf("a=candidate:1 1 udp 2130706431 127.0.0.1 %d typ host", addr.Port)
}

// TestWebrtcRemoteSDPSkipsUnreadableCandidate pins that one a=candidate line
// the ICE stack cannot read does not refuse the description that carries it,
// in an offer and in an answer: the line is left out and the description is
// taken with the candidates of its other lines.
func TestWebrtcRemoteSDPSkipsUnreadableCandidate(t *testing.T) {
	for _, tc := range unreadableCandidates {
		line := "a=candidate:" + tc.value

		t.Run(tc.name+"_in_offer", func(t *testing.T) {
			ctx := t.Context()
			_, err := ice.UnmarshalCandidate(tc.value)
			require.Error(t, err, "the ICE stack reads the candidate, so it is no case of this test")
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			offer, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)

			require.NoError(t, answerer.RemoteSDP(ctx, withCandidatesAfter(t, offer, line), false))
			_, err = answerer.LocalSDP(ctx, true)
			require.NoError(t, err)
		})

		t.Run(tc.name+"_in_answer", func(t *testing.T) {
			ctx := t.Context()
			_, err := ice.UnmarshalCandidate(tc.value)
			require.Error(t, err, "the ICE stack reads the candidate, so it is no case of this test")
			offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
			answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			offer, err := offerer.LocalSDP(ctx, false)
			require.NoError(t, err)
			require.NoError(t, answerer.RemoteSDP(ctx, offer, false))
			answer, err := answerer.LocalSDP(ctx, true)
			require.NoError(t, err)

			require.NoError(t, offerer.RemoteSDP(ctx, withCandidatesAfter(t, answer, line), true))
		})
	}
}

// TestWebrtcUsesCandidatesBesideUnreadableOnes pins that the candidates of a
// description are used when unreadable ones stand before them. The session
// that reads them is the only one of the two that is given the address of its
// peer: the peer is told a port nobody answers on, so the pair is found only
// through the connectivity checks the reading session sends to the candidates
// it kept, and media then runs both ways.
func TestWebrtcUsesCandidatesBesideUnreadableOnes(t *testing.T) {
	t.Run("in_offer", func(t *testing.T) {
		ctx := t.Context()
		offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
		offer, err := offerer.LocalSDP(ctx, false)
		require.NoError(t, err)

		require.NoError(t, answerer.RemoteSDP(ctx, withCandidatesAfter(t, offer, unreadableCandidateLines(t)...), false))
		answer, err := answerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		require.NoError(t, offerer.RemoteSDP(ctx, withCandidateLines(t, answer, silentCandidateLine(t)), true))

		requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE and DTLS negotiation")
		requireWebrtcSRTPBetween(t, offerer, answerer, 1)
		requireWebrtcSRTPBetween(t, answerer, offerer, 2)
	})

	t.Run("in_answer", func(t *testing.T) {
		ctx := t.Context()
		offerer := newSetupRoleWebrtcSession(ctx, t, testdata.ClientCertificate(), nil)
		answerer := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
		offer, err := offerer.LocalSDP(ctx, false)
		require.NoError(t, err)

		require.NoError(t, answerer.RemoteSDP(ctx, withCandidateLines(t, offer, silentCandidateLine(t)), false))
		answer, err := answerer.LocalSDP(ctx, true)
		require.NoError(t, err)
		require.NoError(t, offerer.RemoteSDP(ctx, withCandidatesAfter(t, answer, unreadableCandidateLines(t)...), true))

		requireBothFinalize(t, offerer, answerer, 40*time.Second, "ICE and DTLS negotiation")
		requireWebrtcSRTPBetween(t, offerer, answerer, 1)
		requireWebrtcSRTPBetween(t, answerer, offerer, 2)
	})
}

// TestWebrtcRemoteSDPRefusesDescriptionWithNoReadableCandidate pins where the
// tolerance ends: a description none of whose candidates can be read gives the
// agent nothing to check, and is refused as one without candidates is, with an
// error that matches ErrICEConnection.
func TestWebrtcRemoteSDPRefusesDescriptionWithNoReadableCandidate(t *testing.T) {
	offer := []byte(webrtcOffer(t.Context(), t, testdata.ClientCertificate()))
	body := withCandidateLines(t, offer, unreadableCandidateLines(t)...)

	for _, offered := range []bool{false, true} {
		kind := "offer"
		if offered {
			kind = "answer"
		}
		t.Run("in_"+kind, func(t *testing.T) {
			ctx := t.Context()
			m := newSetupRoleWebrtcSession(ctx, t, testdata.ServerCertificate(), nil)
			err := m.RemoteSDP(ctx, body, offered)
			require.ErrorIs(t, err, ErrICEConnection)
		})
	}
}
