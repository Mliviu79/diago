// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// refreshClock is how long one second of session-timer time lasts in these
// tests: a 90 second session interval lasts 900ms, and a refresh is due every
// 450ms.
const refreshClock = 10 * time.Millisecond

// refreshInterval is the session interval, in session-timer seconds, every
// call in these tests starts with, and refreshWall how long it lasts.
const (
	refreshInterval = 90
	refreshWall     = refreshInterval * refreshClock
)

// diagoRunInChildProcess runs the calling test again, alone, in a child process
// of the test binary, and reports true once it has passed there, so the caller
// returns. In the child it reports false and the caller runs its body. A test
// that shortens sipgo's package-wide transaction timers needs a process of its
// own: transactions that earlier tests leave running read those timers from
// their own goroutines, and changing the timers under them is a data race.
func diagoRunInChildProcess(t *testing.T) bool {
	t.Helper()
	if os.Getenv("DIAGO_TEST_CHILD") == t.Name() {
		return false
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^"+t.Name()+"$",
		"-test.count=1",
		"-test.v",
		"-test.timeout=60s",
	)
	cmd.Env = append(os.Environ(), "DIAGO_TEST_CHILD="+t.Name())
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "child process failed:\n%s", out)
	require.Contains(t, string(out), "--- PASS: "+t.Name(), "the child process did not run the test:\n%s", out)
	return true
}

// peerMessage is one message the raw peer read, and where it came from.
type peerMessage struct {
	msg  sip.Message
	from net.Addr
}

// refreshPeer is a raw SIP peer on a loopback socket that calls one Diago,
// serving with session timers, and plays the far end of the call's session
// refreshes.
type refreshPeer struct {
	conn   net.PacketConn
	port   int
	dgAddr *net.UDPAddr
	log    *recordingLogHandler
	in     chan peerMessage

	callID  string
	fromTag string
	cseq    uint32
	// to and contact are the To and the Contact of the 200 that answered the
	// INVITE: our dialog's remote tag and remote target.
	to      string
	contact string

	// seen holds the Via branches of the requests already handed to the
	// test, so a retransmission is passed over.
	seen map[string]bool

	dialogs     chan *DialogServerSession
	stop        chan struct{}
	handlerDone chan struct{}
	// dialog is the call's dialog, once call has returned it.
	dialog *DialogServerSession
}

// newRefreshPeer serves a Diago with session timers on a loopback port, whose
// serve handler calls setup, when set, and answers, and a raw peer socket.
func newRefreshPeer(t *testing.T, setup func(d *DialogServerSession)) *refreshPeer {
	t.Helper()
	p := &refreshPeer{
		log:         &recordingLogHandler{},
		in:          make(chan peerMessage, 256),
		callID:      fmt.Sprintf("refresh-%d", time.Now().UnixNano()),
		fromTag:     "peer-tag",
		seen:        map[string]bool{},
		dialogs:     make(chan *DialogServerSession, 1),
		stop:        make(chan struct{}),
		handlerDone: make(chan struct{}),
	}

	var err error
	p.conn, err = net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	p.port = p.conn.LocalAddr().(*net.UDPAddr).Port
	readerDone := make(chan struct{})
	go p.read(readerDone)
	t.Cleanup(func() {
		_ = p.conn.Close()
		<-readerDone
	})

	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	policy := SessionTimerPolicy{Enabled: true, PreferUASRefresher: true, clock: refreshClock}
	dg := NewDiago(ua,
		WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}),
		WithSessionTimers(policy),
		WithLogger(slog.New(p.log)),
	)
	require.NoError(t, serveBackground(t, dg, t.Context(), func(d *DialogServerSession) {
		defer close(p.handlerDone)
		if setup != nil {
			setup(d)
		}
		if err := d.Answer(); err != nil {
			return
		}
		p.dialogs <- d
		select {
		case <-d.Context().Done():
		case <-p.stop:
			// The call ends here, so its BYE has its answer before the handler
			// returns.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = d.Hangup(ctx)
		}
	}))
	tran, _ := dg.getTransport("udp")
	p.dgAddr = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tran.BindPort}

	// The call ends with the test: the handler returns, Diago hangs up a call
	// still up, and the peer answers every BYE until the handler and the
	// session timer's goroutine have returned, before the stack closes under a
	// BYE still waiting for its answer.
	t.Cleanup(func() {
		close(p.stop)
		handlerDone := p.handlerDone
		var timerDone <-chan struct{}
		if p.dialog != nil {
			if timer := p.dialog.timer.Load(); timer != nil {
				timerDone = timer.done
			}
		}
		deadline := time.After(15 * time.Second)
		for handlerDone != nil || timerDone != nil {
			select {
			case <-handlerDone:
				handlerDone = nil
			case <-timerDone:
				timerDone = nil
			case m := <-p.in:
				if req, ok := m.msg.(*sip.Request); ok && req.Method == sip.BYE {
					p.write(t, sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil), m.from)
				}
			case <-deadline:
				t.Error("the serve handler or the session timer did not return with the test")
				return
			}
		}
	})
	return p
}

