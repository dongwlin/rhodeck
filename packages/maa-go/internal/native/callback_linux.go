//go:build linux

// 本文件实现 Linux 上的 callback trampoline 装配(绑定设计 §4,MAA-13):
// 唯一具名的 nativeCallback 函数体被 purego.NewCallback(nativeCallback)
// 直接包装——不传闭包、不经过间接变量、不加 CDecl 标记。每进程恰好
// 创建并复用一个 trampoline(purego 为每次 NewCallback 分配的执行内存
// 永不释放,且可用总数有限,因此严禁按实例/任务反复创建);签名与
// callback_types.go 冻结的 unixCallback 一致:Unix 无返回值,C void
// 契约(AsstCaller.h:53 的 AsstApiCallback)保持不变。
//
// 边界与所有权:
//   - customArg 是原生持有的整数 token(CreateEx 按值传入、回调按值
//     收回),不是 Go 地址,也绝不解引用;实例分流由后续里程碑的
//     token 注册表完成;
//   - detailsJSON 指向原生持有的 const char* 缓冲:只在本次回调内
//     有效,必须在返回前有界读取并复制为 Go 拥有的字节串;复制是
//     原始字节复制,不做任何 JSON 解析——不经 map[string]any 的
//     float64 往返,超过 2^53 的整数原样保留(绑定设计 §6.1);
//   - panic 边界只 recover 本回调函数体内的 Go panic,使 Go panic
//     不越过 native 边界、C 线程得以继续存活并正常返回;不宣称能
//     recover 原生崩溃(SIGSEGV、abort 等)或 C++ 异常——那类失败
//     由独立子进程执行器(ffitest)表现为可见的进程失败;
//   - recover 到的 panic 与其他边界异常(超界 details、无处理器)
//     都转为未导出的原子故障记录(测试可读),不静默吞掉。
//
// 本文件同时提供 M2 投递缝:复制后的 (msg, details, token) 投递到
// 未导出的最小包级 sink。sink 只是一个可替换的观察口,M3 的实例
// token 注册表、callback 准入门与事件分发器/队列建立后将整体替换
// 它——本里程碑严禁提前实现那三者。

package native

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/ebitengine/purego"
)

// maxCallbackDetailsBytes 是 details 有界读取的安全上界(1 MiB):读到
// NUL 为止,至多读这么多个字节。它不是容量协商协议,只防御原生侧
// 违反「NUL 结尾」契约或异常超长的缓冲——锁定 MaaCore 的回调
// details(任务 ID 列表、动作详情等)远小于该值,正常消息不受影响。
// 到达上界仍未遇到 NUL 即记故障(见 callbackFaultDetailsOverBound),
// 不静默截断。
const maxCallbackDetailsBytes = 1 << 20

// callbackFault 是 nativeCallback 边界内一次可见故障的记录:Kind 取
// 下述 callbackFault* 常量,Msg/Token 是触发回调的消息 ID 与整数
// token,Detail 是人类可读的诊断(panic 值文本、超界说明等)。
type callbackFault struct {
	Kind   string
	Msg    int32
	Token  uintptr
	Detail string
}

// 边界故障类别。panic 只指本回调体内的 Go panic;no-sink 表示消息
// 已复制但无处理器接收;details-over-bound 表示有界读取未遇 NUL。
const (
	callbackFaultPanic            = "panic"
	callbackFaultNoSink           = "no-sink"
	callbackFaultDetailsOverBound = "details-over-bound"
)

// lastCallbackFault 保存最近一次边界故障(后写覆盖先写)。原子槽是
// 有意的最小形态:M2 阶段测试是唯一读者;M3 建立注册表与分发器后
// 故障走正式通道。
var lastCallbackFault atomic.Pointer[callbackFault]

// recordCallbackFault 记录一次边界故障。
func recordCallbackFault(fault callbackFault) {
	lastCallbackFault.Store(&fault)
}

// callbackSink 是 M2 投递缝的处理器形态:收到的是已经复制完成、Go
// 拥有的 details 字节串,可以在回调返回后继续使用。M3 的实例 token
// 注册表+准入门+事件分发器将替换本缝。
type callbackSink func(msg int32, details []byte, token uintptr)

