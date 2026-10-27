// 本文件实现实例的私有共享状态与调用准入原语:实例阶段、唯一受门控的
// handle、已准入调用与 callback 计数、callback 准入门、首次故障、共享
// closeDone 与最终错误存储。它提供调用 lease 准入/释放、独立 raw 串行
// 门禁、关闭选主、在途调用清零等待、首次故障记录与终态发布原语,并作为
// callback 准入、pending Operation 结算与 Stop/Destroy 关闭流程的共同入口。
//
// 故障通知与丢失诊断在同一状态门内维护:一次性 faultDone 通知只由首次
// 故障关闭,正常关闭不触发;丢失条数采用饱和 uint64 计数,不回绕;首次
// 故障只保留有界元数据和至多 faultRawPrefixBytes 字节原文前缀(不是完整
// Event,不积累错误列表)。
//
// pending Operation 表与统一结算使用同一状态门:表容量取 Open 时
// validateLimits 已校验的 PendingOperations 快照,信任不重验;完成结算、
// 首因线性化扫描与关闭清空全部在状态锁内进行,保证任一线性化点上每条
// 记录要么仍在表中、要么已完整结算——故障后到达的完成消息因此不能覆盖
// 故障终态。operation 自身的终态存储与等待语义见 operation.go。
//
// 创建期发布原语也在同一状态门内:publishHandle 登记 CreateEx 返回的
// 句柄,tryPublishReady 在最终发布点原子复核早到故障并发布 ready——发布
// 与故障复核同锁段,早到故障的任意到达时序都只能得到「不发布 + 按首因
// 回滚」一种结论。
//
// 并发契约:
//   - 状态锁 mu 只用于检查状态、登记/释放 lease 与发布结果;任何 native
//     调用都不持有状态锁。等待在途调用清零期间绝不持有状态锁,否则已准入
//     调用无法释放 lease,关闭将死锁。
//   - 调用纪律:先取得调用 lease,再排队等待独立的 raw 串行门禁;lease
//     覆盖排队、native 调用、输出复制与结果登记全过程——门禁前排队的调用
//     已计入在途计数,关闭等待不会漏掉它们。
//   - 关闭在状态锁内选主:恰一个申请者把 ready/faulted 转为 closing 并
//     成为所有者;其余申请者共享同一 closeDone 与同一最终错误存储,只允许
//     所有者执行一次 Stop/Destroy。
//   - 阶段迁移:initializing → ready → closing → closed;异常路径进入
//     faulted 后仍经 closing 关闭。非法迁移与状态机不变量破坏按编程错误
//     panic,不允许静默继续污染状态。
//
// 错误语义:拒绝新业务调用按阶段返回可辨识错误——closing 包装
// native.ErrClosing,closed 包装 native.ErrClosed,faulted 包装首次故障的
// 原始错误;全部经 %w 保留错误链,errors.Is / errors.As 沿链可达 native
// 哨兵或真正的失败原因。已准入的 lease 在任何阶段都能正常释放,否则关闭
// 等待无法收敛。

package maa

