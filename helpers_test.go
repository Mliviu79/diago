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
	require.NoError(t, serveRequests(t.Context(), dg))
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
