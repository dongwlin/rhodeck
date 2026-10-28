package maa

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// 注入只经过 fake native 的 callback token；观察结果全部从公开方法读取。
func deliverObservationEvent(t *testing.T, rig *createRig, id MessageID, raw string) {
	t.Helper()
	deliver := native.LookupCallbackDelivery(rig.creator.exToken(0))
	if deliver == nil {
		t.Fatal("实例 callback token 未登记")
	}
	deliver(int32(id), callbackCBytes(raw))
}

func waitObservationSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(hangGuard):
		t.Fatalf("等待 %s 超时", what)
	}
}

func assertObservationOpen(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
		t.Fatalf("%s 提前关闭", what)
	default:
	}
}

func TestInstanceObservationTaskCompletionAndNormalClose(t *testing.T) {
	inst, rig := newSyncRig(t, 0x0B5100)
	if inst.Events() == nil || inst.Done() == nil || inst.Fault() == nil {
		t.Fatal("创建成功后观察通道必须可用")
	}
	if inst.Events() != inst.Events() || inst.Done() != inst.Done() || inst.Fault() != inst.Fault() {
		t.Fatal("重复调用必须返回同一个观察通道")
	}
	if err := inst.Err(); err != nil {
		t.Fatalf("ready Err = %v, want nil", err)
	}
	diag := inst.Diagnostics()
	if diag.LostEvents != 0 || diag.FirstFault != nil || diag.StopDiagnostic != nil || len(diag.RawPrefix) != 0 {
		t.Fatalf("ready Diagnostics = %+v, want 空诊断", diag)
	}
	assertObservationOpen(t, inst.Done(), "Done")
	assertObservationOpen(t, inst.Fault(), "Fault")

	const raw = `{"what":"AllTasksCompleted","future":9007199254740993}`
	deliverObservationEvent(t, rig, 3, raw)
	ev := mustReadEvent(t, inst.Events())
	if ev.MessageID != 3 || ev.Sequence != 1 || string(ev.JSON) != raw {
		t.Fatalf("任务完成事件 = %+v, want 原始消息与序号 1", ev)
	}
	assertObservationOpen(t, inst.Done(), "任务完成后的 Done")
	assertObservationOpen(t, inst.Fault(), "正常任务完成后的 Fault")

	// 缓冲事件保留到关闭之后；Done 的观察者无需先排空 Events。
	const buffered = `{"what":"FutureMessage"}`
	deliverObservationEvent(t, rig, 99, buffered)
	closed := make(chan error, 1)
	go func() { closed <- inst.Close() }()
	waitObservationSignal(t, inst.Done(), "Done")
	if err := inst.Err(); err != nil {
		t.Fatalf("Done 后 Err = %v, want nil", err)
	}
	ev = mustReadEvent(t, inst.Events())
	if ev.MessageID != 99 || ev.Sequence != 2 || string(ev.JSON) != buffered {
		t.Fatalf("关闭后缓冲事件 = %+v, want FutureMessage 与序号 2", ev)
	}
	select {
	case _, ok := <-inst.Events():
		if ok {
			t.Fatal("Done 后收到意外额外事件")
		}
	default:
		t.Fatal("Done 已关闭但 Events 尚未关闭")
	}
	if err := waitErr(t, closed, "Instance.Close"); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	assertObservationOpen(t, inst.Fault(), "正常关闭后的 Fault")
}

