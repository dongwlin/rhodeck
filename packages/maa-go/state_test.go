// 本文件是实例共享状态与调用准入原语的受控用例(MAA-16):白盒同包
// 验证绑定设计 §7、§8 的并发契约——closing/faulted/closed 拒绝新业
// 务调用而已准入 lease 照常释放、raw 门禁前排队的调用已计入在途计数
// 且关闭等待不漏掉它们、状态锁只用于检查与发布(raw 段与等待清零期
// 间不持锁)、并发关闭申请恰一所有者并共享同一 closeDone 与最终错
// 误、状态路径只保留一个受门控的 handle,以及 sticky 首因、非法迁
// 移 panic 与 callback 准入门的基础语义。全部用例以受控屏障
// (channel/WaitGroup/原子变量)确定性同步,不加载任何原生库、不触
// 达 loader/Runtime,CGO_ENABLED=0 可跑。

package maa

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

const (
	// hangGuard 是防挂死兜底上限:仅用于「操作必须完成」的正向断言
	// 的等待上限,超时即失败并报告疑似死锁。同步本身一律经
	// channel/WaitGroup/原子屏障完成,不依赖这段时间。
	hangGuard = 5 * time.Second

	// notWithin 是「必须不返回」负面断言的观察窗:证明阻塞语义天然
	// 需要一段有界时间观察,窗口内返回即实现违约。观察窗不是同步
	// 手段,窗口耗尽是预期路径。
	notWithin = 100 * time.Millisecond
)

// assertPanics 断言 f 按契约 panic:state.go 把非法迁移与状态机不
// 变量破坏声明为编程错误 panic(对照 runtime.go lifecycleGate.
// publish 的先例),不允许静默继续污染状态。
func assertPanics(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s 必须按契约 panic", what)
		}
	}()
	f()
}

// inFlightCalls 在状态锁内读取已准入、未释放的业务调用计数(白盒
// 诊断读取,不绕过锁)。
func inFlightCalls(s *instanceState) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// stateHandle 在状态锁内读取状态内唯一 handle 字段(白盒诊断读取)。
func stateHandle(s *instanceState) uintptr {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.handle
}

// mustAdmit 准入一次业务调用并要求成功。
func mustAdmit(t *testing.T, s *instanceState, what string) callLease {
	t.Helper()
	l, err := s.admitCall(what)
	if err != nil {
		t.Fatalf("准入 %q 不得失败: %v", what, err)
	}
	return l
}

// mustElectClose 要求关闭选主胜出(首个申请恰成为唯一所有者)。
func mustElectClose(t *testing.T, s *instanceState) {
	t.Helper()
	if !s.electClose() {
		t.Fatal("首个关闭申请必须胜出成为所有者")
	}
}

// finishClose 以所有者身份完成关闭发布序列:关门、发布最终错误、
// 发布终态。供已 electClose 胜出(或用例代行所有者职责)的状态收尾。
func finishClose(t *testing.T, s *instanceState, finalErr error) {
	t.Helper()
	s.closeCallbackGate()
	s.publishFinalError(finalErr)
	s.publishClosed()
}

// newClosedState 构造一个已完整走完正常关闭的状态(ready → closing
// → closed,正常关闭最终错误为 nil)。
func newClosedState(t *testing.T, handle uintptr) *instanceState {
	t.Helper()
	s := newInstanceState(handle, 128)
	s.publishReady()
	mustElectClose(t, s)
	finishClose(t, s, nil)
	return s
}

// releaseAndAssertDrained 释放在途 lease 并确认:release 在任何阶段
// 都正常,释放后已准入调用归零、关闭等待收敛(§7.1:已准入调用在
// 任何阶段都必须能正常退出,否则关闭等待无法收敛)。
func releaseAndAssertDrained(t *testing.T, s *instanceState, l callLease) {
	t.Helper()
	drained := make(chan struct{})
	go func() {
		s.waitCallsDrained()
		close(drained)
	}()
	l.release()
	select {
	case <-drained:
	case <-time.After(hangGuard):
		t.Fatal("lease 释放后 waitCallsDrained 未收敛(防挂死上限触发)")
	}
	if n := inFlightCalls(s); n != 0 {
		t.Fatalf("释放后在途调用计数 = %d, want 0", n)
	}
}

