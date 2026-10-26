// 本文件是异步提交与结果关联的受控用例(MAA-23;绑定设计 §5、§7、
// §7.1):白盒同包验证公开异步入口的提交与关联契约——Connect/Click/
// Screencap 以 block=false 触达三个异步 raw 入口,非零 ID 只表示提交
// 被接受;完成回调先于 raw 返回时按 token + ID + 预期 what 接合,晚
// 到结果由统一 dispatcher 结算,不依赖消息到达顺序;ID 0 是明确的提
// 交错误并回收预留,不发布成功 Operation;pending/early 表受冻结容
// 量限制,重复或不可解释的完成信息以 ErrProtocol 故障,不伪造成功;
// Wait 取消与调用者丢弃不提前回收容量,晚到结果仍结算一次;关闭与
// 最终结果竞争时只结算一次,Operation 完成不依赖外部读取 Events。
// native 异步提交以 fakeInstanceCaller 的 async* 方法注入
// (instance_create_test.go 的接缝先例),完成消息经真实 token 注册
// 表查表投递(LookupCallbackDelivery → target.deliver),早到时序以
// onAsync* 钩子在 raw 调用内确定性放置。全部用例以受控屏障
// (channel/轮询等待)确定性同步,不加载任何原生库、不触达 loader/
// 真实 API 表,CGO_ENABLED=0 可跑;防挂死只用 hangGuard 兜底,不作
// 同步手段。
//
// maa→native 的 token 注册表是进程级共享状态:本文件用例互不并行
// (不调 t.Parallel);成功创建的实例由用例显式 Close 或经 rig.cleanup
// 兜底收尾,不留悬挂 goroutine 与未注销 token 影响 -race。

package maa

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// newAsyncRig 构造带 fake caller 的受控异步用例环境:与 newSyncRig
// 同一生产创建事务接线,handle 为给定占位值,Limits 经 mutate 调整
// (mutate 为 nil 时用整套默认值)。
func newAsyncRig(t *testing.T, handle uintptr, mutate func(*Limits)) (*Instance, *createRig) {
	t.Helper()
	gate := &lifecycleGate{state: lifecycleReady}
	limits := defaultLimits()
	if mutate != nil {
		mutate(&limits)
	}
	rt := &Runtime{
		caps:     native.Capabilities{},
		version:  "fake",
		nullSize: profileNullSize,
		limits:   limits,
		gate:     gate,
	}
	creator := newFakeInstanceCreator(handle)
	closer := newFakeInstanceCloser()
	caller := newFakeInstanceCaller()
	rig := &createRig{rt: rt, gate: gate, creator: creator, closer: closer, caller: caller}
	t.Cleanup(func() { rig.cleanup() })
	inst, err := runNewInstance(t, rt, creator, closer, caller, InstanceConfig{})
	if err != nil {
		t.Fatalf("NewInstance = %v, want nil", err)
	}
	rig.track(inst)
	return inst, rig
}

// deliverAsyncInfo 经真实 token 注册表查表投递一条 AsyncCallInfo 完成
// 消息:payload 带 NUL 终止(callback 准入按有界扫描寻找 NUL)。
func deliverAsyncInfo(t *testing.T, token uintptr, id AsyncID, what, details string) {
	t.Helper()
	deliver := native.LookupCallbackDelivery(token)
	if deliver == nil {
		t.Fatalf("token %d 未在注册表登记,无法投递完成消息", token)
	}
	deliver(int32(asyncCallInfoMessageID), callbackCBytes(string(asyncCallInfoPayload(id, what, details))))
}

// earlyWindowView 是早到结果窗口的白盒快照(锁内读取,不绕锁)。
type earlyWindowView struct {
	open      bool
	has       bool
	id        AsyncID
	what      string
	watermark AsyncID
}

func earlyWindowOf(s *instanceState) earlyWindowView {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := earlyWindowView{open: s.earlyWindowOpen, watermark: s.asyncWatermark}
	if s.earlyResult != nil {
		v.has = true
		v.id = s.earlyResult.ID
		v.what = s.earlyResult.What
	}
	return v
}

// pendingReservedOf 在状态锁内读取已预留、尚未登记的提交槽数(白盒
// 诊断读取)。
func pendingReservedOf(s *instanceState) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingReserved
}

// waitOperationGuarded 在独立 goroutine 中执行一次公开 Wait 并以防挂
// 死上限兜底(对照 waitGuarded 先例)。
func waitOperationGuarded(t *testing.T, op *Operation, ctx context.Context) (AsyncResult, error) {
	t.Helper()
	type outcome struct {
		res AsyncResult
		err error
	}
	ch := make(chan outcome, 1)
	go func() {
		res, err := op.Wait(ctx)
		ch <- outcome{res, err}
	}()
	select {
	case o := <-ch:
		return o.res, o.err
	case <-time.After(hangGuard):
		t.Fatal("Operation.Wait 未在防挂死上限内返回:done 通知疑似未关闭")
		return AsyncResult{}, nil
	}
}

// connectInput 是异步用例的合法连接输入(Config 允许空串)。
var connectInput = ConnectConfig{ADBPath: "/usr/bin/adb", Address: "127.0.0.1:5555", Config: ""}