// read hands every message the peer socket receives to p.in until the socket
// is closed.
func (p *refreshPeer) read(done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, 65535)
	for {
		n, from, err := p.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		msg, err := sip.ParseMessage(bytes.Clone(buf[:n]))
		if err != nil {
			continue
		}
		select {
		case p.in <- peerMessage{msg: msg, from: from}:
		default:
		}
	}
}

func (p *refreshPeer) write(t *testing.T, msg sip.Message, to net.Addr) {
	t.Helper()
	_, err := p.conn.WriteTo([]byte(msg.String()), to)
	require.NoError(t, err)
}

func (p *refreshPeer) offer() []byte {
	return sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34800, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ULAW})
}

// request builds a request of the peer's with the given start line and
// headers, CSeq and a fresh Via branch.
func (p *refreshPeer) request(t *testing.T, method sip.RequestMethod, target string, cseq uint32, headers []string, body []byte) *sip.Request {
	t.Helper()
	lines := []string{
		fmt.Sprintf("%s %s SIP/2.0", method, target),
		fmt.Sprintf("Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-%s-%s-%d-%d", p.port, p.callID, method, cseq, time.Now().UnixNano()),
		fmt.Sprintf("From: <sip:peer@127.0.0.1>;tag=%s", p.fromTag),
		"Call-ID: " + p.callID,
		fmt.Sprintf("CSeq: %d %s", cseq, method),
		fmt.Sprintf("Contact: <sip:peer@127.0.0.1:%d>", p.port),
		"Max-Forwards: 70",
	}
	if p.to != "" {
		lines = append(lines, "To: "+p.to)
	} else {
		lines = append(lines, "To: <sip:dg@127.0.0.1>")
	}
	lines = append(lines, headers...)
	if len(body) > 0 {
		lines = append(lines, "Content-Type: application/sdp")
	}
	lines = append(lines, fmt.Sprintf("Content-Length: %d", len(body)))
	raw := strings.Join(lines, "\r\n") + "\r\n\r\n" + string(body)
	msg, err := sip.ParseMessage([]byte(raw))
	require.NoError(t, err)
	return msg.(*sip.Request)
}

// call sends the INVITE with the given extra headers, reads its 200, ACKs it
// and returns the dialog Diago's handler answered.
func (p *refreshPeer) call(t *testing.T, headers ...string) *DialogServerSession {
	t.Helper()
	p.cseq = 1
	invite := p.request(t, sip.INVITE, fmt.Sprintf("sip:dg@127.0.0.1:%d", p.dgAddr.Port), p.cseq, headers, p.offer())
	p.write(t, invite, p.dgAddr)
	res := p.final(t, p.cseq, time.Second)
	require.Equal(t, sip.StatusOK, res.StatusCode)
	p.to = res.To().Value()
	require.NotNil(t, res.Contact())
	p.contact = res.Contact().Address.String()
	p.ack(t, p.cseq)
	select {
	case d := <-p.dialogs:
		p.dialog = d
		return d
	case <-time.After(2 * time.Second):
		require.FailNow(t, "the call was not answered")
		return nil
	}
}

func (p *refreshPeer) ack(t *testing.T, cseq uint32) {
	t.Helper()
	p.write(t, p.request(t, sip.ACK, p.contact, cseq, nil, nil), p.dgAddr)
}

// final returns the final response to the peer's request with cseq, passing
// over provisional responses and retransmissions of earlier finals.
func (p *refreshPeer) final(t *testing.T, cseq uint32, within time.Duration) *sip.Response {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case m := <-p.in:
			res, ok := m.msg.(*sip.Response)
			if !ok {
				req := m.msg.(*sip.Request)
				require.FailNow(t, fmt.Sprintf("unexpected %s while waiting for a response", req.Method))
			}
			if res.IsProvisional() || res.CSeq() == nil || res.CSeq().SeqNo != cseq {
				continue
			}
			return res
		case <-deadline:
			require.FailNow(t, fmt.Sprintf("no final response to CSeq %d within %s", cseq, within))
			return nil
		}
	}
}