import (
	"bytes"
	"fmt"
	"math"
	"sync"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// faultRawPrefixBytes 是首次故障保留的原文前缀字节上限(冻结:
// 首次故障及停止诊断只保留有界元数据和至多 4,096 字节原文前缀,
// 不是完整 Event,不积累错误列表)。
const faultRawPrefixBytes = 4096

// instancePhase 是实例私有生命周期的阶段(状态变化为
// initializing → ready → closing → closed,异常路径可进入 faulted
// 后关闭;faulted 不允许继续业务操作)。合法迁移只有:
//
//	initializing → ready 创建事务成功后发布(publishReady)
//	initializing → faulted 创建期早到故障(recordFault)
//	ready → faulted 业务期首次故障(recordFault)
//	ready → closing 关闭选主胜出(electClose)
//	faulted → closing 关闭选主胜出(electClose)
//	closing → closed 所有者发布终态(publishClosed)
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

// instanceState 是一个实例的唯一私有共享状态(实例只维护一份
// 私有生命周期状态,包含阶段、handle、已准入调用数、已准入
// callback 数、pending Operation 表、首次故障与共享 closeDone;
// 有界早到结果表不在本层,由本层承接)。状态内含锁,只能经
// 指针使用,不按值复制。
//
// handle 是状态路径唯一受门控的实例句柄:不设第二份裸 handle,不
// 提供绕过阶段检查的访问闭包或导出。业务路径只能经已准入
// callLease.handle 读取,关闭路径只能经所有者专用的 closeHandle
// 读取;0 表示尚未取得有效句柄(0 无效,不解引用)。
type instanceState struct {
	mu sync.Mutex

	// callsDrained 在已准入调用计数归零时广播,callbacksDrained 在
	// 已准入 callback 计数归零时广播。Cond.Wait 在挂起期间释放 mu,
	// 等待清零期间因此不持有状态锁(第 2 步)。
	callsDrained     *sync.Cond
	callbacksDrained *sync.Cond

	phase  instancePhase
	handle uintptr
	// calls 是已准入、尚未释放的业务调用数,含尚在 raw 门禁前排队
	// 的调用(先取得 lease 再排队)。
	calls uint64

	// callback 准入门:门开放时 callback 可取得 lease,
	// closing 阶段仍开放——必要的原生消息和 Destroyed 复制入队不受
	// 关闭影响;关闭流程在 Destroy 返回后关门并等待已准入 callback
	// 退出。接入策略归本层,本层只承载标志与计数。
	callbackGateClosed bool
	callbacks          uint64

	// pending 是已接受、尚未最终结算的 Operation 表(pending 在
	// 提交进入 raw 串行门禁前预留,满时直接拒绝):键为提交返回的
	// 异步 ID,准入与移除只在状态锁内进行。当前占用 = len(pending) +
	// pendingReserved(已预留、尚未登记的提交槽)。记录的终态存储与
	// 等待语义见 operation.go。
	pending map[AsyncID]*operation
	// pendingOperations 是表容量上限:只能取 Open 时 validateLimits
	// 已校验并冻结的 Limits.PendingOperations 快照(1–4,096),
	// 信任不重验;超限拒绝本次提交并包装 native.ErrOperationLimit,
	// 实例不因此故障。
	pendingOperations uint64
	// pendingReserved 是已预留、尚未登记为 pending 表项的提交槽数
	// (异步提交:预留发生在进入 raw 串行门禁之前,而登记键
	// async ID 要到 raw 返回并校验后才存在,故预留以计数持有,登记
	// 时转为 map 表项,失败时释放)。计入容量占用,保证「满时直接
	// 拒绝,不调用 native」对门禁前排队中的提交同样成立。
	pendingReserved uint64

	// asyncWatermark 是已接受异步提交的高水位 ID(binding 只接
	// 受 1..MaxInt32 且严格大于上次已接受 ID 的 raw 返回值,维护一个
	// 高水位而不保存无界的 completed-ID 集合):raw 返回值必须非负、
	// 非零且严格大于本值,负值/零/复用/倒退均为 ErrProtocol;高水位
	// 已达 MaxInt32 时在串行门禁内、进入下一次异步 raw 前直接故障。
	// 不要求 ID 连续,也不要求完成消息按 ID 顺序到达。
	asyncWatermark AsyncID

	// 早到结果窗口与有界早到表(raw 返回前可能已收到完成;:
	// 同一时刻 raw 串行门禁内至多一个「提交已进入 raw、尚未接合返回
	// ID」的窗口,故 EarlyResults 固定为 1,仅该窗口内允许临存一个未
	// 知 ID 的完成结果;raw 返回后按 token、ID、预期 what 接合并立即
	// 清空,不把残留留给未来提交)。earlyWindowOpen 只在 raw 门禁段
	// 内置位,earlyResult 非 nil 表示窗口内已存有早到结果;earlyBytes
	// 是早到结果原始 JSON 的字节上限,只能取 Open 时 validateLimits
	// 已校验并冻结的 Limits.EarlyBytes 快照(1–16,777,216,交叉
	// 校验保证 CallbackBytes ≤ EarlyBytes),信任不重验。
	earlyWindowOpen bool
	earlyResult     *completionInfo
	earlyBytes      uint64

	// firstFault 是首次故障,sticky:保留首个,不覆盖。
	firstFault error
	// firstFaultPrefix 是首次故障的有界原文前缀:只在首因线性化时
	// 深拷贝至多 faultRawPrefixBytes 字节,后续故障不覆盖、不积累
	// (不是完整 Event,不积累错误列表)。
	firstFaultPrefix []byte
	// lostEvents 是饱和丢失计数(丢失条数采用饱和 uint64 计数,
	// 不回绕;关闭完成前可能增加,观察 Done 后稳定)。
	lostEvents uint64
	// faultDone 从构造起即存在,只由首次故障在状态门内线性化关闭
	// 一次(首次故障关闭独立的一次性 Fault 通知,绝不依赖
	// Events 还有空间;正常关闭不触发)。faultNotified 是防重复关闭
	// 的哨兵,只在持有 mu 时读写。
	faultDone     chan struct{}
	faultNotified bool
	// finalErr 是关闭所有者发布的不可变最终错误;所有关闭申请共享
	// 同一存储(并发 Close 共享同一 closeDone 和最终错误)。
	finalErr error
	// closeDone 从构造起即存在,只由所有者在 publishClosed 中关闭
	// 一次;先于它的最终错误发布保证观察者见到关闭即见稳定终态。
	closeDone chan struct{}

	// raw 是独立于 mu 的 raw 串行门禁:不受状态锁保护、也不保护任何
	// 状态字段;调用纪律见 rawGate。
	raw rawGate
}

// newInstanceState 构造实例共享状态:不依赖 NewInstance,供创建
// 接线与受控用例复用。handle 允许传 0(真实创建在
// CreateEx 返回前尚未取得句柄)或非零占位值(受控用例构造);业务
// 调用路径读取到 0 视为内部不变量破坏。pendingOperations 是 pending
// Operation 表容量上限,earlyBytes 是早到结果窗口的字节上限,两者
// 只能取 Open 时 validateLimits 已校验并冻结的 Limits 快照值,信任
// 不重验(受控用例传充裕值)。初始阶段为 initializing,创建成功后
// 经 publishReady 发布;closeDone 在构造时建好,保证并发关闭申请从
// 构造起共享同一通道。
func newInstanceState(handle uintptr, pendingOperations uint64, earlyBytes uint64) *instanceState {
	s := &instanceState{
		phase:             phaseInitializing,
		handle:            handle,
		pending:           make(map[AsyncID]*operation),
		pendingOperations: pendingOperations,
		earlyBytes:        earlyBytes,
		closeDone:         make(chan struct{}),
		faultDone:         make(chan struct{}),
	}
	s.callsDrained = sync.NewCond(&s.mu)
	s.callbacksDrained = sync.NewCond(&s.mu)
	return s
}

// publishReady 在创建事务全部成功后发布 ready(NewInstance 第
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

// publishHandle 在创建事务中登记 AsstCreateEx 返回的有效非零句柄:
// CreateEx 调用前状态必须已建立(handle=0 构造),返回后由创建者把
// 句柄写入唯一受门控的 handle 字段。只允许调用一次,重复登记或以 0
// 登记即内部不变量破坏,panic(0 无效,不解引用)。
//
// 允许的阶段是 initializing 与 faulted:创建期间的早到故障可能先于
// CreateEx 返回进入 faulted(回调在原生线程上并发准入),而回滚
// 路径仍需要句柄执行一次 Stop 再一次 Destroy(已创建 handle 后
// 的失败执行同一销毁流程),因此 faulted 阶段照常登记。closing 及
// 之后到达此处即内部不变量破坏,panic。锁序:创建流程在持有
// Runtime 状态门(lifecycleGate.mu)之外调用本方法;本方法只取状态
// 锁,不反向获取任何 Runtime 锁。
func (s *instanceState) publishHandle(handle uintptr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if handle == 0 {
		panic("maa: instance handle published as 0")
	}
	if s.handle != 0 {
		panic(fmt.Sprintf("maa: instance handle %#x published over existing handle %#x", handle, s.handle))
	}
	switch s.phase {
	case phaseInitializing, phaseFaulted:
		s.handle = handle
	default:
		panic(fmt.Sprintf("maa: instance handle published in phase %d", s.phase))
	}
}

// tryPublishReady 在创建事务的最终发布点原子复核并发布 ready(
// NewInstance 第 6 步:复核 Runtime 状态并发布 ready Instance;
// 冻结:早到故障会使创建失败):仍在 initializing 时发布 ready 并返
// 回 nil;已因早到故障进入 faulted 时返回该首因、不发布——发布与
// 故障复核在同一状态锁段内完成,故障早于、晚于选项应用或恰在发布
// 前到达都只能得到「不发布 + 按首因回滚」一种结论,不存在「已
// faulted 仍 publishReady」的 panic 窗口。其余阶段到达此处即内部不
// 变量破坏,panic。锁序:由 Runtime 状态门的发布决策持有
// lifecycleGate.mu 时调用(Runtime 门 → 状态锁,无反向获取)。
func (s *instanceState) tryPublishReady() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseInitializing:
		s.phase = phaseReady
		return nil
	case phaseFaulted:
		if s.firstFault == nil {
			panic("maa: faulted instance state without a recorded first fault")
		}
		return s.firstFault
	default:
		panic(fmt.Sprintf("maa: instance state tries to publish from phase %d", s.phase))
	}
}

