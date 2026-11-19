// SPDX-License-Identifier: MPL-2.0

package media

import "fmt"

// connectionLine returns the c= line of an SDP of ours. candidateAddressType
// and candidateIP are the address of the default candidate, which the line
// gives unless Config.ICE.ExternalIP names another: the address a host behind
// a static address mapping is reached at. Only the c= line takes it. The o=
// line and the candidates keep the addresses that were gathered. The caller
// holds m.mu.
func (m *MediaSessionWebrtc) connectionLine(candidateAddressType, candidateIP string) string {
	external := m.Config.ICE.ExternalIP
	if external == nil {
		return fmt.Sprintf("c=IN %s %s", candidateAddressType, candidateIP)
	}
	addressType := "IP6"
	if external.To4() != nil {
		addressType = "IP4"
	}
	return fmt.Sprintf("c=IN %s %s", addressType, external)
}
