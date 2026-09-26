// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/examples"
	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/diago/testdata"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testDiagoClient(t *testing.T, onRequest func(req *sip.Request) *sip.Response, opts ...DiagoOption) *Diago {
	// Create client transaction request
	cTxReq := &clientTxRequester{
		onRequest: onRequest,
	}

	ua, _ := sipgo.NewUA()
	client, _ := sipgo.NewClient(ua)
	client.TxRequester = cTxReq
	t.Cleanup(func() {
		ua.Close()
	})

	opts = append(opts, WithClient(client))
	return NewDiago(ua, opts...)
}

func TestMain(m *testing.M) {
	examples.SetupLogger()
	m.Run()
}

func TestDiagoRegister(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		sync.OnceFunc(func() {
			sip.NewResponseFromRequest(req, 100, "Trying", nil)
		})()

		return sip.NewResponseFromRequest(req, 200, "OK", nil)
	})

	ctx := context.TODO()
	rtx, err := dg.RegisterTransaction(ctx, sip.Uri{User: "alice", Host: "localhost"}, RegisterOptions{})
	require.NoError(t, err)

	err = rtx.Register(ctx)
	require.NoError(t, err)
}

func TestDiagoRegisterAuthorization(t *testing.T) {
	t.Skip("Do test with sending Register and authorization returned")
}

func TestDiagoInviteCallerID(t *testing.T) {

	t.Run("NoSDPInResponse", func(t *testing.T) {
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		})

		_, err := dg.Invite(context.Background(), sip.Uri{User: "alice", Host: "localhost"}, InviteOptions{})
		if assert.Error(t, err) {
			assert.Equal(t, "no SDP in response", err.Error())
		}
	})

	reqCh := make(chan *sip.Request)
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		reqCh <- req
		return sip.NewResponseFromRequest(req, 500, "", nil)
	})

	t.Run("DefaultCallerID", func(t *testing.T) {
		go dg.Invite(context.Background(), sip.Uri{User: "alice", Host: "localhost"}, InviteOptions{})
		req := <-reqCh

		assert.Equal(t, dg.ua.Name(), req.From().Address.User)
		assert.Equal(t, dg.ua.Hostname(), req.From().Address.Host)
		assert.NotEmpty(t, req.From().Params.GetOr("tag", ""))
	})

}

func TestDiagoTransportConfs(t *testing.T) {
	type testCase = struct {
		tran                    Transport
		expectedContactHostPort string
		expectedMediaHost       string
	}

	doTest := func(tc testCase) {
		tran := tc.tran
		reqCh := make(chan *sip.Request)
		dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
			reqCh <- req
			return sip.NewResponseFromRequest(req, 200, "OK", nil)
		}, WithTransport(tran))

		go dg.Invite(context.TODO(), sip.Uri{User: "alice", Host: "localhost"}, InviteOptions{})

		// Now check our req passed on client
		req := <-reqCh

		// parse SDP
		sd := sdp.SessionDescription{}
		require.NoError(t, sdp.Unmarshal(req.Body(), &sd))
		connInfo, err := sd.ConnectionInformation()
		require.NoError(t, err)

		assert.Equal(t, tc.expectedContactHostPort, req.Contact().Address.HostPort())
		assert.Equal(t, tc.expectedMediaHost, connInfo.IP.String())
	}

	t.Run("ExternalHost", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Transport:    "udp",
				BindHost:     "127.0.0.111",
				BindPort:     15060,
				ExternalHost: "1.2.3.4",
			},
			expectedContactHostPort: "1.2.3.4:15060",
			expectedMediaHost:       "1.2.3.4",
		}

		doTest(tc)
	})

	t.Run("ExternalHostFQDN", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Transport:    "udp",
				BindHost:     "127.0.0.111",
				BindPort:     15060,
				ExternalHost: "myhost.pbx.com",
			},
			expectedContactHostPort: "myhost.pbx.com:15060",
			expectedMediaHost:       "127.0.0.111", // Hosts are not resolved so it goes with bind
		}

		doTest(tc)
	})

	t.Run("ExternalHostFQDNExternalMedia", func(t *testing.T) {
		tc := testCase{
			tran: Transport{
				Transport:       "udp",
				BindHost:        "127.0.0.111",
				BindPort:        15060,
				ExternalHost:    "myhost.pbx.com",
				MediaExternalIP: net.IPv4(1, 2, 3, 4),
			},
			expectedContactHostPort: "myhost.pbx.com:15060",
			expectedMediaHost:       "1.2.3.4", // Hosts are not resolved so it goes with bind
		}

		doTest(tc)
	})
}

