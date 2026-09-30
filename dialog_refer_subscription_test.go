// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// referRole is one side our dialog can take in a call: the one that answered
// the peer's INVITE, or the one that sent its own.
type referRole struct {
	name string
	dial func(t *testing.T) *referPeer
}

var referRoles = []referRole{
	{name: "server session", dial: newReferPeerCallingUs},
	{name: "client session", dial: newReferPeerWeCall},
}

// referPeer is a raw SIP peer on a loopback socket holding one call with a
// Diago, and the transferee of the REFER our side of that call sends: the test
// scripts every answer, NOTIFY and BYE it sends.
type referPeer struct {
	conn net.PacketConn
	port int
	in   chan peerMessage
	// seen holds the Via branches of Diago's requests already handed to the
	// test, so a retransmission is passed over.
	seen map[string]bool

	// dgAddr is where Diago listens. callID, from, to and target are the dialog
	// as the peer's requests carry it: From naming the peer with its tag, To
	// naming Diago with Diago's tag, and Diago's Contact URI.
	dgAddr *net.UDPAddr
	callID string
	from   string
	to     string
	target string
	cseq   uint32

	// dialog is our side of the call.
	dialog observingReferrer
}

// referResult is what one ReferAndObserve returned, and when.
type referResult struct {
	obs ReferObservation
	err error
	at  time.Time
}

// newReferPeerSocket opens the peer's loopback socket and reads it until the
// test ends.
func newReferPeerSocket(t *testing.T) *referPeer {
	t.Helper()
	p := &referPeer{
		in:     make(chan peerMessage, 256),
		seen:   map[string]bool{},
		callID: fmt.Sprintf("refer-%d", time.Now().UnixNano()),
	}
	var err error
	p.conn, err = net.ListenPacket("udp", "127.0.0.1:0")
	require.NoError(t, err)
	p.port = p.conn.LocalAddr().(*net.UDPAddr).Port
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
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
	}()
	t.Cleanup(func() {
		_ = p.conn.Close()
		<-readerDone
	})
	return p
}

// newReferDiago serves a Diago on a loopback port with f as its serve handler,
// and returns it with the address it listens on.
func newReferDiago(t *testing.T, f ServeDialogFunc) (*Diago, *net.UDPAddr) {
	t.Helper()
	ua, err := sipgo.NewUA()
	require.NoError(t, err)
	t.Cleanup(func() { _ = ua.Close() })
	dg := NewDiago(ua, WithTransport(Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}))
	require.NoError(t, serveBackground(t, dg, t.Context(), f))
	tran, _ := dg.getTransport("udp")
	return dg, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: tran.BindPort}
}

// newReferPeerCallingUs has the peer call a Diago, whose serve handler answers
// and then holds the dialog until the test ends, so an ended dialog stays where
// an in-dialog NOTIFY finds it. Our side is a DialogServerSession.
func newReferPeerCallingUs(t *testing.T) *referPeer {
	t.Helper()
	p := newReferPeerSocket(t)
	dialogs := make(chan *DialogServerSession, 1)
	hold := make(chan struct{})
	handlerDone := make(chan struct{})
	_, p.dgAddr = newReferDiago(t, func(d *DialogServerSession) {
		defer close(handlerDone)
		if err := d.Answer(); err != nil {
			return
		}
		dialogs <- d
		<-hold
	})
	// The handler returns with the test; Diago hangs up a call still up, and
	// the peer answers that BYE until the handler has returned.
	t.Cleanup(func() {
		close(hold)
		p.answerByesUntil(t, handlerDone)
	})

	p.from = "<sip:peer@127.0.0.1>;tag=peer-tag"
	p.to = "<sip:dg@127.0.0.1>"
	p.target = fmt.Sprintf("sip:dg@127.0.0.1:%d", p.dgAddr.Port)
	p.cseq = 1
	p.write(t, p.request(t, sip.INVITE, p.cseq, nil, "application/sdp", referPeerOffer()), p.dgAddr)
	res := p.final(t, sip.INVITE, p.cseq, 2*time.Second)
	require.Equal(t, sip.StatusOK, res.StatusCode)
	require.NotNil(t, res.Contact())
	p.to = res.To().Value()
	p.target = res.Contact().Address.String()
	p.write(t, p.request(t, sip.ACK, p.cseq, nil, "", nil), p.dgAddr)

	select {
	case d := <-dialogs:
		p.dialog = d
	case <-time.After(2 * time.Second):
		require.FailNow(t, "the call was not answered")
	}
	return p
}

