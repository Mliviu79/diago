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

// TestDiagoInfoWithoutContentType sends real INFO requests to the INFO handler
// over a loopback socket. A body-less INFO is valid SIP and carries no
// Content-Type (RFC 3261 §20.15), so the handler must answer it rather than
// fail on the missing header. The dialog is matched first, so one outside any
// dialog is refused as every in-dialog request without the tags of a dialog
// is. A middleware recovers any panic from the handler so the failure is
// reported instead of ending the test binary.
func TestDiagoInfoWithoutContentType(t *testing.T) {
	var mu sync.Mutex
	panics := map[string]string{}
	recoverPanic := func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) {
			defer func() {
				if r := recover(); r != nil {
					mu.Lock()
					panics[req.CallID().Value()] = fmt.Sprint(r)
					mu.Unlock()
					_ = tx.Respond(ctx, sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Server Internal Error", nil))
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
		name        string
		contentType string
		toTag       string
		body        []byte
		wantStatus  int
	}{
		{name: "no Content-Type", wantStatus: sip.StatusBadRequest},
		{name: "not DTMF relay", contentType: "text/plain", body: []byte("hello"), wantStatus: sip.StatusBadRequest},
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

// TestDiagoInfoMatchesDialogFirst sends real INFO requests to the INFO handler
// over a loopback socket, each carrying a Content-Type other than DTMF relay.
// An INFO for no dialog, or for an ended one, matches no live dialog, and RFC
// 3261 section 12.2.2 has it answered 481, whatever it carries. The
// Content-Type was checked first, so it was answered 406. An INFO for a live
// dialog is still refused 406.
func TestDiagoInfoMatchesDialogFirst(t *testing.T) {
	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua)

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

	// newInfo builds an INFO in the dialog of d, or in no dialog when d is
	// nil.
	newInfo := func(t *testing.T, d *DialogServerSession) *sip.Request {
		t.Helper()
		req := sip.NewRequest(sip.INFO, sip.Uri{User: "dg", Host: "127.0.0.1", Port: port})
		if d == nil {
			req.AppendHeader(sip.NewHeader("From", "<sip:peer@127.0.0.1>;tag=peer"))
			req.AppendHeader(sip.NewHeader("To", "<sip:dg@127.0.0.1>;tag=nodialog"))
			req.AppendHeader(sip.NewHeader("Call-ID", "info-first-nodialog"))
		} else {
			inv := d.InviteRequest
			req.AppendHeader(sip.HeaderClone(inv.From()))
			req.AppendHeader(sip.HeaderClone(inv.To()))
			req.AppendHeader(sip.HeaderClone(inv.CallID()))
		}
		req.AppendHeader(sip.NewHeader("CSeq", "500 INFO"))
		req.AppendHeader(sip.NewHeader("Content-Type", "text/plain"))
		req.SetBody([]byte("hello"))
		return req
	}
	// storedDialog returns an inbound dialog, confirmed, stored in the cache
	// of dg, with a Call-ID of its own.
	storedDialog := func(t *testing.T, callID string) *DialogServerSession {
		t.Helper()
		d := newTestDialogOverUA(t, newByeServerTx(), &sipgo.DialogUA{})
		d.InviteRequest.ReplaceHeader(sip.NewHeader("Call-ID", callID))
		id, err := sip.DialogIDFromRequestUAS(d.InviteRequest)
		require.NoError(t, err)
		d.ID = id
		d.InviteResponse = sip.NewResponseFromRequest(d.InviteRequest, sip.StatusOK, "OK", nil)
		confirm(t, d)
		require.NoError(t, dg.cache.server.DialogStore(context.Background(), d.ID, d))
		t.Cleanup(func() { _ = dg.cache.server.DialogDelete(context.Background(), d.ID) })
		return d
	}

	for _, tc := range []struct {
		name       string
		dialog     func(t *testing.T) *DialogServerSession
		wantStatus int
	}{
		{
			name:       "no dialog",
			dialog:     func(*testing.T) *DialogServerSession { return nil },
			wantStatus: sip.StatusCallTransactionDoesNotExists,
		},
		{
			name: "ended dialog",
			dialog: func(t *testing.T) *DialogServerSession {
				d := storedDialog(t, "info-first-ended")
				require.NoError(t, d.ReadBye(t.Context(), newBye(t, d, d.InviteRequest.CSeq().SeqNo+1), newByeServerTx()))
				return d
			},
			wantStatus: sip.StatusCallTransactionDoesNotExists,
		},
		{
			name:       "live dialog",
			dialog:     func(t *testing.T) *DialogServerSession { return storedDialog(t, "info-first-live") },
			wantStatus: sip.StatusNotAcceptable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := newInfo(t, tc.dialog(t))
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			res, err := client.Do(ctx, req)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, res.StatusCode)
		})
	}
}
