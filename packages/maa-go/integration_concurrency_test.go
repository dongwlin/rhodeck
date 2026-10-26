// 本文件是 MAA-26 的综合并发/故障/数据所有权跨组件回归套件(绑定设计
// §7、§8、§9.1 补充行为验收):白盒同包验证公开 API 全链路——公开提交
// 方法 → fake native 接缝 → 真实 token 注册表查表投递
// (LookupCallbackDelivery → callbackTarget.deliver)→ callback 准入 →
// 有界分发器 → 实例状态结算 → Operation/Events 公开面——在关闭、故
// 障与完成竞争组合下的终态与数据所有权契约。按任务执行范围「集成
// MAA-14 至 MAA-25,补跨组件回归,不重复各任务已有的局部测试」,本
// 文件只覆盖既有局部用例未组合的交叉形态,并在下方维护 §9.1 补充行
// 为验收与测试用例的映射记录。全部用例以受控屏障(channel/轮询等待)
// 确定性同步,不加载任何原生库、不触达 loader/真实 API 表,
// CGO_ENABLED=0 可跑;防挂死只用 hangGuard 兜底,不作同步手段;race
// 检查只辅助发现 Go 并发问题,不作为原生 ABI 正确性的证明(§9)。
//
// maa→native 的 token 注册表是进程级共享状态:本文件用例互不并行
// (不调 t.Parallel);实例由 createRig.cleanup 兜底 Close,不留悬挂
// goroutine 与未注销 token 影响 -race。
//
// §9.1 补充行为验收 → 测试用例映射(既有局部用例 + 本文件新增跨组
// 件组合;「补」指本文件新增的组合形态):
//   - 「调用已准入但尚在串行门禁前排队,同时 Close:新调用被拒绝,
//     旧调用完成后 Destroy 恰好一次;多个 Close 等同一关闭结果」:
//     既有 TestCloseWaitsForCallsQueuedBeforeRawGate(state_test.go)、
//     TestCloseWaitsQueuedSyncEntries(instance_sync_test.go)、
//     TestInstanceCloseWaitsCallsBeforeDestroy(instance_test.go);
//     补 TestIntegrationCloseWithQueuedAsyncAndSyncDestroyed——异步
//     提交(pending 预留横跨关闭选主)与 Destroy 期间同步 Destroyed、
//     关门后迟到投递的组合形态。
//   - 「观察到 Done 或多个 Close 返回后立即读取 Err、等待 Operation:
//     最终错误与结果稳定,不依赖额外调度或延时」:既有
//     TestInstanceCloseWithStalledEventConsumer(instance_test.go);
//     补 TestIntegrationStableTerminalStatesAndSnapshotIsolation——
//     经公开投递路径取得副本(Events/Wait)后再改写、多次 Close 同
//     终态的组合形态。
//   - 「callback 查 token/准入处暂停,同时关闭:未准入者不能读取原
//     生内存,已准入者退出前不释放状态;Stop/Destroy 同步回调不锁
//     死」:既有 TestCallbackDeliverTokenRemovedAfterGateCloseAndDrain、
//     TestCallbackDeliverGateClosedSkipsNativeDetails(callback_test.go)、
//     TestInstanceCloseGateAndTokenAfterDestroy(instance_test.go);
//     补 TestIntegrationCloseWithQueuedAsyncAndSyncDestroyed——真实
//     注册表查表路径上「Destroy 调用线程同步投递 Destroyed 不锁死 +
//     关门后迟到(未准入)投递不读 details(nil 探针)」的组合形态。
//   - 「丢弃 Operation、取消 Wait 后收到早到/晚到结果:关联和容量回
//     收仍正确;Close 竞争只结算一次;消费者完全停读也不阻塞 native
//     清理」:既有 TestAsyncWaitCancellationAndLateSettlement、
//     TestAsyncCloseRacesSettleOnce、TestAsyncCompletionArrivalOrders
//     (instance_async_test.go)、TestOperationSettlesExactlyOnceUnder
//     Races(operation_test.go);补 TestIntegrationCompletionFaultMatrix
//     ——取消等待 + 早到接合 + ret=false 晚到 + 非法 JSON 故障 + 重
//     复完成的混排组合形态。
//   - 「原生内容在调用后改写/销毁、配置和输入在方法返回后改写、一
//     次 Wait 的结果被修改:Go 快照与后续 Wait 不受影响」:既有
//     TestOpenSnapshotIsDeepCopy(runtime_test.go)、TestInstanceConfig
//     SnapshotIsDeepCopy(input_test.go)、TestTaskInputSnapshotIsDeepCopy
//     (input_test.go)、TestReadSnapshotsSurviveNativeAndCallerWrites
//     (instance_read_test.go)、TestEventRecordSnapshotIsIndependent、
//     TestOperationWaitSnapshotIndependence(event_test.go/operation_test.go);
//     补 TestIntegrationStableTerminalStatesAndSnapshotIsolation——经
//     真实投递链取得 Event/AsyncResult 副本后原地改写的组合形态。
//   - 容量上限(§7.1)与消费者停读:单条 CallbackBytes、公开缓冲字节
//     预算在消费者完全停读下的触发、故障通知独立可见与 pending 结算
//     归 TestIntegrationCapacityLimitsWithStalledConsumer;内部队列计
//     数/字节预算、pending 与早到上限的既有覆盖见
//     TestEventDispatchBudgetOverflowReportsFault(dispatch_test.go)、
//     TestAsyncPendingLimitReclaimsAndRejects、
//     TestAsyncProtocolViolationsFaultInstance(instance_async_test.go)
//     ——分发循环全程非阻塞,内部队列积压无法经公开 API 确定性构
//     造(见文件尾的边界说明),其预算由 admit 原语在真实投递路径上
//     继续把守。

