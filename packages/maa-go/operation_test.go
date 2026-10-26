// 本文件是 Operation 结算与等待的受控用例(MAA-19):白盒同包验证
// 绑定设计 §5、§6.1、§7、§7.1、§8 的结算契约——完成、关闭与故障三
// 类原因经同一单次结算路径竞争时先到者胜且所有等待者观察同一终态、
// 三类错误语义互可辨识、Wait 取消只结束本次等待且已发布终态优先、
// 等待快照相互独立、丢弃调用者引用不影响实例关联且容量由结算统一
// 回收、pending 表准入边界与首因线性化扫描,以及 dispatcher 结算接
// 线(先结算后投递)与重复结算 panic 纪律。
//
// 结构性说明(验收 5「不启动逐操作轮询」):经代码审查确认,
// operation.go 的 newOperation 只建立终态存储与通知通道,不构造任
// 何 goroutine;instanceState 的结算入口(settleCompletion、首因线
// 性化扫描、settleRemainingPending)全部在状态锁内同步完成移除与
// 回收,不存在逐操作轮询、定时器或 reaper。本文件不再做运行时
// goroutine 计数断言。
//
// 全部用例以受控屏障(channel/WaitGroup/原子变量)确定性同步,不
// 加载任何原生库,CGO_ENABLED=0 可跑;防挂死只作兜底,不作同步手
// 段;「不会发生」类断言使用有界观察窗。

package maa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
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
	if err != nil {
		t.Fatalf("准入 pending operation %d(%s)不得失败: %v", id, what, err)
	}
	if op == nil {
		t.Fatalf("准入 pending operation %d 返回 nil 记录", id)
	}
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
		t.Fatal("operation.wait 未在防挂死上限内返回:done 通知疑似未关闭")
		return AsyncResult{}, nil
	}
}

// asyncCallInfoPayload 构造一条 AsyncCallInfo 回调消息原文:details
// 是消息中 details 子对象的原始文本,逐字节嵌入(含内部空白)。
func asyncCallInfoPayload(id AsyncID, what, details string) []byte {
	return []byte(fmt.Sprintf(`{"what":%q,"async_call_id":%d,"details":%s}`, what, int32(id), details))
}

// TestOperationSettlesExactlyOnceUnderRaces 钉住验收条件 1「多个完
// 成或关闭/故障竞争只能结算一次,所有等待者观察同一终态」:并发等
// 待者全部取得同一份终态;完成结算、关闭清空与首因线性化扫描三方
// 竞争时每个 operation 恰好取得一种终态,先结算者胜,后到的结算入
// 口不得覆盖已发布终态、不得 panic。
func TestOperationSettlesExactlyOnceUnderRaces(t *testing.T) {
	t.Run("8 个并发等待者观察同一完成终态", func(t *testing.T) {
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
			t.Fatal("并发等待者未在防挂死上限内收敛:done 通知疑似未关闭")
		}
		if n := mismatch.Load(); n != 0 {
			t.Fatalf("%d 个等待者取得的终态与唯一一次结算结果不一致", n)
		}
		// 全部并发等待收敛后,再次等待仍取得同一终态。
		res, err := waitGuarded(t, op, context.Background())
		if err != nil || res.ID != id || res.What != what || !res.OK || string(res.Details) != details {
			t.Fatalf("结算后的单独等待 = (%+v, %v), want 同一完成终态", res, err)
		}
	})

	t.Run("完成、关闭扫描与故障扫描竞争恰一终态", func(t *testing.T) {
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
		faultCause := fmt.Errorf("竞争故障: %w", native.ErrProtocol)
		lateCause := fmt.Errorf("后到故障: %w", native.ErrEventOverflow)

		// classify 把一次等待结果归入三类终态之一;字段越界或类别串
		// 染归入失败类别,由调用方断言。
		classify := func(res AsyncResult, err error) string {
			zero := res.ID == 0 && res.What == "" && !res.OK && res.Details == nil
			switch {
			case err == nil:
				if res.ID != id || res.What != what || !res.OK || string(res.Details) != details {
					return "成功内容损坏"
				}
				return "完成"
			case errors.Is(err, faultCause):
				if !zero || errors.Is(err, native.ErrClosed) || errors.Is(err, native.ErrNativeRejected) {
					return "故障类别损坏"
				}
				return "故障"
			case errors.Is(err, native.ErrClosed):
				if !zero || errors.Is(err, faultCause) {
					return "关闭类别损坏"
				}
				return "关闭"
			default:
				return "未知终态"
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
				t.Fatalf("第 %d 轮竞争未在防挂死上限内收敛", round)
			}

			if len(seen) != roundWaiters {
				t.Fatalf("第 %d 轮等待者结果数 = %d, want %d", round, len(seen), roundWaiters)
			}
			for i, class := range seen {
				if class != seen[0] {
					t.Fatalf("第 %d 轮等待者 %d 见终态 %q, 与等待者 0 的 %q 不一致", round, i, class, seen[0])
				}
			}
			switch seen[0] {
			case "完成", "关闭", "故障":
			default:
				t.Fatalf("第 %d 轮终态类别 %q 越界", round, seen[0])
			}

			// 先结算者胜:后到的其余结算入口不得覆盖、不得 panic。
			// electClose 把阶段统一带入 closing(ready/faulted 合法迁
			// 移,已 closing 幂等失败),settleRemainingPending 才可安
			// 全调用;完成未命中、故障 sticky 后空表扫描均为空操作。
			s.electClose()
			s.settleCompletion(successInfo)
			s.recordFault(lateCause)
			s.settleRemainingPending()
			again, err := op.wait(context.Background())
			if class := classify(again, err); class != seen[0] {
				t.Fatalf("第 %d 轮后到结算改写终态: %q → %q(%+v, %v)", round, seen[0], class, again, err)
			}
		}
	})

	t.Run("完成先行时后到的关闭与故障结算不得覆盖", func(t *testing.T) {
		s := newReadyState(t, 0x0A78, 8)
		id := AsyncID(78)
		op := mustAdmitPending(t, s, id, "先完成的提交")
		successInfo := completionInfo{ID: id, What: "FirstWin", OK: true, Details: json.RawMessage(`{"ret":true}`)}
		s.settleCompletion(successInfo)

		// 后到的完成未命中;后到的故障记录首因但扫不到记录;closing
		// 清空为空操作。任何一路都不得改写已发布终态,也不得 panic。
		mustElectClose(t, s)
		s.settleCompletion(successInfo)
		s.recordFault(fmt.Errorf("后到故障: %w", native.ErrEventOverflow))
		s.settleRemainingPending()

		res, err := waitGuarded(t, op, context.Background())
		if err != nil || !res.OK || res.ID != id || string(res.Details) != `{"ret":true}` {
			t.Fatalf("后到结算改写了终态: (%+v, %v), want 完成终态原样", res, err)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("后到结算不得复活 pending 记录: 占用 = %d", n)
		}
	})
}

