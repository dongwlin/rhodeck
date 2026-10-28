// 本文件是实例共享状态与调用准入原语的受控用例:验证阶段迁移、lease
// 准入/释放、raw 门禁、关闭选主、sticky 首因与非法迁移 panic。

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
			t.Errorf("%s must panic per the contract", what)
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
		t.Fatalf("admitting %q must not fail: %v", what, err)
	}
	return l
}

// mustElectClose 要求关闭选主胜出(首个申请恰成为唯一所有者)。
func mustElectClose(t *testing.T, s *instanceState) {
	t.Helper()
	if !s.electClose() {
		t.Fatal("the first close request must win and become the owner")
	}
}

// finishClose 以所有者身份完成关闭发布序列:关门、发布最终错误、
// 发布终态。供已 electClose 胜出(或用例代行所有者职责)的状态收尾。
func finishClose(t *testing.T, s *instanceState, finalErr error) {
	t.Helper()
	s.closeCallbackGate()
	s.publishFinalError(finalErr)
	s.publishClosed()
	s.publishCloseDone()
}

// newClosedState 构造一个已完整走完正常关闭的状态(ready → closing
// → closed,正常关闭最终错误为 nil)。
func newClosedState(t *testing.T, handle uintptr) *instanceState {
	t.Helper()
	s := newInstanceState(handle, 128, 1_048_576)
	s.publishReady()
	mustElectClose(t, s)
	finishClose(t, s, nil)
	return s
}

// releaseAndAssertDrained 释放在途 lease 并确认:release 在任何阶段
// 都正常,释放后已准入调用归零、关闭等待收敛(已准入调用在
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
		t.Fatal("waitCallsDrained did not converge after the lease was released (hang-guard limit hit)")
	}
	if n := inFlightCalls(s); n != 0 {
		t.Fatalf("in-flight call count after release = %d, want 0", n)
	}
}

// TestInstanceStateRejectsCallsByPhase 验证「closing、
// faulted、closed 状态拒绝新业务调用,已准入调用仍可正常释放
// lease」:各阶段逐一断言 admitCall 的错误类别(closing 包装
// native.ErrClosing,closed 包装 native.ErrClosed,faulted 沿错误链
// 可达首个故障的底层哨兵),并用先行准入的 lease 证明已准入调用在
// 这些阶段照常释放、计数归零。
func TestInstanceStateRejectsCallsByPhase(t *testing.T) {
	t.Run("closing rejects and wraps ErrClosing", func(t *testing.T) {
		s := newInstanceState(0x1001, 128, 1_048_576)
		s.publishReady()
		inflight := mustAdmit(t, s, "admitted beforehand")
		mustElectClose(t, s)

		_, err := s.admitCall("new call during closing")
		if err == nil {
			t.Fatal("closing must reject new business calls")
		}
		if !errors.Is(err, native.ErrClosing) {
			t.Errorf("errors.Is(err, native.ErrClosing) = false; err = %v", err)
		}
		// 逐一排除相邻哨兵:类别不得与 closed/协议破坏混淆。
		if errors.Is(err, native.ErrClosed) {
			t.Errorf("the closing rejection must not be mislabelled as ErrClosed: %v", err)
		}
		if errors.Is(err, native.ErrProtocol) {
			t.Errorf("the closing rejection must not be mislabelled as ErrProtocol: %v", err)
		}
		if !strings.Contains(err.Error(), "new call during closing") {
			t.Errorf("the rejection error should carry the what context text:\n%s", err.Error())
		}

		releaseAndAssertDrained(t, s, inflight)
	})

	t.Run("faulted rejects and reaches the first cause along the error chain", func(t *testing.T) {
		s := newInstanceState(0x1002, 128, 1_048_576)
		s.publishReady()
		inflight := mustAdmit(t, s, "admitted beforehand")
		cause := fmt.Errorf("event queue overflow wrapped: %w", native.ErrEventOverflow)
		s.recordFault(cause)

		_, err := s.admitCall("new call during faulted")
		if err == nil {
			t.Fatal("faulted must reject new business calls")
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
			t.Errorf("the faulted rejection must not be mislabelled as closing/closed: %v", err)
		}

		releaseAndAssertDrained(t, s, inflight)
	})

	t.Run("closed rejects and wraps ErrClosed", func(t *testing.T) {
		s := newInstanceState(0x1003, 128, 1_048_576)
		s.publishReady()
		// 先行准入的 lease 在整个关闭过程中持有,直到 closed 之后
		// 才释放:钉住「已准入 lease 在 closed 阶段仍可正常释放」。
		inflight := mustAdmit(t, s, "admitted beforehand")
		mustElectClose(t, s)
		finishClose(t, s, nil)

		_, err := s.admitCall("new call during closed")
		if err == nil {
			t.Fatal("closed must reject new business calls")
		}
		if !errors.Is(err, native.ErrClosed) {
			t.Errorf("errors.Is(err, native.ErrClosed) = false; err = %v", err)
		}
		if errors.Is(err, native.ErrClosing) {
			t.Errorf("the closed rejection must not be mislabelled as ErrClosing: %v", err)
		}
		// 正常关闭(无故障)的最终错误为 nil。
		if got := s.waitClose(); got != nil {
			t.Errorf("waitClose() of a normal close = %v, want nil", got)
		}

		releaseAndAssertDrained(t, s, inflight)
	})
}