func TestDiagoNewDialog(t *testing.T) {
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		body := sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ALAW})
		return sip.NewResponseFromRequest(req, 200, "OK", body)
	})
	ctx := context.TODO()

	t.Run("CloseNoError", func(t *testing.T) {
		dialog, err := dg.NewDialog(sip.Uri{User: "alice", Host: "localhost"}, NewDialogOptions{})
		require.NoError(t, err)
		dialog.Close()
	})

	// t.Run("NoAcked", func(t *testing.T) {
	// 	dialog, err := dg.NewDialog( sip.Uri{User: "alice", Host: "localhost"}, NewDialogOpts{})
	// 	require.NoError(t, err)
	// 	defer dialog.Close()

	// 	err = dialog.Invite(ctx, InviteOptions{})
	// 	require.NoError(t, err)

	// 	dialog.Audio
	// })

	t.Run("FullDialog", func(t *testing.T) {
		dialog, err := dg.NewDialog(sip.Uri{User: "alice", Host: "localhost"}, NewDialogOptions{})
		require.NoError(t, err)
		defer dialog.Close()

		err = dialog.Invite(ctx, InviteClientOptions{})
		require.NoError(t, err)
		assert.NotEmpty(t, dialog.ID)

		err = dialog.Ack(ctx)
		require.NoError(t, err)

		// assert.NotEmpty(t, dialog.ID)
	})

	t.Run("StoredBeforeAck", func(t *testing.T) {
		// The peer may send an in-dialog request the moment our ACK reaches
		// it, before the ACK write has returned here, so the dialog has to be
		// found from its 2xx on. Closing it removes it again.
		dialog, err := dg.NewDialog(sip.Uri{User: "alice", Host: "localhost"}, NewDialogOptions{})
		require.NoError(t, err)
		defer dialog.Close()

		err = dialog.Invite(ctx, InviteClientOptions{})
		require.NoError(t, err)

		stored, err := dg.cache.client.DialogLoad(ctx, dialog.ID)
		require.NoError(t, err)
		assert.Same(t, dialog, stored)

		require.NoError(t, dialog.Close())
		_, err = dg.cache.client.DialogLoad(ctx, dialog.ID)
		assert.ErrorIs(t, err, sipgo.ErrDialogDoesNotExists)
	})

	// _, err := dg.Invite(context.Background(), sip.Uri{User: "alice", Host: "localhost"}, InviteOptions{})
	// if assert.Error(t, err) {
	// 	assert.Equal(t, "no SDP in response", err.Error())
	// }
}