// TestAsyncEntriesForwardBlockFalseAndAcceptedID 钉住验收条件「三个
// 异步 raw 入口均传 block=false,非零 ID 只表示提交被接受」:三个入
// 口的 fake 记录 block 均为 0、handle 来自已准入 lease,连接参数逐字
// 节透传(空 Config 解码为空串而非 nil);非零 ID 返回 (op, nil) 且
// op.ID 与返回 ID 一致——在完成消息到达之前,等待取得取消态与零值
// 结果(ID 不是成功证明),完成到达后才取得真实终态。
func TestAsyncEntriesForwardBlockFalseAndAcceptedID(t *testing.T) {
	const handle = uintptr(0xF100)
	inst, rig := newAsyncRig(t, handle, nil)
	rig.caller.asyncRets = map[string]int32{"asyncConnect": 1, "asyncClick": 2, "asyncScreencap": 3}

	op, err := inst.Connect(connectInput)
	if err != nil {
		t.Fatalf("Connect = %v, want nil", err)
	}
	if got := rig.caller.count("asyncConnect"); got != 1 {
		t.Fatalf("asyncConnect 调用次数 = %d, want 1", got)
	}
	rec := rig.caller.asyncRecordByName(0, "asyncConnect")
	if rec.connect == nil {
		t.Fatal("asyncConnect 调用记录缺失")
	}
	if rec.connect.handle != handle {
		t.Errorf("asyncConnect 收到 handle %#x, want %#x", rec.connect.handle, handle)
	}
	if rec.connect.block != 0 {
		t.Errorf("asyncConnect block = %d, want 0(冻结 block=false)", rec.connect.block)
	}
	if rec.connect.adbPath != connectInput.ADBPath || rec.connect.address != connectInput.Address {
		t.Errorf("asyncConnect 连接参数被改写: adb=%q address=%q, want %q/%q",
			rec.connect.adbPath, rec.connect.address, connectInput.ADBPath, connectInput.Address)
	}
	if rec.connect.config != "" {
		t.Errorf("asyncConnect 空 Config 解码 = %q, want 空串(原样传空 C 字符串,不传 nil)", rec.connect.config)
	}
	if op.ID() != 1 {
		t.Errorf("Connect 返回 ID = %d, want 1(fake 注入的非零 ID)", op.ID())
	}

	// 非零 ID 只表示提交被接受:完成尚未到达,取消的等待返回零值结
	// 果与 ctx.Err(),不得到任何成功结果。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := waitOperationGuarded(t, op, ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("完成前的等待错误 = %v, want context.Canceled", err)
	}
	if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
		t.Fatalf("完成前的等待必须返回零值结果: %+v", res)
	}

	// Click 与 Screencap:同样 block=false,坐标带符号透传。
	if _, err := inst.Click(-5, 7); err != nil {
		t.Fatalf("Click = %v, want nil", err)
	}
	clickRec := rig.caller.asyncRecordByName(0, "asyncClick")
	if clickRec.click == nil || clickRec.click.block != 0 || clickRec.click.x != -5 || clickRec.click.y != 7 {
		t.Errorf("asyncClick 记录 = %+v, want block=0、x=-5、y=7 原样透传", clickRec.click)
	}
	if _, err := inst.Screencap(); err != nil {
		t.Fatalf("Screencap = %v, want nil", err)
	}
	capRec := rig.caller.asyncRecordByName(0, "asyncScreencap")
	if capRec.screencap == nil || capRec.screencap.block != 0 {
		t.Errorf("asyncScreencap 记录 = %+v, want block=0", capRec.screencap)
	}

	// 完成到达后取得真实终态:ID 关联到正确的 Operation。
	token := rig.creator.exToken(0)
	deliverAsyncInfo(t, token, 1, "Connect", `{"ret":true,"cost":7}`)
	res, err = waitOperationGuarded(t, op, context.Background())
	if err != nil {
		t.Fatalf("完成后的等待 = %v, want nil", err)
	}
	if res.ID != 1 || res.What != "Connect" || !res.OK || string(res.Details) != `{"ret":true,"cost":7}` {
		t.Fatalf("完成结果 = %+v, want {1 Connect true {\"ret\":true,\"cost\":7}}", res)
	}
	if got := watermarkOfState(inst.inst.state); got != 3 {
		t.Errorf("提交高水位 = %d, want 3(三笔已接受提交严格递增)", got)
	}
}

// watermarkOfState 在状态锁内读取异步提交高水位(白盒诊断读取)。
func watermarkOfState(s *instanceState) AsyncID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asyncWatermark
}

// TestAsyncEarlyJoinByIDAndExpectedWhat 钉住验收条件「完成回调先于
// raw 返回时能按 token + ID 关联,不依赖消息到达顺序」:完成消息在
// raw 调用内投递并临存进早到窗口(白盒确认),raw 返回后按 ID 与预
// 期 what 接合——Connect 返回时 Operation 已带完成终态,等待立即返
// 回;窗口与早到表立即清空,不把残留留给未来提交。
func TestAsyncEarlyJoinByIDAndExpectedWhat(t *testing.T) {
	const handle = uintptr(0xF110)
	inst, rig := newAsyncRig(t, handle, nil)
	token := rig.creator.exToken(0)
	s := inst.inst.state

	rig.caller.onAsyncConnect = func(uintptr) {
		deliverAsyncInfo(t, token, 1, "Connect", `{"ret":true,"cost":3}`)
		waitUntil(t, "早到结果已临存进窗口", func() bool {
			v := earlyWindowOf(s)
			return v.open && v.has && v.id == 1 && v.what == "Connect"
		})
	}

	op, err := inst.Connect(connectInput)
	if err != nil {
		t.Fatalf("Connect = %v, want nil", err)
	}
	if op.ID() != 1 {
		t.Fatalf("Connect 返回 ID = %d, want 1", op.ID())
	}
	// 接合完成:等待立即返回终态,不依赖任何消息时序。
	res, err := waitOperationGuarded(t, op, context.Background())
	if err != nil {
		t.Fatalf("早到接合后的等待 = %v, want nil", err)
	}
	if res.ID != 1 || res.What != "Connect" || !res.OK || string(res.Details) != `{"ret":true,"cost":3}` {
		t.Fatalf("早到接合结果 = %+v, want 完成终态", res)
	}
	// 窗口关闭、早到表清空、pending 表回收、高水位推进。
	if v := earlyWindowOf(s); v.open || v.has {
		t.Errorf("接合后早到窗口未清空: %+v", v)
	}
	if n := pendingOccupancy(s); n != 0 {
		t.Errorf("接合后 pending 占用 = %d, want 0(容量已回收)", n)
	}
	if got := watermarkOfState(s); got != 1 {
		t.Errorf("提交高水位 = %d, want 1", got)
	}

	// 早到 what 与预期不符:ErrProtocol 故障,不伪造成功(§7.1:
	// ID/what 不符)。fake 返回与早到结果相同的 ID(2):raw 返回后
	// join 的 ID 检查通过,故障精确来自预期 what 比对分支,而非相邻
	// 的高水位或 join ID 分支。
	rig.caller.asyncRets = map[string]int32{"asyncConnect": 2}
	rig.caller.onAsyncConnect = func(uintptr) {
		deliverAsyncInfo(t, token, 2, "Click", `{"ret":true}`)
		waitUntil(t, "不匹配早到结果已临存", func() bool {
			v := earlyWindowOf(s)
			return v.open && v.has && v.id == 2 && v.what == "Click"
		})
	}
	if op2, err := inst.Connect(connectInput); err == nil {
		t.Fatalf("早到 what 不符时 Connect = (%+v, nil), want 协议故障", op2)
	} else if !errors.Is(err, native.ErrProtocol) {
		t.Fatalf("早到 what 不符的错误 = %v, want errors.Is ErrProtocol", err)
	} else if !strings.Contains(err.Error(), `reports what "Click", want "Connect"`) {
		t.Fatalf("早到 what 不符的错误未指明 what 比对:\n%s", err.Error())
	}
	if got := s.currentFault(); got == nil || !errors.Is(got, native.ErrProtocol) ||
		!strings.Contains(got.Error(), `reports what "Click", want "Connect"`) {
		t.Errorf("首因 = %v, want 来自预期 what 比对分支的 ErrProtocol", got)
	}
	if n := pendingOccupancy(s); n != 0 {
		t.Errorf("故障扫描后 pending 占用 = %d, want 0", n)
	}
}

