// SPDX-License-Identifier: MPL-2.0

package diago

import "slices"

// mediaUpdateHook is one function addMediaUpdateHook registered.
type mediaUpdateHook struct {
	f func()
}

// mediaUpdateHooks are the media update hooks of a dialog's media, guarded by
// the media's lock.
type mediaUpdateHooks []*mediaUpdateHook

// add registers f and returns the hook that remove takes.
func (hs *mediaUpdateHooks) add(f func()) *mediaUpdateHook {
	h := &mediaUpdateHook{f: f}
	*hs = append(*hs, h)
	return h
}

// remove takes h out again.
func (hs *mediaUpdateHooks) remove(h *mediaUpdateHook) {
	*hs = slices.DeleteFunc(*hs, func(x *mediaUpdateHook) bool { return x == h })
}

// run calls each of the hooks a copy holds; the copy is taken under the
// media's lock and run with it released.
func (hs mediaUpdateHooks) run() {
	for _, h := range hs {
		h.f()
	}
}

// addMediaUpdateHook has f called after every update that replaces the media
// session, once the session is in place and with the media's lock released,
// and before the application's OnMediaUpdate. It returns the function that
// removes f again. f runs on the goroutine that handled the update, which a
// re-INVITE may hold until f returns.
//
// The hooks are for the package's own use: a bridge mixing or proxying the
// media's audio has to know when its codec changes, and OnMediaUpdate belongs
// to the application.
func (d *DialogMedia) addMediaUpdateHook(f func()) (remove func()) {
	d.mu.Lock()
	h := d.mediaUpdateHooks.add(f)
	d.mu.Unlock()
	return func() {
		d.mu.Lock()
		d.mediaUpdateHooks.remove(h)
		d.mu.Unlock()
	}
}

// runMediaUpdateHooksUnsafe calls the media update hooks, with the media's
// lock released while they run. It is called where a media session has just
// been put in place of the one in use, so a session that is only staged runs
// none until it is installed.
//
// Must be called with d.mu held, and returns with it held.
func (d *DialogMedia) runMediaUpdateHooksUnsafe() {
	hooks := slices.Clone(d.mediaUpdateHooks)
	if len(hooks) == 0 {
		return
	}
	d.mu.Unlock()
	defer d.mu.Lock()
	hooks.run()
}
