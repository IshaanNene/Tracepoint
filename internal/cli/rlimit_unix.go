//go:build unix

package cli

import (
	"math"
	"syscall"
)

// openFileLimit reports the soft limit on open files.
//
// It matters because every in-flight operation may hold a connection, and a generator
// that runs out of descriptors reports connection errors indistinguishable from a
// target refusing traffic. Better to say so before the run.
func openFileLimit() (int, bool) {
	var lim syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &lim); err != nil {
		return 0, false
	}
	if lim.Cur > uint64(math.MaxInt32) {
		return 0, false // effectively unlimited
	}
	return int(lim.Cur), true
}
