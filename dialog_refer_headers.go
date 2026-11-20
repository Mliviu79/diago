package diago

import (
	"errors"
	"strings"

	"github.com/emiago/sipgo/sip"
)

// appendReferHeaders adds to a REFER our Refer-To and Referred-By and then
// every header the caller passed, in the caller's order. A Refer-To or a
// Referred-By among the caller's headers replaces ours; any other header is
// appended as given, so nothing the caller passes is dropped.
//
// Header field names are case-insensitive (RFC 3261 section 7.3.1), so a
// caller's "referred-by" or "REFER-TO" replaces ours just as the canonical
// spelling does. Only the canonical names are matched: "reffered-by" names
// no header and is appended like any other.
//
// A nil header among headers fails the REFER before anything is added.
func appendReferHeaders(req *sip.Request, referTo, referredBy sip.Uri, headers []sip.Header) error {
	for _, h := range headers {
		if h == nil {
			return errors.New("refer header is nil")
		}
	}
	if !namesHeader(headers, "Refer-To") {
		req.AppendHeader(sip.NewHeader("Refer-To", uri2Header(referTo)))
	}
	if !namesHeader(headers, "Referred-By") {
		req.AppendHeader(sip.NewHeader("Referred-By", uri2Header(referredBy)))
	}
	for _, h := range headers {
		req.AppendHeader(h)
	}
	return nil
}

// namesHeader reports whether a header among headers is named name, compared
// case-insensitively.
func namesHeader(headers []sip.Header, name string) bool {
	for _, h := range headers {
		if strings.EqualFold(h.Name(), name) {
			return true
		}
	}
	return false
}
