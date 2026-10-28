//go:build linux || darwin

package native

import "github.com/ebitengine/purego"

// loadFlags 使用平台定义的 RTLD_NOW|RTLD_LOCAL。Linux 上值为 0x2,
// macOS 上值为 0x6;不混入 RTLD_GLOBAL 或 RTLD_LAZY:
//
//   - RTLD_NOW:全部重定位在加载期完成,缺符号与依赖缺失等链接
//     失败在打开阶段即暴露,失败时机与诊断位置可预测;
//   - RTLD_LOCAL:库自身符号不进入进程全局符号名字空间,后续加载
//     的其他库不会因本库的符号而改变解析结果;
//   - 不用 RTLD_GLOBAL:本层不向进程其他部分扩大符号可见性,依赖
//     解析由库内的 $ORIGIN RUNPATH(Linux)、@loader_path/@rpath
//     (macOS)与系统 loader 负责;
//   - 不用 RTLD_LAZY:延迟绑定会把链接错误推迟到首次调用时才
//     暴露,与本层「加载失败在打开阶段即报告」的策略相悖。
const loadFlags = purego.RTLD_NOW | purego.RTLD_LOCAL

// dlopenPrimitives 是 Linux/macOS 上基于 purego 的平台原语实现,直接调用
// 系统 loader 的 dlopen/dlsym,不引入 cgo。purego 的 Dlopen 与 Dlsym
// 在底层返回零值时已包装 dlerror 文本返回错误,本类型只做直传,
// 不截断、不改写底层错误。
type dlopenPrimitives struct{}

// open 以冻结标志打开 path 指向的动态库。
func (dlopenPrimitives) open(path string) (uintptr, error) {
	return purego.Dlopen(path, loadFlags)
}

// lookup 在 handle 指向的已加载库中查找符号 name。
func (dlopenPrimitives) lookup(handle uintptr, name string) (uintptr, error) {
	return purego.Dlsym(handle, name)
}

// NewLoader 构造生产加载器:Linux/macOS 使用真实平台原语(dlopenPrimitives,
// 即 purego dlopen/dlsym 直传)与冻结的 ABI 组合标识。生产代码经本
// 函数取得加载器;测试注入用 newLoader 挂接假原语,不替换任何全局
// 函数变量。Windows 实现见 loader_windows.go。
func NewLoader() *Loader {
	return newLoader(dlopenPrimitives{}, abiProfile)
}
