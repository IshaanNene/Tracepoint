//go:build unix

package runstore

import (
	"errors"
	"syscall"
)

// processAlive reports whether a process exists. Signal 0 checks without delivering
// anything; EPERM means it exists but belongs to someone else.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
