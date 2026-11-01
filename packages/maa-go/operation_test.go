// 本文件是 Operation 结算与等待的受控用例:验证三类终态的单次结算、
// 错误语义、Wait 取消、快照独立性、pending 容量回收与首因线性化。

package maa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newReadyState 构造已发布 ready 的实例状态,pendingOperations 为
// pending 表容量快照(受控用例自行给定,信任不重验)。
func newReadyState(t *testing.T, handle uintptr, pendingOperations uint64) *instanceState {
	t.Helper()
	s := newInstanceState(handle, pendingOperations, 1_048_576)
	s.publishReady()
	return s
}

// mustAdmitPending 准入一条 pending Operation 并要求成功。
func mustAdmitPending(t *testing.T, s *instanceState, id AsyncID, what string) *operation {
	t.Helper()
	op, err := s.admitPendingOperation(id, what)
	require.NoError(t, err, "admitting pending operation %d (%s) must not fail: %v", id, what, err)
	require.NotNil(t, op, "admitting pending operation %d returned a nil record", id)
	return op
}

// pendingOccupancy 在状态锁内读取 pending 表当前占用(白盒诊断读取,
// 不绕过锁)。
func pendingOccupancy(s *instanceState) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending)
}

// waitGuarded 在独立 goroutine 中执行一次等待并以防挂死上限兜底:
// 等待若因实现缺陷永不返回,本辅助超时失败而不是用例挂死。
func waitGuarded(t *testing.T, op *operation, ctx context.Context) (AsyncResult, error) {
	t.Helper()
	type outcome struct {
		res AsyncResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := op.wait(ctx)
		ch <- outcome{res, err}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-time.After(hangGuard):
		require.Fail(t, "operation.wait did not return within the hang-guard limit: the done notification appears not closed")
		return AsyncResult{}, nil
	}
}

// asyncCallInfoPayload 构造一条 AsyncCallInfo 回调消息原文:details
// 是消息中 details 子对象的原始文本,逐字节嵌入(含内部空白)。
func asyncCallInfoPayload(id AsyncID, what, details string) []byte {
	return fmt.Appendf(nil, `{"what":%q,"async_call_id":%d,"details":%s}`, what, int32(id), details)
}

// TestOperationSettlesExactlyOnceUnderRaces 验证 1「多个完
// 成或关闭/故障竞争只能结算一次,所有等待者观察同一终态」:并发等
// 待者全部取得同一份终态;完成结算、关闭清空与首因线性化扫描三方
// 竞争时每个 operation 恰好取得一种终态,先结算者胜,后到的结算入
// 口不得覆盖已发布终态、不得 panic。
func TestOperationSettlesExactlyOnceUnderRaces(t *testing.T) {
	t.Run("8 concurrent waiters observe the same completion terminal state", func(t *testing.T) {
		const (
			waiters = 8
			rounds  = 50
		)
		const (
			id      = AsyncID(501)
			what    = "ConnectReady"
			details = `{"ret":true,"uuid":"x"}`
		)
		op := newOperation(id)

		var (
			wg       sync.WaitGroup
			start    = make(chan struct{})
			mismatch atomic.Int32
		)
		for w := 0; w < waiters; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for i := 0; i < rounds; i++ {
					res, err := op.wait(context.Background())
					if err != nil || res.ID != id || res.What != what || !res.OK || string(res.Details) != details {
						mismatch.Add(1)
						return
					}
				}
			}()
		}
		// 恰一次结算:全部等待者只消费这一份终态。
		close(start)
		op.complete(newResultRecord(id, what, true, json.RawMessage(details)))

		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(hangGuard):
			require.Fail(t, "concurrent waiters did not converge within the hang-guard limit: the done notification appears not closed")
		}
		require.Equal(t, int32(0), mismatch.Load(), "%d waiters obtained a terminal state inconsistent with the single settlement result", mismatch.Load())
		// 全部并发等待收敛后,再次等待仍取得同一终态。
		res, err := waitGuarded(t, op, context.Background())
		require.NoError(t, err, "standalone wait after settlement = (%+v, %v), want the same completion terminal state", res, err)
		require.Equal(t, id, res.ID, "standalone wait after settlement = (%+v, %v), want the same completion terminal state", res, err)
		require.Equal(t, what, res.What, "standalone wait after settlement = (%+v, %v), want the same completion terminal state", res, err)
		require.True(t, res.OK, "standalone wait after settlement = (%+v, %v), want the same completion terminal state", res, err)
		require.Equal(t, details, string(res.Details), "standalone wait after settlement = (%+v, %v), want the same completion terminal state", res, err)
	})

	t.Run("completion, close scan and fault scan race for exactly one terminal state", func(t *testing.T) {
		const (
			rounds       = 30
			roundWaiters = 4
		)
		const (
			id      = AsyncID(77)
			what    = "RaceCall"
			details = `{"ret":true}`
		)
		successInfo := completionInfo{ID: id, What: what, OK: true, Details: json.RawMessage(details)}
		faultCause := fmt.Errorf("race fault: %w", native.ErrProtocol)
		lateCause := fmt.Errorf("late-arriving fault: %w", native.ErrEventOverflow)

		// classify 把一次等待结果归入三类终态之一;字段越界或类别串
		// 染归入失败类别,由调用方断言。
		classify := func(res AsyncResult, err error) string {
			zero := res.ID == 0 && res.What == "" && !res.OK && res.Details == nil
			switch {
			case err == nil:
				if res.ID != id || res.What != what || !res.OK || string(res.Details) != details {
					return "success content corrupted"
				}
				return "completed"
			case errors.Is(err, faultCause):
				if !zero || errors.Is(err, native.ErrClosed) || errors.Is(err, native.ErrNativeRejected) {
					return "fault class corrupted"
				}
				return "fault"
			case errors.Is(err, native.ErrClosed):
				if !zero || errors.Is(err, faultCause) {
					return "closed class corrupted"
				}
				return "closed"
			default:
				return "unknown terminal state"
			}
		}

		for round := 0; round < rounds; round++ {
			s := newReadyState(t, uintptr(0x0A70+round), 8)
			op := mustAdmitPending(t, s, id, what)

			var (
				wg   sync.WaitGroup
				mu   sync.Mutex
				seen []string
			)
			wg.Add(roundWaiters)
			for i := 0; i < roundWaiters; i++ {
				go func() {
					defer wg.Done()
					res, err := op.wait(context.Background())
					mu.Lock()
					seen = append(seen, classify(res, err))
					mu.Unlock()
				}()
			}
			// 三条结算路径并发竞争同一 operation:完成结算、首因线性
			// 化扫描、关闭选主后清空。全部在状态锁内线性化,恰一胜出。
			wg.Add(3)
			go func() {
				defer wg.Done()
				s.settleCompletion(successInfo)
			}()
			go func() {
				defer wg.Done()
				s.recordFault(faultCause)
			}()
			go func() {
				defer wg.Done()
				s.electClose()
				s.settleRemainingPending()
			}()

			done := make(chan struct{})
			go func() { wg.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(hangGuard):
				require.Fail(t, fmt.Sprintf("round %d race did not converge within the hang-guard limit", round))
			}

			require.Len(t, seen, roundWaiters, "round %d waiter result count = %d, want %d", round, len(seen), roundWaiters)
			for i, class := range seen {
				require.Equal(t, seen[0], class, "round %d waiter %d saw terminal state %q, inconsistent with waiter 0's %q", round, i, class, seen[0])
			}
			require.Contains(t, []string{"completed", "closed", "fault"}, seen[0], "round %d terminal state class %q out of range", round, seen[0])

			// 先结算者胜:后到的其余结算入口不得覆盖、不得 panic。
			// electClose 把阶段统一带入 closing(ready/faulted 合法迁
			// 移,已 closing 幂等失败),settleRemainingPending 才可安
			// 全调用;完成未命中、故障 sticky 后空表扫描均为空操作。
			s.electClose()
			s.settleCompletion(successInfo)
			s.recordFault(lateCause)
			s.settleRemainingPending()
			again, err := op.wait(context.Background())
			lateClass := classify(again, err)
			require.Equal(t, seen[0], lateClass, "round %d late settlement rewrote the terminal state: %q → %q (%+v, %v)", round, seen[0], lateClass, again, err)
		}
	})

	t.Run("with completion first, late close and fault settlements must not overwrite", func(t *testing.T) {
		s := newReadyState(t, 0x0A78, 8)
		id := AsyncID(78)
		op := mustAdmitPending(t, s, id, "submission completed first")
		successInfo := completionInfo{ID: id, What: "FirstWin", OK: true, Details: json.RawMessage(`{"ret":true}`)}
		s.settleCompletion(successInfo)

		// 后到的完成未命中;后到的故障记录首因但扫不到记录;closing
		// 清空为空操作。任何一路都不得改写已发布终态,也不得 panic。
		mustElectClose(t, s)
		s.settleCompletion(successInfo)
		s.recordFault(fmt.Errorf("late-arriving fault: %w", native.ErrEventOverflow))
		s.settleRemainingPending()

		res, err := waitGuarded(t, op, context.Background())
		require.NoError(t, err, "the late settlement rewrote the terminal state: (%+v, %v), want the completion terminal state unchanged", res, err)
		require.True(t, res.OK, "the late settlement rewrote the terminal state: (%+v, %v), want the completion terminal state unchanged", res, err)
		require.Equal(t, id, res.ID, "the late settlement rewrote the terminal state: (%+v, %v), want the completion terminal state unchanged", res, err)
		require.Equal(t, `{"ret":true}`, string(res.Details), "the late settlement rewrote the terminal state: (%+v, %v), want the completion terminal state unchanged", res, err)
		require.Equal(t, 0, pendingOccupancy(s), "the late settlement must not revive the pending record: occupancy = %d", pendingOccupancy(s))
	})
}

