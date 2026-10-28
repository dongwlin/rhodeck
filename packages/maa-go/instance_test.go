// 本文件是实例单次关闭流程的受控用例:验证并发 Close 只执行一次
// Stop/Destroy、关闭等待在途调用、同步 Destroyed 的处理、token 与事件
// 通道清理顺序,以及关闭错误组合。

package maa

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// destroyedMessageID 是 AsstMsg::Destroyed 的消息编号(5),冻结自
// MaaAssistantArknights src/MaaCore/Common/AsstMsg.h(InternalError=0、
// InitFailed=1、ConnectionInfo=2、AllTasksCompleted=3、AsyncCallInfo=4、
// Destroyed=5)。fake closer 的同步 Destroyed 投递按该编号构造:锁定
// Core 在 Destroy 调用线程同步触发 Destroyed(Assistant 析构)。
const destroyedMessageID = MessageID(5)

// fakeInstanceCloser 是关闭流程的受控 fake native:逐次记录 Stop/
// Destroy 调用(顺序、次数、handle),支持注入屏障与动作(如在
// Destroy 内同步投递 Destroyed、在 Stop 期间挂起以探测锁纪律),
// 不加载任何原生库。stopRet 模拟 AsstStop 的 C bool 返回,默认 1
// (成功);Destroy 是 C void 函数,fake 同样无返回值。
type fakeInstanceCloser struct {
	mu             sync.Mutex
	sequence       []string
	stopHandles    []uintptr
	destroyHandles []uintptr
	stopRet        uint8
	onStop         func(handle uintptr)
	onDestroy      func(handle uintptr)
}

// newFakeInstanceCloser 构造 fake closer:Stop 默认返回 1(成功)。
func newFakeInstanceCloser() *fakeInstanceCloser {
	return &fakeInstanceCloser{stopRet: 1}
}

func (f *fakeInstanceCloser) stop(handle uintptr) uint8 {
	f.mu.Lock()
	f.sequence = append(f.sequence, "stop")
	f.stopHandles = append(f.stopHandles, handle)
	f.mu.Unlock()
	if f.onStop != nil {
		f.onStop(handle)
	}
	return f.stopRet
}

func (f *fakeInstanceCloser) destroy(handle uintptr) {
	f.mu.Lock()
	f.sequence = append(f.sequence, "destroy")
	f.destroyHandles = append(f.destroyHandles, handle)
	f.mu.Unlock()
	if f.onDestroy != nil {
		f.onDestroy(handle)
	}
}

// count 返回指定调用的累计次数。
func (f *fakeInstanceCloser) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.sequence {
		if c == name {
			n++
		}
	}
	return n
}

// calls 返回调用序列的副本(断言「先 Stop 后 Destroy」等顺序契约)。
func (f *fakeInstanceCloser) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sequence...)
}

// closeRig 组装完整关闭组件的受控实例:状态发布 ready、分发器接
// s.settleCompletion 并启动、callback target 经生产注册入口登记
// target.deliver(钉住 maa→native 注册接缝形态,对照既有先例)、
// fake closer 注入。token 的生命周期由 close 流程负责:完整执行的
// close 已注销 token;未执行 close 的用例经 cleanup 兜底注销并排空
// 分发循环。
type closeRig struct {
	in     *instance
	s      *instanceState
	d      *eventDispatcher
	target *callbackTarget
	token  uintptr
	closer *fakeInstanceCloser
}

// newCloseRig 构造受控实例,并注册 t.Cleanup 兜底收尾。
func newCloseRig(t *testing.T, handle uintptr, b dispatchBudgets) *closeRig {
	return newCloseRigWithSettle(t, handle, b, nil)
}

// newCloseRigWithSettle 构造受控实例并注入自定义结算通知(nil 取
// s.settleCompletion 的生产接线):供需要拦截分发器处理进度的用例
// (如在可确认结算处设屏障以确定性冻结关闭流程)。
func newCloseRigWithSettle(t *testing.T, handle uintptr, b dispatchBudgets, settle func(completionInfo)) *closeRig {
	t.Helper()
	s := newInstanceState(handle, 128, 1_048_576)
	s.publishReady()
	if settle == nil {
		settle = s.settleCompletion
	}
	d := newEventDispatcher(s, settle, b.callbackBytes, b.internalEvents, b.internalBytes, b.publicEvents, b.publicBytes)
	d.start()
	target := newCallbackTarget(s, d, b.callbackBytes)
	token, err := native.RegisterCallbackTarget(target.deliver)
	if err != nil {
		t.Fatalf("registering target.deliver via the production entry must not fail: %v", err)
	}
	closer := newFakeInstanceCloser()
	in := newInstance(s, d, token, closer)
	rig := &closeRig{in: in, s: s, d: d, target: target, token: token, closer: closer}
	t.Cleanup(func() { rig.cleanup(t) })
	return rig
}

// cleanup 收尾兜底:close 完整执行过的用例(token 已由流程注销、分
// 发循环已随 drain 退出)不做任何事;未执行 close 的用例注销 token
// 并按「关门 → 排空」标准顺序终结分发循环,不留悬挂 goroutine。
func (r *closeRig) cleanup(t *testing.T) {
	t.Helper()
	if r.s.currentPhase() == phaseClosed {
		return
	}
	native.UnregisterCallbackTarget(r.token)
	closeAndDrain(t, r.d)
}

