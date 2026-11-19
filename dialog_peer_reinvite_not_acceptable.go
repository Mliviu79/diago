// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
)

// notAcceptableOffers are the classes of error the media of a call refuses a
// session description with when it has read the description and cannot take
// it: the description and our configuration have no overlap, or it asks for
// what the running session cannot do. Each has the Warning that tells the peer
// why (RFC 3261 section 20.43): the warn-code that means that class, and 399,
// the miscellaneous one, where no code does. The warn-text is fixed, so
// nothing of the error's own text reaches the peer.
var notAcceptableOffers = []struct {
	class    error
	warnCode int
	warnText string
}{
	{class: media.ErrNoCommonCodec, warnCode: 305, warnText: "Incompatible media format"},
	{class: media.ErrNoCommonMedia, warnCode: 399, warnText: "No usable audio media description"},
	{class: media.ErrNoCommonCrypto, warnCode: 399, warnText: "No common SRTP keying"},
	{class: media.ErrWebRTCICERestartRequired, warnCode: 399, warnText: "A new DTLS association needs an ICE restart"},
}

// notAcceptableHere returns the response that refuses req, a re-INVITE of the
// peer's, when mediaErr says the media cannot take its session description,
// and false for an error of any other kind. RFC 3261 section 14.2:
//
//	If the new session description is not acceptable, the UAS can reject
//	it by returning a 488 (Not Acceptable Here) response for the re-
//	INVITE.  This response SHOULD include a Warning header field.
//
// The warn-agent is the host the peer sent req to: section 14.1 has the
// Request-URI of a re-INVITE identify the UA instance the dialog was
// established with, which is the one adding the Warning.
func notAcceptableHere(req *sip.Request, mediaErr error) (*sip.Response, bool) {
	for _, offer := range notAcceptableOffers {
		if !errors.Is(mediaErr, offer.class) {
			continue
		}
		agent := req.Recipient.Host
		if req.Recipient.Port > 0 {
			agent += ":" + strconv.Itoa(req.Recipient.Port)
		}
		res := sip.NewResponseFromRequest(req, sip.StatusNotAcceptableHere, "Not Acceptable Here", nil)
		res.AppendHeader(sip.NewHeader("Warning", fmt.Sprintf("%d %s %q", offer.warnCode, agent, offer.warnText)))
		return res, true
	}
	return nil, false
}