// TestOperationTerminalErrorSemantics 验证 2「ret=false 返
// 回原生结果与可辨识失败错误,关闭和事件丢失保持不同错误语义」:
// 完成失败同时给出完好快照与包装 ErrNativeRejected 的错误;关闭结
// 算包装 ErrClosed 且无结果;故障结算原样返回首因;三类错误交叉
// errors.Is 互不可混淆。已结算后 ID 保持可读。
func TestOperationTerminalErrorSemantics(t *testing.T) {
	const (
		id      = AsyncID(601)
		what    = "Screencap"
		details = `{"ret":false,"cost":7}`
	)

	t.Run("ret=false returns an intact result together with ErrNativeRejected", func(t *testing.T) {
		op := newOperation(id)
		op.complete(newResultRecord(id, what, false, json.RawMessage(details)))

		res, err := waitGuarded(t, op, context.Background())
		require.Error(t, err, "ret=false must return a non-nil error")
		assert.ErrorIs(t, err, native.ErrNativeRejected, "errors.Is(err, native.ErrNativeRejected) = false; err = %v", err)
		// 结果与可辨识失败错误同时给出,快照字段完好(不在 OK
		// 字段编造结构化原因)。
		require.Equal(t, id, res.ID, "ret=false result = %+v, want a failure result with intact fields", res)
		require.Equal(t, what, res.What, "ret=false result = %+v, want a failure result with intact fields", res)
		require.False(t, res.OK, "ret=false result = %+v, want a failure result with intact fields", res)
		require.Equal(t, details, string(res.Details), "ret=false result = %+v, want a failure result with intact fields", res)
		assert.Equal(t, id, op.id(), "ID must stay readable after settlement: got %d, want %d", op.id(), id)
	})

	t.Run("close settlement returns ErrClosed with no result", func(t *testing.T) {
		op := newOperation(id)
		op.settleClosed()

		res, err := waitGuarded(t, op, context.Background())
		require.ErrorIs(t, err, native.ErrClosed, "errors.Is(err, native.ErrClosed) = false; err = %v", err)
		require.Equal(t, AsyncID(0), res.ID, "the close settlement must carry no result: %+v", res)
		require.Empty(t, res.What, "the close settlement must carry no result: %+v", res)
		require.False(t, res.OK, "the close settlement must carry no result: %+v", res)
		require.Nil(t, res.Details, "the close settlement must carry no result: %+v", res)
		assert.Equal(t, id, op.id(), "ID must stay readable after settlement: got %d, want %d", op.id(), id)
	})

	t.Run("fault settlement returns the first cause as is", func(t *testing.T) {
		op := newOperation(id)
		cause := fmt.Errorf("event queue overflow: %w", native.ErrEventOverflow)
		op.settleFault(cause)

		res, err := waitGuarded(t, op, context.Background())
		require.ErrorIs(t, err, cause, "errors.Is(err, cause) = false; err = %v, want first cause %v", err, cause)
		assert.ErrorIs(t, err, native.ErrEventOverflow, "errors.Is(err, native.ErrEventOverflow) = false; err = %v", err)
		require.Equal(t, AsyncID(0), res.ID, "the fault settlement must carry no result: %+v", res)
		require.Empty(t, res.What, "the fault settlement must carry no result: %+v", res)
		require.False(t, res.OK, "the fault settlement must carry no result: %+v", res)
		require.Nil(t, res.Details, "the fault settlement must carry no result: %+v", res)
		assert.Equal(t, id, op.id(), "ID must stay readable after settlement: got %d, want %d", op.id(), id)
	})

	t.Run("the three terminal error classes must not be conflated with each other", func(t *testing.T) {
		nativeOp := newOperation(611)
		nativeOp.complete(newResultRecord(611, what, false, json.RawMessage(details)))
		closedOp := newOperation(612)
		closedOp.settleClosed()
		faultOp := newOperation(613)
		cause := fmt.Errorf("first cause: %w", native.ErrEventOverflow)
		faultOp.settleFault(cause)

		_, nativeErr := waitGuarded(t, nativeOp, context.Background())
		_, closedErr := waitGuarded(t, closedOp, context.Background())
		_, faultErr := waitGuarded(t, faultOp, context.Background())

		// 原生失败只含 ErrNativeRejected,不得混入关闭/溢出。
		assert.ErrorIs(t, nativeErr, native.ErrNativeRejected, "native failure error = %v, want it to contain ErrNativeRejected", nativeErr)
		assert.NotErrorIs(t, nativeErr, native.ErrClosed, "the native failure error mixed in an adjacent class: %v", nativeErr)
		assert.NotErrorIs(t, nativeErr, native.ErrEventOverflow, "the native failure error mixed in an adjacent class: %v", nativeErr)
		assert.NotErrorIs(t, nativeErr, cause, "the native failure error mixed in an adjacent class: %v", nativeErr)
		// 关闭只含 ErrClosed。
		assert.ErrorIs(t, closedErr, native.ErrClosed, "close error = %v, want it to contain ErrClosed", closedErr)
		assert.NotErrorIs(t, closedErr, native.ErrNativeRejected, "the close error mixed in an adjacent class: %v", closedErr)
		assert.NotErrorIs(t, closedErr, native.ErrEventOverflow, "the close error mixed in an adjacent class: %v", closedErr)
		assert.NotErrorIs(t, closedErr, cause, "the close error mixed in an adjacent class: %v", closedErr)
		// 故障只沿首因链可达,不得混入原生失败/关闭。
		assert.ErrorIs(t, faultErr, cause, "fault error = %v, want ErrEventOverflow reachable along the first-cause chain", faultErr)
		assert.ErrorIs(t, faultErr, native.ErrEventOverflow, "fault error = %v, want ErrEventOverflow reachable along the first-cause chain", faultErr)
		assert.NotErrorIs(t, faultErr, native.ErrNativeRejected, "the fault error mixed in an adjacent class: %v", faultErr)
		assert.NotErrorIs(t, faultErr, native.ErrClosed, "the fault error mixed in an adjacent class: %v", faultErr)
	})
}

