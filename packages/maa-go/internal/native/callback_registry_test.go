// 本文件是 callback token 注册表与平台中立分发序的受控用例:验证 token
// 单调性与耗尽拒绝、固定分发序、锁不跨投递、注销时序,以及测试暂停缝。

package native

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// hangGuard 是防挂死兜底上限:仅用于「操作必须完成」的正向断言的等
// 待上限,超时即失败并报告疑似死锁(命名与约定对照 maa 包 state_test
// .go);同步本身一律经 channel/原子屏障完成,不依赖这段时间。
const hangGuard = 5 * time.Second

// registryTestToken 是本文件 install/remove panic 用例使用的固定 token
// 常量(与 callbackfixture 的固定 token 常量同一接缝:生产 token 由生
// 成器铸造,直写登记用本文件内选定的常量)。子进程夹具常量不会出现在
// 本测试二进制的注册表中,取值互不重叠。
const (
	registryTestToken       = uintptr(0x0CEA11)
	registryTestGhostToken  = uintptr(0x5EEDC0) // 从未注册:unknown-token 用例
	registryTestGhostToken2 = uintptr(0x5EEDC1) // 从未注册:remove panic 用例
)

// assertPanics 断言 f 按契约 panic(对照 maa 包 state_test.go 同名辅助
// 的先例;本包此前无此辅助,注册表把重复登记与未登记移除声明为编程
// 错误 panic,不允许静默继续)。
func assertPanics(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Errorf("%s must panic as required by the contract", what)
		}
	}()
	f()
}

// saveFaultSlot 保存边界故障原子槽并在用例结束时恢复:lastCallbackFault
// 是进程级共享的「最近一次故障」槽,写入故障的用例不得把记录泄漏给同
// 包其他用例(callback fixture 在子进程内检查该槽,父进程内只有本文件
// 写入)。
func saveFaultSlot(t *testing.T) {
	t.Helper()
	prev := lastCallbackFault.Load()
	t.Cleanup(func() { lastCallbackFault.Store(prev) })
}

// cbTestCBytes 构造 NUL 结尾的 Go 侧字节缓冲并返回首元素指针:模拟
// 原生持有的 const char* 借用视图(只在本次调用内有效)。分发序自身
// 不读取缓冲内容,缓冲是否带 NUL 由用例按需选择。
func cbTestCBytes(payload string) *byte {
	buf := append([]byte(payload), 0)
	return &buf[0]
}

// cbTestRawBytes 构造不带 NUL 的 Go 侧字节缓冲并返回首元素指针:供
// 「扫描范围内无 NUL」的溢出边界用例使用。
func cbTestRawBytes(payload string) *byte {
	buf := []byte(payload)
	return &buf[0]
}

// cbDeliveryRecord 是捕获投递函数记录的一次投递:msg/token 与
// detailsJSON 指针值(分发序契约只保证指针原样透传,本记录不解引用)。
type cbDeliveryRecord struct {
	msg   int32
	ptr   *byte
	token uintptr
}

// cbDeliveryRecorder 是互斥保护的捕获投递函数:登记每次投递的精确参
// 数,供分发序用例逐项断言;投递可能来自用例 goroutine,计数与副本
// 读取均经锁保护。
type cbDeliveryRecorder struct {
	mu    sync.Mutex
	calls []cbDeliveryRecord
}

// deliver 实现 CallbackDelivery:登记参数,不读取 detailsJSON 内容。
func (r *cbDeliveryRecorder) deliver(msg int32, detailsJSON *byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, cbDeliveryRecord{msg: msg, ptr: detailsJSON})
}

// snapshot 返回已记录投递的独立副本。
func (r *cbDeliveryRecorder) snapshot() []cbDeliveryRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]cbDeliveryRecord(nil), r.calls...)
}

// mustRegister 经生产注册入口铸造 token 并要求成功:token 非零是注册
// 契约的一部分(token 0 不会被接受),失败直接终止用例。
func mustRegister(t *testing.T, deliver CallbackDelivery) uintptr {
	t.Helper()
	token, err := RegisterCallbackTarget(deliver)
	if err != nil {
		t.Fatalf("RegisterCallbackTarget must not fail: %v", err)
	}
	if token == 0 {
		t.Fatalf("registered token = 0, want non-zero (token 0 is never accepted)")
	}
	return token
}

// registryEntryCount 在注册表读锁内读取当前条目数(白盒诊断读取)。
func registryEntryCount() int {
	callbackTargets.mu.RLock()
	defer callbackTargets.mu.RUnlock()
	return len(callbackTargets.targets)
}