// TestCloseWaitsForCallsQueuedBeforeRawGate 验证「在 raw 门
// 禁前排队的调用已计入在途数量,关闭等待不会漏掉它」:调用 A 先准入
// 并占据 raw 段,调用 B 已准入(持有 lease、计入在途)但在门禁前排
// 队——A 全程占据门禁,B 必然尚未进入 raw 段。此时 electClose +
// waitCallsDrained 必须阻塞;在途计数含排队者;A、B 依次完成并释放
// 后等待才收敛。
func TestCloseWaitsForCallsQueuedBeforeRawGate(t *testing.T) {
	s := newInstanceState(0x2A00, 128, 1_048_576)
	s.publishReady()

	// A:已准入并进入 raw 段(占据门禁)。
	leaseA := mustAdmit(t, s, "call A")
	s.raw.enter()

	// B:已准入、将在门禁前排队。A 全程未退出,B 必然还在门禁前。
	leaseB := mustAdmit(t, s, "call B")
	if n := inFlightCalls(s); n != 2 {
		t.Fatalf("in-flight count after both calls admitted = %d, want 2", n)
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
		t.Fatal("waitCallsDrained returned early while in-flight calls were unreleased: it missed an in-flight call")
	case <-time.After(notWithin):
	}

	// 等待期间在途计数必须包含尚在门禁前排队的 B。
	if n := inFlightCalls(s); n != 2 {
		t.Fatalf("in-flight count during the close wait = %d, want 2 (including the caller queued before the raw gate)", n)
	}

	// A 完成 raw 段并释放 lease。
	s.raw.exit()
	leaseA.release()

	// 负面断言二:B 仍在途(排队转准入段、未释放),等待必须继续。
	select {
	case <-drained:
		t.Fatal("waitCallsDrained returned early while call B was unreleased: it missed the queued caller")
	case <-time.After(notWithin):
	}

	// B 获得门禁、完成 raw 段并释放 lease。
	<-bInRaw
	close(bFinish)

	select {
	case <-drained:
	case <-time.After(hangGuard):
		t.Fatal("waitCallsDrained still did not converge after all leases were released (hang-guard limit hit)")
	}
	if n := inFlightCalls(s); n != 0 {
		t.Fatalf("in-flight count after convergence = %d, want 0", n)
	}
	if got := s.currentPhase(); got != phaseClosing {
		t.Fatalf("phase after the close wait converged = %d, want closing (terminal state publication belongs to the later flow)", got)
	}
}