// TestOperationWaitCancellation 验证 3「一次 Wait 取消后仍
// 可再次等待并取得后续真实结果」:取消/超时只结束本次等待,返回
// ctx.Err() 与零值结果,不改写终态、不提前回收 pending 记录;终态
// 已发布时与取消竞争,已发布终态优先。
func TestOperationWaitCancellation(t *testing.T) {
	t.Run("a cancelled context returns Canceled without rewriting the terminal state or reclaiming the slot", func(t *testing.T) {
		s := newReadyState(t, 0x0A31, 8)
		id := AsyncID(21)
		op := mustAdmitPending(t, s, id, "submission whose wait will be cancelled")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		res, err := waitGuarded(t, op, ctx)
		require.ErrorIs(t, err, context.Canceled, "wait error with a cancelled ctx = %v, want context.Canceled", err)
		require.Equal(t, AsyncID(0), res.ID, "the cancelled wait must return a zero-value result: %+v", res)
		require.Empty(t, res.What, "the cancelled wait must return a zero-value result: %+v", res)
		require.False(t, res.OK, "the cancelled wait must return a zero-value result: %+v", res)
		require.Nil(t, res.Details, "the cancelled wait must return a zero-value result: %+v", res)
		// 取消不迁移终态、不提前回收:记录仍在实例 pending 表中。
		require.Equal(t, 1, pendingOccupancy(s), "pending occupancy after cancelling one wait = %d, want 1 (cancel does not reclaim early)", pendingOccupancy(s))
		// 之后仍可再次等待取得真实结果。
		s.settleCompletion(completionInfo{ID: id, What: "Click", OK: true, Details: json.RawMessage(`{"ret":true}`)})
		res2, err2 := waitGuarded(t, op, context.Background())
		require.NoError(t, err2, "re-wait after cancellation = (%+v, %v), want the real completion result", res2, err2)
		require.Equal(t, id, res2.ID, "re-wait after cancellation = (%+v, %v), want the real completion result", res2, err2)
		require.True(t, res2.OK, "re-wait after cancellation = (%+v, %v), want the real completion result", res2, err2)
	})

	t.Run("an expired deadline returns DeadlineExceeded and can wait again", func(t *testing.T) {
		s := newReadyState(t, 0x0A32, 8)
		id := AsyncID(22)
		op := mustAdmitPending(t, s, id, "submission whose wait times out")
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
		defer cancel()

		res, err := waitGuarded(t, op, ctx)
		require.ErrorIs(t, err, context.DeadlineExceeded, "wait error with an expired deadline = %v, want context.DeadlineExceeded", err)
		require.Equal(t, AsyncID(0), res.ID, "the timed-out wait must return a zero-value result: %+v", res)
		require.Empty(t, res.What, "the timed-out wait must return a zero-value result: %+v", res)
		require.False(t, res.OK, "the timed-out wait must return a zero-value result: %+v", res)
		require.Nil(t, res.Details, "the timed-out wait must return a zero-value result: %+v", res)
		require.Equal(t, 1, pendingOccupancy(s), "pending occupancy after one timed-out wait = %d, want 1", pendingOccupancy(s))
		op.complete(newResultRecord(id, "Click", true, json.RawMessage(`{"ret":true}`)))
		res2, err2 := waitGuarded(t, op, context.Background())
		require.NoError(t, err2, "re-wait after timeout = (%+v, %v), want the real completion result", res2, err2)
		require.Equal(t, id, res2.ID, "re-wait after timeout = (%+v, %v), want the real completion result", res2, err2)
		require.True(t, res2.OK, "re-wait after timeout = (%+v, %v), want the real completion result", res2, err2)
		require.Equal(t, `{"ret":true}`, string(res2.Details), "re-wait after timeout = (%+v, %v), want the real completion result", res2, err2)
	})

	t.Run("a cancelled ctx still obtains the terminal state once it is published", func(t *testing.T) {
		const (
			rounds = 30
			id     = AsyncID(23)
		)
		// wait 的 select 在 done 与 ctx.Done() 同时就绪时随机择一,
		// 终态优先靠实现的复核纪律保证;单次抽样只有约 35% 概率走进
		// 取消分支而命中「复核缺失」缺陷,故循环采样 30 轮(参照同
		// 文件竞争用例的做法),每轮新建 operation 发布终态后配已取
		// 消的 ctx,任一轮观察到取消形态即失败,使检出概率趋近必然。
		for round := 0; round < rounds; round++ {
			op := newOperation(id)
			op.complete(newResultRecord(id, "Click", true, json.RawMessage(`{"ret":true}`)))
			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			// 完成与取消竞争时已发布终态优先:done 与 ctx.Done() 同时
			// 就绪,无论 select 取哪一支,观察到的都必须是稳定终态。
			res, err := waitGuarded(t, op, ctx)
			require.NotErrorIs(t, err, context.Canceled, "round %d: the wait with a cancelled ctx after settlement was misreported as cancelled (err = %v), want the terminal state to win", round, err)
			require.NoError(t, err, "round %d: the wait with a cancelled ctx after settlement = (%+v, %v), want the terminal state to win", round, res, err)
			require.Equal(t, id, res.ID, "round %d: the wait with a cancelled ctx after settlement = (%+v, %v), want the terminal state to win", round, res, err)
			require.True(t, res.OK, "round %d: the wait with a cancelled ctx after settlement = (%+v, %v), want the terminal state to win", round, res, err)
			require.Equal(t, `{"ret":true}`, string(res.Details), "round %d: the wait with a cancelled ctx after settlement = (%+v, %v), want the terminal state to win", round, res, err)
		}
	})
}

