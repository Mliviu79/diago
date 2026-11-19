// SPDX-License-Identifier: MPL-2.0

package diago

import "errors"

// ErrNoMediaSetup is returned when the dialog's audio is asked for before its
// media is set up, which the answer does, or ProgressMedia for early media.
// Until then the dialog has no media session, or one without a packet reader
// and writer.
var ErrNoMediaSetup = errors.New("no media setup")