package maa

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// TestIntegrationCloseWithQueuedAsyncAndSyncDestroyed 钉住验收条件
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
		t.Fatal("token 未在注册表登记,无法构造迟到投递探针")
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
	waitUntil(t, "两笔调用均已准入", func() bool { return inFlightCalls(s) == 2 })
	// A 在 raw 段内尚未返回(预留未接合)、B 在门禁前已预留:两份预留
	// 都横跨关闭选主,均计入容量占用。
	if n, r := pendingOccupancy(s), pendingReservedOf(s); n != 0 || r != 2 {
		t.Fatalf("B 在门禁前排队时 pending 占用 = (%d, %d), want (0, 2)(两份预留均未登记)", n, r)
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
	waitUntil(t, "close 完成选主转入 closing", func() bool { return s.currentPhase() == phaseClosing })

	// 新调用被拒绝(closing),且不触达 native。
	before := rig.caller.total()
	if _, err := inst.Click(3, 4); !errors.Is(err, native.ErrClosing) {
		t.Fatalf("closing 中的新调用 = %v, want errors.Is native.ErrClosing", err)
	}
	if got := rig.caller.total(); got != before {
		t.Errorf("closing 中被拒的调用触达 native %d → %d 次, want 不变", before, got)
	}

	// 负面观察窗:A、B 均在途(含门禁前的排队者),Stop/Destroy 不得
	// 发生。
	<-time.After(notWithin)
	assertNoStopDestroy(t, rig.closer, "在途调用(含门禁前排队者)未退出时")

	// 放行 A:B 转入门禁段并照常登记(先于关闭准入的提交不被撤销)。
	// 两笔提交的登记不由中间状态断言(它们与关闭前进并发,瞬时占用
	// 不可确定性观察):登记未撤销由下文「ID 保持可读的 ErrClosed 结
	// 算」与 pending 归零共同证明。
	close(releaseA)
	opA := <-opChA
	if err := <-errChA; err != nil {
		t.Fatalf("A 的 Connect = %v, want nil", err)
	}
	opB := <-opChB
	if err := <-errChB; err != nil {
		t.Fatalf("B 的 Click = %v, want nil(closing 中照常登记)", err)
	}
	if opB.ID() != 2 {
		t.Fatalf("B 的提交 ID = %d, want 2", opB.ID())
	}

	// 关闭收敛:一次 Stop、一次 Destroy,Destroy 同步 Destroyed 不锁死。
	if err := <-closeErr; err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	assertSingleStopDestroy(t, rig.closer, handle)
	select {
	case <-destroyedDelivered:
	default:
		t.Fatal("Destroy 期间的同步 Destroyed 投递未发生")
	}

	// 观察到 Close 返回后立即读取稳定终态:无完成的提交以 ErrClosed
	// 结算恰一次,ID 保持可读,不依赖额外调度或延时。
	select {
	case <-s.done():
	default:
		t.Error("Close 返回后 Done 未关闭")
	}
	if got := s.finalError(); got != nil {
		t.Errorf("最终错误 = %v, want nil(正常 Stop/Destroy 且无故障)", got)
	}
	for i, op := range []*Operation{opA, opB} {
		res, err := waitOperationGuarded(t, op, context.Background())
		if !errors.Is(err, native.ErrClosed) {
			t.Errorf("提交 %d 的等待错误 = %v, want errors.Is native.ErrClosed", i, err)
		}
		if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
			t.Errorf("提交 %d 的关闭结算必须无结果: %+v", i, res)
		}
		if got := op.ID(); got != AsyncID(i+1) {
			t.Errorf("已接受 Operation %d 的 ID 关闭后 = %d, want 保持可读 %d", i, got, i+1)
		}
	}
	if n := pendingOccupancy(s); n != 0 {
		t.Errorf("关闭清空后 pending 占用 = %d, want 0(容量已回收)", n)
	}

	// Destroyed 事件在关闭后仍可读(缓冲);Events 已关闭。
	ev := mustReadEvent(t, inst.inst.disp.events())
	if ev.MessageID != destroyedMessageID {
		t.Errorf("缓冲事件 MessageID = %d, want Destroyed(%d)", ev.MessageID, destroyedMessageID)
	}
	if _, ok := <-inst.inst.disp.events(); ok {
		t.Error("Close 后 Events 应已关闭")
	}

	// 关门后迟到的原生线程投递:未获 callback lease,绝不读取原生
	// details(nil 探针——路径若错误读取会按 ErrProtocol 记故障)。
	lateDeliver(66, nil)
	if s.currentFault() != nil {
		t.Errorf("未准入的迟到投递不得记录故障(nil 探针被读取),得到首因 %v", s.currentFault())
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("丢失计数 = %d, want 0(未准入的消息不计丢失)", lost)
	}
	if n := inFlightCallbacks(s); n != 0 {
		t.Errorf("迟到投递后的在途 callback = %d, want 0", n)
	}
}

