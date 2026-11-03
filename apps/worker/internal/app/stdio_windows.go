//go:build windows

package app

import (
	"fmt"
	"os"
	"syscall"
)

func isolateStdout() (*os.File, error) {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return nil, err
	}
	var wire syscall.Handle
	if err = syscall.DuplicateHandle(process, syscall.Handle(os.Stdout.Fd()), process, &wire, 0, false, syscall.DUPLICATE_SAME_ACCESS); err != nil {
		return nil, err
	}
	outputHandle := int32(syscall.STD_OUTPUT_HANDLE)
	setStdHandle := syscall.NewLazyDLL("kernel32.dll").NewProc("SetStdHandle")
	ok, _, callErr := setStdHandle.Call(uintptr(outputHandle), os.Stderr.Fd())
	if ok == 0 {
		_ = syscall.CloseHandle(wire)
		return nil, fmt.Errorf("redirect native stdout: %w", callErr)
	}
	os.Stdout = os.Stderr
	return os.NewFile(uintptr(wire), "worker-protocol"), nil
}