// TestDiagoInfoWithoutContentType sends real INFO requests to the INFO handler
// over a loopback socket. A body-less INFO is valid SIP and carries no
// Content-Type (RFC 3261 §20.15), so the handler must answer it rather than
// fail on the missing header. A middleware recovers any panic from the handler
// so the failure is reported instead of ending the test binary.
func TestDiagoInfoWithoutContentType(t *testing.T) {
	var mu sync.Mutex
	panics := map[string]string{}
	recoverPanic := func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(req *sip.Request, tx sip.ServerTransaction) {
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					panics[req.CallID().Value()] = fmt.Sprint(r)
					mu.Unlock()
					_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Server Internal Error", nil))
				}
			}()
			next(req, tx)
		}
	}

	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithServerRequestMiddleware(recoverPanic))

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	port := conn.LocalAddr().(*net.UDPAddr).Port
	go func() { _ = dg.server.ServeUDP(conn) }()

	clientUA, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = clientUA.Close() })
	client, err := sipgo.NewClient(clientUA, sipgo.WithClientHostname("127.0.0.1"))
	require.NoError(t, err)

	tests := []struct {
		name        string
		contentType string
		toTag       string
		body        []byte
		wantStatus  int
	}{
		{name: "no Content-Type", wantStatus: sip.StatusNotAcceptable},
		{name: "not DTMF relay", contentType: "text/plain", body: []byte("hello"), wantStatus: sip.StatusNotAcceptable},
		{name: "DTMF relay outside any dialog", contentType: "application/dtmf-relay", toTag: ";tag=nodialog", body: []byte("Signal=1\r\nDuration=100\r\n"), wantStatus: sip.StatusCallTransactionDoesNotExists},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			callID := fmt.Sprintf("info-no-ctype-%d", i)
			req := sip.NewRequest(sip.INFO, sip.Uri{User: "dg", Host: "127.0.0.1", Port: port})
			req.AppendHeader(sip.NewHeader("From", "<sip:peer@127.0.0.1>;tag=peer"))
			req.AppendHeader(sip.NewHeader("To", "<sip:dg@127.0.0.1>"+tc.toTag))
			req.AppendHeader(sip.NewHeader("Call-ID", callID))
			req.AppendHeader(sip.NewHeader("CSeq", "1 INFO"))
			if tc.contentType != "" {
				req.AppendHeader(sip.NewHeader("Content-Type", tc.contentType))
			}
			req.SetBody(tc.body)
			if tc.contentType == "" {
				require.Nil(t, req.ContentType(), "the request must reach the handler without Content-Type")
			}

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := client.Do(ctx, req)
			require.NoError(t, err)

			mu.Lock()
			recovered := panics[callID]
			mu.Unlock()
			require.Empty(t, recovered, "INFO handler panicked")
			assert.Equal(t, tc.wantStatus, res.StatusCode)
		})
	}
}

// TestDiagoFailedRequestWithoutFromOrTo sends real BYE, NOTIFY and INVITE
// requests lacking From or To to the handlers over a loopback socket. Dialog
// matching fails exactly when either header is missing, and a new INVITE is
// rejected before it is read, so each request is answered 400 and the failure
// path must not read the headers unchecked. A middleware recovers any
// panic from every handler run so the failure is reported instead of ending
// the test binary, and signals when the first run has returned, because the
// 400 goes out before the failure is logged.
func TestDiagoFailedRequestWithoutFromOrTo(t *testing.T) {
	var mu sync.Mutex
	panics := map[string][]string{}
	handlerDone := map[string]chan struct{}{}
	recoverPanic := func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(req *sip.Request, tx sip.ServerTransaction) {
			callID := req.CallID().Value()
			defer func() {
				r := recover()
				mu.Lock()
				if r != nil {
					panics[callID] = append(panics[callID], fmt.Sprint(r))
				}
				done := handlerDone[callID]
				delete(handlerDone, callID)
				mu.Unlock()
				if r != nil {
					_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Server Internal Error", nil))
				}
				if done != nil {
					close(done)
				}
			}()
			next(req, tx)
		}
	}

	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithServerRequestMiddleware(recoverPanic))

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	port := conn.LocalAddr().(*net.UDPAddr).Port
	go func() { _ = dg.server.ServeUDP(conn) }()

	clientUA, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = clientUA.Close() })
	client, err := sipgo.NewClient(clientUA, sipgo.WithClientHostname("127.0.0.1"))
	require.NoError(t, err)

	tests := []struct {
		name       string
		method     sip.RequestMethod
		absent     string
		headers    []sip.Header
		wantReason string
	}{
		{name: "BYE without From", method: sip.BYE, absent: "From", wantReason: "Bad Request"},
		{name: "BYE without To", method: sip.BYE, absent: "To", wantReason: "Bad Request"},
		{name: "NOTIFY without To", method: sip.NOTIFY, absent: "To", headers: []sip.Header{sip.NewHeader("Event", "refer")}, wantReason: "Bad Request"},
		{name: "INVITE without From", method: sip.INVITE, absent: "From", wantReason: "Missing From Header Field"},
		{name: "INVITE without To", method: sip.INVITE, absent: "To", wantReason: "Missing To Header Field"},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			callID := fmt.Sprintf("no-from-to-%d", i)
			req := sip.NewRequest(tc.method, sip.Uri{User: "dg", Host: "127.0.0.1", Port: port})
			if tc.absent != "From" {
				req.AppendHeader(sip.NewHeader("From", "<sip:peer@127.0.0.1>;tag=peer"))
			}
			if tc.absent != "To" {
				req.AppendHeader(sip.NewHeader("To", "<sip:dg@127.0.0.1>;tag=dg"))
			}
			req.AppendHeader(sip.NewHeader("Call-ID", callID))
			req.AppendHeader(sip.NewHeader("CSeq", "1 "+tc.method.String()))
			req.AppendHeader(sip.NewHeader("Contact", "<sip:peer@127.0.0.1>"))
			for _, h := range tc.headers {
				req.AppendHeader(h)
			}
			if tc.absent == "From" {
				require.Nil(t, req.From(), "the request must reach the handler without From")
			} else {
				require.Nil(t, req.To(), "the request must reach the handler without To")
			}

			done := make(chan struct{})
			mu.Lock()
			handlerDone[callID] = done
			mu.Unlock()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := client.Do(ctx, req, sipgo.ClientRequestAddVia)
			require.NoError(t, err)

			select {
			case <-done:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "handler did not return")
			}

			mu.Lock()
			recovered := panics[callID]
			mu.Unlock()
			require.Empty(t, recovered, "%s handler panicked", tc.method)
			assert.Equal(t, sip.StatusBadRequest, res.StatusCode)
			assert.Equal(t, tc.wantReason, res.Reason)
		})
	}
}

