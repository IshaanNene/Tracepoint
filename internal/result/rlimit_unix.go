//go:build unix

package result

import (
	"math"
	"syscall"
)

// openFileLimit reports the soft limit on open files, which caps how many concurrent
// connections a run can hold. A generator that runs out of descriptors reports
// connection errors that look exactly like a target refusing traffic.
func openFileLimit() (int, bool) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, false
	}
	// RLIM_INFINITY comes back as a huge value; reporting it as a negative int would be
	// worse than reporting nothing.
	if lim.Cur > uint64(math.MaxInt32) {
		return 0, false
	}
	return int(lim.Cur), true
}
