// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/diago/media"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type OnReferDialogFunc func(referDialog *DialogClientSession) error

// DialogServerSession represents inbound channel
type DialogServerSession struct {
	*sipgo.DialogServerSession

	// MediaSession *media.MediaSession
	DialogMedia

	onReferDialog OnReferDialogFunc

	mediaConf MediaConfig

	// sessionTimers is the RFC 4028 policy stamped from dg.sessionTimers at
	// construction. It is consumed by the answer path.
	sessionTimers SessionTimerPolicy
	// timerAnswer holds the negotiated timer outcome for this answer, nil until a
	// timer-supporting peer is negotiated. Guarded by d.mu.
	timerAnswer *timerAnswer
	// timerOnce guards the refresh loop and watchdog launch so a re-answer or an
	// inbound refresh can never spawn a second goroutine.
	timerOnce sync.Once
	// watchdog is the armed peer-refresh watchdog, set when the peer is the
	// refresher and reset on an inbound refresh re-INVITE. Guarded by d.mu.
	watchdog *peerRefreshWatchdog
	// answering is set by an answer before it sends its 2xx, and closed when
	// that answer returns: after the ACK is read and, for an answer that sets
	// up its media after the ACK, after that too. awaitAnswer waits on it.
	// Guarded by d.mu.
	answering chan struct{}
	// ackAnswerErr is why the answer an ACK carried to the offer in our 2xx
	// could not be applied or keyed. ReadAck sets it before the ACK confirms
	// the dialog, for the answer waiting on that ACK to report. Guarded by
	// d.mu.
	ackAnswerErr error
	// earlyAnswerSDP is the answer the 183 of ProgressMedia carried, which the
	// 200 repeats: RFC 3261 section 13.2.1 has the answer placed in a
	// provisional response be that same exact answer. LocalSDP would change it,
	// at least in its o= version, and cannot read the media session while a
	// handshake ProgressMedia left running uses it. Guarded by d.mu.
	earlyAnswerSDP []byte
	// earlyUnkeyed is set when ProgressMedia returned ErrEarlyMediaNotKeyed.
	// Guarded by d.mu.
	earlyUnkeyed bool

	// terminatingBye holds the BYE request that ended this dialog, captured by
	// ReadBye for read-only inspection. Without it the BYE is unrecoverable:
	// ReadBye answers 200 and drops the request, so an application observes the
	// ending only as a Context() cancel and cannot see a cause the peer stated
	// for itself, such as an RFC 3326 Reason header.
	//
	// nil until a remote BYE arrives. A local Hangup, a transport death or a
	// cancel all leave it nil, which is the truthful "the peer stated no cause".
	terminatingBye atomic.Pointer[sip.Request]

	closed atomic.Uint32
}

// TerminatingBye returns the BYE request that ended this dialog, or nil when the
// dialog was not ended by a remote BYE or has not ended yet. The request is
// read-only; callers must not mutate it.
//
// Safe to call from any goroutine. The store happens before the dialog moves to
// DialogStateEnded, so an observer that reacts to Context() being done sees it.
func (d *DialogServerSession) TerminatingBye() *sip.Request {
	return d.terminatingBye.Load()
}

func (d *DialogServerSession) Id() string {
	return d.ID
}

func (d *DialogServerSession) Close() error {
	if !d.closed.CompareAndSwap(0, 1) {
		return nil
	}
	e1 := d.DialogMedia.Close()
	e2 := d.DialogServerSession.Close()
	return errors.Join(e1, e2)
}

func (d *DialogServerSession) FromUser() string {
	return d.InviteRequest.From().Address.User
}

// User that was dialed
func (d *DialogServerSession) ToUser() string {
	return d.InviteRequest.To().Address.User
}

func (d *DialogServerSession) Transport() string {
	return d.InviteRequest.Transport()
}

func (d *DialogServerSession) Trying() error {
	return d.Respond(sip.StatusTrying, "Trying", nil)
}

// Progress sends 100 trying.
//
// Deprecated: Use Trying. It will change behavior to 183 Sesion Progress in future releases
func (d *DialogServerSession) Progress() error {
	return d.Respond(sip.StatusTrying, "Trying", nil)
}

// ProgressMedia sends 183 Session Progress and creates early media
//
// For DTLS-SRTP it returns once the caller has keyed the early media, or
// ErrEarlyMediaNotKeyed when the caller has not within
// ProgressMediaOptions.KeyTimeout.
//
// Experimental: Naming of API might change
func (d *DialogServerSession) ProgressMedia() error {
	return d.ProgressMediaOptions(ProgressMediaOptions{})
}

type ProgressMediaOptions struct {
	// Codecs that will be used
	Codecs []media.Codec

	// RTPNAT exposes MediaSession property
	RTPNAT int

	// KeyTimeout bounds how long a DTLS-SRTP ProgressMedia waits for the
	// caller to key the early media, by completing the ICE connectivity checks
	// and the DTLS handshake with us, before it returns ErrEarlyMediaNotKeyed.
	// Zero means five seconds, for the reasons defaultEarlyMediaKeyTimeout
	// gives. It bounds the wait only: the handshake goes on, for Answer to
	// complete after the 200.
	KeyTimeout time.Duration
}

// defaultEarlyMediaKeyTimeout is how long ProgressMedia waits for a caller to
// key DTLS-SRTP early media when ProgressMediaOptions leaves it unset.
//
// A caller that takes the answer in the 183 keys it within a few round trips:
// a DTLS 1.2 handshake with a cookie exchange is three, about a second on a 300
// ms path, and ICE adds its checks ahead of it. Five seconds also covers two
// lost flights, whose retransmission timers start at one second and double
// (RFC 6347 section 4.2.4.1). A caller that ignores the 183 (RFC 3960) never
// keys it, and costs the application this long before it learns early media is
// not available. Too short a bound costs only the early media, never the call,
// since the handshake is not abandoned with the wait.
const defaultEarlyMediaKeyTimeout = 5 * time.Second

