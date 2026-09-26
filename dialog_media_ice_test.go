// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// dialogICETestIP returns a routable IPv4 address. ICE gathers no loopback
// host candidates, so on a host without one there is no pair to establish.
func dialogICETestIP(t *testing.T) net.IP {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	require.NoError(t, err)
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.IsLoopback() {
			continue
		}
		if ip4 := ipnet.IP.To4(); ip4 != nil {
			return ip4
		}
	}
	t.Skip("no non-loopback IPv4 interface: ICE cannot gather host candidates")
	return nil
}

// establishedICEMedia is an ICE + DTLS-SRTP call that completed connectivity
// checks, the handshake and SRTP keying.
type establishedICEMedia struct {
	offerer  *media.MediaSession
	answerer *media.MediaSession
	offer    []byte
	answer   []byte
	// answererIP and answererPort are where the answerer's ICE socket is
	// bound, so a test can check the socket was released.
	answererIP   net.IP
	answererPort int
}

// newEstablishedICEMedia negotiates an offerer and an answerer over ICE and
// DTLS, and closes both when the test ends.
func newEstablishedICEMedia(t *testing.T) establishedICEMedia {
	t.Helper()
	ip := dialogICETestIP(t)

	newSess := func(role media.DTLSEndpointRole, cert tls.Certificate) *media.MediaSession {
		t.Helper()
		s := &media.MediaSession{
			Codecs:    []media.Codec{media.CodecAudioUlaw},
			Mode:      sdp.ModeSendrecv,
			SecureRTP: media.SecureRTPModeDTLS,
			ICEConf:   &media.ICEConfig{},
			DTLSRole:  role,
			DTLSConf: media.DTLSConfig{
				Certificates:     []tls.Certificate{cert},
				ServerClientAuth: media.ServerClientAuthRequireCert,
			},
			Laddr: net.UDPAddr{IP: ip, Port: 0},
		}
		require.NoError(t, s.Init())
		t.Cleanup(func() { _ = s.Close() })
		return s
	}

	e := establishedICEMedia{
		offerer:  newSess(media.DTLSEndpointRoleOfferer, testdata.ClientCertificate()),
		answerer: newSess(media.DTLSEndpointRoleAnswerer, testdata.ServerCertificate()),
	}
	e.answererIP = ip
	e.answererPort = e.answerer.Laddr.Port

	e.offer = e.offerer.LocalSDP()
	require.NoError(t, e.answerer.RemoteSDP(e.offer))
	e.answer = e.answerer.LocalSDP()
	// As applyRemoteSDP does for the answer to our own INVITE.
	e.offerer.RemoteSDPIsAnswer = true
	require.NoError(t, e.offerer.RemoteSDP(e.answer))

	errCh := make(chan error, 2)
	go func() { errCh <- e.answerer.Finalize() }()
	go func() { errCh <- e.offerer.Finalize() }()
	for i := 0; i < 2; i++ {
		select {
		case err := <-errCh:
			require.NoError(t, err, "ICE + DTLS negotiation failed")
		case <-time.After(40 * time.Second):
			t.Fatal("ICE + DTLS negotiation did not complete")
		}
	}
	return e
}

// newICEDialogMedia installs sess in a DialogMedia the way an answered call
// has it: with an RTP session whose RTCP monitor is running.
func newICEDialogMedia(t *testing.T, sess *media.MediaSession) *DialogMedia {
	t.Helper()
	d := &DialogMedia{}
	rtpSess := media.NewRTPSession(sess)
	d.initRTPSessionUnsafe(sess, rtpSess)
	require.NoError(t, rtpSess.MonitorBackground())
	t.Cleanup(func() { _ = d.Close() })
	return d
}

// handleMediaUpdateNoPanic turns a panic into an error whose text begins
// "panic: ", so a crash fails the test rather than the whole binary.
func handleMediaUpdateNoPanic(d *DialogMedia, req *sip.Request, tx sip.ServerTransaction, contact sip.Header) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return d.handleMediaUpdate(context.Background(), req, tx, contact)
}

// checkEarlyMediaNoPanic is handleMediaUpdateNoPanic for the early media path.
func checkEarlyMediaNoPanic(d *DialogMedia, body []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return d.checkEarlyMedia(body)
}

func requireNoRecoveredPanic(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		require.False(t, strings.HasPrefix(err.Error(), "panic: "), "recovered a panic: %v", err)
	}
}

