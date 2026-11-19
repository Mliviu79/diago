// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestDiagoByeMatchesNoDialogIsGone pins that a BYE matching no dialog is
// answered 481 by name. The INFO row of TestDiagoInfoMatchesDialogFirst covers
// the shared answer; this names the BYE path, so a change to the BYE handler's
// no-dialog branch fails here.
func TestDiagoByeMatchesNoDialogIsGone(t *testing.T) {
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

	req := sip.NewRequest(sip.BYE, sip.Uri{User: "dg", Host: "127.0.0.1", Port: port})
	req.AppendHeader(sip.NewHeader("From", "<sip:peer@127.0.0.1>;tag=peer"))
	req.AppendHeader(sip.NewHeader("To", "<sip:dg@127.0.0.1>;tag=nodialog"))
	req.AppendHeader(sip.NewHeader("Call-ID", "bye-nodialog"))
	req.AppendHeader(sip.NewHeader("CSeq", "500 BYE"))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	res, err := client.Do(ctx, req)
	require.NoError(t, err)
	assert.Equal(t, sip.StatusCallTransactionDoesNotExists, res.StatusCode)
}

// TestDiagoOutOfOrderBye sends the BYE handler, over a loopback socket, a BYE
// whose CSeq is below the INVITE's and then one in order. The first is out of
// order: RFC 3261 section 12.2.2 has it answered 500, and the call goes on, so
// its media must stay open. The second ends the dialog, and closing the media
// then unblocks whoever is still reading or writing it.
func TestDiagoOutOfOrderBye(t *testing.T) {
	handled := make(chan struct{}, 1)
	signalHandled := func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) {
			defer func() {
				select {
				case handled <- struct{}{}:
				default:
				}
			}()
			next(ctx, req, tx)
		}
	}

	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithServerRequestMiddleware(signalHandled))

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	go func() { _ = dg.server.ServeUDP(conn) }()
	serveRequests(t, dg)

	d, med := newAnsweredTestServerCall(t)
	require.NoError(t, dg.cache.server.DialogStore(context.Background(), d.ID, d))

	clientUA, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = clientUA.Close() })
	client, err := sipgo.NewClient(clientUA, sipgo.WithClientHostname("127.0.0.1"))
	require.NoError(t, err)

	mediaClosed := func() bool {
		med.mu.Lock()
		defer med.mu.Unlock()
		return med.closed
	}
	// sendBye returns the BYE's final response, nil when none came, once the
	// handler has returned.
	sendBye := func(t *testing.T, seq uint32) *sip.Response {
		t.Helper()
		bye := newBye(t, d, seq)
		bye.SetDestination(conn.LocalAddr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := client.Do(ctx, bye, sipgo.ClientRequestAddVia)
		select {
		case <-handled:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "the BYE handler did not return")
		}
		assert.NoError(t, err, "the BYE was never answered")
		return res
	}

	res := sendBye(t, d.InviteRequest.CSeq().SeqNo-1)
	if assert.NotNil(t, res) {
		assert.Equal(t, sip.StatusInternalServerError, res.StatusCode)
	}
	assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
	assert.False(t, mediaClosed(), "an out-of-order BYE closed the media of a call that goes on")

	res = sendBye(t, d.InviteRequest.CSeq().SeqNo+1)
	if assert.NotNil(t, res) {
		assert.Equal(t, sip.StatusOK, res.StatusCode)
	}
	assert.Equal(t, sip.DialogStateEnded, d.LoadState())
	assert.True(t, mediaClosed(), "the BYE that ended the call left its media open")
}

// TestDiagoOutOfOrderByeCallingSide sends the BYE handler, over a loopback
// socket, a BYE for an outgoing call whose CSeq is below that of a re-INVITE
// the peer sent before it, and then one in order. The first is out of order
// (RFC 3261 section 12.2.2). A sipgo that refuses it answers 500 and the call
// goes on, so its media must stay open; one that does not check the order
// answers 200 and ends the call. Either way the media is closed exactly when
// the dialog has ended.
func TestDiagoOutOfOrderByeCallingSide(t *testing.T) {
	handled := make(chan struct{}, 1)
	signalHandled := func(next sipgo.RequestHandler) sipgo.RequestHandler {
		return func(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) {
			defer func() {
				select {
				case handled <- struct{}{}:
				default:
				}
			}()
			next(ctx, req, tx)
		}
	}

	ua, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithServerRequestMiddleware(signalHandled))

	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	go func() { _ = dg.server.ServeUDP(conn) }()
	serveRequests(t, dg)

	d, med, _ := newAnsweredTestClientCall(t)
	require.NoError(t, d.ReadRequest(newPeerInDialogRequest(sip.INVITE, 5), newByeServerTx()))
	// The peer's requests carry tags of their own, so the dialog is stored
	// under the ID they give.
	id, err := sip.DialogIDFromRequestUAC(newPeerInDialogRequest(sip.BYE, 4))
	require.NoError(t, err)
	require.NoError(t, dg.cache.client.DialogStore(context.Background(), id, d))

	clientUA, _ := sipgo.NewUA()
	t.Cleanup(func() { _ = clientUA.Close() })
	client, err := sipgo.NewClient(clientUA, sipgo.WithClientHostname("127.0.0.1"))
	require.NoError(t, err)

	mediaClosed := func() bool {
		med.mu.Lock()
		defer med.mu.Unlock()
		return med.closed
	}
	// sendBye returns the BYE's final response, nil when none came, once the
	// handler has returned.
	sendBye := func(t *testing.T, seq uint32) *sip.Response {
		t.Helper()
		bye := newPeerInDialogRequest(sip.BYE, seq)
		bye.SetDestination(conn.LocalAddr().String())
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		res, err := client.Do(ctx, bye, sipgo.ClientRequestAddVia)
		select {
		case <-handled:
		case <-time.After(5 * time.Second):
			require.FailNow(t, "the BYE handler did not return")
		}
		assert.NoError(t, err, "the BYE was never answered")
		return res
	}

	res := sendBye(t, 4)
	require.NotNil(t, res)
	if res.StatusCode == sip.StatusInternalServerError {
		assert.Equal(t, sip.DialogStateConfirmed, d.LoadState())
		assert.False(t, mediaClosed(), "an out-of-order BYE closed the media of a call that goes on")
		res = sendBye(t, 6)
		require.NotNil(t, res)
	}
	assert.Equal(t, sip.StatusOK, res.StatusCode)
	assert.Equal(t, sip.DialogStateEnded, d.LoadState())
	assert.True(t, mediaClosed(), "the BYE that ended the call left its media open")
}