// newReferPeerWeCall has a Diago call the peer, which answers. Our side is a
// DialogClientSession, kept until the test ends.
func newReferPeerWeCall(t *testing.T) *referPeer {
	t.Helper()
	p := newReferPeerSocket(t)
	var dg *Diago
	dg, p.dgAddr = newReferDiago(t, nil)

	type dialed struct {
		d   *DialogClientSession
		err error
	}
	out := make(chan dialed, 1)
	go func() {
		d, err := dg.Invite(t.Context(), sip.Uri{User: "peer", Host: "127.0.0.1", Port: p.port}, InviteOptions{})
		out <- dialed{d, err}
	}()

	invite, from := p.await(t, sip.INVITE, 2*time.Second)
	res := sip.NewResponseFromRequest(invite, sip.StatusOK, "OK", referPeerOffer())
	res.AppendHeader(&sip.ContactHeader{Address: sip.Uri{User: "peer", Host: "127.0.0.1", Port: p.port}})
	res.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	p.write(t, res, from)

	var d *DialogClientSession
	select {
	case r := <-out:
		require.NoError(t, r.err)
		d = r.d
	case <-time.After(2 * time.Second):
		require.FailNow(t, "our INVITE was not answered")
	}
	p.await(t, sip.ACK, 2*time.Second)
	// The call ends with the test: a dialog still up is hung up, the peer
	// answering its BYE, and then closed.
	t.Cleanup(func() {
		if d.DialogSIP().LoadState() != sip.DialogStateEnded {
			hungUp := make(chan struct{})
			go func() {
				defer close(hungUp)
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				_ = d.Hangup(ctx)
			}()
			p.answerByesUntil(t, hungUp)
		}
		_ = d.Close()
	})

	p.callID = invite.CallID().Value()
	p.from = res.To().Value()
	p.to = invite.From().Value()
	p.target = invite.Contact().Address.String()
	p.cseq = 0
	p.dialog = d
	return p
}

func referPeerOffer() []byte {
	return sdp.GenerateForAudio(net.IPv4(127, 0, 0, 1), net.IPv4(127, 0, 0, 1), 34800, sdp.ModeSendrecv, []string{sdp.FORMAT_TYPE_ULAW})
}

func (p *referPeer) write(t *testing.T, msg sip.Message, to net.Addr) {
	t.Helper()
	_, err := p.conn.WriteTo([]byte(msg.String()), to)
	require.NoError(t, err)
}

// request builds a request of the peer's in the call with cseq, the extra
// headers and the body.
func (p *referPeer) request(t *testing.T, method sip.RequestMethod, cseq uint32, headers []string, contentType string, body []byte) *sip.Request {
	t.Helper()
	lines := []string{
		fmt.Sprintf("%s %s SIP/2.0", method, p.target),
		fmt.Sprintf("Via: SIP/2.0/UDP 127.0.0.1:%d;branch=z9hG4bK-%s-%s-%d-%d", p.port, p.callID, method, cseq, time.Now().UnixNano()),
		"From: " + p.from,
		"To: " + p.to,
		"Call-ID: " + p.callID,
		fmt.Sprintf("CSeq: %d %s", cseq, method),
		fmt.Sprintf("Contact: <sip:peer@127.0.0.1:%d>", p.port),
		"Max-Forwards: 70",
	}
	lines = append(lines, headers...)
	if contentType != "" {
		lines = append(lines, "Content-Type: "+contentType)
	}
	lines = append(lines, fmt.Sprintf("Content-Length: %d", len(body)))
	raw := strings.Join(lines, "\r\n") + "\r\n\r\n" + string(body)
	msg, err := sip.ParseMessage([]byte(raw))
	require.NoError(t, err)
	return msg.(*sip.Request)
}