// TestInstanceStateRejectsCallsByPhase 钉住验收条件「closing、
// faulted、closed 状态拒绝新业务调用,已准入调用仍可正常释放
// lease」:各阶段逐一断言 admitCall 的错误类别(closing 包装
// native.ErrClosing,closed 包装 native.ErrClosed,faulted 沿错误链
// 可达首个故障的底层哨兵),并用先行准入的 lease 证明已准入调用在
// 这些阶段照常释放、计数归零。
func TestInstanceStateRejectsCallsByPhase(t *testing.T) {
	t.Run("closing 拒绝并包装 ErrClosing", func(t *testing.T) {
		s := newInstanceState(0x1001, 128)
		s.publishReady()
		inflight := mustAdmit(t, s, "先行准入")
		mustElectClose(t, s)

		_, err := s.admitCall("closing 中的新调用")
		if err == nil {
			t.Fatal("closing 必须拒绝新业务调用")
		}
		if !errors.Is(err, native.ErrClosing) {
			t.Errorf("errors.Is(err, native.ErrClosing) = false; err = %v", err)
		}
		// 逐一排除相邻哨兵:类别不得与 closed/协议破坏混淆。
		if errors.Is(err, native.ErrClosed) {
			t.Errorf("closing 拒绝不得误标为 ErrClosed: %v", err)
		}
		if errors.Is(err, native.ErrProtocol) {
			t.Errorf("closing 拒绝不得误标为 ErrProtocol: %v", err)
		}
		if !strings.Contains(err.Error(), "closing 中的新调用") {
			t.Errorf("拒绝错误应携带 what 上下文文本:\n%s", err.Error())
		}

		releaseAndAssertDrained(t, s, inflight)
	})

	t.Run("faulted 拒绝并沿错误链到达首因", func(t *testing.T) {
		s := newInstanceState(0x1002, 128)
		s.publishReady()
		inflight := mustAdmit(t, s, "先行准入")
		cause := fmt.Errorf("事件队列溢出被包装: %w", native.ErrEventOverflow)
		s.recordFault(cause)

		_, err := s.admitCall("faulted 中的新调用")
		if err == nil {
			t.Fatal("faulted 必须拒绝新业务调用")
		}
		// errors.Is 沿 %w 链可达真正故障的底层哨兵。
		if !errors.Is(err, native.ErrEventOverflow) {
			t.Errorf("errors.Is(err, native.ErrEventOverflow) = false; err = %v", err)
		}
		// faulted 拒绝与实例首因共享同一错误身份。
		if !errors.Is(err, cause) {
			t.Errorf("errors.Is(err, cause) = false; err = %v", err)
		}
		if errors.Is(err, native.ErrClosing) || errors.Is(err, native.ErrClosed) {
			t.Errorf("faulted 拒绝不得误标为 closing/closed: %v", err)
		}

		releaseAndAssertDrained(t, s, inflight)
	})

	t.Run("closed 拒绝并包装 ErrClosed", func(t *testing.T) {
		s := newInstanceState(0x1003, 128)
		s.publishReady()
		// 先行准入的 lease 在整个关闭过程中持有,直到 closed 之后
		// 才释放:钉住「已准入 lease 在 closed 阶段仍可正常释放」。
		inflight := mustAdmit(t, s, "先行准入")
		mustElectClose(t, s)
		finishClose(t, s, nil)

		_, err := s.admitCall("closed 中的新调用")
		if err == nil {
			t.Fatal("closed 必须拒绝新业务调用")
		}
		if !errors.Is(err, native.ErrClosed) {
			t.Errorf("errors.Is(err, native.ErrClosed) = false; err = %v", err)
		}
		if errors.Is(err, native.ErrClosing) {
			t.Errorf("closed 拒绝不得误标为 ErrClosing: %v", err)
		}
		// 正常关闭(无故障)的最终错误为 nil。
		if got := s.waitClose(); got != nil {
			t.Errorf("正常关闭的 waitClose() = %v, want nil", got)
		}

		releaseAndAssertDrained(t, s, inflight)
	})
}