// ErrEarlyMediaNotKeyed is returned by ProgressMedia when the caller did not key
// DTLS-SRTP early media within ProgressMediaOptions.KeyTimeout, most likely
// because it ignores early media (RFC 3960) and takes part in the DTLS
// handshake only once the call is answered.
//
// The call stays answerable. The dialog has no early media, so no audio reader
// or writer, and its media session refuses to send with media.ErrDTLSNotKeyed;
// nothing may use it before Answer. Answer sends the 200 and then completes the
// handshake ProgressMedia started, bounded by the call and by
// media.DTLSHandshakeTimeout, before it sets the media up. When that handshake
// has failed by then, Answer declines the call 488 instead.
var ErrEarlyMediaNotKeyed = errors.New("early media not keyed: the caller has not completed the DTLS handshake")

func (d *DialogServerSession) ProgressMediaOptions(opt ProgressMediaOptions) error {
	d.updateMediaConf(opt.Codecs, opt.RTPNAT)
	if err := d.initMediaSessionFromConf(d.mediaConf); err != nil {
		return err
	}
	sess := d.mediaSession
	offer := d.InviteRequest.Body()
	if offer == nil {
		return fmt.Errorf("no sdp present in INVITE")
	}
	if err := sess.RemoteSDP(offer); err != nil {
		return err
	}

	headers := []sip.Header{sip.NewHeader("Content-Type", "application/sdp")}
	body := sess.LocalSDP()
	if err := d.DialogServerSession.Respond(183, "Session Progress", body, headers...); err != nil {
		return err
	}

	// The 183 carries our answer. A caller that treats it as the answer (RFC
	// 3261 section 13.2.1) keys the early media with us: we take setup:active
	// and start the DTLS handshake at once (RFC 5763 section 6.2), and no media
	// may go out before it completes (RFC 5764 section 5.1). A caller may also
	// ignore early media (RFC 3960) and take part only after the 200, so the
	// wait is bounded by KeyTimeout, and by the dialog when the caller gives
	// up.
	//
	// The handshake is not abandoned with the wait. The ClientHellos already
	// sent wait on the caller's socket until its DTLS stack reads them after
	// the 200: it completes this association from them, and fails a new one
	// they are read ahead of. Answer waits for it instead.
	keyTimeout := opt.KeyTimeout
	if keyTimeout <= 0 {
		keyTimeout = defaultEarlyMediaKeyTimeout
	}
	if err := sess.FinalizeWithin(d.Context(), keyTimeout); err != nil {
		if errors.Is(err, media.ErrFinalizeInProgress) {
			d.mu.Lock()
			d.earlyAnswerSDP = body
			d.earlyUnkeyed = true
			d.mu.Unlock()
			return ErrEarlyMediaNotKeyed
		}
		return err
	}

	rtpSess := media.NewRTPSession(sess)
	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	d.earlyAnswerSDP = body
	d.mu.Unlock()
	return rtpSess.MonitorBackground()
}

func (d *DialogServerSession) Ringing() error {
	return d.Respond(sip.StatusRinging, "Ringing", nil)
}

func (d *DialogServerSession) DialogSIP() *sipgo.Dialog {
	return &d.Dialog
}

func (d *DialogServerSession) RemoteContact() *sip.ContactHeader {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.remoteContactTarget != nil {
		return d.remoteContactTarget
	}
	return d.InviteRequest.Contact()
}

// statusSessionIntervalTooSmall is the RFC 4028 §6 rejection for an offered
// Session-Expires below the Min-SE floor. sipgo carries no constant for it.
const statusSessionIntervalTooSmall = 422

// errSessionIntervalTooSmall aborts the answer when the offered Session-Expires
// is below the Min-SE floor. A 422 has already been sent to the peer.
var errSessionIntervalTooSmall = errors.New("session interval too small")

// timerAnswer is the negotiated RFC 4028 outcome for a single answer: the
// headers to append to the 200 OK and the parameters for the refresh loop or the
// peer-refresh watchdog. It is computed before RespondSDP and consumed once the
// 200 OK is sent.
type timerAnswer struct {
	headers     []sip.Header
	interval    time.Duration
	weRefresh   bool
	armWatchdog bool
	expiry      time.Duration
}

// buildTimerAnswer is the pure UAS answer-path decision for RFC 4028 session
// timers. Given the offered INVITE and our policy it returns the headers to add
// to the 200 OK (or the Min-SE header for a 422) and the response status. It
// keeps every SIP-transaction dependency out so the decision is unit testable
// without a PBX.
//
//   - timers disabled, or the peer offered no Session-Expires (timer-unaware) →
//     no headers, status 200, nil answer: a graceful no-op, never a rejection.
//   - offered Session-Expires below the floor → status 422 plus Min-SE, nil
//     answer, so no media, no loop and no watchdog.
//   - otherwise → Session-Expires with the honored refresher plus Supported:
//     timer (never Require: timer), and an answer that either refreshes or arms
//     the watchdog.
func buildTimerAnswer(invite *sip.Request, policy SessionTimerPolicy) (ta *timerAnswer, hdrs []sip.Header, status int) {
	if !policy.Enabled {
		return nil, nil, sip.StatusOK
	}

	offer, ok := parseSessionTimerOffer(invite)
	if !ok {
		// Timer-unaware peer: answer normally with no timer headers, start nothing.
		return nil, nil, sip.StatusOK
	}

	dec := negotiate(offer.SE, offer.MinSE, offer.Refresher, policy, refresherUAS)

	if dec.BelowFloor {
		// RFC 4028 §6: reject below-floor and echo our Min-SE, establishing nothing.
		return nil, []sip.Header{sip.NewHeader("Min-SE", secondsOf(dec.Negotiated))}, statusSessionIntervalTooSmall
	}

	seValue := secondsOf(dec.Negotiated)
	if dec.Refresher != "" {
		seValue += ";refresher=" + dec.Refresher
	}
	hdrs = []sip.Header{
		sip.NewHeader("Session-Expires", seValue),
		sip.NewHeader("Supported", "timer"),
	}
	// We refresh when we are the elected refresher, otherwise the peer refreshes
	// and we arm a watchdog to bound a peer that stops refreshing.
	return &timerAnswer{
		headers:     hdrs,
		interval:    dec.Interval,
		weRefresh:   dec.WeRefresh,
		armWatchdog: !dec.WeRefresh,
		expiry:      dec.Negotiated,
	}, hdrs, sip.StatusOK
}

