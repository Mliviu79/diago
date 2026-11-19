// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// beginOwnMediaUpdate waits, bounded by ctx, until no re-INVITE is in
// progress on dialog, and marks one of ours in progress until the returned
// func is called. RFC 3261 section 14.1 has a UAC start no re-INVITE while
// another INVITE transaction is in progress in either direction. It returns
// errMediaClosed once dialog has ended, and the cause of ctx when ctx ends
// first.
func (s *sessionState) beginOwnMediaUpdate(ctx context.Context, dialog *sipgo.Dialog) (func(), error) {
	for {
		s.mediaUpdateMu.Lock()
		if dialog.LoadState() == sip.DialogStateEnded {
			s.mediaUpdateMu.Unlock()
			return nil, errMediaClosed
		}
		done := s.mediaUpdateDone
		if done == nil {
			done = make(chan struct{})
			s.mediaUpdateDone = done
			s.mediaUpdateMu.Unlock()
			return func() { s.endMediaUpdate(done) }, nil
		}
		s.mediaUpdateMu.Unlock()

		select {
		case <-done:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
}

// beginPeerMediaUpdate marks a re-INVITE of the peer's in progress until the
// returned func is called, and reports false when one of ours is in progress
// instead, which RFC 3261 section 14.2 has the peer's answered 491. The peer's
// re-INVITEs take turns under requestMu, so only ours can be in progress.
func (s *sessionState) beginPeerMediaUpdate() (func(), bool) {
	s.mediaUpdateMu.Lock()
	defer s.mediaUpdateMu.Unlock()
	if s.mediaUpdateDone != nil {
		return nil, false
	}
	done := make(chan struct{})
	s.mediaUpdateDone = done
	return func() { s.endMediaUpdate(done) }, true
}

// endMediaUpdate ends the re-INVITE that was marked in progress with done, and
// wakes a re-INVITE of ours waiting for it.
func (s *sessionState) endMediaUpdate(done chan struct{}) {
	s.mediaUpdateMu.Lock()
	if s.mediaUpdateDone == done {
		s.mediaUpdateDone = nil
	}
	s.mediaUpdateMu.Unlock()
	close(done)
}