// TestIntegrationCompletionFaultMatrix 钉住验收条件「取消或丢弃
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
		t.Fatalf("完成前的取消等待 = (%+v, %v), want (零值, context.Canceled)", res, err)
	}
	if n := pendingOccupancy(s); n != 1 {
		t.Fatalf("取消后 pending 占用 = %d, want 1(取消不提前回收)", n)
	}
	op1Details := `{"ret":false,"cost":9007199254740993}`
	deliverAsyncInfo(t, token, 1, "Screencap", op1Details)
	res1, err1 := waitOperationGuarded(t, op1, context.Background())
	if !errors.Is(err1, native.ErrNativeRejected) {
		t.Fatalf("晚到 ret=false 的等待错误 = %v, want errors.Is ErrNativeRejected", err1)
	}
	if res1.ID != 1 || res1.What != "Screencap" || res1.OK || string(res1.Details) != op1Details {
		t.Fatalf("晚到失败结果 = %+v, want {1 Screencap false %s}", res1, op1Details)
	}

	// ② Connect(id 2):完成在 raw 窗口内早到(未知字段与大整数原文),
	// raw 返回后按 ID + 预期 what 接合,等待立即返回终态。
	op2Details := `{"ret":true,"future":{"nested":[1,2,3]},"big":9007199254740993}`
	rig.caller.onAsyncConnect = func(uintptr) {
		deliverAsyncInfo(t, token, 2, "Connect", op2Details)
		waitUntil(t, "早到结果已临存进窗口", func() bool {
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
		t.Fatalf("早到接合结果 = (%+v, %v), want {2 Connect true %s}", res2, err2, op2Details)
	}
	if v := earlyWindowOf(s); v.open || v.has {
		t.Errorf("接合后早到窗口未清空: %+v", v)
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
	waitUntil(t, "非法完成已触发首因", func() bool { return s.currentFault() != nil })
	fault := s.currentFault()
	if !errors.Is(fault, native.ErrProtocol) {
		t.Fatalf("首因 = %v, want errors.Is ErrProtocol", fault)
	}
	res3, err3 := waitOperationGuarded(t, op3, context.Background())
	if err3 == nil || !errors.Is(err3, fault) || !errors.Is(err3, native.ErrProtocol) {
		t.Fatalf("无法关联完成的结算错误 = %v, want 与首因同一身份且沿链可达 ErrProtocol", err3)
	}
	if res3.ID != 0 || res3.OK || res3.Details != nil {
		t.Fatalf("无法关联完成的结算必须无结果: %+v", res3)
	}
	snap := s.faultDiagnostics()
	if !bytes.Equal(snap.rawPrefix, invalidPayload) {
		t.Fatalf("原文前缀 = %q, want 逐字节保留的 %q", snap.rawPrefix, invalidPayload)
	}
	if n, r := pendingOccupancy(s), pendingReservedOf(s); n != 0 || r != 0 {
		t.Fatalf("故障扫描后 pending 占用 = (%d, %d), want (0, 0)", n, r)
	}
	select {
	case <-s.faultNotify():
	default:
		t.Error("首次故障的通知未关闭")
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
			t.Errorf("公开事件 %d 的 MessageID = %d, want AsyncCallInfo(%d)", i, ev.MessageID, asyncCallInfoMessageID)
		}
		if !bytes.Equal(ev.JSON, want.raw) {
			t.Errorf("公开事件 %d 原文 = %s, want 逐字节相等的 %s", i, ev.JSON, want.raw)
		}
	}

	// 重复完成处理完成后的终态断言:首因保留、op3 终态未被二次结算、
	// 界内已复制的原文零丢失。
	if got := s.currentFault(); got != fault {
		t.Fatalf("重复完成改写了首因: %v, want 首因 %v", got, fault)
	}
	res3b, err3b := waitOperationGuarded(t, op3, context.Background())
	if err3b != err3 {
		t.Fatalf("重复完成二次结算了终态: (%+v, %v), want 与首次结算同一存储值", res3b, err3b)
	}
	if lost := s.currentLostEvents(); lost != 0 {
		t.Errorf("丢失计数 = %d, want 0(界内已复制的原文仍逐条交付)", lost)
	}

	// ⑤ faulted 拒绝新提交,错误与首因共享同一错误身份。
	if op, err := inst.Screencap(); op != nil || !errors.Is(err, fault) {
		t.Fatalf("faulted 中的新提交 = (%+v, %v), want 沿链可达首因", op, err)
	}

	// ⑥ Close:返回首因身份,native 正常返回(一次 Stop、一次
	// Destroy),尾段不覆盖已发布终态;多次 Close 同一终态。
	if err := inst.Close(); err == nil || !errors.Is(err, fault) {
		t.Fatalf("Close = %v, want 与首因同一身份", err)
	}
	assertSingleStopDestroy(t, rig.closer, handle)
	if err := inst.Close(); err == nil || !errors.Is(err, fault) {
		t.Errorf("幂等 Close = %v, want 同一终态", err)
	}
	if n := pendingOccupancy(s); n != 0 {
		t.Errorf("关闭后 pending 占用 = %d, want 0", n)
	}
	if _, err := waitOperationGuarded(t, op1, context.Background()); !errors.Is(err, native.ErrNativeRejected) {
		t.Errorf("关闭后 op1 的终态被改写: %v, want 仍是原生失败", err)
	}
	if _, err := waitOperationGuarded(t, op2, context.Background()); err != nil {
		t.Errorf("关闭后 op2 的成功终态被改写: %v", err)
	}
	if _, err := waitOperationGuarded(t, op3, context.Background()); err == nil || !errors.Is(err, fault) {
		t.Errorf("关闭后 op3 的终态 = %v, want 与首因同一身份", err)
	}
	if _, ok := <-inst.inst.disp.events(); ok {
		t.Error("Close 后 Events 应已关闭")
	}
}

// TestIntegrationCapacityLimitsWithStalledConsumer 钉住验收条件「消费
// 者完全停读时故障通知仍可见,pending 得到结算,Close 在 native 正常
// 返回的前提下完成」与 §7.1 容量上限的公开路径触发:单条超过
// CallbackBytes 在投递路径预留点即拒绝;公开缓冲字节预算在消费者完全
// 停读下溢出、故障通知独立于 Events 可观察、丢失计数按条累计且首因
// sticky;Close 在 native 正常返回下完成,返回首因身份,已确认的完成
// 结果不被溢出改写、未完成项按首因结项,缓冲事件关闭后仍逐字节可读。
func TestIntegrationCapacityLimitsWithStalledConsumer(t *testing.T) {
	t.Run("单条消息超过 CallbackBytes 在预留点拒绝", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xB620, func(l *Limits) { l.CallbackBytes = 96 })
		token := rig.creator.exToken(0)
		s := inst.inst.state

		oversized := bytes.Repeat([]byte{'a'}, 120)
		native.LookupCallbackDelivery(token)(80, callbackCBytes(string(oversized)))
		fault := s.currentFault()
		if fault == nil || !errors.Is(fault, native.ErrEventOverflow) {
			t.Fatalf("单条超限的首因 = %v, want errors.Is ErrEventOverflow", fault)
		}
		if lost := s.currentLostEvents(); lost != 1 {
			t.Errorf("丢失计数 = %d, want 1(单条超限未交付)", lost)
		}
		if n, b := internalBudgetHold(inst.inst.disp); n != 0 || b != 0 {
			t.Errorf("内部预算占用 = (%d, %d), want (0, 0)(预留点拒绝,不占额度)", n, b)
		}
		// 非阻塞检查(实例未关闭,通道开放):超限消息不得产生公开事件。
		select {
		case ev := <-inst.inst.disp.events():
			t.Errorf("超限消息不得产生公开事件: %v", ev)
		default:
		}
		// 故障后新提交被拒,错误链可达首因。
		if op, err := inst.Screencap(); op != nil || !errors.Is(err, fault) {
			t.Fatalf("溢出故障后的提交 = (%+v, %v), want 沿链可达首因", op, err)
		}
		// Close 在 native 正常返回下完成,返回首因身份,销毁恰一次。
		if err := inst.Close(); err == nil || !errors.Is(err, native.ErrEventOverflow) {
			t.Fatalf("Close = %v, want 与首因同一身份", err)
		}
		assertSingleStopDestroy(t, rig.closer, 0xB620)
	})

	t.Run("公开缓冲字节预算在消费者停读时溢出且故障通知可见", func(t *testing.T) {
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
		waitUntil(t, "四条溢出已全部按条计入丢失", func() bool {
			return s.currentLostEvents() == 4
		})
		fault := s.currentFault()
		if fault == nil || !errors.Is(fault, native.ErrEventOverflow) {
			t.Fatalf("首因 = %v, want errors.Is ErrEventOverflow", fault)
		}

		// 故障通知独立于 Events:未读任何事件即可观察到已关闭。
		select {
		case <-s.faultNotify():
		default:
			t.Error("溢出故障的通知未关闭(消费者完全停读下仍必须可见)")
		}
		// 首因 sticky:重复溢出只累计丢失,不覆盖首因。
		if s.currentFault() != fault {
			t.Errorf("首因被后到溢出覆盖: %v", s.currentFault())
		}

		// Close 在 native 正常返回的前提下完成;返回首因身份,销毁恰
		// 一次;pending 得到结算。
		if err := inst.Close(); err == nil || !errors.Is(err, native.ErrEventOverflow) {
			t.Fatalf("Close = %v, want 与首因同一身份", err)
		}
		assertSingleStopDestroy(t, rig.closer, 0xB621)
		if n := pendingOccupancy(s); n != 0 {
			t.Errorf("关闭结算后 pending 占用 = %d, want 0", n)
		}
		// 已确认的完成结果不被随后的溢出改写;未完成项按首因结项。
		res1, err1 := waitOperationGuarded(t, op1, context.Background())
		if err1 != nil || res1.ID != 1 || res1.What != "Screencap" || !res1.OK {
			t.Fatalf("op1 终态 = (%+v, %v), want 成功结果不被溢出改写", res1, err1)
		}
		res2, err2 := waitOperationGuarded(t, op2, context.Background())
		if err2 != nil || res2.ID != 2 || !res2.OK {
			t.Fatalf("op2 终态 = (%+v, %v), want 成功结果不被溢出改写", res2, err2)
		}
		res3, err3 := waitOperationGuarded(t, op3, context.Background())
		if err3 == nil || !errors.Is(err3, fault) {
			t.Fatalf("op3 结算错误 = %v, want 与首因同一身份", err3)
		}
		if res3.ID != 0 || res3.OK {
			t.Errorf("op3 的故障结算必须无结果: %+v", res3)
		}
		// 已在公开缓冲中的两条事件关闭后仍可读且逐字节一致;随后已关闭。
		ev1 := mustReadEvent(t, inst.inst.disp.events())
		ev2 := mustReadEvent(t, inst.inst.disp.events())
		if !bytes.Equal(ev1.JSON, asyncCallInfoPayload(1, "Screencap", `{"ret":true}`)) {
			t.Errorf("缓冲事件 1 = %s, want 完成原文逐字节一致", ev1.JSON)
		}
		if !bytes.Equal(ev2.JSON, asyncCallInfoPayload(2, "Connect", `{"ret":true}`)) {
			t.Errorf("缓冲事件 2 = %s, want 完成原文逐字节一致", ev2.JSON)
		}
		if _, ok := <-inst.inst.disp.events(); ok {
			t.Error("Close 后 Events 应已关闭")
		}
	})
}