// callLease 是一次已准入业务调用的在途凭证:准入在状态锁内完成,
// 覆盖排队等待 raw 门禁、native 调用、输出复制与结果登记全过程
// 。零值不可用;准入与释放一一对应,推荐 defer release。
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
// 已准入调用在 closing/faulted/closed 阶段照常释放 lease(
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
// 不得外泄、缓存或另存副本(不能另存一份裸 handle 绕过状态
// 检查)。发布后的实例 handle 必须非零(0 无效,不解引用),
// 为 0 即内部不变量破坏,panic。
func (l callLease) handle() uintptr {
	l.s.mu.Lock()
	defer l.s.mu.Unlock()
	if l.s.handle == 0 {
		panic("maa: admitted call on an instance state without a valid handle")
	}
	return l.s.handle
}

// rawGate 是独立于状态锁的 raw 串行门禁(普通调用先取得调用
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

// electClose 在状态锁内选主(第 1 步:在 stateMu 下选出唯一关闭
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

// waitCallsDrained 等待已准入调用全部释放(第 2 步:不持状态锁
// 等待所有已准入调用退出,包括尚在 raw 门禁前排队的调用——它们先
// 取得 lease,已计入在途计数)。谓词在状态锁内复查,Cond.Wait 挂起
// 期间释放锁,在途调用可正常登记与释放并触发广播;closing 阶段拒绝
// 新准入,计数归零后不再增加。不设期限,也不用 context 取消强行使
// native 调用返回:关闭超时交给 worker 的外部监督处理。由
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
// 除此之外不得读取。全部调用 lease 已退出是前置条件,在途
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