// TestStateLockNotHeldAcrossRawSection 验证「状态锁只用于
// 检查与发布;可控 raw 调用期间不持状态锁」:在受控 raw 段(持
// lease、持门禁)内部与关闭等待阻塞期间,另一 goroutine 都能在限时
// 内完成需要状态锁的操作;实现若在 raw 段或等待清零期间持有状态锁,
// 本用例必然超时失败。限时只作防挂死兜底,信号经 channel 传递。
func TestStateLockNotHeldAcrossRawSection(t *testing.T) {
	s := newInstanceState(0x5EED, 128, 1_048_576)
	s.publishReady()
	leaseA := mustAdmit(t, s, "raw-section call")
	s.raw.enter()

	// raw 段内:另一 goroutine 的状态锁操作必须能完成。
	phaseCh := make(chan instancePhase, 1)
	go func() { phaseCh <- s.currentPhase() }()
	select {
	case got := <-phaseCh:
		if got != phaseReady {
			t.Fatalf("currentPhase() inside the raw section = %d, want ready", got)
		}
	case <-time.After(hangGuard):
		t.Fatal("another goroutine's state lock operation inside the raw section did not complete in time: the implementation appears to hold the state lock across raw calls")
	}

	// 进入关闭:等待清零阻塞在 A 未释放上。
	mustElectClose(t, s)
	drained := make(chan struct{})
	go func() {
		s.waitCallsDrained()
		close(drained)
	}()

	// 等待清零期间(第 2 步绝不持状态锁):拒绝新调用的
	// admitCall 与阶段读取都必须能完成。
	probe := make(chan error, 1)
	go func() {
		_, err := s.admitCall("probe while waiting for the drain")
		probe <- err
	}()
	select {
	case err := <-probe:
		if !errors.Is(err, native.ErrClosing) {
			t.Fatalf("probe during closing = %v, want errors.Is native.ErrClosing", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("another goroutine's admitCall while waiting for the drain did not complete in time: the implementation appears to hold the state lock while waiting for in-flight calls to drain")
	}
	phaseCh2 := make(chan instancePhase, 1)
	go func() { phaseCh2 <- s.currentPhase() }()
	select {
	case got := <-phaseCh2:
		if got != phaseClosing {
			t.Fatalf("currentPhase() while waiting for the drain = %d, want closing", got)
		}
	case <-time.After(hangGuard):
		t.Fatal("another goroutine's currentPhase while waiting for the drain did not complete in time: the implementation appears to hold the state lock while waiting for in-flight calls to drain")
	}

	// A 完成 raw 段并释放,关闭等待收敛。
	s.raw.exit()
	leaseA.release()
	select {
	case <-drained:
	case <-time.After(hangGuard):
		t.Fatal("waitCallsDrained did not converge after the lease was released (hang-guard limit hit)")
	}
}

// TestConcurrentElectCloseSingleOwner 验证「多个关闭申请只
// 选出一个所有者,并共享同一 closeDone 和最终错误存储」:N 个
// goroutine 并发 electClose 恰一胜出;所有者发布终态后,全部失败者
// 经 waitClose 取得同一份最终错误(同一错误值、同一底层哨兵),并共
// 享同一已关闭的 closeDone;closed 后的幂等申请仍不为所有者。
func TestConcurrentElectCloseSingleOwner(t *testing.T) {
	t.Run("concurrent requests in ready elect exactly one owner sharing the terminal state", func(t *testing.T) {
		s := newInstanceState(0x2001, 128, 1_048_576)
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
			t.Fatalf("election winners = %d, want exactly 1", wins.Load())
		}
		if got := s.currentPhase(); got != phaseClosing {
			t.Fatalf("phase after the election = %d, want closing", got)
		}

		finalErr := fmt.Errorf("stop diagnostic: %w", native.ErrProtocol)
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
			t.Fatalf("loser count = %d, want %d", len(loserErrs), n-1)
		}
		for i, err := range loserErrs {
			// 同一最终错误存储:失败者拿到的就是所有者发布的同一错误值。
			if err != finalErr {
				t.Errorf("loser %d final error = %v, want the same stored %v as the owner", i, err, finalErr)
			}
			if !errors.Is(err, native.ErrProtocol) {
				t.Errorf("loser %d: errors.Is(err, native.ErrProtocol) = false; err = %v", i, err)
			}
		}
		select {
		case <-closeCh:
		default:
			t.Fatal("closeDone not closed after the owner published the terminal state")
		}
		if s.done() != closeCh {
			t.Fatal("done() returned a different channel: closeDone is not shared")
		}
		if got := s.currentPhase(); got != phaseClosed {
			t.Fatalf("phase after publication = %d, want closed", got)
		}
		// closed 后的幂等申请仍不成为所有者。
		if s.electClose() {
			t.Error("a repeated request after closed must not become the owner")
		}
	})

	t.Run("concurrent requests in faulted elect exactly one owner sharing the first-cause terminal state", func(t *testing.T) {
		s := newInstanceState(0x2002, 128, 1_048_576)
		s.publishReady()
		cause := fmt.Errorf("event overflow: %w", native.ErrEventOverflow)
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
			t.Fatalf("election winners in the faulted phase = %d, want exactly 1", wins.Load())
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
				t.Errorf("loser %d final error = %v, want first cause %v", i, err, cause)
			}
			if !errors.Is(err, native.ErrEventOverflow) {
				t.Errorf("loser %d: errors.Is(err, native.ErrEventOverflow) = false; err = %v", i, err)
			}
		}
	})
}