// final returns the final response to the peer's request with method and
// cseq, passing over provisional responses, responses to other requests and
// retransmissions of Diago's requests. A new request of Diago's fails the test.
func (p *referPeer) final(t *testing.T, method sip.RequestMethod, cseq uint32, within time.Duration) *sip.Response {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case m := <-p.in:
			res, ok := m.msg.(*sip.Response)
			if !ok {
				req := m.msg.(*sip.Request)
				if p.seen[viaBranch(req)] {
					continue
				}
				require.FailNow(t, fmt.Sprintf("unexpected %s while waiting for the answer to %s", req.Method, method))
			}
			if res.IsProvisional() || res.CSeq() == nil || res.CSeq().SeqNo != cseq || res.CSeq().MethodName != method {
				continue
			}
			return res
		case <-deadline:
			require.FailNow(t, fmt.Sprintf("no final response to %s CSeq %d within %s", method, cseq, within))
			return nil
		}
	}
}

// await returns the next request Diago sends with method, passing over
// retransmissions and, unless method is ACK, ACKs. Any other request fails the
// test.
func (p *referPeer) await(t *testing.T, method sip.RequestMethod, within time.Duration) (*sip.Request, net.Addr) {
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

// answerByesUntil answers every BYE Diago sends until done is closed, bounded.
func (p *referPeer) answerByesUntil(t *testing.T, done <-chan struct{}) {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		select {
		case <-done:
			return
		case m := <-p.in:
			if req, ok := m.msg.(*sip.Request); ok && req.Method == sip.BYE {
				p.write(t, sip.NewResponseFromRequest(req, sip.StatusOK, "OK", nil), m.from)
			}
		case <-deadline:
			t.Error("the call did not end with the test")
			return
		}
	}
}

// send sends a request of the peer's in the call and returns the status of its
// final response.
func (p *referPeer) send(t *testing.T, method sip.RequestMethod, headers []string, contentType string, body []byte) int {
	t.Helper()
	p.cseq++
	p.write(t, p.request(t, method, p.cseq, headers, contentType, body), p.dgAddr)
	return p.final(t, method, p.cseq, 2*time.Second).StatusCode
}

// notify sends a refer NOTIFY for the REFER with CSeq id, carrying state as its
// Subscription-State and frag as its sipfrag, and returns its answer's status.
func (p *referPeer) notify(t *testing.T, id uint32, state, frag string) int {
	t.Helper()
	headers := []string{fmt.Sprintf("Event: refer;id=%d", id), "Subscription-State: " + state}
	return p.send(t, sip.NOTIFY, headers, "message/sipfrag;version=2.0", []byte(frag))
}

// bye sends the peer's BYE and returns its answer's status.
func (p *referPeer) bye(t *testing.T) int {
	t.Helper()
	return p.send(t, sip.BYE, nil, "", nil)
}

// startRefer has our dialog send a REFER, observed on its own goroutine under
// ctx, and returns once the peer has it, with the channel its result arrives on.
func (p *referPeer) startRefer(t *testing.T, ctx context.Context) (<-chan referResult, *sip.Request, net.Addr) {
	t.Helper()
	out := make(chan referResult, 1)
	go func() {
		obs, err := p.dialog.ReferAndObserve(ctx, sip.Uri{User: "carol", Host: "127.0.0.1", Port: 5070}, ReferObserveOptions{})
		out <- referResult{obs: obs, err: err, at: time.Now()}
	}()
	refer, from := p.await(t, sip.REFER, 2*time.Second)
	return out, refer, from
}

