// 本文件实现可重复等待且只能结算一次的私有 Operation 组件与公开
// Operation 类型:每条已接受的异步提交对应一个 operation,持有一次性终态
// 存储与完成通知,允许任意次(含并发)等待;完成消息、实例关闭与 sticky
// 首因三类原因经同一单次结算路径写入终态。公开 Operation 是私有记录的
// 公开句柄,只暴露 ID 与 Wait,不经它触达任何实例状态或 raw 函数。
//
// 所有权与容量契约:operation 由实例 pending 表与调用者双方持有,调用者
// 丢弃引用不影响实例关联;只有最终结算(完成结算、早到接合、首因线性化
// 扫描、关闭清空)把记录移出 pending 表并回收 PendingOperations 容量,
// 不启动逐操作轮询或 reaper;取消一次等待、丢弃引用都不提前释放槽位。
// 已结算的 operation 自身继续保存结果,供此后任意次等待取得独立深拷贝,
// 实例关闭后副本仍有效。
//
// 等待语义:等待的超时/取消只结束本次等待,返回 ctx.Err 与零值结果,
// 不改写终态、不撤销已提交的原生动作,之后仍可再次等待取得真实结果;
// 等待与结算竞争时已发布终态优先。终态判别:完成成功返回(结果副本,
// nil);ret=false 同时返回结果副本与包装 native.ErrNativeRejected 的
// 错误(不在 OK 字段编造结构化原因);以关闭结算返回包装
// native.ErrClosed 的错误,与事件丢失(ErrEventOverflow 链)语义可辨;
// 以故障结算原样返回首因。
//
// 结算纪律:恰一次语义,重复结算即内部不变量破坏,panic;结算在
// operation 自身锁内先写终态再关闭 done,恰一次;等待者要么在 done 上
// 看到完整终态,要么继续等待,不存在中间视图。operation 自持互斥锁,
// 不经 instanceState.mu 保护;state.go 的结算入口在状态锁内调用本文件的
// 结算方法,锁序固定为状态锁 → operation 锁,operation 自身绝不回调
// 状态,无反序获取。

package maa