// recordFault 在状态锁内线性化记录首次故障(保留首个 fault,
// 拒绝新业务调用),无原文前缀;带前缀的记录经 recordFaultWithDetail。
func (s *instanceState) recordFault(cause error) {
	s.recordFaultWithDetail(cause, nil)
}

// recordFaultWithDetail 在状态锁内线性化记录首次故障并可选保留一段
// 有界原文前缀(保留首个 fault,拒绝新业务调用;首次故障只
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
// 线性化点)时,恰好关闭一次 faultDone;重复记录不重复关闭。同一
// 线性化点将当时全部 pending Operation 统一结为该故障原因(
// 等待语义见 operation.go),故障后到达的完成消息因此不能覆盖故障
// 终态;重复故障不重复扫描。
func (s *instanceState) recordFaultWithDetail(cause error, prefix []byte) {
	if cause == nil {
		panic("maa: instance fault recorded without a cause")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faultLocked(cause, prefix)
}

// faultLocked 是 recordFaultWithDetail 的锁内实现,供已在状态锁段内
// 的调用方(提交接合与完成关联)复用同一首因线性化语义;
// 要求持有 mu。语义与锁序见 recordFaultWithDetail。
func (s *instanceState) faultLocked(cause error, prefix []byte) {
	first := false
	switch s.phase {
	case phaseInitializing, phaseReady:
		first = s.keepFirstFaultLocked(cause, prefix)
		s.phase = phaseFaulted
	case phaseFaulted, phaseClosing:
		first = s.keepFirstFaultLocked(cause, prefix)
	case phaseClosed:
		panic(fmt.Sprintf("maa: instance fault recorded in phase %d", s.phase))
	default:
		panic(fmt.Sprintf("maa: instance state in unknown phase %d", s.phase))
	}
	if first {
		// 首因线性化扫描(立即将该时点尚未结算的 Operation 统
		// 一结为该故障,清空表):扫描与完成结算同锁段串行化,故障后
		// 的完成消息查不到记录,只能按未命中放弃。
		for id, op := range s.pending {
			delete(s.pending, id)
			op.settleFault(cause)
		}
	}
	// 首次故障通知(独立于事件队列的一次性通知):恰在首次
	// 故障的线性化点关闭,重复记录被 faultNotified 拦下,正常关闭
	// 永不触发。
	if !s.faultNotified {
		s.faultNotified = true
		close(s.faultDone)
	}
}

// keepFirstFaultLocked 保留首个故障与其有界原文前缀;要求持有 mu。
// sticky:已有首因时不做任何事(不覆盖错误、不覆盖前缀、不积累)
// 并返回 false;实际记录首因时返回 true,供调用方区分首次故障的线
// 性化点。
func (s *instanceState) keepFirstFaultLocked(cause error, prefix []byte) bool {
	if s.firstFault != nil {
		return false
	}
	if len(prefix) > faultRawPrefixBytes {
		prefix = prefix[:faultRawPrefixBytes]
	}
	s.firstFault = cause
	s.firstFaultPrefix = bytes.Clone(prefix)
	return true
}

// publishFinalError 由关闭所有者在原生销毁与 callback 清理完成之后、
// publishClosed 之前发布不可变最终错误(第 5 步;:已有首次
// 故障/创建失败是主因,停止失败是唯一附加清理诊断,没有主因时停止
// 拒绝才成为 Close/Err 的原因——组合归本层,本原语只负责存储与
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
// (第 5 步:先发布最终错误和 closed 状态,再关闭 Done 与
// closeDone;观察到 closeDone 后 Err 已稳定)。调用契约:仅所有者
// 在 publishFinalError 之后调用;callback 准入门必须已关闭且已准入
// callback 全部退出(第 4 步先于第 5 步)。通道只关闭一次;
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
// 关闭申请经它取得与所有者一致的结果(多次 Close 返回同一
// 终态)。观察到 closeDone 关闭后 finalErr 必已发布且不再变化
// (第 5 步先发布最终错误,再关闭通道)。
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
// 即可经此观察首因(Err 在 faulted 时即可见首次故障);最终
// 错误的稳定观察仍以 closeDone 与 finalError 为准。
func (s *instanceState) currentFault() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.firstFault
}