// replaceICECredentials rewrites the ice-ufrag and ice-pwd of body, which is
// how a peer asks for an ICE restart.
func replaceICECredentials(t *testing.T, body []byte) []byte {
	t.Helper()
	out := make([]string, 0, 32)
	replaced := 0
	for _, l := range strings.Split(string(body), "\r\n") {
		switch {
		case strings.HasPrefix(l, "a=ice-ufrag:"):
			l = "a=ice-ufrag:RstUfragZ9"
			replaced++
		case strings.HasPrefix(l, "a=ice-pwd:"):
			l = "a=ice-pwd:RestartPasswordValue0123456789"
			replaced++
		}
		out = append(out, l)
	}
	require.Equal(t, 2, replaced, "SDP must carry ice-ufrag and ice-pwd:\n%s", body)
	return []byte(strings.Join(out, "\r\n"))
}

func iceSDPLine(t *testing.T, body []byte, prefix string) string {
	t.Helper()
	for _, l := range strings.Split(string(body), "\r\n") {
		if strings.HasPrefix(l, prefix) {
			return l
		}
	}
	t.Fatalf("SDP has no %q line:\n%s", prefix, body)
	return ""
}

// TestDialogICEForkSameCredentialsReinvite is a peer's re-INVITE with an SDP
// offer on an established ICE call, credentials unchanged. It must be answered
// 200 with our established ICE attributes, and tearing the dialog down must
// still release the ICE socket although the session that owns it was replaced.
func TestDialogICEForkSameCredentialsReinvite(t *testing.T) {
	e := newEstablishedICEMedia(t)
	d := newICEDialogMedia(t, e.answerer)
	tx := &fakeServerTransaction{}
	contact := &sip.ContactHeader{Address: sip.Uri{User: "us", Host: "127.0.0.1"}}

	err := handleMediaUpdateNoPanic(d, newReInvite(t, e.offer), tx, contact)
	requireNoRecoveredPanic(t, err)
	require.NoError(t, err)
	require.NotNil(t, tx.res)
	require.Equal(t, sip.StatusOK, tx.res.StatusCode, "reason: %s", tx.res.Reason)
	body := string(tx.res.Body())
	require.Contains(t, body, iceSDPLine(t, e.answer, "a=ice-ufrag:")+"\r\n")
	require.Contains(t, body, "a=candidate:")
	require.NotSame(t, e.answerer, d.MediaSession(), "the re-INVITE must install a fork")

	require.NoError(t, d.Close())
	reuse, err := net.ListenUDP("udp", &net.UDPAddr{IP: e.answererIP, Port: e.answererPort})
	require.NoError(t, err, "the ICE socket must be released when the dialog closes")
	require.NoError(t, reuse.Close())
}

// TestDialogICEForkEarlyMediaRestartSurfaced is an early media update whose
// answer changes the ICE credentials. The client path must get the restart
// error back rather than crash, and keep its current session.
func TestDialogICEForkEarlyMediaRestartSurfaced(t *testing.T) {
	e := newEstablishedICEMedia(t)
	require.True(t, e.offerer.RemoteSDPIsAnswer)
	d := newICEDialogMedia(t, e.offerer)

	err := checkEarlyMediaNoPanic(d, replaceICECredentials(t, e.answer))
	requireNoRecoveredPanic(t, err)
	require.True(t, errors.Is(err, media.ErrICERestartUnsupported), "want ErrICERestartUnsupported, got %v", err)
	require.Same(t, e.offerer, d.MediaSession(), "a refused update must leave the session unchanged")
}

// TestDialogICEForkRestartReinviteAnswers488 is a peer's re-INVITE asking for
// an ICE restart on an established ICE call. The offer is well formed and
// cannot be met, so it is answered 488 with a fixed reason phrase, and the
// call carries on over its current session (RFC 3261 section 14.2).
func TestDialogICEForkRestartReinviteAnswers488(t *testing.T) {
	e := newEstablishedICEMedia(t)
	d := newICEDialogMedia(t, e.answerer)
	tx := &fakeServerTransaction{}
	contact := &sip.ContactHeader{Address: sip.Uri{User: "us", Host: "127.0.0.1"}}

	err := handleMediaUpdateNoPanic(d, newReInvite(t, replaceICECredentials(t, e.offer)), tx, contact)
	requireNoRecoveredPanic(t, err)
	require.NoError(t, err)
	require.NotNil(t, tx.res)
	require.Equal(t, sip.StatusNotAcceptableHere, tx.res.StatusCode, "reason: %s", tx.res.Reason)
	require.Equal(t, "Not Acceptable Here", tx.res.Reason)
	require.Empty(t, tx.res.Body())
	require.Same(t, e.answerer, d.MediaSession(), "a refused re-INVITE must leave the session unchanged")
}
