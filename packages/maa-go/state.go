// 本文件实现实例的私有共享状态与调用准入原语(MAA-16;绑定设计 §2
// 的 state.go:实例状态、调用/回调准入与单次关闭;语义依据 §7、§8,
// 所有权、容量与停止策略已由 MAA-03 冻结)。单一 instanceState 承载
// 实例阶段、唯一受门控的 handle、已准入调用与 callback 计数、
// callback 准入门、首次故障、共享 closeDone 与最终错误存储,并据此
// 提供调用 lease 准入/释放、独立 raw 串行门禁、关闭选主、在途调用
// 清零等待、首次故障记录与终态发布原语,作为后续 callback 准入接线
// (MAA-18)、pending Operation 表(MAA-19/23)与 Stop/Destroy 关闭
// 流程(MAA-20)的共同入口。
//
// MAA-17 在同一状态门内增补故障通知与丢失诊断(§7、§7.1,全部在
// 状态锁内线性化):一次性 faultDone 通知只由首次故障关闭,正常
// 关闭不触发;丢失条数采用饱和 uint64 计数,不回绕;首次故障只
// 保留有界元数据和至多 faultRawPrefixBytes 字节原文前缀(不是完整
// Event,不积累错误列表)。
//
// 本单元只承载状态与基础 lease 原语:不接 CreateEx/NewInstance,不
// 实现关闭流程本体,不接 callback 分发;公开面(导出符号)零变化。
//
// 并发契约(绑定设计 §7、§8):
//   - 状态锁 mu 只用于检查状态、登记/释放 lease 与发布结果;任何
//     native 调用都不持有状态锁。等待在途调用清零期间绝不持有状态
//     锁(§8 第 2 步),否则已准入调用无法释放 lease,关闭将死锁。
//   - 调用纪律:先取得调用 lease,再排队等待独立的 raw 串行门禁;
//     lease 覆盖排队、native 调用、输出复制与结果登记全过程——
//     门禁前排队的调用已计入在途计数,关闭等待不会漏掉它们。
//   - 关闭在状态锁内选主:恰一个申请者把 ready/faulted 转为 closing
//     并成为所有者;其余申请者共享同一 closeDone 与同一最终错误
//     存储,只允许所有者执行一次 Stop/Destroy(流程本体归 MAA-20)。
//   - 阶段迁移:initializing → ready → closing → closed;异常路径
//     进入 faulted 后仍经 closing 关闭。非法迁移与状态机不变量破坏
//     按编程错误 panic(对照 runtime.go lifecycleGate.publish 的
//     先例),不允许静默继续污染状态。
//
// 错误语义:拒绝新业务调用按阶段返回可辨识错误——closing 包装
// native.ErrClosing,closed 包装 native.ErrClosed,faulted 包装首次
// 故障的原始错误;全部经 %w 保留错误链,errors.Is / errors.As 沿链
// 可达 native 哨兵或真正的失败原因。已准入的 lease 在任何阶段都能
// 正常释放,否则关闭等待无法收敛。

package maa

