// SPDX-License-Identifier: MPL-2.0

package media

import "github.com/pion/stun/v3"

// UsableICEURLs returns the entries of urls that the ICE agent can take as a
// STUN or TURN server, in their order, and logs a warning for each entry it
// leaves out.
//
// It is for a caller that fills [MediaSessionWebrtcConfig.ICEURLs] from a list
// it did not write, an operator's configuration for one.
// [MediaSessionWebrtc.Init] refuses a configuration for a single URL it cannot
// parse, so one mistyped entry would end every call, while a session without
// that server still gathers its host candidates and those the other servers
// give it.
func UsableICEURLs(urls []string) []string {
	var usable []string
	for _, rawURL := range urls {
		if _, err := stun.ParseURI(rawURL); err != nil {
			DefaultLogger().Warn("Skipping bad ICE server URL", "url", rawURL, "error", err)
			continue
		}
		usable = append(usable, rawURL)
	}
	return usable
}
