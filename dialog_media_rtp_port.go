// SPDX-License-Identifier: MPL-2.0

package diago

// takeRTPPortReleaseUnsafe takes the release of the media's RTP port for the
// Close that latched the media closed, and returns what that Close runs last:
// it hands the port back to the allocator that chose it, when one did, and
// then lets every Close made meanwhile return.
//
// The returned function runs after the sockets are closed, never before. An
// allocator may hold the port for a drain window so late RTP from this call
// can not land on the next call socket, and that window has to start when the
// wire is actually down. The closed latch makes the release exactly once.
// Must be called with d.mu held.
func (d *DialogMedia) takeRTPPortReleaseUnsafe() func() {
	done := make(chan struct{})
	d.closeDone = done
	release := d.releaseRTPPort
	d.releaseRTPPort = nil
	return func() {
		defer close(done)
		if release != nil {
			release()
		}
	}
}
