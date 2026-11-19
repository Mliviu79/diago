// SPDX-License-Identifier: MPL-2.0

package diago

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/emiago/sipgo/sip"
)

var errInviteRequiresUnsupportedExtension = errors.New("new INVITE rejected with 420 Bad Extension")

// rejectInviteRequiringUnsupportedExtension answers a new INVITE whose Require
// names an option tag diago does not support with 420 Bad Extension and one
// Unsupported header listing those tags, and returns the rejection joined with
// the error of sending it. It returns nil, and answers nothing, when the INVITE
// requires nothing diago lacks.
//
// RFC 3261 section 8.2.2.3: a UAS that does not understand an option tag in
// Require MUST answer 420 listing it in Unsupported. Serving such an INVITE
// regardless gives its sender a call without the extension it made a condition:
// one requiring replaces, an attended transfer naming a dialog, would become a
// second ordinary call beside the one it meant to replace instead of telling
// the sender to fall back.
func rejectInviteRequiringUnsupportedExtension(ctx context.Context, req *sip.Request, tx sip.ServerTransaction, timers SessionTimerPolicy) error {
	tags := unsupportedRequiredExtensions(req, timers)
	if len(tags) == 0 {
		return nil
	}
	unsupported := strings.Join(tags, ", ")
	res := sip.NewResponseFromRequest(req, sip.StatusBadExtension, "Bad Extension", nil)
	res.AppendHeader(sip.NewHeader("Unsupported", unsupported))
	return errors.Join(fmt.Errorf("%w: %s", errInviteRequiresUnsupportedExtension, unsupported), tx.Respond(ctx, res))
}

// unsupportedRequiredExtensions returns the option tags of the request's
// Require headers that diago does not support, in the order they appear. The
// only extension diago supports is timer (RFC 4028), and only while the
// session-timer policy is enabled.
func unsupportedRequiredExtensions(req *sip.Request, timers SessionTimerPolicy) []string {
	var tags []string
	for _, h := range req.GetHeaders("Require") {
		for _, tag := range strings.Split(h.Value(), ",") {
			tag = strings.TrimSpace(tag)
			if tag == "" || (timers.Enabled && strings.EqualFold(tag, "timer")) {
				continue
			}
			tags = append(tags, tag)
		}
	}
	return tags
}