// accept answers the REFER 202 and returns its CSeq, the Event id of its
// subscription's NOTIFYs (RFC 3515 section 2.4.6).
func (p *referPeer) accept(t *testing.T, refer *sip.Request, from net.Addr) uint32 {
	t.Helper()
	p.write(t, sip.NewResponseFromRequest(refer, sip.StatusAccepted, "Accepted", nil), from)
	return refer.CSeq().SeqNo
}

// referToItsFinal runs one REFER whose subscription ends on a final NOTIFY of
// 200, and returns its Event id.
func (p *referPeer) referToItsFinal(t *testing.T) uint32 {
	t.Helper()
	out, refer, from := p.startRefer(t, t.Context())
	id := p.accept(t, refer, from)
	require.Equal(t, sip.StatusOK, p.notify(t, id, "terminated;reason=noresource", "SIP/2.0 200 OK"))
	r := awaitReferResult(t, out, 5*time.Second)
	require.NoError(t, r.err)
	require.Equal(t, ReferEndFinalNotify, r.obs.End)
	return id
}

func awaitReferResult(t *testing.T, out <-chan referResult, within time.Duration) referResult {
	t.Helper()
	select {
	case r := <-out:
		return r
	case <-time.After(within):
		require.FailNow(t, fmt.Sprintf("the REFER attempt did not end within %s", within))
		return referResult{}
	}
}

// requireEndedAtItsBound fails unless an attempt whose bound started at or
// after start ended no earlier than bound after it, and not much later.
func requireEndedAtItsBound(t *testing.T, start time.Time, r referResult, bound time.Duration) {
	t.Helper()
	elapsed := r.at.Sub(start)
	assert.GreaterOrEqual(t, elapsed, bound, "the attempt ended before its subscription did")
	assert.Less(t, elapsed, bound+time.Second, "the attempt outlived its subscription")
}

// requireMediaClosed waits, bounded, until d's media is closed.
func requireMediaClosed(t *testing.T, d DialogSession) {
	t.Helper()
	m := d.Media()
	require.Eventually(t, func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.closed
	}, 2*time.Second, 5*time.Millisecond, "the dialog's media was not closed")
}

// pace waits d, the gap the peer leaves between two of its messages.
func pace(d time.Duration) {
	<-time.After(d)
}

