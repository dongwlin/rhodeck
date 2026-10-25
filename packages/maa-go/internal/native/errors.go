// 本文件是 binding 错误的唯一定义处,位于 raw 边界层:loader、
// 注册事务等内部层需要直接构造这些结构化错误,而依赖方向禁止
// 内部层反向引用公开层。公开层(packages/maa-go)以类型别名与
// 同名变量把本文件的定义再导出,公开名称、类型同一性与错误链
// 行为均与定义处一致。
//
// 错误文本统一带 "maa: " 前缀——这是公开错误文本的约定,与定义
// 所在的包无关。判别一律经 errors.Is / errors.As 沿错误链完成,
// 不得解析错误文本;包装底层原因时必须用 %w 保留可追溯性。

package native

import (
	"errors"
	"fmt"
	"strings"
)

// 状态哨兵错误,表达生命周期能力与协议层面的可辨识分类。构造
// 约定:包装时以 fmt.Errorf("...: %w", 哨兵) 保持 errors.Is 可达;
// 各哨兵互为不同错误值,判别不依赖错误文本。
var (
	// ErrClosed 表示目标实例或 Runtime 已进入 closed 终态:其后的
	// 一切业务调用都被拒绝,正常 Close 时仍未结算的 Operation 也
	// 统一以此错误结项。真正的失败原因由实例 Err 另行保留,本
	// 哨兵只表达“生命周期已结束”。
	ErrClosed = errors.New("maa: instance or runtime is closed")

	// ErrClosing 表示目标正处于 closing 阶段:新业务调用与新的
	// 创建请求都被拒绝,由创建者完成同一单次关闭/回滚流程。
	ErrClosing = errors.New("maa: close in progress")

	// ErrRuntimeInUse 表示并发 Open 被拒绝:进程内已有预占或活动
	// 的 Runtime。binding 不复用已有对象、也不忽略新配置,直接
	// 拒绝本次 Open,以维持每进程单 Runtime 的生命周期契约。
	ErrRuntimeInUse = errors.New("maa: runtime already reserved or active in this process")

	// ErrRuntimeTerminated 表示本进程的 Runtime 使用权已终止:原生
	// 加载开始之后发生了失败,或生命周期已经结束(关闭、创建失败
	// 回滚)。进入该状态后,后续 Open 一律返回本错误;不得在同
	// 进程内反复尝试不同资源配置。
	ErrRuntimeTerminated = errors.New("maa: runtime terminated for this process")

	// ErrUnsupported 表示当前平台或当前已探测的能力集不提供该功能。
	// binding 不在其他系统上模拟成功,也不静默降级。
	ErrUnsupported = errors.New("maa: not supported on this platform or capability set")

	// ErrInvalidInput 表示纯 Go 侧的输入校验失败:路径、UTF-8 与
	// NUL、JSON 形状、Limits 字段范围、Wait(nil) 等。此类失败在
	// 提交前即被拒绝,不进入 native,也不占用任何队列或原生资源。
	ErrInvalidInput = errors.New("maa: invalid input")

	// ErrNativeRejected 表示原生调用返回失败(如 C bool=false)。
	// C bool=false 通常没有结构化原因,构造本错误时不得编造
	// LastError 或其他不存在的诊断信息;C void 返回只证明调用已
	// 返回,不构成成功证明。
	ErrNativeRejected = errors.New("maa: native call returned failure")

	// ErrBufferLimit 表示输出缓冲区的容量探测已耗尽:NullSize 重试
	// 达到 Max 容量或 GrowthRetries 上限后仍无法取得完整输出。这是
	// binding 侧的有界放弃(保守内存策略),不代表原生调用失败;
	// 构造时诊断需保留接口、尝试次数、最后容量等元数据,不得编造
	// “所需容量”。
	ErrBufferLimit = errors.New("maa: output buffer capacity probing exhausted")

	// ErrEventOverflow 表示事件容量越界:单条消息超过上限,或内部/
	// 公开队列的事件计数、字节预算耗尽。此时实例故障:未结清的
	// 等待者以本错误结项,新操作被拒绝,丢失条数进入诊断;binding
	// 不静默丢弃完成/失败事件后继续宣称结果完整,也不自动重放
	// 设备动作。
	ErrEventOverflow = errors.New("maa: event queue capacity exceeded")

	// ErrOperationLimit 表示未结算 Operation 数已达 PendingOperations
	// 上限,本次提交被拒绝且不会调用 native;实例不因此故障,已
	// 接受的提交与其他资源不受影响。提交前的纯 Go 校验失败不占槽,
	// 与本错误无关。
	ErrOperationLimit = errors.New("maa: pending operation limit exceeded")

	// ErrProtocol 表示绑定与原生之间的协议被破坏,即出现无法解释的
	// 矛盾证据:未知/重复/回退的 ID、第二个早到结果、ID 或 what
	// 不符、关联字段缺失或类型错误、界内非法 UTF-8/JSON、本地事件
	// 序号耗尽、原生返回值大于实际容量等。构造时必须保留原文并
	// 进入故障流程,不得丢弃矛盾证据伪装成功。
	ErrProtocol = errors.New("maa: binding protocol violation")
)