// TestCloseWaitsForCallsQueuedBeforeRawGate 钉住验收条件「在 raw 门
// 禁前排队的调用已计入在途数量,关闭等待不会漏掉它」:调用 A 先准入
// 并占据 raw 段,调用 B 已准入(持有 lease、计入在途)但在门禁前排
// 队——A 全程占据门禁,B 必然尚未进入 raw 段。此时 electClose +
// waitCallsDrained 必须阻塞;在途计数含排队者;A、B 依次完成并释放
// 后等待才收敛。
func TestCloseWaitsForCallsQueuedBeforeRawGate(t *testing.T) {
	s := newInstanceState(0x2A00, 128)
	s.publishReady()

	// A:已准入并进入 raw 段(占据门禁)。
	leaseA := mustAdmit(t, s, "调用 A")
	s.raw.enter()

	// B:已准入、将在门禁前排队。A 全程未退出,B 必然还在门禁前。
	leaseB := mustAdmit(t, s, "调用 B")
	if n := inFlightCalls(s); n != 2 {
		t.Fatalf("两笔调用准入后在途计数 = %d, want 2", n)
	}
	bInRaw := make(chan struct{})
	bFinish := make(chan struct{})
	go func() {
		s.raw.enter() // 在 A 退出前一直排队
		close(bInRaw)
		<-bFinish
		s.raw.exit()
		leaseB.release()
	}()

	mustElectClose(t, s)

	drained := make(chan struct{})
	go func() {
		s.waitCallsDrained()
		close(drained)
	}()

	// 负面断言一:A、B 均未释放,关闭等待必须仍阻塞。
	select {
	case <-drained:
		t.Fatal("在途调用未释放时 waitCallsDrained 提前返回:漏掉在途调用")
	case <-time.After(notWithin):
	}

	// 等待期间在途计数必须包含尚在门禁前排队的 B。
	if n := inFlightCalls(s); n != 2 {
		t.Fatalf("关闭等待期间在途计数 = %d, want 2(含 raw 门禁前的排队者)", n)
	}

	// A 完成 raw 段并释放 lease。
	s.raw.exit()
	leaseA.release()

	// 负面断言二:B 仍在途(排队转准入段、未释放),等待必须继续。
	select {
	case <-drained:
		t.Fatal("调用 B 尚未释放时 waitCallsDrained 提前返回:漏掉排队者")
	case <-time.After(notWithin):
	}

	// B 获得门禁、完成 raw 段并释放 lease。
	<-bInRaw
	close(bFinish)

	select {
	case <-drained:
	case <-time.After(hangGuard):
		t.Fatal("全部 lease 释放后 waitCallsDrained 仍未收敛(防挂死上限触发)")
	}
	if n := inFlightCalls(s); n != 0 {
		t.Fatalf("收敛后在途计数 = %d, want 0", n)
	}
	if got := s.currentPhase(); got != phaseClosing {
		t.Fatalf("关闭等待收敛后阶段 = %d, want closing(终态发布归后续流程)", got)
	}
}

