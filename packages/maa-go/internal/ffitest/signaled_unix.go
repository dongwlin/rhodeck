//go:build unix

package ffitest

import (
	"os/exec"
	"syscall"
)

func exitSignaled(ee *exec.ExitError) bool {
	ws, ok := ee.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled()
}