// prepareSessionTimers negotiates RFC 4028 session timers for the answer. On a
// below-floor offer it sends 422 with Min-SE and returns
// errSessionIntervalTooSmall so the caller aborts before any media, loop or
// watchdog. Otherwise it stashes the timer headers, which RespondSDP appends to
// the 200 OK, along with the launch parameters. A timer-unaware peer or a
// disabled policy leaves d.timerAnswer nil, a graceful no-op.
func (d *DialogServerSession) prepareSessionTimers() error {
	ta, hdrs, status := buildTimerAnswer(d.InviteRequest, d.sessionTimers)
	if status == statusSessionIntervalTooSmall {
		if err := d.Respond(statusSessionIntervalTooSmall, "Session Interval Too Small", nil, hdrs...); err != nil {
			return err
		}
		return errSessionIntervalTooSmall
	}
	if ta == nil {
		return nil
	}
	d.mu.Lock()
	d.timerAnswer = ta
	d.mu.Unlock()
	return nil
}

// launchSessionTimers starts the RFC 4028 timer for this dialog exactly once.
// When we are the elected refresher it runs the refresh loop, re-INVITEing at
// half the negotiated interval. Otherwise the peer refreshes and it arms a
// watchdog that hangs up on a missed refresh. Both run on d.Context() so dialog
// teardown cancels them, and the grace comes from the policy via watchdogGrace.
func (d *DialogServerSession) launchSessionTimers(ta *timerAnswer) {
	d.timerOnce.Do(func() {
		switch {
		case ta.weRefresh:
			// Fire and forget: the loop's lifecycle is the dialog context, so it exits
			// on Close or Bye. Its escalation error is not propagated here because
			// there is no per-dialog error channel on the answer path, and a stalled
			// refresh is independently bounded by the peer's own session timer.
			go sessionRefreshLoop(d.Context(), ta.interval, d.ReInvite) //nolint:errcheck // see comment above
		case ta.armWatchdog:
			w := armPeerRefreshWatchdog(d.Context(), ta.expiry, watchdogGrace(d.sessionTimers), d.Hangup)
			d.mu.Lock()
			d.watchdog = w
			d.mu.Unlock()
		}
	})
}

// RespondSDP answers the INVITE with a 2xx carrying body and waits for its
// ACK. An in-dialog request that arrives meanwhile is handled once it returns,
// see awaitAnswer.
func (d *DialogServerSession) RespondSDP(body []byte) error {
	d.mu.Lock()
	answering := make(chan struct{})
	d.answering = answering
	d.mu.Unlock()
	defer close(answering)

	return d.respondSDP(body)
}

// respondSDP is RespondSDP for an answer that has marked itself in progress,
// because it sets up media after the ACK and must keep in-dialog requests
// waiting until that is done too.
func (d *DialogServerSession) respondSDP(body []byte) error {
	headers := []sip.Header{sip.NewHeader("Content-Type", "application/sdp")}

	d.mu.Lock()
	ta := d.timerAnswer
	d.mu.Unlock()
	if ta != nil {
		headers = append(headers, ta.headers...)
	}

	if err := d.DialogServerSession.Respond(200, "OK", body, headers...); err != nil {
		return err
	}

	// The ACK carried the answer to an offer in our 2xx, and it failed. ReadAck
	// left ending the call to this answer, which reports why.
	d.mu.Lock()
	ackErr := d.ackAnswerErr
	d.mu.Unlock()
	if ackErr != nil {
		return errors.Join(ackErr, d.hangupNoMedia())
	}

	// The 200 OK established the session, so start the refresh loop or arm the
	// peer-refresh watchdog now, exactly once and on the dialog context so
	// teardown cancels it.
	if ta != nil {
		d.launchSessionTimers(ta)
	}
	return nil
}

// Answer creates media session and answers
// After this new AudioReader and AudioWriter are created for audio manipulation
// NOTE: Not final API
func (d *DialogServerSession) Answer() error {
	// RFC 4028: negotiate session timers before the 200 OK. A below-floor offer is
	// rejected here with 422 and aborts the answer. The timer headers are stashed
	// for RespondSDP, which launches the loop or watchdog once the 200 OK is sent.
	if err := d.prepareSessionTimers(); err != nil {
		return err
	}

	// Media Exists as early
	if d.mediaSession != nil {
		return d.answerEarlyMedia()
	}

	if err := d.initMediaSessionFromConf(d.mediaConf); err != nil {
		return err
	}

	rtpSess := media.NewRTPSession(d.mediaSession)
	return d.answerSession(rtpSess)
}

type AnswerOptions struct {
	// OnMediaUpdate triggers when media update happens. It is blocking func, so make sure you exit
	OnMediaUpdate func(d *DialogMedia)

	// OnRefer is called on successfull REFER handling
	//
	// It creates new dialog (NewDialog) on which you need to call Invite() and Ack()
	// Any error from invite, ack or other processing should be returned for correct Notify handling
	//
	// NOTE: IT is SCOPED to handler and exiting handler will Close/Terminate this dialog!
	OnRefer func(referDialog *DialogClientSession) error
	// Codecs that will be used
	Codecs []media.Codec

	// RTPNAT is media.MediaSession.RTPNAT
	// Check media.RTPNAT... options
	RTPNAT int
}

