// 本文件是综合并发、故障与数据所有权跨组件回归套件:覆盖公开提交 →
// fake native → token 注册表 → callback 准入 → 分发器 → Operation/Events
// 的交叉形态,补充各组件局部用例未组合的竞态。

package maa

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// TestIntegrationCloseWithQueuedAsyncAndSyncDestroyed 验证
// 「已准入排队调用受保护,新调用被拒绝;Destroy 同步 callback 不死锁
// 且只执行一次销毁」与「未取得 callback lease 的线程不读 details」的
// 跨组件组合:异步提交 A 在 raw 门禁段内执行、B 已准入并预留 pending
// 但排队在门禁前,Close 选主后新调用被拒、Stop/Destroy 被两笔在途调
// 用挡住;A、B 退出后照常登记,Destroy 恰一次,Destroy 调用线程经真
// 实注册表查表路径同步投递 Destroyed 不锁死;无完成的提交由关闭尾段
// 以 ErrClosed 结算恰一次、ID 保持可读;关门后迟到的原生线程投递(仍
// 持查表取得的 deliver 值)未获准入、以 nil 探针证明不读 details。
func TestIntegrationCloseWithQueuedAsyncAndSyncDestroyed(t *testing.T) {
	const handle = uintptr(0xB600)
	inst, rig := newAsyncRig(t, handle, nil)
	token := rig.creator.exToken(0)
	s := inst.inst.state
	rig.caller.asyncRets = map[string]int32{"asyncConnect": 1, "asyncClick": 2}

	// 迟到投递探针:在关闭前经真实注册表查表取得 deliver 值(模拟原
	// 生线程持有查表结果跨越整个关闭流程)。
	lateDeliver := native.LookupCallbackDelivery(token)
	if lateDeliver == nil {
		t.Fatal("token not registered in the registry, cannot construct the late delivery probe")
	}

	// A:已准入并在 raw 门禁段内执行(onAsyncConnect 屏障保持其在途)。
	enteredA := make(chan struct{})
	releaseA := make(chan struct{})
	rig.caller.onAsyncConnect = func(uintptr) {
		close(enteredA)
		<-releaseA
	}
	opChA := make(chan *Operation, 1)
	errChA := make(chan error, 1)
	go func() {
		op, err := inst.Connect(connectInput)
		opChA <- op
		errChA <- err
	}()
	<-enteredA

	// B:已准入、已预留 pending 槽位,排队在 raw 门禁之前(A 全程占据
	// 门禁)——预留横跨关闭选主,关闭等待必须覆盖它。
	opChB := make(chan *Operation, 1)
	errChB := make(chan error, 1)
	go func() {
		op, err := inst.Click(1, 2)
		opChB <- op
		errChB <- err
	}()
	waitUntil(t, "both calls admitted", func() bool { return inFlightCalls(s) == 2 })
	// A 在 raw 段内尚未返回(预留未接合)、B 在门禁前已预留:两份预留
	// 都横跨关闭选主,均计入容量占用。
	if n, r := pendingOccupancy(s), pendingReservedOf(s); n != 0 || r != 2 {
		t.Fatalf("pending occupancy while B is queued before the gate = (%d, %d), want (0, 2) (neither reservation registered)", n, r)
	}

	// Destroy 期间同步投递 Destroyed(锁定 Core 的真实形态:Destroy
	// 调用线程 join 后同步触发;此刻 callback 门必须仍开放)。
	destroyedDelivered := make(chan struct{})
	rig.closer.onDestroy = func(uintptr) {
		deliver := native.LookupCallbackDelivery(token)
		if deliver == nil {
			panic("integration: token missing during Destroy")
		}
		deliver(int32(destroyedMessageID), callbackCBytes(`{}`))
		close(destroyedDelivered)
	}

	closeErr := make(chan error, 1)
	go func() { closeErr <- inst.Close() }()
	waitUntil(t, "close finished the election and entered closing", func() bool { return s.currentPhase() == phaseClosing })

	// 新调用被拒绝(closing),且不触达 native。
	before := rig.caller.total()
	if _, err := inst.Click(3, 4); !errors.Is(err, native.ErrClosing) {
		t.Fatalf("new call during closing = %v, want errors.Is native.ErrClosing", err)
	}
	if got := rig.caller.total(); got != before {
		t.Errorf("the call rejected during closing reached native %d → %d times, want unchanged", before, got)
	}

	// 负面观察窗:A、B 均在途(含门禁前的排队者),Stop/Destroy 不得
	// 发生。
	<-time.After(notWithin)
	assertNoStopDestroy(t, rig.closer, "while in-flight calls (including the caller queued before the gate) have not exited")

	// 放行 A:B 转入门禁段并照常登记(先于关闭准入的提交不被撤销)。
	// 两笔提交的登记不由中间状态断言(它们与关闭前进并发,瞬时占用
	// 不可确定性观察):登记未撤销由下文「ID 保持可读的 ErrClosed 结
	// 算」与 pending 归零共同证明。
	close(releaseA)
	opA := <-opChA
	if err := <-errChA; err != nil {
		t.Fatalf("A's Connect = %v, want nil", err)
	}
	opB := <-opChB
	if err := <-errChB; err != nil {
		t.Fatalf("B's Click = %v, want nil (registered as usual during closing)", err)
	}
	if opB.ID() != 2 {
		t.Fatalf("B's submission ID = %d, want 2", opB.ID())
	}

	// 关闭收敛:一次 Stop、一次 Destroy,Destroy 同步 Destroyed 不锁死。
	if err := <-closeErr; err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	assertSingleStopDestroy(t, rig.closer, handle)
	select {
	case <-destroyedDelivered:
	default:
		t.Fatal("the synchronous Destroyed delivery during Destroy did not happen")
	}

	// 观察到 Close 返回后立即读取稳定终态:无完成的提交以 ErrClosed
	// 结算恰一次,ID 保持可读,不依赖额外调度或延时。
	select {
	case <-s.done():
	default:
		t.Error("Done not closed after Close returned")
	}
	if got := s.finalError(); got != nil {
		t.Errorf("final error = %v, want nil (normal Stop/Destroy with no fault)", got)
	}
	for i, op := range []*Operation{opA, opB} {
		res, err := waitOperationGuarded(t, op, context.Background())
		if !errors.Is(err, native.ErrClosed) {
			t.Errorf("wait error of submission %d = %v, want errors.Is native.ErrClosed", i, err)
		}
		if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
			t.Errorf("close settlement of submission %d must carry no result: %+v", i, res)
		}
		if got := op.ID(); got != AsyncID(i+1) {
			t.Errorf("accepted Operation %d ID after close = %d, want kept readable %d", i, got, i+1)
		}
	}
	if n := pendingOccupancy(s); n != 0 {
		t.Errorf("pending occupancy after close drained it = %d, want 0 (capacity reclaimed)", n)
	}

	// Destroyed 事件在关闭后仍可读(缓冲);Events 已关闭。
	ev := mustReadEvent(t, inst.inst.disp.events())
	if ev.MessageID != destroyedMessageID {
		t.Errorf("buffered event MessageID = %d, want Destroyed(%d)", ev.MessageID, destroyedMessageID)
	}
	if _, ok := <-inst.inst.disp.events(); ok {
		t.Error("Events should be closed after Close")
	}

	// 关门后迟到的原生线程投递:未获 callback lease,绝不读取原生
	// details(nil 探针——路径若错误读取会按 ErrProtocol 记故障)。
	lateDeliver(66, nil)
	if s.currentFault() != nil {
		t.Errorf("the unadmitted late delivery must not record a fault (the nil probe was read), got first cause %v", s.currentFault())
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("lost count = %d, want 0 (unadmitted messages are not counted as lost)", lost)
	}
	if n := inFlightCallbacks(s); n != 0 {
		t.Errorf("in-flight callbacks after the late delivery = %d, want 0", n)
	}
}