// runClose 以防挂死上限兜底执行一次完整 close:正常路径必须有限步
// 完成;若实现挂死,按超时失败而不是用例挂死。
func runClose(t *testing.T, in *instance) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- in.close() }()
	select {
	case err := <-errCh:
		return err
	case <-time.After(hangGuard):
		t.Fatal("close did not complete within the hang-guard deadline: the close flow is suspected hung")
		return nil
	}
}

// waitUntil 在时限内轮询等待 cond 成立(1ms 步长):用于把 goroutine
// 调度延迟从被测行为中分离——等待的实现行为本身不是断言对象,超时
// 即失败。cond 必须最终成立,否则用例按防挂死上限失败。
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(hangGuard)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not hold within the hang-guard deadline", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// assertClosedChannels 断言关闭完成后的通道终态:Done/closeDone 已
// 关闭;Events 缓冲事件读尽后通道已关闭(关闭后已缓冲事件仍可读)。
func assertClosedChannels(t *testing.T, rig *closeRig) {
	t.Helper()
	select {
	case <-rig.s.done():
	default:
		t.Error("closeDone/Done not closed after close completed")
	}
	for {
		select {
		case _, ok := <-rig.d.events():
			if !ok {
				return
			}
		case <-time.After(hangGuard):
			t.Fatal("timed out reading public events (hang-guard deadline hit)")
		}
	}
}

// TestNewInstanceContract 钉住组装构造的接线契约:nil state/disp/
// closer 与零 token 都是接线层编程错误,panic(对照 newCallbackTarget
// 先例);合法组合构造成功。
func TestNewInstanceContract(t *testing.T) {
	b := generousBudgets()
	s := newInstanceState(0xC000, 128, 1_048_576)
	s.publishReady()
	d := newEventDispatcher(s, s.settleCompletion, b.callbackBytes, b.internalEvents, b.internalBytes, b.publicEvents, b.publicBytes)
	d.start()
	target := newCallbackTarget(s, d, b.callbackBytes)
	token, err := native.RegisterCallbackTarget(target.deliver)
	if err != nil {
		t.Fatalf("registration must not fail: %v", err)
	}
	defer native.UnregisterCallbackTarget(token)
	closer := newFakeInstanceCloser()

	if got := newInstance(s, d, token, closer); got == nil || got.state != s || got.disp != d || got.token != token {
		t.Fatalf("newInstance assembled fields mismatch: %+v", got)
	}
	assertPanics(t, "nil state", func() { newInstance(nil, d, token, closer) })
	assertPanics(t, "nil dispatcher", func() { newInstance(s, nil, token, closer) })
	assertPanics(t, "nil closer", func() { newInstance(s, d, token, nil) })
	assertPanics(t, "zero token", func() { newInstance(s, d, 0, closer) })
}

// TestInstanceCloseConcurrentSingleDestroy 验证「并发多次
// Close 只执行一次 Destroy,并等待同一 closeDone 后返回同一最终错
// 误」:N 个 goroutine 并发 close 恰一所有者,Stop/Destroy 各恰一次
// 且先 Stop 后 Destroy、handle 逐字转发;全部申请者(所有者与失败
// 者)取得同一最终错误存储值(正常 nil、首因同一错误值);closed 后
// 的幂等 close 不再触发 native 调用。
func TestInstanceCloseConcurrentSingleDestroy(t *testing.T) {
	t.Run("concurrent closes perform exactly one Stop/Destroy with the same final state", func(t *testing.T) {
		const handle = uintptr(0xC100)
		rig := newCloseRig(t, handle, generousBudgets())

		const n = 8
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = rig.in.close()
			}(i)
		}
		wg.Wait()

		if got := rig.closer.count("stop"); got != 1 {
			t.Fatalf("Stop call count = %d, want exactly 1", got)
		}
		if got := rig.closer.count("destroy"); got != 1 {
			t.Fatalf("Destroy call count = %d, want exactly 1", got)
		}
		if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
			t.Fatalf("native call sequence = %v, want [stop destroy] (one Stop then one Destroy)", seq)
		}
		if got := rig.closer.stopHandles[0]; got != handle {
			t.Errorf("Stop received handle %#x, want %#x", got, handle)
		}
		if got := rig.closer.destroyHandles[0]; got != handle {
			t.Errorf("Destroy received handle %#x, want %#x", got, handle)
		}
		for i, err := range errs {
			if err != nil {
				t.Errorf("Close %d = %v, want nil (normal Stop/Destroy with no fault)", i, err)
			}
		}

		// closed 后的幂等 close:同一终态,不再触发 native 调用。
		if err := rig.in.close(); err != nil {
			t.Errorf("idempotent close = %v, want nil", err)
		}
		if got := rig.closer.count("destroy"); got != 1 {
			t.Errorf("Destroy count after the idempotent close = %d, want still 1", got)
		}
		assertClosedChannels(t, rig)
		if got := rig.s.currentPhase(); got != phaseClosed {
			t.Errorf("phase after close = %d, want closed", got)
		}
	})

	t.Run("concurrent closes share the first-cause final state", func(t *testing.T) {
		rig := newCloseRig(t, 0xC101, generousBudgets())
		cause := fmt.Errorf("business-phase event overflow: %w", native.ErrEventOverflow)
		rig.s.recordFault(cause)

		const n = 8
		errs := make([]error, n)
		var wg sync.WaitGroup
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = rig.in.close()
			}(i)
		}
		wg.Wait()

		for i, err := range errs {
			// 同一最终错误存储:所有申请者取得与首因同一错误值,首因
			// 身份沿错误链可达,不混入其他哨兵。
			if err != cause {
				t.Errorf("Close %d = %v, want the same stored value as the first cause %v", i, err, cause)
			}
			if !errors.Is(err, native.ErrEventOverflow) {
				t.Errorf("Close %d: errors.Is(err, native.ErrEventOverflow) = false; err = %v", i, err)
			}
		}
		// faulted 进入关闭仍无条件执行一次 Stop/Destroy()。
		if got := rig.closer.count("destroy"); got != 1 {
			t.Errorf("Destroy count of the faulted close = %d, want exactly 1", got)
		}
	})
}