// AnswerOptions allows to answer dialog with options
// Experimental
//
// NOTE: API may change
func (d *DialogServerSession) AnswerOptions(opt AnswerOptions) error {
	d.mu.Lock()
	d.onReferDialog = opt.OnRefer
	d.onMediaUpdate = opt.OnMediaUpdate
	d.mu.Unlock()

	// RFC 4028: mirror Answer and negotiate session timers before the 200 OK.
	if err := d.prepareSessionTimers(); err != nil {
		return err
	}

	// If media exists as early, only respond 200
	if d.mediaSession != nil {
		// Check do codecs match
		return d.answerEarlyMedia()
	}

	d.updateMediaConf(opt.Codecs, opt.RTPNAT)
	if err := d.initMediaSessionFromConf(d.mediaConf); err != nil {
		return err
	}
	rtpSess := media.NewRTPSession(d.mediaSession)
	return d.answerSession(rtpSess)
}

func (d *DialogServerSession) updateMediaConf(codecs []media.Codec, rtpNAT int) {
	// Let override of formats
	conf := &d.mediaConf
	if codecs != nil {
		conf.Codecs = codecs
	}
	conf.rtpNAT = rtpNAT
}

// answerEarlyMedia answers a call whose media session exists already: set up
// by ProgressMedia, whose answer the 200 repeats, or installed by the
// application. It only sends the 200, unless ProgressMedia left the DTLS
// handshake of the early media running.
func (d *DialogServerSession) answerEarlyMedia() error {
	d.mu.Lock()
	sess := d.mediaSession
	earlyAnswer := d.earlyAnswerSDP
	unkeyed := d.earlyUnkeyed
	d.mu.Unlock()

	if unkeyed {
		return d.answerUnkeyedEarlyMedia(sess, earlyAnswer)
	}
	if earlyAnswer == nil {
		earlyAnswer = sess.LocalSDP()
	}
	// This will now block until ACK received with 64*T1 as max.
	return d.RespondSDP(earlyAnswer)
}

// answerUnkeyedEarlyMedia answers a call whose caller did not key the early
// media, which ProgressMedia reported with ErrEarlyMediaNotKeyed. The 200
// repeats the answer in the 183, and once its ACK is read, the handshake
// ProgressMedia left running is waited for, now bounded by the call and by
// media.DTLSHandshakeTimeout: a caller that ignored the 183 takes part in it
// only after the 200. The media is set up once the handshake has keyed it. An
// in-dialog request waits for all of it, as it does for answerSession.
//
// A handshake that has already failed leaves the call no media to answer
// with, so the call is declined 488 rather than answered 200 and ended with a
// BYE: RFC 3261 section 21.4.26 gives 488 the meaning of 606, a session
// description the callee cannot support, and RFC 5763 section 5 has the media
// session torn down on a certificate that does not match its fingerprint.
func (d *DialogServerSession) answerUnkeyedEarlyMedia(sess *media.MediaSession, earlyAnswer []byte) error {
	d.mu.Lock()
	answering := make(chan struct{})
	d.answering = answering
	d.mu.Unlock()
	defer close(answering)

	if err := sess.FinalizeWithin(d.Context(), 0); err != nil && !errors.Is(err, media.ErrFinalizeInProgress) {
		return errors.Join(err, d.Respond(sip.StatusNotAcceptableHere, "Not Acceptable Here", nil))
	}
	if err := d.respondSDP(earlyAnswer); err != nil {
		return err
	}
	if err := sess.FinalizeContext(d.Context()); err != nil {
		return err
	}

	rtpSess := media.NewRTPSession(sess)
	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	d.mu.Unlock()
	// Must be called after media and reader writer is setup
	return rtpSess.MonitorBackground()
}

// answerSession. It allows answering with custom RTP Session.
// NOTE: Not final API
func (d *DialogServerSession) answerSession(rtpSess *media.RTPSession) error {
	sess := rtpSess.Sess
	sdp := d.InviteRequest.Body()
	if sdp == nil {
		return fmt.Errorf("no sdp present in INVITE")
	}

	if err := sess.RemoteSDP(sdp); err != nil {
		return err
	}

	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	answering := make(chan struct{})
	d.answering = answering
	d.mu.Unlock()
	defer close(answering)

	// This will now block until ACK received with 64*T1 as max.
	// How to let caller to cancel this?
	if err := d.respondSDP(sess.LocalSDP()); err != nil {
		return err
	}

	// The handshake ends with the call.
	if err := sess.FinalizeContext(d.Context()); err != nil {
		return err
	}
	// fmt.Println("--------SErver finalized")

	// Must be called after media and reader writer is setup
	return rtpSess.MonitorBackground()
}

// AnswerLate does answer with Late offer.
func (d *DialogServerSession) AnswerLate() error {
	if err := d.initMediaSessionFromConf(d.mediaConf); err != nil {
		return err
	}
	sess := d.mediaSession
	rtpSess := media.NewRTPSession(sess)
	localSDP := sess.LocalSDP()

	d.mu.Lock()
	d.initRTPSessionUnsafe(sess, rtpSess)
	// Close RTP session
	// d.onCloseUnsafe(func() error {
	// 	return rtpSess.Close()
	// })
	answering := make(chan struct{})
	d.answering = answering
	d.mu.Unlock()
	defer close(answering)

	// This will now block until ACK received with 64*T1 as max.
	// How to let caller to cancel this?
	if err := d.respondSDP(localSDP); err != nil {
		return err
	}
	// Must be called after media and reader writer is setup
	return rtpSess.MonitorBackground()
}

