// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diagotest

import (
	"sync"
	"testing"

	"github.com/emiago/sipgo/sip"
	"github.com/stretchr/testify/require"
)

// TestConnRecorderConcurrentWrites writes to a recorder from two goroutines at
// once, as a transaction does from its own goroutine and from its timers.
// Under -race the unguarded append was reported.
func TestConnRecorderConcurrentWrites(t *testing.T) {
	rec := NewConnRecorder()
	req := sip.NewRequest(sip.OPTIONS, sip.Uri{User: "peer", Host: "127.0.0.1"})

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 100 {
				_ = rec.WriteMsg(req)
			}
		}()
	}
	wg.Wait()
	require.Len(t, rec.msgs, 200)
}