// TestTokenMonotonicNeverReused 验证「token 非零、单调且永不
// 复用」():连续注册得到严格递增、互不相同的非零 token;注销后再
// 次注册拿到的 token 仍大于之前全部——注销后的 token 不再发出,原生侧
// 迟到的回调只会得到查表未命中的可见故障,绝不会投递给新实例。
func TestTokenMonotonicNeverReused(t *testing.T) {
	noop := func(int32, *byte) {}

	var registered []uintptr
	removed := make(map[uintptr]bool)
	t.Cleanup(func() {
		for _, token := range registered {
			if !removed[token] {
				UnregisterCallbackTarget(token)
			}
		}
	})
	register := func() uintptr {
		t.Helper()
		token := mustRegister(t, noop)
		registered = append(registered, token)
		return token
	}

	first := register()
	second := register()
	third := register()
	if !(first < second && second < third) {
		t.Fatalf("consecutively registered tokens must strictly increase: %d, %d, %d",
			uint64(first), uint64(second), uint64(third))
	}

	// 注销中间 token 后再注册:新 token 必须仍大于之前全部(永不复用),
	// 而不是回收已注销的值。
	UnregisterCallbackTarget(second)
	removed[second] = true
	again := register()
	if again <= third || again <= second || again <= first {
		t.Fatalf("token registered after unregistration = %d, must be greater than all previous (%d, %d, %d): tokens must not be reused",
			uint64(again), uint64(first), uint64(second), uint64(third))
	}
}

// TestTokenExhaustionRefusedAtWrapBoundary 白盒验证「回绕及耗
// 尽均不会被接受」(耗尽或回绕时拒绝创建):把进程级计数器推到
// ^uintptr(0) 后,RegisterCallbackTarget 返回包装 ErrProtocol 的错误、
// token 为 0、注册表不新增条目;恢复计数器后注册表照常铸造可用 token
// ——拒绝是精确的边界行为,不是注册表整体失效。
func TestTokenExhaustionRefusedAtWrapBoundary(t *testing.T) {
	noop := func(int32, *byte) {}

	// 白盒注入:在生成器锁内保存原值并把 last 推到回绕边界;恢复经
	// Cleanup 兜底(失败路径也要还原共享计数器),成功路径手动恢复后
	// Cleanup 的重复恢复是幂等的。
	callbackTokens.mu.Lock()
	prev := callbackTokens.last
	callbackTokens.last = ^uintptr(0)
	callbackTokens.mu.Unlock()
	restore := func() {
		callbackTokens.mu.Lock()
		callbackTokens.last = prev
		callbackTokens.mu.Unlock()
	}
	t.Cleanup(restore)

	before := registryEntryCount()

	token, err := RegisterCallbackTarget(noop)
	if err == nil {
		t.Fatalf("registration at the counter wrap boundary must be rejected, got token %d", uint64(token))
	}
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("errors.Is(err, ErrProtocol) = false; err = %v", err)
	}
	if errors.Is(err, ErrInvalidInput) {
		t.Errorf("exhaustion rejection must not be mislabeled as ErrInvalidInput: %v", err)
	}
	if token != 0 {
		t.Errorf("rejected registration token = %d, want 0", uint64(token))
	}
	if after := registryEntryCount(); after != before {
		t.Errorf("a rejected registration must not modify the registry: entry count %d -> %d", before, after)
	}

	// 恢复计数器后注册表照常可用:正常铸造、查表命中。
	restore()
	okToken := mustRegister(t, noop)
	t.Cleanup(func() { UnregisterCallbackTarget(okToken) })
	if lookupCallbackTarget(okToken) == nil {
		t.Fatalf("token %d registered after restoring the counter must be found in the registry", uint64(okToken))
	}
}