// TestOperationWaitSnapshotIndependence 验证 4「修改某次
// Wait 返回的 JSON 不改变缓存或其他等待者结果」:每次等待经快照取
// 得独立深拷贝,原地改写先前副本不影响缓存、其他副本与之后的任何
// 等待;多次等待互不共享底层数组(交叉改写验证)。
func TestOperationWaitSnapshotIndependence(t *testing.T) {
	const (
		id      = AsyncID(31)
		what    = "ConnectReady"
		details = `{"ret":true,"big":9007199254740993}`
	)
	settledOp := func(t *testing.T) *operation {
		t.Helper()
		op := newOperation(id)
		op.complete(newResultRecord(id, what, true, json.RawMessage(details)))
		return op
	}

	t.Run("mutating one wait's copy does not affect the cache or other waiters", func(t *testing.T) {
		op := settledOp(t)
		w1, _ := waitGuarded(t, op, context.Background())
		w2, _ := waitGuarded(t, op, context.Background())

		// 原地改写 w1:w2 与缓存的后续快照均不受影响(互不共享底层数
		// 组的交叉验证)。
		for i := range w1.Details {
			w1.Details[i] = '#'
		}
		w1.What = "tampered"
		w1.OK = false
		require.Equal(t, id, w2.ID, "mutating an earlier copy affected another waiter's result: %+v", w2)
		require.Equal(t, what, w2.What, "mutating an earlier copy affected another waiter's result: %+v", w2)
		require.True(t, w2.OK, "mutating an earlier copy affected another waiter's result: %+v", w2)
		require.Equal(t, details, string(w2.Details), "mutating an earlier copy affected another waiter's result: %+v", w2)
		w3, _ := waitGuarded(t, op, context.Background())
		require.Equal(t, id, w3.ID, "mutating an earlier copy affected the cache: %+v", w3)
		require.Equal(t, what, w3.What, "mutating an earlier copy affected the cache: %+v", w3)
		require.True(t, w3.OK, "mutating an earlier copy affected the cache: %+v", w3)
		require.Equal(t, details, string(w3.Details), "mutating an earlier copy affected the cache: %+v", w3)
		// 改写 w2 同理:w1 保持被改写状态(它是调用者拥有的独立数据),
		// 新快照仍取原始内容。
		for i := range w2.Details {
			w2.Details[i] = '@'
		}
		w4, _ := waitGuarded(t, op, context.Background())
		require.Equal(t, details, string(w4.Details), "mutating the second copy affected the cache: %+v", w4)
		require.Equal(t, what, w4.What, "mutating the second copy affected the cache: %+v", w4)
	})

	t.Run("concurrent waiters mutating their own copies do not crosstalk", func(t *testing.T) {
		op := settledOp(t)
		const n = 8
		var (
			wg       sync.WaitGroup
			polluted atomic.Int32
		)
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				res, err := op.wait(context.Background())
				if err != nil || res.ID != id || string(res.Details) != details {
					polluted.Add(1)
					return
				}
				for j := range res.Details {
					res.Details[j] = byte('a' + i)
				}
			}(i)
		}
		wg.Wait()
		require.Equal(t, int32(0), polluted.Load(), "%d concurrent waiters obtained polluted copies", polluted.Load())
		final, err := waitGuarded(t, op, context.Background())
		require.NoError(t, err, "concurrent mutation affected the cache: (%+v, %v)", final, err)
		require.Equal(t, details, string(final.Details), "concurrent mutation affected the cache: (%+v, %v)", final, err)
		require.Equal(t, what, final.What, "concurrent mutation affected the cache: (%+v, %v)", final, err)
	})
}