import (
	"bytes"
	"fmt"
	"math"
	"sync"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// faultRawPrefixBytes 是首次故障保留的原文前缀字节上限(§7.1 冻结:
// 首次故障及停止诊断只保留有界元数据和至多 4,096 字节原文前缀,
// 不是完整 Event,不积累错误列表)。
const faultRawPrefixBytes = 4096

// instancePhase 是实例私有生命周期的阶段(§8:状态变化为
// initializing → ready → closing → closed,异常路径可进入 faulted
// 后关闭;faulted 不允许继续业务操作)。合法迁移只有:
//
//	initializing → ready    创建事务成功后发布(publishReady)
//	initializing → faulted  创建期早到故障(recordFault)
//	ready       → faulted   业务期首次故障(recordFault)
//	ready       → closing   关闭选主胜出(electClose)
//	faulted     → closing   关闭选主胜出(electClose)
//	closing     → closed    所有者发布终态(publishClosed)
//
// closing 期间仍可经 recordFault 记录首因故障(阶段不变,最终错误
// 随后发布);除此之外的组合都意味着私有状态机内部不变量被破坏,
// 按编程错误 panic。
type instancePhase uint8

const (
	phaseInitializing instancePhase = iota // 已登记,创建事务进行中
	phaseReady                             // 已发布,接受业务调用
	phaseClosing                           // 已选出关闭所有者,拒绝新调用
	phaseClosed                            // 关闭完成,终态
	phaseFaulted                           // 首次故障,拒绝业务操作,等待关闭
)

// instanceState 是一个实例的唯一私有共享状态(§7:实例只维护一份
// 私有生命周期状态,包含阶段、handle、已准入调用数、已准入
// callback 数、首次故障与共享 closeDone;pending Operation 与早到
// 结果表不在本单元,由 MAA-19/23 承接)。状态内含锁,只能经指针
// 使用,不按值复制。
//
// handle 是状态路径唯一受门控的实例句柄:不设第二份裸 handle,不
// 提供绕过阶段检查的访问闭包或导出(§8)。业务路径只能经已准入
// callLease.handle 读取,关闭路径只能经所有者专用的 closeHandle
// 读取;0 表示尚未取得有效句柄(§3:0 无效,不解引用)。
type instanceState struct {
	mu sync.Mutex

	// callsDrained 在已准入调用计数归零时广播,callbacksDrained 在
	// 已准入 callback 计数归零时广播。Cond.Wait 在挂起期间释放 mu,
	// 等待清零期间因此不持有状态锁(§8 第 2 步)。
	callsDrained     *sync.Cond
	callbacksDrained *sync.Cond

	phase  instancePhase
	handle uintptr
	// calls 是已准入、尚未释放的业务调用数,含尚在 raw 门禁前排队
	// 的调用(先取得 lease 再排队,§7)。
	calls uint64

	// callback 准入门(§4、§7):门开放时 callback 可取得 lease,
	// closing 阶段仍开放——必要的原生消息和 Destroyed 复制入队不受
	// 关闭影响;关闭流程在 Destroy 返回后关门并等待已准入 callback
	// 退出。接入策略归 MAA-18,本单元只承载标志与计数。
	callbackGateClosed bool
	callbacks          uint64

	// firstFault 是首次故障,sticky:保留首个,不覆盖(§7.1)。
	firstFault error
	// firstFaultPrefix 是首次故障的有界原文前缀:只在首因线性化时
	// 深拷贝至多 faultRawPrefixBytes 字节,后续故障不覆盖、不积累
	// (§7.1:不是完整 Event,不积累错误列表)。
	firstFaultPrefix []byte
	// lostEvents 是饱和丢失计数(§7.1:丢失条数采用饱和 uint64 计数,
	// 不回绕;关闭完成前可能增加,观察 Done 后稳定)。
	lostEvents uint64
	// faultDone 从构造起即存在,只由首次故障在状态门内线性化关闭
	// 一次(§7.1:首次故障关闭独立的一次性 Fault 通知,绝不依赖
	// Events 还有空间;正常关闭不触发)。faultNotified 是防重复关闭
	// 的哨兵,只在持有 mu 时读写。
	faultDone     chan struct{}
	faultNotified bool
	// finalErr 是关闭所有者发布的不可变最终错误;所有关闭申请共享
	// 同一存储(§8:并发 Close 共享同一 closeDone 和最终错误)。
	finalErr error
	// closeDone 从构造起即存在,只由所有者在 publishClosed 中关闭
	// 一次;先于它的最终错误发布保证观察者见到关闭即见稳定终态。
	closeDone chan struct{}

	// raw 是独立于 mu 的 raw 串行门禁:不受状态锁保护、也不保护任何
	// 状态字段;调用纪律见 rawGate。
	raw rawGate
}

// newInstanceState 构造实例共享状态:不依赖 NewInstance,供创建
// 接线(MAA-20)与受控用例复用。handle 允许传 0(真实创建在
// CreateEx 返回前尚未取得句柄)或非零占位值(受控用例构造);业务
// 调用路径读取到 0 视为内部不变量破坏。初始阶段为 initializing,
// 创建成功后经 publishReady 发布;closeDone 在构造时建好,保证并发
// 关闭申请从构造起共享同一通道。
func newInstanceState(handle uintptr) *instanceState {
	s := &instanceState{
		phase:     phaseInitializing,
		handle:    handle,
		closeDone: make(chan struct{}),
		faultDone: make(chan struct{}),
	}
	s.callsDrained = sync.NewCond(&s.mu)
	s.callbacksDrained = sync.NewCond(&s.mu)
	return s
}

// publishReady 在创建事务全部成功后发布 ready(§8 NewInstance 第
// 5 步:复核 Runtime 状态并发布 ready Instance)。调用契约:只能对
// 同一次创建、在 initializing 阶段调用;其他阶段到达此处意味着私有
// 状态机内部不变量被破坏,按编程错误 panic(对照 lifecycleGate.
// publish 先例)。
func (s *instanceState) publishReady() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseInitializing {
		panic(fmt.Sprintf("maa: instance state publish from phase %d", s.phase))
	}
	s.phase = phaseReady
}