import (
	"context"
	"fmt"
	"sync"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// operation 是一次已接受异步提交的私有关联记录(非零 async ID
// 只证明提交成功,最终结果以 AsyncCallInfo 的 async_call_id 关联确
// 认):done 从构造起即存在,只由唯一一次结算关闭;settled 防重复结
// 算,只在持有 mu 时读写。零值不可用,只经 newOperation(已知 ID)或
// newPendingOperation + bindAsyncID(异步提交,raw 返回后绑定)构造,
// 以指针使用,不按值复制。
type operation struct {
	// asyncID 是关联提交的异步 ID:构造(或 bindAsyncID)后不可变,
	// 已结算后保持可读(已接受 Operation 的 ID 保持可读)。
	// newPendingOperation 构造的记录以 0 占位,绑定恰一次且必须非零
	// (ids.go:0 是异步提交的失败值);它是公开 Operation.ID 的语义
	// 基础。
	asyncID AsyncID

	mu      sync.Mutex
	settled bool
	// done 是一次性完成通知:构造即存在,唯一一次结算时关闭。等待
	// 者只接收,读取终态前再取 mu,保证见到的是结算完整写入的终态。
	done chan struct{}
	// result 与 hasResult 是完成结算保存的结果(hasResult 区分完成
	// 与关闭/故障两类无结果终态);cause 是结算错误:完成失败为包装
	// native.ErrNativeRejected 的错误,关闭为包装 native.ErrClosed
	// 的错误,故障为首因原样。三者只在唯一一次结算中写入,此后不可
	// 变。
	result    resultRecord
	hasResult bool
	cause     error
}

// newOperation 构造一条已知 ID 的 pending 记录:只建立终态存储与通
// 知通道,不启动 goroutine。公开异步提交(raw 返回前 ID 未知)的记
// 录经 newPendingOperation 构造、bindAsyncID 绑定,见。
func newOperation(id AsyncID) *operation {
	op := newPendingOperation()
	op.bindAsyncID(id)
	return op
}

// newPendingOperation 构造一条未绑定 ID 的 pending 记录(异步
// 提交路径;:每次提交先建立 Go 请求/Operation 状态,再进入 raw,
// 而关联键 async ID 要到 raw 返回并校验后才存在):asyncID 以 0 占位,
// 由 finishAsyncRaw 在 raw 返回、ID 校验通过后经 bindAsyncID 恰一次
// 绑定。绑定前记录不进入 pending 表、不经任何结算路径、公开面不可
// 达,0 占位不可观察。
func newPendingOperation() *operation {
	return &operation{
		done: make(chan struct{}),
	}
}

// bindAsyncID 恰一次绑定关联提交的异步 ID:绑定后不可变,id 返回该
// 值。id 为 0 或重复绑定即内部不变量破坏,panic(ids.go:0 是异步提交
// 的失败值,不是合法的已接受 ID;操作数通道与终态存储在构造时已建
// 好,绑定只补全关联键)。绑定发生在状态锁段内(登记与接合同锁),
// 先于任何结算与公开面发布。
func (op *operation) bindAsyncID(id AsyncID) {
	if id == 0 {
		panic("maa: operation bound with the zero async id")
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.asyncID != 0 {
		panic(fmt.Sprintf("maa: operation bound twice (%d over %d)", id, op.asyncID))
	}
	op.asyncID = id
}

// id 返回关联提交的异步 ID。
func (op *operation) id() AsyncID {
	return op.asyncID
}

// wait 等待唯一一次结算并取得终态副本;可重复、可并发调用:
//   - ctx(超时/取消,即 context.Canceled/DeadlineExceeded)先于结
//     算就绪时返回 (AsyncResult{}, ctx.Err())——只结束本次等待,不
//     改写操作终态、不撤销已提交的原生动作,之后仍可再次等待(
//     Wait 取消不迁移终态;:取消不使 pending 提前回收);
//   - 已结算则按原因返回:完成成功 (快照, nil);完成失败 (快照,
//     包装 native.ErrNativeRejected 的错误);关闭 (零值,包装
//     native.ErrClosed 的错误);故障 (零值,首因原样)。
//
// ctx 不得为 nil(公开 Operation.Wait 把 Wait(nil) 转为
// ErrInvalidInput)。每次调用(含并发重复等待)都经 resultRecord.snapshot
// 取得独立深拷贝,修改先前副本不影响缓存或其他等待者。
func (op *operation) wait(ctx context.Context) (AsyncResult, error) {
	select {
	case <-op.done:
	case <-ctx.Done():
		// 完成与取消竞争时已发布终态优先:done 可能与本分支
		// 同时就绪而 select 随机选择,在此复核一次,不把稳定终态误
		// 报为本次取消。
		select {
		case <-op.done:
		default:
			return AsyncResult{}, ctx.Err()
		}
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if !op.settled {
		// done 只由 settle 关闭,到达此处即内部不变量破坏。
		panic("maa: operation done closed without a settlement")
	}
	if op.hasResult {
		return op.result.snapshot(), op.cause
	}
	return AsyncResult{}, op.cause
}

// complete 以完成消息结算(What/OK/Details 来自 AsyncCallInfo 的
// 确认字段):成功写 (结果, nil);ret=false 同时写结果与包装
// native.ErrNativeRejected 的错误(结果与可辨识失败错误同时给
// 出)。由 instanceState.settleCompletion 在状态锁内调用。
func (op *operation) complete(rec resultRecord) {
	if rec.ok {
		op.settle(rec, true, nil)
		return
	}
	op.settle(rec, true, fmt.Errorf("maa: async call %d (%s) failed on the native side: %w",
		rec.id, rec.what, native.ErrNativeRejected))
}

// settleClosed 以实例关闭结算:无结果,错误包装 native.ErrClosed,
// 与事件丢失(ErrEventOverflow 链)语义可辨(第 5 步)。由
// instanceState.settleRemainingPending 在状态锁内调用。
func (op *operation) settleClosed() {
	op.settle(resultRecord{}, false, fmt.Errorf("maa: operation %d is settled by the instance close: %w",
		op.asyncID, native.ErrClosed))
}

// settleFault 以 sticky 首因结算:无结果,错误为首因原样——调用方
// 传入的 cause 已包装 native 哨兵(溢出包装 ErrEventOverflow
// 等),沿错误链可达。cause 不得为 nil:无因故障是内部不变量破坏,
// panic。由 instanceState 首因线性化扫描在状态锁内调用。
func (op *operation) settleFault(cause error) {
	if cause == nil {
		panic("maa: operation settled with a fault without a cause")
	}
	op.settle(resultRecord{}, false, cause)
}

// settle 是唯一单次结算路径:在 operation 自身锁内写终态后关闭 done
// 恰一次。重复结算即内部不变量破坏,panic(对照 state.go/dispatch.go
// 先例),不允许静默改写已发布终态(此前已结算的成功/原生失
// 败不改写)。
func (op *operation) settle(rec resultRecord, hasResult bool, cause error) {
	op.mu.Lock()
	defer op.mu.Unlock()
	if op.settled {
		panic(fmt.Sprintf("maa: operation %d settled more than once", op.asyncID))
	}
	op.settled = true
	op.result = rec
	op.hasResult = hasResult
	op.cause = cause
	close(op.done)
}

// ---- 公开 Operation ----

// Operation 是一次已接受异步提交(Connect/Click/Screencap)的公开句
// 柄:非零 ID 只证明提交被 Core 接受,最终结果以 AsyncCallInfo
// 的 async_call_id 关联确认后保存在本对象内,允许任意次(含并发)
// 等待。全部字段未导出,只能经提交方法的返回值使用,以指针持有,不
// 按值复制;实例关闭后仍可等待并取得已保存终态的独立副本。
//
// 实例关闭或故障时尚未完成的提交由统一结算路径结为 ErrClosed 或首
// 因(第 5 步、),等待取得对应可辨识错误;等待取消只结束本
// 次等待,不撤销已提交的原生动作,也不提前释放实例侧关联容量。
type Operation struct {
	// op 是私有关联记录:终态存储与通知通道的持有者。非 nil 由提交
	// 路径保证(提交失败返回错误而不返回 Operation)。
	op *operation
}

// ID 返回关联提交的异步 ID(非零):只证明提交被 Core 接受,不是任
// 何执行成功的证明(非零 ID 只表示提交被接受)。提交成功后 ID
// 即确定且不可变,已结算后保持可读。
func (o *Operation) ID() AsyncID {
	return o.op.id()
}

// Wait 等待本次提交的最终结果并取得独立副本;可重复、可并发调用:
//   - ctx 为 nil:返回包装 ErrInvalidInput 的错误(Wait(nil)
//     返回 ErrInvalidInput);
//   - ctx 的取消/超时先于结算就绪:返回 (AsyncResult{}, ctx.Err())
//     ——只结束本次等待,不改写操作终态、不撤销已提交的原生动作,
//     之后仍可再次等待;
//   - 已结算则按原因返回:完成成功 (结果副本, nil);完成失败
//     (结果副本, 包装 ErrNativeRejected 的错误,OK=false 保留原生
//     ret 语义);关闭 (零值,包装 ErrClosed 的错误);故障 (零值,
//     实例首因)。
//
// 完成与取消竞争时已发布终态优先。每次调用都取得独立深拷
// 贝:修改返回副本不影响缓存、其他等待者或之后的等待。
func (o *Operation) Wait(ctx context.Context) (AsyncResult, error) {
	if ctx == nil {
		return AsyncResult{}, fmt.Errorf("maa: Operation.Wait requires a context, got nil: %w", native.ErrInvalidInput)
	}
	return o.op.wait(ctx)
}
