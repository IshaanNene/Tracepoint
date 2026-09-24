//go:build unix

package session

import "syscall"

// detachAttrs starts the child in its own session, so closing the terminal or ending
// the parent's process group does not take the run with it.
func detachAttrs() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setsid: true} }