// TestInstanceCloseWaitsCallsBeforeDestroy 验证「排队及执行
// 中的调用退出前不 Destroy,新业务调用在 closing 后被拒绝」:执行中
// 的 A(占据 raw 串行门禁)与门禁前排队的 B 均在途时,close 停在等
// 待清零段,Stop/Destroy 不得发生;closing 后 admitCall 拒绝并包装
// ErrClosing;A 退出后 B 转入门禁段仍被等待覆盖;B 退出后关闭才前进
// 到一次 Stop、一次 Destroy;closed 后 admitCall 包装 ErrClosed。
func TestInstanceCloseWaitsCallsBeforeDestroy(t *testing.T) {
	const handle = uintptr(0xC200)
	rig := newCloseRig(t, handle, generousBudgets())
	s := rig.s

	// A:已准入并进入 raw 段(执行中);B:已准入、将在门禁前排队,
	// 进入门禁段后由屏障保持在途,直到用例放行。
	leaseA := mustAdmit(t, s, "call A")
	s.raw.enter()
	leaseB := mustAdmit(t, s, "call B")
	bInRaw := make(chan struct{})
	releaseB := make(chan struct{})
	go func() {
		s.raw.enter() // A 退出前一直排队
		close(bInRaw)
		<-releaseB
		s.raw.exit()
		leaseB.release()
	}()

	closeErr := make(chan error, 1)
	go func() { closeErr <- rig.in.close() }()

	// 等待选主完成(阶段转 closing),把调度延迟与被测行为分离。
	waitUntil(t, "close won the election and transitioned to closing", func() bool { return s.currentPhase() == phaseClosing })

	// 负面断言一:A、B 均在途,Stop/Destroy 不得发生。
	<-time.After(notWithin)
	if got := rig.closer.count("stop"); got != 0 {
		t.Fatalf("Stop called %d times while in-flight calls have not exited", got)
	}
	if got := rig.closer.count("destroy"); got != 0 {
		t.Fatalf("Destroy called %d times while in-flight calls have not exited", got)
	}
	// closing 后新业务调用被拒绝(包装 native.ErrClosing)。
	if _, err := s.admitCall("new call during closing"); !errors.Is(err, native.ErrClosing) {
		t.Fatalf("new call during closing = %v, want errors.Is native.ErrClosing", err)
	}

	// A 完成 raw 段并释放;排队者 B 转入门禁段(排队者被等待覆盖)。
	s.raw.exit()
	leaseA.release()
	select {
	case <-bInRaw:
	case <-time.After(hangGuard):
		t.Fatal("B did not acquire the raw gate after A released (hang-guard deadline hit)")
	}

	// 负面断言二:B 仍在途,Stop/Destroy 仍不得发生。
	<-time.After(notWithin)
	if got := rig.closer.count("stop"); got != 0 {
		t.Fatalf("Stop called %d times while queued call B has not exited", got)
	}
	if got := rig.closer.count("destroy"); got != 0 {
		t.Fatalf("Destroy called %d times while queued call B has not exited", got)
	}

	// B 退出后关闭才前进:先 Stop 后 Destroy 各恰一次,handle 逐字
	// 转发;close 返回 nil。
	close(releaseB)
	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("close = %v, want nil", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("close did not complete within the hang-guard deadline after B released")
	}
	if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
		t.Fatalf("native call sequence = %v, want [stop destroy]", seq)
	}
	if got := rig.closer.destroyHandles[0]; got != handle {
		t.Errorf("Destroy received handle %#x, want %#x", got, handle)
	}

	// closed 后新业务调用包装 native.ErrClosed。
	if _, err := s.admitCall("new call after closed"); !errors.Is(err, native.ErrClosed) {
		t.Fatalf("new call after closed = %v, want errors.Is native.ErrClosed", err)
	}
}