// TestPendingOperationOwnershipAndReclaim 验证 5「调用者丢
// 弃 Operation 不影响实例持有的关联;容量由最终结算统一回收,不启
// 动逐操作轮询」:白盒确认记录仍由实例 pending 表持有,经
// settleCompletion 结算后移出表、容量回收可再 admit;取消一次等待
// 不提前释放槽位。无轮询/reaper 为结构性结论,见文件头代码审查说
// 明,不做运行时 goroutine 计数。
func TestPendingOperationOwnershipAndReclaim(t *testing.T) {
	t.Run("dropping the caller reference does not affect the instance association; settlement reclaims uniformly", func(t *testing.T) {
		s := newReadyState(t, 0x0A51, 4)
		id := AsyncID(9)
		mustAdmitPending(t, s, id, "submission to be dropped")
		// 调用者从此不再持有 *operation:实例关联不受影响。
		require.Equal(t, 1, pendingOccupancy(s), "pending occupancy after admit = %d, want 1", pendingOccupancy(s))
		s.mu.Lock()
		_, stillTracked := s.pending[id]
		s.mu.Unlock()
		require.True(t, stillTracked, "the instance pending table must still hold the record after the caller reference is dropped")
		// 只有最终结算移出记录并回收 PendingOperations 容量。
		s.settleCompletion(completionInfo{ID: id, What: "DroppedCall", OK: true, Details: json.RawMessage(`{"ret":true}`)})
		require.Equal(t, 0, pendingOccupancy(s), "pending occupancy after settlement = %d, want 0 (capacity reclaimed)", pendingOccupancy(s))
		_, admitErr := s.admitPendingOperation(id, "new submission after reclamation")
		require.NoError(t, admitErr, "re-admitting the same ID after reclamation must not fail: %v", admitErr)
	})

	t.Run("cancelling one wait does not release the slot early", func(t *testing.T) {
		s := newReadyState(t, 0x0A52, 4)
		id := AsyncID(10)
		op := mustAdmitPending(t, s, id, "submission still holding a slot after cancellation")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		_, cancelErr := waitGuarded(t, op, ctx)
		require.ErrorIs(t, cancelErr, context.Canceled, "cancelled wait error = %v, want context.Canceled", cancelErr)
		// 取消不释放槽位:容量仍被占用,真实完成仍可按 ID 结算。
		require.Equal(t, 1, pendingOccupancy(s), "pending occupancy after cancel = %d, want 1", pendingOccupancy(s))
		s.settleCompletion(completionInfo{ID: id, What: "Click", OK: false, Details: json.RawMessage(`{"ret":false}`)})
		require.Equal(t, 0, pendingOccupancy(s), "pending occupancy after completion settlement = %d, want 0", pendingOccupancy(s))
	})
}

// TestAdmitPendingOperationBoundaries 验证 6「容量边界与准
// 入阶段拒绝」:pending 表填满后 admit 返回包装 ErrOperationLimit
// 的错误且实例不故障(阶段仍 ready、无首因、故障通知不触发),被拒
// 的准入不占容量,结算释放后可再 admit;closing/faulted/closed 阶段
// 分别按各自哨兵拒绝;id==0 与 initializing 阶段准入按编程错误
// panic。
func TestAdmitPendingOperationBoundaries(t *testing.T) {
	t.Run("full-capacity rejection wraps ErrOperationLimit and the instance does not fault", func(t *testing.T) {
		s := newReadyState(t, 0x0A61, 2)
		mustAdmitPending(t, s, AsyncID(1), "submission 1")
		mustAdmitPending(t, s, AsyncID(2), "submission 2")

		_, err := s.admitPendingOperation(AsyncID(3), "submission 3")
		require.Error(t, err, "admit must reject once capacity is full")
		assert.ErrorIs(t, err, native.ErrOperationLimit, "errors.Is(err, native.ErrOperationLimit) = false; err = %v", err)
		assert.NotErrorIs(t, err, native.ErrClosed, "the capacity rejection must not be mislabelled as closed/closing: %v", err)
		assert.NotErrorIs(t, err, native.ErrClosing, "the capacity rejection must not be mislabelled as closed/closing: %v", err)
		assert.Contains(t, err.Error(), "submission 3", "the rejection error should carry the what context text:\n%s", err.Error())
		// 实例不因容量拒绝而故障。
		assert.Equal(t, phaseReady, s.currentPhase(), "phase after the capacity rejection = %d, want still ready", s.currentPhase())
		assert.Nil(t, s.currentFault(), "first cause after the capacity rejection = %v, want nil", s.currentFault())
		select {
		case <-s.faultNotify():
			assert.Fail(t, "the capacity rejection must not trigger the one-shot fault notification")
		default:
		}
		// 被拒的准入不占容量:占用仍为 2。
		require.Equal(t, 2, pendingOccupancy(s), "occupancy after the rejected admit = %d, want 2 (a rejection consumes no capacity)", pendingOccupancy(s))
		// 结算释放后可再 admit;再次填满后再次拒绝。
		s.settleCompletion(completionInfo{ID: 1, What: "submission 1", OK: true, Details: json.RawMessage(`{"ret":true}`)})
		require.Equal(t, 1, pendingOccupancy(s), "occupancy after settlement released it = %d, want 1", pendingOccupancy(s))
		_, admitErr := s.admitPendingOperation(AsyncID(3), "submission 3")
		require.NoError(t, admitErr, "admit must not fail after settlement released capacity: %v", admitErr)
		_, fullErr := s.admitPendingOperation(AsyncID(4), "submission 4")
		require.ErrorIs(t, fullErr, native.ErrOperationLimit, "must reject again once capacity is full again: %v", fullErr)
	})

	t.Run("closing, faulted and closed phases reject admission", func(t *testing.T) {
		closing := newReadyState(t, 0x0A62, 8)
		mustElectClose(t, closing)
		_, err := closing.admitPendingOperation(AsyncID(1), "submission during closing")
		require.ErrorIs(t, err, native.ErrClosing, "errors.Is(err, native.ErrClosing) = false; err = %v", err)
		assert.NotErrorIs(t, err, native.ErrClosed, "the closing rejection must not be mislabelled as an adjacent class: %v", err)
		assert.NotErrorIs(t, err, native.ErrOperationLimit, "the closing rejection must not be mislabelled as an adjacent class: %v", err)

		faulted := newReadyState(t, 0x0A63, 8)
		cause := fmt.Errorf("event queue overflow: %w", native.ErrEventOverflow)
		faulted.recordFault(cause)
		_, err = faulted.admitPendingOperation(AsyncID(2), "submission during faulted")
		require.ErrorIs(t, err, cause, "the faulted rejection should reach the first cause along the chain: err = %v", err)
		require.ErrorIs(t, err, native.ErrEventOverflow, "the faulted rejection should reach the first cause along the chain: err = %v", err)
		assert.NotErrorIs(t, err, native.ErrClosing, "the faulted rejection must not be mislabelled as closing/closed: %v", err)
		assert.NotErrorIs(t, err, native.ErrClosed, "the faulted rejection must not be mislabelled as closing/closed: %v", err)

		closed := newClosedState(t, 0x0A64)
		_, err = closed.admitPendingOperation(AsyncID(3), "submission during closed")
		require.ErrorIs(t, err, native.ErrClosed, "errors.Is(err, native.ErrClosed) = false; err = %v", err)
		assert.NotErrorIs(t, err, native.ErrClosing, "the closed rejection must not be mislabelled as an adjacent class: %v", err)
		assert.NotErrorIs(t, err, native.ErrOperationLimit, "the closed rejection must not be mislabelled as an adjacent class: %v", err)
	})

	t.Run("admission panics for id 0 and in the initializing phase", func(t *testing.T) {
		ready := newReadyState(t, 0x0A65, 8)
		assertPanics(t, "pending admission with id 0", func() { _, _ = ready.admitPendingOperation(0, "zero ID") })
		initializing := newInstanceState(0x0A66, 8, 1_048_576)
		assertPanics(t, "pending admission in the initializing phase", func() { _, _ = initializing.admitPendingOperation(1, "unpublished instance") })
	})
}

