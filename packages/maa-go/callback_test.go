// 本文件是实例 callback 准入接缝的受控用例(MAA-18;绑定设计 §4、§7、
// §7.1、§8):白盒同包验证 callbackTarget.deliver 的固定准入序——正常
// 路径经分发器到达 Events 且在途 lease 归零、查到 token 但未获
// callback lease 的路径绝不读取原生 details(§4 验收红线,nil 探针 +
// 暂停屏障竞态 A)、已准入 callback 结束前状态可达且关门等待退空后才
// 允许移除 token(§8 第 4 步,竞态 B:真实注册 maa→native 接缝并按顺
// 序钉死注销时点)、closing 阶段可接收必要消息且不获取 raw 串行门禁
// (§7)、回调内 Go panic 转为实例 sticky fault 而不越过 native 边界
// (§4)、单条 CallbackBytes 超限与 §7.1 冻结边界、disp.admit 溢出契
// 约不经 deliver 额外计丢失、空 details 判内容归 dispatcher,以及构
// 造与并发准入纪律。
//
// 全部用例以受控屏障(channel/WaitGroup/原子标志)确定性同步,不加载
// 任何原生库、不触达 loader/Runtime,CGO_ENABLED=0 可跑;防挂死只用
// state_test.go 的 hangGuard 兜底,不作同步手段。maa→native 的注册表
// 是进程级共享状态:本文件用例互不并行(不调 t.Parallel),注册的
// token 由用例自己注销,dispatcher/goroutine 收尾经 closeAndDrain/
// mustDrain 或防挂死兜底排空,不留悬挂 goroutine 影响 -race。

package maa

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// callbackCBytes 构造 NUL 结尾的 Go 侧 details 缓冲并返回首元素指针:
// 模拟原生持有的 const char* 借用视图(只在本次调用内有效);deliver
// 在 callback lease 覆盖窗口内有界读取并复制它。
func callbackCBytes(payload string) *byte {
	buf := append([]byte(payload), 0)
	return &buf[0]
}

// inFlightCallbacks 在状态锁内读取已准入、未退出的 callback 计数(白
// 盒诊断读取,不绕过锁;对照 inFlightCalls 先例)。
func inFlightCallbacks(s *instanceState) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.callbacks
}

// newCallbackRig 构造已启动分发循环的 callback 准入接缝:状态发布
// ready、分发器携带同一预算快照启动、target 挂接同一状态与分发器。
// settle 恒为 nil(本单元不关心结算;需要阻塞结算的用例自行构造)。
// 用例收尾负责 closeAndDrain(intake 已关时 mustDrain)排空分发循环。
func newCallbackRig(t *testing.T, b dispatchBudgets) (*callbackTarget, *instanceState, *eventDispatcher) {
	t.Helper()
	d, s := newTestDispatcher(t, nil, b)
	d.start()
	return newCallbackTarget(s, d, b.callbackBytes), s, d
}

// TestCallbackDeliverHappyPath 钉住准入序的正常路径(§4、§7.1:取得
// lease → 有界定长 → 预留额度 → 复制 → 入队,全程在 callback 返回之
// 前):deliver 返回后事件已按序号到达公开 Events,原文逐字节一致;
// 无故障、零丢失、在途 callback 归零(lease 在 deliver 返回前释放)。
func TestCallbackDeliverHappyPath(t *testing.T) {
	target, s, d := newCallbackRig(t, generousBudgets())
	defer closeAndDrain(t, d)

	const msg = int32(42)
	payload := `{"what":"HappyPath","big":9007199254740993}`
	target.deliver(msg, callbackCBytes(payload))

	ev := mustReadEvent(t, d.events())
	if ev.MessageID != MessageID(msg) {
		t.Fatalf("事件 MessageID = %d, want %d", ev.MessageID, msg)
	}
	if string(ev.JSON) != payload {
		t.Fatalf("事件原文 = %s, want 逐字节相等的 %s", ev.JSON, payload)
	}
	if ev.Sequence != 1 {
		t.Fatalf("事件序号 = %d, want 1", ev.Sequence)
	}
	select {
	case <-s.faultNotify():
		t.Fatalf("正常投递不得触发故障,得到首因 %v", s.currentFault())
	default:
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("丢失计数 = %d, want 0", lost)
	}
	if n := inFlightCallbacks(s); n != 0 {
		t.Errorf("在途 callback = %d, want 0(lease 在 deliver 返回前释放)", n)
	}
}