// reInvite sends a re-INVITE of the peer's with the given extra headers and
// SDP, and returns its final response, ACKed.
func (p *refreshPeer) reInvite(t *testing.T, headers ...string) *sip.Response {
	t.Helper()
	p.cseq++
	p.write(t, p.request(t, sip.INVITE, p.contact, p.cseq, headers, p.offer()), p.dgAddr)
	res := p.final(t, p.cseq, 2*time.Second)
	p.ack(t, p.cseq)
	return res
}

// await returns the next request Diago sends with method, passing over
// retransmissions and, unless method is ACK, ACKs. Any other request fails
// the test.
func (p *refreshPeer) await(t *testing.T, method sip.RequestMethod, within time.Duration) (*sip.Request, net.Addr) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case m := <-p.in:
			req, ok := m.msg.(*sip.Request)
			if !ok {
				continue
			}
			branch := viaBranch(req)
			if p.seen[branch] {
				continue
			}
			if req.Method == sip.ACK && method != sip.ACK {
				continue
			}
			p.seen[branch] = true
			require.Equal(t, method, req.Method, "Diago sent an unexpected request")
			return req, m.from
		case <-deadline:
			require.FailNow(t, fmt.Sprintf("Diago sent no %s within %s", method, within))
			return nil, nil
		}
	}
}

// expectNothing fails the test if Diago sends a request other than an ACK or
// a retransmission within the duration.
func (p *refreshPeer) expectNothing(t *testing.T, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case m := <-p.in:
			req, ok := m.msg.(*sip.Request)
			if !ok || req.Method == sip.ACK || p.seen[viaBranch(req)] {
				continue
			}
			require.FailNow(t, fmt.Sprintf("Diago sent %s CSeq %s where it should have sent nothing", req.Method, req.CSeq().Value()))
		case <-deadline:
			return
		}
	}
}

// answer answers Diago's request with status, the extra headers and, for a
// 2xx, a Contact and body when body is set.
func (p *refreshPeer) answer(t *testing.T, req *sip.Request, from net.Addr, status int, reason string, body []byte, headers ...string) {
	t.Helper()
	res := sip.NewResponseFromRequest(req, status, reason, body)
	if status >= 200 && status < 300 {
		res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: p.port}})
		if len(body) > 0 {
			res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		}
	}
	for _, h := range headers {
		name, value, _ := strings.Cut(h, ":")
		res.AppendHeader(sip.NewHeader(name, strings.TrimSpace(value)))
	}
	p.write(t, res, from)
}

// answerOK answers Diago's re-INVITE 200 with SDP and the given headers.
func (p *refreshPeer) answerOK(t *testing.T, req *sip.Request, from net.Addr, headers ...string) {
	t.Helper()
	p.answer(t, req, from, sip.StatusOK, "OK", p.offer(), headers...)
}

// warned reports whether Diago logged a warning with message whose attrs
// include want.
func (p *refreshPeer) warned(message string, want map[string]string) bool {
	p.log.mu.Lock()
	defer p.log.mu.Unlock()
	for _, r := range p.log.records {
		if r.level != slog.LevelWarn || r.message != message {
			continue
		}
		match := true
		for k, v := range want {
			if r.attrs[k] != v {
				match = false
			}
		}
		if match {
			return true
		}
	}
	return false
}

func viaBranch(req *sip.Request) string {
	if via := req.Via(); via != nil {
		branch, _ := via.Params.Get("branch")
		return branch
	}
	return ""
}

func headerValue(msg interface{ GetHeader(string) sip.Header }, name string) string {
	if h := msg.GetHeader(name); h != nil {
		return h.Value()
	}
	return ""
}

func cseqOf(req *sip.Request) uint32 {
	return req.CSeq().SeqNo
}

// weRefreshCall is the INVITE of a peer that names us the refresher.
var weRefreshCall = []string{"Supported: timer", "Session-Expires: " + strconv.Itoa(refreshInterval) + ";refresher=uas"}

// refreshDue bounds the wait for a refresh of ours: it is due half a session
// interval after the last change.
const refreshDue = refreshWall/2 + time.Second