// TestDiagoNewInviteMissingHeader sends new INVITEs that each lack one header
// dialog setup needs, and checks that each is answered once through its
// transaction: the transaction answers the retransmission and absorbs the ACK,
// the handler runs once and the rejection is logged once. The INVITEs go out as
// raw datagrams over a loopback socket, because client.Do would fill in the
// missing headers, a request without CSeq cannot open a client transaction,
// and the retransmission and the ACK have to be sent deliberately. A request
// without CSeq never reaches the handler: the transaction layer answers it
// before any handler runs, with a reason phrase sipgo chooses and its own tests
// pin, so that row asserts the status only. The control row is a complete
// INVITE for a dialog that does not exist: the handler runs once and answers
// it 481 (RFC 3261 section 12.2.2) without logging a warning. Each row gets
// its own Diago so the handler runs and the logged warnings belong to that row
// alone.
func TestDiagoNewInviteMissingHeader(t *testing.T) {
	tests := []struct {
		name       string
		absent     string
		toTag      string
		wantStatus int
		wantReason string
		wantRuns   int
		wantWarn   bool
	}{
		{name: "without From", absent: "From", wantStatus: sip.StatusBadRequest, wantReason: "Missing From Header Field", wantRuns: 1, wantWarn: true},
		{name: "without From tag", absent: "From tag", wantStatus: sip.StatusBadRequest, wantReason: "Missing From Tag", wantRuns: 1, wantWarn: true},
		{name: "without To", absent: "To", wantStatus: sip.StatusBadRequest, wantReason: "Missing To Header Field", wantRuns: 1, wantWarn: true},
		{name: "without Call-ID", absent: "Call-ID", wantStatus: sip.StatusBadRequest, wantReason: "Missing Call-ID Header Field", wantRuns: 1, wantWarn: true},
		{name: "without Contact", absent: "Contact", wantStatus: sip.StatusBadRequest, wantReason: "Missing Contact Header Field", wantRuns: 1, wantWarn: true},
		{name: "without CSeq", absent: "CSeq", wantStatus: sip.StatusBadRequest, wantRuns: 0},
		{name: "unknown dialog", toTag: "unknown", wantStatus: sip.StatusCallTransactionDoesNotExists, wantReason: "Call/Transaction Does Not Exist", wantRuns: 1},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newRawInviteHarness(t)
			id := fmt.Sprintf("missing-header-%d", i)
			invite := h.newInvite(id, tc.absent, tc.toTag)

			h.send(t, "INVITE", invite)
			first := readFinalResponse(t, h.client, time.Second)
			assert.Equal(t, tc.wantStatus, first.StatusCode)
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, first.Reason)
			}

			h.send(t, "INVITE", invite)
			second := readFinalResponse(t, h.client, time.Second)
			assert.Equal(t, first.StatusCode, second.StatusCode, "the retransmission must get the same answer")
			assert.Equal(t, first.Reason, second.Reason, "the retransmission must get the same answer")

			if tc.absent != "CSeq" {
				h.send(t, "ACK", ackLines(invite, first))
			}
			drainDatagrams(h.client, 100*time.Millisecond)
			requireNoDatagram(t, h.client, 1500*time.Millisecond)

			if tc.wantRuns > 0 {
				select {
				case <-h.done:
				case <-time.After(5 * time.Second):
					require.FailNow(t, "handler did not return")
				}
			}

			h.mu.Lock()
			defer h.mu.Unlock()
			require.Empty(t, h.panics, "INVITE handler panicked")
			assert.Equal(t, tc.wantRuns, h.runs, "handler runs")

			warns := h.log.failedRequestWarnings()
			if !tc.wantWarn {
				assert.Empty(t, warns)
				return
			}
			require.Len(t, warns, 1)
			assert.Contains(t, warns[0].attrs["error"], tc.wantReason)
		})
	}
}