// TestTokenNotConsumedByNilDelivery 钉住「nil deliver 拒绝且不消耗
// token」(RegisterCallbackTarget 契约):返回包装 ErrInvalidInput 的错
// 误、token 为 0,且下一个成功 token 与之前连续——失败的注册不得在单
// 调序列上留下空洞。
func TestTokenNotConsumedByNilDelivery(t *testing.T) {
	noop := func(int32, *byte) {}

	first := mustRegister(t, noop)
	t.Cleanup(func() { UnregisterCallbackTarget(first) })

	bad, err := RegisterCallbackTarget(nil)
	if err == nil {
		t.Fatalf("registration with a nil deliver must be rejected, got token %d", uint64(bad))
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Errorf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
	}
	if errors.Is(err, ErrProtocol) {
		t.Errorf("nil deliver rejection must not be mislabeled as ErrProtocol: %v", err)
	}
	if bad != 0 {
		t.Errorf("rejected registration token = %d, want 0", uint64(bad))
	}

	next := mustRegister(t, noop)
	t.Cleanup(func() { UnregisterCallbackTarget(next) })
	if next != first+1 {
		t.Fatalf("next token after the nil rejection = %d, want %d (contiguous with the previous; a nil registration consumes no token)",
			uint64(next), uint64(first+1))
	}
}

// TestDispatchCallbackDeliveryOrder 钉住分发序的固定三步(不允许
// 静默丢弃):token 0 与查表未命中分别记 token-zero/unknown-token 的
// 可见边界故障后返回、绝不投递、不 panic(原生线程上的回调不能因接线
// 层错误终止 C 线程);命中的投递收到精确的 (msg, details 指针)——
// 指针按值透传,分发序自身不读取内容。
func TestDispatchCallbackDeliveryOrder(t *testing.T) {
	t.Run("token 0 records token-zero fault, no delivery", func(t *testing.T) {
		saveFaultSlot(t)
		rec := &cbDeliveryRecorder{}
		dispatchCallback(7, cbTestCBytes(`{"t":0}`), 0)

		f := lastCallbackFault.Load()
		if f == nil {
			t.Fatal("token 0 must leave a visible boundary fault; silent drops are not allowed")
		}
		if f.Kind != callbackFaultTokenZero {
			t.Errorf("fault kind = %q, want %q", f.Kind, callbackFaultTokenZero)
		}
		if f.Msg != 7 || f.Token != 0 {
			t.Errorf("fault msg = %d, want 7; token = %d, want 0", f.Msg, uint64(f.Token))
		}
		if f.Detail == "" {
			t.Error("the fault detail must not be empty")
		}
		if got := rec.snapshot(); len(got) != 0 {
			t.Errorf("token 0's message was delivered %d times, want 0 (details pointer must not be read)", len(got))
		}
	})

	t.Run("unregistered token records unknown-token fault, no delivery", func(t *testing.T) {
		saveFaultSlot(t)
		rec := &cbDeliveryRecorder{}
		dispatchCallback(-3, cbTestCBytes(`{}`), registryTestGhostToken)

		f := lastCallbackFault.Load()
		if f == nil {
			t.Fatal("an unregistered token must leave a visible boundary fault; silent drops are not allowed")
		}
		if f.Kind != callbackFaultUnknownToken {
			t.Errorf("fault kind = %q, want %q", f.Kind, callbackFaultUnknownToken)
		}
		if f.Msg != -3 || f.Token != registryTestGhostToken {
			t.Errorf("fault msg = %d, want -3; token = %d, want %d",
				f.Msg, uint64(f.Token), uint64(registryTestGhostToken))
		}
		if got := rec.snapshot(); len(got) != 0 {
			t.Errorf("the unregistered token's message was delivered %d times, want 0", len(got))
		}
	})

	t.Run("hit delivers msg and details pointer exactly", func(t *testing.T) {
		saveFaultSlot(t)
		rec := &cbDeliveryRecorder{}
		token := mustRegister(t, rec.deliver)
		t.Cleanup(func() { UnregisterCallbackTarget(token) })

		// 缓冲故意不带 NUL:分发序契约只透传指针,不读取内容;指针值
		// 相等即透传正确(解引用后比较反而是越界读取)。
		buf := []byte(`{"hit":1}`)
		p := &buf[0]
		const msg = int32(-9)
		dispatchCallback(msg, p, token)

		got := rec.snapshot()
		if len(got) != 1 {
			t.Fatalf("hit delivery count = %d, want 1", len(got))
		}
		if got[0].msg != msg {
			t.Errorf("delivered msg = %d, want %d", got[0].msg, msg)
		}
		if got[0].ptr != p {
			t.Errorf("delivered details pointer = %p, want pass-through %p (pointers pass by value; the dispatch order neither copies nor rewrites)", got[0].ptr, p)
		}
		if f := lastCallbackFault.Load(); f != nil {
			t.Errorf("a hit delivery must not record a boundary fault, got kind=%s", f.Kind)
		}
	})
}