// TestIntegrationCompletionFaultMatrix 验证「取消或丢弃
// Operation 不泄漏容量;关闭/故障/完成竞争只结算一次;无法关联的完
// 成不能伪造成成功」的混排组合:取消的等待不回收容量、晚到 ret=false
// 结算为原生失败并回收;早到接合按 ID 与预期 what 完成、未知字段与大
// 整数逐字节保留;截断的非法 JSON 完成使实例以 ErrProtocol 故障、不
// 结算成功、原文前缀保留且原文仍交付;故障线性化扫描统一结项未决记
// 录且不改写此前终态;重复完成保留首因、不二次结算;faulted 拒绝新
// 提交并与首因共享错误身份;Close 返回首因身份且 Stop/Destroy 恰一
// 次。
func TestIntegrationCompletionFaultMatrix(t *testing.T) {
	const handle = uintptr(0xB610)
	inst, rig := newAsyncRig(t, handle, nil)
	token := rig.creator.exToken(0)
	s := inst.inst.state
	rig.caller.asyncRets = map[string]int32{"asyncScreencap": 1, "asyncConnect": 2, "asyncClick": 3}

	// ① Screencap(id 1):完成未到时取消一次等待(容量不回收),晚到
	// ret=false(含大整数)结算为原生失败并回收容量。
	op1, err := inst.Screencap()
	if err != nil {
		t.Fatalf("Screencap = %v, want nil", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if res, err := waitOperationGuarded(t, op1, ctx); !errors.Is(err, context.Canceled) || res.ID != 0 {
		t.Fatalf("cancelled wait before completion = (%+v, %v), want (zero value, context.Canceled)", res, err)
	}
	if n := pendingOccupancy(s); n != 1 {
		t.Fatalf("pending occupancy after cancel = %d, want 1 (cancel does not reclaim early)", n)
	}
	op1Details := `{"ret":false,"cost":9007199254740993}`
	deliverAsyncInfo(t, token, 1, "Screencap", op1Details)
	res1, err1 := waitOperationGuarded(t, op1, context.Background())
	if !errors.Is(err1, native.ErrNativeRejected) {
		t.Fatalf("wait error for the late ret=false = %v, want errors.Is ErrNativeRejected", err1)
	}
	if res1.ID != 1 || res1.What != "Screencap" || res1.OK || string(res1.Details) != op1Details {
		t.Fatalf("late failure result = %+v, want {1 Screencap false %s}", res1, op1Details)
	}

	// ② Connect(id 2):完成在 raw 窗口内早到(未知字段与大整数原文),
	// raw 返回后按 ID + 预期 what 接合,等待立即返回终态。
	op2Details := `{"ret":true,"future":{"nested":[1,2,3]},"big":9007199254740993}`
	rig.caller.onAsyncConnect = func(uintptr) {
		deliverAsyncInfo(t, token, 2, "Connect", op2Details)
		waitUntil(t, "early result staged in the window", func() bool {
			v := earlyWindowOf(s)
			return v.open && v.has && v.id == 2 && v.what == "Connect"
		})
	}
	op2, err := inst.Connect(connectInput)
	if err != nil {
		t.Fatalf("Connect = %v, want nil", err)
	}
	res2, err2 := waitOperationGuarded(t, op2, context.Background())
	if err2 != nil || res2.ID != 2 || res2.What != "Connect" || !res2.OK || string(res2.Details) != op2Details {
		t.Fatalf("early-join result = (%+v, %v), want {2 Connect true %s}", res2, err2, op2Details)
	}
	if v := earlyWindowOf(s); v.open || v.has {
		t.Errorf("early window not cleared after the join: %+v", v)
	}

	// ③ Click(id 3):完成消息是截断的非法 JSON——无法关联的完成不得
	// 伪造成成功:ErrProtocol 故障、结算通知不发生、原文前缀保留、原
	// 文仍按公开预算交付;故障线性化扫描统一结项未决记录。
	op3, err := inst.Click(4, 5)
	if err != nil {
		t.Fatalf("Click = %v, want nil", err)
	}
	invalidPayload := []byte(`{"what":"Click","async_call_id":3,`)
	native.LookupCallbackDelivery(token)(int32(asyncCallInfoMessageID), callbackCBytes(string(invalidPayload)))
	waitUntil(t, "the illegal completion triggered the first cause", func() bool { return s.currentFault() != nil })
	fault := s.currentFault()
	if !errors.Is(fault, native.ErrProtocol) {
		t.Fatalf("first cause = %v, want errors.Is ErrProtocol", fault)
	}
	res3, err3 := waitOperationGuarded(t, op3, context.Background())
	if err3 == nil || !errors.Is(err3, fault) || !errors.Is(err3, native.ErrProtocol) {
		t.Fatalf("settlement error of the uncorrelatable completion = %v, want the same identity as the first cause with ErrProtocol reachable along the chain", err3)
	}
	if res3.ID != 0 || res3.OK || res3.Details != nil {
		t.Fatalf("settlement of the uncorrelatable completion must carry no result: %+v", res3)
	}
	snap := s.faultDiagnostics()
	if !bytes.Equal(snap.rawPrefix, invalidPayload) {
		t.Fatalf("raw prefix = %q, want byte-for-byte preserved %q", snap.rawPrefix, invalidPayload)
	}
	if n, r := pendingOccupancy(s), pendingReservedOf(s); n != 0 || r != 0 {
		t.Fatalf("pending occupancy after the fault scan = (%d, %d), want (0, 0)", n, r)
	}
	select {
	case <-s.faultNotify():
	default:
		t.Error("the first-fault notification is not closed")
	}

	// ④ 同一 ID 的重复完成在故障后到达:不能伪造成新的成功(首因保
	// 留,已结算终态不改写)。
	deliverAsyncInfo(t, token, 3, "Click", `{"ret":true}`)

	// 公开事件逐条交付(界内原文不丢失):完成、早到、非法原文、重复
	// ——第 4 条的到达证明重复完成已经过结算入口(结算先于投递)。
	for i, want := range []struct {
		raw []byte
	}{
		{asyncCallInfoPayload(1, "Screencap", op1Details)},
		{asyncCallInfoPayload(2, "Connect", op2Details)},
		{invalidPayload},
		{asyncCallInfoPayload(3, "Click", `{"ret":true}`)},
	} {
		ev := mustReadEvent(t, inst.inst.disp.events())
		if ev.MessageID != asyncCallInfoMessageID {
			t.Errorf("public event %d MessageID = %d, want AsyncCallInfo(%d)", i, ev.MessageID, asyncCallInfoMessageID)
		}
		if !bytes.Equal(ev.JSON, want.raw) {
			t.Errorf("public event %d raw = %s, want byte-for-byte equal %s", i, ev.JSON, want.raw)
		}
	}

	// 重复完成处理完成后的终态断言:首因保留、op3 终态未被二次结算、
	// 界内已复制的原文零丢失。
	if got := s.currentFault(); got != fault {
		t.Fatalf("the duplicate completion overwrote the first cause: %v, want first cause %v", got, fault)
	}
	res3b, err3b := waitOperationGuarded(t, op3, context.Background())
	if err3b != err3 {
		t.Fatalf("the duplicate completion settled the terminal state a second time: (%+v, %v), want the same stored value as the first settlement", res3b, err3b)
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("lost count = %d, want 0 (in-budget copied raw messages are still delivered one by one)", lost)
	}

	// ⑤ faulted 拒绝新提交,错误与首因共享同一错误身份。
	if op, err := inst.Screencap(); op != nil || !errors.Is(err, fault) {
		t.Fatalf("new submission during faulted = (%+v, %v), want the first cause reachable along the chain", op, err)
	}

	// ⑥ Close:返回首因身份,native 正常返回(一次 Stop、一次
	// Destroy),尾段不覆盖已发布终态;多次 Close 同一终态。
	if err := inst.Close(); err == nil || !errors.Is(err, fault) {
		t.Fatalf("Close = %v, want the same identity as the first cause", err)
	}
	assertSingleStopDestroy(t, rig.closer, handle)
	if err := inst.Close(); err == nil || !errors.Is(err, fault) {
		t.Errorf("idempotent Close = %v, want the same terminal state", err)
	}
	if n := pendingOccupancy(s); n != 0 {
		t.Errorf("pending occupancy after close = %d, want 0", n)
	}
	if _, err := waitOperationGuarded(t, op1, context.Background()); !errors.Is(err, native.ErrNativeRejected) {
		t.Errorf("op1 terminal state rewritten after close: %v, want still the native failure", err)
	}
	if _, err := waitOperationGuarded(t, op2, context.Background()); err != nil {
		t.Errorf("op2 success terminal state rewritten after close: %v", err)
	}
	if _, err := waitOperationGuarded(t, op3, context.Background()); err == nil || !errors.Is(err, fault) {
		t.Errorf("op3 terminal state after close = %v, want the same identity as the first cause", err)
	}
	if _, ok := <-inst.inst.disp.events(); ok {
		t.Error("Events should be closed after Close")
	}
}

// TestIntegrationCapacityLimitsWithStalledConsumer 验证「消费
// 者完全停读时故障通知仍可见,pending 得到结算,Close 在 native 正常
// 返回的前提下完成」与容量上限的公开路径触发:单条超过
// CallbackBytes 在投递路径预留点即拒绝;公开缓冲字节预算在消费者完全
// 停读下溢出、故障通知独立于 Events 可观察、丢失计数按条累计且首因
// sticky;Close 在 native 正常返回下完成,返回首因身份,已确认的完成
// 结果不被溢出改写、未完成项按首因结项,缓冲事件关闭后仍逐字节可读。
func TestIntegrationCapacityLimitsWithStalledConsumer(t *testing.T) {
	t.Run("single message exceeding CallbackBytes rejected at the reservation point", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xB620, func(l *Limits) { l.CallbackBytes = 96 })
		token := rig.creator.exToken(0)
		s := inst.inst.state

		oversized := bytes.Repeat([]byte{'a'}, 120)
		native.LookupCallbackDelivery(token)(80, callbackCBytes(string(oversized)))
		fault := s.currentFault()
		if fault == nil || !errors.Is(fault, native.ErrEventOverflow) {
			t.Fatalf("first cause of the single-message overflow = %v, want errors.Is ErrEventOverflow", fault)
		}
		if lost := s.currentLostEvents(); lost != 1 {
			t.Errorf("lost count = %d, want 1 (the oversized message was not delivered)", lost)
		}
		if n, b := internalBudgetHold(inst.inst.disp); n != 0 || b != 0 {
			t.Errorf("internal budget occupancy = (%d, %d), want (0, 0) (rejected at the reservation point, no budget consumed)", n, b)
		}
		// 非阻塞检查(实例未关闭,通道开放):超限消息不得产生公开事件。
		select {
		case ev := <-inst.inst.disp.events():
			t.Errorf("an oversized message must not produce a public event: %v", ev)
		default:
		}
		// 故障后新提交被拒,错误链可达首因。
		if op, err := inst.Screencap(); op != nil || !errors.Is(err, fault) {
			t.Fatalf("submission after the overflow fault = (%+v, %v), want the first cause reachable along the chain", op, err)
		}
		// Close 在 native 正常返回下完成,返回首因身份,销毁恰一次。
		if err := inst.Close(); err == nil || !errors.Is(err, native.ErrEventOverflow) {
			t.Fatalf("Close = %v, want the same identity as the first cause", err)
		}
		assertSingleStopDestroy(t, rig.closer, 0xB620)
	})

	t.Run("public buffer byte budget overflows while the consumer stalls reading, with the fault notification visible", func(t *testing.T) {
		// 预算满足 validateLimits 的全部交叉校验(CallbackBytes ≤
		// InternalBytes/PublicBytes/EarlyBytes),与真实 Open 快照同一
		// 纪律;PublicBytes 收紧到两条完成消息之后必然溢出。
		inst, rig := newAsyncRig(t, 0xB621, func(l *Limits) {
			l.CallbackBytes = 96
			l.InternalEvents = 8
			l.InternalBytes = 1024
			l.PublicEvents = 8
			l.PublicBytes = 140
			l.EarlyBytes = 96
		})
		token := rig.creator.exToken(0)
		s := inst.inst.state
		rig.caller.asyncRets = map[string]int32{"asyncScreencap": 1, "asyncConnect": 2, "asyncClick": 3}

		// 三笔公开提交:两笔带完成(各 61/59 字节,进入公开缓冲),一笔
		// 无完成(留给首因线性化扫描)。消费者全程停读。
		op1, err := inst.Screencap()
		if err != nil {
			t.Fatalf("Screencap = %v, want nil", err)
		}
		op2, err := inst.Connect(connectInput)
		if err != nil {
			t.Fatalf("Connect = %v, want nil", err)
		}
		op3, err := inst.Click(4, 5)
		if err != nil {
			t.Fatalf("Click = %v, want nil", err)
		}

		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":true}`)
		deliverAsyncInfo(t, token, 2, "Connect", `{"ret":true}`)
		unknownPayload := `{"what":"UnknownMsg"}` // 21 字节:120+21 > PublicBytes 140
		deliverUnknown := func() {
			native.LookupCallbackDelivery(token)(99, callbackCBytes(unknownPayload))
		}
		for i := 0; i < 4; i++ {
			deliverUnknown()
		}
		waitUntil(t, "all four overflows counted as lost one by one", func() bool {
			return s.currentLostEvents() == 4
		})
		fault := s.currentFault()
		if fault == nil || !errors.Is(fault, native.ErrEventOverflow) {
			t.Fatalf("first cause = %v, want errors.Is ErrEventOverflow", fault)
		}

		// 故障通知独立于 Events:未读任何事件即可观察到已关闭。
		select {
		case <-s.faultNotify():
		default:
			t.Error("overflow fault notification not closed (must remain visible even with the consumer fully stalled)")
		}
		// 首因 sticky:重复溢出只累计丢失,不覆盖首因。
		if s.currentFault() != fault {
			t.Errorf("the first cause was overwritten by a later overflow: %v", s.currentFault())
		}

		// Close 在 native 正常返回的前提下完成;返回首因身份,销毁恰
		// 一次;pending 得到结算。
		if err := inst.Close(); err == nil || !errors.Is(err, native.ErrEventOverflow) {
			t.Fatalf("Close = %v, want the same identity as the first cause", err)
		}
		assertSingleStopDestroy(t, rig.closer, 0xB621)
		if n := pendingOccupancy(s); n != 0 {
			t.Errorf("pending occupancy after the close settlement = %d, want 0", n)
		}
		// 已确认的完成结果不被随后的溢出改写;未完成项按首因结项。
		res1, err1 := waitOperationGuarded(t, op1, context.Background())
		if err1 != nil || res1.ID != 1 || res1.What != "Screencap" || !res1.OK {
			t.Fatalf("op1 terminal state = (%+v, %v), want the success result not rewritten by the overflow", res1, err1)
		}
		res2, err2 := waitOperationGuarded(t, op2, context.Background())
		if err2 != nil || res2.ID != 2 || !res2.OK {
			t.Fatalf("op2 terminal state = (%+v, %v), want the success result not rewritten by the overflow", res2, err2)
		}
		res3, err3 := waitOperationGuarded(t, op3, context.Background())
		if err3 == nil || !errors.Is(err3, fault) {
			t.Fatalf("op3 settlement error = %v, want the same identity as the first cause", err3)
		}
		if res3.ID != 0 || res3.OK {
			t.Errorf("op3 fault settlement must carry no result: %+v", res3)
		}
		// 已在公开缓冲中的两条事件关闭后仍可读且逐字节一致;随后已关闭。
		ev1 := mustReadEvent(t, inst.inst.disp.events())
		ev2 := mustReadEvent(t, inst.inst.disp.events())
		if !bytes.Equal(ev1.JSON, asyncCallInfoPayload(1, "Screencap", `{"ret":true}`)) {
			t.Errorf("buffered event 1 = %s, want byte-for-byte identical to the completion raw", ev1.JSON)
		}
		if !bytes.Equal(ev2.JSON, asyncCallInfoPayload(2, "Connect", `{"ret":true}`)) {
			t.Errorf("buffered event 2 = %s, want byte-for-byte identical to the completion raw", ev2.JSON)
		}
		if _, ok := <-inst.inst.disp.events(); ok {
			t.Error("Events should be closed after Close")
		}
	})
}