// TestDescribeMissingInviteHeader checks every header the new-INVITE check
// names, including CSeq, which the loopback test cannot reach because the
// transaction layer answers a request without it before any handler runs.
func TestDescribeMissingInviteHeader(t *testing.T) {
	tests := []struct {
		name   string
		absent string
		want   string
	}{
		{name: "complete", want: ""},
		{name: "without From", absent: "From", want: "Missing From Header Field"},
		{name: "without From tag", absent: "From tag", want: "Missing From Tag"},
		{name: "without To", absent: "To", want: "Missing To Header Field"},
		{name: "without Call-ID", absent: "Call-ID", want: "Missing Call-ID Header Field"},
		{name: "without CSeq", absent: "CSeq", want: "Missing CSeq Header Field"},
		{name: "without Contact", absent: "Contact", want: "Missing Contact Header Field"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := sip.NewRequest(sip.INVITE, sip.Uri{User: "dg", Host: "127.0.0.1", Port: 5060})
			switch tc.absent {
			case "From":
			case "From tag":
				req.AppendHeader(sip.NewHeader("From", "<sip:peer@127.0.0.1>"))
			default:
				req.AppendHeader(sip.NewHeader("From", "<sip:peer@127.0.0.1>;tag=peer"))
			}
			if tc.absent != "To" {
				req.AppendHeader(sip.NewHeader("To", "<sip:dg@127.0.0.1>"))
			}
			if tc.absent != "Call-ID" {
				req.AppendHeader(sip.NewHeader("Call-ID", "describe-missing"))
			}
			if tc.absent != "CSeq" {
				req.AppendHeader(sip.NewHeader("CSeq", "1 INVITE"))
			}
			if tc.absent != "Contact" {
				req.AppendHeader(sip.NewHeader("Contact", "<sip:peer@127.0.0.1>"))
			}

			assert.Equal(t, tc.want, describeMissingInviteHeader(req))
		})
	}
}

// rawInviteHarness is one Diago served on a loopback socket, a separate
// loopback socket that plays the peer, and what the handler and the logger saw.
type rawInviteHarness struct {
	client     net.PacketConn
	serverAddr net.Addr
	serverPort int
	clientPort int
	log        *recordingLogHandler
	done       chan struct{}

	mu     sync.Mutex
	runs   int
	panics []string
}