// faultNotify 返回一次性故障通知的只读视图:首次故障线性化时关闭
// 一次(通知独立于事件队列,绝不依赖 Events 还有空间;观察者
// 可同时选择 done()——Done 仍只表示关闭完成),正常关闭不触发。
// 通道字段构造后不再改变,读取无需状态锁。
func (s *instanceState) faultNotify() <-chan struct{} {
	return s.faultDone
}

// recordLostEvents 把 n 条丢失事件计入饱和丢失计数(丢失条数
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

// faultSnapshot 是首次故障的有界诊断快照(Diagnostics 返回
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

// callbackLease 是一条已准入 callback 的在途凭证(查到 token
// 后必须取得 callback lease,才可读取 details 指针;准入失败则不
// 读取原生 details)。复制 details、投递事件等全部原生内存访问都
// 必须发生在持有本 lease 期间;零值不可用,准入与退出一一对应,
// 推荐 defer release。
type callbackLease struct {
	s *instanceState
}

// admitCallback 尝试准入一条 callback:准入门开放时递增已准入计数
// 并返回 lease;门已关闭返回 false,调用方不得读取原生 details,
// 立即退出(与关闭形成明确的准入顺序,不留「取得状态后尚未
// 登记在途引用就释放状态」的窗口)。closing 阶段门仍开放(仍
// 允许必要的原生消息和 Destroyed 复制入队)。callback 不取得 raw
// 串行门禁。
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
// 失败。由关闭流程在 Destroy 返回后调用(第 4 步),或由
// CreateEx=0 的创建回滚调用(关闭临时 callback 门)。至多一次,
// 重复关闭即内部不变量破坏,panic。
func (s *instanceState) closeCallbackGate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callbackGateClosed {
		panic("maa: callback admission gate closed twice")
	}
	s.callbackGateClosed = true
}

// waitCallbacksDrained 等待已准入 callback 全部退出(第 4 步:
// 关门后等待,再移除 token)。Cond.Wait 挂起期间释放状态锁,已准入
// callback 可正常复制、投递并退出。不设期限:原生回调卡死依赖
// worker 进程隔离,不以持锁等待或超时伪装安全回收。
func (s *instanceState) waitCallbacksDrained() {
	s.mu.Lock()
	for s.callbacks != 0 {
		s.callbacksDrained.Wait()
	}
	s.mu.Unlock()
}

// admitPendingOperation 准入一条已知 ID 的 pending Operation:
// ready 阶段在容量内创建 operation 插入表并返回;其余阶段拒绝
// 本次提交,返回带 what 上下文文本、可经 errors.Is 判别的错误(风格
// 对照 admitCall)。容量占用含异步提交已预留、尚未登记的槽
// 位(pendingReserved),与 reservePendingOperation 共用同一上限:
//   - 容量满:包装 native.ErrOperationLimit,实例不因此故障,已接受
//     的提交与其他资源不受影响;
//   - closing:包装 native.ErrClosing;
//   - faulted:包装首次故障的原始错误——提交沿错误链可达真正的失败
//     原因,与 admitCall 的 faulted 拒绝共享同一错误身份;
//   - closed:包装 native.ErrClosed;
//   - initializing:提交只存在于发布之后,到达此处即内部不变量破坏,
//     panic。
//
// id 为 0 属编程错误(ids.go:0 是异步提交的失败值,不会作为已接受
// 提交的 ID 准入),panic。what 目前只进入拒绝文本作诊断。本入口供
// 已知 ID 的直接准入(用例与受控场景)使用;公开异步提交的
// 准入走 reservePendingOperation(raw 返回前 ID 未知,先预留后登记)。
func (s *instanceState) admitPendingOperation(id AsyncID, what string) (*operation, error) {
	if id == 0 {
		panic("maa: pending operation admitted with the zero async id")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseReady:
		if s.pendingOccupiedLocked() >= s.pendingOperations {
			return nil, fmt.Errorf("maa: pending operation limit %d reached, %s rejected: %w",
				s.pendingOperations, what, native.ErrOperationLimit)
		}
		op := newOperation(id)
		s.pending[id] = op
		return op, nil
	case phaseClosing:
		return nil, fmt.Errorf("maa: instance is closing, %s rejected: %w", what, native.ErrClosing)
	case phaseFaulted:
		if s.firstFault == nil {
			panic("maa: faulted instance state without a recorded first fault")
		}
		return nil, fmt.Errorf("maa: %s rejected by instance fault: %w", what, s.firstFault)
	case phaseClosed:
		return nil, fmt.Errorf("maa: instance is closed, %s rejected: %w", what, native.ErrClosed)
	default:
		// initializing:业务提交只存在于发布之后;其余值不可达。
		panic(fmt.Sprintf("maa: instance state admits a pending operation in phase %d", s.phase))
	}
}