// TestOperationTerminalErrorSemantics 钉住验收条件 2「ret=false 返
// 回原生结果与可辨识失败错误,关闭和事件丢失保持不同错误语义」:
// 完成失败同时给出完好快照与包装 ErrNativeRejected 的错误;关闭结
// 算包装 ErrClosed 且无结果;故障结算原样返回首因;三类错误交叉
// errors.Is 互不可混淆。已结算后 ID 保持可读(§8.1)。
func TestOperationTerminalErrorSemantics(t *testing.T) {
	const (
		id      = AsyncID(601)
		what    = "Screencap"
		details = `{"ret":false,"cost":7}`
	)

	t.Run("ret=false 返回完好结果与 ErrNativeRejected", func(t *testing.T) {
		op := newOperation(id)
		op.complete(newResultRecord(id, what, false, json.RawMessage(details)))

		res, err := waitGuarded(t, op, context.Background())
		if err == nil {
			t.Fatal("ret=false 必须返回非 nil 错误")
		}
		if !errors.Is(err, native.ErrNativeRejected) {
			t.Errorf("errors.Is(err, native.ErrNativeRejected) = false; err = %v", err)
		}
		// 结果与可辨识失败错误同时给出,快照字段完好(§5:不在 OK
		// 字段编造结构化原因)。
		if res.ID != id || res.What != what || res.OK || string(res.Details) != details {
			t.Fatalf("ret=false 的结果 = %+v, want 字段完好的失败结果", res)
		}
		if got := op.id(); got != id {
			t.Errorf("已结算后 ID 必须保持可读: got %d, want %d", got, id)
		}
	})

	t.Run("关闭结算返回 ErrClosed 且无结果", func(t *testing.T) {
		op := newOperation(id)
		op.settleClosed()

		res, err := waitGuarded(t, op, context.Background())
		if !errors.Is(err, native.ErrClosed) {
			t.Fatalf("errors.Is(err, native.ErrClosed) = false; err = %v", err)
		}
		if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
			t.Fatalf("关闭结算必须无结果: %+v", res)
		}
		if got := op.id(); got != id {
			t.Errorf("已结算后 ID 必须保持可读: got %d, want %d", got, id)
		}
	})

	t.Run("故障结算原样返回首因", func(t *testing.T) {
		op := newOperation(id)
		cause := fmt.Errorf("事件队列溢出: %w", native.ErrEventOverflow)
		op.settleFault(cause)

		res, err := waitGuarded(t, op, context.Background())
		if !errors.Is(err, cause) {
			t.Fatalf("errors.Is(err, cause) = false; err = %v, want 首因 %v", err, cause)
		}
		if !errors.Is(err, native.ErrEventOverflow) {
			t.Errorf("errors.Is(err, native.ErrEventOverflow) = false; err = %v", err)
		}
		if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
			t.Fatalf("故障结算必须无结果: %+v", res)
		}
		if got := op.id(); got != id {
			t.Errorf("已结算后 ID 必须保持可读: got %d, want %d", got, id)
		}
	})

	t.Run("三类终态错误互不可混淆", func(t *testing.T) {
		nativeOp := newOperation(611)
		nativeOp.complete(newResultRecord(611, what, false, json.RawMessage(details)))
		closedOp := newOperation(612)
		closedOp.settleClosed()
		faultOp := newOperation(613)
		cause := fmt.Errorf("首因: %w", native.ErrEventOverflow)
		faultOp.settleFault(cause)

		_, nativeErr := waitGuarded(t, nativeOp, context.Background())
		_, closedErr := waitGuarded(t, closedOp, context.Background())
		_, faultErr := waitGuarded(t, faultOp, context.Background())

		// 原生失败只含 ErrNativeRejected,不得混入关闭/溢出。
		if !errors.Is(nativeErr, native.ErrNativeRejected) {
			t.Errorf("原生失败错误 = %v, want 含 ErrNativeRejected", nativeErr)
		}
		if errors.Is(nativeErr, native.ErrClosed) || errors.Is(nativeErr, native.ErrEventOverflow) || errors.Is(nativeErr, cause) {
			t.Errorf("原生失败错误混入相邻类别: %v", nativeErr)
		}
		// 关闭只含 ErrClosed。
		if !errors.Is(closedErr, native.ErrClosed) {
			t.Errorf("关闭错误 = %v, want 含 ErrClosed", closedErr)
		}
		if errors.Is(closedErr, native.ErrNativeRejected) || errors.Is(closedErr, native.ErrEventOverflow) || errors.Is(closedErr, cause) {
			t.Errorf("关闭错误混入相邻类别: %v", closedErr)
		}
		// 故障只沿首因链可达,不得混入原生失败/关闭。
		if !errors.Is(faultErr, cause) || !errors.Is(faultErr, native.ErrEventOverflow) {
			t.Errorf("故障错误 = %v, want 沿首因链可达 ErrEventOverflow", faultErr)
		}
		if errors.Is(faultErr, native.ErrNativeRejected) || errors.Is(faultErr, native.ErrClosed) {
			t.Errorf("故障错误混入相邻类别: %v", faultErr)
		}
	})
}

