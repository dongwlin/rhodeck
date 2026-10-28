//go:build linux || darwin

package main

import (
	"os"
	"syscall"
)

// Duplicate the protocol pipe before redirecting fd 1. This also catches native
// constructors, C stdio and C++ streams; changing os.Stdout alone would not.
func isolateStdout() (*os.File, error) {
	fd, err := syscall.Dup(int(os.Stdout.Fd()))
	if err != nil {
		return nil, err
	}
	syscall.CloseOnExec(fd)
	if err := syscall.Dup2(int(os.Stderr.Fd()), int(os.Stdout.Fd())); err != nil {
		_ = syscall.Close(fd)
		return nil, err
	}
	os.Stdout = os.Stderr
	return os.NewFile(uintptr(fd), "worker-protocol"), nil
}