// TestSessionRefreshFollowsRFC4028 checks the session refresh we send while we
// are the refresher, and what we do with its answer (RFC 4028 sections 7 and
// 10), against a raw peer on a loopback whose INVITE names us the refresher.
func TestSessionRefreshFollowsRFC4028(t *testing.T) {
	t.Run("our refresh carries the timer headers", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, weRefreshCall...)
		req, from := p.await(t, sip.INVITE, refreshDue)
		assert.Equal(t, "timer", headerValue(req, "Supported"))
		assert.Equal(t, "90;refresher=uac", headerValue(req, "Session-Expires"))
		assert.Nil(t, req.GetHeader("Min-SE"), "no Min-SE was seen, so none is sent")
		p.answerOK(t, req, from, "Session-Expires: 90;refresher=uac")
	})

	t.Run("our refresh carries the Min-SE the INVITE carried", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, append([]string{"Min-SE: 90"}, weRefreshCall...)...)
		req, from := p.await(t, sip.INVITE, refreshDue)
		assert.Equal(t, "timer", headerValue(req, "Supported"))
		assert.Equal(t, "90;refresher=uac", headerValue(req, "Session-Expires"))
		assert.Equal(t, "90", headerValue(req, "Min-SE"))
		p.answerOK(t, req, from, "Session-Expires: 90;refresher=uac")
	})

	t.Run("a 2xx without Session-Expires turns the timer off", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, weRefreshCall...)
		req, from := p.await(t, sip.INVITE, refreshDue)
		p.answerOK(t, req, from)
		p.expectNothing(t, 2*refreshWall)
	})

	t.Run("a 481 to our refresh ends the call with our BYE", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		d := p.call(t, weRefreshCall...)
		req, from := p.await(t, sip.INVITE, refreshDue)
		p.answer(t, req, from, sip.StatusCallTransactionDoesNotExists, "Call/Transaction Does Not Exist", nil)
		bye, from := p.await(t, sip.BYE, 5*time.Second)
		p.answer(t, bye, from, sip.StatusOK, "OK", nil)
		select {
		case <-d.Context().Done():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "the dialog did not end with our BYE")
		}
		assert.True(t, p.warned("Session refresh failed, ending the call", map[string]string{"status": "481"}))
	})

	t.Run("a refresh with no final answer ends the call with our BYE", func(t *testing.T) {
		if diagoRunInChildProcess(t) {
			return
		}
		sip.SetTimers(10*time.Millisecond, 40*time.Millisecond, 50*time.Millisecond)
		p := newRefreshPeer(t, nil)
		p.call(t, weRefreshCall...)
		p.await(t, sip.INVITE, refreshDue)
		// The re-INVITE and its retransmissions go unanswered until Timer B,
		// 64*T1, ends the transaction.
		bye, from := p.await(t, sip.BYE, 5*time.Second)
		p.answer(t, bye, from, sip.StatusOK, "OK", nil)
	})

	t.Run("a 503 with Retry-After is retried once after it", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, weRefreshCall...)
		req, from := p.await(t, sip.INVITE, refreshDue)
		p.answer(t, req, from, sip.StatusServiceUnavailable, "Service Unavailable", nil, "Retry-After: 30")
		answered := time.Now()
		retry, from := p.await(t, sip.INVITE, 5*time.Second)
		assert.GreaterOrEqual(t, time.Since(answered), 30*refreshClock-50*time.Millisecond, "the retry did not wait for Retry-After")
		assert.Greater(t, cseqOf(retry), cseqOf(req), "the retry is a new request")
		assert.Equal(t, "90;refresher=uac", headerValue(retry, "Session-Expires"))
		p.answer(t, retry, from, sip.StatusServiceUnavailable, "Service Unavailable", nil, "Retry-After: 30")
		p.expectNothing(t, 2*refreshWall)
		assert.True(t, p.warned("Session refresh failed, refreshing stopped", map[string]string{"status": "503"}))
	})

	t.Run("a 422 is retried once with its Min-SE", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, weRefreshCall...)
		req, from := p.await(t, sip.INVITE, refreshDue)
		p.answer(t, req, from, statusSessionIntervalTooSmall, "Session Interval Too Small", nil, "Min-SE: 120")
		retry, from := p.await(t, sip.INVITE, 2*time.Second)
		assert.Greater(t, cseqOf(retry), cseqOf(req), "the retry is a new request")
		assert.Equal(t, "120;refresher=uac", headerValue(retry, "Session-Expires"))
		assert.Equal(t, "120", headerValue(retry, "Min-SE"))
		p.answerOK(t, retry, from, "Session-Expires: 120;refresher=uac")
	})

	t.Run("a 2xx without SDP keeps the media session and refreshing goes on", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		d := p.call(t, weRefreshCall...)
		before := d.MediaSession()
		req, from := p.await(t, sip.INVITE, refreshDue)
		p.answer(t, req, from, sip.StatusOK, "OK", nil, "Session-Expires: 90;refresher=uac")
		next, from := p.await(t, sip.INVITE, refreshDue)
		assert.Same(t, before, d.MediaSession(), "the running media session was replaced")
		assert.NoError(t, d.Context().Err(), "the call ended")
		assert.True(t, p.warned("Session refresh answered without SDP, keeping the media session", nil))
		p.answerOK(t, next, from, "Session-Expires: 90;refresher=uac")
	})

	t.Run("a challenge to our refresh is answered with the dialog's credentials", func(t *testing.T) {
		p := newRefreshPeer(t, func(d *DialogServerSession) {
			d.SetCredentials(sipgo.DigestAuth{Username: "agent", Password: "secret"})
		})
		p.call(t, weRefreshCall...)
		req, from := p.await(t, sip.INVITE, refreshDue)
		p.answer(t, req, from, sip.StatusUnauthorized, "Unauthorized", nil, `WWW-Authenticate: Digest realm="pbx", nonce="refresh-nonce", algorithm=MD5`)
		retry, from := p.await(t, sip.INVITE, 2*time.Second)
		assert.Contains(t, headerValue(retry, "Authorization"), `username="agent"`)
		assert.Greater(t, cseqOf(retry), cseqOf(req), "the retry carries the dialog's next CSeq")
		assert.Equal(t, "90;refresher=uac", headerValue(retry, "Session-Expires"))
		p.answerOK(t, retry, from, "Session-Expires: 90;refresher=uac")
		ack, _ := p.await(t, sip.ACK, 2*time.Second)
		assert.Equal(t, cseqOf(retry), cseqOf(ack), "our ACK names the retried re-INVITE")
	})
}