// LibraryLoadError 报告原生动态库加载失败,与符号查找、函数注册等
// 其他失败阶段相区分,不把所有加载失败合成一个字符串。打开阶段的
// 每一处失败都构造本类型:构造约定是各诊断字段全部填写,Err 必须
// 非 nil。
type LibraryLoadError struct {
	// LibraryName 是请求加载的库名(不含路径的规范名);路径为空
	// 的退化情形记为空串,不回退为 "."。
	LibraryName string

	// Path 是本次尝试加载的规范化路径,即实际传给系统 loader 的
	// 路径。多次尝试多个路径时,每次失败各自产生一条错误。
	Path string

	// GOOS、GOARCH 是本次加载的目标平台(运行时探测到的 GOOS 与
	// GOARCH 组合),与实际运行环境一致,不来自编译期假设。
	GOOS   string
	GOARCH string

	// Err 是系统 loader 返回的底层错误,构造时必须提供非 nil 值;
	// 依赖库缺失等系统级原因经 Unwrap 保留在错误链上,可被
	// errors.Is / errors.As 追溯。输入校验类失败以 fmt.Errorf 的
	// %w 包装 ErrInvalidInput 后放入本字段。
	Err error
}

// Error 输出库名、规范化路径、目标平台与底层原因。
func (e *LibraryLoadError) Error() string {
	return fmt.Sprintf("maa: load library %q at %s for %s/%s failed: %v",
		e.LibraryName, e.Path, e.GOOS, e.GOARCH, e.Err)
}

// Unwrap 暴露底层错误,使 errors.Is 能沿错误链定位具体加载原因
// (如依赖库缺失)。
func (e *LibraryLoadError) Unwrap() error { return e.Err }

// SymbolLookupError 报告从已加载的原生库中查找符号失败。聚合报告
// 多个缺失符号时,每个缺失符号都有独立的一项,诊断字段足以单独
// 定位每一个符号。构造约定:查找失败与「查找成功但地址为 0」都
// 构造本类型,Err 必须非 nil;零地址情形由构造方生成明确原因,
// 不得把零地址当作可调用的函数地址返回。
type SymbolLookupError struct {
	// LibraryPath 是被查找的库的路径。
	LibraryPath string

	// Symbol 是缺失或不可解析的符号名。
	Symbol string

	// Profile 是已选定并验证的 ABI 兼容组合标识。它只描述当前
	// 选择,不声称“升级到最新版本一定能解决”。
	Profile string

	// Err 是底层查找错误,构造时必须提供非 nil 值。查找成功但
	// 地址为 0 时,构造方生成明确的原因放入本字段。
	Err error
}

// Error 输出符号名、库路径、ABI 组合标识与底层原因。
func (e *SymbolLookupError) Error() string {
	return fmt.Sprintf("maa: symbol %q not found in %s (profile %s): %v",
		e.Symbol, e.LibraryPath, e.Profile, e.Err)
}

// Unwrap 暴露底层查找错误。
func (e *SymbolLookupError) Unwrap() error { return e.Err }

// FunctionRegistrationError 报告把已查到的原生符号注册为可调用 Go
// 函数时失败。它只表示注册环节的问题(签名不匹配、注册期 panic
// 等),不得伪装成缺符号(SymbolLookupError)或原生执行错误。
type FunctionRegistrationError struct {
	// Symbol 是注册失败的符号名(符号本身已找到,注册环节失败)。
	Symbol string

	// Signature 是目标 Go 签名的文本描述,例如
	// "func(callback uintptr, customArg uintptr) uintptr"。
	Signature string

	// Err 是注册失败的底层原因,构造时必须提供非 nil 值。
	//
	// 构造契约:注册边界 recover 捕获到的 panic 值本身是 error 时,
	// 该 error 直接作为本字段(不再包一层诊断错误),错误链上的
	// errors.Is / errors.As 仍能识别这个原始原因;非 error 的
	// panic 值由注册代码转换为诊断错误后放入本字段。
	Err error
}

// Error 输出符号名、目标签名与底层原因。
func (e *FunctionRegistrationError) Error() string {
	return fmt.Sprintf("maa: register function %q with signature %s failed: %v",
		e.Symbol, e.Signature, e.Err)
}

// Unwrap 暴露注册失败的底层原因;注册 panic 产生的 error 原因
// 也经此保留在错误链上。
func (e *FunctionRegistrationError) Unwrap() error { return e.Err }

// AggregateError 汇集同一次加载事务中收集的多个错误,典型场景是
// 一次性报告多个缺失符号。构造约定:成员为可独立判别的结构化
// 错误,清理诊断保留原始失败,不相互覆盖;空聚合不得用于伪装
// 成功。
//
// 方法行为:Error 逐项编号列出各成员文本,空聚合输出固定占位
// 文本(不产生空串、不 panic);Unwrap 返回成员切片,使 Go 1.20+
// 的 errors.Is / errors.As 按多错误展开规则自动遍历每个成员,
// 调用方也可直接 range 遍历本类型的成员逐项检查。
type AggregateError []error

// Error 逐项编号列出各成员文本。空聚合输出固定的占位文本,
// 不产生空串,也不会 panic。
func (e AggregateError) Error() string {
	if len(e) == 0 {
		return "maa: aggregate error (0 items)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "maa: %d errors occurred:", len(e))
	for i, err := range e {
		fmt.Fprintf(&b, "\n\t[%d] %v", i+1, err)
	}
	return b.String()
}

// Unwrap 返回成员切片,使 errors.Is / errors.As 按 Go 1.20+ 的
// 多错误展开规则自动遍历每个成员;调用方也可据此取出每一项。
func (e AggregateError) Unwrap() []error { return e }