// TestIntegrationStableTerminalStatesAndSnapshotIsolation 钉住验收条件
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
		waitUntil(t, "早到结果已临存进窗口", func() bool {
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
		t.Fatalf("op2 终态 = (%+v, %v), want 早到接合成功", res2, err2)
	}
	ev2 := mustReadEvent(t, inst.inst.disp.events())
	if ev2.MessageID != asyncCallInfoMessageID {
		t.Fatalf("公开事件 MessageID = %d, want AsyncCallInfo(%d)", ev2.MessageID, asyncCallInfoMessageID)
	}
	for i := range ev2.JSON {
		ev2.JSON[i] = 'x'
	}
	for i := range res2.Details {
		res2.Details[i] = 'y'
	}
	res2again, err2again := waitOperationGuarded(t, op2, context.Background())
	if err2again != nil || string(res2again.Details) != op2Details {
		t.Fatalf("改写副本后的再次等待 = (%+v, %v), want 缓存终态不受污染", res2again, err2again)
	}

	// op1 晚到 ret=false 完成结算;改写过的先前副本不影响后续投递的
	// 逐字节保真。
	op1Details := `{"ret":false}`
	deliverAsyncInfo(t, token, 1, "Screencap", op1Details)
	res1, err1 := waitOperationGuarded(t, op1, context.Background())
	if !errors.Is(err1, native.ErrNativeRejected) || res1.ID != 1 || res1.OK {
		t.Fatalf("op1 终态 = (%+v, %v), want 原生失败", res1, err1)
	}
	if string(res1.Details) != op1Details {
		t.Fatalf("op1 结果 = %s, want 逐字节保留的 %s", res1.Details, op1Details)
	}
	ev1 := mustReadEvent(t, inst.inst.disp.events())
	if !bytes.Equal(ev1.JSON, asyncCallInfoPayload(1, "Screencap", op1Details)) {
		t.Fatalf("后续投递事件 = %s, want 逐字节相等的 %s(先前副本改写不得穿透)", ev1.JSON, asyncCallInfoPayload(1, "Screencap", op1Details))
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
		t.Error("Close 返回后 Done 未关闭")
	}
	if got := s.finalError(); got != nil {
		t.Errorf("最终错误 = %v, want nil", got)
	}
	if err := inst.Close(); err != nil {
		t.Errorf("幂等 Close = %v, want 与首次同一终态 nil", err)
	}
	if _, ok := <-inst.inst.disp.events(); ok {
		t.Error("Close 后 Events 应已关闭")
	}
	res1b, err1b := waitOperationGuarded(t, op1, context.Background())
	if !errors.Is(err1b, native.ErrNativeRejected) || res1b.ID != 1 || string(res1b.Details) != op1Details {
		t.Errorf("关闭后 op1 终态 = (%+v, %v), want 与关闭前一致", res1b, err1b)
	}
	res2b, err2b := waitOperationGuarded(t, op2, context.Background())
	if err2b != nil || res2b.ID != 2 || string(res2b.Details) != op2Details {
		t.Errorf("关闭后 op2 终态 = (%+v, %v), want 与关闭前一致", res2b, err2b)
	}
}
