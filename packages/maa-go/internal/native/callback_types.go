// 本文件声明 Unix/Windows callback 的 Go 适配签名:C 契约为 void
// (AsstCaller.h:53 的 AsstApiCallback),回调不向 C 侧返回任何
// 状态;Windows purego 路径的适配签名返回 uintptr,实际 trampoline
// 的函数体必须显式返回常量 0。
//
// 实际 trampoline 由 MAA-13 以唯一具名 nativeCallback 经
// purego.NewCallback 接入;本文件只提供可静态核验的声明,两个
// 类型都不带平台条件——callback 的平台差异由装配层处理,声明层
// 共同保留。签名形式由 symbols_static_test.go 钉死。

package native

// unixCallback 是 Unix 系统 ABI 下的回调适配签名,无返回值,
// 与 C void 契约一致。
type unixCallback func(msg int32, detailsJSON *byte, customArg uintptr)

// windowsCallback 是 Windows purego 路径下的回调适配签名:注册
// 桩要求 uintptr 返回槽,trampoline 函数体显式返回常量 0。
type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr) uintptr