// callLease 是一次已准入业务调用的在途凭证:准入在状态锁内完成,
// 覆盖排队等待 raw 门禁、native 调用、输出复制与结果登记全过程
// (§7)。零值不可用;准入与释放一一对应,推荐 defer release。
type callLease struct {
	s *instanceState
}

// admitCall 准入一次业务调用:在状态锁内检查阶段,ready 递增在途
// 计数并返回 lease;其余阶段拒绝新调用,返回带 what 上下文文本、可
// 经 errors.Is 判别的错误(风格对照 runtime.go 的错误包装):
//   - closing:包装 native.ErrClosing;
//   - faulted:包装首次故障的原始错误——调用方沿错误链可达真正的
//     失败原因(含其包装的 native 哨兵),faulted 拒绝因此与实例
//     首因共享同一错误身份;
//   - closed:包装 native.ErrClosed;
//   - initializing:实例未发布前公开面拿不到它,业务调用不可达,
//     到达此处即内部不变量破坏,panic。
//
// 已准入调用在 closing/faulted/closed 阶段照常释放 lease(§7.1:
// 故障时仍在 raw 门禁前的预留也释放 lease),release 不检查阶段。
func (s *instanceState) admitCall(what string) (callLease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseReady:
		s.calls++
		return callLease{s: s}, nil
	case phaseClosing:
		return callLease{}, fmt.Errorf("maa: instance is closing, %s rejected: %w", what, native.ErrClosing)
	case phaseFaulted:
		if s.firstFault == nil {
			panic("maa: faulted instance state without a recorded first fault")
		}
		return callLease{}, fmt.Errorf("maa: %s rejected by instance fault: %w", what, s.firstFault)
	case phaseClosed:
		return callLease{}, fmt.Errorf("maa: instance is closed, %s rejected: %w", what, native.ErrClosed)
	default:
		// initializing:业务调用只存在于发布之后;其余值不可达。
		panic(fmt.Sprintf("maa: instance state admits a call in phase %d", s.phase))
	}
}

// release 释放在途凭证:在状态锁内递减,归零时广播唤醒等待在途
// 清零的关闭所有者。释放不检查阶段——已准入调用在任何阶段都必须
// 能正常退出,否则关闭等待无法收敛。
func (l callLease) release() {
	if l.s == nil {
		panic("maa: release of a zero call lease")
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if l.s.calls == 0 {
		panic("maa: call lease released more times than admitted")
	}
	l.s.calls--
	if l.s.calls == 0 {
		l.s.callsDrained.Broadcast()
	}
}

// handle 返回本次调用可用的实例 handle:只能在持有本 lease 期间、
// 按调用纪律处于 raw 门禁段内读取,并立即用于本次 native 调用;
// 不得外泄、缓存或另存副本(§8:不能另存一份裸 handle 绕过状态
// 检查)。发布后的实例 handle 必须非零(§3:0 无效,不解引用),
// 为 0 即内部不变量破坏,panic。
func (l callLease) handle() uintptr {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if l.s.handle == 0 {
		panic("maa: admitted call on an instance state without a valid handle")
	}
	return l.s.handle
}

// rawGate 是独立于状态锁的 raw 串行门禁(§7:普通调用先取得调用
// lease,再等待独立的 raw 串行门禁;任何 native 调用都不持有
// stateMu 或注册表锁)。同一时刻至多一个调用处于 enter 与 exit 之
// 间;门禁前的排队者已持有调用 lease、计入在途计数,关闭等待因此
// 不会漏掉它们。
//
// 调用纪律:admitCall 取得 lease → enter →(读取 handle、native
// 调用、输出复制、结果登记)→ exit → release。enter/exit 之间绝不
// 触碰 instanceState.mu,也不执行任何会重新进入本门禁的调用。
type rawGate struct {
	mu sync.Mutex
}

// enter 占据 raw 段;已持有 lease 的调用者在此排队。
func (g *rawGate) enter() { g.mu.Lock() }

// exit 释放 raw 段。
func (g *rawGate) exit() { g.mu.Unlock() }

// electClose 在状态锁内选主(§8 第 1 步:在 stateMu 下选出唯一关闭
// 所有者并转为 closing,拒绝新业务调用):ready/faulted 阶段恰一个
// 申请者赢得所有权,阶段转为 closing,返回 true;其余申请者——并发
// 竞争失败者与之后的幂等申请(closing 关闭进行中、closed 已完成)——
// 不成为所有者,返回 false,经 waitClose 等待同一 closeDone 并取得
// 同一最终错误。已取得 handle 的创建失败回滚先经 recordFault 进入
// faulted 再选主;initializing 阶段不存在可关闭的已发布实例,到达
// 此处即内部不变量破坏,panic。
func (s *instanceState) electClose() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseReady, phaseFaulted:
		s.phase = phaseClosing
		return true
	case phaseClosing, phaseClosed:
		return false
	default:
		panic(fmt.Sprintf("maa: instance state elects close in phase %d", s.phase))
	}
}