// TestInstanceCloseSyncCallbackDuringNativeCalls 验证「Stop/
// Destroy 同步进入 callback 时无锁死,Destroyed 不被当作关闭完成的
// 唯一条件」:native stop 调用挂起期间,状态锁操作与 callback 准入
// 投递都能限时完成(实现若持状态锁/callback 门/分发器锁/注册表锁跨
// native 调用必然超时);Destroy 调用线程内同步投递 Destroyed(锁定
// Core 的真实形态)照常准入入队,close 不锁死且事件关闭后仍可读;
// 完全不投递 Destroyed 时 close 照常完成——Destroyed 只是诊断事件,
// 不是关闭完成的条件。
func TestInstanceCloseSyncCallbackDuringNativeCalls(t *testing.T) {
	t.Run("holds no state or callback/dispatcher lock during Stop", func(t *testing.T) {
		rig := newCloseRig(t, 0xC300, generousBudgets())
		s := rig.s

		stopStarted := make(chan struct{})
		releaseStop := make(chan struct{})
		rig.closer.onStop = func(handle uintptr) {
			close(stopStarted)
			<-releaseStop
		}
		closeErr := make(chan error, 1)
		go func() { closeErr <- rig.in.close() }()
		<-stopStarted

		// 探针一:状态锁操作(native stop 调用挂起期间)必须能完成。
		phaseCh := make(chan instancePhase, 1)
		go func() { phaseCh <- s.currentPhase() }()
		select {
		case got := <-phaseCh:
			if got != phaseClosing {
				t.Errorf("currentPhase() during stop = %d, want closing", got)
			}
		case <-time.After(hangGuard):
			t.Fatal("the state-lock operation of another goroutine did not complete during stop: the implementation is suspected of holding the state lock across native calls")
		}

		// 探针二:callback 准入 + 分发器入队(native stop 调用挂起期
		// 间)必须能完成——closing 阶段门仍开放。
		delivered := make(chan struct{})
		go func() {
			rig.target.deliver(60, callbackCBytes(`{"what":"DuringStop"}`))
			close(delivered)
		}()
		select {
		case <-delivered:
		case <-time.After(hangGuard):
			t.Fatal("callback delivery during stop did not complete: the implementation is suspected of holding the callback/dispatcher/registry lock across native calls")
		}

		close(releaseStop)
		if err := <-closeErr; err != nil {
			t.Fatalf("close = %v, want nil", err)
		}
		// stop 期间投递的事件关闭后仍可读,Events 随后已关闭。
		ev := mustReadEvent(t, rig.d.events())
		if ev.MessageID != 60 {
			t.Errorf("event MessageID = %d, want 60", ev.MessageID)
		}
		if _, ok := <-rig.d.events(); ok {
			t.Error("Events should be closed after close")
		}
	})

	t.Run("Destroy synchronously triggering Destroyed does not deadlock", func(t *testing.T) {
		rig := newCloseRig(t, 0xC301, generousBudgets())

		destroyEntered := make(chan struct{})
		releaseDestroy := make(chan struct{})
		rig.closer.onDestroy = func(handle uintptr) {
			close(destroyEntered)
			// 模拟锁定 Core:Destroy 调用线程 join 后同步触发 Destroyed
			// (Assistant 析构);此刻 callback 门必须仍开放。
			rig.target.deliver(int32(destroyedMessageID), callbackCBytes(`{}`))
			<-releaseDestroy
		}
		closeErr := make(chan error, 1)
		go func() { closeErr <- rig.in.close() }()
		<-destroyEntered
		close(releaseDestroy)

		if err := <-closeErr; err != nil {
			t.Fatalf("close = %v, want nil (a synchronous Destroyed must not deadlock close)", err)
		}
		// Destroyed 事件关闭后仍可读;Events 随后已关闭。
		ev := mustReadEvent(t, rig.d.events())
		if ev.MessageID != destroyedMessageID {
			t.Errorf("event MessageID = %d, want Destroyed(%d)", ev.MessageID, destroyedMessageID)
		}
		if _, ok := <-rig.d.events(); ok {
			t.Error("Events should be closed after close")
		}
	})

	t.Run("Destroyed is not the sole condition for close completion", func(t *testing.T) {
		rig := newCloseRig(t, 0xC302, generousBudgets())
		// fake Destroy 不投递任何回调:close 仍必须完成。
		if err := runClose(t, rig.in); err != nil {
			t.Fatalf("close = %v, want nil (close should complete without Destroyed too)", err)
		}
		select {
		case <-rig.s.done():
		default:
			t.Fatal("Done not closed after close returned")
		}
		if got := rig.s.currentPhase(); got != phaseClosed {
			t.Errorf("phase = %d, want closed", got)
		}
		if _, ok := <-rig.d.events(); ok {
			t.Error("Events should be closed and empty (no Destroyed event)")
		}
	})
}

