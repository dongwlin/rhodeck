//go:build linux

// 本文件实现 Linux 上的 callback trampoline 装配:唯一具名的
// nativeCallback 函数体被 purego.NewCallback(nativeCallback) 直接包装——
// 不传闭包、不经过间接变量、不加 CDecl 标记。每进程恰好创建并复用一个
// trampoline(purego 为每次 NewCallback 分配的执行内存永不释放,且可用
// 总数有限,因此严禁按实例/任务反复创建);签名与 callback_types.go 冻结
// 的 unixCallback 一致:Unix 无返回值,C void 契约保持不变。
//
// 本文件只承载 trampoline 装配与 panic 边界,查表后的分发序(token 校验
// → 查找 → 投递)统一在平台中立的 dispatchCallback 完成,原生 details
// 缓冲的有界读取与复制由注册表命中的投递函数在取得 callback lease 后
// 执行。
//
// 边界与所有权:
//   - customArg 是原生持有的整数 token(CreateEx 按值传入、回调按值
//     收回),不是 Go 地址,也绝不解引用;实例分流经 token 注册表完成;
//   - detailsJSON 指向原生持有的 const char* 缓冲,只在本次回调内有效;
//     本文件不读取、不复制该缓冲,读取纪律交给注册表命中的投递函数;
//   - panic 边界只 recover 本回调函数体内的 Go panic(含投递函数的
//     panic),使 Go panic 不越过 native 边界;C 线程得以继续存活并正常
//     返回;不宣称能 recover 原生崩溃(SIGSEGV、abort 等)或 C++ 异常;
//   - recover 到的 panic 转为可见的原子故障记录,不静默吞掉。

package native

import (
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
)

// nativeCallback 是唯一的 Unix callback 适配函数:由 purego trampoline
// 在原生线程上调用,无返回值(与 C void 契约一致)。执行顺序固定:
// 先登记 panic 顶栏,再进入平台中立的 dispatchCallback 分发序;函数
// 体内的任何 Go panic 都被顶栏拦在 native 边界内,转为故障记录后正
// 常返回。
func nativeCallback(msg int32, detailsJSON *byte, customArg uintptr) {
	defer func() {
		if r := recover(); r != nil {
			recordCallbackFault(callbackFault{
				Kind:   callbackFaultPanic,
				Msg:    msg,
				Token:  customArg,
				Detail: fmt.Sprintf("Go panic inside the callback recovered at the native boundary (no claim of recovering native crashes or C++ exceptions): %v", r),
			})
		}
	}()

	dispatchCallback(msg, detailsJSON, customArg)
}

// nativeCallbackTrampoline 以 sync.OnceValue 固定「每进程恰好一个」:
// 首次访问创建 trampoline,之后所有访问复用同一地址;数量不随实例/
// 任务/获取次数增长。
var nativeCallbackTrampoline = sync.OnceValue(func() uintptr {
	return purego.NewCallback(nativeCallback)
})

// CallbackTrampoline 返回进程唯一的 callback trampoline 地址:供
// AsstCreateEx 的 callback 参数接线与 callback fixture 使
// 用。地址进程内稳定,重复获取返回同一值,数量不随获取次数增长。
func CallbackTrampoline() uintptr {
	return nativeCallbackTrampoline()
}

// 编译期核对:nativeCallback 与冻结的 unixCallback 签名逐参一致
// (Unix 无返回值)。漂移即构建失败,不等运行期。
var _ unixCallback = nativeCallback