// TestPublishFinalErrorFirstWriteWins 钉住最终错误的一次性语义:
// finalErr 一经设置不再被后续发布覆盖,观察者经 waitClose 取得首次
// 发布的那一份。
func TestPublishFinalErrorFirstWriteWins(t *testing.T) {
	s := newInstanceState(0x2100, 128, 1_048_576)
	s.publishReady()
	mustElectClose(t, s)

	first := fmt.Errorf("first final error: %w", native.ErrProtocol)
	s.publishFinalError(first)
	second := fmt.Errorf("later diagnostic: %w", native.ErrClosing)
	s.publishFinalError(second)

	s.closeCallbackGate()
	s.publishClosed()
	s.publishCloseDone()
	got := s.waitClose()
	if got != first {
		t.Fatalf("waitClose() = %v, want the first published %v (must not be overwritten)", got, first)
	}
	if errors.Is(got, native.ErrClosing) {
		t.Errorf("the final error must not mix in the later diagnostic: %v", got)
	}
}

func TestCloseCompletionFollowsFinalState(t *testing.T) {
	s := newInstanceState(0x2101, 128, 1_048_576)
	s.publishReady()
	mustElectClose(t, s)
	s.closeCallbackGate()
	s.publishFinalError(nil)
	// 正常最终 nil 也必须保持首次发布,不因后续发布变成错误。
	s.publishFinalError(native.ErrProtocol)
	s.publishClosed()
	select {
	case <-s.done():
		t.Fatal("publishing the closed state must not notify Events closed early")
	default:
	}
	if s.currentError() != nil {
		t.Fatal("the normal final error was rewritten by a later publication")
	}
	s.publishCloseDone()
	if err := s.waitClose(); err != nil {
		t.Fatalf("waitClose = %v, want nil", err)
	}
	assertPanics(t, "duplicate close-completion notification", s.publishCloseDone)
}

func TestStopDiagnosticIsVisibleWithoutFault(t *testing.T) {
	s := newInstanceState(0x2102, 128, 1_048_576)
	s.publishReady()
	mustElectClose(t, s)
	diagnostic := fmt.Errorf("AsstStop returned 0 (false): %w", native.ErrNativeRejected)
	s.recordStopDiagnostic(diagnostic)
	if got := s.currentError(); got != diagnostic {
		t.Fatalf("error during close = %v, want the Stop diagnostic", got)
	}
	if got := s.faultDiagnostics(); got.stopDiagnostic != diagnostic || got.firstFault != nil {
		t.Fatalf("diagnostics during close = %+v", got)
	}
	select {
	case <-s.faultNotify():
		t.Fatal("a Stop rejection during close must not trigger the first-fault notification")
	default:
	}
	finishClose(t, s, s.currentError())
	if got := s.currentError(); got != s.waitClose() || got != diagnostic {
		t.Fatalf("error after completion = %v, want the stable Stop diagnostic", got)
	}
}