// ReadAck reads the ACK to our 2xx. When the 2xx carried an offer, the ACK
// carries the answer (RFC 3261 section 13.2.1). The answer to the offer in the
// 2xx to our INVITE is applied to the media session, not carrying media yet,
// and finalizes it, running the DTLS handshake of a DTLS-SRTP session. The
// answer to the offer in the 2xx to a re-INVITE of the peer's is applied to the
// fork that made the offer, which is then installed, since the media session
// carries the media meanwhile.
//
// An ACK cannot be refused, so an answer that cannot be applied, or a handshake
// that fails, ends the call with a BYE once the ACK has confirmed the dialog:
// RFC 3261 section 13.2.2.4 has a UAC do the same with an offer it cannot
// accept, and RFC 5763 section 5 has the media session torn down on a
// fingerprint mismatch. An answer waiting for this ACK sends the BYE and
// reports the error itself.
func (d *DialogServerSession) ReadAck(req *sip.Request, tx sip.ServerTransaction) error {
	if reInvite, err := d.applyAckAnswer(d.Context(), req); reInvite {
		ackErr := d.DialogServerSession.ReadAck(req, tx)
		d.ackPeerReInvite(req)
		if err != nil {
			return errors.Join(err, ackErr, d.hangupNoMedia())
		}
		d.runMediaUpdateHooks()
		d.mu.Lock()
		onMediaUpdate := d.onMediaUpdate
		d.mu.Unlock()
		if onMediaUpdate != nil {
			onMediaUpdate(&d.DialogMedia)
		}
		return ackErr
	}
	// The ACK to the 2xx to a re-INVITE of the peer's that carried an offer
	// ends the retransmission of that 2xx.
	d.ackPeerReInvite(req)

	// Check do we have some session
	answerWaiting := false
	err := func() error {
		d.mu.Lock()
		defer d.mu.Unlock()
		sess := d.mediaSession
		if sess == nil {
			return nil
		}
		// Only the ACK confirming the dialog answers the offer in the 2xx to
		// our INVITE. A copy of it read later answers nothing more.
		if d.LoadState() != sip.DialogStateEstablished {
			return nil
		}
		contentType := req.ContentType()
		if contentType == nil {
			return nil
		}
		body := req.Body()
		if body != nil && contentType.Value() == "application/sdp" {
			if d.answering != nil {
				select {
				case <-d.answering:
				default:
					answerWaiting = true
				}
			}

			// This is Late offer response
			sess.RemoteSDPIsAnswer = true
			if err := sess.RemoteSDP(body); err != nil {
				d.ackAnswerErr = err
				return err
			}

			// Finalize session. The handshake ends with the call.
			if err := sess.FinalizeContext(d.Context()); err != nil {
				d.ackAnswerErr = err
				return err
			}
		}
		return nil
	}()
	if err != nil {
		if ackErr := d.DialogServerSession.ReadAck(req, tx); ackErr != nil {
			return errors.Join(err, ackErr)
		}
		if answerWaiting {
			return err
		}
		return errors.Join(err, d.hangupNoMedia())
	}

	return d.DialogServerSession.ReadAck(req, tx)
}

// awaitAnswer waits until our answer is complete before an in-dialog request is
// handled, and reports whether it is. Complete means the ACK to our 2xx is read
// and the answer has returned. A peer sends its ACK before any request that
// follows it, but sipgo hands each request to its handler on its own
// goroutine, so a later request can be handled before the ACK, or before the
// answer has finalized and started the media that a re-INVITE would replace.
// Reading the ACK is not enough either with sipgo v1.6.0, which tells the
// dialog's state observers of each transition on the goroutine that makes it,
// one observer after another: a BYE woken by the ACK's notification could end
// the dialog, and have that told to the answer, before the ACK's notification
// reached it, and the answer would then report the ACK missing. A sipgo that
// tells the observers of the transitions in the order they happen cannot
// reorder them so. Waiting restores the order the peer sent them in.
// The wait ends with tx, or after two T1 intervals, which covers an ACK lost
// in transit and resent when our 2xx is first retransmitted.
func (d *DialogServerSession) awaitAnswer(tx sip.ServerTransaction) bool {
	timer := time.NewTimer(2 * sip.T1)
	defer timer.Stop()

	if d.LoadState() == sip.DialogStateEstablished {
		// The state is loaded again after the read is registered, so an ACK
		// read in between is not missed.
		stateCh := d.StateRead()
		for state := d.LoadState(); state == sip.DialogStateEstablished; {
			select {
			case state = <-stateCh:
			case <-tx.Done():
				return false
			case <-timer.C:
				return false
			}
		}
	}

	// Loaded only now: an answer sets it before its 2xx, so once the 2xx is
	// acknowledged it is in place.
	d.mu.Lock()
	answering := d.answering
	d.mu.Unlock()
	if answering == nil {
		return true
	}
	select {
	case <-answering:
		return true
	case <-tx.Done():
		return false
	case <-timer.C:
		return false
	}
}

// ReadBye stashes the terminating BYE and then hands it to the embedded session,
// which owns BYE handling: validation, the 200, and the move to
// DialogStateEnded. The request is recorded here, not acted upon differently,
// but for the answer the embedded session leaves to its caller when it refuses
// a BYE, described below.
//
// A BYE that arrives before our answer is complete is read once it is. The
// wait is for the media: an answer that sets up its media after the ACK,
// finalizing the session and starting its RTP monitor, would otherwise have
// that media closed under it by the BYE's handler. It also has the answer see
// the ACK the peer sent ahead of the BYE, rather than the dialog ending before
// any ACK, which the answer would report as a missing ACK. A BYE that arrives
// while a re-INVITE is being handled does not wait for it: the BYE is answered,
// and the re-INVITE, when it has no final response yet, is answered 487 (RFC
// 3261 section 15.1.2), so the peer never gets a 200 to the re-INVITE after
// the one to its BYE.
//
// The stash must precede the delegation, because the delegate is what ends the
// dialog: storing afterwards would let an observer woken by Context() read nil.
// A rejected BYE is therefore rolled back rather than pre-filtered, which keeps
// validation in one place instead of duplicating the embedded session's rules
// here and letting the copies drift. Such a BYE is briefly visible, but it is
// one no observer can be looking at: only a BYE the delegate accepts ends the
// dialog, and until the dialog ends nothing has cause to read the stash.
//
// The delegate refuses a BYE it finds out of order with
// sipgo.ErrDialogInvalidCseq, and RFC 3261 section 12.2.2 has such a BYE
// rejected with 500. Depending on the sipgo version, the delegate answers it
// 500 itself or leaves the answer to its caller; readByeOnce answers it only
// when the delegate did not, and the error is returned: the dialog goes on.
func (d *DialogServerSession) ReadBye(req *sip.Request, tx sip.ServerTransaction) error {
	d.awaitAnswer(tx)
	d.answerMu.Lock()
	defer d.answerMu.Unlock()
	d.terminatingBye.Store(req)
	if err := readByeOnce(req, tx, d.DialogServerSession.ReadBye); err != nil {
		// Not this dialog's ending: leave no cause planted on it.
		d.terminatingBye.CompareAndSwap(req, nil)
		return err
	}
	return d.terminatePeerReInviteLocked()
}

