// 本文件实现可重复等待且只能结算一次的私有 Operation 组件(MAA-19;
// 绑定设计 §5、§6.1、§7、§7.1):每条已接受的异步提交对应一个
// operation,持有一次性终态存储与完成通知,允许任意次(含并发)等
// 待;完成消息、实例关闭与 sticky 首因三类原因经同一单次结算路径写
// 入终态。公开面(导出符号)零变化:公开 Operation 类型待 MAA-23 接
// 提交路径时再引入,本单元只交付私有组件与 state.go 的 pending 表
// 结算入口。
//
// 所有权与容量契约(§7):operation 由实例 pending 表与调用者双方
// 持有,调用者丢弃引用不影响实例关联;只有最终结算(完成结算、首
// 因线性化扫描、关闭清空)把记录移出 pending 表并回收
// PendingOperations 容量,不启动逐操作轮询或 reaper;取消一次等待、
// 丢弃引用都不提前释放槽位。已结算的 operation 自身继续保存结果,
// 供此后任意次等待取得独立深拷贝(§6.1),实例关闭后副本仍有效。
//
// 等待语义(§7、§8.1):等待的超时/取消只结束本次等待,返回
// ctx.Err() 与零值结果,不改写终态、不撤销已提交的原生动作,之后
// 仍可再次等待取得真实结果;等待与结算竞争时已发布终态优先。终态
// 判别(§5):完成成功返回 (结果副本, nil);ret=false 同时返回结
// 果副本与包装 native.ErrNativeRejected 的错误(不在 OK 字段编造结
// 构化原因);以关闭结算返回包装 native.ErrClosed 的错误,与事件丢
// 失(ErrEventOverflow 链)语义可辨;以故障结算原样返回首因(调用
// 方传入的 cause 已包装 native 哨兵)。
//
// 结算纪律:恰一次语义,重复结算即内部不变量破坏,panic(对照
// state.go/dispatch.go 先例);结算在 operation 自身锁内先写终态再
// 关闭 done,恰一次;等待者要么在 done 上看到完整终态,要么继续等
// 待,不存在中间视图。并发契约:operation 自持互斥锁,不经
// instanceState.mu 保护;state.go 的结算入口在状态锁内调用本文件的
// 结算方法,锁序固定为状态锁 → operation 锁,operation 自身绝不回
// 调状态,无反序获取。

package maa

import (
	"context"
	"fmt"
	"sync"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// operation 是一次已接受异步提交的私有关联记录(§7:非零 async ID
// 只证明提交成功,最终结果以 AsyncCallInfo 的 async_call_id 关联确
// 认):done 从构造起即存在,只由唯一一次结算关闭;settled 防重复结
// 算,只在持有 mu 时读写。零值不可用,只经 newOperation 构造,以指
// 针使用,不按值复制。
type operation struct {
	// asyncID 是关联提交时的异步 ID,准入后不可变,已结算后保持可
	// 读(§8.1:已接受 Operation 的 ID 保持可读)。准入契约保证非零
	// (ids.go:0 是异步提交的失败值),它是公开 Operation.ID 的语义
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

// newOperation 构造一条 pending 记录:只建立终态存储与通知通道,不
// 启动 goroutine;结算由 instanceState 的 pending 表入口驱动(MAA-23
// 接公开提交路径)。
func newOperation(id AsyncID) *operation {
	return &operation{
		asyncID: id,
		done:    make(chan struct{}),
	}
}

// id 返回关联提交的异步 ID。
func (op *operation) id() AsyncID {
	return op.asyncID
}

// wait 等待唯一一次结算并取得终态副本;可重复、可并发调用:
//   - ctx(超时/取消,即 context.Canceled/DeadlineExceeded)先于结
//     算就绪时返回 (AsyncResult{}, ctx.Err())——只结束本次等待,不
//     改写操作终态、不撤销已提交的原生动作,之后仍可再次等待(§7:
//     Wait 取消不迁移终态;§8.1:取消不使 pending 提前回收);
//   - 已结算则按原因返回:完成成功 (快照, nil);完成失败 (快照,
//     包装 native.ErrNativeRejected 的错误);关闭 (零值,包装
//     native.ErrClosed 的错误);故障 (零值,首因原样)。
//
// ctx 不得为 nil(公开 Wait 负责把 Wait(nil) 转为 ErrInvalidInput,
// 归 MAA-23)。每次调用(含并发重复等待)都经 resultRecord.snapshot
// 取得独立深拷贝,修改先前副本不影响缓存或其他等待者(§6.1)。
func (op *operation) wait(ctx context.Context) (AsyncResult, error) {
	select {
	case <-op.done:
	case <-ctx.Done():
		// 完成与取消竞争时已发布终态优先(§8.1):done 可能与本分支
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

// complete 以完成消息结算(§7:What/OK/Details 来自 AsyncCallInfo 的
// 确认字段):成功写 (结果, nil);ret=false 同时写结果与包装
// native.ErrNativeRejected 的错误(§5:结果与可辨识失败错误同时给
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
// 与事件丢失(ErrEventOverflow 链)语义可辨(§5、§8 第 5 步)。由
// instanceState.settleRemainingPending 在状态锁内调用。
func (op *operation) settleClosed() {
	op.settle(resultRecord{}, false, fmt.Errorf("maa: operation %d is settled by the instance close: %w",
		op.asyncID, native.ErrClosed))
}

// settleFault 以 sticky 首因结算:无结果,错误为首因原样——调用方
// 传入的 cause 已包装 native 哨兵(§7.1:溢出包装 ErrEventOverflow
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
// 先例),不允许静默改写已发布终态(§7.1:此前已结算的成功/原生失
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