// waitCallsDrained 等待已准入调用全部释放(§8 第 2 步:不持状态锁
// 等待所有已准入调用退出,包括尚在 raw 门禁前排队的调用——它们先
// 取得 lease,已计入在途计数)。谓词在状态锁内复查,Cond.Wait 挂起
// 期间释放锁,在途调用可正常登记与释放并触发广播;closing 阶段拒绝
// 新准入,计数归零后不再增加。不设期限,也不用 context 取消强行使
// native 调用返回:关闭超时交给 worker 的外部监督处理(§8)。由
// 关闭所有者在 electClose 胜出后调用。
func (s *instanceState) waitCallsDrained() {
	s.mu.Lock()
	for s.calls != 0 {
		s.callsDrained.Wait()
	}
	s.mu.Unlock()
}

// closeHandle 供关闭所有者读取 handle:契约是 electClose 胜出、
// waitCallsDrained 返回之后,为执行一次 Stop 再一次 Destroy 而读取
// (§8.1);除此之外不得读取。全部调用 lease 已退出是前置条件,在途
// 计数非零到达此处即内部不变量破坏,panic。ok 为 false 表示不存在
// 有效非零 handle(CreateEx=0 的路径不执行 Stop/Destroy)。
func (s *instanceState) closeHandle() (uintptr, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls != 0 {
		panic(fmt.Sprintf("maa: close handle read with %d calls in flight", s.calls))
	}
	return s.handle, s.handle != 0
}

// recordFault 在状态锁内线性化记录首次故障(§7.1:保留首个 fault,
// 拒绝新业务调用),无原文前缀;带前缀的记录经 recordFaultWithDetail。
func (s *instanceState) recordFault(cause error) {
	s.recordFaultWithDetail(cause, nil)
}

// recordFaultWithDetail 在状态锁内线性化记录首次故障并可选保留一段
// 有界原文前缀(§7.1:保留首个 fault,拒绝新业务调用;首次故障只
// 保留有界元数据和至多 faultRawPrefixBytes 字节原文前缀):
//
//   - ready/initializing 阶段随即进入 faulted,此后 admitCall 一律
//     拒绝,已准入 lease 照常释放;
//   - closing 阶段阶段不变,故障仅作为首因记录,随 publishFinalError
//     进入最终错误;
//   - faulted 重复记录保留首个,不覆盖首因、不覆盖前缀、不积累;
//   - closed 到达此处即内部不变量破坏(关闭完成后不应再有故障源),
//     panic。
//
// cause 不得为 nil:故障必须携带可判别的原始错误,不接受无因故障。
// prefix 允许为 nil;非 nil 时截取至多 faultRawPrefixBytes 字节并在
// 首次故障时立即深拷贝,调用者返回后修改原切片不影响已保存副本。
// 当本调用使 firstFault 从 nil 变为非 nil(即本调用是首次故障的
// 线性化点)时,恰好关闭一次 faultDone;重复记录不重复关闭。
func (s *instanceState) recordFaultWithDetail(cause error, prefix []byte) {
	if cause == nil {
		panic("maa: instance fault recorded without a cause")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseInitializing, phaseReady:
		s.keepFirstFaultLocked(cause, prefix)
		s.phase = phaseFaulted
	case phaseFaulted, phaseClosing:
		s.keepFirstFaultLocked(cause, prefix)
	case phaseClosed:
		panic(fmt.Sprintf("maa: instance fault recorded in phase %d", s.phase))
	default:
		panic(fmt.Sprintf("maa: instance state in unknown phase %d", s.phase))
	}
	// 首次故障通知(§7.1:独立于事件队列的一次性通知):恰在首次
	// 故障的线性化点关闭,重复记录被 faultNotified 拦下,正常关闭
	// 永不触发。
	if !s.faultNotified {
		s.faultNotified = true
		close(s.faultDone)
	}
}