// TestStateLockNotHeldAcrossRawSection 钉住验收条件「状态锁只用于
// 检查与发布;可控 raw 调用期间不持状态锁」:在受控 raw 段(持
// lease、持门禁)内部与关闭等待阻塞期间,另一 goroutine 都能在限时
// 内完成需要状态锁的操作;实现若在 raw 段或等待清零期间持有状态锁,
// 本用例必然超时失败。限时只作防挂死兜底,信号经 channel 传递。
func TestStateLockNotHeldAcrossRawSection(t *testing.T) {
	s := newInstanceState(0x5EED, 128)
	s.publishReady()
	leaseA := mustAdmit(t, s, "raw 段调用")
	s.raw.enter()

	// raw 段内:另一 goroutine 的状态锁操作必须能完成。
	phaseCh := make(chan instancePhase, 1)
	go func() { phaseCh <- s.currentPhase() }()
	select {
	case got := <-phaseCh:
		if got != phaseReady {
			t.Fatalf("raw 段内 currentPhase() = %d, want ready", got)
		}
	case <-time.After(hangGuard):
		t.Fatal("raw 段内另一 goroutine 的状态锁操作未在限时内完成:实现疑似在 raw 调用期间持有状态锁")
	}

	// 进入关闭:等待清零阻塞在 A 未释放上。
	mustElectClose(t, s)
	drained := make(chan struct{})
	go func() {
		s.waitCallsDrained()
		close(drained)
	}()

	// 等待清零期间(§8 第 2 步绝不持状态锁):拒绝新调用的
	// admitCall 与阶段读取都必须能完成。
	probe := make(chan error, 1)
	go func() {
		_, err := s.admitCall("等待清零期间的探测")
		probe <- err
	}()
	select {
	case err := <-probe:
		if !errors.Is(err, native.ErrClosing) {
			t.Fatalf("closing 中的探测 = %v, want errors.Is native.ErrClosing", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("等待清零期间另一 goroutine 的 admitCall 未在限时内完成:实现疑似在等待在途清零时持有状态锁")
	}
	phaseCh2 := make(chan instancePhase, 1)
	go func() { phaseCh2 <- s.currentPhase() }()
	select {
	case got := <-phaseCh2:
		if got != phaseClosing {
			t.Fatalf("等待清零期间 currentPhase() = %d, want closing", got)
		}
	case <-time.After(hangGuard):
		t.Fatal("等待清零期间另一 goroutine 的 currentPhase 未在限时内完成:实现疑似在等待在途清零时持有状态锁")
	}

	// A 完成 raw 段并释放,关闭等待收敛。
	s.raw.exit()
	leaseA.release()
	select {
	case <-drained:
	case <-time.After(hangGuard):
		t.Fatal("lease 释放后 waitCallsDrained 未收敛(防挂死上限触发)")
	}
}

// TestConcurrentElectCloseSingleOwner 钉住验收条件「多个关闭申请只
// 选出一个所有者,并共享同一 closeDone 和最终错误存储」:N 个
// goroutine 并发 electClose 恰一胜出;所有者发布终态后,全部失败者
// 经 waitClose 取得同一份最终错误(同一错误值、同一底层哨兵),并共
// 享同一已关闭的 closeDone;closed 后的幂等申请仍不为所有者。
func TestConcurrentElectCloseSingleOwner(t *testing.T) {
	t.Run("ready 并发申请恰一所有者共享终态", func(t *testing.T) {
		s := newInstanceState(0x2001, 128)
		s.publishReady()
		closeCh := s.done()

		const n = 8
		results := make([]bool, n)
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				results[i] = s.electClose()
				if results[i] {
					wins.Add(1)
				}
			}(i)
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("选主胜出者 = %d, want 恰 1", wins.Load())
		}
		if got := s.currentPhase(); got != phaseClosing {
			t.Fatalf("选主后阶段 = %d, want closing", got)
		}

		finalErr := fmt.Errorf("停止诊断: %w", native.ErrProtocol)
		// 全部失败者先行等待同一 closeDone;随后代行所有者发布终态。
		var waitWG sync.WaitGroup
		loserErrs := make([]error, 0, n)
		var mu sync.Mutex
		for i := range results {
			if results[i] {
				continue
			}
			waitWG.Add(1)
			go func() {
				defer waitWG.Done()
				err := s.waitClose()
				mu.Lock()
				loserErrs = append(loserErrs, err)
				mu.Unlock()
			}()
		}
		finishClose(t, s, finalErr)
		waitWG.Wait()

		if len(loserErrs) != n-1 {
			t.Fatalf("失败者数 = %d, want %d", len(loserErrs), n-1)
		}
		for i, err := range loserErrs {
			// 同一最终错误存储:失败者拿到的就是所有者发布的同一错误值。
			if err != finalErr {
				t.Errorf("失败者 %d 的最终错误 = %v, want 与所有者同一存储的 %v", i, err, finalErr)
			}
			if !errors.Is(err, native.ErrProtocol) {
				t.Errorf("失败者 %d: errors.Is(err, native.ErrProtocol) = false; err = %v", i, err)
			}
		}
		select {
		case <-closeCh:
		default:
			t.Fatal("所有者发布终态后 closeDone 未关闭")
		}
		if s.done() != closeCh {
			t.Fatal("done() 返回了不同的通道:closeDone 不共享")
		}
		if got := s.currentPhase(); got != phaseClosed {
			t.Fatalf("发布后阶段 = %d, want closed", got)
		}
		// closed 后的幂等申请仍不成为所有者。
		if s.electClose() {
			t.Error("closed 后重复申请不得成为所有者")
		}
	})

	t.Run("faulted 并发申请恰一所有者共享首因终态", func(t *testing.T) {
		s := newInstanceState(0x2002, 128)
		s.publishReady()
		cause := fmt.Errorf("事件溢出: %w", native.ErrEventOverflow)
		s.recordFault(cause)

		const n = 8
		var wins atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if s.electClose() {
					wins.Add(1)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("faulted 阶段选主胜出者 = %d, want 恰 1", wins.Load())
		}

		var waitWG sync.WaitGroup
		loserErrs := make([]error, 0, n)
		var mu sync.Mutex
		for i := 0; i < n-1; i++ {
			waitWG.Add(1)
			go func() {
				defer waitWG.Done()
				err := s.waitClose()
				mu.Lock()
				loserErrs = append(loserErrs, err)
				mu.Unlock()
			}()
		}
		finishClose(t, s, cause)
		waitWG.Wait()

		for i, err := range loserErrs {
			if err != cause {
				t.Errorf("失败者 %d 的最终错误 = %v, want 首因 %v", i, err, cause)
			}
			if !errors.Is(err, native.ErrEventOverflow) {
				t.Errorf("失败者 %d: errors.Is(err, native.ErrEventOverflow) = false; err = %v", i, err)
			}
		}
	})
}

