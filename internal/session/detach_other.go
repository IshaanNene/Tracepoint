//go:build !unix && !windows

package session

import "syscall"

func detachAttrs() *syscall.SysProcAttr { return nil }
