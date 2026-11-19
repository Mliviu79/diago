// SPDX-License-Identifier: MPL-2.0

package media

import (
	"errors"

	"github.com/pion/ice/v4"
)

// errICELiteWithURLs is what Init reports, inside an ICEError, for a lite
// agent configured with STUN or TURN URLs.
var errICELiteWithURLs = errors.New("ice: a lite agent gathers host candidates only and uses no STUN or TURN servers")

// ICEConfig holds the settings of a WebRTC media session's ICE agent that
// [MediaSessionWebrtcConfig] does not carry itself.
type ICEConfig struct {
	// Lite runs an ICE lite agent (RFC 8445 section 2.5), which gathers host
	// candidates only, says so with a=ice-lite, and takes the controlled role
	// against a full agent, answering its connectivity checks. Suitable for an
	// endpoint on a public address. The zero value runs a full agent.
	//
	// ICEURLs must be empty, since a lite agent gathers no server reflexive
	// and no relayed candidates, and CandidateTypes must be empty or name host
	// candidates only: [MediaSessionWebrtc.Init] refuses anything else with
	// an *ICEError that matches ErrICEGathering.
	Lite bool
}

// appendICEConfigOptions returns opts followed by the agent options conf.ICE
// asks for. They go last, since a lite agent narrows the candidate types the
// options before them chose.
func appendICEConfigOptions(opts []ice.AgentOption, conf MediaSessionWebrtcConfig) ([]ice.AgentOption, error) {
	if !conf.ICE.Lite {
		return opts, nil
	}
	// Refused here, since the ICE stack's own refusal of the URLs names
	// neither them nor the lite agent.
	if len(conf.ICEURLs) > 0 {
		return nil, errICELiteWithURLs
	}
	opts = append(opts, ice.WithICELite(true))
	if len(conf.CandidateTypes) == 0 {
		// A lite agent has host candidates only (RFC 8445 section 2.5).
		// Without a list every type is gathered, which the ICE stack refuses
		// for one. A list the caller wrote is left to that refusal.
		opts = append(opts, ice.WithCandidateTypes([]ice.CandidateType{ice.CandidateTypeHost}))
	}
	return opts, nil
}

// iceLiteLines returns the session-level lines of our SDP that say which kind
// of ICE agent we run: a=ice-lite from a lite agent, and none from a full
// one, which never writes it (RFC 8839 section 4.2.1.4). The attribute is
// session level only (section 5.3), so the lines belong before the first m=
// line. The caller holds m.mu.
func (m *MediaSessionWebrtc) iceLiteLines() []string {
	if !m.Config.ICE.Lite {
		return nil
	}
	return []string{"a=ice-lite"}
}

// iceControlling reports whether our agent takes the controlling role in the
// connectivity checks of this ICE generation, by RFC 8445 section 6.1.1:
// against a lite agent the full one controls, since a lite agent never sends
// checks, and otherwise the offerer, the agent that started ICE, does.
// offered says the remote description answers an offer of ours. The caller
// holds m.mu.
//
// Two lite agents send no checks by that section. The ICE stack runs them for
// a lite agent that is controlling, which only a lite peer makes it, and the
// lite peer answers them, so the pair is found all the same.
func (m *MediaSessionWebrtc) iceControlling(offered bool) bool {
	if lite := m.Config.ICE.Lite; lite != m.remoteICELite {
		return !lite
	}
	return offered
}
