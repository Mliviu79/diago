// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import (
	"context"
	"log/slog"
	"net"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/emiago/sipgo/sip"
)

// connRecorder is a connection that records the messages written to it.
// Transactions write from goroutines of their own, such as their timers, so
// the messages are guarded by mu.
type connRecorder struct {
	mu   sync.Mutex
	msgs []sip.Message

	ref atomic.Int32
}

func NewConnRecorder() *connRecorder {
	return &connRecorder{}
}

func (c *connRecorder) LocalAddr() net.Addr {
	return nil
}

func (c *connRecorder) WriteMsg(msg sip.Message) error {
	c.mu.Lock()
	c.msgs = append(c.msgs, msg)
	c.mu.Unlock()
	return nil
}

// messages returns the messages written so far.
func (c *connRecorder) messages() []sip.Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.msgs)
}

func (c *connRecorder) Ref(i int) int {
	return int(c.ref.Add(int32(i)))
}
func (c *connRecorder) TryClose() (int, error) {
	new := c.ref.Add(int32(-1))
	return int(new), nil
}
func (c *connRecorder) Close() error { return nil }

type clientTxRequester struct {
	// rec *siptest.ClientTxRecorder
	onRequest func(req *sip.Request) *sip.Response
}

func (r *clientTxRequester) Request(ctx context.Context, req *sip.Request) (sip.ClientTransaction, error) {
	key, _ := sip.ClientTxKeyMake(req)
	rec := NewConnRecorder()
	tx := sip.NewClientTx(key, req, rec, slog.Default())
	if err := tx.Init(); err != nil {
		return nil, err
	}

	resp := r.onRequest(req)
	go tx.Receive(resp)

	return tx, nil
}