// TestReferAttemptLivesAsItsSubscription checks our REFER's attempt ends where
// its refer subscription does and nowhere else: on the expires the last active
// NOTIFY gave (RFC 6665 section 4.1.3), on Timer N when no NOTIFY comes
// (section 4.1.2.4), on a final or terminating NOTIFY, and at once when the
// caller gives it up. T1 is shortened, in a child process of its own because
// sipgo's timers are package state, so Timer N, 64×T1, is 1.28s.
func TestReferAttemptLivesAsItsSubscription(t *testing.T) {
	if diagoRunInChildProcess(t) {
		return
	}
	// Not restored: the change lives and dies with this child process, and a
	// write while a transaction of the test winds down would be a data race.
	sip.SetTimers(20*time.Millisecond, sip.T2, sip.T4)
	timerN := 64 * sip.T1

	for _, role := range referRoles {
		t.Run(role.name, func(t *testing.T) {
			t.Parallel()

			t.Run("an active NOTIFY's expires ends the attempt when it passes", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				out, refer, from := p.startRefer(t, t.Context())
				id := p.accept(t, refer, from)
				sent := time.Now()
				require.Equal(t, sip.StatusOK, p.notify(t, id, "active;expires=1", "SIP/2.0 100 Trying"))

				r := awaitReferResult(t, out, 5*time.Second)
				require.NoError(t, r.err)
				assert.Equal(t, ReferEndSubscriptionExpired, r.obs.End)
				assert.Equal(t, []int{100}, notifyStatuses(r.obs))
				requireEndedAtItsBound(t, sent, r, time.Second)
			})

			t.Run("a later active NOTIFY's expires replaces the bound", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				out, refer, from := p.startRefer(t, t.Context())
				id := p.accept(t, refer, from)
				require.Equal(t, sip.StatusOK, p.notify(t, id, "active;expires=1", "SIP/2.0 100 Trying"))
				pace(500 * time.Millisecond)
				second := time.Now()
				require.Equal(t, sip.StatusOK, p.notify(t, id, "active;expires=2", "SIP/2.0 180 Ringing"))

				r := awaitReferResult(t, out, 5*time.Second)
				require.NoError(t, r.err)
				assert.Equal(t, ReferEndSubscriptionExpired, r.obs.End)
				assert.Equal(t, []int{100, 180}, notifyStatuses(r.obs))
				requireEndedAtItsBound(t, second, r, 2*time.Second)
			})

			t.Run("no NOTIFY after the 202 ends the attempt at Timer N", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				out, refer, from := p.startRefer(t, t.Context())
				accepted := time.Now()
				p.accept(t, refer, from)

				r := awaitReferResult(t, out, 5*time.Second)
				require.NoError(t, r.err)
				assert.Equal(t, ReferEndNoNotify, r.obs.End)
				assert.Empty(t, r.obs.Notifies)
				assert.True(t, r.obs.ResponseReceived)
				assert.Equal(t, sip.StatusAccepted, r.obs.Response.Status)
				requireEndedAtItsBound(t, accepted, r, timerN)
			})

			t.Run("a final NOTIFY ends the attempt after Timer N would have", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				out, refer, from := p.startRefer(t, t.Context())
				id := p.accept(t, refer, from)
				require.Equal(t, sip.StatusOK, p.notify(t, id, "active;expires=60", "SIP/2.0 100 Trying"))
				pace(1500 * time.Millisecond)
				final := time.Now()
				require.Equal(t, sip.StatusOK, p.notify(t, id, "terminated;reason=noresource", "SIP/2.0 200 OK"))

				r := awaitReferResult(t, out, 5*time.Second)
				require.NoError(t, r.err)
				assert.Equal(t, ReferEndFinalNotify, r.obs.End)
				assert.Equal(t, []int{100, 200}, notifyStatuses(r.obs))
				assert.False(t, r.at.Before(final), "the attempt ended before its final NOTIFY")
			})

			t.Run("a progress NOTIFY that terminates the subscription ends it with no final status", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				out, refer, from := p.startRefer(t, t.Context())
				id := p.accept(t, refer, from)
				require.Equal(t, sip.StatusOK, p.notify(t, id, "terminated;reason=noresource", "SIP/2.0 100 Trying"))

				r := awaitReferResult(t, out, 5*time.Second)
				require.NoError(t, r.err)
				assert.Equal(t, ReferEndSubscriptionEnded, r.obs.End)
				assert.Equal(t, []int{100}, notifyStatuses(r.obs))
			})

			t.Run("an active NOTIFY without expires bounds the attempt at 64×T1 from it", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				out, refer, from := p.startRefer(t, t.Context())
				id := p.accept(t, refer, from)
				pace(500 * time.Millisecond)
				sent := time.Now()
				require.Equal(t, sip.StatusOK, p.notify(t, id, "active", "SIP/2.0 100 Trying"))

				r := awaitReferResult(t, out, 5*time.Second)
				require.NoError(t, r.err)
				assert.Equal(t, ReferEndSubscriptionExpired, r.obs.End)
				assert.Equal(t, []int{100}, notifyStatuses(r.obs))
				requireEndedAtItsBound(t, sent, r, timerN)
			})

			t.Run("the caller giving the attempt up ends it at once and its NOTIFYs get 481", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				out, refer, from := p.startRefer(t, ctx)
				id := p.accept(t, refer, from)
				require.Equal(t, sip.StatusOK, p.notify(t, id, "active;expires=60", "SIP/2.0 100 Trying"))
				cancel()
				cancelled := time.Now()

				r := awaitReferResult(t, out, 5*time.Second)
				require.NoError(t, r.err)
				assert.Equal(t, ReferEndCancelled, r.obs.End)
				assert.Less(t, r.at.Sub(cancelled), 500*time.Millisecond, "the attempt outlived its caller")
				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, p.notify(t, id, "terminated;reason=noresource", "SIP/2.0 200 OK"),
					"a NOTIFY for an attempt its caller gave up was accepted")
			})
		})
	}
}