// TestCallbackRegistryLockNotHeldAcrossDelivery 钉住红线「注册表
// 锁不跨投递」:捕获投递函数在投递中途重入注册表(重入查表、经生产
// 入口注册并注销新 token)。查表函数取出函数值后必须已释放读锁,重入
// 操作才能在限时内完成;实现若在投递期间持有任何注册表锁,本用例必然
// 自死锁并由防挂死上限暴露。
func TestCallbackRegistryLockNotHeldAcrossDelivery(t *testing.T) {
	noop := func(int32, *byte) {}

	done := make(chan struct{})
	var token uintptr
	reentrant := func(msg int32, detailsJSON *byte) {
		defer close(done)
		// 重入查表:读锁必须可用。
		if lookupCallbackTarget(token) == nil {
			t.Errorf("reentrant lookup inside delivery missed token %d", uint64(token))
		}
		// 重入登记/移除:写锁必须可用(读锁未跨投递持有)。
		inner, err := RegisterCallbackTarget(noop)
		if err != nil {
			t.Errorf("reentrant registration inside delivery must not fail: %v", err)
			return
		}
		UnregisterCallbackTarget(inner)
	}

	token = mustRegister(t, reentrant)
	t.Cleanup(func() { UnregisterCallbackTarget(token) })

	go dispatchCallback(11, cbTestCBytes(`{}`), token)
	select {
	case <-done:
	case <-time.After(hangGuard):
		t.Fatal("registry reentrancy inside delivery did not finish within the hang guard: the implementation appears to hold the registry lock during delivery (self-deadlock)")
	}
}

// TestCallbackRegistryInFlightDeliverySurvivesUnregister 钉住第 4
// 步竞态的注册表侧:注册表持有投递函数(及其捕获状态的强引用)直至
// 显式注销,已开始的投递不因注销失败、捕获状态在投递期间保持可达。
// 投递在 channel 屏障处暂停,主测试在暂停窗口内执行注销——注销必须
// 立即返回(remove 不等待、注册表锁不在投递期间被持有);放行后投递
// 照常完成,捕获的参数完好。
func TestCallbackRegistryInFlightDeliverySurvivesUnregister(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	completed := make(chan struct{})
	const msg = int32(13)
	var details *byte

	var gotMsg int32
	var gotPtr *byte
	paused := func(m int32, p *byte) {
		close(entered)
		<-release
		gotMsg = m
		gotPtr = p
		close(completed)
	}

	token := mustRegister(t, paused)
	// 失败路径兜底:正常序列在用例中段已显式注销;Once 保证清理不双
	// 重移除,失败时不把泄漏的注册条目带到同包后续用例。
	var unregistered sync.Once
	doUnregister := func() { unregistered.Do(func() { UnregisterCallbackTarget(token) }) }
	t.Cleanup(doUnregister)
	buf := []byte(`{"inflight":1}`)
	details = &buf[0]
	go dispatchCallback(msg, details, token)

	<-entered
	// 注销在投递中途发生:remove 不等待在途投递(等待归 maa 层关闭流
	// 程, 第 4 步的前置由准入门与退空承载),必须立即返回。
	removed := make(chan struct{})
	go func() {
		doUnregister()
		close(removed)
	}()
	select {
	case <-removed:
	case <-time.After(hangGuard):
		t.Fatal("UnregisterCallbackTarget did not return within the hang guard: remove appears to be waiting for in-flight deliveries")
	}
	if lookupCallbackTarget(token) != nil {
		t.Fatal("the token is still found in the registry after unregistration")
	}

	// 放行:已开始的投递不因注销失败,捕获状态在投递期间保持可达。
	close(release)
	select {
	case <-completed:
	case <-time.After(hangGuard):
		t.Fatal("released after unregistration, the in-flight delivery did not finish within the hang guard")
	}
	if gotMsg != msg || gotPtr != details {
		t.Errorf("in-flight delivery captured msg = %d, want %d; pointer = %p, want %p (strong references keep the arguments intact)",
			gotMsg, msg, gotPtr, details)
	}
}