// keepFirstFaultLocked 保留首个故障与其有界原文前缀;要求持有 mu。
// sticky:已有首因时不做任何事(不覆盖错误、不覆盖前缀、不积累)。
func (s *instanceState) keepFirstFaultLocked(cause error, prefix []byte) {
	if s.firstFault != nil {
		return
	}
	if len(prefix) > faultRawPrefixBytes {
		prefix = prefix[:faultRawPrefixBytes]
	}
	s.firstFault = cause
	s.firstFaultPrefix = bytes.Clone(prefix)
}

// publishFinalError 由关闭所有者在原生销毁与 callback 清理完成之后、
// publishClosed 之前发布不可变最终错误(§8 第 5 步;§8.1:已有首次
// 故障/创建失败是主因,停止失败是唯一附加清理诊断,没有主因时停止
// 拒绝才成为 Close/Err 的原因——组合归 MAA-20,本原语只负责存储与
// 一次性语义)。只首次生效:finalErr 一经设置不再被任何后续发布
// 覆盖;所有关闭申请共享同一存储,经 finalError/waitClose 取得同一
// 份。仅在 closing 阶段可调用,其他阶段到达此处即内部不变量破坏,
// panic。
func (s *instanceState) publishFinalError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseClosing {
		panic(fmt.Sprintf("maa: instance final error published in phase %d", s.phase))
	}
	if s.finalErr == nil {
		s.finalErr = err
	}
}

// publishClosed 由关闭所有者发布 closed 终态并关闭共享 closeDone
// (§8 第 5 步:先发布最终错误和 closed 状态,再关闭 Done 与
// closeDone;观察到 closeDone 后 Err 已稳定)。调用契约:仅所有者
// 在 publishFinalError 之后调用;callback 准入门必须已关闭且已准入
// callback 全部退出(§8 第 4 步先于第 5 步)。通道只关闭一次;
// 违反契约即内部不变量破坏,panic。
func (s *instanceState) publishClosed() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseClosing {
		panic(fmt.Sprintf("maa: instance closed published in phase %d", s.phase))
	}
	if !s.callbackGateClosed {
		panic("maa: instance closed published with the callback admission gate open")
	}
	if s.callbacks != 0 {
		panic(fmt.Sprintf("maa: instance closed published with %d callbacks in flight", s.callbacks))
	}
	s.phase = phaseClosed
	close(s.closeDone)
}

// done 返回共享 closeDone 的只读视图:非所有者经它等待同一关闭
// 结果,或以 select 观察关闭完成。通道字段构造后不再改变,读取无需
// 状态锁;只在 publishClosed 中关闭一次。
func (s *instanceState) done() <-chan struct{} {
	return s.closeDone
}

// waitClose 等待共享 closeDone 并返回已稳定的最终错误:非所有者的
// 关闭申请经它取得与所有者一致的结果(§8.1:多次 Close 返回同一
// 终态)。观察到 closeDone 关闭后 finalErr 必已发布且不再变化
// (§8 第 5 步先发布最终错误,再关闭通道)。
func (s *instanceState) waitClose() error {
	<-s.closeDone
	return s.finalError()
}

// finalError 返回已发布的最终错误(尚未发布返回 nil)。发布前的
// 读取不构成稳定观察:稳定性以 closeDone 关闭为界。
func (s *instanceState) finalError() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalErr
}

// currentPhase 返回当前阶段(只读诊断辅助,无迁移语义)。
func (s *instanceState) currentPhase() instancePhase {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phase
}

// currentFault 返回已记录的首次故障(尚无返回 nil)。faulted 阶段
// 即可经此观察首因(§8.1:Err 在 faulted 时即可见首次故障);最终
// 错误的稳定观察仍以 closeDone 与 finalError 为准。
func (s *instanceState) currentFault() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstFault
}

// faultNotify 返回一次性故障通知的只读视图:首次故障线性化时关闭
// 一次(§7.1:通知独立于事件队列,绝不依赖 Events 还有空间;观察者
// 可同时选择 done()——Done 仍只表示关闭完成),正常关闭不触发。
// 通道字段构造后不再改变,读取无需状态锁。
func (s *instanceState) faultNotify() <-chan struct{} {
	return s.faultDone
}

