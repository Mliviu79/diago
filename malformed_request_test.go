// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
		return func(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) {
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
					_ = tx.Respond(ctx, sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Server Internal Error", nil))
				}
				if done != nil {
					close(done)
				}
			}()
			next(ctx, req, tx)
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
	serveRequests(t, dg)

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