// TestCallbackRegistryPauseSeamBetweenLookupAndDeliver 验证测试暂停缝
// (callbackDispatchHookSlot)恰位于「查到 token 之后、投递之前」——
// 关闭流程两类准入竞态的固定注入点:hook 触发(已查到 token、尚未投
// 递)时,主测试并发注销同 token 并翻转 fake 的准入拒绝标志;放行后
// fake 照常被调用(分发序对已命中的投递不复查注册表——注销不阻断已
// 命中投递),但 fake 因拒绝标志未读取 details。生产路径 hook 恒为
// nil,本缝不承载任何生产语义。
func TestCallbackRegistryPauseSeamBetweenLookupAndDeliver(t *testing.T) {
	saveFaultSlot(t)

	var (
		reject      atomic.Bool
		invoked     atomic.Bool
		readDetails atomic.Bool
		hookMatched atomic.Bool
	)
	deliver := func(msg int32, detailsJSON *byte) {
		invoked.Store(true)
		if reject.Load() {
			// 准入拒绝:立即返回,不读取原生 details(验收红线的
			// 分发侧前提;红线本体由 maa 层 deliver 承载)。
			return
		}
		readDetails.Store(true)
	}
	token := mustRegister(t, deliver)
	// 本用例在暂停窗口内已注销该 token:以 Once 保证清理路径不双重
	// 移除(remove 对未登记 token panic)。
	var unregistered sync.Once
	doUnregister := func() { unregistered.Do(func() { UnregisterCallbackTarget(token) }) }
	t.Cleanup(doUnregister)

	entered := make(chan struct{})
	release := make(chan struct{})
	hook := func(hookToken uintptr) {
		hookMatched.Store(hookToken == token)
		close(entered)
		<-release
	}
	callbackDispatchHookSlot.Store(&hook)
	t.Cleanup(func() { callbackDispatchHookSlot.Store(nil) })

	const msg = int32(17)
	go dispatchCallback(msg, cbTestCBytes(`{}`), token)

	// hook 触发:此刻查表已命中、投递尚未开始。
	<-entered
	if !hookMatched.Load() {
		t.Fatal("the pause seam was called without the matched token")
	}
	// 暂停窗口内并发注销同 token:remove 不被暂停的投递阻塞(注册表
	// 锁不在查表之后持有)。
	removed := make(chan struct{})
	go func() {
		doUnregister()
		close(removed)
	}()
	select {
	case <-removed:
	case <-time.After(hangGuard):
		t.Fatal("unregistration inside the pause window did not return within the hang guard: the registry lock appears to be held across lookup and delivery")
	}
	if lookupCallbackTarget(token) != nil {
		t.Fatal("unregistration inside the pause window did not take effect")
	}
	reject.Store(true)
	close(release)

	deadline := time.Now().Add(hangGuard)
	for !invoked.Load() {
		if time.Now().After(deadline) {
			t.Fatal("after release the delivery did not reach the fake within the hang guard: the pause seam appears to sit outside lookup and delivery")
		}
		time.Sleep(time.Millisecond)
	}
	// 分发序照常调用已命中的 fake(不复查注册表),但拒绝标志使其未
	// 读取 details——证明 hook 恰位于查表与投递之间。
	if readDetails.Load() {
		t.Error("the reject flag is set; the fake must not read details")
	}
	if f := lastCallbackFault.Load(); f != nil {
		t.Errorf("a matched delivery must not record a boundary fault, got kind=%s", f.Kind)
	}
}

// TestCallbackRegistryInstallRemovePanics 逐条钉住注册表的接线层编程
// 错误 panic(本文件头:token 永不复用,重复登记即内部不变量破坏;未
// 登记或已移除的 token 移除即接线错误):重复 install、未登记 remove
// 与双重 remove 都必须 panic,不允许静默继续污染注册表。
func TestCallbackRegistryInstallRemovePanics(t *testing.T) {
	noop := func(int32, *byte) {}
	t.Cleanup(func() {
		// 失败路径兜底:若断言中断在已登记状态,恢复注册表共享状态。
		if callbackTargets.lookup(registryTestToken) != nil {
			callbackTargets.remove(registryTestToken)
		}
	})

	callbackTargets.install(registryTestToken, noop)
	assertPanics(t, "duplicate install of a registered token", func() {
		callbackTargets.install(registryTestToken, noop)
	})

	callbackTargets.remove(registryTestToken)
	assertPanics(t, "duplicate remove of a removed token", func() {
		callbackTargets.remove(registryTestToken)
	})
	assertPanics(t, "remove of a never-registered token", func() {
		callbackTargets.remove(registryTestGhostToken2)
	})
}

