//go:build !linux

// 本文件是非 Linux 平台的生产加载器构造,与 loader_linux.go 的
// NewLoader 同签名。macOS 与 Windows 均为规划能力、未验收。本文件
// 存在的目的是让公开层持有平台中立的生产入口、
// 保证 GOOS=windows/darwin 交叉构建可编译,而不是声明任何平台
// 支持——这里返回使用「不支持平台原语」的加载器,其 open/lookup
// 一律返回包装 ErrUnsupported 的错误,明确声明该平台的 loader 未
// 实现。Open 会经 Loader.Open 的既有包装得到 *LibraryLoadError,
// 不会进入任何系统 loader,也不会产生半初始化状态。
//
// Windows/macOS 的 Go/fake-native 单测覆盖平台中立行为;单测和
// 交叉编译都不会使这里的占位原语获得实际动态装载能力。

package native

import (
	"fmt"
	"runtime"
)

// unsupportedPrimitives 是非 Linux 平台的占位原语:两个方法都拒绝
// 服务并声明 loader 未实现,不假装成功、不静默降级。
type unsupportedPrimitives struct{}

// open 拒绝打开任何库:非 Linux 平台的 loader 未实现。
func (unsupportedPrimitives) open(path string) (uintptr, error) {
	return 0, fmt.Errorf("native loader is not implemented for %s/%s: %w",
		runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

// lookup 拒绝查找任何符号:非 Linux 平台的 loader 未实现。生产路径
// 不会到达这里(open 已拒绝),方法只为满足 loaderPrimitives 接口。
func (unsupportedPrimitives) lookup(handle uintptr, name string) (uintptr, error) {
	return 0, fmt.Errorf("native loader is not implemented for %s/%s: %w",
		runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

// NewLoader 构造非 Linux 平台的生产加载器入口:经 Loader.Open 的
// 既有包装,任何加载请求都会得到可识别的 *LibraryLoadError,其
// 错误链可经 errors.Is 识别 ErrUnsupported。
func NewLoader() *Loader {
	return newLoader(unsupportedPrimitives{}, abiProfile)
}