// callbackSinkSlot 是包级 sink 槽:nil 表示未安装处理器。生产路径在
// M2 阶段保持 nil(每次真实回调都记 no-sink 故障);测试经本槽安装
// 观察处理器。
var callbackSinkSlot atomic.Pointer[callbackSink]

// nativeCallback 是唯一的 Unix callback 适配函数:由 purego trampoline
// 在原生线程上调用,无返回值(与 C void 契约一致)。执行顺序固定:
// 先在返回前复制 details,再投递或记故障;任何一步的 Go panic 都被
// 本函数顶部的 defer/recover 拦在 native 边界内。
func nativeCallback(msg int32, detailsJSON *byte, customArg uintptr) {
	defer func() {
		if r := recover(); r != nil {
			recordCallbackFault(callbackFault{
				Kind:  callbackFaultPanic,
				Msg:   msg,
				Token: customArg,
				Detail: fmt.Sprintf("callback 内 Go panic 已被 native 边界 recover(不宣称 recover 原生崩溃或 C++ 异常): %v", r),
			})
		}
	}()

	snapshot, overBound := copyCallbackDetails(detailsJSON)
	if overBound {
		recordCallbackFault(callbackFault{
			Kind:  callbackFaultDetailsOverBound,
			Msg:   msg,
			Token: customArg,
			Detail: fmt.Sprintf("callback details 在 %d 字节内未遇到 NUL,疑似违反 NUL 结尾契约;有界前缀已保留为诊断原文,快照不完整",
				maxCallbackDetailsBytes),
		})
	}
	deliverCallback(msg, snapshot, customArg)
}

// copyCallbackDetails 从原生 const char* 有界读取到 NUL 为止,在返回
// 前复制为 Go 拥有的字节串。原始字节复制,不做任何 JSON 解析(不经
// float64 往返,见绑定设计 §6.1)。detailsJSON 为 nil 按空快照处理,
// 不算故障(C 侧允许传 NULL);到达上界仍未遇 NUL 时返回已读前缀并
// 置 overBound,由调用方记故障。
func copyCallbackDetails(detailsJSON *byte) (snapshot []byte, overBound bool) {
	if detailsJSON == nil {
		return nil, false
	}
	out := make([]byte, 0, 256)
	for i := 0; i < maxCallbackDetailsBytes; i++ {
		c := *(*byte)(unsafe.Add(unsafe.Pointer(detailsJSON), i))
		if c == 0 {
			return out, false
		}
		out = append(out, c)
	}
	return out, true
}

// deliverCallback 把复制完成的 (msg, details, token) 投递到当前
// sink;未安装处理器时记 no-sink 可见故障——不允许静默丢弃消息。
func deliverCallback(msg int32, details []byte, token uintptr) {
	sink := callbackSinkSlot.Load()
	if sink == nil {
		recordCallbackFault(callbackFault{
			Kind:  callbackFaultNoSink,
			Msg:   msg,
			Token: token,
			Detail: "当前无已安装的 callback 处理器,消息未投递;M3 的实例 token 注册表+准入门+事件分发器建立前不得静默丢弃",
		})
		return
	}
	(*sink)(msg, details, token)
}

// nativeCallbackTrampoline 以 sync.OnceValue 固定「每进程恰好一个」:
// 首次访问创建 trampoline,之后所有访问复用同一地址;数量不随实例/
// 任务/获取次数增长。
var nativeCallbackTrampoline = sync.OnceValue(func() uintptr {
	return purego.NewCallback(nativeCallback)
})

// callbackTrampoline 返回进程内唯一的 callback trampoline 地址,供
// AsstCreateEx 的 callback 参数使用(后续里程碑接入)。未导出:raw
// 函数指针与 trampoline 地址不向公开层外泄。
func callbackTrampoline() uintptr {
	return nativeCallbackTrampoline()
}

// 编译期核对:nativeCallback 与冻结的 unixCallback 签名逐参一致
// (Unix 无返回值)。漂移即构建失败,不等运行期。
var _ unixCallback = nativeCallback