// recordLostEvents 把 n 条丢失事件计入饱和丢失计数(§7.1:丢失条数
// 采用饱和 uint64 计数):相加将回绕时夹到 math.MaxUint64,不回绕。
// 纯诊断计数,不参与阶段迁移,可在任何未关闭阶段调用。
func (s *instanceState) recordLostEvents(n uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	total, ok := addChecked(s.lostEvents, n)
	if !ok {
		total = math.MaxUint64
	}
	s.lostEvents = total
}

// currentLostEvents 返回当前丢失计数(只读诊断)。
func (s *instanceState) currentLostEvents() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lostEvents
}

// faultSnapshot 是首次故障的有界诊断快照(§7.1:Diagnostics 返回
// 丢失条数、首错和停止诊断的深副本):lostEvents 是饱和丢失计数,
// firstFault 是首因(可能尚为 nil),rawPrefix 是首因原文前缀的
// 独立深拷贝(至多 faultRawPrefixBytes 字节;未保留前缀时为 nil)。
// 修改副本不影响状态内的原值。
type faultSnapshot struct {
	lostEvents uint64
	firstFault error
	rawPrefix  []byte
}

// faultDiagnostics 返回故障诊断的深拷贝快照:丢失计数、首因与原文
// 前缀各自独立复制,供后续公开 Diagnostics 接线与用例消费;数据
// 读取与复制在状态锁内完成,关闭完成前内容仍可能增加。
func (s *instanceState) faultDiagnostics() faultSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return faultSnapshot{
		lostEvents: s.lostEvents,
		firstFault: s.firstFault,
		rawPrefix:  bytes.Clone(s.firstFaultPrefix),
	}
}

// callbackLease 是一条已准入 callback 的在途凭证(§4:查到 token
// 后必须取得 callback lease,才可读取 details 指针;准入失败则不
// 读取原生 details)。复制 details、投递事件等全部原生内存访问都
// 必须发生在持有本 lease 期间;零值不可用,准入与退出一一对应,
// 推荐 defer release。
type callbackLease struct {
	s *instanceState
}

// admitCallback 尝试准入一条 callback:准入门开放时递增已准入计数
// 并返回 lease;门已关闭返回 false,调用方不得读取原生 details,
// 立即退出(§4:与关闭形成明确的准入顺序,不留「取得状态后尚未
// 登记在途引用就释放状态」的窗口)。closing 阶段门仍开放(§7:仍
// 允许必要的原生消息和 Destroyed 复制入队)。callback 不取得 raw
// 串行门禁(§7)。
func (s *instanceState) admitCallback() (callbackLease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callbackGateClosed {
		return callbackLease{}, false
	}
	s.callbacks++
	return callbackLease{s: s}, true
}

// release 退出一条已准入 callback:在状态锁内递减,归零时广播唤醒
// 等待 callback 清零的关闭流程。不检查阶段——已准入 callback 在
// 任何阶段都必须能正常退出,否则关闭等待无法收敛。
func (l callbackLease) release() {
	if l.s == nil {
		panic("maa: release of a zero callback lease")
	}
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if l.s.callbacks == 0 {
		panic("maa: callback lease released more times than admitted")
	}
	l.s.callbacks--
	if l.s.callbacks == 0 {
		l.s.callbacksDrained.Broadcast()
	}
}

// closeCallbackGate 关闭 callback 准入门,此后新 callback 一律准入
// 失败。由关闭流程在 Destroy 返回后调用(§8 第 4 步),或由
// CreateEx=0 的创建回滚调用(§8:关闭临时 callback 门)。至多一次,
// 重复关闭即内部不变量破坏,panic。
func (s *instanceState) closeCallbackGate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callbackGateClosed {
		panic("maa: callback admission gate closed twice")
	}
	s.callbackGateClosed = true
}

// waitCallbacksDrained 等待已准入 callback 全部退出(§8 第 4 步:
// 关门后等待,再移除 token)。Cond.Wait 挂起期间释放状态锁,已准入
// callback 可正常复制、投递并退出。不设期限:原生回调卡死依赖
// worker 进程隔离,不以持锁等待或超时伪装安全回收(§8)。
func (s *instanceState) waitCallbacksDrained() {
	s.mu.Lock()
	for s.callbacks != 0 {
		s.callbacksDrained.Wait()
	}
	s.mu.Unlock()
}