// Hangup ends the call: it declines one not answered yet with 480, and ends an
// answered one with a BYE. Once our 2xx is sent the INVITE transaction takes no
// other final response, so a call whose 2xx awaits its ACK is ended with a BYE
// too, which Bye sends once the ACK is read or the transaction has timed out
// without one (RFC 3261 section 15). Until then it waits, bounded by ctx.
func (d *DialogServerSession) Hangup(ctx context.Context) error {
	state := d.LoadState()
	if state >= sip.DialogStateEstablished {
		return d.Bye(ctx)
	}
	return d.Respond(sip.StatusTemporarilyUnavailable, "Temporarly unavailable", nil)
}

// ReInvite sends a re-INVITE offering the media session as it is, and installs
// it with the answer. The offer is made by a fork of the media session, which
// takes the answer: a subsequent offer carries a=setup:actpass (RFC 8842
// section 5.5), which the media session in place, whose role is set, would not
// offer. It offers the codecs the call runs on, see negotiatedOffer, as a
// session refresh renegotiates nothing.
func (d *DialogServerSession) ReInvite(ctx context.Context) error {
	return d.reInviteMedia(ctx, func(cur *media.MediaSession) (*media.MediaSession, []byte) {
		m := cur.Fork()
		return m, negotiatedOffer(m, cur)
	})
}

// reInviteMediaSession re-INVITEs with ms, a fork of the media session, and
// installs it with the answer.
func (d *DialogServerSession) reInviteMediaSession(ctx context.Context, ms *media.MediaSession) error {
	return d.reInviteMedia(ctx, func(*media.MediaSession) (*media.MediaSession, []byte) {
		return ms, ms.LocalSDP()
	})
}

// reInviteMedia re-INVITEs with the fork of the installed media session that
// fork returns, and its offer, and installs the fork with the answer. Each attempt waits
// first for a re-INVITE in progress to end, and marks ours in progress, see
// beginOwnMediaUpdate. A 491 is retried after the wait RFC 3261 section 14.1
// gives, with no re-INVITE of ours in progress meanwhile.
func (d *DialogServerSession) reInviteMedia(ctx context.Context, fork func(cur *media.MediaSession) (*media.MediaSession, []byte)) error {
	for {
		end, err := d.beginOwnMediaUpdate(ctx)
		if err != nil {
			return err
		}
		ms, sdp := fork(d.MediaSession())
		pending, err := d.reInviteMediaOnce(ctx, ms, sdp)
		end()
		if !pending {
			return err
		}
		// The caller generated the Call-ID.
		if err := reInviteRetryWait(ctx, false); err != nil {
			d.mu.Lock()
			derr := d.discardForkUnsafe(ms)
			d.mu.Unlock()
			return errors.Join(err, derr)
		}
	}
}

// reInviteMediaOnce sends one re-INVITE with sdp, the offer of ms, and installs
// ms with the answer, unless the dialog media is closed or the dialog has ended
// by then. ms is discarded when it is not installed. It reports a 491, for the
// caller to retry.
func (d *DialogServerSession) reInviteMediaOnce(ctx context.Context, ms *media.MediaSession, sdp []byte) (bool, error) {
	// NOTE: we do not change original invite request
	d.mu.Lock()
	contact := d.remoteContactUnsafe()
	d.mu.Unlock()

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	req.SetBody(sdp)

	res, err := d.reInviteSend(ctx, req)
	if res != nil && res.StatusCode == sip.StatusRequestPending {
		return true, nil
	}
	if err != nil {
		d.mu.Lock()
		derr := d.discardForkUnsafe(ms)
		d.mu.Unlock()
		return false, errors.Join(err, derr)
	}

	// Save new remote target contact and update media
	err = func() error {
		d.mu.Lock()
		defer d.mu.Unlock()
		if d.DialogMedia.closed || d.Context().Err() != nil {
			return errors.Join(errMediaClosed, d.discardForkUnsafe(ms))
		}
		d.remoteContactTarget = res.Contact()

		remoteSDP := res.Body()
		// The 200 OK answers the offer we sent on the re-INVITE.
		ms.RemoteSDPIsAnswer = true
		if err := ms.RemoteSDP(remoteSDP); err != nil {
			return errors.Join(fmt.Errorf("sdp update media remote SDP applying failed: %w", err), d.discardForkUnsafe(ms))
		}

		// The answer completes a new DTLS association, whose handshake runs
		// before ms carries any media.
		if ms.FinalizePending() {
			if err := d.finalizeAndReplaceUnsafe(d.Context(), ms); err != nil {
				if errors.Is(err, errMediaClosed) {
					return err
				}
				return fmt.Errorf("%w: %w", errMediaUpdateAfterAnswer, err)
			}
			return nil
		}
		if err := d.mediaUpdateUnsafe(ms); err != nil {
			return errors.Join(err, d.discardForkUnsafe(ms))
		}
		return nil
	}()
	if errors.Is(err, errMediaUpdateAfterAnswer) {
		return false, errors.Join(err, d.hangupNoMedia())
	}
	if err == nil {
		d.runMediaUpdateHooks()
	}
	return false, err
}