// peerRefreshesCall is the INVITE of a peer that names itself the refresher.
var peerRefreshesCall = []string{"Supported: timer", "Session-Expires: " + strconv.Itoa(refreshInterval) + ";refresher=uac"}

// TestSessionRefreshAnswersThePeersRefreshAsRFC4028 checks the 2xx we send to
// a peer's re-INVITE while the session timer runs, and the timer it sets (RFC
// 4028 section 9), against a raw peer on a loopback whose INVITE names itself
// the refresher.
func TestSessionRefreshAnswersThePeersRefreshAsRFC4028(t *testing.T) {
	t.Run("the 2xx to the peer's refresh carries the timer", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, peerRefreshesCall...)
		res := p.reInvite(t, "Supported: timer", "Session-Expires: 90;refresher=uac")
		require.Equal(t, sip.StatusOK, res.StatusCode)
		assert.Equal(t, "90;refresher=uac", headerValue(res, "Session-Expires"))
		assert.Equal(t, "timer", headerValue(res, "Require"))
		assert.Equal(t, "timer", headerValue(res, "Supported"))
	})

	t.Run("a peer that keeps refreshing is not cut, and one that stops is", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		d := p.call(t, peerRefreshesCall...)
		var last time.Time
		for range 10 {
			// Half an interval apart, five intervals in all.
			p.expectNothing(t, refreshWall/2)
			res := p.reInvite(t, "Supported: timer", "Session-Expires: 90;refresher=uac")
			require.Equal(t, sip.StatusOK, res.StatusCode)
			last = time.Now()
		}
		bye, from := p.await(t, sip.BYE, refreshWall+time.Second)
		assert.GreaterOrEqual(t, time.Since(last), refreshWall, "the BYE came before the session expired")
		p.answer(t, bye, from, sip.StatusOK, "OK", nil)
		select {
		case <-d.Context().Done():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "the dialog did not end with our BYE")
		}
	})

	t.Run("a refresh naming us the refresher moves the refreshing to us", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, peerRefreshesCall...)
		res := p.reInvite(t, "Supported: timer", "Session-Expires: 90;refresher=uas")
		require.Equal(t, sip.StatusOK, res.StatusCode)
		assert.Equal(t, "90;refresher=uas", headerValue(res, "Session-Expires"))
		assert.Nil(t, res.GetHeader("Require"), "Require: timer is for a peer that refreshes")
		sent := time.Now()
		req, from := p.await(t, sip.INVITE, refreshWall)
		assert.Less(t, time.Since(sent), refreshWall, "our refresh came after the session would have expired")
		assert.Equal(t, "90;refresher=uac", headerValue(req, "Session-Expires"))
		p.answerOK(t, req, from, "Session-Expires: 90;refresher=uac")
		p.expectNothing(t, refreshWall/4)
	})

	t.Run("a re-INVITE of a peer without the timer makes us the refresher", func(t *testing.T) {
		p := newRefreshPeer(t, nil)
		p.call(t, peerRefreshesCall...)
		res := p.reInvite(t)
		require.Equal(t, sip.StatusOK, res.StatusCode)
		assert.Equal(t, "90;refresher=uas", headerValue(res, "Session-Expires"))
		assert.Equal(t, "timer", headerValue(res, "Supported"))
		assert.Nil(t, res.GetHeader("Require"))
	})
}