// TestAsyncCompletionArrivalOrders 钉住验收条件「不依赖消息到达顺
// 序」:旧提交的完成在新提交的 raw 窗口内到达(pending 命中,窗口不
// 受影响)、新提交自身的早到接合、以及完成消息晚于 raw 返回到达,三
// 种时序混排都按 ID 关联到正确请求,且不要求完成消息按 ID 顺序到达。
func TestAsyncCompletionArrivalOrders(t *testing.T) {
	const handle = uintptr(0xF120)

	t.Run("旧提交完成在新窗口内到达(到达顺序 11→12)", func(t *testing.T) {
		inst, rig := newAsyncRig(t, handle, nil)
		rig.caller.asyncRets = map[string]int32{"asyncConnect": 11, "asyncClick": 12}
		token := rig.creator.exToken(0)
		s := inst.inst.state

		opConnect, err := inst.Connect(connectInput)
		if err != nil {
			t.Fatalf("Connect = %v, want nil", err)
		}
		// Click 的 raw 窗口内投递 Connect 的完成(11):pending 命中,
		// 不进早到窗口、不影响 Click 的窗口。
		rig.caller.onAsyncClick = func(uintptr) {
			deliverAsyncInfo(t, token, 11, "Connect", `{"ret":true}`)
			waitUntil(t, "Connect 的完成已在窗口内结算 opConnect", func() bool {
				select {
				case <-opConnect.op.done:
					return true
				default:
					return false
				}
			})
		}
		opClick, err := inst.Click(3, 4)
		if err != nil {
			t.Fatalf("Click = %v, want nil", err)
		}
		res, err := waitOperationGuarded(t, opConnect, context.Background())
		if err != nil || res.ID != 11 || res.What != "Connect" || !res.OK {
			t.Fatalf("opConnect 终态 = (%+v, %v), want 完成结果", res, err)
		}
		// Click 的完成按 ID 顺序晚到:统一 dispatcher 结算。
		deliverAsyncInfo(t, token, 12, "Click", `{"ret":false}`)
		res, err = waitOperationGuarded(t, opClick, context.Background())
		if !errors.Is(err, native.ErrNativeRejected) {
			t.Fatalf("opClick 终态错误 = %v, want ErrNativeRejected(ret=false)", err)
		}
		if res.ID != 12 || res.What != "Click" || res.OK {
			t.Fatalf("opClick 结果 = %+v, want {12 Click false}", res)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Errorf("两笔结算后 pending 占用 = %d, want 0", n)
		}
		if got := watermarkOfState(s); got != 12 {
			t.Errorf("提交高水位 = %d, want 12", got)
		}
	})

	t.Run("新提交早到接合后旧提交完成晚到(到达顺序 22→21)", func(t *testing.T) {
		inst, rig := newAsyncRig(t, handle+1, nil)
		rig.caller.asyncRets = map[string]int32{"asyncConnect": 21, "asyncClick": 22}
		token := rig.creator.exToken(0)
		s := inst.inst.state

		opConnect, err := inst.Connect(connectInput)
		if err != nil {
			t.Fatalf("Connect = %v, want nil", err)
		}
		// Click 的 raw 窗口内投递 Click 自身的完成(22):早到临存,
		// raw 返回后接合。
		rig.caller.onAsyncClick = func(uintptr) {
			deliverAsyncInfo(t, token, 22, "Click", `{"ret":true,"cost":1}`)
			waitUntil(t, "Click 的完成已临存进窗口", func() bool {
				v := earlyWindowOf(s)
				return v.open && v.has && v.id == 22
			})
		}
		opClick, err := inst.Click(1, 2)
		if err != nil {
			t.Fatalf("Click = %v, want nil", err)
		}
		// 早到接合:opClick 返回时已带终态。
		res, err := waitOperationGuarded(t, opClick, context.Background())
		if err != nil || res.ID != 22 || !res.OK {
			t.Fatalf("opClick 早到接合 = (%+v, %v), want 完成结果", res, err)
		}
		// Connect 的完成(21)按逆序晚到:pending 命中照常结算。
		deliverAsyncInfo(t, token, 21, "Connect", `{"ret":false}`)
		res, err = waitOperationGuarded(t, opConnect, context.Background())
		if !errors.Is(err, native.ErrNativeRejected) {
			t.Fatalf("opConnect 终态错误 = %v, want ErrNativeRejected", err)
		}
		if res.ID != 21 || res.OK {
			t.Fatalf("opConnect 结果 = %+v, want {21 Connect false}", res)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Errorf("两笔结算后 pending 占用 = %d, want 0", n)
		}
	})
}

