//go:build !linux && !darwin && !windows

// Linux、macOS 与 Windows 以外的平台没有原生 loader 实现。
// 该入口返回包装 ErrUnsupported 的结构化错误,不进入系统 loader。

package native

import (
	"fmt"
	"runtime"
)

// unsupportedPrimitives 是其他平台的占位原语:两个方法都拒绝
// 服务并声明 loader 未实现,不假装成功、不静默降级。
type unsupportedPrimitives struct{}

// open 拒绝打开任何库:当前平台的 loader 未实现。
func (unsupportedPrimitives) open(path string) (uintptr, error) {
	return 0, fmt.Errorf("native loader is not implemented for %s/%s: %w",
		runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

// lookup 拒绝查找任何符号:当前平台的 loader 未实现。生产路径
// 不会到达这里(open 已拒绝),方法只为满足 loaderPrimitives 接口。
func (unsupportedPrimitives) lookup(handle uintptr, name string) (uintptr, error) {
	return 0, fmt.Errorf("native loader is not implemented for %s/%s: %w",
		runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

// NewLoader 构造其他平台的生产加载器入口:经 Loader.Open 的
// 既有包装,任何加载请求都会得到可识别的 *LibraryLoadError,其
// 错误链可经 errors.Is 识别 ErrUnsupported。
func NewLoader() *Loader {
	return newLoader(unsupportedPrimitives{}, abiProfile)
}