// TestReferAttemptOutlivesTheInviteUsage checks a BYE, the peer's or ours,
// leaves our REFER's subscription running (RFC 5057 section 5.5: "ending an
// invite usage with a BYE does not terminate any active Event: refer
// subscriptions established on that dialog"; RFC 5359 section 2.4): with the
// dialog ended and its media closed, the terminal NOTIFY is answered 200 and
// applied to the attempt.
func TestReferAttemptOutlivesTheInviteUsage(t *testing.T) {
	for _, role := range referRoles {
		t.Run(role.name, func(t *testing.T) {
			t.Parallel()

			t.Run("the transferee's BYE", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				out, refer, from := p.startRefer(t, t.Context())
				id := p.accept(t, refer, from)
				require.Equal(t, sip.StatusOK, p.notify(t, id, "active;expires=60", "SIP/2.0 100 Trying"))
				require.Equal(t, sip.StatusOK, p.bye(t))
				requireMediaClosed(t, p.dialog)

				assert.Equal(t, sip.StatusOK, p.notify(t, id, "terminated;reason=noresource", "SIP/2.0 200 OK"),
					"the outcome after the BYE was refused")
				r := awaitReferResult(t, out, 5*time.Second)
				require.NoError(t, r.err)
				assert.Equal(t, ReferEndFinalNotify, r.obs.End)
				assert.Equal(t, []int{100, 200}, notifyStatuses(r.obs))
			})

			t.Run("our own BYE", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				out, refer, from := p.startRefer(t, t.Context())
				id := p.accept(t, refer, from)
				require.Equal(t, sip.StatusOK, p.notify(t, id, "active;expires=60", "SIP/2.0 100 Trying"))

				hungUp := make(chan error, 1)
				go func() { hungUp <- p.dialog.Hangup(t.Context()) }()
				bye, byeFrom := p.await(t, sip.BYE, 2*time.Second)
				p.write(t, sip.NewResponseFromRequest(bye, sip.StatusOK, "OK", nil), byeFrom)
				select {
				case err := <-hungUp:
					require.NoError(t, err)
				case <-time.After(5 * time.Second):
					require.FailNow(t, "our BYE did not complete")
				}
				require.NoError(t, p.dialog.Media().Close())
				requireMediaClosed(t, p.dialog)

				assert.Equal(t, sip.StatusOK, p.notify(t, id, "terminated;reason=noresource", "SIP/2.0 200 OK"),
					"the outcome after our BYE was refused")
				r := awaitReferResult(t, out, 5*time.Second)
				require.NoError(t, r.err)
				assert.Equal(t, ReferEndFinalNotify, r.obs.End)
				assert.Equal(t, []int{100, 200}, notifyStatuses(r.obs))
			})
		})
	}
}

// unmatchedReports records what OnReferNotifyUnmatched hands over.
type unmatchedReports chan ReferNotify

// watchUnmatched sets the dialog's unmatched-NOTIFY callback. It takes both of
// the media's locks, so a report made under either never completes.
func watchUnmatched(d DialogSession) unmatchedReports {
	reports := make(unmatchedReports, 8)
	m := d.Media()
	m.OnReferNotifyUnmatched(func(n ReferNotify) {
		m.referMu.Lock()
		_ = len(m.referAttempts)
		m.referMu.Unlock()
		m.mu.Lock()
		_ = m.closed
		m.mu.Unlock()
		reports <- n
	})
	return reports
}

func (r unmatchedReports) next(t *testing.T) ReferNotify {
	t.Helper()
	select {
	case n := <-r:
		return n
	case <-time.After(2 * time.Second):
		require.FailNow(t, "the unmatched NOTIFY was not reported")
		return ReferNotify{}
	}
}