// pendingOccupiedLocked 返回 pending 表的当前容量占用:已登记表项数
// 加上已预留、尚未登记的提交槽;要求持有 mu。
func (s *instanceState) pendingOccupiedLocked() uint64 {
	occ, ok := addChecked(uint64(len(s.pending)), s.pendingReserved)
	if !ok {
		// len(pending) ≤ PendingOperations ≤ 4,096 且预留同界,加法
		// 不可能回绕;防御分支保持占用语义不被伪造。
		panic("maa: pending occupancy accounting overflowed")
	}
	return occ
}

// settleCompletion 是事件分发器的完成结算与关联接入(结算、
//
//	关联;:最终结果由 AsyncCallInfo 的 async_call_id、what、
//
// details.ret 确认;:回调先查 pending,不在 pending 且 ID 不高
// 于高水位的完成是重复/旧结果,不允许放进 early 窗口)。仅由唯一
// dispatcher goroutine 调用,与提交接合(finishAsyncRaw)、首因线性
// 化扫描、关闭清空共用状态锁,保证任一线性化点上每条记录要么仍在
// 表中、要么已完整结算。分支语义(全部在状态锁内线性化):
//   - ID 命中 pending:移除记录(PendingOperations 容量就此回收)并
//     单次结算 operation——成功写结果,ret=false 同时写结果与包装
//     native.ErrNativeRejected 的错误(早到结果由提交接合方结算,
//     见 finishAsyncRaw;两条路径经 pending 表互斥,恰一胜出);
//   - 未命中且早到窗口开放:窗口内已存有早到结果时,第二个早到结果
//     (含重复)为 ErrProtocol 故障;否则 ID 不高于高水位的重复/旧
//     结果为 ErrProtocol 故障(不放进 early 窗口),未知 ID 临存进早
//     到结果表——字节超 EarlyBytes 为 ErrEventOverflow 故障,不缓存;
//   - 未命中且无窗口:重复/旧结果(ID ≤ 高水位)与无窗口的未知 ID
//     均为 ErrProtocol 故障——已完成 ID 的重复回调不能伪造新的成功,
//     无高水位归因的未知 ID 不能静默丢弃矛盾证据。
//
// 故障保留首因(sticky)并经首因线性化扫描立即结算全部未决记录;
// 消息本身仍由 dispatcher 按公开预算交付,不在此处计丢失。what 不
// 在本入口与表项比对:关联键是 token + ID,预期 what 的校验发生在
// 早到接合(finishAsyncRaw)。
func (s *instanceState) settleCompletion(info completionInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if op, ok := s.pending[info.ID]; ok {
		delete(s.pending, info.ID)
		op.complete(newResultRecord(info.ID, info.What, info.OK, info.Details))
		return
	}
	if s.earlyWindowOpen {
		if s.earlyResult != nil {
			s.faultLocked(fmt.Errorf("maa: a second early completion (id %d, what %q) arrived while "+
				"the early-result window already holds id %d: %w",
				info.ID, info.What, s.earlyResult.ID, native.ErrProtocol), nil)
			return
		}
		if info.ID <= s.asyncWatermark {
			s.faultLocked(fmt.Errorf("maa: a completion for already-accepted id %d (what %q) arrived "+
				"again while a submission is inside the raw gate: %w", info.ID, info.What, native.ErrProtocol), nil)
			return
		}
		if uint64(info.Bytes) > s.earlyBytes {
			s.faultLocked(fmt.Errorf("maa: early completion (id %d) payload %d bytes exceed Limits.EarlyBytes %d: %w",
				info.ID, info.Bytes, s.earlyBytes, native.ErrEventOverflow), nil)
			return
		}
		stored := info
		s.earlyResult = &stored
		return
	}
	if info.ID <= s.asyncWatermark {
		s.faultLocked(fmt.Errorf("maa: a duplicate completion for already-accepted id %d (what %q) "+
			"arrived after settlement: %w", info.ID, info.What, native.ErrProtocol), nil)
		return
	}
	s.faultLocked(fmt.Errorf("maa: a completion for unknown id %d (what %q) arrived with no early-result "+
		"window open: %w", info.ID, info.What, native.ErrProtocol), nil)
}