// TestCallbackDeliverGateClosedSkipsNativeDetails 钉住 §4 验收红线
// 「查到 token 但未获 callback lease 的路径不读取原生 details」:关门
// 与投递经暂停屏障定序(竞态 A 的 deliver 侧等价前提),关门后到达的
// deliver 以 nil details 作探针——路径若错误地读取原生 details,会按
// ErrProtocol 记故障;断言零故障、零丢失、零在途、零事件。对照子用例
// 证明同一探针在门开时必然记录 ErrProtocol,排除探针失效的假通过。
func TestCallbackDeliverGateClosedSkipsNativeDetails(t *testing.T) {
	t.Run("关门后未获 lease 的投递不读取原生 details", func(t *testing.T) {
		target, s, d := newCallbackRig(t, generousBudgets())

		started := make(chan struct{})
		release := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			// 宣告「已查到 token」的等价前提(dispatchCallback 查表
			// 在先,deliver 在后;此处以屏障钉住两者的先后),等待主
			// 测试关门后放行。
			close(started)
			<-release
			target.deliver(48, nil)
		}()
		<-started
		s.closeCallbackGate()
		close(release)
		select {
		case <-done:
		case <-time.After(hangGuard):
			t.Fatal("未获 lease 的 deliver 未在防挂死上限内返回")
		}

		select {
		case <-s.faultNotify():
			t.Fatalf("未获 lease 的路径不得记录故障(nil 探针被读取),得到首因 %v", s.currentFault())
		default:
		}
		if s.currentFault() != nil {
			t.Errorf("首因 = %v, want nil", s.currentFault())
		}
		if lost := s.currentLostEvents(); lost != 0 {
			t.Errorf("丢失计数 = %d, want 0(消息未获准入,不计丢失)", lost)
		}
		if n := inFlightCallbacks(s); n != 0 {
			t.Errorf("在途 callback = %d, want 0", n)
		}
		// 无事件入队:排空后公开通道关闭且为空。
		closeAndDrain(t, d)
		if n := len(d.events()); n != 0 {
			t.Errorf("公开缓冲滞留事件 = %d, want 0", n)
		}
		if _, ok := <-d.events(); ok {
			t.Fatal("未获 lease 的路径不得产生事件")
		}
	})

	t.Run("门开时 nil details 记 ErrProtocol(探针对照)", func(t *testing.T) {
		target, s, d := newCallbackRig(t, generousBudgets())
		defer closeAndDrain(t, d)

		target.deliver(49, nil)
		fault := s.currentFault()
		if fault == nil {
			t.Fatal("门开时 nil details 必须记录 ErrProtocol 故障,否则探针无效")
		}
		if !errors.Is(fault, native.ErrProtocol) {
			t.Errorf("errors.Is(首因, native.ErrProtocol) = false; 首因 = %v", fault)
		}
		if errors.Is(fault, native.ErrEventOverflow) {
			t.Errorf("nil details 故障不得误标为 ErrEventOverflow: %v", fault)
		}
		if !strings.Contains(fault.Error(), fmt.Sprintf("message id %d", 49)) {
			t.Errorf("故障文本应携带 msg:\n%s", fault.Error())
		}
		if lost := s.currentLostEvents(); lost != 1 {
			t.Errorf("丢失计数 = %d, want 1(空指针消息未能交付)", lost)
		}
	})
}