// TestPublishFinalErrorFirstWriteWins 钉住最终错误的一次性语义:
// finalErr 一经设置不再被后续发布覆盖,观察者经 waitClose 取得首次
// 发布的那一份。
func TestPublishFinalErrorFirstWriteWins(t *testing.T) {
	s := newInstanceState(0x2100, 128)
	s.publishReady()
	mustElectClose(t, s)

	first := fmt.Errorf("首份最终错误: %w", native.ErrProtocol)
	s.publishFinalError(first)
	second := fmt.Errorf("后到的诊断: %w", native.ErrClosing)
	s.publishFinalError(second)

	s.closeCallbackGate()
	s.publishClosed()
	got := s.waitClose()
	if got != first {
		t.Fatalf("waitClose() = %v, want 首次发布的 %v(不得被覆盖)", got, first)
	}
	if errors.Is(got, native.ErrClosing) {
		t.Errorf("最终错误不得混入后到诊断: %v", got)
	}
}

// TestInstanceStateSingleGatedHandle 钉住验收条件「状态路径只保留
// 一个受门控的 handle,不向外暴露可绕过生命周期的引用」:业务路径只
// 能凭已准入 lease 读取 handle;closeHandle 仅在调用清零后可取,
// CreateEx=0 路径(handle=0)无关闭句柄;状态内唯一 handle 字段在
// 构造→发布→关闭全程值一致、不被旁路修改。
func TestInstanceStateSingleGatedHandle(t *testing.T) {
	t.Run("handle 全程唯一且凭 lease 读取", func(t *testing.T) {
		const h = uintptr(0x3001)
		s := newInstanceState(h, 128)
		if got := stateHandle(s); got != h {
			t.Fatalf("构造后 handle = %#x, want %#x", got, h)
		}
		s.publishReady()
		if got := stateHandle(s); got != h {
			t.Fatalf("发布 ready 后 handle = %#x, want %#x", got, h)
		}
		l := mustAdmit(t, s, "handle 读取")
		if got := l.handle(); got != h {
			t.Fatalf("lease.handle() = %#x, want %#x", got, h)
		}
		mustElectClose(t, s)
		if got := stateHandle(s); got != h {
			t.Fatalf("closing 阶段 handle = %#x, want %#x", got, h)
		}
		finishClose(t, s, nil)
		if got := stateHandle(s); got != h {
			t.Fatalf("closed 后 handle = %#x, want %#x(不得被旁路改写)", got, h)
		}
	})

	t.Run("closeHandle 仅在调用清零后可取", func(t *testing.T) {
		const h = uintptr(0x3002)
		s := newInstanceState(h, 128)
		s.publishReady()
		l := mustAdmit(t, s, "占用在途")
		mustElectClose(t, s)

		// 在途计数非零时读取关闭句柄是内部不变量破坏。
		assertPanics(t, "在途未清零时读取 closeHandle", func() { s.closeHandle() })

		releaseAndAssertDrained(t, s, l)
		got, ok := s.closeHandle()
		if !ok {
			t.Fatalf("清零后 closeHandle 的 ok = false, want true(handle=%#x)", h)
		}
		if got != h {
			t.Fatalf("closeHandle() = %#x, want %#x", got, h)
		}
	})

	t.Run("CreateEx=0 路径 handle=0 无关闭句柄", func(t *testing.T) {
		s := newInstanceState(0, 128)
		s.publishReady()
		l := mustAdmit(t, s, "零句柄实例的调用")
		// 0 无效、不解引用:业务路径读到 0 即内部不变量破坏。
		assertPanics(t, "零句柄实例的 lease.handle()", func() { l.handle() })
		l.release()

		mustElectClose(t, s)
		s.waitCallsDrained()
		finishClose(t, s, nil)
		got, ok := s.closeHandle()
		if ok {
			t.Fatalf("CreateEx=0 路径 closeHandle 的 ok = true, want false")
		}
		if got != 0 {
			t.Fatalf("CreateEx=0 路径 closeHandle() = %#x, want 0", got)
		}
	})
}