func newRawInviteHarness(t *testing.T) *rawInviteHarness {
	t.Helper()
	h := &rawInviteHarness{
		log:  &recordingLogHandler{},
		done: make(chan struct{}),
	}
	var firstRun sync.Once
	middleware := func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(req *sip.Request, tx sip.ServerTransaction) {
			h.mu.Lock()
			h.runs++
			h.mu.Unlock()
			defer func() {
				if r := recover(); r != nil {
					h.mu.Lock()
					h.panics = append(h.panics, fmt.Sprint(r))
					h.mu.Unlock()
					_ = tx.Respond(sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Server Internal Error", nil))
				}
				firstRun.Do(func() { close(h.done) })
			}()
			next(req, tx)
		}
	}

	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithLogger(slog.New(h.log)), WithServerRequestMiddleware(middleware))

	server, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = server.Close() })
	go func() { _ = dg.server.ServeUDP(server) }()
	h.serverAddr = server.LocalAddr()
	h.serverPort = server.LocalAddr().(*net.UDPAddr).Port

	h.client, err = net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = h.client.Close() })
	h.clientPort = h.client.LocalAddr().(*net.UDPAddr).Port
	return h
}

// newInvite returns the header lines of a new INVITE without the one header
// named by absent. toTag, when set, makes it an INVITE within a dialog.
func (h *rawInviteHarness) newInvite(id, absent, toTag string) []string {
	lines := []string{fmt.Sprintf("Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-%s", h.clientPort, id)}
	switch absent {
	case "From":
	case "From tag":
		lines = append(lines, "From: <sip:peer@127.0.0.1>")
	default:
		lines = append(lines, "From: <sip:peer@127.0.0.1>;tag=peer")
	}
	if absent != "To" {
		to := "To: <sip:dg@127.0.0.1>"
		if toTag != "" {
			to += ";tag=" + toTag
		}
		lines = append(lines, to)
	}
	if absent != "Call-ID" {
		lines = append(lines, "Call-ID: "+id)
	}
	if absent != "CSeq" {
		lines = append(lines, "CSeq: 1 INVITE")
	}
	if absent != "Contact" {
		lines = append(lines, fmt.Sprintf("Contact: <sip:peer@127.0.0.1:%d>", h.clientPort))
	}
	return append(lines, "Max-Forwards: 70", "Content-Length: 0")
}

// send writes one request with the given method and header lines to the Diago.
func (h *rawInviteHarness) send(t *testing.T, method string, headers []string) {
	t.Helper()
	startLine := fmt.Sprintf("%s sip:dg@127.0.0.1:%d SIP/2.0", method, h.serverPort)
	datagram := strings.Join(append([]string{startLine}, headers...), "\r\n") + "\r\n\r\n"
	_, err := h.client.WriteTo([]byte(datagram), h.serverAddr)
	require.NoError(t, err)
}

// ackLines returns the header lines of the ACK for a non-2xx final response to
// the INVITE: the same Via branch, the response's To and CSeq method ACK.
func ackLines(invite []string, res *sip.Response) []string {
	ack := make([]string, 0, len(invite))
	for _, line := range invite {
		switch {
		case strings.HasPrefix(line, "CSeq:"):
			ack = append(ack, "CSeq: 1 ACK")
		case strings.HasPrefix(line, "To:"):
			if to := res.To(); to != nil {
				ack = append(ack, "To: "+to.Value())
			}
		default:
			ack = append(ack, line)
		}
	}
	return ack
}

// readFinalResponse returns the next final response the peer socket receives,
// skipping provisional ones, and fails the test if none arrives in time.
func readFinalResponse(t *testing.T, conn net.PacketConn, within time.Duration) *sip.Response {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(within)))
	buf := make([]byte, 65535)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			require.FailNow(t, fmt.Sprintf("no final response within %s: %v", within, err))
		}
		msg, err := sip.ParseMessage(bytes.Clone(buf[:n]))
		require.NoError(t, err)
		res, ok := msg.(*sip.Response)
		require.True(t, ok, "expected a response, got %T", msg)
		if res.IsProvisional() {
			continue
		}
		return res
	}
}