// TestAsyncZeroIDReclaimsReservation 钉住验收条件「ID 0 返回明确提交
// 错误并回收本次预留,不发布成功 Operation」:raw 返回 0 时得到包装
// ErrNativeRejected 的提交错误,预留计数与 pending 表回收,实例不因
// 此故障;窗口内已有早到结果时,矛盾证据以 ErrProtocol 故障保留,不
// 被丢弃。
func TestAsyncZeroIDReclaimsReservation(t *testing.T) {
	t.Run("ID 0 返回 ErrNativeRejected 并回收预留", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF130, nil)
		s := inst.inst.state
		rig.caller.asyncRets = map[string]int32{"asyncScreencap": 0}

		op, err := inst.Screencap()
		if op != nil {
			t.Fatalf("ID 0 不得返回成功 Operation: %+v", op)
		}
		if !errors.Is(err, native.ErrNativeRejected) {
			t.Fatalf("ID 0 的错误 = %v, want errors.Is ErrNativeRejected", err)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Errorf("ID 0 后 pending 占用 = %d, want 0", n)
		}
		if n := pendingReservedOf(s); n != 0 {
			t.Errorf("ID 0 后预留计数 = %d, want 0(预留已回收)", n)
		}
		if v := earlyWindowOf(s); v.open || v.has {
			t.Errorf("ID 0 后早到窗口未关闭: %+v", v)
		}
		// 实例不因提交失败而故障:阶段仍 ready,后续提交照常接受。
		if got := s.currentPhase(); got != phaseReady {
			t.Errorf("阶段 = %d, want 仍为 ready", got)
		}
		if s.currentFault() != nil {
			t.Errorf("首因 = %v, want nil", s.currentFault())
		}
		select {
		case <-s.faultNotify():
			t.Error("提交失败不得触发一次性故障通知")
		default:
		}
		if op2, err := inst.Click(1, 1); err != nil || op2.ID() != 1 {
			t.Errorf("后续提交 = (%+v, %v), want 正常接受", op2, err)
		}
	})

	t.Run("ID 0 且窗口已有早到结果为 ErrProtocol 故障", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF131, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state
		rig.caller.asyncRets = map[string]int32{"asyncClick": 0}
		rig.caller.onAsyncClick = func(uintptr) {
			deliverAsyncInfo(t, token, 9, "Click", `{"ret":true}`)
			waitUntil(t, "早到结果已临存进窗口", func() bool {
				v := earlyWindowOf(s)
				return v.open && v.has && v.id == 9
			})
		}

		op, err := inst.Click(1, 1)
		if op != nil {
			t.Fatalf("矛盾证据下不得返回成功 Operation: %+v", op)
		}
		if !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("矛盾证据的错误 = %v, want errors.Is ErrProtocol", err)
		}
		if got := s.currentFault(); got == nil || !errors.Is(got, native.ErrProtocol) {
			t.Errorf("首因 = %v, want ErrProtocol", got)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Errorf("故障扫描后 pending 占用 = %d, want 0", n)
		}
		if n := pendingReservedOf(s); n != 0 {
			t.Errorf("故障扫描后预留计数 = %d, want 0", n)
		}
	})
}

// errTheAsyncSubmissionRemoved 占位:已移除的辅助哨兵。