func (r unmatchedReports) none(t *testing.T) {
	t.Helper()
	select {
	case n := <-r:
		t.Errorf("a NOTIFY was reported a second time: %+v", n)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestReferNotifyNoSubscriptionHoldsIsReported checks a refer NOTIFY that no
// live attempt holds is answered 481 (RFC 6665 section 4.1.3), never applied,
// and reported once, with its sipfrag status, to the dialog's unmatched-NOTIFY
// callback, outside every lock; on a live dialog and on an ended one still
// held, and with no callback set.
func TestReferNotifyNoSubscriptionHoldsIsReported(t *testing.T) {
	busy := func(id uint32) ReferNotify {
		return ReferNotify{
			Status: 486, Reason: "Busy Here",
			SubscriptionState: "terminated;reason=noresource", HasSubscriptionState: true,
			EventID: id, HasEventID: true,
		}
	}

	for _, role := range referRoles {
		t.Run(role.name, func(t *testing.T) {
			t.Parallel()

			t.Run("a NOTIFY after its attempt's final one", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				reports := watchUnmatched(p.dialog)
				id := p.referToItsFinal(t)

				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, p.notify(t, id, "terminated;reason=noresource", "SIP/2.0 486 Busy Here"))
				assert.Equal(t, busy(id), reports.next(t))
				reports.none(t)
				assert.Equal(t, sip.DialogStateConfirmed, p.dialog.DialogSIP().LoadState(), "the unmatched NOTIFY ended the dialog")
			})

			t.Run("a NOTIFY on an ended dialog still held", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				reports := watchUnmatched(p.dialog)
				id := p.referToItsFinal(t)
				require.Equal(t, sip.StatusOK, p.bye(t))

				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, p.notify(t, id, "terminated;reason=noresource", "SIP/2.0 486 Busy Here"))
				assert.Equal(t, busy(id), reports.next(t))
				reports.none(t)
			})

			t.Run("with no callback set", func(t *testing.T) {
				t.Parallel()
				p := role.dial(t)
				id := p.referToItsFinal(t)

				assert.Equal(t, sip.StatusCallTransactionDoesNotExists, p.notify(t, id, "terminated;reason=noresource", "SIP/2.0 486 Busy Here"))
				assert.Equal(t, sip.DialogStateConfirmed, p.dialog.DialogSIP().LoadState(), "the unmatched NOTIFY ended the dialog")
			})
		})
	}
}

// TestReferSubscriptionBound checks the duration a NOTIFY that did not end the
// subscription gives it: its Subscription-State's expires (RFC 6665 section
// 4.1.3), or 64×T1 when it carries none that parses as delta-seconds, so a
// malformed or missing value never leaves an attempt unbounded.
func TestReferSubscriptionBound(t *testing.T) {
	timerN := 64 * sip.T1
	for _, tt := range []struct {
		name  string
		state string
		has   bool
		want  time.Duration
	}{
		{name: "active with expires", state: "active;expires=60", has: true, want: 60 * time.Second},
		{name: "pending with expires", state: "pending;expires=5", has: true, want: 5 * time.Second},
		{name: "expires among other parameters", state: "active;reason=probation;expires=30", has: true, want: 30 * time.Second},
		{name: "expires in capitals with spaces", state: "active; EXPIRES = 30", has: true, want: 30 * time.Second},
		{name: "an expires of zero", state: "active;expires=0", has: true, want: 0},
		{name: "no expires", state: "active", has: true, want: timerN},
		{name: "an empty expires", state: "active;expires=", has: true, want: timerN},
		{name: "a negative expires", state: "active;expires=-1", has: true, want: timerN},
		{name: "a non-numeric expires", state: "active;expires=soon", has: true, want: timerN},
		{name: "an expires past 32 bits", state: "active;expires=4294967296", has: true, want: timerN},
		{name: "an unknown state without expires", state: "waiting", has: true, want: timerN},
		{name: "no Subscription-State", has: false, want: timerN},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := ReferNotify{Status: 100, SubscriptionState: tt.state, HasSubscriptionState: tt.has}
			assert.Equal(t, tt.want, referSubscriptionBound(n))
		})
	}
}