// TestInstanceStateSingleGatedHandle 验证「状态路径只保留
// 一个受门控的 handle,不向外暴露可绕过生命周期的引用」:业务路径只
// 能凭已准入 lease 读取 handle;closeHandle 仅在调用清零后可取,
// CreateEx=0 路径(handle=0)无关闭句柄;状态内唯一 handle 字段在
// 构造→发布→关闭全程值一致、不被旁路修改。
func TestInstanceStateSingleGatedHandle(t *testing.T) {
	t.Run("handle unique throughout and read only via a lease", func(t *testing.T) {
		const h = uintptr(0x3001)
		s := newInstanceState(h, 128, 1_048_576)
		if got := stateHandle(s); got != h {
			t.Fatalf("handle after construction = %#x, want %#x", got, h)
		}
		s.publishReady()
		if got := stateHandle(s); got != h {
			t.Fatalf("handle after publishing ready = %#x, want %#x", got, h)
		}
		l := mustAdmit(t, s, "handle read")
		if got := l.handle(); got != h {
			t.Fatalf("lease.handle() = %#x, want %#x", got, h)
		}
		mustElectClose(t, s)
		if got := stateHandle(s); got != h {
			t.Fatalf("handle in the closing phase = %#x, want %#x", got, h)
		}
		finishClose(t, s, nil)
		if got := stateHandle(s); got != h {
			t.Fatalf("handle after closed = %#x, want %#x (must not be rewritten by a side channel)", got, h)
		}
	})

	t.Run("closeHandle obtainable only after calls drain", func(t *testing.T) {
		const h = uintptr(0x3002)
		s := newInstanceState(h, 128, 1_048_576)
		s.publishReady()
		l := mustAdmit(t, s, "holding in flight")
		mustElectClose(t, s)

		// 在途计数非零时读取关闭句柄是内部不变量破坏。
		assertPanics(t, "closeHandle read while in-flight calls have not drained", func() { s.closeHandle() })

		releaseAndAssertDrained(t, s, l)
		got, ok := s.closeHandle()
		if !ok {
			t.Fatalf("closeHandle ok after draining = false, want true (handle=%#x)", h)
		}
		if got != h {
			t.Fatalf("closeHandle() = %#x, want %#x", got, h)
		}
	})

	t.Run("CreateEx=0 path with handle=0 has no close handle", func(t *testing.T) {
		s := newInstanceState(0, 128, 1_048_576)
		s.publishReady()
		l := mustAdmit(t, s, "call on the zero-handle instance")
		// 0 无效、不解引用:业务路径读到 0 即内部不变量破坏。
		assertPanics(t, "lease.handle() on the zero-handle instance", func() { l.handle() })
		l.release()

		mustElectClose(t, s)
		s.waitCallsDrained()
		finishClose(t, s, nil)
		got, ok := s.closeHandle()
		if ok {
			t.Fatalf("CreateEx=0 path closeHandle ok = true, want false")
		}
		if got != 0 {
			t.Fatalf("CreateEx=0 path closeHandle() = %#x, want 0", got)
		}
	})
}