// TestAsyncProtocolViolationsFaultInstance 钉住验收条件「pending/
// early 表受冻结容量限制,重复或不可解释结果不能无限缓存或伪造成
// 功」:早到窗口的第二个结果(含重复)为 ErrProtocol 故障;ID 复用/
// 倒退与负值 ID 为 ErrProtocol 故障;已接受 ID 的重复完成不能伪造新
// 的成功;早到结果字节超 EarlyBytes 为 ErrEventOverflow 故障且不缓
// 存。故障保留首因,pending 表由首因扫描清空。
func TestAsyncProtocolViolationsFaultInstance(t *testing.T) {
	t.Run("早到窗口的第二个结果故障且提交不登记", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF140, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state
		rig.caller.onAsyncConnect = func(uintptr) {
			deliverAsyncInfo(t, token, 3, "Connect", `{"ret":true}`)
			waitUntil(t, "第一个早到结果已临存", func() bool {
				return earlyWindowOf(s).has
			})
			// 第二个早到结果(含重复):ErrProtocol 故障。故障必须先
			// 于 raw 返回发生(分支隔离:settleCompletion 的第二早到
			// 分支,而非 finishAsyncRaw 的接合不匹配分支),且首因文本
			// 指明第二早到,不得以覆盖缓存代替故障。
			deliverAsyncInfo(t, token, 4, "Click", `{"ret":true}`)
			waitUntil(t, "第二个早到结果已在窗口内触发首因", func() bool {
				f := s.currentFault()
				return f != nil && strings.Contains(f.Error(), "second early completion")
			})
			if v := earlyWindowOf(s); !v.has || v.id != 3 {
				t.Errorf("第二早到故障后窗口内容被改写: %+v, want 首条早到(id 3)原样保留作矛盾证据", v)
			}
		}

		op, err := inst.Connect(connectInput)
		if op != nil {
			t.Fatalf("第二个早到结果下不得返回成功 Operation: %+v", op)
		}
		if !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("错误 = %v, want errors.Is ErrProtocol", err)
		}
		if got := s.currentFault(); got == nil || !errors.Is(got, native.ErrProtocol) ||
			!strings.Contains(got.Error(), "second early completion") {
			t.Errorf("首因 = %v, want 来自第二早到分支的 ErrProtocol", got)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Errorf("故障扫描后 pending 占用 = %d, want 0", n)
		}
		if n := pendingReservedOf(s); n != 0 {
			t.Errorf("故障扫描后预留计数 = %d, want 0", n)
		}
	})

	t.Run("ID 复用与负值返回为 ErrProtocol 故障", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF141, nil)
		s := inst.inst.state

		if op, err := inst.Connect(connectInput); err != nil || op.ID() != 1 {
			t.Fatalf("首次 Connect = (%+v, %v), want 接受", op, err)
		}
		// 复用/倒退:不严格大于高水位 1。
		rig.caller.asyncRets = map[string]int32{"asyncClick": 1}
		if op, err := inst.Click(0, 0); op != nil || !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("ID 复用 = (%+v, %v), want ErrProtocol", op, err)
		}
		if got := s.currentFault(); got == nil || !errors.Is(got, native.ErrProtocol) {
			t.Errorf("ID 复用后的首因 = %v, want ErrProtocol", got)
		}
		// 负值 ID:违反 1..MaxInt32 接受域。
		rig.caller.asyncRets = map[string]int32{"asyncScreencap": -3}
		if op, err := inst.Screencap(); op != nil || !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("负值 ID = (%+v, %v), want ErrProtocol", op, err)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Errorf("故障扫描后 pending 占用 = %d, want 0", n)
		}
		if n := pendingReservedOf(s); n != 0 {
			t.Errorf("故障扫描后预留计数 = %d, want 0", n)
		}
	})

	t.Run("已接受 ID 的重复完成不能伪造新的成功", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF142, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state

		op, err := inst.Screencap()
		if err != nil {
			t.Fatalf("Screencap = %v, want nil", err)
		}
		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":true}`)
		if res, err := waitOperationGuarded(t, op, context.Background()); err != nil || res.ID != 1 {
			t.Fatalf("首次完成 = (%+v, %v), want 成功", res, err)
		}
		// 同一 ID 的重复完成:ErrProtocol 故障,不伪造新的成功。
		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":true}`)
		waitUntil(t, "重复完成已触发首因", func() bool {
			return s.currentFault() != nil
		})
		if got := s.currentFault(); !errors.Is(got, native.ErrProtocol) {
			t.Errorf("首因 = %v, want ErrProtocol", got)
		}
		// 已结算的终态不被重复完成改写。
		res, err := waitOperationGuarded(t, op, context.Background())
		if err != nil || res.ID != 1 || !res.OK {
			t.Errorf("重复完成改写了终态: (%+v, %v)", res, err)
		}
	})

	t.Run("早到结果字节超 EarlyBytes 以 ErrEventOverflow 故障且不缓存", func(t *testing.T) {
		// 经 validateLimits 的交叉校验(CallbackBytes ≤ EarlyBytes),
		// 真实提交路径上该分支防御性可达;此处以白盒窗口构造。
		s := newInstanceState(0xF143, 8, 4)
		s.publishReady()
		if err := s.beginAsyncRaw(); err != nil {
			t.Fatalf("beginAsyncRaw = %v, want nil", err)
		}
		s.settleCompletion(completionInfo{ID: 5, What: "Click", OK: true, Details: nil, Bytes: 100})
		if got := s.currentFault(); got == nil || !errors.Is(got, native.ErrEventOverflow) {
			t.Fatalf("首因 = %v, want ErrEventOverflow", got)
		}
		if v := earlyWindowOf(s); v.has {
			t.Errorf("超限早到结果不得缓存: %+v", v)
		}
	})
}

// TestAsyncPendingLimitReclaimsAndRejects 钉住验收条件「pending 表受
// 冻结容量限制」:未结清 Operation 数达上限时,新提交在进入 raw 前
// 被拒(ErrOperationLimit,无原生副作用,实例不故障);晚到完成结算
// 回收容量后,提交恢复接受。
func TestAsyncPendingLimitReclaimsAndRejects(t *testing.T) {
	inst, rig := newAsyncRig(t, 0xF150, func(l *Limits) { l.PendingOperations = 1 })
	rig.caller.asyncRets = map[string]int32{"asyncScreencap": 1, "asyncClick": 2}
	s := inst.inst.state

	op, err := inst.Screencap()
	if err != nil {
		t.Fatalf("Screencap = %v, want nil", err)
	}
	if n := pendingOccupancy(s); n != 1 {
		t.Fatalf("登记后 pending 占用 = %d, want 1", n)
	}
	// 容量满:拒绝发生在预留点,不调用 native。
	before := rig.caller.total()
	if op2, err := inst.Connect(connectInput); op2 != nil || !errors.Is(err, native.ErrOperationLimit) {
		t.Fatalf("容量满的提交 = (%+v, %v), want ErrOperationLimit", op2, err)
	}
	if got := rig.caller.total(); got != before {
		t.Errorf("容量满后 native 调用次数变化 %d → %d, want 不变(不调用 native)", before, got)
	}
	if got := s.currentPhase(); got != phaseReady {
		t.Errorf("容量拒绝后阶段 = %d, want 仍为 ready", got)
	}
	if s.currentFault() != nil {
		t.Errorf("容量拒绝后首因 = %v, want nil", s.currentFault())
	}
	// 晚到完成结算并回收容量:提交恢复接受。
	deliverAsyncInfo(t, rig.creator.exToken(0), 1, "Screencap", `{"ret":true}`)
	if res, err := waitOperationGuarded(t, op, context.Background()); err != nil || res.ID != 1 {
		t.Fatalf("晚到完成结算 = (%+v, %v), want 成功", res, err)
	}
	if op3, err := inst.Click(2, 2); err != nil || op3.ID() != 2 {
		t.Errorf("容量回收后的提交 = (%+v, %v), want 接受", op3, err)
	}
}