// TestCallbackDeliverTokenRemovedAfterGateCloseAndDrain 钉住 §8 第 4
// 步的两条契约(暂停屏障竞态 B):已准入 callback 结束前状态保持可达
// (关门等待不收敛);准入关闭并等待全部已准入 callback 退出之后,才
// 允许移除 token。用例经真实注册入口 RegisterCallbackTarget 注册
// target.deliver(顺带钉住 maa→native 的注册接缝形态),投递 goroutine
// 在主测试持有分发器锁后到达 admit——此刻已持有 callback lease、暂停
// 在分发器锁上;关门后 waitCallbacksDrained 必须不收敛,放行后退空收
// 敛,此后才执行 native.UnregisterCallbackTarget。
func TestCallbackDeliverTokenRemovedAfterGateCloseAndDrain(t *testing.T) {
	target, s, d := newCallbackRig(t, generousBudgets())

	const msg = int32(47)
	payload := `{"what":"InFlight","async_call_id":9,"details":{"ret":true}}`
	token, err := native.RegisterCallbackTarget(target.deliver)
	if err != nil {
		t.Fatalf("经生产入口注册 target.deliver 不得失败: %v", err)
	}
	var unregistered sync.Once
	doUnregister := func() { unregistered.Do(func() { native.UnregisterCallbackTarget(token) }) }
	// 失败路径兜底:正常序列在退空后才显式注销;Once 保证不双重移除。
	defer doUnregister()

	started := make(chan struct{})
	dispLocked := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		close(started) // 宣告即将投递(token 前提由注册表命中保证)
		<-dispLocked   // 主测试已持分发器锁:deliver 必暂停在 admit 的锁上
		target.deliver(msg, callbackCBytes(payload))
	}()
	<-started

	// 持分发器锁制造暂停:admitCallback(状态锁)不受影响,deliver 在
	// 取得 lease 之后、预留队列额度之前停住。
	target.disp.mu.Lock()
	close(dispLocked)
	admitted := false
	deadline := time.Now().Add(hangGuard)
	for time.Now().Before(deadline) {
		if inFlightCallbacks(s) == 1 {
			admitted = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !admitted {
		target.disp.mu.Unlock()
		t.Fatal("deliver 未在防挂死上限内取得 callback lease(在途计数未达 1)")
	}

	// 关门:已准入的这条不受影响;随后退空等待必须被它挡住。
	s.closeCallbackGate()
	drained := make(chan struct{})
	go func() {
		s.waitCallbacksDrained()
		close(drained)
	}()
	// 负面断言:先等满观察窗,再在检查点确认等待仍未收敛——提前收敛
	// (缺陷)发生在窗口内任意时刻都会被确定性观察到,避免 select 双就
	// 绪时随机选择漏报「提前收敛」这一被测缺陷本身。
	<-time.After(notWithin)
	select {
	case <-drained:
		target.disp.mu.Unlock()
		t.Fatal("已准入 callback 未结束时 waitCallbacksDrained 提前返回:关闭等待漏计在途 callback")
	default:
	}

	// 放行 admit:复制、入队、退出 lease,关闭等待随之收敛。
	target.disp.mu.Unlock()
	select {
	case <-drained:
	case <-time.After(hangGuard):
		t.Fatal("放行后 waitCallbacksDrained 未在防挂死上限内收敛")
	}

	// §8 第 4 步顺序钉死:准入已关闭且全部已准入 callback 退出之后,
	// 才允许移除 token(此前的任何提前移除都让本时序失序)。
	doUnregister()

	ev := mustReadEvent(t, d.events())
	if ev.MessageID != MessageID(msg) {
		t.Fatalf("事件 MessageID = %d, want %d", ev.MessageID, msg)
	}
	if string(ev.JSON) != payload {
		t.Fatalf("事件原文 = %s, want %s", ev.JSON, payload)
	}
	select {
	case <-s.faultNotify():
		t.Fatalf("正常准入与退空不得触发故障,得到首因 %v", s.currentFault())
	default:
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("丢失计数 = %d, want 0", lost)
	}
	if n := inFlightCallbacks(s); n != 0 {
		t.Errorf("在途 callback = %d, want 0", n)
	}
	closeAndDrain(t, d)
}

// TestCallbackDeliverClosingIgnoresRawGate 钉住 §7 验收条件「closing
// 阶段可接收必要消息及同步 Destroyed,不获取 raw 串行门禁」:选主转
// closing 后准入门仍开放,deliver 照常入队;主测试先行占据 raw 串行
// 门禁再投递——callback 全程不被 raw 门禁阻塞(deliver 若错误地获取
// raw 门禁将与本测试互等死锁,由防挂死上限暴露),事件照常到达。
func TestCallbackDeliverClosingIgnoresRawGate(t *testing.T) {
	target, s, d := newCallbackRig(t, generousBudgets())
	defer closeAndDrain(t, d)

	mustElectClose(t, s)
	const msg = int32(50)
	payload := `{"what":"ClosingMessage"}`

	s.raw.enter()
	defer s.raw.exit()
	done := make(chan struct{})
	go func() {
		defer close(done)
		target.deliver(msg, callbackCBytes(payload))
	}()
	select {
	case <-done:
	case <-time.After(hangGuard):
		t.Fatal("closing 阶段的 deliver 未在防挂死上限内返回:callback 疑似获取了 raw 串行门禁")
	}

	ev := mustReadEvent(t, d.events())
	if ev.MessageID != MessageID(msg) {
		t.Fatalf("事件 MessageID = %d, want %d", ev.MessageID, msg)
	}
	if string(ev.JSON) != payload {
		t.Fatalf("事件原文 = %s, want %s", ev.JSON, payload)
	}
	select {
	case <-s.faultNotify():
		t.Fatalf("closing 阶段的必要消息入队不得触发故障,得到首因 %v", s.currentFault())
	default:
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("丢失计数 = %d, want 0", lost)
	}
	if n := inFlightCallbacks(s); n != 0 {
		t.Errorf("在途 callback = %d, want 0", n)
	}
}

// TestCallbackDeliverPanicBecomesStickyFault 钉住 §4 验收条件「回调 Go
// panic 转成可观察故障且不越过 native 边界」:closeIntake 制造 admit
// 的编程错误 panic 路径,deliver 顶栏 recover 后转为实例 sticky fault
// (包装 native.ErrProtocol、携带 msg、无原文前缀);defer LIFO 保证
// lease 先释放——已准入 callback 在任何阶段都必须能正常退出(§7.1),
// 否则关闭等待无法收敛。再次 deliver(门仍开):admitCallback 与阶段
// 无关照常成功,重复故障不覆盖首因。
func TestCallbackDeliverPanicBecomesStickyFault(t *testing.T) {
	target, s, d := newCallbackRig(t, generousBudgets())
	// intake 已关:收尾只排空分发循环(drain 的前置契约由此满足)。
	d.closeIntake()
	defer mustDrain(t, d)

	const msg = int32(51)
	target.deliver(msg, callbackCBytes(`{"k":1}`))

	first := s.currentFault()
	if first == nil {
		t.Fatal("deliver 内 panic 必须转为实例故障,不允许静默继续")
	}
	if !errors.Is(first, native.ErrProtocol) {
		t.Errorf("errors.Is(首因, native.ErrProtocol) = false; 首因 = %v", first)
	}
	if errors.Is(first, native.ErrEventOverflow) {
		t.Errorf("panic 故障不得误标为 ErrEventOverflow: %v", first)
	}
	if got := s.currentPhase(); got != phaseFaulted {
		t.Fatalf("panic 后阶段 = %d, want faulted", got)
	}
	if n := inFlightCallbacks(s); n != 0 {
		t.Fatalf("panic 后在途 callback = %d, want 0(LIFO:lease 先释放再记故障)", n)
	}
	if !strings.Contains(first.Error(), fmt.Sprintf("message id %d", msg)) {
		t.Errorf("故障文本应携带 msg:\n%s", first.Error())
	}
	if snap := s.faultDiagnostics(); snap.rawPrefix != nil {
		t.Errorf("panic 故障经 recordFault 记录,不得携带原文前缀,得到 %q", snap.rawPrefix)
	}

	// 门与阶段无关:gate 仍开,admitCallback 照常成功;panic 后的
	// recordFault 已 sticky——首因保持原值不变。
	target.deliver(msg, callbackCBytes(`{"k":2}`))
	if s.currentFault() != first {
		t.Fatalf("重复 panic 后首因被覆盖: %v, want 保持 %v", s.currentFault(), first)
	}
}

// TestCallbackDeliverSingleMessageOverflow 钉住单条容量契约(§7.1):
// 扫描范围 callbackBytes+1 内无 NUL 以 ErrEventOverflow 故障、计 1 条
// 丢失、不投递截断 JSON;恰好 CallbackBytes 字节后是 NUL 合法——
// Event 只携带不含 NUL 的界内原文。
func TestCallbackDeliverSingleMessageOverflow(t *testing.T) {
	t.Run("单条超限拒绝且不投递截断 JSON", func(t *testing.T) {
		b := generousBudgets()
		b.callbackBytes = 8
		target, s, d := newCallbackRig(t, b)

		// 9 字节 payload、NUL 在下标 9(扫描范围 0..8 之外)。
		target.deliver(52, callbackCBytes("123456789"))

		assertOverflowClass(t, s.currentFault())
		if lost := s.currentLostEvents(); lost != 1 {
			t.Errorf("丢失计数 = %d, want 1", lost)
		}
		if n := inFlightCallbacks(s); n != 0 {
			t.Errorf("在途 callback = %d, want 0", n)
		}
		// 无事件入队:排空后公开通道关闭且为空(本子用例在用例体内
		// 排空以断言空通道,不再 defer 重复关闭)。
		closeAndDrain(t, d)
		if n := len(d.events()); n != 0 {
			t.Errorf("公开缓冲滞留事件 = %d, want 0(截断 JSON 不得投递)", n)
		}
		if _, ok := <-d.events(); ok {
			t.Fatal("超限消息不得产生事件")
		}
	})

	t.Run("恰 CallbackBytes 字节后是 NUL 合法", func(t *testing.T) {
		b := generousBudgets()
		b.callbackBytes = 8
		target, s, d := newCallbackRig(t, b)
		defer closeAndDrain(t, d)

		// §7.1 冻结边界:8 字节 payload、NUL 恰在下标 8(扫描范围含
		// 端点)→ length 8 界内,正常入队。
		const payload = "12345678"
		target.deliver(53, callbackCBytes(payload))

		ev := mustReadEvent(t, d.events())
		if string(ev.JSON) != payload {
			t.Fatalf("事件原文 = %q, want 恰 %d 字节的 %q(NUL 不入 payload)", ev.JSON, len(payload), payload)
		}
		if ev.MessageID != MessageID(53) {
			t.Errorf("事件 MessageID = %d, want 53", ev.MessageID)
		}
		select {
		case <-s.faultNotify():
			t.Fatalf("边界合法的消息不得触发故障,得到首因 %v", s.currentFault())
		default:
		}
		if lost := s.currentLostEvents(); lost != 0 {
			t.Errorf("丢失计数 = %d, want 0", lost)
		}
	})
}

// TestCallbackDeliverAdmitOverflowContract 钉住「disp.admit 溢出按契
// 约上报、deliver 自身不额外计丢失」(§7.1):internalEvents=1 时,被
// 阻塞在结算通知里的消息持续占用唯一内部额度,后续 admit 必然拒绝;
// 拒绝的上报(故障 + 1 条丢失)全部由 admit 内部完成。公开缓冲预先滞
// 留 1 条未读事件,放行后被阻塞的消息也无法投递公开缓冲——再按公开溢
// 出契约计 1 条丢失,合计 2 条、首因保持 admit 溢出;全部丢失都由
// dispatch.go 语义承载。
func TestCallbackDeliverAdmitOverflowContract(t *testing.T) {
	b := generousBudgets()
	b.internalEvents = 1
	b.publicEvents = 1

	// 阻塞式结算:被阻塞的消息在结算点持续持有内部预留(对照
	// dispatch_test 的「先结算后投递」屏障先例)。
	entered := make(chan struct{})
	release := make(chan struct{})
	settle := func(completionInfo) {
		entered <- struct{}{}
		<-release
	}
	s := newInstanceState(0x0D17000, 128, 1_048_576)
	s.publishReady()
	d := newEventDispatcher(s, settle, b.callbackBytes, b.internalEvents, b.internalBytes, b.publicEvents, b.publicBytes)
	d.start()
	target := newCallbackTarget(s, d, b.callbackBytes)
	defer closeAndDrain(t, d)

	// 第 1 条:预先滞留公开缓冲(消费者停读),等它处理完成——公开缓
	// 冲滞留恰 1 条且内部预算清零(在途项已释放),后续 admit 才能确
	// 定性地成功。
	const p0Msg = int32(500)
	target.deliver(p0Msg, callbackCBytes(`{"p0":1}`))
	deadline := time.Now().Add(hangGuard)
	for {
		count, _ := internalBudgetHold(d)
		if len(d.events()) == 1 && count == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("第 1 条未在防挂死上限内滞留公开缓冲并释放内部预算")
		}
		time.Sleep(time.Millisecond)
	}

	// 第 2 条(合法 AsyncCallInfo):admit 成功,dispatcher 阻塞在结
	// 算通知,持续占用唯一内部额度。
	holdMsg := int32(asyncCallInfoMessageID)
	holdPayload := `{"what":"Held","async_call_id":3,"details":{"ret":true}}`
	target.deliver(holdMsg, callbackCBytes(holdPayload))
	select {
	case <-entered:
	case <-time.After(hangGuard):
		t.Fatal("第 2 条未在防挂死上限内进入结算通知:dispatcher 疑似未运行或未取件")
	}

	// 第 3 条:admit 溢出——deliver 除 admit 内部上报外不额外计丢失、
	// 不预留任何额度。
	target.deliver(501, callbackCBytes(`{"p3":1}`))
	if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
		t.Fatalf("首因 = %v, want 含 native.ErrEventOverflow", s.currentFault())
	}
	if errors.Is(s.currentFault(), native.ErrProtocol) {
		t.Errorf("admit 溢出不得误标为 ErrProtocol: %v", s.currentFault())
	}
	if lost := s.currentLostEvents(); lost != 1 {
		t.Fatalf("admit 溢出后丢失计数 = %d, want 1(仅 admit 按溢出契约上报)", lost)
	}

	// 放行结算:被阻塞条目先结算后投递,公开缓冲已满 → 按公开溢出契
	// 约再计 1 条丢失;首因 sticky 保持 admit 溢出。
	close(release)
	deadline = time.Now().Add(hangGuard)
	for {
		if s.currentLostEvents() == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("公开溢出未在防挂死上限内计入丢失,当前丢失计数 = %d", s.currentLostEvents())
		}
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(s.currentFault(), native.ErrEventOverflow) {
		t.Errorf("放行后首因 = %v, want 仍含 native.ErrEventOverflow(sticky)", s.currentFault())
	}

	// 公开缓冲仍只有滞留的第 1 条:溢出的两条都没有成为事件。
	if n := len(d.events()); n != 1 {
		t.Fatalf("公开缓冲滞留事件 = %d, want 1(仅预先滞留的第 1 条)", n)
	}
	ev := mustReadEvent(t, d.events())
	if ev.MessageID != MessageID(p0Msg) || string(ev.JSON) != `{"p0":1}` {
		t.Fatalf("滞留事件 = (MessageID %d, %s), want (500, {\"p0\":1})", ev.MessageID, ev.JSON)
	}
	if n := inFlightCallbacks(s); n != 0 {
		t.Errorf("在途 callback = %d, want 0", n)
	}
}

