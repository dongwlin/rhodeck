//go:build windows

package native

import (
	"syscall"
	"unsafe"
)

const (
	loadLibrarySearchDLLLoadDir = 0x00000100
	loadLibrarySearchSystem32   = 0x00000800
	windowsLoadFlags            = loadLibrarySearchDLLLoadDir | loadLibrarySearchSystem32
)

var loadLibraryExW = syscall.NewLazyDLL("kernel32.dll").NewProc("LoadLibraryExW")

type windowsPrimitives struct{}

func (windowsPrimitives) open(path string) (uintptr, error) {
	pathUTF16, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	handle, _, callErr := loadLibraryExW.Call(uintptr(unsafe.Pointer(pathUTF16)), 0, windowsLoadFlags)
	if handle == 0 {
		if callErr != syscall.Errno(0) {
			return 0, callErr
		}
		return 0, errNullHandle
	}
	return handle, nil
}

func (windowsPrimitives) lookup(handle uintptr, name string) (uintptr, error) {
	return syscall.GetProcAddress(syscall.Handle(handle), name)
}

func NewLoader() *Loader {
	return newLoader(windowsPrimitives{}, abiProfile)
}