// drainDatagrams discards whatever the peer socket receives for the duration.
func drainDatagrams(conn net.PacketConn, duration time.Duration) {
	_ = conn.SetReadDeadline(time.Now().Add(duration))
	buf := make([]byte, 65535)
	for {
		if _, _, err := conn.ReadFrom(buf); err != nil {
			return
		}
	}
}

// requireNoDatagram fails the test if the peer socket receives anything within
// the duration.
func requireNoDatagram(t *testing.T, conn net.PacketConn, within time.Duration) {
	t.Helper()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(within)))
	buf := make([]byte, 65535)
	n, _, err := conn.ReadFrom(buf)
	if err == nil {
		require.FailNow(t, fmt.Sprintf("unexpected datagram: %q", strings.SplitN(string(buf[:n]), "\r\n", 2)[0]))
	}
	var netErr net.Error
	require.True(t, errors.As(err, &netErr) && netErr.Timeout(), "reading the peer socket failed: %v", err)
}

// recordingLogHandler is a slog.Handler that keeps every record it receives.
type recordingLogHandler struct {
	mu      sync.Mutex
	records []recordedLog
}

type recordedLog struct {
	level   slog.Level
	message string
	attrs   map[string]string
}

func (l *recordingLogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (l *recordingLogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := map[string]string{}
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.String()
		return true
	})
	l.mu.Lock()
	defer l.mu.Unlock()
	l.records = append(l.records, recordedLog{level: r.Level, message: r.Message, attrs: attrs})
	return nil
}

func (l *recordingLogHandler) WithAttrs([]slog.Attr) slog.Handler { return l }

func (l *recordingLogHandler) WithGroup(string) slog.Handler { return l }

// failedRequestWarnings returns the warnings Diago logged for a request its
// handler failed.
func (l *recordingLogHandler) failedRequestWarnings() []recordedLog {
	l.mu.Lock()
	defer l.mu.Unlock()
	var warns []recordedLog
	for _, r := range l.records {
		if r.level == slog.LevelWarn && r.message == "Failed to handle request" {
			warns = append(warns, r)
		}
	}
	return warns
}

func TestIntegrationDiagoTransportEmpheralPort(t *testing.T) {
	tran := Transport{
		Transport: "udp",
		BindHost:  "127.0.0.1",
		BindPort:  0,
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()

	dg := NewDiago(ua, WithTransport(tran))

	err := dg.ServeBackground(context.TODO(), func(d *DialogServerSession) {})
	require.NoError(t, err)

	newTran, _ := dg.getTransport("udp")
	t.Log("port assigned", newTran.BindPort)
	assert.NotEmpty(t, newTran.BindPort)
}

func TestIntegrationDiagoCallWithCustomCodecs(t *testing.T) {
	// TODO: USE TLS as transport for more correct test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	l16Codec := media.Codec{
		Name:        "L16",
		PayloadType: 98,
		SampleRate:  8000,
		SampleDur:   20 * time.Millisecond,
		NumChannels: 1,
	}

	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua,
			WithTransport(
				Transport{
					ID:        "tcp",
					Transport: "tcp",
					BindHost:  "127.0.0.1",
					BindPort:  15066,
				},
			),
			WithMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{l16Codec, media.CodecAudioAlaw},
				},
			))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.Trying()
			if err := d.Answer(); err != nil {
				panic(err)
			}

			err := d.Echo()
			slog.Info("Echo finished with", "error", err)

		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua,
		WithTransport(
			Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
			},
		),
		WithMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{l16Codec, media.CodecAudioAlaw},
			},
		))

	d, err := dg.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 15066}, InviteOptions{Transport: "tcp"})
	require.NoError(t, err)

	l16Audio := bytes.Repeat([]byte{0, 16, 96, 0}, l16Codec.Samples16()/4)
	reader := bytes.NewBuffer(l16Audio)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, len(l16Audio))
	r.Read(recv)
	assert.Equal(t, l16Audio, recv)
}