// TestIntegrationStableTerminalStatesAndSnapshotIsolation 验证
// 「Done 或任一 Close 返回后可立即读取稳定 Err 和 Operation 结果,修
// 改先前结果不污染内部快照」的公开路径组合:经真实投递链取得的
// Event 与 Wait 结果各自是独立副本,原地改写不影响内部缓存、其他等
// 待与后续投递;多次 Close 返回同一稳定终态,Done 关闭后终态立即稳
// 定,不依赖额外调度或延时。
func TestIntegrationStableTerminalStatesAndSnapshotIsolation(t *testing.T) {
	const handle = uintptr(0xB630)
	inst, rig := newAsyncRig(t, handle, nil)
	token := rig.creator.exToken(0)
	s := inst.inst.state
	rig.caller.asyncRets = map[string]int32{"asyncScreencap": 1, "asyncConnect": 2}

	op1, err := inst.Screencap()
	if err != nil {
		t.Fatalf("Screencap = %v, want nil", err)
	}
	op2Details := `{"ret":true,"cost":9007199254740993}`
	rig.caller.onAsyncConnect = func(uintptr) {
		deliverAsyncInfo(t, token, 2, "Connect", op2Details)
		waitUntil(t, "early result staged in the window", func() bool {
			v := earlyWindowOf(s)
			return v.open && v.has && v.id == 2
		})
	}
	op2, err := inst.Connect(connectInput)
	if err != nil {
		t.Fatalf("Connect = %v, want nil", err)
	}

	// op2 早到接合后立即取得终态;经公开投递链取得的 Event 与 Wait
	// 结果各自独立,原地改写互不污染。
	res2, err2 := waitOperationGuarded(t, op2, context.Background())
	if err2 != nil || res2.ID != 2 || !res2.OK {
		t.Fatalf("op2 terminal state = (%+v, %v), want early-join success", res2, err2)
	}
	ev2 := mustReadEvent(t, inst.inst.disp.events())
	if ev2.MessageID != asyncCallInfoMessageID {
		t.Fatalf("public event MessageID = %d, want AsyncCallInfo(%d)", ev2.MessageID, asyncCallInfoMessageID)
	}
	for i := range ev2.JSON {
		ev2.JSON[i] = 'x'
	}
	for i := range res2.Details {
		res2.Details[i] = 'y'
	}
	res2again, err2again := waitOperationGuarded(t, op2, context.Background())
	if err2again != nil || string(res2again.Details) != op2Details {
		t.Fatalf("re-wait after mutating the copy = (%+v, %v), want the cached terminal state unpolluted", res2again, err2again)
	}

	// op1 晚到 ret=false 完成结算;改写过的先前副本不影响后续投递的
	// 逐字节保真。
	op1Details := `{"ret":false}`
	deliverAsyncInfo(t, token, 1, "Screencap", op1Details)
	res1, err1 := waitOperationGuarded(t, op1, context.Background())
	if !errors.Is(err1, native.ErrNativeRejected) || res1.ID != 1 || res1.OK {
		t.Fatalf("op1 terminal state = (%+v, %v), want the native failure", res1, err1)
	}
	if string(res1.Details) != op1Details {
		t.Fatalf("op1 result = %s, want byte-for-byte preserved %s", res1.Details, op1Details)
	}
	ev1 := mustReadEvent(t, inst.inst.disp.events())
	if !bytes.Equal(ev1.JSON, asyncCallInfoPayload(1, "Screencap", op1Details)) {
		t.Fatalf("subsequently delivered event = %s, want byte-for-byte equal %s (the earlier copy mutation must not leak through)", ev1.JSON, asyncCallInfoPayload(1, "Screencap", op1Details))
	}

	// 关闭:多次 Close 同一稳定终态;Done 关闭后立即读取稳定 Err 与
	// Operation 终态(无需额外调度或延时);改写过的副本不产生任何内
	// 部影响。
	if err := inst.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	select {
	case <-s.done():
	default:
		t.Error("Done not closed after Close returned")
	}
	if got := s.finalError(); got != nil {
		t.Errorf("final error = %v, want nil", got)
	}
	if err := inst.Close(); err != nil {
		t.Errorf("idempotent Close = %v, want the same terminal state nil as the first call", err)
	}
	if _, ok := <-inst.inst.disp.events(); ok {
		t.Error("Events should be closed after Close")
	}
	res1b, err1b := waitOperationGuarded(t, op1, context.Background())
	if !errors.Is(err1b, native.ErrNativeRejected) || res1b.ID != 1 || string(res1b.Details) != op1Details {
		t.Errorf("op1 terminal state after close = (%+v, %v), want identical to before the close", res1b, err1b)
	}
	res2b, err2b := waitOperationGuarded(t, op2, context.Background())
	if err2b != nil || res2b.ID != 2 || string(res2b.Details) != op2Details {
		t.Errorf("op2 terminal state after close = (%+v, %v), want identical to before the close", res2b, err2b)
	}
}
