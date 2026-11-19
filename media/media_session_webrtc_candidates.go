// SPDX-License-Identifier: MPL-2.0

package media

// skipRemoteCandidate reports that one a=candidate line of a remote
// description is left out of the candidates the ICE agent is given: value is
// the line's value and err is why the ICE stack could not read it.
//
// One unreadable line does not make a description unusable, since the agent
// can still find a pair among the candidates of the other lines. RFC 8839
// section 5.1 requires that of the lines refused for their address: "An agent
// processing remote candidates MUST ignore "candidate" lines that include
// candidates with FQDNs or IP address versions that are not supported or
// recognized". A line that cannot be read for another reason, a candidate type
// defined after this stack was written or a field that is not what the
// grammar asks for, is left out the same way. A description left with no
// candidate at all is still refused by RemoteSDP.
func skipRemoteCandidate(value string, err error) {
	DefaultLogger().Warn("Skipping bad ICE candidate", "attr", "candidate:"+value, "error", err)
}