// TestRecordFaultStickySemantics 钉住首次故障的 sticky 语义(§7.1):
// 两次故障只保留首个;closing 期间 recordFault 只记因不改阶段;
// closed 后再有故障源是内部不变量破坏;无因故障不被接受。
func TestRecordFaultStickySemantics(t *testing.T) {
	t.Run("两次故障保留首个", func(t *testing.T) {
		s := newInstanceState(0x4001, 128)
		s.publishReady()
		first := fmt.Errorf("首个故障: %w", native.ErrProtocol)
		second := fmt.Errorf("第二个故障: %w", native.ErrEventOverflow)
		s.recordFault(first)
		s.recordFault(second)

		if got := s.currentFault(); !errors.Is(got, first) {
			t.Fatalf("currentFault() = %v, want 首因 %v", got, first)
		}
		if got := s.currentFault(); errors.Is(got, native.ErrEventOverflow) {
			t.Fatalf("首因被后到故障覆盖: %v", got)
		}
		_, err := s.admitCall("faulted 后的新调用")
		if !errors.Is(err, first) || !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("拒绝错误应沿链到达首因: err = %v", err)
		}
		if errors.Is(err, native.ErrEventOverflow) {
			t.Fatalf("拒绝错误混入了后到故障: %v", err)
		}
	})

	t.Run("closing 期间只记因不改阶段", func(t *testing.T) {
		s := newInstanceState(0x4002, 128)
		s.publishReady()
		inflight := mustAdmit(t, s, "先行准入")
		mustElectClose(t, s)

		cause := fmt.Errorf("关闭期间故障: %w", native.ErrProtocol)
		s.recordFault(cause)
		if got := s.currentPhase(); got != phaseClosing {
			t.Fatalf("closing 期间 recordFault 后阶段 = %d, want closing(只记因不改阶段)", got)
		}
		if got := s.currentFault(); !errors.Is(got, cause) {
			t.Fatalf("currentFault() = %v, want 新记首因 %v", got, cause)
		}
		// 阶段未变:新调用仍按 closing 拒绝。
		_, err := s.admitCall("closing 故障后的新调用")
		if !errors.Is(err, native.ErrClosing) {
			t.Fatalf("errors.Is(err, native.ErrClosing) = false; err = %v", err)
		}
		// 所有者随后发布的最终错误携带首因。
		finishClose(t, s, cause)
		if got := s.waitClose(); !errors.Is(got, cause) {
			t.Fatalf("waitClose() = %v, want 含首因 %v", got, cause)
		}
		// 已准入调用在关闭全过程后照常释放。
		releaseAndAssertDrained(t, s, inflight)
	})

	t.Run("closed 后记录故障 panic", func(t *testing.T) {
		s := newClosedState(t, 0x4003)
		assertPanics(t, "closed 后 recordFault", func() { s.recordFault(native.ErrProtocol) })
	})

	t.Run("无因故障 panic", func(t *testing.T) {
		s := newInstanceState(0x4004, 128)
		s.publishReady()
		assertPanics(t, "cause 为 nil 的 recordFault", func() { s.recordFault(nil) })
	})
}

