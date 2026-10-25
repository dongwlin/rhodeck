// maa 包的公开错误面。全部错误唯一定义于 raw 边界层
// (packages/maa-go/internal/native/errors.go),本文件以类型别名与
// 同名变量再导出:公开名称、错误文本与类型同一性同迁移前逐项
// 一致——别名与定义是同一类型,errors.Is / errors.As 的错误链行为
// 不变,既有调用方代码无需任何修改。字段级构造契约见定义处;
// 以下注释面向调用方,保留完整语义。

package maa

import "github.com/dongwlin/rhodeck/packages/maa-go/internal/native"

// 状态哨兵错误。调用方必须用 errors.Is 判别,不得依赖错误文本匹配;
// worker 会把错误转换成稳定的协议错误码,而不是解析字符串。
var (
	// ErrClosed 表示目标实例或 Runtime 已完成关闭(closed 终态),
	// 其后的一切业务调用都被拒绝;正常 Close 时仍未结算的 Operation
	// 也统一以此错误结项。首个故障等真正的失败原因由实例 Err 另行
	// 保留,本错误只表达“生命周期已结束”。
	ErrClosed = native.ErrClosed

	// ErrClosing 表示目标正处于 closing 阶段:新业务调用与新的创建
	// 请求都被拒绝。例如 Runtime.Close 期间创建者不得再发布实例,
	// 返回本错误并由创建者完成同一单次关闭/回滚流程。
	ErrClosing = native.ErrClosing

	// ErrRuntimeInUse 表示并发 Open 被拒绝:进程内已有预占或活动的
	// Runtime。binding 不复用已有对象、也不忽略新配置,直接拒绝本次
	// Open,以维持首期“每进程单 Runtime”的生命周期契约。
	ErrRuntimeInUse = native.ErrRuntimeInUse

	// ErrRuntimeTerminated 表示本进程的 Runtime 使用权已终止:
	// 原生加载开始之后发生了失败,或生命周期已经结束(关闭、创建失败
	// 回滚)。进入该状态后,后续 Open 一律返回本错误;不得在同进程内
	// 反复尝试不同资源配置。
	ErrRuntimeTerminated = native.ErrRuntimeTerminated

	// ErrUnsupported 表示当前平台或当前已探测的能力集不提供该功能,
	// 例如首期 Linux 上请求 Windows 专属窗口 API。binding 不在其他
	// 系统上模拟成功,也不静默降级。
	ErrUnsupported = native.ErrUnsupported

	// ErrInvalidInput 表示纯 Go 侧的输入校验失败:路径、UTF-8 与 NUL、
	// JSON 形状、Limits 字段范围、Wait(nil) 等。此类失败在提交前即被
	// 拒绝,不会进入 native,也不占用任何队列或原生资源。
	ErrInvalidInput = native.ErrInvalidInput

	// ErrNativeRejected 表示原生调用返回失败(如 C bool=false)。
	// C bool=false 通常没有结构化原因,binding 不编造 LastError 或其他
	// 不存在的诊断信息。异步提交返回 ID=0 且该窗口无矛盾证据时,
	// 也归入本错误;C void 返回只证明调用已返回,不构成成功证明。
	ErrNativeRejected = native.ErrNativeRejected

	// ErrBufferLimit 表示输出缓冲区的容量探测已耗尽:NullSize 重试达到
	// Max 容量或 GrowthRetries 上限后仍无法取得完整输出。这是 binding
	// 侧的有界放弃(保守内存策略),不代表原生调用失败;诊断需保留
	// 接口、尝试次数、最后容量等元数据,不得编造“所需容量”。
	ErrBufferLimit = native.ErrBufferLimit

	// ErrEventOverflow 表示事件容量越界:单条消息超过上限,或内部/
	// 公开队列的事件计数、字节预算耗尽。此时实例故障:未结清的等待者
	// 以本错误结项,新操作被拒绝,丢失条数进入诊断;binding 不静默
	// 丢弃完成/失败事件后继续宣称结果完整,也不自动重放设备动作。
	ErrEventOverflow = native.ErrEventOverflow

	// ErrOperationLimit 表示未结算 Operation 数已达 PendingOperations
	// 上限,本次提交被拒绝且不会调用 native;实例不因此故障,已接受的
	// 提交与其他资源不受影响。提交前的纯 Go 校验失败不占槽,与本错误
	// 无关。
	ErrOperationLimit = native.ErrOperationLimit

	// ErrProtocol 表示绑定与原生之间的协议被破坏,即出现无法解释的
	// 矛盾证据:未知/重复/回退的 ID、第二个早到结果、ID 或 what 不符、
	// 关联字段缺失或类型错误、界内非法 UTF-8/JSON、本地事件序号耗尽、
	// 原生返回值大于实际容量等。此类情况必须保留原文并进入故障流程,
	// 不得丢弃矛盾证据伪装成功。
	ErrProtocol = native.ErrProtocol
)

// LibraryLoadError 报告原生动态库加载失败,与符号查找、函数注册等
// 其他失败阶段相区分,不把所有加载失败合成一个字符串。
//
// 字段语义:LibraryName 是请求加载的库名(不含路径的规范名);Path
// 是本次尝试加载的规范化路径,即实际传给系统 loader 的路径,多次
// 尝试多个路径时每次失败各自产生一条错误;GOOS、GOARCH 是本次
// 加载的目标平台(运行时探测到的组合),与实际运行环境一致,不
// 来自编译期假设;Err 是系统 loader 返回的底层错误,依赖库缺失等
// 系统级原因可通过该错误定位(如 dlopen 的错误文本经包装保留),
// 构造方保证其非 nil。
type LibraryLoadError = native.LibraryLoadError

// SymbolLookupError 报告从已加载的原生库中查找符号失败。
// 聚合报告多个缺失符号时,每个缺失符号都有独立的一项,
// 诊断字段足以单独定位每一个符号。
//
// 字段语义:LibraryPath 是被查找的库的路径;Symbol 是缺失或不可
// 解析的符号名;Profile 是已选定并验证的 ABI 兼容组合标识,只描述
// 当前选择,不声称“升级到最新版本一定能解决”;Err 是底层查找
// 错误,查找成功但地址为 0 时,loader 会生成明确的原因放入本字段
// (空地址同样视为失败,不返回可用函数),构造方保证其非 nil。
type SymbolLookupError = native.SymbolLookupError

// FunctionRegistrationError 报告把已查到的原生符号注册为可调用 Go
// 函数时失败。它只表示注册环节的问题(签名不匹配、注册期 panic 等),
// 不得伪装成缺符号(SymbolLookupError)或原生执行错误。
//
// 字段语义:Symbol 是注册失败的符号名(符号本身已找到,注册环节
// 失败);Signature 是目标 Go 签名的文本描述,例如
// "func(callback uintptr, customArg uintptr) uintptr";Err 是注册
// 失败的原因,构造方保证其非 nil——当注册边界的 recover 捕获到的
// panic 值本身是 error 时,该 error 直接保留为本字段,错误链上的
// errors.Is / errors.As 仍能识别这个原始原因;非 error 的 panic 值
// 由注册代码转换为诊断错误后放入本字段。
type FunctionRegistrationError = native.FunctionRegistrationError

// AggregateError 汇集同一次加载事务中收集的多个错误,典型场景是
// 一次性报告多个缺失符号。清理诊断保留原始失败,不相互覆盖。
//
// 调用方无需解析错误文本:可以直接 range 遍历本类型的成员,
// 也可以通过 Unwrap 取出成员切片;Go 1.20+ 的 errors.Is 与
// errors.As 会自动遍历每个成员。
type AggregateError = native.AggregateError