// TestOperationWaitCancellation 钉住验收条件 3「一次 Wait 取消后仍
// 可再次等待并取得后续真实结果」:取消/超时只结束本次等待,返回
// ctx.Err() 与零值结果,不改写终态、不提前回收 pending 记录;终态
// 已发布时与取消竞争,已发布终态优先(§8.1)。
func TestOperationWaitCancellation(t *testing.T) {
	t.Run("已取消 context 返回 Canceled 且不改写终态、不回收槽位", func(t *testing.T) {
		s := newReadyState(t, 0x0A31, 8)
		id := AsyncID(21)
		op := mustAdmitPending(t, s, id, "将被取消等待的提交")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		res, err := waitGuarded(t, op, ctx)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("已取消 ctx 的等待错误 = %v, want context.Canceled", err)
		}
		if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
			t.Fatalf("取消的等待必须返回零值结果: %+v", res)
		}
		// 取消不迁移终态、不提前回收:记录仍在实例 pending 表中。
		if n := pendingOccupancy(s); n != 1 {
			t.Fatalf("取消一次等待后 pending 占用 = %d, want 1(取消不提前回收)", n)
		}
		// 之后仍可再次等待取得真实结果。
		s.settleCompletion(completionInfo{ID: id, What: "Click", OK: true, Details: json.RawMessage(`{"ret":true}`)})
		res2, err2 := waitGuarded(t, op, context.Background())
		if err2 != nil || res2.ID != id || !res2.OK {
			t.Fatalf("取消后的再次等待 = (%+v, %v), want 真实完成结果", res2, err2)
		}
	})

	t.Run("已到期 deadline 返回 DeadlineExceeded 且可再等待", func(t *testing.T) {
		s := newReadyState(t, 0x0A32, 8)
		id := AsyncID(22)
		op := mustAdmitPending(t, s, id, "超时等待的提交")
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Hour))
		defer cancel()

		res, err := waitGuarded(t, op, ctx)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("已到期 deadline 的等待错误 = %v, want context.DeadlineExceeded", err)
		}
		if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
			t.Fatalf("超时的等待必须返回零值结果: %+v", res)
		}
		if n := pendingOccupancy(s); n != 1 {
			t.Fatalf("超时一次等待后 pending 占用 = %d, want 1", n)
		}
		op.complete(newResultRecord(id, "Click", true, json.RawMessage(`{"ret":true}`)))
		res2, err2 := waitGuarded(t, op, context.Background())
		if err2 != nil || res2.ID != id || !res2.OK || string(res2.Details) != `{"ret":true}` {
			t.Fatalf("超时后的再次等待 = (%+v, %v), want 真实完成结果", res2, err2)
		}
	})

	t.Run("终态已发布时取消的 ctx 仍取得终态", func(t *testing.T) {
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
			if errors.Is(err, context.Canceled) {
				t.Fatalf("第 %d 轮:已结算后取消 ctx 的等待被误报为取消(err = %v), want 终态优先", round, err)
			}
			if err != nil || res.ID != id || !res.OK || string(res.Details) != `{"ret":true}` {
				t.Fatalf("第 %d 轮:已结算后取消 ctx 的等待 = (%+v, %v), want 终态优先", round, res, err)
			}
		}
	})
}