// TestInstanceStateIllegalTransitionsPanic 逐条钉住 state.go 声明的
// 非法迁移与不变量守卫 panic:违反阶段机与前置条件的调用按编程错误
// panic,不静默继续污染状态。白盒子用例直接注入被破坏的内部阶段以
// 到达守卫分支。
func TestInstanceStateIllegalTransitionsPanic(t *testing.T) {
	t.Run("initializing admitCall panic", func(t *testing.T) {
		s := newInstanceState(0x5001, 128)
		assertPanics(t, "initializing 阶段 admitCall", func() { _, _ = s.admitCall("未发布实例") })
	})

	t.Run("initializing electClose panic", func(t *testing.T) {
		s := newInstanceState(0x5002, 128)
		assertPanics(t, "initializing 阶段 electClose", func() { _ = s.electClose() })
	})

	t.Run("publishReady 重复发布 panic", func(t *testing.T) {
		s := newInstanceState(0x5003, 128)
		s.publishReady()
		assertPanics(t, "ready 后 publishReady", func() { s.publishReady() })
	})

	t.Run("closing 后 publishReady panic", func(t *testing.T) {
		s := newInstanceState(0x5004, 128)
		s.publishReady()
		mustElectClose(t, s)
		assertPanics(t, "closing 后 publishReady", func() { s.publishReady() })
	})

	t.Run("ready 阶段发布最终错误 panic", func(t *testing.T) {
		s := newInstanceState(0x5005, 128)
		s.publishReady()
		assertPanics(t, "ready 阶段 publishFinalError", func() { s.publishFinalError(nil) })
	})

	t.Run("closed 后发布最终错误 panic", func(t *testing.T) {
		s := newClosedState(t, 0x5006)
		assertPanics(t, "closed 后 publishFinalError", func() { s.publishFinalError(nil) })
	})

	t.Run("closed 后重复发布终态 panic", func(t *testing.T) {
		s := newClosedState(t, 0x5007)
		assertPanics(t, "closed 后 publishClosed", func() { s.publishClosed() })
	})

	t.Run("faulted 无首因 admitCall panic", func(t *testing.T) {
		s := newInstanceState(0x5008, 128)
		s.publishReady()
		// 白盒:直接置位阶段,模拟不变量已被破坏的内部状态。
		s.mu.Lock()
		s.phase = phaseFaulted
		s.mu.Unlock()
		assertPanics(t, "faulted 无首因的 admitCall", func() { _, _ = s.admitCall("不变量破坏") })
	})

	t.Run("未知阶段 recordFault panic", func(t *testing.T) {
		s := newInstanceState(0x5009, 128)
		s.publishReady()
		s.mu.Lock()
		s.phase = instancePhase(200)
		s.mu.Unlock()
		assertPanics(t, "未知阶段 recordFault", func() { s.recordFault(native.ErrProtocol) })
	})
}