// TestInstanceCloseGateAndTokenAfterDestroy 验证「Destroy 返
// 回后才关闭 callback 门,最后一个已准入 callback 退出后才清理 token
// 和通道」:Destroy 返回前同步投递的消息必须照常准入入队(门若提前
// 关闭即拒收,事件缺失即失序暴露);close 完成后门已关闭、token 已由
// 流程移除(重复注销按注册表契约 panic)、Events 已关闭;最后一个已
// 准入 callback 未退出时 close 不完成(关门等待被它挡住),退出后才
// 收敛——token 与通道的清理不可能早于该时点。
func TestInstanceCloseGateAndTokenAfterDestroy(t *testing.T) {
	t.Run("callback gate stays open while Destroy returns", func(t *testing.T) {
		rig := newCloseRig(t, 0xC400, generousBudgets())
		rig.closer.onDestroy = func(handle uintptr) {
			// Destroy 返回前同步投递:门必须仍开放,消息照常入队。
			rig.target.deliver(int32(destroyedMessageID), callbackCBytes(`{}`))
		}
		if err := runClose(t, rig.in); err != nil {
			t.Fatalf("close = %v, want nil", err)
		}
		// Destroy 返回前投递的消息在关闭后仍可读——若实现把关门提前
		// 到 Destroy 返回之前,该投递被拒收,事件缺失即暴露失序。
		ev := mustReadEvent(t, rig.d.events())
		if ev.MessageID != destroyedMessageID {
			t.Errorf("event MessageID = %d, want Destroyed(%d)", ev.MessageID, destroyedMessageID)
		}
		if _, ok := <-rig.d.events(); ok {
			t.Error("Events should be closed after close")
		}
		// close 完成后:门已关闭,新 callback 一律拒绝。
		if _, ok := rig.s.admitCallback(); ok {
			t.Error("callback admission gate still open after close returned")
		}
	})

	t.Run("close does not complete before the last admitted callback exits", func(t *testing.T) {
		rig := newCloseRig(t, 0xC401, generousBudgets())
		s := rig.s

		// 主测试持分发器锁:deliver 将在取得 callback lease 后、预留
		// 队列额度前暂停(对照竞态 B 先例)。
		rig.d.mu.Lock()
		go func() {
			rig.target.deliver(61, callbackCBytes(`{"what":"InFlight"}`))
		}()
		waitUntil(t, "in-flight callback acquired the lease", func() bool { return inFlightCallbacks(s) == 1 })

		closeErr := make(chan error, 1)
		go func() { closeErr <- rig.in.close() }()

		// 等待 close 流程关门(进入等待退空段),把调度延迟与被测行为
		// 分离;此刻在途 callback 仍持 lease。
		waitUntil(t, "close closed the callback admission gate", func() bool {
			s.mu.Lock()
			defer s.mu.Unlock()
			return s.callbackGateClosed
		})

		// 负面断言一:close 未返回——最后一个已准入 callback 未退出,
		// 关闭流程不得完成任何清理并收敛。
		<-time.After(notWithin)
		select {
		case err := <-closeErr:
			t.Fatalf("close returned early while the in-flight callback has not exited: %v", err)
		default:
		}
		// 关门后新准入一律失败,而在途者仍持 lease 不受影响。
		if _, ok := s.admitCallback(); ok {
			t.Error("admitCallback must fail after the gate closed")
		}

		// 放行:复制、入队、退出 lease;关闭流程随之收敛(入队发生在
		// closeIntake 之前——waitCallbacksDrained 先于排空段)。
		rig.d.mu.Unlock()
		select {
		case err := <-closeErr:
			if err != nil {
				t.Fatalf("close = %v, want nil", err)
			}
		case <-time.After(hangGuard):
			t.Fatal("close did not complete within the hang-guard deadline after the callback exited")
		}

		// token 已由流程移除:重复注销按注册表契约 panic(正向证明)。
		assertPanics(t, "double unregister after close removed the token", func() {
			native.UnregisterCallbackTarget(rig.token)
		})
		// 在途 callback 的消息排空期间已投递,关闭后仍可读;Events 随
		// 后已关闭;终态发布完成。
		ev := mustReadEvent(t, rig.d.events())
		if ev.MessageID != 61 {
			t.Errorf("event MessageID = %d, want 61", ev.MessageID)
		}
		if _, ok := <-rig.d.events(); ok {
			t.Error("Events should be closed after close")
		}
		if got := s.currentPhase(); got != phaseClosed {
			t.Errorf("phase = %d, want closed", got)
		}
		select {
		case <-s.done():
		default:
			t.Error("closeDone not closed after close completed")
		}
	})
}