// TestRecordFaultStickySemantics 钉住首次故障的 sticky 语义:
// 两次故障只保留首个;closing 期间 recordFault 只记因不改阶段;
// closed 后再有故障源是内部不变量破坏;无因故障不被接受。
func TestRecordFaultStickySemantics(t *testing.T) {
	t.Run("two faults keep the first", func(t *testing.T) {
		s := newInstanceState(0x4001, 128, 1_048_576)
		s.publishReady()
		first := fmt.Errorf("first fault: %w", native.ErrProtocol)
		second := fmt.Errorf("second fault: %w", native.ErrEventOverflow)
		s.recordFault(first)
		s.recordFault(second)

		if got := s.currentFault(); !errors.Is(got, first) {
			t.Fatalf("currentFault() = %v, want first cause %v", got, first)
		}
		if got := s.currentFault(); errors.Is(got, native.ErrEventOverflow) {
			t.Fatalf("the first cause was overwritten by the later fault: %v", got)
		}
		_, err := s.admitCall("new call after faulted")
		if !errors.Is(err, first) || !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("the rejection error should reach the first cause along the chain: err = %v", err)
		}
		if errors.Is(err, native.ErrEventOverflow) {
			t.Fatalf("the rejection error mixed in the later fault: %v", err)
		}
	})

	t.Run("during closing only the cause is recorded, the phase unchanged", func(t *testing.T) {
		s := newInstanceState(0x4002, 128, 1_048_576)
		s.publishReady()
		inflight := mustAdmit(t, s, "admitted beforehand")
		mustElectClose(t, s)

		cause := fmt.Errorf("fault during close: %w", native.ErrProtocol)
		s.recordFault(cause)
		if got := s.currentPhase(); got != phaseClosing {
			t.Fatalf("phase after recordFault during closing = %d, want closing (only the cause is recorded, the phase unchanged)", got)
		}
		if got := s.currentFault(); !errors.Is(got, cause) {
			t.Fatalf("currentFault() = %v, want the newly recorded first cause %v", got, cause)
		}
		// 阶段未变:新调用仍按 closing 拒绝。
		_, err := s.admitCall("new call after the closing fault")
		if !errors.Is(err, native.ErrClosing) {
			t.Fatalf("errors.Is(err, native.ErrClosing) = false; err = %v", err)
		}
		// 所有者随后发布的最终错误携带首因。
		finishClose(t, s, cause)
		if got := s.waitClose(); !errors.Is(got, cause) {
			t.Fatalf("waitClose() = %v, want it to contain the first cause %v", got, cause)
		}
		// 已准入调用在关闭全过程后照常释放。
		releaseAndAssertDrained(t, s, inflight)
	})

	t.Run("recording a fault after closed panics", func(t *testing.T) {
		s := newClosedState(t, 0x4003)
		assertPanics(t, "recordFault after closed", func() { s.recordFault(native.ErrProtocol) })
	})

	t.Run("fault without a cause panics", func(t *testing.T) {
		s := newInstanceState(0x4004, 128, 1_048_576)
		s.publishReady()
		assertPanics(t, "recordFault with nil cause", func() { s.recordFault(nil) })
	})
}

// TestInstanceStateIllegalTransitionsPanic 逐条钉住 state.go 声明的
// 非法迁移与不变量守卫 panic:违反阶段机与前置条件的调用按编程错误
// panic,不静默继续污染状态。白盒子用例直接注入被破坏的内部阶段以
// 到达守卫分支。
func TestInstanceStateIllegalTransitionsPanic(t *testing.T) {
	t.Run("initializing admitCall panic", func(t *testing.T) {
		s := newInstanceState(0x5001, 128, 1_048_576)
		assertPanics(t, "admitCall in the initializing phase", func() { _, _ = s.admitCall("unpublished instance") })
	})

	t.Run("initializing electClose panic", func(t *testing.T) {
		s := newInstanceState(0x5002, 128, 1_048_576)
		assertPanics(t, "electClose in the initializing phase", func() { _ = s.electClose() })
	})

	t.Run("publishReady duplicate publication panics", func(t *testing.T) {
		s := newInstanceState(0x5003, 128, 1_048_576)
		s.publishReady()
		assertPanics(t, "publishReady after ready", func() { s.publishReady() })
	})

	t.Run("publishReady after closing panics", func(t *testing.T) {
		s := newInstanceState(0x5004, 128, 1_048_576)
		s.publishReady()
		mustElectClose(t, s)
		assertPanics(t, "publishReady after closing", func() { s.publishReady() })
	})

	t.Run("publishing the final error in the ready phase panics", func(t *testing.T) {
		s := newInstanceState(0x5005, 128, 1_048_576)
		s.publishReady()
		assertPanics(t, "publishFinalError in the ready phase", func() { s.publishFinalError(nil) })
	})

	t.Run("publishing the final error after closed panics", func(t *testing.T) {
		s := newClosedState(t, 0x5006)
		assertPanics(t, "publishFinalError after closed", func() { s.publishFinalError(nil) })
	})

	t.Run("duplicate terminal state publication after closed panics", func(t *testing.T) {
		s := newClosedState(t, 0x5007)
		assertPanics(t, "publishClosed after closed", func() { s.publishClosed() })
	})

	t.Run("admitCall in faulted without a first cause panics", func(t *testing.T) {
		s := newInstanceState(0x5008, 128, 1_048_576)
		s.publishReady()
		// 白盒:直接置位阶段,模拟不变量已被破坏的内部状态。
		s.mu.Lock()
		s.phase = phaseFaulted
		s.mu.Unlock()
		assertPanics(t, "admitCall in faulted without a first cause", func() { _, _ = s.admitCall("invariant broken") })
	})

	t.Run("recordFault in an unknown phase panics", func(t *testing.T) {
		s := newInstanceState(0x5009, 128, 1_048_576)
		s.publishReady()
		s.mu.Lock()
		s.phase = instancePhase(200)
		s.mu.Unlock()
		assertPanics(t, "recordFault in an unknown phase", func() { s.recordFault(native.ErrProtocol) })
	})
}