// TestOperationWaitSnapshotIndependence 钉住验收条件 4「修改某次
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

	t.Run("改写一次等待的副本不影响缓存与其他等待者", func(t *testing.T) {
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
		if w2.ID != id || w2.What != what || !w2.OK || string(w2.Details) != details {
			t.Fatalf("改写先前副本影响了另一等待者的结果: %+v", w2)
		}
		w3, _ := waitGuarded(t, op, context.Background())
		if w3.ID != id || w3.What != what || !w3.OK || string(w3.Details) != details {
			t.Fatalf("改写先前副本影响了缓存: %+v", w3)
		}
		// 改写 w2 同理:w1 保持被改写状态(它是调用者拥有的独立数据),
		// 新快照仍取原始内容。
		for i := range w2.Details {
			w2.Details[i] = '@'
		}
		w4, _ := waitGuarded(t, op, context.Background())
		if string(w4.Details) != details || w4.What != what {
			t.Fatalf("改写第二份副本影响了缓存: %+v", w4)
		}
	})

	t.Run("并发等待各自改写互不串扰", func(t *testing.T) {
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
		if polluted.Load() != 0 {
			t.Fatalf("%d 个并发等待者取得被污染的副本", polluted.Load())
		}
		final, err := waitGuarded(t, op, context.Background())
		if err != nil || string(final.Details) != details || final.What != what {
			t.Fatalf("并发改写影响了缓存: (%+v, %v)", final, err)
		}
	})
}

// TestPendingOperationOwnershipAndReclaim 钉住验收条件 5「调用者丢
// 弃 Operation 不影响实例持有的关联;容量由最终结算统一回收,不启
// 动逐操作轮询」:白盒确认记录仍由实例 pending 表持有,经
// settleCompletion 结算后移出表、容量回收可再 admit;取消一次等待
// 不提前释放槽位。无轮询/reaper 为结构性结论,见文件头代码审查说
// 明,不做运行时 goroutine 计数。
func TestPendingOperationOwnershipAndReclaim(t *testing.T) {
	t.Run("丢弃调用者引用不影响实例关联,结算统一回收", func(t *testing.T) {
		s := newReadyState(t, 0x0A51, 4)
		id := AsyncID(9)
		mustAdmitPending(t, s, id, "将被丢弃的提交")
		// 调用者从此不再持有 *operation:实例关联不受影响。
		if n := pendingOccupancy(s); n != 1 {
			t.Fatalf("admit 后 pending 占用 = %d, want 1", n)
		}
		s.mu.Lock()
		_, stillTracked := s.pending[id]
		s.mu.Unlock()
		if !stillTracked {
			t.Fatal("丢弃调用者引用后实例 pending 表必须仍持有该记录")
		}
		// 只有最终结算移出记录并回收 PendingOperations 容量。
		s.settleCompletion(completionInfo{ID: id, What: "DroppedCall", OK: true, Details: json.RawMessage(`{"ret":true}`)})
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("结算后 pending 占用 = %d, want 0(容量已回收)", n)
		}
		if _, err := s.admitPendingOperation(id, "回收后的新提交"); err != nil {
			t.Fatalf("回收后同 ID 再次 admit 不得失败: %v", err)
		}
	})

	t.Run("取消一次等待不提前释放槽位", func(t *testing.T) {
		s := newReadyState(t, 0x0A52, 4)
		id := AsyncID(10)
		op := mustAdmitPending(t, s, id, "取消后仍占位的提交")
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if _, err := waitGuarded(t, op, ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消的等待错误 = %v, want context.Canceled", err)
		}
		// 取消不释放槽位:容量仍被占用,真实完成仍可按 ID 结算。
		if n := pendingOccupancy(s); n != 1 {
			t.Fatalf("取消后 pending 占用 = %d, want 1", n)
		}
		s.settleCompletion(completionInfo{ID: id, What: "Click", OK: false, Details: json.RawMessage(`{"ret":false}`)})
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("完成结算后 pending 占用 = %d, want 0", n)
		}
	})
}