// TestInstanceCloseWithStalledEventConsumer 验证「外部停读
// Events 时仍能完成清理;观察 Done 或 Close 返回后,Err 与全部
// Operation 终态立即稳定」:公开缓冲收紧到 1 条且消费者全程停读,
// 排空对无法投递的事件按溢出契约记录故障并计丢失、不等待读者,关闭
// 照常收敛(返回溢出首因);已确认的完成结果不被随后的溢出改写,未
// 完成项按首因统一结项;close 返回后全部 Operation 的等待立
// 即返回终态,无需任何额外等待。
func TestInstanceCloseWithStalledEventConsumer(t *testing.T) {
	b := generousBudgets()
	b.publicEvents = 1 // 公开缓冲收紧:仅第 1 条事件可投递,其余溢出
	rig := newCloseRig(t, 0xC500, b)
	s := rig.s

	// 两笔 pending Operation:一笔带可确认完成消息,一笔无完成。
	idSettled := AsyncID(7)
	idClosed := AsyncID(8)
	opSettled := mustAdmitPending(t, s, idSettled, "settled submission")
	opClosed := mustAdmitPending(t, s, idClosed, "unsettled submission")

	enqueue(t, rig.d, asyncCallInfoMessageID, asyncCallInfoPayload(idSettled, "Screencap", `{"ret":true}`))
	enqueue(t, rig.d, 99, []byte(`{"what":"UnknownOne"}`))
	enqueue(t, rig.d, 99, []byte(`{"what":"UnknownTwo"}`))

	// 消费者全程停读:close 仍必须有限步完成,不得等待外部读者。
	closeErr := make(chan error, 1)
	go func() { closeErr <- rig.in.close() }()
	var finalErr error
	select {
	case finalErr = <-closeErr:
	case <-time.After(hangGuard):
		t.Fatal("close did not complete within the hang-guard deadline with Events reads externally stopped: the drain is suspected of waiting on readers")
	}

	// 溢出按契约成为首因并成为 Close 错误;丢失计数为 2;故障通知独立
	// 于 Events 有空间与否,已关闭。
	if finalErr == nil {
		t.Fatal("the public drain overflow must become the Close error and must not masquerade as complete delivery")
	}
	if !errors.Is(finalErr, native.ErrEventOverflow) {
		t.Errorf("errors.Is(finalErr, native.ErrEventOverflow) = false; err = %v", finalErr)
	}
	if lost := s.currentLostEvents(); lost != 2 {
		t.Errorf("lost count = %d, want 2", lost)
	}
	select {
	case <-s.faultNotify():
	default:
		t.Error("the overflow fault notification is not closed")
	}

	// 观察到 Done 即见稳定终态:Done 已关闭,Err 与 finalErr 同一存储
	// 值且不再变化。
	select {
	case <-s.done():
	default:
		t.Error("closeDone not closed after close returned")
	}
	if got := s.finalError(); got != finalErr {
		t.Errorf("finalError() = %v, want the same stored value as the Close return %v", got, finalErr)
	}

	// 全部 Operation 终态立即稳定:已确认的成功结果不被随后的溢出
	// 改写;未完成项由首因线性化扫描统一结项,错误与最
	// 终错误同一存储值;等待无需任何额外延迟。
	res, werr := waitGuarded(t, opSettled, context.Background())
	if werr != nil {
		t.Fatalf("wait of the confirmed completion = %v, want nil (the successful final state is not rewritten by the overflow)", werr)
	}
	if res.ID != idSettled || res.What != "Screencap" || !res.OK {
		t.Fatalf("settlement result = %+v, want {7 Screencap true}", res)
	}
	_, werr = waitGuarded(t, opClosed, context.Background())
	if werr == nil {
		t.Fatal("the unsettled item must already be closed out under the first cause and must not remain waitable")
	}
	if !errors.Is(werr, native.ErrEventOverflow) {
		t.Errorf("errors.Is(unsettled item settlement error, native.ErrEventOverflow) = false; err = %v", werr)
	}
	if werr != finalErr {
		t.Errorf("unsettled item settlement error = %v, want the same stored value as the final error %v", werr, finalErr)
	}
	if n := pendingOccupancy(s); n != 0 {
		t.Errorf("pending occupancy = %d, want 0 (all closed out)", n)
	}

	// 已在公开缓冲中的事件关闭后仍可读:恰为第 1 条(完成消息)。
	ev := mustReadEvent(t, rig.d.events())
	if ev.MessageID != asyncCallInfoMessageID {
		t.Errorf("buffered event MessageID = %d, want AsyncCallInfo(%d)", ev.MessageID, asyncCallInfoMessageID)
	}
	if _, ok := <-rig.d.events(); ok {
		t.Error("Events should be closed after close")
	}
	// faulted 进入关闭仍无条件执行一次 Stop/Destroy()。
	if got := rig.closer.count("destroy"); got != 1 {
		t.Errorf("Destroy count = %d, want exactly 1", got)
	}
}

// TestInstanceCloseErrorComposition 验证「不从 Destroy 的
// void 返回制造成功码,也不将等待超时描述为原生资源已安全回收」:
// Stop 返回 0 时 Close 返回含接口与原始 bool 的 ErrNativeRejected 诊
// 断(非 nil——Destroy 的 void 返回不制造成功),继续 Destroy,剩余
// Operation 以 ErrClosed 结项而非成功;已有首因是主因,停止拒绝作为
// 附加诊断保留,两者沿错误链可辨;Destroy 卡住时 close 不返回、Done
// 不关闭、错误不发布——不存在自设超时后宣称回收完成的路径,并发第
// 二申请等待同一 closeDone。
func TestInstanceCloseErrorComposition(t *testing.T) {
	t.Run("Stop rejection becomes the Close cause with no success code fabricated", func(t *testing.T) {
		rig := newCloseRig(t, 0xC600, generousBudgets())
		rig.closer.stopRet = 0
		id := AsyncID(9)
		op := mustAdmitPending(t, rig.s, id, "unsettled submission")

		err := runClose(t, rig.in)
		if err == nil {
			t.Fatal("Close must not return nil when Stop returns 0: a success code must not be fabricated from the void return of Destroy")
		}
		if !errors.Is(err, native.ErrNativeRejected) {
			t.Errorf("errors.Is(err, native.ErrNativeRejected) = false; err = %v", err)
		}
		for _, want := range []string{"AsstStop", "0 (false)"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the stop diagnostic should contain the interface and raw bool %q:\n%s", want, err.Error())
			}
		}
		// 拒绝后继续 Destroy(),各恰一次。
		if got := rig.closer.count("stop"); got != 1 || rig.closer.count("destroy") != 1 {
			t.Errorf("Stop/Destroy count = %d/%d, want 1/1", rig.closer.count("stop"), rig.closer.count("destroy"))
		}
		// 剩余 Operation 不被结为成功:以 ErrClosed 结项(第 5 步)。
		_, werr := waitGuarded(t, op, context.Background())
		if !errors.Is(werr, native.ErrClosed) {
			t.Errorf("errors.Is(remaining Operation settlement error, native.ErrClosed) = false; err = %v", werr)
		}
	})

	t.Run("first cause as the primary cause, stop rejection as an additional diagnostic, both discernible", func(t *testing.T) {
		rig := newCloseRig(t, 0xC601, generousBudgets())
		cause := fmt.Errorf("business-phase protocol violation: %w", native.ErrProtocol)
		rig.s.recordFault(cause)
		rig.closer.stopRet = 0

		err := runClose(t, rig.in)
		if err == nil {
			t.Fatal("Close must not return nil with the first cause plus stop rejection")
		}
		// 两者均沿错误链可辨:首因身份保留,停止拒绝可达。
		if !errors.Is(err, cause) || !errors.Is(err, native.ErrProtocol) {
			t.Errorf("the final error must preserve the first-cause identity: err = %v", err)
		}
		if !errors.Is(err, native.ErrNativeRejected) {
			t.Errorf("the stop-rejection diagnostic must be discernible along the error chain: err = %v", err)
		}
	})

	t.Run("close sets no timeout of its own while Destroy is stuck; concurrent requests wait on the same closeDone", func(t *testing.T) {
		rig := newCloseRig(t, 0xC602, generousBudgets())
		destroyEntered := make(chan struct{})
		releaseDestroy := make(chan struct{})
		rig.closer.onDestroy = func(handle uintptr) {
			close(destroyEntered)
			<-releaseDestroy
		}
		closeErr := make(chan error, 1)
		go func() { closeErr <- rig.in.close() }()
		<-destroyEntered
		// 确认 close 已进入 native 段(Stop 已发生)后进入观察窗。
		waitUntil(t, "close reached the native stop section", func() bool { return rig.closer.count("stop") == 1 })

		// 负面观察窗:Destroy 未返回期间 close 不返回、Done 不关闭、
		// 最终错误不发布——不存在「超时后宣称原生资源已安全回收」的
		// 路径(关闭由 control 的 worker 监督兜底)。
		<-time.After(notWithin)
		select {
		case err := <-closeErr:
			t.Fatalf("close returned early while Destroy has not returned: %v (no self-imposed forced-reclaim timeout allowed)", err)
		default:
		}
		select {
		case <-rig.s.done():
			t.Fatal("closeDone closed while Destroy has not returned: masquerading close completion")
		default:
		}
		if err := rig.s.finalError(); err != nil {
			t.Errorf("final error published while Destroy has not returned: %v", err)
		}

		// 并发第二申请:等待同一 closeDone,同样不得提前返回。
		closeErr2 := make(chan error, 1)
		go func() { closeErr2 <- rig.in.close() }()
		<-time.After(notWithin)
		select {
		case err := <-closeErr2:
			t.Fatalf("the concurrent second request did not wait on the same closeDone: returned early %v", err)
		default:
		}

		close(releaseDestroy)
		for i, ch := range []chan error{closeErr, closeErr2} {
			select {
			case err := <-ch:
				if err != nil {
					t.Errorf("close %d = %v, want nil", i, err)
				}
			case <-time.After(hangGuard):
				t.Fatal("close did not complete within the hang-guard deadline after release")
			}
		}
		if got := rig.closer.count("destroy"); got != 1 {
			t.Errorf("Destroy count = %d, want exactly 1", got)
		}
	})
}