// TestFirstFaultLinearizesPendingOperations 钉住首次故障线性
// 化扫描(验收 7):有 pending 时 recordFault 在状态门内立即将全部
// 未决 Operation 统一结为首因;故障前已结算的成功不改写;故障后到
// 达的完成消息查不到记录,不能覆盖故障终态;重复故障不重复扫描、
// 不覆盖首因、不 panic。
func TestFirstFaultLinearizesPendingOperations(t *testing.T) {
	t.Run("the first cause immediately settles all pending operations in one sweep", func(t *testing.T) {
		s := newReadyState(t, 0x0A71, 8)
		op1 := mustAdmitPending(t, s, AsyncID(11), "pending 1")
		op2 := mustAdmitPending(t, s, AsyncID(12), "pending 2")
		op3 := mustAdmitPending(t, s, AsyncID(13), "pending 3")
		// 故障前先结算 op2:已结算的成功不得被故障改写。
		s.settleCompletion(completionInfo{ID: 12, What: "earlier success", OK: true, Details: json.RawMessage(`{"ret":true}`)})

		cause := fmt.Errorf("event queue overflow: %w", native.ErrEventOverflow)
		s.recordFault(cause)

		for i, op := range []*operation{op1, op3} {
			res, err := waitGuarded(t, op, context.Background())
			require.ErrorIs(t, err, cause, "pending item %d error = %v, want first cause %v", i+1, err, cause)
			assert.NotErrorIs(t, err, native.ErrClosed, "pending item %d fault error mixed in an adjacent class: %v", i+1, err)
			assert.NotErrorIs(t, err, native.ErrNativeRejected, "pending item %d fault error mixed in an adjacent class: %v", i+1, err)
			assert.Equal(t, AsyncID(0), res.ID, "pending item %d fault settlement must carry no result: %+v", i+1, res)
			assert.Empty(t, res.What, "pending item %d fault settlement must carry no result: %+v", i+1, res)
			assert.False(t, res.OK, "pending item %d fault settlement must carry no result: %+v", i+1, res)
			assert.Nil(t, res.Details, "pending item %d fault settlement must carry no result: %+v", i+1, res)
		}
		res2, err2 := waitGuarded(t, op2, context.Background())
		require.NoError(t, err2, "the success settled before the fault was rewritten: (%+v, %v)", res2, err2)
		require.True(t, res2.OK, "the success settled before the fault was rewritten: (%+v, %v)", res2, err2)
		require.Equal(t, `{"ret":true}`, string(res2.Details), "the success settled before the fault was rewritten: (%+v, %v)", res2, err2)
		require.Equal(t, 0, pendingOccupancy(s), "pending occupancy after the first-cause scan = %d, want 0", pendingOccupancy(s))
		assert.Equal(t, phaseFaulted, s.currentPhase(), "phase after recording the first cause = %d, want faulted", s.currentPhase())
	})

	t.Run("a completion arriving after the fault must not overwrite the fault terminal state", func(t *testing.T) {
		s := newReadyState(t, 0x0A72, 8)
		id := AsyncID(21)
		op := mustAdmitPending(t, s, id, "pending before the fault")
		cause := fmt.Errorf("first cause: %w", native.ErrProtocol)
		s.recordFault(cause)

		// 故障后完成消息查不到记录,只能按未命中放弃:等待者必须见
		// 故障而非成功。
		s.settleCompletion(completionInfo{ID: id, What: "late completion", OK: true, Details: json.RawMessage(`{"ret":true}`)})
		res, err := waitGuarded(t, op, context.Background())
		require.ErrorIs(t, err, cause, "the completion arriving after the fault rewrote the terminal state: (%+v, %v), want the first-cause fault", res, err)
		require.Equal(t, AsyncID(0), res.ID, "the completion arriving after the fault rewrote the terminal state: (%+v, %v), want the first-cause fault", res, err)
		require.Equal(t, 0, pendingOccupancy(s), "a missed completion must not revive the record: occupancy = %d", pendingOccupancy(s))
	})

	t.Run("a duplicate fault neither rescans nor overwrites the first cause", func(t *testing.T) {
		s := newReadyState(t, 0x0A73, 8)
		op1 := mustAdmitPending(t, s, AsyncID(31), "pending A")
		op2 := mustAdmitPending(t, s, AsyncID(32), "pending B")
		first := fmt.Errorf("first cause: %w", native.ErrProtocol)
		second := fmt.Errorf("late: %w", native.ErrEventOverflow)

		s.recordFault(first)
		// 重复故障不得 panic(重复关闭 faultDone 或重复结算都会在实
		// 现缺陷下以 panic 暴露),也不得让等待者改见后到原因。
		s.recordFault(second)

		for i, op := range []*operation{op1, op2} {
			_, err := waitGuarded(t, op, context.Background())
			require.ErrorIs(t, err, first, "pending item %d error = %v, want first cause %v without the late cause", i+1, err, first)
			require.NotErrorIs(t, err, second, "pending item %d error = %v, want first cause %v without the late cause", i+1, err, first)
		}
		got := s.currentFault()
		require.ErrorIs(t, got, first, "the first cause was overwritten by the later fault: %v", got)
		require.NotErrorIs(t, got, native.ErrEventOverflow, "the first cause was overwritten by the later fault: %v", got)
		select {
		case <-s.faultNotify():
		default:
			require.Fail(t, "the first cause must close the one-shot fault notification")
		}
	})
}

