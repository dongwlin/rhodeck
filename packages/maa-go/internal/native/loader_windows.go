//go:build windows

package native

import (
	"syscall"
	"unsafe"
)

// 仅从目标 DLL 所在目录与 System32 解析依赖,不搜索工作目录或 PATH,
// 不修改进程级 DLL 搜索目录。DLL_LOAD_DIR 要求传入绝对路径,
// 由 Loader.Open 在调用前校验。
const (
	loadLibrarySearchDLLLoadDir = 0x00000100
	loadLibrarySearchSystem32   = 0x00000800
	windowsLoadFlags            = loadLibrarySearchDLLLoadDir | loadLibrarySearchSystem32
)

// kernel32 是 Windows KnownDLL;这里只解析系统入口,不按名字加载用户库。
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
	// 成功时 GetLastError 可能仍有旧值,只以非零句柄判断成功。
	return handle, nil
}

func (windowsPrimitives) lookup(handle uintptr, name string) (uintptr, error) {
	return syscall.GetProcAddress(syscall.Handle(handle), name)
}

// NewLoader 构造 Windows 生产加载器,使用 LoadLibraryExW 与 GetProcAddress。
// 库保持加载到进程退出,不调用 FreeLibrary。
func NewLoader() *Loader {
	return newLoader(windowsPrimitives{}, abiProfile)
}