// TestInstanceCloseZeroHandleSkipsStopDestroy 钉住 「CreateEx=0
// 的路径不调用 Stop/Destroy」:handle=0 的回滚路径跳过 native 停止/
// 销毁,但关门、退空、移除 token、排空结算与终态发布照常完成。
func TestInstanceCloseZeroHandleSkipsStopDestroy(t *testing.T) {
	rig := newCloseRig(t, 0, generousBudgets())

	if err := runClose(t, rig.in); err != nil {
		t.Fatalf("close = %v, want nil", err)
	}
	if got := rig.closer.count("stop"); got != 0 {
		t.Errorf("Stop count on the CreateEx=0 path = %d, want 0", got)
	}
	if got := rig.closer.count("destroy"); got != 0 {
		t.Errorf("Destroy count on the CreateEx=0 path = %d, want 0", got)
	}
	// 其余清理照常:门已关闭、token 已移除、Events 已关闭、终态发布。
	if _, ok := rig.s.admitCallback(); ok {
		t.Error("callback admission gate still open after close")
	}
	assertPanics(t, "double unregister after close removed the token", func() {
		native.UnregisterCallbackTarget(rig.token)
	})
	if _, ok := <-rig.d.events(); ok {
		t.Error("Events should be closed and empty")
	}
	if got := rig.s.currentPhase(); got != phaseClosed {
		t.Errorf("phase = %d, want closed", got)
	}
	select {
	case <-rig.s.done():
	default:
		t.Error("closeDone not closed after close completed")
	}
}