// TestSettleRemainingPendingClosingSemantics 验证 8:closing
// 阶段把剩余 pending 全部以 ErrClosed 结算一次并清空,表已空时为无
// 害空操作;非 closing 阶段调用按编程错误 panic。
func TestSettleRemainingPendingClosingSemantics(t *testing.T) {
	t.Run("closing settles the remaining pending as ErrClosed", func(t *testing.T) {
		s := newReadyState(t, 0x0A81, 8)
		op1 := mustAdmitPending(t, s, AsyncID(1), "remaining 1")
		op2 := mustAdmitPending(t, s, AsyncID(2), "remaining 2")
		mustElectClose(t, s)

		s.settleRemainingPending()
		for i, op := range []*operation{op1, op2} {
			res, err := waitGuarded(t, op, context.Background())
			require.ErrorIs(t, err, native.ErrClosed, "remaining item %d error = %v, want ErrClosed", i+1, err)
			assert.NotErrorIs(t, err, native.ErrNativeRejected, "remaining item %d close error mixed in an adjacent class: %v", i+1, err)
			assert.NotErrorIs(t, err, native.ErrClosing, "remaining item %d close error mixed in an adjacent class: %v", i+1, err)
			assert.Equal(t, AsyncID(0), res.ID, "remaining item %d close settlement must carry no result: %+v", i+1, res)
			assert.Empty(t, res.What, "remaining item %d close settlement must carry no result: %+v", i+1, res)
			assert.False(t, res.OK, "remaining item %d close settlement must carry no result: %+v", i+1, res)
			assert.Nil(t, res.Details, "remaining item %d close settlement must carry no result: %+v", i+1, res)
		}
		require.Equal(t, 0, pendingOccupancy(s), "occupancy after close drained it = %d, want 0", pendingOccupancy(s))
		// 表已空后再次调用为无害空操作,随后可正常走完关闭发布。
		s.settleRemainingPending()
		finishClose(t, s, nil)
		require.Equal(t, phaseClosed, s.currentPhase(), "close completed after draining, phase = %d, want closed", s.currentPhase())
	})

	t.Run("calling in a non-closing phase panics", func(t *testing.T) {
		ready := newReadyState(t, 0x0A82, 8)
		mustAdmitPending(t, ready, AsyncID(3), "pending item")
		assertPanics(t, "settleRemainingPending in the ready phase", func() { ready.settleRemainingPending() })

		faulted := newReadyState(t, 0x0A83, 8)
		faulted.recordFault(fmt.Errorf("fault: %w", native.ErrProtocol))
		assertPanics(t, "settleRemainingPending in the faulted phase", func() { faulted.settleRemainingPending() })

		closed := newClosedState(t, 0x0A84)
		assertPanics(t, "settleRemainingPending in the closed phase", func() { closed.settleRemainingPending() })
	})
}

// newSettlementWiredDispatcher 构造真实接线的事件分发器:
// settle 接 instanceState.settleCompletion,预算取互不设限的基线并
// 已 start。返回的分发器由用例负责 closeAndDrain 收尾。
func newSettlementWiredDispatcher(t *testing.T, pendingOperations uint64) (*eventDispatcher, *instanceState) {
	t.Helper()
	b := generousBudgets()
	s := newInstanceState(0x0D1900, pendingOperations, 1_048_576)
	s.publishReady()
	d := newEventDispatcher(s, s.settleCompletion, b.callbackBytes, b.internalEvents, b.internalBytes, b.publicEvents, b.publicBytes)
	d.start()
	return d, s
}