// TestCallbackDeliverEmptyDetails 钉住空 details(下标 0 即 NUL)的所
// 有权边界(§7.1):callback 层只定长与复制、不判内容——length 0 照常
// 入队;空原文按公开预算交付为 len(JSON)==0 的 Event,非法 JSON 的
// ErrProtocol 故障由 dispatcher 承载(故障文本可辨),交付成功不计丢
// 失。
func TestCallbackDeliverEmptyDetails(t *testing.T) {
	target, s, d := newCallbackRig(t, generousBudgets())
	defer closeAndDrain(t, d)

	target.deliver(54, callbackCBytes(""))

	ev := mustReadEvent(t, d.events())
	if ev.MessageID != MessageID(54) {
		t.Fatalf("事件 MessageID = %d, want 54", ev.MessageID)
	}
	if len(ev.JSON) != 0 {
		t.Fatalf("空 details 的 Event JSON 长度 = %d, want 0", len(ev.JSON))
	}
	fault := s.currentFault()
	if fault == nil {
		t.Fatal("空原文必须由 dispatcher 按非法 JSON 报协议故障")
	}
	if !errors.Is(fault, native.ErrProtocol) {
		t.Errorf("errors.Is(首因, native.ErrProtocol) = false; 首因 = %v", fault)
	}
	if !strings.Contains(fault.Error(), "not valid UTF-8/JSON") {
		t.Errorf("协议故障应来自 dispatcher 的内容检查而非 callback 准入层:\n%s", fault.Error())
	}
	// 原文(空)仍按公开预算交付:不丢失。
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("丢失计数 = %d, want 0(空原文已完整交付)", lost)
	}
	if n := inFlightCallbacks(s); n != 0 {
		t.Errorf("在途 callback = %d, want 0", n)
	}
}

