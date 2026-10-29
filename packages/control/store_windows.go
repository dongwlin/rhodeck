package control

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

// Windows does not support fsync on directory handles through os.File.Sync.
// The record itself is flushed before its atomic replacement.
func syncDirectory(string) error { return nil }
