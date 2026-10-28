// 本文件实现 callback 的 token 注册表与平台中立的分发序:进程级单调
// token 生成器、token → 投递函数的注册表,以及查表后的固定分发步序
// (token 校验 → 查找 → 测试暂停缝 → 投递)。平台装配层
// (callback_linux.go 的 nativeCallback)只承载 trampoline 装配与 panic
// 边界,其余分发都经 dispatchCallback 完成;边界故障原语也在本文件,使
// 注册表与分发序在无平台条件的文件内自洽。
//
// 并发契约:
//   - 注册表锁只覆盖查表与登记/移除;取出投递函数值后立即释放,锁绝不
//     跨原生内存复制、投递或任何 native 调用;
//   - token 由互斥锁保护的计数器单调生成:从 1 起、永不复用(注销后的
//     token 也不再发出);计数器将回绕时拒绝生成;
//   - 注册表持有投递函数(及其捕获的实例状态强引用),直至显式注销;
//     token 移除只允许发生在 callback 准入门已关闭且全部已准入 callback
//     退出之后,移除本身不等待。
//
// 边界故障不静默丢弃:token 0 与查表未命中都记为可见的原子故障记录,
// 不投递、不 panic——原生线程上的回调不能因接线层错误而终止 C 线程。
// panic 边界由平台装配层的 nativeCallback 顶栏承载,本文件不 recover。

package native

import (
	"fmt"
	"sync"
	"sync/atomic"
	"unsafe"
)

// CallbackDelivery 是注册表持有的投递函数形态:detailsJSON 指向原生
// 持有的 const char* 缓冲,仅在本次调用内有效;读取纪律(先取得
// callback lease 再读原生内存)与实现方自身的锁纪律(状
// 态锁不跨原生内存复制)由实现方负责。注册表锁绝不跨本调用持有。
type CallbackDelivery func(msg int32, detailsJSON *byte)

// callbackFault 是 callback 分发边界内一次可见故障的记录:Kind 取
// 下述 callbackFault* 常量,Msg/Token 是触发回调的消息 ID 与整数
// token,Detail 是人类可读的诊断(panic 值文本、token 异常说明等)。
type callbackFault struct {
	Kind   string
	Msg    int32
	Token  uintptr
	Detail string
}

// 边界故障类别。panic 只指平台装配层回调体内的 Go panic(nativeCallback
// 顶栏 recover 的那一类);token-zero 与 unknown-token 表示消息没有
// 可投递的目标——原生侧携带的 token 不会被接受为 0,尚未注册或已
// 注销的 token 也不再命中,两者都是绑定与原生之间的协议破坏证据,
// 记录后不投递、不静默丢弃。
const (
	callbackFaultPanic        = "panic"
	callbackFaultTokenZero    = "token-zero"
	callbackFaultUnknownToken = "unknown-token"
)

// lastCallbackFault 保存最近一次边界故障(后写覆盖先写)。原子槽是
// 有意的最小形态:正式的故障通道由 maa 层实例状态承接(的
// sticky fault),到达本槽的故障意味着消息未进入任何实例,仅供测试
// 与诊断观察。
var lastCallbackFault atomic.Pointer[callbackFault]

// recordCallbackFault 记录一次边界故障。
func recordCallbackFault(fault callbackFault) {
	lastCallbackFault.Store(&fault)
}

// callbackTokenSource 是进程级单调 token 生成器:互斥锁保护的计数
// 器,last 是已发出的最后一个 token(0 表示尚未发出,首个 token 为
// 1)。token 在进程内永不复用:注销后的 token 也不再发出,保证原生
// 侧迟到的回调只会得到查表未命中的可见故障,绝不会投递给新实例。
type callbackTokenSource struct {
	mu   sync.Mutex
	last uintptr
}

// callbackTokens 是进程唯一的 token 生成器。
var callbackTokens callbackTokenSource

// next 生成下一个 token:计数器将回绕(超过 uintptr 最大值)时返回
// 包装 ErrProtocol 的错误,成功返回未发出过的非零 token(耗尽
// 或回绕时拒绝创建)。
func (s *callbackTokenSource) next() (uintptr, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.last == ^uintptr(0) {
		return 0, fmt.Errorf("maa: callback token space is exhausted: %w", ErrProtocol)
	}
	s.last++
	return s.last, nil
}