// TestPendingReservationAccounting 钉住预留原语的容量记账与阶段拒
// 绝:预留计入占用(len(pending)+pendingReserved),释放后可再预留;
// closing/faulted/closed 阶段拒绝;双重释放为编程错误 panic。
func TestPendingReservationAccounting(t *testing.T) {
	t.Run("预留计入占用,释放后可再预留", func(t *testing.T) {
		s := newReadyState(t, 0xF151, 2)
		for i := 0; i < 2; i++ {
			if _, err := s.reservePendingOperation("预留"); err != nil {
				t.Fatalf("第 %d 次预留 = %v, want nil", i+1, err)
			}
		}
		if _, err := s.reservePendingOperation("第三次"); !errors.Is(err, native.ErrOperationLimit) {
			t.Fatalf("第三次预留 = %v, want ErrOperationLimit", err)
		}
		if _, err := s.admitPendingOperation(9, "直接准入"); !errors.Is(err, native.ErrOperationLimit) {
			t.Fatalf("预留占用下的直接准入 = %v, want ErrOperationLimit", err)
		}
		s.releasePendingReservation()
		if _, err := s.reservePendingOperation("回收后再预留"); err != nil {
			t.Fatalf("释放后的预留 = %v, want nil", err)
		}
		// 此刻占用 = 2(两笔未释放预留):再连发三次释放,前两次归位、
		// 第三次下溢,按编程错误 panic。
		assertPanics(t, "双重释放预留", func() {
			s.releasePendingReservation()
			s.releasePendingReservation()
			s.releasePendingReservation()
		})
	})

	t.Run("closing、faulted、closed 阶段拒绝预留", func(t *testing.T) {
		closing := newReadyState(t, 0xF152, 8)
		mustElectClose(t, closing)
		if _, err := closing.reservePendingOperation("closing"); !errors.Is(err, native.ErrClosing) {
			t.Fatalf("closing 预留 = %v, want ErrClosing", err)
		}
		faulted := newReadyState(t, 0xF153, 8)
		cause := fmt.Errorf("故障: %w", native.ErrProtocol)
		faulted.recordFault(cause)
		if _, err := faulted.reservePendingOperation("faulted"); !errors.Is(err, cause) {
			t.Fatalf("faulted 预留 = %v, want 沿链可达首因", err)
		}
		closed := newClosedState(t, 0xF154)
		if _, err := closed.reservePendingOperation("closed"); !errors.Is(err, native.ErrClosed) {
			t.Fatalf("closed 预留 = %v, want ErrClosed", err)
		}
		initializing := newInstanceState(0xF155, 8, 1_048_576)
		assertPanics(t, "initializing 阶段预留", func() { _, _ = initializing.reservePendingOperation("未发布") })
	})

	t.Run("高水位 MaxInt32 在门禁内、进入 raw 前故障并回收预留", func(t *testing.T) {
		s := newReadyState(t, 0xF156, 8)
		// 先预留一次:耗尽故障路径必须回收该预留(§7.1:每份预留在
		// 拒绝/结算竞争中只回收一次),否则计数永久滞留。
		if _, err := s.reservePendingOperation("耗尽前的预留"); err != nil {
			t.Fatalf("reservePendingOperation = %v, want nil", err)
		}
		s.mu.Lock()
		s.asyncWatermark = math.MaxInt32
		s.mu.Unlock()
		err := s.beginAsyncRaw()
		if !errors.Is(err, native.ErrProtocol) {
			t.Fatalf("beginAsyncRaw = %v, want ErrProtocol", err)
		}
		if v := earlyWindowOf(s); v.open {
			t.Error("耗尽故障后不得打开早到窗口")
		}
		if got := s.currentFault(); got == nil || !errors.Is(got, native.ErrProtocol) {
			t.Errorf("首因 = %v, want ErrProtocol", got)
		}
		if n := pendingReservedOf(s); n != 0 {
			t.Errorf("耗尽故障后 pendingReserved = %d, want 0(预留已回收,不得滞留)", n)
		}
	})
}

// TestAsyncWaitCancellationAndLateSettlement 钉住验收条件「Wait 被
// 取消或调用者丢弃 Operation 后,晚到结果仍结算一次并回收关联容
// 量」:取消只结束本次等待、不回收容量;晚到完成结算恰一次(重复完
// 成触发协议故障而非二次结算);丢弃调用者引用不影响实例关联;晚到
// ret=false 结算为原生失败。
func TestAsyncWaitCancellationAndLateSettlement(t *testing.T) {
	t.Run("取消等待后晚到结果仍结算一次并回收容量", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF160, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state

		op, err := inst.Screencap()
		if err != nil {
			t.Fatalf("Screencap = %v, want nil", err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if res, err := waitOperationGuarded(t, op, ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("取消的等待 = (%+v, %v), want Canceled", res, err)
		}
		if n := pendingOccupancy(s); n != 1 {
			t.Fatalf("取消后 pending 占用 = %d, want 1(取消不提前回收)", n)
		}
		// 晚到 ret=false:结算为原生失败,容量回收;再次等待取得同一
		// 终态。
		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":false}`)
		res, err := waitOperationGuarded(t, op, context.Background())
		if !errors.Is(err, native.ErrNativeRejected) {
			t.Fatalf("晚到失败结算 = (%+v, %v), want ErrNativeRejected", res, err)
		}
		if res.ID != 1 || res.OK || string(res.Details) != `{"ret":false}` {
			t.Fatalf("晚到失败结果 = %+v, want {1 Screencap false}", res)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Fatalf("结算后 pending 占用 = %d, want 0(容量已回收)", n)
		}
		// 重复完成:协议故障,不得二次结算。
		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":false}`)
		waitUntil(t, "重复完成触发协议故障", func() bool { return s.currentFault() != nil })
	})

	t.Run("丢弃 Operation 后晚到结果仍结算", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF161, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state

		_, err := inst.Connect(connectInput)
		if err != nil {
			t.Fatalf("Connect = %v, want nil", err)
		}
		// 调用者从此不再持有 *Operation:实例关联不受影响,晚到完成
		// 仍结算恰一次并回收容量。
		deliverAsyncInfo(t, token, 1, "Connect", `{"ret":true}`)
		waitUntil(t, "晚到完成已结算并回收", func() bool {
			return pendingOccupancy(s) == 0
		})
		if got := watermarkOfState(s); got != 1 {
			t.Errorf("提交高水位 = %d, want 1", got)
		}
	})

	t.Run("Wait(nil) 返回 ErrInvalidInput", func(t *testing.T) {
		inst, _ := newAsyncRig(t, 0xF162, nil)
		op, err := inst.Screencap()
		if err != nil {
			t.Fatalf("Screencap = %v, want nil", err)
		}
		res, err := waitOperationGuarded(t, op, nil)
		if !errors.Is(err, native.ErrInvalidInput) {
			t.Fatalf("Wait(nil) = (%+v, %v), want ErrInvalidInput", res, err)
		}
		if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
			t.Fatalf("Wait(nil) 必须返回零值结果: %+v", res)
		}
	})
}