func TestIntegrationDiagoSRTPCall(t *testing.T) {
	// TODO: USE TLS as transport for more correct test
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua,
			WithTransport(
				Transport{
					ID:        "tcp",
					Transport: "tcp",
					BindHost:  "127.0.0.1",
					BindPort:  15443,
					MediaSRTP: 1, // This enables SRTP
				},
			),
			WithMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
				},
			))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.Trying()
			if err := d.Answer(); err != nil {
				panic(err)
			}

			err := d.Echo()
			slog.Info("Echo finished with", "error", err)

		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua,
		WithTransport(
			Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
				BindPort:  15441,
				MediaSRTP: 1, // USE SRTP
			},
		),
		WithMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			},
		))

	// err = dg.ServeBackground(ctx, func(d *DialogServerSession) {})
	// require.NoError(t, err)

	d, err := dg.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 15443}, InviteOptions{Transport: "tcp"})
	require.NoError(t, err)

	// pb, err := d.PlaybackCreate()
	// if err != nil {
	// 	panic(err)
	// }

	ulaw := make([]byte, 160)
	audio.EncodeUlawTo(ulaw, bytes.Repeat([]byte{1}, 320))

	reader := bytes.NewBuffer(ulaw)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, 160)
	r.Read(recv)
	assert.Equal(t, ulaw, recv)
}

func TestIntegrationDiagoDTLSCall(t *testing.T) {
	// TODO: USE TLS as transport for more correct test
	// media.DTLSDebug = true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	{
		ua, _ := sipgo.NewUA()
		defer ua.Close()

		dg := NewDiago(ua,
			WithTransport(
				Transport{
					ID:        "tcp",
					Transport: "tcp",
					BindHost:  "127.0.0.1",
					BindPort:  16443,
					MediaSRTP: 2, // This enables SRTP DTLS
					MediaDTLSConf: media.DTLSConfig{
						Certificates:     []tls.Certificate{testdata.ServerCertificate()},
						ServerClientAuth: media.ServerClientAuthNoCert,
					},
				},
			),
			WithMediaConfig(
				MediaConfig{
					Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
				},
			))

		err := dg.ServeBackground(ctx, func(d *DialogServerSession) {
			d.Trying()
			if err := d.Answer(); err != nil {
				panic(err)
			}

			err := d.Echo()
			slog.Info("Echo finished with", "error", err)

		})
		require.NoError(t, err)
	}

	ua, _ := sipgo.NewUA()
	defer ua.Close()
	dg := NewDiago(ua,
		WithTransport(
			Transport{
				ID:        "tcp",
				Transport: "tcp",
				BindHost:  "127.0.0.1",
				BindPort:  16441,
				MediaSRTP: 2, // USE DTLS
				// RFC 5763 section 5: the offerer advertises actpass and must be
				// ready to act as the DTLS server, which needs a certificate. The
				// a=fingerprint binds the certificate to the signalling, so the
				// server has to ask for the peer certificate to verify it.
				MediaDTLSConf: media.DTLSConfig{
					Certificates:     []tls.Certificate{testdata.ClientCertificate()},
					ServerClientAuth: media.ServerClientAuthRequireCert,
				},
			},
		),
		WithMediaConfig(
			MediaConfig{
				Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw},
			},
		))

	// err = dg.ServeBackground(ctx, func(d *DialogServerSession) {})
	// require.NoError(t, err)

	d, err := dg.Invite(ctx, sip.Uri{User: "11", Host: "127.0.0.1", Port: 16443}, InviteOptions{Transport: "tcp"})
	require.NoError(t, err)

	// pb, err := d.PlaybackCreate()
	// if err != nil {
	// 	panic(err)
	// }

	ulaw := make([]byte, 160)
	audio.EncodeUlawTo(ulaw, bytes.Repeat([]byte{1}, 320))

	reader := bytes.NewBuffer(ulaw)
	r, _ := d.AudioReader()
	w, _ := d.AudioWriter()
	time.Sleep(1 * time.Second)
	t.Log("---------------------------Writing media")
	_, err = media.Copy(reader, w)
	require.ErrorIs(t, err, io.EOF)

	recv := make([]byte, 160)
	r.Read(recv)
	assert.Equal(t, ulaw, recv)
}
