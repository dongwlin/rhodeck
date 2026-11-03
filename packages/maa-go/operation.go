package maa

import (
	"context"
	"fmt"
	"sync"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

type operation struct {
	asyncID AsyncID

	mu        sync.Mutex
	settled   bool
	done      chan struct{}
	result    resultRecord
	hasResult bool
	cause     error
}

func newOperation(id AsyncID) *operation {
	op := newPendingOperation()
	op.bindAsyncID(id)
	return op
}

func newPendingOperation() *operation {
	return &operation{
		done: make(chan struct{}),
	}
}

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

func (op *operation) id() AsyncID {
	return op.asyncID
}

func (op *operation) wait(ctx context.Context) (AsyncResult, error) {
	select {
	case <-op.done:
	case <-ctx.Done():
		select {
		case <-op.done:
		default:
			return AsyncResult{}, ctx.Err()
		}
	}
	op.mu.Lock()
	defer op.mu.Unlock()
	if !op.settled {
		panic("maa: operation done closed without a settlement")
	}
	if op.hasResult {
		return op.result.snapshot(), op.cause
	}
	return AsyncResult{}, op.cause
}

func (op *operation) complete(rec resultRecord) {
	if rec.ok {
		op.settle(rec, true, nil)
		return
	}
	op.settle(rec, true, fmt.Errorf("maa: async call %d (%s) failed on the native side: %w",
		rec.id, rec.what, native.ErrNativeRejected))
}

func (op *operation) settleClosed() {
	op.settle(resultRecord{}, false, fmt.Errorf("maa: operation %d is settled by the instance close: %w",
		op.asyncID, native.ErrClosed))
}

func (op *operation) settleFault(cause error) {
	if cause == nil {
		panic("maa: operation settled with a fault without a cause")
	}
	op.settle(resultRecord{}, false, cause)
}

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

// Operation 是一次已接受异步提交(Connect/Click/Screencap)的公开句柄:
// 非零 ID 只证明提交被 Core 接受,最终结果以 AsyncCallInfo 的
// async_call_id 关联确认后保存在本对象内,允许任意次(含并发)等待。
// 全部字段未导出,只能经提交方法的返回值使用,以指针持有,不按值复制;
// 实例关闭后仍可等待并取得已保存终态的独立副本。
//
// 实例关闭或故障时尚未完成的提交结为 ErrClosed 或首因,等待取得对应可
// 辨识错误;等待取消只结束本次等待,不撤销已提交的原生动作,也不提前释放
// 实例侧关联容量。
type Operation struct {
	op *operation
}

// ID 返回关联提交的异步 ID(非零):只证明提交被 Core 接受,不是任何执行
// 成功的证明。提交成功后 ID 即确定且不可变,已结算后保持可读。
func (o *Operation) ID() AsyncID {
	return o.op.id()
}

// Wait 等待本次提交的最终结果并取得独立副本;可重复、可并发调用:
//   - ctx 为 nil:返回包装 ErrInvalidInput 的错误;
//   - ctx 的取消/超时先于结算就绪:返回 (AsyncResult{}, ctx.Err())——只
//     结束本次等待,不改写操作终态,不撤销已提交的原生动作,之后仍可再次
//     等待;
//   - 已结算则按原因返回:完成成功 (结果副本, nil);完成失败 (结果副本,
//     包装 ErrNativeRejected 的错误,OK=false 保留原生 ret 语义);关闭
//     (零值,包装 ErrClosed 的错误);故障 (零值,实例首因)。
//
// 完成与取消竞争时已发布终态优先。每次调用都取得独立深拷贝:修改返回
// 副本不影响缓存、其他等待者或之后的等待。
func (o *Operation) Wait(ctx context.Context) (AsyncResult, error) {
	if ctx == nil {
		return AsyncResult{}, fmt.Errorf("maa: Operation.Wait requires a context, got nil: %w", native.ErrInvalidInput)
	}
	return o.op.wait(ctx)
}