// TestAdmitPendingOperationBoundaries 钉住验收条件 6「容量边界与准
// 入阶段拒绝」:pending 表填满后 admit 返回包装 ErrOperationLimit
// 的错误且实例不故障(阶段仍 ready、无首因、故障通知不触发),被拒
// 的准入不占容量,结算释放后可再 admit;closing/faulted/closed 阶段
// 分别按各自哨兵拒绝;id==0 与 initializing 阶段准入按编程错误
// panic。
func TestAdmitPendingOperationBoundaries(t *testing.T) {
	t.Run("容量填满拒绝包装 ErrOperationLimit 且实例不故障", func(t *testing.T) {
		s := newReadyState(t, 0x0A61, 2)
		mustAdmitPending(t, s, AsyncID(1), "第 1 笔")
		mustAdmitPending(t, s, AsyncID(2), "第 2 笔")

		_, err := s.admitPendingOperation(AsyncID(3), "第 3 笔")
		if err == nil {
			t.Fatal("容量填满后 admit 必须拒绝")
		}
		if !errors.Is(err, native.ErrOperationLimit) {
			t.Errorf("errors.Is(err, native.ErrOperationLimit) = false; err = %v", err)
		}
		if errors.Is(err, native.ErrClosed) || errors.Is(err, native.ErrClosing) {
			t.Errorf("容量拒绝不得误标为 closed/closing: %v", err)
		}
		if !strings.Contains(err.Error(), "第 3 笔") {
			t.Errorf("拒绝错误应携带 what 上下文文本:\n%s", err.Error())
		}
		// 实例不因容量拒绝而故障。
		if got := s.currentPhase(); got != phaseReady {
			t.Errorf("容量拒绝后阶段 = %d, want 仍为 ready", got)
		}
		if s.currentFault() != nil {
			t.Errorf("容量拒绝后首因 = %v, want nil", s.currentFault())
		}
		select {
		case <-s.faultNotify():
			t.Error("容量拒绝不得触发一次性故障通知")
		default:
		}
		// 被拒的准入不占容量:占用仍为 2。
		if n := pendingOccupancy(s); n != 2 {
			t.Fatalf("被拒的 admit 后占用 = %d, want 2(拒绝不占容量)", n)
		}
		// 结算释放后可再 admit;再次填满后再次拒绝。
		s.settleCompletion(completionInfo{ID: 1, What: "第 1 笔", OK: true, Details: json.RawMessage(`{"ret":true}`)})
		if n := pendingOccupancy(s); n != 1 {
			t.Fatalf("结算释放后占用 = %d, want 1", n)
		}
		if _, err := s.admitPendingOperation(AsyncID(3), "第 3 笔"); err != nil {
			t.Fatalf("结算释放后 admit 不得失败: %v", err)
		}
		if _, err := s.admitPendingOperation(AsyncID(4), "第 4 笔"); !errors.Is(err, native.ErrOperationLimit) {
			t.Fatalf("再次填满后必须再次拒绝: %v", err)
		}
	})

	t.Run("closing、faulted、closed 阶段拒绝准入", func(t *testing.T) {
		closing := newReadyState(t, 0x0A62, 8)
		mustElectClose(t, closing)
		_, err := closing.admitPendingOperation(AsyncID(1), "closing 中的提交")
		if !errors.Is(err, native.ErrClosing) {
			t.Fatalf("errors.Is(err, native.ErrClosing) = false; err = %v", err)
		}
		if errors.Is(err, native.ErrClosed) || errors.Is(err, native.ErrOperationLimit) {
			t.Errorf("closing 拒绝不得误标为相邻类别: %v", err)
		}

		faulted := newReadyState(t, 0x0A63, 8)
		cause := fmt.Errorf("事件队列溢出: %w", native.ErrEventOverflow)
		faulted.recordFault(cause)
		_, err = faulted.admitPendingOperation(AsyncID(2), "faulted 中的提交")
		if !errors.Is(err, cause) || !errors.Is(err, native.ErrEventOverflow) {
			t.Fatalf("faulted 拒绝应沿链到达首因: err = %v", err)
		}
		if errors.Is(err, native.ErrClosing) || errors.Is(err, native.ErrClosed) {
			t.Errorf("faulted 拒绝不得误标为 closing/closed: %v", err)
		}

		closed := newClosedState(t, 0x0A64)
		_, err = closed.admitPendingOperation(AsyncID(3), "closed 中的提交")
		if !errors.Is(err, native.ErrClosed) {
			t.Fatalf("errors.Is(err, native.ErrClosed) = false; err = %v", err)
		}
		if errors.Is(err, native.ErrClosing) || errors.Is(err, native.ErrOperationLimit) {
			t.Errorf("closed 拒绝不得误标为相邻类别: %v", err)
		}
	})

	t.Run("id 为 0 与 initializing 阶段准入 panic", func(t *testing.T) {
		ready := newReadyState(t, 0x0A65, 8)
		assertPanics(t, "id 为 0 的 pending 准入", func() { _, _ = ready.admitPendingOperation(0, "零 ID") })
		initializing := newInstanceState(0x0A66, 8, 1_048_576)
		assertPanics(t, "initializing 阶段的 pending 准入", func() { _, _ = initializing.admitPendingOperation(1, "未发布实例") })
	})
}