// TestCallbackDetailsLengthBounds 钉住有界寻找 NUL 的冻结边界(
// 在下标 0..maxBytes 含端点内寻找,恰好 CallbackBytes 字节后是 NUL 合
// 法,超过则按溢出处理;nil 缓冲返回未找到,由调用方按 ErrProtocol 处
// 理):整段无 NUL 返回 (0, false),不投递截断 JSON 的决定归调用方。
func TestCallbackDetailsLengthBounds(t *testing.T) {
	t.Run("nil buffer returns not found", func(t *testing.T) {
		if length, found := CallbackDetailsLength(nil, 1024); found || length != 0 {
			t.Fatalf("CallbackDetailsLength(nil, 1024) = (%d, %v), want (0, false)", length, found)
		}
	})

	t.Run("mid-buffer NUL returns its index", func(t *testing.T) {
		if length, found := CallbackDetailsLength(cbTestCBytes("abc"), 1024); !found || length != 3 {
			t.Fatalf("CallbackDetailsLength = (%d, %v), want (3, true)", length, found)
		}
	})

	t.Run("NUL exactly at the maxBytes index is valid", func(t *testing.T) {
		//  冻结边界:扫描范围含端点 maxBytes,恰好 CallbackBytes
		// 字节后是 NUL 合法(9 字节缓冲、maxBytes=8)。
		if length, found := CallbackDetailsLength(cbTestCBytes("12345678"), 8); !found || length != 8 {
			t.Fatalf("CallbackDetailsLength = (%d, %v), want (8, true)", length, found)
		}
	})

	t.Run("no NUL within maxBytes returns not found", func(t *testing.T) {
		// 9 字节全非 NUL、maxBytes=8:扫描 0..8 无 NUL(唯一的 NUL 在
		// 下标 9,界外)→ (0, false)。
		if length, found := CallbackDetailsLength(cbTestRawBytes("123456789"), 8); found || length != 0 {
			t.Fatalf("CallbackDetailsLength = (%d, %v), want (0, false)", length, found)
		}
	})

	t.Run("maxBytes 0 only inspects the first byte", func(t *testing.T) {
		if length, found := CallbackDetailsLength(cbTestCBytes(""), 0); !found || length != 0 {
			t.Fatalf("first byte is NUL: CallbackDetailsLength = (%d, %v), want (0, true)", length, found)
		}
		if _, found := CallbackDetailsLength(cbTestRawBytes("x"), 0); found {
			t.Fatalf("first byte is not NUL: found = true, want false (no scan room when maxBytes=0)")
		}
	})
}

// TestCallbackDetailsCopySemantics 钉住精确复制的所有权与纪律(
// 原始字节复制,不做任何解析;副本与源独立):精确复制 length 字节、
// length 0 不读取任何原生字节、负长度按接线层编程错误 panic、复制后
// 改写源缓冲不影响副本。
func TestCallbackDetailsCopySemantics(t *testing.T) {
	t.Run("copies exactly length bytes", func(t *testing.T) {
		src := cbTestCBytes(`{"k":9007199254740993}`)
		got := CopyCallbackDetails(src, len(`{"k":9007199254740993}`))
		if string(got) != `{"k":9007199254740993}` {
			t.Fatalf("copy = %q, want the exact payload (without NUL)", got)
		}
	})

	t.Run("length 0 reads no source buffer", func(t *testing.T) {
		// 空消息(下标 0 即 NUL)与 nil 指针都必须安全返回空字节串:
		// length 0 时不发生任何原生字节读取。
		if got := CopyCallbackDetails(cbTestCBytes(""), 0); len(got) != 0 {
			t.Fatalf("copy length at length 0 = %d, want 0", len(got))
		}
		if got := CopyCallbackDetails(nil, 0); len(got) != 0 {
			t.Fatalf("copy length with a nil source and length 0 = %d, want 0", len(got))
		}
	})

	t.Run("negative length panics", func(t *testing.T) {
		assertPanics(t, "CopyCallbackDetails with a negative length", func() {
			CopyCallbackDetails(cbTestCBytes("abc"), -1)
		})
	})

	t.Run("copy is independent of the source", func(t *testing.T) {
		payload := "independent"
		buf := append([]byte(payload), 0)
		src := &buf[0]
		got := CopyCallbackDetails(src, len(payload))
		// 改写源缓冲(模拟原生在 callback 返回后改写 details):副本
		// 是独立 Go 数据,不受影响。
		for i := range buf[:len(payload)] {
			buf[i] = 'X'
		}
		if string(got) != payload {
			t.Fatalf("copy after rewriting the source = %q, want unchanged %q (copy and source do not share the underlying buffer)", got, payload)
		}
	})
}