// TestCallbackTargetNilArgumentsPanic 钉住构造契约(对照 newEventDispatcher
// 先例):state 与 disp 均不得为 nil——准入与故障上报是核心语义,违反
// 即编程错误 panic,不允许静默构造出不可用的接缝。
func TestCallbackTargetNilArgumentsPanic(t *testing.T) {
	st := newInstanceState(0x0C0A1, 128, 1_048_576)
	d := newEventDispatcher(st, nil, 1024, 64, 4096, 64, 4096)
	assertPanics(t, "nil state 的 newCallbackTarget", func() {
		newCallbackTarget(nil, d, 1024)
	})
	assertPanics(t, "nil dispatcher 的 newCallbackTarget", func() {
		newCallbackTarget(st, nil, 1024)
	})
}

// TestCallbackDeliverConcurrentAdmission 钉住验收条件「并发 deliver 在
// -race 下干净」(对照 dispatch_test 并发用例):8 个 goroutine 并发
// deliver 400 条,充足预算下全部消息恰好入队一次;单一 dispatcher 串
// 行分配序号并按 FIFO 发送,公开缓冲中的事件序号严格单调递增、从 1
// 开始;无故障、零丢失、在途归零。
func TestCallbackDeliverConcurrentAdmission(t *testing.T) {
	b := generousBudgets()
	b.internalEvents = 1024
	b.internalBytes = 1 << 20
	b.publicEvents = 1024
	b.publicBytes = 1 << 20
	target, s, d := newCallbackRig(t, b)

	const (
		workers   = 8
		perWorker = 50
	)
	want := make(map[string]bool, workers*perWorker)
	for w := 0; w < workers; w++ {
		for i := 0; i < perWorker; i++ {
			want[fmt.Sprintf(`{"w":%d,"i":%d}`, w, i)] = true
		}
	}

	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < perWorker; i++ {
				target.deliver(int32(600)+int32(w), callbackCBytes(fmt.Sprintf(`{"w":%d,"i":%d}`, w, i)))
			}
		}(w)
	}
	wg.Wait()

	if n := inFlightCallbacks(s); n != 0 {
		t.Fatalf("并发投递完成后在途 callback = %d, want 0", n)
	}
	// 预算按 1024 条给足:全部滞留公开缓冲,关门排空不丢失。
	closeAndDrain(t, d)

	if n := len(d.events()); n != workers*perWorker {
		t.Fatalf("公开缓冲滞留事件 = %d, want %d(每条恰好入队一次)", n, workers*perWorker)
	}
	prevSeq := uint64(0)
	for i := 0; i < workers*perWorker; i++ {
		ev := mustReadEvent(t, d.events())
		if ev.Sequence != prevSeq+1 {
			t.Fatalf("第 %d 条事件序号 = %d, want %d(单一 dispatcher 串行分配,FIFO 严格单调)",
				i+1, ev.Sequence, prevSeq+1)
		}
		prevSeq = ev.Sequence
		if !want[string(ev.JSON)] {
			t.Fatalf("意外的第 %d 条事件原文 %s", i+1, ev.JSON)
		}
		delete(want, string(ev.JSON))
	}
	if len(want) != 0 {
		t.Fatalf("缺失 %d 条事件原文(并发复制有丢失)", len(want))
	}
	select {
	case <-s.faultNotify():
		t.Fatalf("充足预算下的并发准入不得产生故障,得到首因 %v", s.currentFault())
	default:
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("丢失计数 = %d, want 0", lost)
	}
}
