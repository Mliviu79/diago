// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

type RegisterResponseError struct {
	RegisterReq *sip.Request
	RegisterRes *sip.Response

	Msg string
}

func (e *RegisterResponseError) StatusCode() int {
	return e.RegisterRes.StatusCode
}

func (e RegisterResponseError) Error() string {
	return e.Msg
}

type RegisterOptions struct {
	// Digest auth
	Username  string
	Password  string
	ProxyHost string

	// Expiry is for Expire header
	Expiry time.Duration
	// RetryInterval is the interval before the next Register is sent. It can only
	// bring the refresh forward: an interval longer than what the registrar
	// granted is clamped to the grant, otherwise the binding lapses before the
	// refresh fires.
	RetryInterval time.Duration
	AllowHeaders  []string

	OnRegistered func()

	// Useragent default will be used on what is provided as NewUA()
	// UserAgent         string
	// UserAgentHostname string
}

type RegisterTransaction struct {
	opts   RegisterOptions
	Origin *sip.Request

	client *sipgo.Client
	log    *slog.Logger

	// mu guards the registration state below. Diago.Register writes expiry from
	// its own goroutine while the refresh loop reads it.
	mu sync.RWMutex
	// expiry drives the refresh cadence, see calcRetry.
	expiry time.Duration
	// grantedExpires and lastRegistered are written only by a successful
	// REGISTER and describe the binding the registrar actually holds. They are
	// deliberately kept apart from expiry, which Diago.Register repurposes as a
	// Retry-After hint on a 503; mixing the two would make an otherwise live
	// binding look already lapsed.
	grantedExpires time.Duration
	lastRegistered time.Time
	// expiredEmitted latches the escalation so a lapsed binding is reported once
	// per outage rather than on every subsequent refresh.
	expiredEmitted bool
}

func newRegisterTransaction(client *sipgo.Client, recipient sip.Uri, contact sip.ContactHeader, log *slog.Logger, opts RegisterOptions) *RegisterTransaction {
	expiry, allowHDRS := opts.Expiry, opts.AllowHeaders
	// log := p.getLoggerCtx(ctx, "Register")
	req := sip.NewRequest(sip.REGISTER, recipient)
	req.AppendHeader(&contact)

	if opts.ProxyHost != "" {
		req.SetDestination(opts.ProxyHost)
	}
	if expiry > 0 {
		expires := sip.ExpiresHeader(expiry.Seconds())
		req.AppendHeader(&expires)
	}
	if allowHDRS != nil {
		req.AppendHeader(sip.NewHeader("Allow", strings.Join(allowHDRS, ", ")))
	}

	// if opts.Username == "" {
	// 	opts.Username = opts.UserAgent
	// }

	if opts.Username == "" {
		opts.Username = client.Name()
	}

	t := &RegisterTransaction{
		Origin: req, // origin maybe updated after first register
		opts:   opts,
		client: client,
		log:    log.With("caller", "Register"),
	}

	return t
}

func (t *RegisterTransaction) Register(ctx context.Context) error {
	if err := t.register(ctx); err != nil {
		return err
	}

	if t.opts.OnRegistered != nil {
		t.opts.OnRegistered()
	}
	return nil
}
func (t *RegisterTransaction) register(ctx context.Context) error {
	username, password := t.opts.Username, t.opts.Password
	client := t.client
	req := t.Origin
	contact := req.Contact().Clone()

	res, err := func() (*sip.Response, error) {

		contact := req.Contact()
		if contact.Address.Port == 0 && net.ParseIP(contact.Address.Host) != nil {

			sipgo.ClientRequestRegisterBuild(client, req)

			tx, err := client.TransactionLayer().NewClientTransaction(ctx, req)
			if err != nil {
				return nil, err
			}

			host, port, err := sip.ParseAddr(tx.Connection().LocalAddr().String())
			if err != nil {
				return nil, err
			}
			contact.Address.Host = host
			contact.Address.Port = port

			err = tx.Init()
			if err != nil {
				tx.Terminate()
				return nil, err
			}

			for {
				select {
				case res := <-tx.Responses():
					if res.IsProvisional() {
						continue
					}
					return res, nil

				case <-tx.Done():
					return nil, tx.Err()

				case <-ctx.Done():
					return nil, ctx.Err()
				}
			}
		}

		return client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	}()
	if err != nil {
		return fmt.Errorf("fail to create transaction req=%q: %w", req.StartLine(), err)
	}

	via := res.Via()
	if via == nil {
		return fmt.Errorf("no Via header in response")
	}

	// https://datatracker.ietf.org/doc/html/rfc3581#section-9
	if rport, _ := via.Params.Get("rport"); rport != "" {
		if p, err := strconv.Atoi(rport); err == nil {
			contact.Address.Port = p
		}

		if received, _ := via.Params.Get("received"); received != "" {
			// TODO: consider parsing IP
			contact.Address.Host = received
		}

		// Update contact address of NAT
		req.ReplaceHeader(contact)
	}

	if isDigestChallenge(res) {
		res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: username,
			Password: password,
		})
		if err != nil {
			return fmt.Errorf("fail to get response req=%q : %w", req.StartLine(), err)
		}
	}

	if res.StatusCode != 200 {
		return &RegisterResponseError{
			RegisterReq: req,
			RegisterRes: res,
			Msg:         res.StartLine(),
		}
	}

	// Record what the registrar granted, which may be far shorter than what was
	// requested. That grant, not the request, bounds the binding's lifetime.
	t.recordGrant(grantedExpiry(res, t.opts.Expiry))

	return nil
}