// TestFirstFaultLinearizesPendingOperations 钉住 §7.1 首次故障线性
// 化扫描(验收 7):有 pending 时 recordFault 在状态门内立即将全部
// 未决 Operation 统一结为首因;故障前已结算的成功不改写;故障后到
// 达的完成消息查不到记录,不能覆盖故障终态;重复故障不重复扫描、
// 不覆盖首因、不 panic。
func TestFirstFaultLinearizesPendingOperations(t *testing.T) {
	t.Run("首因立即统一结算全部未决 pending", func(t *testing.T) {
		s := newReadyState(t, 0x0A71, 8)
		op1 := mustAdmitPending(t, s, AsyncID(11), "未决 1")
		op2 := mustAdmitPending(t, s, AsyncID(12), "未决 2")
		op3 := mustAdmitPending(t, s, AsyncID(13), "未决 3")
		// 故障前先结算 op2:已结算的成功不得被故障改写(§7.1)。
		s.settleCompletion(completionInfo{ID: 12, What: "先到成功", OK: true, Details: json.RawMessage(`{"ret":true}`)})

		cause := fmt.Errorf("事件队列溢出: %w", native.ErrEventOverflow)
		s.recordFault(cause)

		for i, op := range []*operation{op1, op3} {
			res, err := waitGuarded(t, op, context.Background())
			if !errors.Is(err, cause) {
				t.Fatalf("未决项 %d 的错误 = %v, want 首因 %v", i+1, err, cause)
			}
			if errors.Is(err, native.ErrClosed) || errors.Is(err, native.ErrNativeRejected) {
				t.Errorf("未决项 %d 的故障错误混入相邻类别: %v", i+1, err)
			}
			if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
				t.Errorf("未决项 %d 的故障结算必须无结果: %+v", i+1, res)
			}
		}
		res2, err2 := waitGuarded(t, op2, context.Background())
		if err2 != nil || !res2.OK || string(res2.Details) != `{"ret":true}` {
			t.Fatalf("故障前已结算的成功被改写: (%+v, %v)", res2, err2)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("首因扫描后 pending 占用 = %d, want 0", n)
		}
		if got := s.currentPhase(); got != phaseFaulted {
			t.Errorf("首因记录后阶段 = %d, want faulted", got)
		}
	})

	t.Run("故障后到达的完成不能覆盖故障终态", func(t *testing.T) {
		s := newReadyState(t, 0x0A72, 8)
		id := AsyncID(21)
		op := mustAdmitPending(t, s, id, "故障前未决")
		cause := fmt.Errorf("首因: %w", native.ErrProtocol)
		s.recordFault(cause)

		// 故障后完成消息查不到记录,只能按未命中放弃:等待者必须见
		// 故障而非成功。
		s.settleCompletion(completionInfo{ID: id, What: "迟到完成", OK: true, Details: json.RawMessage(`{"ret":true}`)})
		res, err := waitGuarded(t, op, context.Background())
		if !errors.Is(err, cause) || res.ID != 0 {
			t.Fatalf("故障后到达的完成改写了终态: (%+v, %v), want 首因故障", res, err)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("未命中的完成不得复活记录: 占用 = %d", n)
		}
	})

	t.Run("重复故障不重复扫描且不覆盖首因", func(t *testing.T) {
		s := newReadyState(t, 0x0A73, 8)
		op1 := mustAdmitPending(t, s, AsyncID(31), "未决 A")
		op2 := mustAdmitPending(t, s, AsyncID(32), "未决 B")
		first := fmt.Errorf("首因: %w", native.ErrProtocol)
		second := fmt.Errorf("后到: %w", native.ErrEventOverflow)

		s.recordFault(first)
		// 重复故障不得 panic(重复关闭 faultDone 或重复结算都会在实
		// 现缺陷下以 panic 暴露),也不得让等待者改见后到原因。
		s.recordFault(second)

		for i, op := range []*operation{op1, op2} {
			_, err := waitGuarded(t, op, context.Background())
			if !errors.Is(err, first) || errors.Is(err, second) {
				t.Fatalf("未决项 %d 的错误 = %v, want 首因 %v 且不含后到", i+1, err, first)
			}
		}
		if got := s.currentFault(); !errors.Is(got, first) || errors.Is(got, native.ErrEventOverflow) {
			t.Fatalf("首因被后到故障覆盖: %v", got)
		}
		select {
		case <-s.faultNotify():
		default:
			t.Fatal("首因必须关闭一次性故障通知")
		}
	})
}