// TestDispatcherWiringSettlesCompletion 验证 9(dispatcher
// 集成):newEventDispatcher 接 instanceState.settleCompletion 后,
// 合法 AsyncCallInfo 先结算再投递,公开事件可读时等待立即取得终态,
// details 子对象逐字节保留(含内部空白);ret=false 结算为原生失
// 败;未命中 ID 不结算、不故障,事件照常投递(边界:本层
// 无提交高水位核对,未知/重复完成不归因、不猜测、不计丢失)。
func TestDispatcherWiringSettlesCompletion(t *testing.T) {
	t.Run("a valid completion settles before delivery, details raw preserved byte-for-byte", func(t *testing.T) {
		d, s := newSettlementWiredDispatcher(t, 16)
		defer closeAndDrain(t, d)
		id := AsyncID(42)
		details := `{"ret":true, "cost":3}` // 内部空白必须原样保留(非紧凑化)
		payload := asyncCallInfoPayload(id, "Screencap", details)
		op := mustAdmitPending(t, s, id, "wired submission")

		enqueue(t, d, asyncCallInfoMessageID, payload)
		ev := mustReadEvent(t, d.events())
		require.Equal(t, string(payload), string(ev.JSON), "public event raw = %s, want byte-for-byte preserved %s", ev.JSON, payload)
		// 先结算后投递(第 5 步):公开事件可读时,等待必须立即取
		// 得终态,不得再有任何延迟。
		res, err := waitGuarded(t, op, context.Background())
		require.NoError(t, err, "the wait must return the terminal state immediately once the event is readable: %v", err)
		require.Equal(t, id, res.ID, "settled result = %+v, want {42 Screencap true}", res)
		require.Equal(t, "Screencap", res.What, "settled result = %+v, want {42 Screencap true}", res)
		require.True(t, res.OK, "settled result = %+v, want {42 Screencap true}", res)
		require.Equal(t, details, string(res.Details), "settled details = %q, want byte-for-byte identical to the details sub-object in the message %q", res.Details, details)
		require.Equal(t, 0, pendingOccupancy(s), "pending occupancy after settlement = %d, want 0", pendingOccupancy(s))
		select {
		case <-s.faultNotify():
			require.Fail(t, "a valid completion must not trigger a fault")
		default:
		}
		assert.Equal(t, uint64(0), s.currentLostEvents(), "lost count = %d, want 0", s.currentLostEvents())
	})

	t.Run("a ret=false message settles as a native failure", func(t *testing.T) {
		d, s := newSettlementWiredDispatcher(t, 16)
		defer closeAndDrain(t, d)
		id := AsyncID(43)
		payload := asyncCallInfoPayload(id, "Click", `{"ret":false}`)
		op := mustAdmitPending(t, s, id, "submission with a failing completion")

		enqueue(t, d, asyncCallInfoMessageID, payload)
		mustReadEvent(t, d.events())

		res, err := waitGuarded(t, op, context.Background())
		require.ErrorIs(t, err, native.ErrNativeRejected, "wait error for ret=false = %v, want ErrNativeRejected", err)
		require.Equal(t, id, res.ID, "ret=false result = %+v, want the intact snapshot coexisting with the failure error", res)
		require.Equal(t, "Click", res.What, "ret=false result = %+v, want the intact snapshot coexisting with the failure error", res)
		require.False(t, res.OK, "ret=false result = %+v, want the intact snapshot coexisting with the failure error", res)
		require.Equal(t, `{"ret":false}`, string(res.Details), "ret=false result = %+v, want the intact snapshot coexisting with the failure error", res)
		assert.NotErrorIs(t, err, native.ErrClosed, "ret=false is this Operation's normal failure and must not be mixed into the lifecycle/overflow classes: %v", err)
		assert.NotErrorIs(t, err, native.ErrEventOverflow, "ret=false is this Operation's normal failure and must not be mixed into the lifecycle/overflow classes: %v", err)
		// ret=false 不使实例故障。
		select {
		case <-s.faultNotify():
			require.Fail(t, "ret=false must not fault the instance")
		default:
		}
		require.Equal(t, 0, pendingOccupancy(s), "pending occupancy after settlement = %d, want 0", pendingOccupancy(s))
	})

	t.Run("an unknown-ID completion without a window faults with ErrProtocol, raw still delivered", func(t *testing.T) {
		//  已接提交高水位与关联语义(无窗口的未知 ID 为
		// ErrProtocol):不能归因的完成不得静默丢弃矛盾证据,以 ErrProtocol
		// 故障保留首因;消息原文仍按公开预算交付,pending 记录由首因
		// 线性化扫描统一结算,等待者见故障而非成功。
		d, s := newSettlementWiredDispatcher(t, 16)
		defer closeAndDrain(t, d)
		id := AsyncID(100)
		op := mustAdmitPending(t, s, id, "unresolved submission")

		unknown := asyncCallInfoPayload(AsyncID(999), "Screencap", `{"ret":true}`)
		enqueue(t, d, asyncCallInfoMessageID, unknown)
		ev := mustReadEvent(t, d.events())
		require.Equal(t, string(unknown), string(ev.JSON), "unknown completion event raw = %s, want %s", ev.JSON, unknown)

		// 故障即首因:错误链可达 ErrProtocol;记录被首因扫描统一结算,
		// 等待取得首因而非成功。
		res, err := waitGuarded(t, op, context.Background())
		require.ErrorIs(t, err, native.ErrProtocol, "wait error after the unknown completion = %v, want ErrProtocol reachable along the first-cause chain", err)
		require.Equal(t, AsyncID(0), res.ID, "the fault settlement must carry no result: %+v", res)
		require.Empty(t, res.What, "the fault settlement must carry no result: %+v", res)
		require.False(t, res.OK, "the fault settlement must carry no result: %+v", res)
		require.Nil(t, res.Details, "the fault settlement must carry no result: %+v", res)
		got := s.currentFault()
		require.NotNil(t, got, "first cause = %v, want ErrProtocol", got)
		require.ErrorIs(t, got, native.ErrProtocol, "first cause = %v, want ErrProtocol", got)
		select {
		case <-s.faultNotify():
		default:
			require.Fail(t, "an unknown completion must trigger the one-shot fault notification")
		}
		require.Equal(t, 0, pendingOccupancy(s), "occupancy after the first-cause scan = %d, want 0", pendingOccupancy(s))
	})
}

// TestOperationSettlementPanicDiscipline 验证 10(panic 纪
// 律):operation 恰一次结算,任何形式(完成/关闭/故障)的重复结算
// 都是内部不变量破坏,panic;无因故障结算 panic;nil 与零值
// operation 不可用,结算/等待按实现契约 panic。
func TestOperationSettlementPanicDiscipline(t *testing.T) {
	t.Run("duplicate settlement panics", func(t *testing.T) {
		id := AsyncID(41)
		op := newOperation(id)
		op.complete(newResultRecord(id, "Click", true, json.RawMessage(`{"ret":true}`)))
		assertPanics(t, "complete after complete", func() {
			op.complete(newResultRecord(id, "Click", true, json.RawMessage(`{"ret":true}`)))
		})
		assertPanics(t, "settle closed after complete", func() { op.settleClosed() })
		assertPanics(t, "settle fault after complete", func() {
			op.settleFault(fmt.Errorf("late fault: %w", native.ErrProtocol))
		})
	})

	t.Run("fault settlement without a cause panics", func(t *testing.T) {
		op := newOperation(42)
		assertPanics(t, "settleFault with nil cause", func() { op.settleFault(nil) })
	})

	t.Run("nil and zero-value operations are unusable", func(t *testing.T) {
		// 零值结构体的 done 通道不存在:结算路径 close nil 通道即
		// panic(恰一次结算纪律的最后防线)。
		var zero operation
		assertPanics(t, "zero-value operation complete", func() {
			zero.complete(newResultRecord(43, "Click", true, json.RawMessage(`{"ret":true}`)))
		})
		assertPanics(t, "zero-value operation close settlement", func() { zero.settleClosed() })
		// nil 指针:读取任何字段即 panic。
		var nilOp *operation
		assertPanics(t, "nil operation wait", func() { _, _ = nilOp.wait(context.Background()) })
		assertPanics(t, "nil operation complete", func() {
			nilOp.complete(newResultRecord(44, "Click", true, json.RawMessage(`{"ret":true}`)))
		})
	})
}
