//go:build unix

package telemetry

import (
	"syscall"
	"time"
)

// processCPU is the user plus system CPU time this process has consumed.
func processCPU() (time.Duration, bool) {
	var ru syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &ru); err != nil {
		return 0, false
	}
	return time.Duration(ru.Utime.Nano() + ru.Stime.Nano()), true
}