// TestSettleRemainingPendingClosingSemantics 钉住验收条件 8:closing
// 阶段把剩余 pending 全部以 ErrClosed 结算一次并清空,表已空时为无
// 害空操作;非 closing 阶段调用按编程错误 panic。
func TestSettleRemainingPendingClosingSemantics(t *testing.T) {
	t.Run("closing 清剩余 pending 为 ErrClosed", func(t *testing.T) {
		s := newReadyState(t, 0x0A81, 8)
		op1 := mustAdmitPending(t, s, AsyncID(1), "剩余 1")
		op2 := mustAdmitPending(t, s, AsyncID(2), "剩余 2")
		mustElectClose(t, s)

		s.settleRemainingPending()
		for i, op := range []*operation{op1, op2} {
			res, err := waitGuarded(t, op, context.Background())
			if !errors.Is(err, native.ErrClosed) {
				t.Fatalf("剩余项 %d 的错误 = %v, want ErrClosed", i+1, err)
			}
			if errors.Is(err, native.ErrNativeRejected) || errors.Is(err, native.ErrClosing) {
				t.Errorf("剩余项 %d 的关闭错误混入相邻类别: %v", i+1, err)
			}
			if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
				t.Errorf("剩余项 %d 的关闭结算必须无结果: %+v", i+1, res)
			}
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("关闭清空后占用 = %d, want 0", n)
		}
		// 表已空后再次调用为无害空操作,随后可正常走完关闭发布。
		s.settleRemainingPending()
		finishClose(t, s, nil)
		if got := s.currentPhase(); got != phaseClosed {
			t.Fatalf("清空后完成关闭,阶段 = %d, want closed", got)
		}
	})

	t.Run("非 closing 阶段调用 panic", func(t *testing.T) {
		ready := newReadyState(t, 0x0A82, 8)
		mustAdmitPending(t, ready, AsyncID(3), "未决项")
		assertPanics(t, "ready 阶段 settleRemainingPending", func() { ready.settleRemainingPending() })

		faulted := newReadyState(t, 0x0A83, 8)
		faulted.recordFault(fmt.Errorf("故障: %w", native.ErrProtocol))
		assertPanics(t, "faulted 阶段 settleRemainingPending", func() { faulted.settleRemainingPending() })

		closed := newClosedState(t, 0x0A84)
		assertPanics(t, "closed 阶段 settleRemainingPending", func() { closed.settleRemainingPending() })
	})
}

// newSettlementWiredDispatcher 构造 MAA-19 真实接线的事件分发器:
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