// reservePendingOperation 为一次异步提交预留 pending 容量并登记未绑
// 定 ID 的请求 Operation:每次提交先建立 Go 请求/
// Operation 状态再进入 raw;pending 在调用进入 raw 串行门禁前
// 预留,满时直接拒绝。ready 阶段在容量内递增预留计数、创建未绑定
// ID 的 operation 并返回;登记(finishAsyncRaw)把预留转为表项,失
// 败路径的释放见 releasePendingReservation。其余阶段的拒绝语义与
// admitPendingOperation 一致(closing/faulted/closed 各按哨兵拒绝,
// initializing 为编程错误 panic);容量满返回包装
// native.ErrOperationLimit 的错误,实例不因此故障。
//
// 返回的 operation 尚无关联 ID(公开面拿不到),ID 在 raw 返回并校
// 验后由 finishAsyncRaw 恰一次绑定;调用方在预留与登记/释放之间不
// 得把记录交给任何结算路径。
func (s *instanceState) reservePendingOperation(what string) (*operation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch s.phase {
	case phaseReady:
		if s.pendingOccupiedLocked() >= s.pendingOperations {
			return nil, fmt.Errorf("maa: pending operation limit %d reached, %s rejected: %w",
				s.pendingOperations, what, native.ErrOperationLimit)
		}
		s.pendingReserved++
		return newPendingOperation(), nil
	case phaseClosing:
		return nil, fmt.Errorf("maa: instance is closing, %s rejected: %w", what, native.ErrClosing)
	case phaseFaulted:
		if s.firstFault == nil {
			panic("maa: faulted instance state without a recorded first fault")
		}
		return nil, fmt.Errorf("maa: %s rejected by instance fault: %w", what, s.firstFault)
	case phaseClosed:
		return nil, fmt.Errorf("maa: instance is closed, %s rejected: %w", what, native.ErrClosed)
	default:
		panic(fmt.Sprintf("maa: instance state reserves a pending operation in phase %d", s.phase))
	}
}

// releasePendingReservation 释放一次已预留、尚未登记的提交槽(容量
// 就此回收):提交被拒(高水位耗尽、ID 0、ID 非法、实例已故障)或
// 不再登记时调用。未预留即释放或重复释放即内部不变量破坏,panic。
func (s *instanceState) releasePendingReservation() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releasePendingReservationLocked()
}

// releasePendingReservationLocked 是 releasePendingReservation 的锁内
// 实现;要求持有 mu。
func (s *instanceState) releasePendingReservationLocked() {
	if s.pendingReserved == 0 {
		panic("maa: pending reservation released more times than reserved")
	}
	s.pendingReserved--
}

// beginAsyncRaw 在 raw 串行门禁段内、进入异步 native 调用前执行
// (高水位已达 MaxInt32 时在串行门禁内、进入下一次异
// 步 raw 前直接故障,禁止让原生计数回绕):
//   - 高水位已达 MaxInt32:以 ErrProtocol 故障(原生计数耗尽,binding
//     拒绝让 ID 回绕),在状态锁内回收本次预留并返回错误——调用方不
//     得进入 native,记录作废(每份预留在拒绝/结算竞争中只回收
//     一次,与 finishAsyncRaw 的失败路径同一纪律);
//   - 否则打开早到结果窗口:此后的完成消息在 raw 返回前按未知 ID 临
//     存进有界早到表(settleCompletion),raw 返回后由 finishAsyncRaw
//     接合并清空。
//
// 调用契约:调用方已持有 raw 串行门禁(窗口互斥由此保证,同刻至多
// 一个窗口,EarlyResults 固定为 1)、已预留 pending 槽位且尚未释放;
// 违反即接线层编程错误。窗口已开放到达此处同样为编程错误,panic。
func (s *instanceState) beginAsyncRaw() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.earlyWindowOpen {
		panic("maa: async raw window opened while another window is still open")
	}
	if s.asyncWatermark == math.MaxInt32 {
		err := fmt.Errorf("maa: async call id high watermark reached MaxInt32, refusing to let the native counter wrap around: %w",
			native.ErrProtocol)
		s.faultLocked(err, nil)
		s.releasePendingReservationLocked()
		return err
	}
	s.earlyWindowOpen = true
	return nil
}