func TestInstanceObservationOverflowPreservesCompletedOperation(t *testing.T) {
	inst, rig := newAsyncRig(t, 0x0B5200, func(l *Limits) { l.PublicEvents = 1 })
	completed, err := inst.Screencap()
	if err != nil {
		t.Fatal(err)
	}
	rig.caller.asyncRets = map[string]int32{"asyncClick": 2}
	pending, err := inst.Click(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	deliverAsyncInfo(t, rig.creator.exToken(0), completed.ID(), "Screencap", `{"ret":true}`)
	before, err := waitOperationGuarded(t, completed, context.Background())
	if err != nil || !before.OK {
		t.Fatalf("首次完成 = %+v, %v, want 成功", before, err)
	}
	// 唯一公开缓冲由完成消息占据，消费者停读，后两条均应明确计失。
	deliverObservationEvent(t, rig, 90, `{"future":1}`)
	deliverObservationEvent(t, rig, 91, `{"future":2}`)
	waitObservationSignal(t, inst.Fault(), "溢出 Fault")
	assertObservationOpen(t, inst.Done(), "仅故障时的 Done")
	if !errors.Is(inst.Err(), ErrEventOverflow) {
		t.Fatalf("故障期间 Err = %v, want ErrEventOverflow", inst.Err())
	}
	if err := inst.Start(); !errors.Is(err, ErrEventOverflow) {
		t.Fatalf("故障后的新请求 = %v, want ErrEventOverflow", err)
	}
	if _, err := waitOperationGuarded(t, pending, context.Background()); !errors.Is(err, ErrEventOverflow) {
		t.Fatalf("未完成 Operation = %v, want ErrEventOverflow", err)
	}
	closed := make(chan error, 1)
	go func() { closed <- inst.Close() }()
	waitObservationSignal(t, inst.Done(), "停读 Events 时的 Done")
	final := inst.Err()
	if !errors.Is(final, ErrEventOverflow) {
		t.Fatalf("最终 Err = %v, want ErrEventOverflow", final)
	}
	diag := inst.Diagnostics()
	if diag.LostEvents != 2 || diag.FirstFault != final || diag.StopDiagnostic != nil {
		t.Fatalf("最终 Diagnostics = %+v, want 丢失 2 条并保留首因", diag)
	}
	if err := waitErr(t, closed, "Instance.Close"); err != final {
		t.Fatalf("Close = %v, want 与 Err 同一终态 %v", err, final)
	}
	after, err := waitOperationGuarded(t, completed, context.Background())
	if err != nil || after.ID != before.ID || after.OK != before.OK || !bytes.Equal(after.Details, before.Details) {
		t.Fatalf("后续溢出改写了完成结果：before=%+v after=%+v err=%v", before, after, err)
	}
	_ = mustReadEvent(t, inst.Events())
	if _, ok := <-inst.Events(); ok {
		t.Fatal("缓冲排空后 Events 应已关闭")
	}
	if later := inst.Diagnostics(); later.LostEvents != diag.LostEvents || later.FirstFault != diag.FirstFault || inst.Err() != final {
		t.Fatalf("Done 后诊断/错误发生变化：%+v", later)
	}
}

func TestInstanceObservationStopDiagnosticBeforeDestroyReturns(t *testing.T) {
	for _, faultFirst := range []bool{false, true} {
		name := "仅停止拒绝"
		if faultFirst {
			name = "首因加停止拒绝"
		}
		t.Run(name, func(t *testing.T) {
			inst, rig := newSyncRig(t, 0x0B5300)
			var first error
			if faultFirst {
				deliverObservationEvent(t, rig, 92, `invalid-json`)
				waitObservationSignal(t, inst.Fault(), "协议 Fault")
				first = inst.Err()
				if !errors.Is(first, ErrProtocol) {
					t.Fatalf("首因 = %v, want ErrProtocol", first)
				}
			}
			rig.closer.stopRet = 0
			destroying := make(chan struct{})
			release := make(chan struct{})
			var releaseOnce sync.Once
			defer releaseOnce.Do(func() { close(release) })
			rig.closer.onDestroy = func(uintptr) {
				close(destroying)
				<-release
			}
			const closers = 8
			results := make(chan error, closers)
			for range closers {
				go func() { results <- inst.Close() }()
			}
			waitObservationSignal(t, destroying, "进入 Destroy")
			assertObservationOpen(t, inst.Done(), "Destroy 尚未返回的 Done")
			diag := inst.Diagnostics()
			if !errors.Is(diag.StopDiagnostic, ErrNativeRejected) || diag.FirstFault != first {
				t.Fatalf("Destroy 返回前 Diagnostics = %+v, want 已保存停止诊断与首因", diag)
			}
			if !errors.Is(inst.Err(), ErrNativeRejected) || (first != nil && !errors.Is(inst.Err(), first)) {
				t.Fatalf("Destroy 返回前 Err = %v, want 可识别停止拒绝和首因", inst.Err())
			}
			if !faultFirst {
				assertObservationOpen(t, inst.Fault(), "仅停止拒绝的 Fault")
			}
			releaseOnce.Do(func() { close(release) })
			waitObservationSignal(t, inst.Done(), "Done")
			final := inst.Err()
			for range closers {
				if err := waitErr(t, results, "并发 Instance.Close"); err != final {
					t.Fatalf("并发 Close = %v, want 同一最终错误 %v", err, final)
				}
			}
			if err := inst.Close(); err != final || inst.Err() != final {
				t.Fatalf("重复关闭改变最终错误：Close=%v Err=%v", err, inst.Err())
			}
			later := inst.Diagnostics()
			if later.FirstFault != first || later.StopDiagnostic != diag.StopDiagnostic {
				t.Fatalf("Done 后诊断改变：before=%+v after=%+v", diag, later)
			}
			if !faultFirst {
				assertObservationOpen(t, inst.Fault(), "仅停止拒绝关闭后的 Fault")
			}
			assertSingleStopDestroy(t, rig.closer, 0x0B5300)
		})
	}
}

func TestInstanceObservationDiagnosticPrefixOwnership(t *testing.T) {
	inst, rig := newSyncRig(t, 0x0B5400)
	raw := strings.Repeat("invalid-json-", 600)
	deliverObservationEvent(t, rig, 93, raw)
	waitObservationSignal(t, inst.Fault(), "协议 Fault")
	first := inst.Err()
	diag := inst.Diagnostics()
	if !errors.Is(first, ErrProtocol) || diag.FirstFault != first {
		t.Fatalf("协议首因未公开：Err=%v Diagnostics=%+v", first, diag)
	}
	if len(diag.RawPrefix) != 4096 || string(diag.RawPrefix) != raw[:4096] {
		t.Fatalf("首因前缀长度 = %d, want 4096 且保留原文", len(diag.RawPrefix))
	}
	ev := mustReadEvent(t, inst.Events())
	if string(ev.JSON) != raw {
		t.Fatal("非法 JSON 事件应保留完整原文供诊断")
	}
	diag.RawPrefix[0] = '!'
	ev.JSON[1] = '!'
	deliverObservationEvent(t, rig, 94, `another-invalid-json`)
	_ = mustReadEvent(t, inst.Events())
	if later := inst.Diagnostics(); later.FirstFault != first || string(later.RawPrefix) != raw[:4096] {
		t.Fatal("修改公开副本或后续故障改写了内部首因前缀")
	}
	closed := make(chan error, 1)
	go func() { closed <- inst.Close() }()
	waitObservationSignal(t, inst.Done(), "Done")
	if err := waitErr(t, closed, "Instance.Close"); err != first || inst.Err() != first {
		t.Fatalf("关闭改变首因：Close=%v Err=%v", err, inst.Err())
	}
	if later := inst.Diagnostics(); later.LostEvents != 0 || string(later.RawPrefix) != raw[:4096] {
		t.Fatalf("关闭后首因前缀/丢失计数异常：%+v", later)
	}
}