func (t *RegisterTransaction) QualifyLoop(ctx context.Context) error {
	return t.reregisterLoop(ctx, t.calcRetry(t.getExpiry()))
}

func (t *RegisterTransaction) reregisterLoop(ctx context.Context, retry time.Duration) error {
	return registerRefreshLoop(ctx, retry, registerRetryBackoffInitial, registerRetryBackoffMax, t)
}

// calcRetry derives the refresh cadence from the expiry the registrar granted,
// so a short grant is refreshed early rather than on a caller chosen wall clock.
//
// RetryInterval used to short circuit this outright: whenever it was set it was
// returned verbatim and the granted expiry was never consulted, so a caller
// asking for 30m against a registrar granting 120s saw the binding lapse long
// before the refresh fired. It now only brings the refresh forward.
func (t *RegisterTransaction) calcRetry(expiry time.Duration) time.Duration {
	calc := expiry.Seconds() * registerRefreshRatio
	retry := time.Duration(calc) * time.Second

	// Allow caller to use own interval, but never past the grant.
	if ri := t.opts.RetryInterval; ri > 0 && (retry <= 0 || ri < retry) {
		retry = ri
	}

	// A zero or absent expiry must not yield a zero interval, a busy loop.
	if retry <= 0 {
		retry = registerRetryMin
	}

	return retry
}

func (t *RegisterTransaction) Unregister(ctx context.Context) error {
	req := t.Origin

	req.RemoveHeader("Expires")
	req.RemoveHeader("Contact")
	req.AppendHeader(sip.NewHeader("Contact", "*"))
	expires := sip.ExpiresHeader(0)
	req.AppendHeader(&expires)
	// Deliberately records no grant: the 200 to a de-registration echoes
	// expires=0, which is the binding going away, not a new lifetime.
	_, err := t.doRequest(ctx, req)
	return err
}

// Qualify refreshes the binding and records the newly granted expiry.
func (t *RegisterTransaction) Qualify(ctx context.Context) error {
	res, err := t.doRequest(ctx, t.Origin)
	if err != nil {
		return err
	}
	// A refresh regrants the binding, so reread the granted expiry every time. A
	// registrar is free to shorten it mid registration.
	t.recordGrant(grantedExpiry(res, t.opts.Expiry))
	return nil
}

func (t *RegisterTransaction) doRequest(ctx context.Context, req *sip.Request) (*sip.Response, error) {
	// log := p.getLoggerCtx(ctx, "Register")
	client := t.client
	username, password := t.opts.Username, t.opts.Password
	// Send request and parse response
	// req.SetDestination(*dst)
	req.RemoveHeader("Via")
	res, err := client.Do(ctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return nil, fmt.Errorf("fail to get response req=%q : %w", req.StartLine(), err)
	}

	if isDigestChallenge(res) {
		res, err = client.DoDigestAuth(ctx, req, res, sipgo.DigestAuth{
			Username: username,
			Password: password,
		})
		if err != nil {
			return nil, fmt.Errorf("fail to get response req=%q : %w", req.StartLine(), err)
		}
	}

	if res.StatusCode != 200 {
		return nil, &RegisterResponseError{
			RegisterReq: req,
			RegisterRes: res,
			Msg:         res.StartLine(),
		}
	}

	return res, nil
}

func getResponse(ctx context.Context, tx sip.ClientTransaction) (*sip.Response, error) {
	select {
	case <-tx.Done():
		return nil, fmt.Errorf("transaction died")
	case res := <-tx.Responses():
		return res, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
