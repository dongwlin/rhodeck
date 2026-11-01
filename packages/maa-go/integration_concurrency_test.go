// 本文件是综合并发、故障与数据所有权跨组件回归套件:覆盖公开提交 →
// fake native → token 注册表 → callback 准入 → 分发器 → Operation/Events
// 的交叉形态,补充各组件局部用例未组合的竞态。

package maa

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NotNil(t, lateDeliver, "token not registered in the registry, cannot construct the late delivery probe")

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
	require.Equal(t, 0, pendingOccupancy(s), "pending occupancy while B is queued before the gate = (%d, %d), want (0, 2) (neither reservation registered)", pendingOccupancy(s), pendingReservedOf(s))
	require.Equal(t, uint64(2), pendingReservedOf(s), "pending occupancy while B is queued before the gate = (%d, %d), want (0, 2) (neither reservation registered)", pendingOccupancy(s), pendingReservedOf(s))

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
	_, closingErr := inst.Click(3, 4)
	require.ErrorIs(t, closingErr, native.ErrClosing, "new call during closing = %v, want errors.Is native.ErrClosing", closingErr)
	assert.Equal(t, before, rig.caller.total(), "the call rejected during closing reached native %d → %d times, want unchanged", before, rig.caller.total())

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
	errA := <-errChA
	require.NoError(t, errA, "A's Connect = %v, want nil", errA)
	opB := <-opChB
	errB := <-errChB
	require.NoError(t, errB, "B's Click = %v, want nil (registered as usual during closing)", errB)
	require.Equal(t, AsyncID(2), opB.ID(), "B's submission ID = %d, want 2", opB.ID())

	// 关闭收敛:一次 Stop、一次 Destroy,Destroy 同步 Destroyed 不锁死。
	closeRet := <-closeErr
	require.NoError(t, closeRet, "Close = %v, want nil", closeRet)
	assertSingleStopDestroy(t, rig.closer, handle)
	select {
	case <-destroyedDelivered:
	default:
		require.Fail(t, "the synchronous Destroyed delivery during Destroy did not happen")
	}

	// 观察到 Close 返回后立即读取稳定终态:无完成的提交以 ErrClosed
	// 结算恰一次,ID 保持可读,不依赖额外调度或延时。
	select {
	case <-s.done():
	default:
		assert.Fail(t, "Done not closed after Close returned")
	}
	assert.NoError(t, s.finalError(), "final error = %v, want nil (normal Stop/Destroy with no fault)", s.finalError())
	for i, op := range []*Operation{opA, opB} {
		res, err := waitOperationGuarded(t, op, context.Background())
		assert.ErrorIs(t, err, native.ErrClosed, "wait error of submission %d = %v, want errors.Is native.ErrClosed", i, err)
		assert.Equal(t, AsyncID(0), res.ID, "close settlement of submission %d must carry no result: %+v", i, res)
		assert.Empty(t, res.What, "close settlement of submission %d must carry no result: %+v", i, res)
		assert.False(t, res.OK, "close settlement of submission %d must carry no result: %+v", i, res)
		assert.Nil(t, res.Details, "close settlement of submission %d must carry no result: %+v", i, res)
		assert.Equal(t, AsyncID(i+1), op.ID(), "accepted Operation %d ID after close = %d, want kept readable %d", i, op.ID(), i+1)
	}
	assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after close drained it = %d, want 0 (capacity reclaimed)", pendingOccupancy(s))

	// Destroyed 事件在关闭后仍可读(缓冲);Events 已关闭。
	ev := mustReadEvent(t, inst.inst.disp.events())
	assert.Equal(t, destroyedMessageID, ev.MessageID, "buffered event MessageID = %d, want Destroyed(%d)", ev.MessageID, destroyedMessageID)
	_, eventsOK := <-inst.inst.disp.events()
	assert.False(t, eventsOK, "Events should be closed after Close")

	// 关门后迟到的原生线程投递:未获 callback lease,绝不读取原生
	// details(nil 探针——路径若错误读取会按 ErrProtocol 记故障)。
	lateDeliver(66, nil)
	assert.Nil(t, s.currentFault(), "the unadmitted late delivery must not record a fault (the nil probe was read), got first cause %v", s.currentFault())
	assert.Equal(t, uint64(0), s.currentLostEvents(), "lost count = %d, want 0 (unadmitted messages are not counted as lost)", s.currentLostEvents())
	assert.Equal(t, uint64(0), inFlightCallbacks(s), "in-flight callbacks after the late delivery = %d, want 0", inFlightCallbacks(s))
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
	require.NoError(t, err, "Screencap = %v, want nil", err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, cancelErr := waitOperationGuarded(t, op1, ctx)
	require.ErrorIs(t, cancelErr, context.Canceled, "cancelled wait before completion = (%+v, %v), want (zero value, context.Canceled)", res, cancelErr)
	require.Equal(t, AsyncID(0), res.ID, "cancelled wait before completion = (%+v, %v), want (zero value, context.Canceled)", res, cancelErr)
	require.Equal(t, 1, pendingOccupancy(s), "pending occupancy after cancel = %d, want 1 (cancel does not reclaim early)", pendingOccupancy(s))
	op1Details := `{"ret":false,"cost":9007199254740993}`
	deliverAsyncInfo(t, token, 1, "Screencap", op1Details)
	res1, err1 := waitOperationGuarded(t, op1, context.Background())
	require.ErrorIs(t, err1, native.ErrNativeRejected, "wait error for the late ret=false = %v, want errors.Is ErrNativeRejected", err1)
	require.Equal(t, AsyncID(1), res1.ID, "late failure result = %+v, want {1 Screencap false %s}", res1, op1Details)
	require.Equal(t, "Screencap", res1.What, "late failure result = %+v, want {1 Screencap false %s}", res1, op1Details)
	require.False(t, res1.OK, "late failure result = %+v, want {1 Screencap false %s}", res1, op1Details)
	require.Equal(t, op1Details, string(res1.Details), "late failure result = %+v, want {1 Screencap false %s}", res1, op1Details)

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
	require.NoError(t, err, "Connect = %v, want nil", err)
	res2, err2 := waitOperationGuarded(t, op2, context.Background())
	require.NoError(t, err2, "early-join result = (%+v, %v), want {2 Connect true %s}", res2, err2, op2Details)
	require.Equal(t, AsyncID(2), res2.ID, "early-join result = (%+v, %v), want {2 Connect true %s}", res2, err2, op2Details)
	require.Equal(t, "Connect", res2.What, "early-join result = (%+v, %v), want {2 Connect true %s}", res2, err2, op2Details)
	require.True(t, res2.OK, "early-join result = (%+v, %v), want {2 Connect true %s}", res2, err2, op2Details)
	require.Equal(t, op2Details, string(res2.Details), "early-join result = (%+v, %v), want {2 Connect true %s}", res2, err2, op2Details)
	v := earlyWindowOf(s)
	assert.False(t, v.open, "early window not cleared after the join: %+v", v)
	assert.False(t, v.has, "early window not cleared after the join: %+v", v)

	// ③ Click(id 3):完成消息是截断的非法 JSON——无法关联的完成不得
	// 伪造成成功:ErrProtocol 故障、结算通知不发生、原文前缀保留、原
	// 文仍按公开预算交付;故障线性化扫描统一结项未决记录。
	op3, err := inst.Click(4, 5)
	require.NoError(t, err, "Click = %v, want nil", err)
	invalidPayload := []byte(`{"what":"Click","async_call_id":3,`)
	native.LookupCallbackDelivery(token)(int32(asyncCallInfoMessageID), callbackCBytes(string(invalidPayload)))
	waitUntil(t, "the illegal completion triggered the first cause", func() bool { return s.currentFault() != nil })
	fault := s.currentFault()
	require.ErrorIs(t, fault, native.ErrProtocol, "first cause = %v, want errors.Is ErrProtocol", fault)
	res3, err3 := waitOperationGuarded(t, op3, context.Background())
	require.Error(t, err3, "settlement error of the uncorrelatable completion = %v, want the same identity as the first cause with ErrProtocol reachable along the chain", err3)
	require.ErrorIs(t, err3, fault, "settlement error of the uncorrelatable completion = %v, want the same identity as the first cause with ErrProtocol reachable along the chain", err3)
	require.ErrorIs(t, err3, native.ErrProtocol, "settlement error of the uncorrelatable completion = %v, want the same identity as the first cause with ErrProtocol reachable along the chain", err3)
	require.Equal(t, AsyncID(0), res3.ID, "settlement of the uncorrelatable completion must carry no result: %+v", res3)
	require.False(t, res3.OK, "settlement of the uncorrelatable completion must carry no result: %+v", res3)
	require.Nil(t, res3.Details, "settlement of the uncorrelatable completion must carry no result: %+v", res3)
	snap := s.faultDiagnostics()
	require.True(t, bytes.Equal(snap.rawPrefix, invalidPayload), "raw prefix = %q, want byte-for-byte preserved %q", snap.rawPrefix, invalidPayload)
	require.Equal(t, 0, pendingOccupancy(s), "pending occupancy after the fault scan = (%d, %d), want (0, 0)", pendingOccupancy(s), pendingReservedOf(s))
	require.Equal(t, uint64(0), pendingReservedOf(s), "pending occupancy after the fault scan = (%d, %d), want (0, 0)", pendingOccupancy(s), pendingReservedOf(s))
	select {
	case <-s.faultNotify():
	default:
		assert.Fail(t, "the first-fault notification is not closed")
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
		assert.Equal(t, asyncCallInfoMessageID, ev.MessageID, "public event %d MessageID = %d, want AsyncCallInfo(%d)", i, ev.MessageID, asyncCallInfoMessageID)
		assert.True(t, bytes.Equal(ev.JSON, want.raw), "public event %d raw = %s, want byte-for-byte equal %s", i, ev.JSON, want.raw)
	}

	// 重复完成处理完成后的终态断言:首因保留、op3 终态未被二次结算、
	// 界内已复制的原文零丢失。
	require.True(t, s.currentFault() == fault, "the duplicate completion overwrote the first cause: %v, want first cause %v", s.currentFault(), fault)
	res3b, err3b := waitOperationGuarded(t, op3, context.Background())
	require.True(t, err3b == err3, "the duplicate completion settled the terminal state a second time: (%+v, %v), want the same stored value as the first settlement", res3b, err3b)
	assert.Equal(t, uint64(0), s.currentLostEvents(), "lost count = %d, want 0 (in-budget copied raw messages are still delivered one by one)", s.currentLostEvents())

	// ⑤ faulted 拒绝新提交,错误与首因共享同一错误身份。
	faultedOp, faultedErr := inst.Screencap()
	require.Nil(t, faultedOp, "new submission during faulted = (%+v, %v), want the first cause reachable along the chain", faultedOp, faultedErr)
	require.ErrorIs(t, faultedErr, fault, "new submission during faulted = (%+v, %v), want the first cause reachable along the chain", faultedOp, faultedErr)

	// ⑥ Close:返回首因身份,native 正常返回(一次 Stop、一次
	// Destroy),尾段不覆盖已发布终态;多次 Close 同一终态。
	closeRet := inst.Close()
	require.Error(t, closeRet, "Close = %v, want the same identity as the first cause", closeRet)
	require.ErrorIs(t, closeRet, fault, "Close = %v, want the same identity as the first cause", closeRet)
	assertSingleStopDestroy(t, rig.closer, handle)
	idemErr := inst.Close()
	assert.Error(t, idemErr, "idempotent Close = %v, want the same terminal state", idemErr)
	assert.ErrorIs(t, idemErr, fault, "idempotent Close = %v, want the same terminal state", idemErr)
	assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after close = %d, want 0", pendingOccupancy(s))
	_, op1Err := waitOperationGuarded(t, op1, context.Background())
	assert.ErrorIs(t, op1Err, native.ErrNativeRejected, "op1 terminal state rewritten after close: %v, want still the native failure", op1Err)
	_, op2Err := waitOperationGuarded(t, op2, context.Background())
	assert.NoError(t, op2Err, "op2 success terminal state rewritten after close: %v", op2Err)
	_, op3Err := waitOperationGuarded(t, op3, context.Background())
	assert.Error(t, op3Err, "op3 terminal state after close = %v, want the same identity as the first cause", op3Err)
	assert.ErrorIs(t, op3Err, fault, "op3 terminal state after close = %v, want the same identity as the first cause", op3Err)
	_, eventsOK := <-inst.inst.disp.events()
	assert.False(t, eventsOK, "Events should be closed after Close")
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
		require.NotNil(t, fault, "first cause of the single-message overflow = %v, want errors.Is ErrEventOverflow", fault)
		require.ErrorIs(t, fault, native.ErrEventOverflow, "first cause of the single-message overflow = %v, want errors.Is ErrEventOverflow", fault)
		assert.Equal(t, uint64(1), s.currentLostEvents(), "lost count = %d, want 1 (the oversized message was not delivered)", s.currentLostEvents())
		n, b := internalBudgetHold(inst.inst.disp)
		assert.Equal(t, uint64(0), n, "internal budget occupancy = (%d, %d), want (0, 0) (rejected at the reservation point, no budget consumed)", n, b)
		assert.Equal(t, uint64(0), b, "internal budget occupancy = (%d, %d), want (0, 0) (rejected at the reservation point, no budget consumed)", n, b)
		// 非阻塞检查(实例未关闭,通道开放):超限消息不得产生公开事件。
		select {
		case ev := <-inst.inst.disp.events():
			assert.Fail(t, "an oversized message must not produce a public event", "%v", ev)
		default:
		}
		// 故障后新提交被拒,错误链可达首因。
		op, err := inst.Screencap()
		require.Nil(t, op, "submission after the overflow fault = (%+v, %v), want the first cause reachable along the chain", op, err)
		require.ErrorIs(t, err, fault, "submission after the overflow fault = (%+v, %v), want the first cause reachable along the chain", op, err)
		// Close 在 native 正常返回下完成,返回首因身份,销毁恰一次。
		closeRet := inst.Close()
		require.Error(t, closeRet, "Close = %v, want the same identity as the first cause", closeRet)
		require.ErrorIs(t, closeRet, native.ErrEventOverflow, "Close = %v, want the same identity as the first cause", closeRet)
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
		require.NoError(t, err, "Screencap = %v, want nil", err)
		op2, err := inst.Connect(connectInput)
		require.NoError(t, err, "Connect = %v, want nil", err)
		op3, err := inst.Click(4, 5)
		require.NoError(t, err, "Click = %v, want nil", err)

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
		require.NotNil(t, fault, "first cause = %v, want errors.Is ErrEventOverflow", fault)
		require.ErrorIs(t, fault, native.ErrEventOverflow, "first cause = %v, want errors.Is ErrEventOverflow", fault)

		// 故障通知独立于 Events:未读任何事件即可观察到已关闭。
		select {
		case <-s.faultNotify():
		default:
			assert.Fail(t, "overflow fault notification not closed (must remain visible even with the consumer fully stalled)")
		}
		// 首因 sticky:重复溢出只累计丢失,不覆盖首因。
		assert.True(t, s.currentFault() == fault, "the first cause was overwritten by a later overflow: %v", s.currentFault())

		// Close 在 native 正常返回的前提下完成;返回首因身份,销毁恰
		// 一次;pending 得到结算。
		closeRet := inst.Close()
		require.Error(t, closeRet, "Close = %v, want the same identity as the first cause", closeRet)
		require.ErrorIs(t, closeRet, native.ErrEventOverflow, "Close = %v, want the same identity as the first cause", closeRet)
		assertSingleStopDestroy(t, rig.closer, 0xB621)
		assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after the close settlement = %d, want 0", pendingOccupancy(s))
		// 已确认的完成结果不被随后的溢出改写;未完成项按首因结项。
		res1, err1 := waitOperationGuarded(t, op1, context.Background())
		require.NoError(t, err1, "op1 terminal state = (%+v, %v), want the success result not rewritten by the overflow", res1, err1)
		require.Equal(t, AsyncID(1), res1.ID, "op1 terminal state = (%+v, %v), want the success result not rewritten by the overflow", res1, err1)
		require.Equal(t, "Screencap", res1.What, "op1 terminal state = (%+v, %v), want the success result not rewritten by the overflow", res1, err1)
		require.True(t, res1.OK, "op1 terminal state = (%+v, %v), want the success result not rewritten by the overflow", res1, err1)
		res2, err2 := waitOperationGuarded(t, op2, context.Background())
		require.NoError(t, err2, "op2 terminal state = (%+v, %v), want the success result not rewritten by the overflow", res2, err2)
		require.Equal(t, AsyncID(2), res2.ID, "op2 terminal state = (%+v, %v), want the success result not rewritten by the overflow", res2, err2)
		require.True(t, res2.OK, "op2 terminal state = (%+v, %v), want the success result not rewritten by the overflow", res2, err2)
		res3, err3 := waitOperationGuarded(t, op3, context.Background())
		require.Error(t, err3, "op3 settlement error = %v, want the same identity as the first cause", err3)
		require.ErrorIs(t, err3, fault, "op3 settlement error = %v, want the same identity as the first cause", err3)
		assert.Equal(t, AsyncID(0), res3.ID, "op3 fault settlement must carry no result: %+v", res3)
		assert.False(t, res3.OK, "op3 fault settlement must carry no result: %+v", res3)
		// 已在公开缓冲中的两条事件关闭后仍可读且逐字节一致;随后已关闭。
		ev1 := mustReadEvent(t, inst.inst.disp.events())
		ev2 := mustReadEvent(t, inst.inst.disp.events())
		assert.True(t, bytes.Equal(ev1.JSON, asyncCallInfoPayload(1, "Screencap", `{"ret":true}`)), "buffered event 1 = %s, want byte-for-byte identical to the completion raw", ev1.JSON)
		assert.True(t, bytes.Equal(ev2.JSON, asyncCallInfoPayload(2, "Connect", `{"ret":true}`)), "buffered event 2 = %s, want byte-for-byte identical to the completion raw", ev2.JSON)
		_, eventsOK := <-inst.inst.disp.events()
		assert.False(t, eventsOK, "Events should be closed after Close")
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
	require.NoError(t, err, "Screencap = %v, want nil", err)
	op2Details := `{"ret":true,"cost":9007199254740993}`
	rig.caller.onAsyncConnect = func(uintptr) {
		deliverAsyncInfo(t, token, 2, "Connect", op2Details)
		waitUntil(t, "early result staged in the window", func() bool {
			v := earlyWindowOf(s)
			return v.open && v.has && v.id == 2
		})
	}
	op2, err := inst.Connect(connectInput)
	require.NoError(t, err, "Connect = %v, want nil", err)

	// op2 早到接合后立即取得终态;经公开投递链取得的 Event 与 Wait
	// 结果各自独立,原地改写互不污染。
	res2, err2 := waitOperationGuarded(t, op2, context.Background())
	require.NoError(t, err2, "op2 terminal state = (%+v, %v), want early-join success", res2, err2)
	require.Equal(t, AsyncID(2), res2.ID, "op2 terminal state = (%+v, %v), want early-join success", res2, err2)
	require.True(t, res2.OK, "op2 terminal state = (%+v, %v), want early-join success", res2, err2)
	ev2 := mustReadEvent(t, inst.inst.disp.events())
	require.Equal(t, asyncCallInfoMessageID, ev2.MessageID, "public event MessageID = %d, want AsyncCallInfo(%d)", ev2.MessageID, asyncCallInfoMessageID)
	for i := range ev2.JSON {
		ev2.JSON[i] = 'x'
	}
	for i := range res2.Details {
		res2.Details[i] = 'y'
	}
	res2again, err2again := waitOperationGuarded(t, op2, context.Background())
	require.NoError(t, err2again, "re-wait after mutating the copy = (%+v, %v), want the cached terminal state unpolluted", res2again, err2again)
	require.Equal(t, op2Details, string(res2again.Details), "re-wait after mutating the copy = (%+v, %v), want the cached terminal state unpolluted", res2again, err2again)

	// op1 晚到 ret=false 完成结算;改写过的先前副本不影响后续投递的
	// 逐字节保真。
	op1Details := `{"ret":false}`
	deliverAsyncInfo(t, token, 1, "Screencap", op1Details)
	res1, err1 := waitOperationGuarded(t, op1, context.Background())
	require.ErrorIs(t, err1, native.ErrNativeRejected, "op1 terminal state = (%+v, %v), want the native failure", res1, err1)
	require.Equal(t, AsyncID(1), res1.ID, "op1 terminal state = (%+v, %v), want the native failure", res1, err1)
	require.False(t, res1.OK, "op1 terminal state = (%+v, %v), want the native failure", res1, err1)
	require.Equal(t, op1Details, string(res1.Details), "op1 result = %s, want byte-for-byte preserved %s", res1.Details, op1Details)
	ev1 := mustReadEvent(t, inst.inst.disp.events())
	require.True(t, bytes.Equal(ev1.JSON, asyncCallInfoPayload(1, "Screencap", op1Details)), "subsequently delivered event = %s, want byte-for-byte equal %s (the earlier copy mutation must not leak through)", ev1.JSON, asyncCallInfoPayload(1, "Screencap", op1Details))

	// 关闭:多次 Close 同一稳定终态;Done 关闭后立即读取稳定 Err 与
	// Operation 终态(无需额外调度或延时);改写过的副本不产生任何内
	// 部影响。
	closeRet := inst.Close()
	require.NoError(t, closeRet, "Close = %v, want nil", closeRet)
	select {
	case <-s.done():
	default:
		assert.Fail(t, "Done not closed after Close returned")
	}
	assert.NoError(t, s.finalError(), "final error = %v, want nil", s.finalError())
	idemErr := inst.Close()
	assert.NoError(t, idemErr, "idempotent Close = %v, want the same terminal state nil as the first call", idemErr)
	_, eventsOK := <-inst.inst.disp.events()
	assert.False(t, eventsOK, "Events should be closed after Close")
	res1b, err1b := waitOperationGuarded(t, op1, context.Background())
	assert.ErrorIs(t, err1b, native.ErrNativeRejected, "op1 terminal state after close = (%+v, %v), want identical to before the close", res1b, err1b)
	assert.Equal(t, AsyncID(1), res1b.ID, "op1 terminal state after close = (%+v, %v), want identical to before the close", res1b, err1b)
	assert.Equal(t, op1Details, string(res1b.Details), "op1 terminal state after close = (%+v, %v), want identical to before the close", res1b, err1b)
	res2b, err2b := waitOperationGuarded(t, op2, context.Background())
	assert.NoError(t, err2b, "op2 terminal state after close = (%+v, %v), want identical to before the close", res2b, err2b)
	assert.Equal(t, AsyncID(2), res2b.ID, "op2 terminal state after close = (%+v, %v), want identical to before the close", res2b, err2b)
	assert.Equal(t, op2Details, string(res2b.Details), "op2 terminal state after close = (%+v, %v), want identical to before the close", res2b, err2b)
}