// callbackTargetRegistry 是 token → 投递函数的进程级注册表:查表走
// 读锁(callback 位于原生线程,频率高),登记/移除走写锁(创建与
// 关闭各一次)。锁纪律:任何方法都不在持有锁期间调用投递函数、触
// 碰原生内存或执行 native 调用。
type callbackTargetRegistry struct {
	mu      sync.RWMutex
	targets map[uintptr]CallbackDelivery
}

// callbackTargets 是进程唯一的 callback 注册表:注册表先于 CreateEx
// 建立,使创建过程中的早到回调也能找到投递目标。
var callbackTargets = &callbackTargetRegistry{targets: make(map[uintptr]CallbackDelivery)}

// install 把 deliver 直写登记到指定 token。RegisterCallbackTarget 在
// 生成 token 后经此登记;callback fixture 也经它把捕获处理器固定在
// 选定的 token 常量上(生产 token 由生成器铸造,fixture 需要与原生
// 侧 custom_arg 常量逐一对上)。token 已在注册表中即内部不变量破坏
// (token 永不复用),panic。
func (r *callbackTargetRegistry) install(token uintptr, deliver CallbackDelivery) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.targets[token]; exists {
		panic(fmt.Sprintf("maa: callback token %d registered more than once", token))
	}
	r.targets[token] = deliver
}

// remove 移除已登记的 token,释放注册表对投递函数(及其捕获的实例
// 状态)的强引用。前置契约(第 4 步):callback 准入门已关闭且
// 全部已准入 callback 退出;本方法不等待、不验证该前置(准入门属
// 于 maa 层实例状态)。token 不存在或已移除属于接线层编程错误,
// panic。
func (r *callbackTargetRegistry) remove(token uintptr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.targets[token]; !exists {
		panic(fmt.Sprintf("maa: callback token %d removed without a registration", token))
	}
	delete(r.targets, token)
}

// lookup 返回 token 对应的投递函数:只在读锁内完成查找,取出函数值
// 后立即释放——锁绝不跨复制、投递或任何 native 调用。未命中
// 返回 nil,由调用方(dispatchCallback)记可见边界故障。
func (r *callbackTargetRegistry) lookup(token uintptr) CallbackDelivery {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.targets[token]
}

// RegisterCallbackTarget 生成新 token 并把 deliver 登记进注册表:
//   - deliver 为 nil 返回包装 ErrInvalidInput 的错误,不消耗 token;
//   - token 空间耗尽返回包装 ErrProtocol 的错误(耗尽或回绕时
//     拒绝创建);
//   - 成功返回非零 token,供 AsstCreateEx 的 custom_arg 接线
//     与关闭流程的移除复用。
//
// 生成与登记两步对调用方原子完成:token 先经单调生成器取得,登记
// 完成后本函数才把 token 交给调用方——token 唯一且永不复用,登记
// 前的窗口内无人能引用它,native 侧永远拿不到未登记的 token。
func RegisterCallbackTarget(deliver CallbackDelivery) (uintptr, error) {
	if deliver == nil {
		return 0, fmt.Errorf("maa: callback target registered without a delivery function: %w", ErrInvalidInput)
	}
	token, err := callbackTokens.next()
	if err != nil {
		return 0, err
	}
	callbackTargets.install(token, deliver)
	return token, nil
}

// UnregisterCallbackTarget 移除已注册的 token(注册表对实例状态的强
// 引用释放点)。契约:只允许在 callback 准入门已关闭且全部已准入
// callback 退出后调用(第 4 步);本函数不等待。token 不存在或
// 已移除属于接线层编程错误,panic。
func UnregisterCallbackTarget(token uintptr) {
	callbackTargets.remove(token)
}

// LookupCallbackDelivery 返回 token 对应的投递函数,未命中返回 nil。
// 供 maa 层创建接线用例在 CreateEx 调用期间经真实查表路径投递早到消
// 息,验证「注册表先于 CreateEx 建立、早到回调能找到实例状态」的时
// 序;生产路径不使用本函数——原生回调的分发序由 dispatchCallback
// 承接。锁纪律与 lookupCallbackTarget 相同:取出函数值后立即释放读
// 锁,调用方不得在投递期间持有任何锁。
func LookupCallbackDelivery(token uintptr) CallbackDelivery {
	return lookupCallbackTarget(token)
}

