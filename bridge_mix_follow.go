// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"time"

	"github.com/emiago/diago/media"
)

// bridgeSameAudio reports whether audio in codec a can be mixed with audio in
// codec b: same sample rate and frame duration.
func bridgeSameAudio(a, b media.Codec) bool {
	return a.SampleRate == b.SampleRate && a.SampleDur == b.SampleDur
}

// hookUnsafe has the bridge told of the media updates of m, which has joined
// it. Must be called with b.mu held.
func (b *BridgeMix) hookUnsafe(m bridgeMixMedia) {
	if b.unhooks == nil {
		b.unhooks = map[bridgeMixMedia]func(){}
	}
	b.unhooks[m] = m.addMediaUpdateHook(func() { b.mediaUpdated(m) })
}

// unhookUnsafe removes the media update hook of a media that has left the
// bridge. Must be called with b.mu held.
func (b *BridgeMix) unhookUnsafe(m bridgeMixMedia) {
	if unhook, ok := b.unhooks[m]; ok {
		unhook()
		delete(b.unhooks, m)
	}
}

// mediaUpdated runs after a media update of a media in the bridge. The mix
// decodes and encodes each media with the codec the media had when the mix
// started, so once the media's codec has changed the mix is restarted, as a
// join or leave restarts it: the media is mixed with its new codec, or taken
// out of the bridge when its audio no longer matches the mix.
func (b *BridgeMix) mediaUpdated(m bridgeMixMedia) {
	b.mu.Lock()
	defer b.mu.Unlock()
	mixedWith, mixed := b.streamCodecs[m]
	if !mixed {
		// No mix runs with this media. The next one reads its codec anew.
		return
	}
	codec := bridgeMixCodec(m)
	if codec == mixedWith {
		return
	}

	b.log.Debug("Dialog codec changed, restarting mix", "codec", codec.String())
	if err := b.mixStopWait(); err != nil {
		// As at a leave, the mix restarts on every media regardless.
		b.log.Warn("Stopping the mix for a media update failed", "error", err)
	}
	b.mixStart()
}

// mixEnded runs when a mix loop exits. A loop that ended on its own goes from
// running to idle, and its readers may still be reading the medias until the
// next join or leave stops them. A loop stopped by mixStopWait stays in the
// stopping state, which the stopping caller clears once mixWG has drained, so
// no other caller starts a mix and adds to mixWG while that caller is still
// waiting.
func (b *BridgeMix) mixEnded() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mixState == 1 {
		b.stateWriteUnsafe(0)
	}
}

// armDirectRead gives the next direct read of a stream a deadline one
// millisecond away, on its media's current media session. It sets it under the
// bridge lock and only while the mix runs, so it never replaces the deadline
// mixStop sets to stop the mix, and returns false once a stop has begun. A
// deadline that can not be set leaves the read to report what is wrong with the
// connection.
func (b *BridgeMix) armDirectRead(s *bridgePCMStream) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.mixState != 1 {
		return false
	}
	if err := s.media.StopRTP(1, time.Millisecond); err != nil {
		bridgeTrace("direct read: deadline not set", "error", err, "stream.id", s.id)
	}
	return true
}

// readDirect reads the stream once armDirectRead has set its deadline. A
// re-INVITE that rebinds the media closes the socket that deadline is on, and
// the packet reader carries a read waiting there over to the new socket, which
// has no deadline. A read still waiting once its deadline has passed is
// therefore armed again, on the media's current media session, every two
// milliseconds until it returns. The arming has stopped when readDirect
// returns.
func (b *BridgeMix) readDirect(s *bridgePCMStream) (int, error) {
	readDone := make(chan struct{})
	rearmed := make(chan struct{})
	rearm := time.AfterFunc(2*time.Millisecond, func() {
		defer close(rearmed)
		ticker := time.NewTicker(2 * time.Millisecond)
		defer ticker.Stop()
		for {
			b.armDirectRead(s)
			select {
			case <-readDone:
				return
			case <-ticker.C:
			}
		}
	})
	n, err := s.r.Read(s.buf)
	close(readDone)
	if !rearm.Stop() {
		<-rearmed
	}
	return n, err
}