// TestAsyncCloseRacesSettleOnce 钉住验收条件「关闭与最终结果竞争时
// 只结算一次,Operation 完成不依赖外部读取 Events」:关闭期间到达的
// 完成结算真实结果且不被剩余清空改写;无完成的提交在关闭尾段以
// ErrClosed 结算恰一次;raw 段内的提交被关闭等待、照常登记;用例全
// 程不读取 Events,结算照常发生。
func TestAsyncCloseRacesSettleOnce(t *testing.T) {
	t.Run("closing 中到达的完成结算真实结果", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF170, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state

		op, err := inst.Screencap()
		if err != nil {
			t.Fatalf("Screencap = %v, want nil", err)
		}
		entered := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		rig.closer.onStop = func(uintptr) {
			once.Do(func() { close(entered) })
			<-release
		}
		closeErr := make(chan error, 1)
		go func() { closeErr <- inst.Close() }()
		<-entered // closing 已生效、在途调用已清零,callback 门仍开放。
		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":true}`)
		res, err := waitOperationGuarded(t, op, context.Background())
		if err != nil || res.ID != 1 || !res.OK {
			t.Fatalf("closing 中的完成结算 = (%+v, %v), want 真实结果", res, err)
		}
		close(release)
		select {
		case err := <-closeErr:
			if err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		case <-time.After(hangGuard):
			t.Fatal("Close 未在防挂死上限内完成")
		}
		// 关闭尾段不得改写已结算的真实结果(§8 第 5 步:先确认结果,
		// 再结剩余;剩余为空)。
		res, err = waitOperationGuarded(t, op, context.Background())
		if err != nil || res.ID != 1 || !res.OK {
			t.Errorf("关闭后的等待被改写: (%+v, %v)", res, err)
		}
		if got := s.currentPhase(); got != phaseClosed {
			t.Errorf("阶段 = %d, want closed", got)
		}
	})

	t.Run("无完成的提交以 ErrClosed 结算恰一次", func(t *testing.T) {
		inst, _ := newAsyncRig(t, 0xF171, nil)
		s := inst.inst.state

		op, err := inst.Screencap()
		if err != nil {
			t.Fatalf("Screencap = %v, want nil", err)
		}
		if err := inst.Close(); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		res, err := waitOperationGuarded(t, op, context.Background())
		if !errors.Is(err, native.ErrClosed) {
			t.Fatalf("关闭结算错误 = %v, want ErrClosed", err)
		}
		if errors.Is(err, native.ErrNativeRejected) || errors.Is(err, native.ErrEventOverflow) {
			t.Errorf("关闭结算错误混入相邻类别: %v", err)
		}
		if res.ID != 0 || res.What != "" || res.OK || res.Details != nil {
			t.Fatalf("关闭结算必须无结果: %+v", res)
		}
		if got := op.ID(); got != 1 {
			t.Errorf("已接受 Operation 的 ID 关闭后 = %d, want 保持可读 1(§8.1)", got)
		}
		if n := pendingOccupancy(s); n != 0 {
			t.Errorf("关闭清空后占用 = %d, want 0", n)
		}
	})

	t.Run("raw 段内的提交被关闭等待并照常登记", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF172, nil)
		s := inst.inst.state

		entered := make(chan struct{})
		release := make(chan struct{})
		var once sync.Once
		rig.caller.onAsyncScreencap = func(uintptr) {
			once.Do(func() { close(entered) })
			<-release
		}
		opCh := make(chan *Operation, 1)
		errCh := make(chan error, 1)
		go func() {
			op, err := inst.Screencap()
			opCh <- op
			errCh <- err
		}()
		<-entered
		closeErr := make(chan error, 1)
		go func() { closeErr <- inst.Close() }()
		waitUntil(t, "close 完成选主转入 closing", func() bool { return s.currentPhase() == phaseClosing })
		close(release)
		// 提交已在 raw 段内:关闭等待其退出,提交照常登记(§8 第 2 步:
		// 关闭等待含 raw 返回后的登记/复制),不被关闭撤销。
		op := <-opCh
		if op == nil {
			t.Fatal("关闭前已准入的提交不得被撤销")
		}
		if err := <-errCh; err != nil {
			t.Fatalf("closing 中完成的提交 = %v, want nil", err)
		}
		select {
		case err := <-closeErr:
			if err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		case <-time.After(hangGuard):
			t.Fatal("Close 未在防挂死上限内完成")
		}
		// 无完成消息:关闭尾段把它结为 ErrClosed 恰一次。
		res, err := waitOperationGuarded(t, op, context.Background())
		if !errors.Is(err, native.ErrClosed) {
			t.Fatalf("关闭尾段结算错误 = %v, want ErrClosed", err)
		}
		if res.ID != 0 || res.OK {
			t.Errorf("关闭结算必须无结果: %+v", res)
		}
	})
}

