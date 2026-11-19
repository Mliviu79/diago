// SPDX-License-Identifier: MPL-2.0

package media

import (
	"runtime"
	"strings"
)

// goroutineStatus returns the status a stack dump prints for the goroutine
// whose stack contains fn, such as "select" or "runnable", or "" if no
// goroutine's does.
func goroutineStatus(fn string) string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, fn) {
			continue
		}
		header, _, _ := strings.Cut(g, "\n")
		_, status, _ := strings.Cut(header, "[")
		status, _, _ = strings.Cut(status, "]")
		return status
	}
	return ""
}