// finishAsyncRaw 在 raw 串行门禁段内、异步 native 调用返回后执行
// 返回 ID 的校验、登记与早到接合(binding 只接受
// 1..MaxInt32 且严格大于上次已接受 ID 的 raw 返回值;提交 ID=0 立即
// 释放本次预留并返回 ErrNativeRejected,若该窗口已有完成消息则为
// ErrProtocol,不能丢弃矛盾证据;raw 返回后必须按 token、ID、预期
// what 接合并立即清空)。无论结果如何,早到窗口一律关闭且早到表
// 立即清空——不把残留留给未来提交。
//
// op 是 reservePendingOperation 登记的未绑定记录;expectedWhat 是本
// 入口的预期完成 what(Connect/Click/Screencap,锁定 Core 的
// Assistant::call_proc);id 是 raw 的返回值。分支语义:
//   - 窗口未开放到达:编程错误,panic(与 beginAsyncRaw 同门禁段)。
//   - id == 0:释放预留;窗口已有早到结果时为 ErrProtocol 故障(矛盾
//     证据),否则返回包装 native.ErrNativeRejected 的提交错误;两种
//     情况都不登记、不发布成功 Operation。
//   - id < 0 或不严格大于高水位(复用/倒退):ErrProtocol 故障并释放
//     预留。
//   - 实例已故障(firstFault 非 nil):不再把记录插入 pending(
//     已经进入 raw 的动作不能撤销,返回后只补记返回 ID/诊断,不重新
//     插入 pending 或复活终态),释放预留并返回携带该 ID 与首因的
//     诊断错误;故障前已复制但尚未结算的早到结果不在此接合,窗口照
//     常清空,原文已经由 dispatcher 按事件预算交付。
//   - 登记冲突(ID 已在 pending 表):ErrProtocol 故障并释放预留
//     (严格递增的高水位使该分支防御性可达)。
//   - 正常登记:恰一次绑定 ID、预留转表项、高水位推进;窗口已有早
//     到结果时按 token、ID、预期 what 接合——ID 或 what 不符为
//     ErrProtocol 故障(首因扫描随即统一结算全部未决记录,含本条),
//     匹配则立即单次结算本条并清空早到表,返回 nil 后公开 Operation
//     已带完成终态。
//
// 返回 nil 表示提交已被接受且记录已登记(可能已由早到结果结算);调
// 用方此后把 *Operation 交给公开面。返回非 nil 表示本次提交未登记
// (预留已回收,记录作废),错误沿 errors.Is 可判别哨兵或首因。
func (s *instanceState) finishAsyncRaw(op *operation, expectedWhat string, id int32) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.earlyWindowOpen {
		panic("maa: async raw finished without an open early-result window")
	}
	early := s.earlyResult
	s.earlyResult = nil
	s.earlyWindowOpen = false

	switch {
	case id == 0:
		s.releasePendingReservationLocked()
		if early != nil {
			err := fmt.Errorf("maa: async submission (%s) returned 0 while the early-result window "+
				"already holds a completion (id %d, what %q): %w", expectedWhat, early.ID, early.What, native.ErrProtocol)
			s.faultLocked(err, nil)
			return err
		}
		return fmt.Errorf("maa: async submission (%s) returned 0 (InvalidId), submission rejected: %w",
			expectedWhat, native.ErrNativeRejected)
	case id < 0:
		s.releasePendingReservationLocked()
		err := fmt.Errorf("maa: async submission (%s) returned negative id %d: %w",
			expectedWhat, id, native.ErrProtocol)
		s.faultLocked(err, nil)
		return err
	case AsyncID(id) <= s.asyncWatermark:
		s.releasePendingReservationLocked()
		err := fmt.Errorf("maa: async submission (%s) returned id %d, not greater than the accepted "+
			"high watermark %d: %w", expectedWhat, id, s.asyncWatermark, native.ErrProtocol)
		s.faultLocked(err, nil)
		return err
	}

	if s.firstFault != nil {
		// 故障后返回的提交不重新插入 pending 或复活终态;补记
		// 返回 ID/诊断后放弃记录。
		s.releasePendingReservationLocked()
		return fmt.Errorf("maa: async submission (%s) returned id %d but the instance is already faulted: %w",
			expectedWhat, id, s.firstFault)
	}
	if _, exists := s.pending[AsyncID(id)]; exists {
		s.releasePendingReservationLocked()
		err := fmt.Errorf("maa: async submission (%s) returned id %d which is already pending: %w",
			expectedWhat, id, native.ErrProtocol)
		s.faultLocked(err, nil)
		return err
	}
	op.bindAsyncID(AsyncID(id))
	s.releasePendingReservationLocked()
	s.pending[AsyncID(id)] = op
	s.asyncWatermark = AsyncID(id)

	if early != nil {
		if early.ID != AsyncID(id) {
			err := fmt.Errorf("maa: the early completion (id %d, what %q) does not match the accepted "+
				"submission (%s, id %d): %w", early.ID, early.What, expectedWhat, id, native.ErrProtocol)
			s.faultLocked(err, nil)
			return err
		}
		if early.What != expectedWhat {
			err := fmt.Errorf("maa: the early completion (id %d) reports what %q, want %q: %w",
				early.ID, early.What, expectedWhat, native.ErrProtocol)
			s.faultLocked(err, nil)
			return err
		}
		delete(s.pending, AsyncID(id))
		op.complete(newResultRecord(early.ID, early.What, early.OK, early.Details))
	}
	return nil
}

// settleRemainingPending 把 pending 表剩余记录全部以包装
// native.ErrClosed 的错误结算一次并清空(第 5 步:无 sticky fault
// 时由统一 dispatcher 先处理已复制且可确认的结果,再结剩余项;调用
// 时序归本层——必须在 dispatcher 排空之后,使剩余记录不含可确认
// 完成)。仅 closing 阶段可调用,其他阶段到达此处即内部不变量破坏,
// panic。表已空时为无害空操作;清空由结算本身完成,不启动 goroutine、
// 不轮询。
func (s *instanceState) settleRemainingPending() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.phase != phaseClosing {
		panic(fmt.Sprintf("maa: remaining pending operations settled in phase %d", s.phase))
	}
	for id, op := range s.pending {
		delete(s.pending, id)
		op.settleClosed()
	}
}
