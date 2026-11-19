// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// asyncLog returns a function that logs through t from a goroutine that may
// outlive t, such as a ServeBackground handler or a call it makes. Logging
// through t panics once t and every test above it have completed, as they have
// when the run is over, which a test run on its own or the last test reaches
// at once. The panic ends the test binary, or cuts a request handler short
// where sipgo recovers it. Once t's cleanups start, the returned function
// drops what it is given instead.
func asyncLog(t *testing.T) func(args ...any) {
	var mu sync.Mutex
	completed := false
	t.Cleanup(func() {
		mu.Lock()
		completed = true
		mu.Unlock()
	})
	return func(args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if !completed {
			t.Log(args...)
		}
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
		return func(ctx context.Context, req *sip.Request, tx sip.ServerTransaction) {
			h.mu.Lock()
			h.runs++
			h.mu.Unlock()
			defer func() {
				if r := recover(); r != nil {
					h.mu.Lock()
					h.panics = append(h.panics, fmt.Sprint(r))
					h.mu.Unlock()
					_ = tx.Respond(ctx, sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Server Internal Error", nil))
				}
				firstRun.Do(func() { close(h.done) })
			}()
			next(ctx, req, tx)
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
	serveRequests(t, dg)
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

// sentRequests records the methods of the requests a dialog sends through a
// client built by newRecordingDialogUA.
type sentRequests struct {
	mu      sync.Mutex
	methods []sip.RequestMethod
}

func (s *sentRequests) record(req *sip.Request) *sip.Response {
	s.mu.Lock()
	s.methods = append(s.methods, req.Method)
	s.mu.Unlock()
	return sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil)
}

func (s *sentRequests) reset() {
	s.mu.Lock()
	s.methods = nil
	s.mu.Unlock()
}

func (s *sentRequests) sent() []sip.RequestMethod {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]sip.RequestMethod(nil), s.methods...)
}

// newRecordingDialogUA returns a DialogUA whose client answers every request
// 200 without a transport, and what it was asked to send.
func newRecordingDialogUA(t *testing.T) (*sipgo.DialogUA, *sentRequests) {
	t.Helper()
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	client, err := sipgo.NewClient(ua)
	require.NoError(t, err)
	sent := &sentRequests{}
	client.TxRequester = &clientTxRequester{onRequest: sent.record}
	return &sipgo.DialogUA{Client: client, ContactHDR: sip.ContactHeader{Address: sip.Uri{User: "dg", Host: "127.0.0.1", Port: 5060}}}, sent
}

// peerSDP is the session description the peer of newAnsweredTestClientDialog
// answers and offers with.
func peerSDP() []byte {
	return sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34455, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ULAW})
}

// newAnsweredTestClientDialog returns an outgoing call, invited, answered with
// peerSDP and acknowledged, over a client that answers every request 200
// without a transport, and what the dialog sent after the ACK.
func newAnsweredTestClientDialog(t *testing.T) (*DialogClientSession, *sentRequests) {
	t.Helper()
	d, _, sent := newAnsweredTestClientCall(t)
	return d, sent
}

// newAnsweredTestClientCall returns the call newAnsweredTestClientDialog
// describes together with the media its Invite handed back.
func newAnsweredTestClientCall(t *testing.T) (*DialogClientSession, *DialogMedia, *sentRequests) {
	t.Helper()
	sent := &sentRequests{}
	dg := testDiagoClient(t, func(req *sip.Request) *sip.Response {
		res := sent.record(req)
		if req.IsInvite() {
			res.SetBody(peerSDP())
		}
		return res
	})
	ctx := context.Background()
	d, err := dg.NewDialog(sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}, NewDialogOptions{})
	require.NoError(t, err)
	t.Cleanup(func() { _ = d.Close() })
	med, err := d.Invite(ctx, InviteClientOptions{})
	require.NoError(t, err)
	require.NoError(t, d.Ack(ctx))
	sent.reset()
	return d, med, sent
}

// newPeerInDialogRequest builds a request of the given method the peer sends
// inside a dialog.
func newPeerInDialogRequest(method sip.RequestMethod, seq uint32) *sip.Request {
	peer := sip.Uri{User: "peer", Host: "127.0.0.1", Port: 5070}
	fromParams := sip.NewParams()
	fromParams.Add("tag", "peer-tag")
	toParams := sip.NewParams()
	toParams.Add("tag", "our-tag")
	req := sip.NewRequest(method, sip.Uri{User: "dg", Host: "127.0.0.1", Port: 5060})
	req.AppendHeader(&sip.FromHeader{Address: peer, Params: fromParams})
	req.AppendHeader(&sip.ToHeader{Address: sip.Uri{User: "dg", Host: "127.0.0.1"}, Params: toParams})
	req.AppendHeader(sip.NewHeader("Call-ID", "ended-dialog-request-test"))
	req.AppendHeader(&sip.CSeqHeader{SeqNo: seq, MethodName: method})
	req.AppendHeader(&sip.ContactHeader{Address: peer})
	return req
}

// answeredServerTx is an INVITE transaction that closes answered when a 2xx
// is sent on it.
type answeredServerTx struct {
	*byeServerTx
	once     sync.Once
	answered chan struct{}
}

func (tx *answeredServerTx) Respond(_ context.Context, res *sip.Response) error {
	if res.IsSuccess() {
		tx.once.Do(func() { close(tx.answered) })
	}
	return nil
}

// newAnsweredTestServerCall returns an inbound call over a fake transaction
// that offered peerSDP, was answered after early media and acknowledged, and
// the media its ProgressMedia handed back. That media is closed by the
// session's close callbacks and by nothing else: an answer after early media
// registers no close of its own on the dialog's end.
func newAnsweredTestServerCall(t *testing.T) (*DialogServerSession, *DialogMedia) {
	t.Helper()
	return newAnsweredTestServerCallOverUA(t, &sipgo.DialogUA{})
}

// newAnsweredTestServerCallOverUA returns the call newAnsweredTestServerCall
// describes, sending its own requests through ua.
func newAnsweredTestServerCallOverUA(t *testing.T, ua *sipgo.DialogUA) (*DialogServerSession, *DialogMedia) {
	t.Helper()
	inviteTx := &answeredServerTx{byeServerTx: newByeServerTx(), answered: make(chan struct{})}
	d := newTestDialogOverUA(t, inviteTx, ua)
	t.Cleanup(func() { _ = d.Close() })
	d.InviteRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	d.InviteRequest.SetBody(peerSDP())
	d.mediaConf = MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw}, bindIP: net.IPv4(127, 0, 0, 1)}

	med, err := d.ProgressMedia(t.Context(), ProgressMediaOptions{})
	require.NoError(t, err)
	answered := make(chan error, 1)
	go func() { answered <- d.AnswerEarlyMedia(med, AnswerOptions{}) }()
	select {
	case <-inviteTx.answered:
	case <-time.After(5 * time.Second):
		t.Fatal("the 2xx was never sent")
	}
	confirm(t, d)
	select {
	case err := <-answered:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the answer did not return once its ACK was read")
	}
	return d, med
}
