//go:build unix

package runstore

import (
	"os"
	"syscall"
)

// lockFile takes an exclusive advisory lock on f, blocking until it is free. Closing
// f releases it.
func lockFile(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX) //nolint:gosec // a file descriptor fits an int
		if err != syscall.EINTR {
			return err
		}
	}
}