// TestDispatcherWiringSettlesCompletion 钉住验收条件 9(dispatcher
// 集成):newEventDispatcher 接 instanceState.settleCompletion 后,
// 合法 AsyncCallInfo 先结算再投递,公开事件可读时等待立即取得终态,
// details 子对象逐字节保留(含内部空白);ret=false 结算为原生失
// 败;未命中 ID 不结算、不故障,事件照常投递(MAA-23 边界:本单元
// 无提交高水位核对,未知/重复完成不归因、不猜测、不计丢失)。
func TestDispatcherWiringSettlesCompletion(t *testing.T) {
	t.Run("合法完成先结算后投递,details 原文逐字节保留", func(t *testing.T) {
		d, s := newSettlementWiredDispatcher(t, 16)
		defer closeAndDrain(t, d)
		id := AsyncID(42)
		details := `{"ret":true, "cost":3}` // 内部空白必须原样保留(非紧凑化)
		payload := asyncCallInfoPayload(id, "Screencap", details)
		op := mustAdmitPending(t, s, id, "接线提交")

		enqueue(t, d, asyncCallInfoMessageID, payload)
		ev := mustReadEvent(t, d.events())
		if string(ev.JSON) != string(payload) {
			t.Fatalf("公开事件原文 = %s, want 逐字节保留的 %s", ev.JSON, payload)
		}
		// 先结算后投递(§8 第 5 步):公开事件可读时,等待必须立即取
		// 得终态,不得再有任何延迟。
		res, err := waitGuarded(t, op, context.Background())
		if err != nil {
			t.Fatalf("事件可读时等待必须立即返回终态: %v", err)
		}
		if res.ID != id || res.What != "Screencap" || !res.OK {
			t.Fatalf("结算结果 = %+v, want {42 Screencap true}", res)
		}
		if string(res.Details) != details {
			t.Fatalf("结算 details = %q, want 与消息中 details 子对象逐字节一致的 %q", res.Details, details)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("结算后 pending 占用 = %d, want 0", n)
		}
		select {
		case <-s.faultNotify():
			t.Fatal("合法完成不得触发故障")
		default:
		}
		if lost := s.currentLostEvents(); lost != 0 {
			t.Errorf("丢失计数 = %d, want 0", lost)
		}
	})

	t.Run("ret=false 消息结算为原生失败", func(t *testing.T) {
		d, s := newSettlementWiredDispatcher(t, 16)
		defer closeAndDrain(t, d)
		id := AsyncID(43)
		payload := asyncCallInfoPayload(id, "Click", `{"ret":false}`)
		op := mustAdmitPending(t, s, id, "失败完成的提交")

		enqueue(t, d, asyncCallInfoMessageID, payload)
		mustReadEvent(t, d.events())

		res, err := waitGuarded(t, op, context.Background())
		if !errors.Is(err, native.ErrNativeRejected) {
			t.Fatalf("ret=false 的等待错误 = %v, want ErrNativeRejected", err)
		}
		if res.ID != id || res.What != "Click" || res.OK || string(res.Details) != `{"ret":false}` {
			t.Fatalf("ret=false 的结果 = %+v, want 完好快照与失败错误并存", res)
		}
		if errors.Is(err, native.ErrClosed) || errors.Is(err, native.ErrEventOverflow) {
			t.Errorf("ret=false 是该 Operation 的正常失败,不得混入生命周期/溢出类别: %v", err)
		}
		// ret=false 不使实例故障。
		select {
		case <-s.faultNotify():
			t.Fatal("ret=false 不得使实例故障")
		default:
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("结算后 pending 占用 = %d, want 0", n)
		}
	})

	t.Run("无窗口的未知 ID 完成为 ErrProtocol 故障,原文仍交付", func(t *testing.T) {
		// MAA-23 已接提交高水位与关联语义(§7.1:无窗口的未知 ID 为
		// ErrProtocol):不能归因的完成不得静默丢弃矛盾证据,以 ErrProtocol
		// 故障保留首因;消息原文仍按公开预算交付,pending 记录由首因
		// 线性化扫描统一结算,等待者见故障而非成功。
		d, s := newSettlementWiredDispatcher(t, 16)
		defer closeAndDrain(t, d)
		id := AsyncID(100)
		op := mustAdmitPending(t, s, id, "在决提交")

		unknown := asyncCallInfoPayload(AsyncID(999), "Screencap", `{"ret":true}`)
		enqueue(t, d, asyncCallInfoMessageID, unknown)
		ev := mustReadEvent(t, d.events())
		if string(ev.JSON) != string(unknown) {
			t.Fatalf("未知完成的事件原文 = %s, want %s", ev.JSON, unknown)
		}

		// 故障即首因:错误链可达 ErrProtocol;记录被首因扫描统一结算,
		// 等待取得首因而非成功。
		res, err := waitGuarded(t, op, context.Background())
		if !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("未知完成后的等待错误 = %v, want 沿首因链可达 ErrProtocol", err)
		}
		if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
			t.Fatalf("故障结算必须无结果: %+v", res)
		}
		if got := s.currentFault(); got == nil || !errors.Is(got, native.ErrProtocol) {
			t.Fatalf("首因 = %v, want ErrProtocol", got)
		}
		select {
		case <-s.faultNotify():
		default:
			t.Fatal("未知完成必须触发一次性故障通知")
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("首因扫描后占用 = %d, want 0", n)
		}
	})
}

// TestOperationSettlementPanicDiscipline 钉住验收条件 10(panic 纪
// 律):operation 恰一次结算,任何形式(完成/关闭/故障)的重复结算
// 都是内部不变量破坏,panic;无因故障结算 panic;nil 与零值
// operation 不可用,结算/等待按实现契约 panic。
func TestOperationSettlementPanicDiscipline(t *testing.T) {
	t.Run("重复结算 panic", func(t *testing.T) {
		id := AsyncID(41)
		op := newOperation(id)
		op.complete(newResultRecord(id, "Click", true, json.RawMessage(`{"ret":true}`)))
		assertPanics(t, "完成后再完成", func() {
			op.complete(newResultRecord(id, "Click", true, json.RawMessage(`{"ret":true}`)))
		})
		assertPanics(t, "完成后再以关闭结算", func() { op.settleClosed() })
		assertPanics(t, "完成后再以故障结算", func() {
			op.settleFault(fmt.Errorf("迟到故障: %w", native.ErrProtocol))
		})
	})

	t.Run("无因故障结算 panic", func(t *testing.T) {
		op := newOperation(42)
		assertPanics(t, "cause 为 nil 的 settleFault", func() { op.settleFault(nil) })
	})

	t.Run("nil 与零值 operation 不可用", func(t *testing.T) {
		// 零值结构体的 done 通道不存在:结算路径 close nil 通道即
		// panic(恰一次结算纪律的最后防线)。
		var zero operation
		assertPanics(t, "零值 operation 完成", func() {
			zero.complete(newResultRecord(43, "Click", true, json.RawMessage(`{"ret":true}`)))
		})
		assertPanics(t, "零值 operation 关闭结算", func() { zero.settleClosed() })
		// nil 指针:读取任何字段即 panic。
		var nilOp *operation
		assertPanics(t, "nil operation 等待", func() { _, _ = nilOp.wait(context.Background()) })
		assertPanics(t, "nil operation 完成", func() {
			nilOp.complete(newResultRecord(44, "Click", true, json.RawMessage(`{"ret":true}`)))
		})
	})
}