// lookupCallbackTarget 返回 token 对应的投递函数,未命中返回 nil:
// 分发序与关闭/创建流程共用的内部查表入口,锁纪律见
// callbackTargetRegistry.lookup。
func lookupCallbackTarget(token uintptr) CallbackDelivery {
	return callbackTargets.lookup(token)
}

// callbackDispatchHookSlot 是 dispatchCallback 的测试暂停缝:非 nil
// 时在「查到 token 之后、调用 deliver 之前」以命中 token 调用一次。
// 生产路径恒为 nil,本缝不承载任何生产语义,只供准入竞态用例在固
// 定点挂起已命中的 callback(对照已删除的 callbackSinkSlot 先
// 例;关闭流程的两类准入竞态验证归本层的用例)。
var callbackDispatchHookSlot atomic.Pointer[func(token uintptr)]

// dispatchCallback 是平台中立的分发序,由平台装配层的 nativeCallback
// 与测试调用,顺序固定:
//
//  1. token 为 0:记 token-zero 边界故障后返回,绝不投递(token
//     0 不会被接受为 custom_arg 的注册结果);
//  2. 查表未命中:记 unknown-token 边界故障后返回——不允许静默丢
//     弃;尚未注册、已注销的 token 与原生侧矛盾的 custom_arg
//     都到达此处;
//  3. 命中:先经过测试暂停缝(非 nil 时),再调用投递函数;投递期
//     间不持有注册表锁。
//
// 本函数不 recover:panic 边界由平台装配层(nativeCallback 顶栏)承
// 载;投递函数自身的 panic 纪律(转 sticky fault)由 maa 层实现方承
// 担。
func dispatchCallback(msg int32, detailsJSON *byte, token uintptr) {
	if token == 0 {
		recordCallbackFault(callbackFault{
			Kind:   callbackFaultTokenZero,
			Msg:    msg,
			Token:  token,
			Detail: "callback carries token 0, no deliverable target; token 0 is never accepted by the registry",
		})
		return
	}
	deliver := lookupCallbackTarget(token)
	if deliver == nil {
		recordCallbackFault(callbackFault{
			Kind:   callbackFaultUnknownToken,
			Msg:    msg,
			Token:  token,
			Detail: "callback carries a token not registered in the registry (never registered or already removed), message not delivered; silent drops are not allowed",
		})
		return
	}
	if hook := callbackDispatchHookSlot.Load(); hook != nil {
		(*hook)(token)
	}
	deliver(msg, detailsJSON)
}

// CallbackDetailsLength 在原生 details 缓冲的下标 0..maxBytes(含,
// 共 maxBytes+1 字节)范围内寻找 NUL:在下标 i 找到则返回 (i, true)
// ——i==maxBytes 合法(冻结:恰好 CallbackBytes 字节后是 NUL
// 合法,超过则溢出);整段无 NUL 返回 (0, false),由调用方按溢出契
// 约处理(不投递截断 JSON)。detailsJSON 为 nil 返回 (0, false)(maa
// 层在调用前自行判 nil 并按 ErrProtocol 处理)。maxBytes 应来自
// Open 时 validateLimits 已校验的 Limits.CallbackBytes(1–16,777,216
// int 转换安全);不分配内存,不修改原生缓冲。
func CallbackDetailsLength(detailsJSON *byte, maxBytes int) (length int, found bool) {
	if detailsJSON == nil {
		return 0, false
	}
	for i := 0; i <= maxBytes; i++ {
		if *(*byte)(unsafe.Add(unsafe.Pointer(detailsJSON), i)) == 0 {
			return i, true
		}
	}
	return 0, false
}

// CopyCallbackDetails 从原生缓冲精确复制 length 字节为独立 Go 字节
// 串:原始字节复制,不做任何解析——不经 map[string]any 的 float64
// 往返,超过 2^53 的整数原样保留。调用前置契约:
// length ≥ 0,且已由 CallbackDetailsLength 确认缓冲在 length 范围内
// 有界(通常 length 即其返回值);违反即接线层编程错误,负长度
// panic。length 为 0 时不读取任何原生字节,直接返回空字节串。
func CopyCallbackDetails(detailsJSON *byte, length int) []byte {
	if length < 0 {
		panic(fmt.Sprintf("maa: callback details copied with a negative length %d", length))
	}
	out := make([]byte, length)
	if length > 0 {
		copy(out, unsafe.Slice(detailsJSON, length))
	}
	return out
}