// reInviteSend sends req, a re-INVITE, and acknowledges its 2xx. A final
// response other than 2xx is returned with sipgo.ErrDialogResponse.
func (d *DialogServerSession) reInviteSend(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	res, err := d.Do(ctx, req.Clone())
	if err != nil {
		return nil, err
	}
	if !res.IsSuccess() {
		return res, sipgo.ErrDialogResponse{
			Res: res,
		}
	}

	// Now do ACK on new Contact
	cont := res.Contact()
	if cont == nil {
		return res, fmt.Errorf("reinvite: 2xx without Contact: %w", sipgo.ErrDialogInviteNoContact)
	}
	if err := d.ack(ctx, cont.Address, nil); err != nil {
		return res, err
	}
	return res, nil
}

func (d *DialogServerSession) ack(ctx context.Context, remoteTarget sip.Uri, body []byte) error {
	// inviteRequest := d.InviteRequest
	// recipient := &inviteRequest.Recipient
	// if contact := d.InviteResponse.Contact(); contact != nil {
	// 	recipient = &contact.Address
	// }
	ackRequest := sip.NewRequest(
		sip.ACK,
		remoteTarget,
	)

	if body != nil {
		// This is delayed offer
		ackRequest.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
		ackRequest.SetBody(body)
	}

	if err := d.DialogServerSession.WriteRequest(ackRequest); err != nil {
		return err
	}

	// if err := d.DialogServerSession.WriteAck(ctx, ackRequest); err != nil {
	// 	return err
	// }

	// Now dialog is established and can be add into store
	// if err := DialogsClientCache.DialogStore(ctx, d.ID, d); err != nil {
	// 	return err
	// }
	// d.OnClose(func() error {
	// 	return DialogsClientCache.DialogDelete(context.Background(), d.ID)
	// })
	return nil
}

func (d *DialogServerSession) remoteContactUnsafe() *sip.ContactHeader {
	if d.remoteContactTarget != nil {
		// Invite update can change contact
		return d.remoteContactTarget
	}
	return d.InviteRequest.Contact()
}

// Refer tries todo refer (blind transfer) on call. For more control use ReferOptions
//
// It blocks until the transfer outcome is known: the terminal sipfrag NOTIFY, or
// a 30s deadline. A transfer that did not succeed returns *ReferFailureError.
//
// NOTE: It is expected that after calling this you are hanguping call to send BYE
func (d *DialogServerSession) Refer(ctx context.Context, referTo sip.Uri, headers ...sip.Header) error {
	// cont := d.InviteRequest.Contact()
	// return dialogRefer(ctx, d, cont.Address, referTo, headers...)
	return d.ReferOptions(ctx, referTo, ReferServerOptions{
		Headers: headers,
	})
}

type ReferServerOptions struct {
	Headers []sip.Header
	// OnNotify sends notify status code.
	// Setting this returns from refer as soon as it is accepted, leaving the
	// transfer outcome to the callback.
	OnNotify func(statusCode int)
}

// ReferOptions sends a REFER. With OnNotify set it returns once the REFER is
// accepted and reports outcomes to the callback. Without one it blocks for the
// transfer outcome and returns *ReferFailureError if it did not succeed.
func (d *DialogServerSession) ReferOptions(ctx context.Context, referTo sip.Uri, opts ReferServerOptions) error {
	d.mu.Lock()
	cont := d.remoteContactUnsafe()
	if opts.OnNotify != nil {
		d.onReferNotify = opts.OnNotify
	}
	d.mu.Unlock()
	return dialogRefer(ctx, d, cont.Address, referTo, d.InviteResponse.Contact().Address, opts.OnNotify == nil, opts.Headers...)
}

// ReferAndObserve sends a blind REFER for referTo and returns what the far end
// sent back: the REFER's final response, the status line of each NOTIFY and how
// the wait ended. The wait for a final NOTIFY is bounded by opts.Deadline; a
// terminal NOTIFY that arrives after the wait goes to opts.OnLate. Neither this
// method nor any NOTIFY ends the dialog, so what follows the outcome is the
// caller's decision. Referred-By names this side: the Contact of the 200 OK we
// answered with. It installs no callback on the dialog, so it does not change
// what a later Refer or ReferOptions call reports.
func (d *DialogServerSession) ReferAndObserve(ctx context.Context, referTo sip.Uri, opts ReferObserveOptions) (ReferObservation, error) {
	d.mu.Lock()
	recipient := d.remoteContactUnsafe()
	ourContact := d.InviteResponse.Contact()
	d.mu.Unlock()
	if recipient == nil {
		return ReferObservation{}, fmt.Errorf("refer: dialog has no remote contact")
	}
	if ourContact == nil {
		return ReferObservation{}, fmt.Errorf("refer: dialog has no local contact")
	}
	obs, _, err := dialogReferObserve(ctx, d, recipient.Address, referTo, ourContact.Address, opts)
	return obs, err
}

func (d *DialogServerSession) handleReferNotify(req *sip.Request, tx sip.ServerTransaction) {
	if respondNotifyDialogEnded(d, req, tx) {
		return
	}
	dialogHandleReferNotify(d, req, tx)
}

func (d *DialogServerSession) handleRefer(dg *Diago, req *sip.Request, tx sip.ServerTransaction) {
	// A REFER asks to transfer this call, which an ended dialog no longer has.
	if ended, _ := respondDialogEnded(d, req, tx); ended {
		return
	}

	d.mu.Lock()
	onRefDialog := d.onReferDialog
	d.mu.Unlock()
	if onRefDialog == nil {
		tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
		return
	}

	dialogHandleRefer(d, dg, req, tx, onRefDialog)
}

