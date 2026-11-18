// SPDX-License-Identifier: MPL-2.0
// SPDX-FileCopyrightText: Copyright (c) 2024, Emir Aganovic

package diago

import "github.com/emiago/sipgo"

// clientSourcePort reports the local UDP port a client built without proof of a
// listener may source requests from: want only while the transport layer already
// listens on it. That client is the one built when the transport loads.
//
// Sourcing from a listened port reuses that connection. Sourcing from any other
// port is a fresh bind, which the OS refuses as soon as another process owns the
// port. The distinction matters because a UA may register before it serves: at
// that moment no listener holds the configured port, so pinning it there turns
// an unrelated process on the same port into a failed REGISTER rather than the
// intended reuse. Returning 0 asks for an ephemeral port, which always binds.
//
// The serve path does not ask it. Depending on the sipgo release, a listener
// can be reported ready before its port is recorded, so the answer there could
// be 0 even though the port is held; the ready callback tells createClient
// instead.
func clientSourcePort(ua *sipgo.UserAgent, want int) int {
	if want == 0 {
		return 0
	}
	// The specific port must be listened on, not merely some port: a UA serving
	// 5060 does not make an unheld 25000 bindable.
	for _, p := range ua.TransportLayer().ListenPorts("udp") {
		if p == want {
			return want
		}
	}
	return 0
}