// TestAsyncSubmissionAfterFaultDoesNotRevivePending 钉住 §7.1「已经
// 进入 raw 的动作不能撤销,返回后只补记返回 ID/诊断,不重新插入
// pending 或复活终态」:raw 段内实例故障时,提交完成 raw 后不登记、
// 预留回收,返回沿错误链可达首因的诊断错误;公开面拿不到未登记的
// Operation。
func TestAsyncSubmissionAfterFaultDoesNotRevivePending(t *testing.T) {
	inst, rig := newAsyncRig(t, 0xF180, nil)
	s := inst.inst.state

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rig.caller.onAsyncScreencap = func(uintptr) {
		once.Do(func() { close(entered) })
		<-release
	}
	opCh := make(chan *Operation, 1)
	errCh := make(chan error, 1)
	go func() {
		op, err := inst.Screencap()
		opCh <- op
		errCh <- err
	}()
	<-entered
	cause := fmt.Errorf("事件队列溢出: %w", native.ErrEventOverflow)
	s.recordFault(cause)
	close(release)

	if op := <-opCh; op != nil {
		t.Fatalf("故障后返回的提交不得发布 Operation: %+v", op)
	}
	err := <-errCh
	if !errors.Is(err, cause) || !errors.Is(err, native.ErrEventOverflow) {
		t.Fatalf("故障后提交的错误 = %v, want 沿链可达首因", err)
	}
	if n := pendingOccupancy(s); n != 0 {
		t.Errorf("故障后 pending 占用 = %d, want 0(不重新插入)", n)
	}
	if n := pendingReservedOf(s); n != 0 {
		t.Errorf("故障后预留计数 = %d, want 0", n)
	}
	if got := watermarkOfState(s); got != 0 {
		t.Errorf("故障后高水位 = %d, want 0(未登记)", got)
	}
}

// TestAsyncInputAndLifecycleRejections 钉住异步入口的输入与生命周期
// 拒绝:非法连接输入在 raw 前失败且无原生副作用;closed/faulted/
// closing 阶段三个入口按各自哨兵拒绝,不绕过状态调用。
func TestAsyncInputAndLifecycleRejections(t *testing.T) {
	t.Run("非法连接输入在 raw 前失败", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF190, nil)
		bad := []struct {
			what string
			cfg  ConnectConfig
		}{
			{"相对 ADBPath", ConnectConfig{ADBPath: "adb", Address: "127.0.0.1:5555"}},
			{"空 Address", ConnectConfig{ADBPath: "/usr/bin/adb", Address: ""}},
			{"内嵌 NUL", ConnectConfig{ADBPath: "/usr/bin/a\x00db", Address: "x"}},
			{"非法 UTF-8", ConnectConfig{ADBPath: "/usr/bin/\xff\xfe", Address: "x"}},
		}
		for _, tc := range bad {
			if op, err := inst.Connect(tc.cfg); op != nil || !errors.Is(err, native.ErrInvalidInput) {
				t.Errorf("%s: Connect = (%+v, %v), want ErrInvalidInput", tc.what, op, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("非法输入的业务调用次数 = %d, want 0(raw 前失败)", got)
		}
	})

	t.Run("closed 阶段拒绝全部异步入口", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF191, nil)
		if err := inst.Close(); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		for i, call := range []func() (*Operation, error){
			func() (*Operation, error) { return inst.Connect(connectInput) },
			func() (*Operation, error) { return inst.Click(1, 1) },
			func() (*Operation, error) { return inst.Screencap() },
		} {
			if op, err := call(); op != nil || !errors.Is(err, native.ErrClosed) {
				t.Errorf("closed 后入口 %d = (%+v, %v), want ErrClosed", i, op, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("closed 后业务调用次数 = %d, want 0", got)
		}
	})

	t.Run("faulted 阶段拒绝全部异步入口且错误链可达首因", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF192, nil)
		cause := fmt.Errorf("fake: first fault: %w", native.ErrProtocol)
		inst.inst.state.recordFault(cause)
		for i, call := range []func() (*Operation, error){
			func() (*Operation, error) { return inst.Connect(connectInput) },
			func() (*Operation, error) { return inst.Click(1, 1) },
			func() (*Operation, error) { return inst.Screencap() },
		} {
			if op, err := call(); op != nil || !errors.Is(err, cause) || !errors.Is(err, native.ErrProtocol) {
				t.Errorf("faulted 后入口 %d = (%+v, %v), want 沿链可达首因", i, op, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("faulted 后业务调用次数 = %d, want 0", got)
		}
	})

	t.Run("closing 阶段拒绝全部异步入口", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF193, nil)
		entered := make(chan struct{})
		release := make(chan struct{})
		rig.closer.onStop = func(uintptr) {
			close(entered)
			<-release
		}
		closeErr := make(chan error, 1)
		go func() { closeErr <- inst.Close() }()
		<-entered
		for i, call := range []func() (*Operation, error){
			func() (*Operation, error) { return inst.Connect(connectInput) },
			func() (*Operation, error) { return inst.Click(1, 1) },
			func() (*Operation, error) { return inst.Screencap() },
		} {
			if op, err := call(); op != nil || !errors.Is(err, native.ErrClosing) {
				t.Errorf("closing 中入口 %d = (%+v, %v), want ErrClosing", i, op, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("closing 中业务调用次数 = %d, want 0", got)
		}
		close(release)
		select {
		case err := <-closeErr:
			if err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		case <-time.After(hangGuard):
			t.Fatal("Close 未在防挂死上限内完成")
		}
	})
}