func (d *DialogServerSession) handleReInvite(req *sip.Request, tx sip.ServerTransaction) (err error) {
	// A re-INVITE that arrives after our 2xx but before its ACK is handled once
	// the answer is complete, as RFC 5407 section 3.1.4 recommends. It cannot be
	// read ahead of the ACK: ReadRequest below moves the remote CSeq past the
	// ACK's, and the ACK would then never confirm the dialog. Nor can its media
	// update run before the answer's media is finalized and started. When the
	// answer does not complete in time, RFC 5407 permits 491, which asks the
	// peer to retry.
	if !d.awaitAnswer(tx) {
		return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil))
	}

	d.requestMu.Lock()
	defer d.requestMu.Unlock()

	// A re-INVITE for an ended dialog is answered 481 without reaching the
	// media. A live one is answered through respondPeerReInvite, or 487 by a
	// BYE that ends the dialog first.
	if answered, err := d.beginPeerReInvite(&d.Dialog, req, tx); answered {
		return err
	}
	// Once a 2xx has been sent, its ACK is waited for, and only then is the
	// re-INVITE no longer in progress. A 2xx whose ACK does not come ends the
	// call (RFC 3261 section 13.3.1.4).
	var endUpdate func()
	defer func() {
		if ackErr := d.endPeerReInvite(tx); ackErr != nil {
			err = errors.Join(err, ackErr, d.hangupNoMedia())
		}
		if endUpdate != nil {
			endUpdate()
		}
	}()

	// NOTE: Calling ReadRequest increases remote CSEQ.
	// We should not call this until dialog is confirmed, otherwise any intermidiate response
	// will have wrong CSEQ
	if err := d.ReadRequest(req, tx); err != nil {
		if errors.Is(err, sipgo.ErrDialogInvalidCseq) {
			// https://datatracker.ietf.org/doc/html/rfc3261#section-14.2
			// 			A UAS that receives a second INVITE before it sends the final
			//    response to a first INVITE with a lower CSeq sequence number on the
			//    same dialog MUST return a 500 (Server Internal Error)  response to the
			//    second INVITE and MUST include a Retry-After header field with a
			//    randomly chosen value of between 0 and 10 seconds.
			res := sip.NewResponseFromRequest(req, sip.StatusInternalServerError, "Internal Server Error", nil)
			res.AppendHeader(sip.NewHeader("Retry-After", strconv.Itoa(rand.IntN(10))))
			_, err := d.respondPeerReInvite(tx, res)
			return err
		}

		_, err := d.respondPeerReInvite(tx, sip.NewResponseFromRequest(req, sip.StatusBadRequest, err.Error(), nil))
		return err
	}

	// RFC 3261 section 14.2: a re-INVITE arriving while one of ours is in
	// progress is answered 491.
	var ok bool
	endUpdate, ok = d.beginPeerMediaUpdate()
	if !ok {
		_, err := d.respondPeerReInvite(tx, sip.NewResponseFromRequest(req, sip.StatusRequestPending, "Request Pending", nil))
		return err
	}

	// RFC 4028: an inbound refresh re-INVITE resets the active timer. When the
	// peer is the refresher its watchdog is rearmed. When we refresh, the loop's
	// ticker self-rearms each tick, so no reset is needed. A plain media-update
	// re-INVITE carries no Session-Expires and is a no-op here.
	d.resetSessionTimerOnRefresh(req)

	if err := d.handleMediaUpdate(d.Context(), req, tx, d.InviteResponse.Contact()); err != nil {
		if errors.Is(err, errMediaUpdateAfterAnswer) {
			return errors.Join(err, d.hangupNoMedia())
		}
		return err
	}
	return nil
}

// hangupNoMedia ends a call left without usable media by an answer that could
// not be applied or by a failed DTLS handshake. The offer/answer exchange was
// complete by then, so the peer has moved to the new session. RFC 5763 section
// 5 has the media session torn down at once on a fingerprint mismatch, and with
// a single audio stream that is the call.
func (d *DialogServerSession) hangupNoMedia() error {
	ctx, cancel := context.WithTimeout(d.Context(), 10*time.Second)
	defer cancel()
	return d.Hangup(ctx)
}

// resetSessionTimerOnRefresh rearms the peer-refresh watchdog when req is an RFC
// 4028 refresh re-INVITE. It is a no-op when timers are disabled, when the
// request carries no Session-Expires, or when we are the refresher and therefore
// have no watchdog armed.
func (d *DialogServerSession) resetSessionTimerOnRefresh(req *sip.Request) {
	if !d.sessionTimers.Enabled {
		return
	}
	if _, ok := parseSessionTimerOffer(req); !ok {
		return
	}
	d.mu.Lock()
	w := d.watchdog
	d.mu.Unlock()
	if w != nil {
		w.Reset()
	}
}

func (d *DialogServerSession) readSIPInfoDTMF(req *sip.Request, tx sip.ServerTransaction) error {
	if ended, err := respondDialogEnded(d, req, tx); ended {
		return err
	}
	// DTMF relay is not read yet, so an INFO on a live dialog is refused
	// whatever it carries, without a body and so without a Content-Type (RFC
	// 3261 section 20.15) too.
	return tx.Respond(sip.NewResponseFromRequest(req, sip.StatusNotAcceptable, "Not Acceptable", nil))
	// if err := d.ReadRequest(req, tx); err != nil {
	// 	tx.Respond(sip.NewResponseFromRequest(req, sip.StatusBadRequest, "Bad Request", nil))
	// 	return
	// }

	// Parse this
	//Signal=1
	// Duration=160
	// reader := bytes.NewReader(req.Body())

	// for {

	// }
}

func (d *DialogServerSession) Hold(ctx context.Context) error {
	return d.reInviteMedia(ctx, func(cur *media.MediaSession) (*media.MediaSession, []byte) {
		m := cur.Fork()
		m.Mode = sdp.ModeSendonly
		return m, m.LocalSDP()
	})
}

func (d *DialogServerSession) Unhold(ctx context.Context) error {
	return d.reInviteMedia(ctx, func(cur *media.MediaSession) (*media.MediaSession, []byte) {
		m := cur.Fork()
		m.Mode = sdp.ModeSendrecv
		return m, m.LocalSDP()
	})
}