// TestCallbackGateBasics 钉住 callback 准入门的基础语义(
// 接线归本层):门开时准入、closing 阶段仍开放;关门后一律拒绝;
// 关门至多一次;waitCallbacksDrained 等已准入 callback 退出;
// publishClosed 前必须已关门且已准入 callback 清空。
func TestCallbackGateBasics(t *testing.T) {
	t.Run("admits while open with closing still open, rejects after the gate closes", func(t *testing.T) {
		s := newInstanceState(0x6001, 128, 1_048_576)
		s.publishReady()
		l1, ok := s.admitCallback()
		if !ok {
			t.Fatal("admitCallback must succeed while the gate is open")
		}
		mustElectClose(t, s)
		l2, ok := s.admitCallback()
		if !ok {
			t.Fatal("the gate is still open in the closing phase; admitCallback must succeed")
		}
		l1.release()
		l2.release()

		s.closeCallbackGate()
		if _, ok := s.admitCallback(); ok {
			t.Fatal("admitCallback must fail after the gate is closed")
		}
		s.waitCallbacksDrained()
		// 已显式关门,此处只做所有者的错误与终态发布。
		s.publishFinalError(nil)
		s.publishClosed()
		s.publishCloseDone()
	})

	t.Run("closing the gate twice panics", func(t *testing.T) {
		s := newInstanceState(0x6002, 128, 1_048_576)
		s.publishReady()
		s.closeCallbackGate()
		assertPanics(t, "duplicate closeCallbackGate", func() { s.closeCallbackGate() })
	})

	t.Run("waitCallbacksDrained waits for admitted callbacks to exit", func(t *testing.T) {
		s := newInstanceState(0x6003, 128, 1_048_576)
		s.publishReady()
		l, ok := s.admitCallback()
		if !ok {
			t.Fatal("admitCallback must succeed while the gate is open")
		}
		drained := make(chan struct{})
		go func() {
			s.waitCallbacksDrained()
			close(drained)
		}()

		// 负面断言:callback 在途时不得返回。
		select {
		case <-drained:
			t.Fatal("waitCallbacksDrained returned early while a callback was in flight")
		case <-time.After(notWithin):
		}

		l.release()
		select {
		case <-drained:
		case <-time.After(hangGuard):
			t.Fatal("waitCallbacksDrained did not converge after the callback exited (hang-guard limit hit)")
		}
	})

	t.Run("the gate must be closed and drained before publishClosed", func(t *testing.T) {
		s := newInstanceState(0x6004, 128, 1_048_576)
		s.publishReady()
		mustElectClose(t, s)

		// 门开着不得发布终态。
		assertPanics(t, "publishClosed with the callback gate open", func() { s.publishClosed() })
		// 门开着仍可准入;关门后仍有在途 callback 不得发布终态。
		l, ok := s.admitCallback()
		if !ok {
			t.Fatal("the gate is still open in the closing phase; admitCallback must succeed")
		}
		s.closeCallbackGate()
		assertPanics(t, "publishClosed with a callback in flight", func() { s.publishClosed() })

		l.release()
		s.waitCallbacksDrained()
		s.publishFinalError(nil)
		s.publishClosed()
		s.publishCloseDone()
		if got := s.currentPhase(); got != phaseClosed {
			t.Fatalf("phase after publishClosed following the gate close and drain = %d, want closed", got)
		}
	})
}