// TestInstanceCloseEventsClosedAfterSettlementAndPublication 钉住冻
// 结的关闭顺序(第 5 步:先发布最终错误和 closed 状态,再由通道
// 所有者关闭 Events;:Events 的发送者停止、所有 callback 退出、
// Operation 全部结算后,才关闭 Events——固定容量和关闭顺序是冻结的
// 验收要求):用一次性结算屏障把分发器拦停在「可确认结算已完成、尚
// 未投递」处(发送者停止点因此未到达,关闭流程确定性停在
// waitProcessed 的通道接收上,不持任何锁),测试持有状态锁后放行——
// 分发器到达停止点时流程恰好冻结在剩余结算(settleRemainingPending)
// 的状态锁上,不存在任何调度窗口;若实现把 Events 关闭提前到剩余结
// 算与终态发布之前(审查指出的回归形态),closeEvents 不触碰状态锁,
// 关闭在观察窗内必然完成,冻结点的开放性检查即失序暴露。
func TestInstanceCloseEventsClosedAfterSettlementAndPublication(t *testing.T) {
	reached := make(chan struct{})
	release := make(chan struct{})
	// settleState 由构造后的赋值填充:屏障只在实际处理消息(构造并入
	// 队之后)时运行,赋值经提交入队的 cond 交接对分发器 goroutine 可
	// 见。屏障位于结算之后、投递之前:放行后分发器到停止点的路径不再
	// 需要状态锁,测试持有的状态锁恰好冻结流程于剩余结算。
	var settleState *instanceState
	settle := func(info completionInfo) {
		settleState.settleCompletion(info)
		reached <- struct{}{}
		<-release
	}
	rig := newCloseRigWithSettle(t, 0xC700, generousBudgets(), settle)
	settleState = rig.s
	rig.closer.stopRet = 0 // 最终错误非 nil,发布时点可观察

	// opSettled:带可确认完成消息,结算后拦停在投递之前;op:无完成
	// 消息,留给剩余结算,是顺序断言的观察对象。
	opSettled := mustAdmitPending(t, rig.s, AsyncID(20), "confirmable completion submission")
	op := mustAdmitPending(t, rig.s, AsyncID(21), "task unfinished at close")
	enqueue(t, rig.d, asyncCallInfoMessageID, asyncCallInfoPayload(AsyncID(20), "Screencap", `{"ret":true}`))
	<-reached // 分发器已完成可确认结算,拦停在投递之前(不持任何锁)

	closeErr := make(chan error, 1)
	go func() { closeErr <- rig.in.close() }()

	// 等待关闭流程越过入口关闭:此刻它已请求排空并停在 waitProcessed
	// 的通道接收上——分发器仍被结算屏障拦停,停止点不可能到达,流程
	// 此后不再触碰状态锁,加锁无竞争。
	waitUntil(t, "close flow passed the intake closure", func() bool {
		rig.d.mu.Lock()
		defer rig.d.mu.Unlock()
		return rig.d.intakeClosed
	})

	// 持有状态锁冻结流程。
	rig.s.mu.Lock()

	// 放行投递:分发器非阻塞投递 → 到达发送者停止点并关闭 processed
	// → 在释放信号上挂起;全程不需要状态锁,与测试持有的状态锁无竞争。
	close(release)

	// 等待发送者停止点:流程随即从 waitProcessed 返回并冻结在剩余结
	// 算的状态锁上(测试持有)。
	select {
	case <-rig.d.processed:
	case <-time.After(hangGuard):
		rig.s.mu.Unlock()
		t.Fatal("the sender stop point was not reached within the hang-guard deadline")
	}
	// 观察窗(仍持状态锁):正确实现中,剩余结算、终态发布与
	// closeEvents 都被状态锁挡在冻结点之后,持有期间 Events 不可能被
	// 关闭;回归实现(closeEvents 先于剩余结算)不触碰状态锁,关闭在
	// 本窗口内必然完成,随后的开放性检查即失序暴露。
	<-time.After(notWithin)

	// 冻结点断言(状态锁内白盒读取 + 通道排空,无调度竞争):可确认
	// 结果已结算、剩余 Operation 未结算、最终错误未发布;Events 排空
	// 缓冲后必须仍然开放——带缓冲的已关闭通道会先交出缓冲项(ok=
	// true),故以排空到 ok=false 或 default 为准判别。
	if n := len(rig.s.pending); n != 1 {
		t.Errorf("pending occupancy at the freeze point = %d, want 1 (only the unsettled item without a completion message remains)", n)
	}
	if rig.s.finalErr != nil {
		t.Errorf("final error at the freeze point = %v, want nil (not yet published)", rig.s.finalErr)
	}
	eventsOpen := true
	for eventsOpen {
		select {
		case _, ok := <-rig.d.events():
			eventsOpen = ok
			if !ok {
				t.Error("Events must not be closed before remaining settlement and final-state publication (frozen order)")
			}
		default:
			eventsOpen = false // 已空且未关闭:顺序正确
		}
	}

	// 放行:剩余结算 → 发布最终错误与 closed 状态 → 通道所有者关闭
	// Events → close 返回。
	rig.s.mu.Unlock()
	select {
	case err := <-closeErr:
		if !errors.Is(err, native.ErrNativeRejected) {
			t.Fatalf("close = %v, want a stop diagnostic containing ErrNativeRejected (stopRet=0 makes the final error observable)", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("close did not complete within the hang-guard deadline after release")
	}

	// close 返回后复核终态稳定:未完成项已按 ErrClosed 结项,可确认
	// 结果不因随后公开排空而改写,Events 已由通道所有者关闭,缓冲中
	// 的事件(冻结点若未读走)关闭后仍可读。
	if _, werr := waitGuarded(t, op, context.Background()); !errors.Is(werr, native.ErrClosed) {
		t.Errorf("unsettled item settlement error = %v, want wrapping native.ErrClosed", werr)
	}
	res, werr := waitGuarded(t, opSettled, context.Background())
	if werr != nil || !res.OK || res.ID != AsyncID(20) {
		t.Errorf("confirmable result = %+v, %v; want the successful final state not rewritten", res, werr)
	}
	for {
		select {
		case _, ok := <-rig.d.events():
			if ok {
				continue // 冻结点未读走的缓冲事件关闭后仍可读
			}
		case <-time.After(hangGuard):
			t.Fatal("timed out draining the public buffer (hang-guard deadline hit)")
		}
		break // ok=false:已关闭且排空,终态成立
	}
	select {
	case <-rig.s.done():
	default:
		t.Error("Done not closed after close returned")
	}
}