// TestCallbackGateBasics 钉住 callback 准入门的基础语义(§4、§7;
// 接线归 MAA-18):门开时准入、closing 阶段仍开放;关门后一律拒绝;
// 关门至多一次;waitCallbacksDrained 等已准入 callback 退出;
// publishClosed 前必须已关门且已准入 callback 清空。
func TestCallbackGateBasics(t *testing.T) {
	t.Run("门开准入且 closing 仍开放,关门后拒绝", func(t *testing.T) {
		s := newInstanceState(0x6001, 128)
		s.publishReady()
		l1, ok := s.admitCallback()
		if !ok {
			t.Fatal("门开时 admitCallback 必须成功")
		}
		mustElectClose(t, s)
		l2, ok := s.admitCallback()
		if !ok {
			t.Fatal("closing 阶段门仍开放,admitCallback 必须成功")
		}
		l1.release()
		l2.release()

		s.closeCallbackGate()
		if _, ok := s.admitCallback(); ok {
			t.Fatal("关门后 admitCallback 必须失败")
		}
		s.waitCallbacksDrained()
		// 已显式关门,此处只做所有者的错误与终态发布。
		s.publishFinalError(nil)
		s.publishClosed()
	})

	t.Run("重复关门 panic", func(t *testing.T) {
		s := newInstanceState(0x6002, 128)
		s.publishReady()
		s.closeCallbackGate()
		assertPanics(t, "重复 closeCallbackGate", func() { s.closeCallbackGate() })
	})

	t.Run("waitCallbacksDrained 等已准入 callback 退出", func(t *testing.T) {
		s := newInstanceState(0x6003, 128)
		s.publishReady()
		l, ok := s.admitCallback()
		if !ok {
			t.Fatal("门开时 admitCallback 必须成功")
		}
		drained := make(chan struct{})
		go func() {
			s.waitCallbacksDrained()
			close(drained)
		}()

		// 负面断言:callback 在途时不得返回。
		select {
		case <-drained:
			t.Fatal("callback 在途时 waitCallbacksDrained 提前返回")
		case <-time.After(notWithin):
		}

		l.release()
		select {
		case <-drained:
		case <-time.After(hangGuard):
			t.Fatal("callback 退出后 waitCallbacksDrained 未收敛(防挂死上限触发)")
		}
	})

	t.Run("publishClosed 前必须关门且清空", func(t *testing.T) {
		s := newInstanceState(0x6004, 128)
		s.publishReady()
		mustElectClose(t, s)

		// 门开着不得发布终态。
		assertPanics(t, "callback 门开着 publishClosed", func() { s.publishClosed() })
		// 门开着仍可准入;关门后仍有在途 callback 不得发布终态。
		l, ok := s.admitCallback()
		if !ok {
			t.Fatal("closing 阶段门仍开放,admitCallback 必须成功")
		}
		s.closeCallbackGate()
		assertPanics(t, "callback 在途 publishClosed", func() { s.publishClosed() })

		l.release()
		s.waitCallbacksDrained()
		s.publishFinalError(nil)
		s.publishClosed()
		if got := s.currentPhase(); got != phaseClosed {
			t.Fatalf("关门清空后 publishClosed 后阶段 = %d, want closed", got)
		}
	})
}
