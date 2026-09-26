// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package media

import (
	"crypto/tls"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newICELiteTestSession builds one side of an ICE + DTLS-SRTP session, with a
// lite agent when lite is set, and closes it when the test ends.
func newICELiteTestSession(t *testing.T, ip net.IP, lite bool, cert tls.Certificate) *MediaSession {
	t.Helper()
	s := &MediaSession{
		Codecs:    []Codec{CodecAudioUlaw},
		Mode:      sdp.ModeSendrecv,
		SecureRTP: SecureRTPModeDTLS,
		ICEConf:   &ICEConfig{Lite: lite},
		DTLSConf: DTLSConfig{
			Certificates:     []tls.Certificate{cert},
			ServerClientAuth: ServerClientAuthRequireCert,
		},
		Laddr: net.UDPAddr{IP: ip, Port: 0},
	}
	require.NoError(t, s.Init())
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// sdpSessionLevel returns the lines of body before its first m= line.
func sdpSessionLevel(body []byte) []string {
	var lines []string
	for _, l := range strings.Split(string(body), "\r\n") {
		if strings.HasPrefix(l, "m=") {
			break
		}
		lines = append(lines, l)
	}
	return lines
}

// TestICELite negotiates an ICE lite agent against a full one, in both offer
// directions, and two lite agents, through connectivity checks, the DTLS
// handshake and SRTP. A lite agent gathers host candidates only and says so
// with a session level a=ice-lite (RFC 8839 section 4.2.1.4, section 5.3),
// which a full agent never writes. RFC 8445 section 6.1.1 makes the full agent
// controlling against a lite one whichever side offered, since the lite agent
// never sends checks, and the offerer controlling otherwise. Before, a lite
// agent could not even be built: every candidate type was gathered, which the
// ICE stack refuses for a lite agent.
func TestICELite(t *testing.T) {
	ip := iceTestIP(t)

	cases := []struct {
		name                   string
		offererLite            bool
		answererLite           bool
		wantOffererControlling bool
	}{
		{name: "lite answerer", answererLite: true, wantOffererControlling: true},
		{name: "lite offerer", offererLite: true, wantOffererControlling: false},
		{name: "both lite", offererLite: true, answererLite: true, wantOffererControlling: true},
		{name: "both full", wantOffererControlling: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			offerer := newICELiteTestSession(t, ip, tc.offererLite, testdata.ClientCertificate())
			answerer := newICELiteTestSession(t, ip, tc.answererLite, testdata.ServerCertificate())

			offer := offerer.LocalSDP()
			assert.Equal(t, tc.offererLite, slices.Contains(sdpSessionLevel(offer), "a=ice-lite"), "offer:\n%s", offer)
			require.Equal(t, tc.offererLite, strings.Contains(string(offer), "a=ice-lite"), "a=ice-lite is session level only")
			require.NoError(t, answerer.RemoteSDP(offer))

			answer := answerer.LocalSDP()
			assert.Equal(t, tc.answererLite, slices.Contains(sdpSessionLevel(answer), "a=ice-lite"), "answer:\n%s", answer)
			require.Equal(t, tc.answererLite, strings.Contains(string(answer), "a=ice-lite"), "a=ice-lite is session level only")
			offerer.RemoteSDPIsAnswer = true
			require.NoError(t, offerer.RemoteSDP(answer))

			require.Equal(t, tc.wantOffererControlling, offerer.iceControlling(), "offerer role")
			require.Equal(t, !tc.wantOffererControlling, answerer.iceControlling(), "answerer role")

			errCh := make(chan error, 2)
			go func() { errCh <- answerer.Finalize() }()
			go func() { errCh <- offerer.Finalize() }()
			for i := 0; i < 2; i++ {
				select {
				case err := <-errCh:
					require.NoError(t, err, "ICE + DTLS negotiation failed")
				case <-time.After(40 * time.Second):
					t.Fatal("ICE + DTLS negotiation did not complete")
				}
			}

			requireMediaDelivered(t, offerer, answerer, 71)
			requireMediaDelivered(t, answerer, offerer, 72)
		})
	}
}

// TestICELiteGathersHostCandidatesOnly pins that a lite agent offers host
// candidates only, as RFC 8445 section 2.5 has it, and refuses STUN servers
// with an error that says so instead of the ICE stack's refusal of them.
func TestICELiteGathersHostCandidatesOnly(t *testing.T) {
	ip := iceTestIP(t)
	s := newICELiteTestSession(t, ip, true, testdata.ServerCertificate())
	for _, l := range strings.Split(string(s.LocalSDP()), "\r\n") {
		if strings.HasPrefix(l, "a=candidate:") {
			assert.Contains(t, l, " typ host", "a lite agent offers host candidates only")
		}
	}

	_, err := NewICEAgent(ICEConfig{Lite: true, STUNServers: []string{"stun:stun.example.com:3478"}})
	require.Error(t, err)
	require.Contains(t, err.Error(), "lite")
}
