package recordfs

import (
	"os"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func lockFile(f *os.File) error {
	var overlapped syscall.Overlapped
	r, _, err := lockFileEx.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&overlapped)))
	if r != 0 {
		return nil
	}
	if err == syscall.Errno(33) || err == syscall.Errno(158) {
		return ErrBusy
	}
	return err
}

func syncDirectory(string) error { return nil }
