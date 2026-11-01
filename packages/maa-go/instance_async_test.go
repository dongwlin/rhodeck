// 本文件是异步提交与结果关联的受控用例:验证 Connect/Click/Screencap
// 的提交与关联、早到/晚到完成、ID 0 拒绝、容量限制、Wait 取消,以及
// 关闭与故障下的终态。

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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "NewInstance = %v, want nil", err)
	rig.track(inst)
	return inst, rig
}

// deliverAsyncInfo 经真实 token 注册表查表投递一条 AsyncCallInfo 完成
// 消息:payload 带 NUL 终止(callback 准入按有界扫描寻找 NUL)。
func deliverAsyncInfo(t *testing.T, token uintptr, id AsyncID, what, details string) {
	t.Helper()
	deliver := native.LookupCallbackDelivery(token)
	require.NotNil(t, deliver, "token %d not registered in the registry, cannot deliver the completion message", token)
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
		require.Fail(t, "Operation.Wait did not return within the hang-guard deadline: the done notification is suspected unclosed")
		return AsyncResult{}, nil
	}
}

// connectInput 是异步用例的合法连接输入(Config 允许空串)。
var connectInput = ConnectConfig{ADBPath: testAbsolutePath("adb"), Address: "127.0.0.1:5555", Config: ""}

// TestAsyncEntriesForwardBlockFalseAndAcceptedID 验证「三个
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
	require.NoError(t, err, "Connect = %v, want nil", err)
	require.Equal(t, 1, rig.caller.count("asyncConnect"), "asyncConnect call count = %d, want 1", rig.caller.count("asyncConnect"))
	rec := rig.caller.asyncRecordByName(0, "asyncConnect")
	require.NotNil(t, rec.connect, "asyncConnect call record missing")
	assert.Equal(t, handle, rec.connect.handle, "asyncConnect received handle %#x, want %#x", rec.connect.handle, handle)
	assert.Equal(t, uint8(0), rec.connect.block, "asyncConnect block = %d, want 0 (frozen block=false)", rec.connect.block)
	assert.Equal(t, connectInput.ADBPath, rec.connect.adbPath, "asyncConnect connection parameters rewritten: adb=%q address=%q, want %q/%q",
		rec.connect.adbPath, rec.connect.address, connectInput.ADBPath, connectInput.Address)
	assert.Equal(t, connectInput.Address, rec.connect.address, "asyncConnect connection parameters rewritten: adb=%q address=%q, want %q/%q",
		rec.connect.adbPath, rec.connect.address, connectInput.ADBPath, connectInput.Address)
	assert.Empty(t, rec.connect.config, "asyncConnect empty Config decoded = %q, want an empty string (an empty C string is passed as-is, not nil)", rec.connect.config)
	assert.Equal(t, AsyncID(1), op.ID(), "Connect returned ID = %d, want 1 (nonzero ID injected by the fake)", op.ID())

	// 非零 ID 只表示提交被接受:完成尚未到达,取消的等待返回零值结
	// 果与 ctx.Err(),不得到任何成功结果。
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res, err := waitOperationGuarded(t, op, ctx)
	require.ErrorIs(t, err, context.Canceled, "wait error before completion = %v, want context.Canceled", err)
	require.Equal(t, AsyncID(0), res.ID, "the wait before completion must return a zero-value result: %+v", res)
	require.Empty(t, res.What, "the wait before completion must return a zero-value result: %+v", res)
	require.False(t, res.OK, "the wait before completion must return a zero-value result: %+v", res)
	require.Nil(t, res.Details, "the wait before completion must return a zero-value result: %+v", res)

	// Click 与 Screencap:同样 block=false,坐标带符号透传。
	_, clickErr := inst.Click(-5, 7)
	require.NoError(t, clickErr, "Click = %v, want nil", clickErr)
	clickRec := rig.caller.asyncRecordByName(0, "asyncClick")
	assert.True(t, clickRec.click != nil && clickRec.click.block == 0 && clickRec.click.x == -5 && clickRec.click.y == 7,
		"asyncClick record = %+v, want block=0, x=-5, y=7 passed through as-is", clickRec.click)
	_, capErr := inst.Screencap()
	require.NoError(t, capErr, "Screencap = %v, want nil", capErr)
	capRec := rig.caller.asyncRecordByName(0, "asyncScreencap")
	assert.True(t, capRec.screencap != nil && capRec.screencap.block == 0, "asyncScreencap record = %+v, want block=0", capRec.screencap)

	// 完成到达后取得真实终态:ID 关联到正确的 Operation。
	token := rig.creator.exToken(0)
	deliverAsyncInfo(t, token, 1, "Connect", `{"ret":true,"cost":7}`)
	res, err = waitOperationGuarded(t, op, context.Background())
	require.NoError(t, err, "wait after completion = %v, want nil", err)
	require.Equal(t, AsyncID(1), res.ID, "completion result = %+v, want {1 Connect true {\"ret\":true,\"cost\":7}}", res)
	require.Equal(t, "Connect", res.What, "completion result = %+v, want {1 Connect true {\"ret\":true,\"cost\":7}}", res)
	require.True(t, res.OK, "completion result = %+v, want {1 Connect true {\"ret\":true,\"cost\":7}}", res)
	require.Equal(t, `{"ret":true,"cost":7}`, string(res.Details), "completion result = %+v, want {1 Connect true {\"ret\":true,\"cost\":7}}", res)
	assert.Equal(t, AsyncID(3), watermarkOfState(inst.inst.state), "submission high-water mark = %d, want 3 (three accepted submissions strictly increasing)", watermarkOfState(inst.inst.state))
}

// watermarkOfState 在状态锁内读取异步提交高水位(白盒诊断读取)。
func watermarkOfState(s *instanceState) AsyncID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.asyncWatermark
}

// TestAsyncEarlyJoinByIDAndExpectedWhat 验证「完成回调先于
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
		waitUntil(t, "early result staged into the window", func() bool {
			v := earlyWindowOf(s)
			return v.open && v.has && v.id == 1 && v.what == "Connect"
		})
	}

	op, err := inst.Connect(connectInput)
	require.NoError(t, err, "Connect = %v, want nil", err)
	require.Equal(t, AsyncID(1), op.ID(), "Connect returned ID = %d, want 1", op.ID())
	// 接合完成:等待立即返回终态,不依赖任何消息时序。
	res, err := waitOperationGuarded(t, op, context.Background())
	require.NoError(t, err, "wait after the early join = %v, want nil", err)
	require.Equal(t, AsyncID(1), res.ID, "early join result = %+v, want the completion final state", res)
	require.Equal(t, "Connect", res.What, "early join result = %+v, want the completion final state", res)
	require.True(t, res.OK, "early join result = %+v, want the completion final state", res)
	require.Equal(t, `{"ret":true,"cost":3}`, string(res.Details), "early join result = %+v, want the completion final state", res)
	// 窗口关闭、早到表清空、pending 表回收、高水位推进。
	v := earlyWindowOf(s)
	assert.False(t, v.open, "early window not cleared after the join: %+v", v)
	assert.False(t, v.has, "early window not cleared after the join: %+v", v)
	assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after the join = %d, want 0 (capacity reclaimed)", pendingOccupancy(s))
	assert.Equal(t, AsyncID(1), watermarkOfState(s), "submission high-water mark = %d, want 1", watermarkOfState(s))

	// 早到 what 与预期不符:ErrProtocol 故障,不伪造成功(
	// ID/what 不符)。fake 返回与早到结果相同的 ID(2):raw 返回后
	// join 的 ID 检查通过,故障精确来自预期 what 比对分支,而非相邻
	// 的高水位或 join ID 分支。
	rig.caller.asyncRets = map[string]int32{"asyncConnect": 2}
	rig.caller.onAsyncConnect = func(uintptr) {
		deliverAsyncInfo(t, token, 2, "Click", `{"ret":true}`)
		waitUntil(t, "mismatched early result staged", func() bool {
			v := earlyWindowOf(s)
			return v.open && v.has && v.id == 2 && v.what == "Click"
		})
	}
	op2, mismatchErr := inst.Connect(connectInput)
	require.Error(t, mismatchErr, "Connect on an early what mismatch = (%+v, nil), want a protocol fault", op2)
	require.ErrorIs(t, mismatchErr, native.ErrProtocol, "early what mismatch error = %v, want errors.Is ErrProtocol", mismatchErr)
	require.Contains(t, mismatchErr.Error(), `reports what "Click", want "Connect"`, "early what mismatch error does not indicate the what comparison:\n%s", mismatchErr.Error())
	got := s.currentFault()
	assert.True(t, got != nil && errors.Is(got, native.ErrProtocol) && strings.Contains(got.Error(), `reports what "Click", want "Connect"`),
		"first cause = %v, want ErrProtocol from the expected-what comparison branch", got)
	assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after the fault sweep = %d, want 0", pendingOccupancy(s))
}

// TestAsyncCompletionArrivalOrders 验证「不依赖消息到达顺
// 序」:旧提交的完成在新提交的 raw 窗口内到达(pending 命中,窗口不
// 受影响)、新提交自身的早到接合、以及完成消息晚于 raw 返回到达,三
// 种时序混排都按 ID 关联到正确请求,且不要求完成消息按 ID 顺序到达。
func TestAsyncCompletionArrivalOrders(t *testing.T) {
	const handle = uintptr(0xF120)

	t.Run("old submission completes within the new window (arrival order 11->12)", func(t *testing.T) {
		inst, rig := newAsyncRig(t, handle, nil)
		rig.caller.asyncRets = map[string]int32{"asyncConnect": 11, "asyncClick": 12}
		token := rig.creator.exToken(0)
		s := inst.inst.state

		opConnect, err := inst.Connect(connectInput)
		require.NoError(t, err, "Connect = %v, want nil", err)
		// Click 的 raw 窗口内投递 Connect 的完成(11):pending 命中,
		// 不进早到窗口、不影响 Click 的窗口。
		rig.caller.onAsyncClick = func(uintptr) {
			deliverAsyncInfo(t, token, 11, "Connect", `{"ret":true}`)
			waitUntil(t, "Connect completion settled opConnect within the window", func() bool {
				select {
				case <-opConnect.op.done:
					return true
				default:
					return false
				}
			})
		}
		opClick, err := inst.Click(3, 4)
		require.NoError(t, err, "Click = %v, want nil", err)
		res, err := waitOperationGuarded(t, opConnect, context.Background())
		require.NoError(t, err, "opConnect final state = (%+v, %v), want a completion result", res, err)
		require.Equal(t, AsyncID(11), res.ID, "opConnect final state = (%+v, %v), want a completion result", res, err)
		require.Equal(t, "Connect", res.What, "opConnect final state = (%+v, %v), want a completion result", res, err)
		require.True(t, res.OK, "opConnect final state = (%+v, %v), want a completion result", res, err)
		// Click 的完成按 ID 顺序晚到:统一 dispatcher 结算。
		deliverAsyncInfo(t, token, 12, "Click", `{"ret":false}`)
		res, err = waitOperationGuarded(t, opClick, context.Background())
		require.ErrorIs(t, err, native.ErrNativeRejected, "opClick final-state error = %v, want ErrNativeRejected (ret=false)", err)
		require.Equal(t, AsyncID(12), res.ID, "opClick result = %+v, want {12 Click false}", res)
		require.Equal(t, "Click", res.What, "opClick result = %+v, want {12 Click false}", res)
		require.False(t, res.OK, "opClick result = %+v, want {12 Click false}", res)
		assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after both settlements = %d, want 0", pendingOccupancy(s))
		assert.Equal(t, AsyncID(12), watermarkOfState(s), "submission high-water mark = %d, want 12", watermarkOfState(s))
	})

	t.Run("new submission early join, old completion arrives late (arrival order 22->21)", func(t *testing.T) {
		inst, rig := newAsyncRig(t, handle+1, nil)
		rig.caller.asyncRets = map[string]int32{"asyncConnect": 21, "asyncClick": 22}
		token := rig.creator.exToken(0)
		s := inst.inst.state

		opConnect, err := inst.Connect(connectInput)
		require.NoError(t, err, "Connect = %v, want nil", err)
		// Click 的 raw 窗口内投递 Click 自身的完成(22):早到临存,
		// raw 返回后接合。
		rig.caller.onAsyncClick = func(uintptr) {
			deliverAsyncInfo(t, token, 22, "Click", `{"ret":true,"cost":1}`)
			waitUntil(t, "Click completion staged into the window", func() bool {
				v := earlyWindowOf(s)
				return v.open && v.has && v.id == 22
			})
		}
		opClick, err := inst.Click(1, 2)
		require.NoError(t, err, "Click = %v, want nil", err)
		// 早到接合:opClick 返回时已带终态。
		res, err := waitOperationGuarded(t, opClick, context.Background())
		require.NoError(t, err, "opClick early join = (%+v, %v), want a completion result", res, err)
		require.Equal(t, AsyncID(22), res.ID, "opClick early join = (%+v, %v), want a completion result", res, err)
		require.True(t, res.OK, "opClick early join = (%+v, %v), want a completion result", res, err)
		// Connect 的完成(21)按逆序晚到:pending 命中照常结算。
		deliverAsyncInfo(t, token, 21, "Connect", `{"ret":false}`)
		res, err = waitOperationGuarded(t, opConnect, context.Background())
		require.ErrorIs(t, err, native.ErrNativeRejected, "opConnect final-state error = %v, want ErrNativeRejected", err)
		require.Equal(t, AsyncID(21), res.ID, "opConnect result = %+v, want {21 Connect false}", res)
		require.False(t, res.OK, "opConnect result = %+v, want {21 Connect false}", res)
		assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after both settlements = %d, want 0", pendingOccupancy(s))
	})
}

// TestAsyncZeroIDReclaimsReservation 验证「ID 0 返回明确提交
// 错误并回收本次预留,不发布成功 Operation」:raw 返回 0 时得到包装
// ErrNativeRejected 的提交错误,预留计数与 pending 表回收,实例不因
// 此故障;窗口内已有早到结果时,矛盾证据以 ErrProtocol 故障保留,不
// 被丢弃。
func TestAsyncZeroIDReclaimsReservation(t *testing.T) {
	t.Run("id 0 returns ErrNativeRejected and reclaims the reservation", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF130, nil)
		s := inst.inst.state
		rig.caller.asyncRets = map[string]int32{"asyncScreencap": 0}

		op, err := inst.Screencap()
		require.Nil(t, op, "ID 0 must not return a successful Operation: %+v", op)
		require.ErrorIs(t, err, native.ErrNativeRejected, "ID 0 error = %v, want errors.Is ErrNativeRejected", err)
		assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after ID 0 = %d, want 0", pendingOccupancy(s))
		assert.Equal(t, uint64(0), pendingReservedOf(s), "reservation count after ID 0 = %d, want 0 (reservation reclaimed)", pendingReservedOf(s))
		v := earlyWindowOf(s)
		assert.False(t, v.open, "early window not closed after ID 0: %+v", v)
		assert.False(t, v.has, "early window not closed after ID 0: %+v", v)
		// 实例不因提交失败而故障:阶段仍 ready,后续提交照常接受。
		assert.Equal(t, phaseReady, s.currentPhase(), "phase = %d, want still ready", s.currentPhase())
		assert.Nil(t, s.currentFault(), "first cause = %v, want nil", s.currentFault())
		select {
		case <-s.faultNotify():
			assert.Fail(t, "submission failure must not trigger the one-shot fault notification")
		default:
		}
		op2, nextErr := inst.Click(1, 1)
		assert.True(t, nextErr == nil && op2.ID() == 1, "subsequent submission = (%+v, %v), want normal acceptance", op2, nextErr)
	})

	t.Run("id 0 with an early result in the window faults with ErrProtocol", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF131, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state
		rig.caller.asyncRets = map[string]int32{"asyncClick": 0}
		rig.caller.onAsyncClick = func(uintptr) {
			deliverAsyncInfo(t, token, 9, "Click", `{"ret":true}`)
			waitUntil(t, "early result staged into the window", func() bool {
				v := earlyWindowOf(s)
				return v.open && v.has && v.id == 9
			})
		}

		op, err := inst.Click(1, 1)
		require.Nil(t, op, "must not return a successful Operation under contradictory evidence: %+v", op)
		require.ErrorIs(t, err, native.ErrProtocol, "contradictory-evidence error = %v, want errors.Is ErrProtocol", err)
		got := s.currentFault()
		assert.NotNil(t, got, "first cause = %v, want ErrProtocol", got)
		assert.ErrorIs(t, got, native.ErrProtocol, "first cause = %v, want ErrProtocol", got)
		assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after the fault sweep = %d, want 0", pendingOccupancy(s))
		assert.Equal(t, uint64(0), pendingReservedOf(s), "reservation count after the fault sweep = %d, want 0", pendingReservedOf(s))
	})
}

// errTheAsyncSubmissionRemoved 占位:已移除的辅助哨兵。

// TestAsyncProtocolViolationsFaultInstance 验证「pending/
// early 表受冻结容量限制,重复或不可解释结果不能无限缓存或伪造成
// 功」:早到窗口的第二个结果(含重复)为 ErrProtocol 故障;ID 复用/
// 倒退与负值 ID 为 ErrProtocol 故障;已接受 ID 的重复完成不能伪造新
// 的成功;早到结果字节超 EarlyBytes 为 ErrEventOverflow 故障且不缓
// 存。故障保留首因,pending 表由首因扫描清空。
func TestAsyncProtocolViolationsFaultInstance(t *testing.T) {
	t.Run("second early-window result faults and the submission is not registered", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF140, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state
		rig.caller.onAsyncConnect = func(uintptr) {
			deliverAsyncInfo(t, token, 3, "Connect", `{"ret":true}`)
			waitUntil(t, "first early result staged", func() bool {
				return earlyWindowOf(s).has
			})
			// 第二个早到结果(含重复):ErrProtocol 故障。故障必须先
			// 于 raw 返回发生(分支隔离:settleCompletion 的第二早到
			// 分支,而非 finishAsyncRaw 的接合不匹配分支),且首因文本
			// 指明第二早到,不得以覆盖缓存代替故障。
			deliverAsyncInfo(t, token, 4, "Click", `{"ret":true}`)
			waitUntil(t, "second early result triggered the first cause within the window", func() bool {
				f := s.currentFault()
				return f != nil && strings.Contains(f.Error(), "second early completion")
			})
			v := earlyWindowOf(s)
			assert.True(t, v.has, "window content rewritten after the second-early fault: %+v, want the first early result (id 3) preserved as contradictory evidence", v)
			assert.Equal(t, AsyncID(3), v.id, "window content rewritten after the second-early fault: %+v, want the first early result (id 3) preserved as contradictory evidence", v)
		}

		op, err := inst.Connect(connectInput)
		require.Nil(t, op, "must not return a successful Operation on a second early result: %+v", op)
		require.ErrorIs(t, err, native.ErrProtocol, "error = %v, want errors.Is ErrProtocol", err)
		got := s.currentFault()
		assert.True(t, got != nil && errors.Is(got, native.ErrProtocol) && strings.Contains(got.Error(), "second early completion"),
			"first cause = %v, want ErrProtocol from the second-early branch", got)
		assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after the fault sweep = %d, want 0", pendingOccupancy(s))
		assert.Equal(t, uint64(0), pendingReservedOf(s), "reservation count after the fault sweep = %d, want 0", pendingReservedOf(s))
	})

	t.Run("id reuse and negative return fault with ErrProtocol", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF141, nil)
		s := inst.inst.state

		op1, op1Err := inst.Connect(connectInput)
		require.NoError(t, op1Err, "first Connect = (%+v, %v), want acceptance", op1, op1Err)
		require.Equal(t, AsyncID(1), op1.ID(), "first Connect = (%+v, %v), want acceptance", op1, op1Err)
		// 复用/倒退:不严格大于高水位 1。
		rig.caller.asyncRets = map[string]int32{"asyncClick": 1}
		reuseOp, reuseErr := inst.Click(0, 0)
		require.Nil(t, reuseOp, "ID reuse = (%+v, %v), want ErrProtocol", reuseOp, reuseErr)
		require.ErrorIs(t, reuseErr, native.ErrProtocol, "ID reuse = (%+v, %v), want ErrProtocol", reuseOp, reuseErr)
		got := s.currentFault()
		assert.NotNil(t, got, "first cause after ID reuse = %v, want ErrProtocol", got)
		assert.ErrorIs(t, got, native.ErrProtocol, "first cause after ID reuse = %v, want ErrProtocol", got)
		// 负值 ID:违反 1..MaxInt32 接受域。
		rig.caller.asyncRets = map[string]int32{"asyncScreencap": -3}
		negOp, negErr := inst.Screencap()
		require.Nil(t, negOp, "negative ID = (%+v, %v), want ErrProtocol", negOp, negErr)
		require.ErrorIs(t, negErr, native.ErrProtocol, "negative ID = (%+v, %v), want ErrProtocol", negOp, negErr)
		assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after the fault sweep = %d, want 0", pendingOccupancy(s))
		assert.Equal(t, uint64(0), pendingReservedOf(s), "reservation count after the fault sweep = %d, want 0", pendingReservedOf(s))
	})

	t.Run("duplicate completion for an accepted ID must not forge new success", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF142, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state

		op, err := inst.Screencap()
		require.NoError(t, err, "Screencap = %v, want nil", err)
		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":true}`)
		res, firstErr := waitOperationGuarded(t, op, context.Background())
		require.NoError(t, firstErr, "first completion = (%+v, %v), want success", res, firstErr)
		require.Equal(t, AsyncID(1), res.ID, "first completion = (%+v, %v), want success", res, firstErr)
		// 同一 ID 的重复完成:ErrProtocol 故障,不伪造新的成功。
		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":true}`)
		waitUntil(t, "duplicate completion triggered the first cause", func() bool {
			return s.currentFault() != nil
		})
		assert.ErrorIs(t, s.currentFault(), native.ErrProtocol, "first cause = %v, want ErrProtocol", s.currentFault())
		// 已结算的终态不被重复完成改写。
		res, err = waitOperationGuarded(t, op, context.Background())
		assert.NoError(t, err, "duplicate completion rewrote the final state: (%+v, %v)", res, err)
		assert.Equal(t, AsyncID(1), res.ID, "duplicate completion rewrote the final state: (%+v, %v)", res, err)
		assert.True(t, res.OK, "duplicate completion rewrote the final state: (%+v, %v)", res, err)
	})

	t.Run("early result exceeding EarlyBytes faults with ErrEventOverflow and is not cached", func(t *testing.T) {
		// 经 validateLimits 的交叉校验(CallbackBytes ≤ EarlyBytes),
		// 真实提交路径上该分支防御性可达;此处以白盒窗口构造。
		s := newInstanceState(0xF143, 8, 4)
		s.publishReady()
		rawErr := s.beginAsyncRaw()
		require.NoError(t, rawErr, "beginAsyncRaw = %v, want nil", rawErr)
		s.settleCompletion(completionInfo{ID: 5, What: "Click", OK: true, Details: nil, Bytes: 100})
		got := s.currentFault()
		require.NotNil(t, got, "first cause = %v, want ErrEventOverflow", got)
		require.ErrorIs(t, got, native.ErrEventOverflow, "first cause = %v, want ErrEventOverflow", got)
		v := earlyWindowOf(s)
		assert.False(t, v.has, "an over-limit early result must not be cached: %+v", v)
	})
}

// TestAsyncPendingLimitReclaimsAndRejects 验证「pending 表受
// 冻结容量限制」:未结清 Operation 数达上限时,新提交在进入 raw 前
// 被拒(ErrOperationLimit,无原生副作用,实例不故障);晚到完成结算
// 回收容量后,提交恢复接受。
func TestAsyncPendingLimitReclaimsAndRejects(t *testing.T) {
	inst, rig := newAsyncRig(t, 0xF150, func(l *Limits) { l.PendingOperations = 1 })
	rig.caller.asyncRets = map[string]int32{"asyncScreencap": 1, "asyncClick": 2}
	s := inst.inst.state

	op, err := inst.Screencap()
	require.NoError(t, err, "Screencap = %v, want nil", err)
	require.Equal(t, 1, pendingOccupancy(s), "pending occupancy after registration = %d, want 1", pendingOccupancy(s))
	// 容量满:拒绝发生在预留点,不调用 native。
	before := rig.caller.total()
	op2, limitErr := inst.Connect(connectInput)
	require.Nil(t, op2, "submission at full capacity = (%+v, %v), want ErrOperationLimit", op2, limitErr)
	require.ErrorIs(t, limitErr, native.ErrOperationLimit, "submission at full capacity = (%+v, %v), want ErrOperationLimit", op2, limitErr)
	assert.Equal(t, before, rig.caller.total(), "native call count changed %d -> %d at full capacity, want unchanged (no native calls)", before, rig.caller.total())
	assert.Equal(t, phaseReady, s.currentPhase(), "phase after the capacity rejection = %d, want still ready", s.currentPhase())
	assert.Nil(t, s.currentFault(), "first cause after the capacity rejection = %v, want nil", s.currentFault())
	// 晚到完成结算并回收容量:提交恢复接受。
	deliverAsyncInfo(t, rig.creator.exToken(0), 1, "Screencap", `{"ret":true}`)
	res, settleErr := waitOperationGuarded(t, op, context.Background())
	require.NoError(t, settleErr, "late completion settlement = (%+v, %v), want success", res, settleErr)
	require.Equal(t, AsyncID(1), res.ID, "late completion settlement = (%+v, %v), want success", res, settleErr)
	op3, nextErr := inst.Click(2, 2)
	assert.True(t, nextErr == nil && op3.ID() == 2, "submission after the capacity reclaim = (%+v, %v), want acceptance", op3, nextErr)
}

// TestPendingReservationAccounting 钉住预留原语的容量记账与阶段拒
// 绝:预留计入占用(len(pending)+pendingReserved),释放后可再预留;
// closing/faulted/closed 阶段拒绝;双重释放为编程错误 panic。
func TestPendingReservationAccounting(t *testing.T) {
	t.Run("reservation counts toward occupancy; re-reserve after release", func(t *testing.T) {
		s := newReadyState(t, 0xF151, 2)
		for i := 0; i < 2; i++ {
			_, reserveErr := s.reservePendingOperation("reserve")
			require.NoError(t, reserveErr, "reservation #%d = %v, want nil", i+1, reserveErr)
		}
		_, thirdErr := s.reservePendingOperation("third")
		require.ErrorIs(t, thirdErr, native.ErrOperationLimit, "third reservation = %v, want ErrOperationLimit", thirdErr)
		_, admitErr := s.admitPendingOperation(9, "direct admission")
		require.ErrorIs(t, admitErr, native.ErrOperationLimit, "direct admission under reservation occupancy = %v, want ErrOperationLimit", admitErr)
		s.releasePendingReservation()
		_, reErr := s.reservePendingOperation("re-reserve after reclaim")
		require.NoError(t, reErr, "reservation after release = %v, want nil", reErr)
		// 此刻占用 = 2(两笔未释放预留):再连发三次释放,前两次归位、
		// 第三次下溢,按编程错误 panic。
		assertPanics(t, "double release of the reservation", func() {
			s.releasePendingReservation()
			s.releasePendingReservation()
			s.releasePendingReservation()
		})
	})

	t.Run("closing, faulted, and closed phases reject reservation", func(t *testing.T) {
		closing := newReadyState(t, 0xF152, 8)
		mustElectClose(t, closing)
		_, closingErr := closing.reservePendingOperation("closing")
		require.ErrorIs(t, closingErr, native.ErrClosing, "closing reservation = %v, want ErrClosing", closingErr)
		faulted := newReadyState(t, 0xF153, 8)
		cause := fmt.Errorf("fault: %w", native.ErrProtocol)
		faulted.recordFault(cause)
		_, faultedErr := faulted.reservePendingOperation("faulted")
		require.ErrorIs(t, faultedErr, cause, "faulted reservation = %v, want the first cause reachable along the chain", faultedErr)
		closed := newClosedState(t, 0xF154)
		_, closedErr := closed.reservePendingOperation("closed")
		require.ErrorIs(t, closedErr, native.ErrClosed, "closed reservation = %v, want ErrClosed", closedErr)
		initializing := newInstanceState(0xF155, 8, 1_048_576)
		assertPanics(t, "reservation in the initializing phase", func() { _, _ = initializing.reservePendingOperation("unpublished") })
	})

	t.Run("MaxInt32 high-water mark passes the gate, faults before raw, and reclaims the reservation", func(t *testing.T) {
		s := newReadyState(t, 0xF156, 8)
		// 先预留一次:耗尽故障路径必须回收该预留(每份预留在
		// 拒绝/结算竞争中只回收一次),否则计数永久滞留。
		_, reserveErr := s.reservePendingOperation("reservation before exhaustion")
		require.NoError(t, reserveErr, "reservePendingOperation = %v, want nil", reserveErr)
		s.mu.Lock()
		s.asyncWatermark = math.MaxInt32
		s.mu.Unlock()
		err := s.beginAsyncRaw()
		require.ErrorIs(t, err, native.ErrProtocol, "beginAsyncRaw = %v, want ErrProtocol", err)
		v := earlyWindowOf(s)
		assert.False(t, v.open, "the early window must not open after the exhaustion fault")
		got := s.currentFault()
		assert.NotNil(t, got, "first cause = %v, want ErrProtocol", got)
		assert.ErrorIs(t, got, native.ErrProtocol, "first cause = %v, want ErrProtocol", got)
		assert.Equal(t, uint64(0), pendingReservedOf(s), "pendingReserved after the exhaustion fault = %d, want 0 (reservation reclaimed, must not linger)", pendingReservedOf(s))
	})
}

// TestAsyncWaitCancellationAndLateSettlement 验证「Wait 被
// 取消或调用者丢弃 Operation 后,晚到结果仍结算一次并回收关联容
// 量」:取消只结束本次等待、不回收容量;晚到完成结算恰一次(重复完
// 成触发协议故障而非二次结算);丢弃调用者引用不影响实例关联;晚到
// ret=false 结算为原生失败。
func TestAsyncWaitCancellationAndLateSettlement(t *testing.T) {
	t.Run("late result still settles once and reclaims capacity after a cancelled wait", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF160, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state

		op, err := inst.Screencap()
		require.NoError(t, err, "Screencap = %v, want nil", err)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		res, cancelErr := waitOperationGuarded(t, op, ctx)
		require.ErrorIs(t, cancelErr, context.Canceled, "cancelled wait = (%+v, %v), want Canceled", res, cancelErr)
		require.Equal(t, 1, pendingOccupancy(s), "pending occupancy after cancellation = %d, want 1 (cancellation does not reclaim early)", pendingOccupancy(s))
		// 晚到 ret=false:结算为原生失败,容量回收;再次等待取得同一
		// 终态。
		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":false}`)
		res, err = waitOperationGuarded(t, op, context.Background())
		require.ErrorIs(t, err, native.ErrNativeRejected, "late failure settlement = (%+v, %v), want ErrNativeRejected", res, err)
		require.Equal(t, AsyncID(1), res.ID, "late failure result = %+v, want {1 Screencap false}", res)
		require.False(t, res.OK, "late failure result = %+v, want {1 Screencap false}", res)
		require.Equal(t, `{"ret":false}`, string(res.Details), "late failure result = %+v, want {1 Screencap false}", res)
		require.Equal(t, 0, pendingOccupancy(s), "pending occupancy after settlement = %d, want 0 (capacity reclaimed)", pendingOccupancy(s))
		// 重复完成:协议故障,不得二次结算。
		deliverAsyncInfo(t, token, 1, "Screencap", `{"ret":false}`)
		waitUntil(t, "duplicate completion triggered a protocol fault", func() bool { return s.currentFault() != nil })
	})

	t.Run("late result still settles after the Operation is dropped", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF161, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state

		_, err := inst.Connect(connectInput)
		require.NoError(t, err, "Connect = %v, want nil", err)
		// 调用者从此不再持有 *Operation:实例关联不受影响,晚到完成
		// 仍结算恰一次并回收容量。
		deliverAsyncInfo(t, token, 1, "Connect", `{"ret":true}`)
		waitUntil(t, "late completion settled and reclaimed", func() bool {
			return pendingOccupancy(s) == 0
		})
		assert.Equal(t, AsyncID(1), watermarkOfState(s), "submission high-water mark = %d, want 1", watermarkOfState(s))
	})

	t.Run("Wait(nil) returns ErrInvalidInput", func(t *testing.T) {
		inst, _ := newAsyncRig(t, 0xF162, nil)
		op, err := inst.Screencap()
		require.NoError(t, err, "Screencap = %v, want nil", err)
		res, nilErr := waitOperationGuarded(t, op, nil)
		require.ErrorIs(t, nilErr, native.ErrInvalidInput, "Wait(nil) = (%+v, %v), want ErrInvalidInput", res, nilErr)
		require.Equal(t, AsyncID(0), res.ID, "Wait(nil) must return a zero-value result: %+v", res)
		require.Empty(t, res.What, "Wait(nil) must return a zero-value result: %+v", res)
		require.False(t, res.OK, "Wait(nil) must return a zero-value result: %+v", res)
		require.Nil(t, res.Details, "Wait(nil) must return a zero-value result: %+v", res)
	})
}

// TestAsyncCloseRacesSettleOnce 验证「关闭与最终结果竞争时
// 只结算一次,Operation 完成不依赖外部读取 Events」:关闭期间到达的
// 完成结算真实结果且不被剩余清空改写;无完成的提交在关闭尾段以
// ErrClosed 结算恰一次;raw 段内的提交被关闭等待、照常登记;用例全
// 程不读取 Events,结算照常发生。
func TestAsyncCloseRacesSettleOnce(t *testing.T) {
	t.Run("completion arriving during closing settles the real result", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF170, nil)
		token := rig.creator.exToken(0)
		s := inst.inst.state

		op, err := inst.Screencap()
		require.NoError(t, err, "Screencap = %v, want nil", err)
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
		require.NoError(t, err, "completion settlement during closing = (%+v, %v), want the real result", res, err)
		require.Equal(t, AsyncID(1), res.ID, "completion settlement during closing = (%+v, %v), want the real result", res, err)
		require.True(t, res.OK, "completion settlement during closing = (%+v, %v), want the real result", res, err)
		close(release)
		select {
		case err := <-closeErr:
			require.NoError(t, err, "Close = %v, want nil", err)
		case <-time.After(hangGuard):
			require.Fail(t, "Close did not complete within the hang-guard deadline")
		}
		// 关闭尾段不得改写已结算的真实结果(第 5 步:先确认结果,
		// 再结剩余;剩余为空)。
		res, err = waitOperationGuarded(t, op, context.Background())
		assert.NoError(t, err, "the wait after close was rewritten: (%+v, %v)", res, err)
		assert.Equal(t, AsyncID(1), res.ID, "the wait after close was rewritten: (%+v, %v)", res, err)
		assert.True(t, res.OK, "the wait after close was rewritten: (%+v, %v)", res, err)
		assert.Equal(t, phaseClosed, s.currentPhase(), "phase = %d, want closed", s.currentPhase())
	})

	t.Run("submission without completion settles exactly once with ErrClosed", func(t *testing.T) {
		inst, _ := newAsyncRig(t, 0xF171, nil)
		s := inst.inst.state

		op, err := inst.Screencap()
		require.NoError(t, err, "Screencap = %v, want nil", err)
		closeRet := inst.Close()
		require.NoError(t, closeRet, "Close = %v, want nil", closeRet)
		res, err := waitOperationGuarded(t, op, context.Background())
		require.ErrorIs(t, err, native.ErrClosed, "close settlement error = %v, want ErrClosed", err)
		assert.NotErrorIs(t, err, native.ErrNativeRejected, "close settlement error mixed in an adjacent category: %v", err)
		assert.NotErrorIs(t, err, native.ErrEventOverflow, "close settlement error mixed in an adjacent category: %v", err)
		require.Equal(t, AsyncID(0), res.ID, "close settlement must have no result: %+v", res)
		require.Empty(t, res.What, "close settlement must have no result: %+v", res)
		require.False(t, res.OK, "close settlement must have no result: %+v", res)
		require.Nil(t, res.Details, "close settlement must have no result: %+v", res)
		assert.Equal(t, AsyncID(1), op.ID(), "ID of the accepted Operation after close = %d, want still readable as 1", op.ID())
		assert.Equal(t, 0, pendingOccupancy(s), "occupancy after the close sweep = %d, want 0", pendingOccupancy(s))
	})

	t.Run("submission inside the raw section is waited on by close and still registered", func(t *testing.T) {
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
		waitUntil(t, "close won the election and transitioned to closing", func() bool { return s.currentPhase() == phaseClosing })
		close(release)
		// 提交已在 raw 段内:关闭等待其退出,提交照常登记(第 2 步:
		// 关闭等待含 raw 返回后的登记/复制),不被关闭撤销。
		op := <-opCh
		require.NotNil(t, op, "a submission admitted before close must not be revoked")
		submitErr := <-errCh
		require.NoError(t, submitErr, "submission completed during closing = %v, want nil", submitErr)
		select {
		case err := <-closeErr:
			require.NoError(t, err, "Close = %v, want nil", err)
		case <-time.After(hangGuard):
			require.Fail(t, "Close did not complete within the hang-guard deadline")
		}
		// 无完成消息:关闭尾段把它结为 ErrClosed 恰一次。
		res, err := waitOperationGuarded(t, op, context.Background())
		require.ErrorIs(t, err, native.ErrClosed, "close-tail settlement error = %v, want ErrClosed", err)
		assert.Equal(t, AsyncID(0), res.ID, "close settlement must have no result: %+v", res)
		assert.False(t, res.OK, "close settlement must have no result: %+v", res)
	})
}

// TestAsyncSubmissionAfterFaultDoesNotRevivePending 钉住 「已经
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
	cause := fmt.Errorf("event queue overflow: %w", native.ErrEventOverflow)
	s.recordFault(cause)
	close(release)

	op := <-opCh
	require.Nil(t, op, "a submission returning after the fault must not publish an Operation: %+v", op)
	err := <-errCh
	require.ErrorIs(t, err, cause, "post-fault submission error = %v, want the first cause reachable along the chain", err)
	require.ErrorIs(t, err, native.ErrEventOverflow, "post-fault submission error = %v, want the first cause reachable along the chain", err)
	assert.Equal(t, 0, pendingOccupancy(s), "pending occupancy after the fault = %d, want 0 (not re-inserted)", pendingOccupancy(s))
	assert.Equal(t, uint64(0), pendingReservedOf(s), "reservation count after the fault = %d, want 0", pendingReservedOf(s))
	assert.Equal(t, AsyncID(0), watermarkOfState(s), "high-water mark after the fault = %d, want 0 (not registered)", watermarkOfState(s))
}

// TestAsyncInputAndLifecycleRejections 钉住异步入口的输入与生命周期
// 拒绝:非法连接输入在 raw 前失败且无原生副作用;closed/faulted/
// closing 阶段三个入口按各自哨兵拒绝,不绕过状态调用。
func TestAsyncInputAndLifecycleRejections(t *testing.T) {
	t.Run("invalid connect input fails before raw", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF190, nil)
		bad := []struct {
			what string
			cfg  ConnectConfig
		}{
			{"relative ADBPath", ConnectConfig{ADBPath: "adb", Address: "127.0.0.1:5555"}},
			{"empty Address", ConnectConfig{ADBPath: testAbsolutePath("adb"), Address: ""}},
			{"embedded NUL", ConnectConfig{ADBPath: testAbsolutePath("adb") + "\x00", Address: "x"}},
			{"invalid UTF-8", ConnectConfig{ADBPath: testAbsolutePath("adb") + "\xff\xfe", Address: "x"}},
		}
		for _, tc := range bad {
			op, err := inst.Connect(tc.cfg)
			assert.Nil(t, op, "%s: Connect = (%+v, %v), want ErrInvalidInput", tc.what, op, err)
			assert.ErrorIs(t, err, native.ErrInvalidInput, "%s: Connect = (%+v, %v), want ErrInvalidInput", tc.what, op, err)
		}
		assert.Equal(t, 0, rig.caller.total(), "business call count with invalid input = %d, want 0 (fails before raw)", rig.caller.total())
	})

	t.Run("closed phase rejects all async entries", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF191, nil)
		closeRet := inst.Close()
		require.NoError(t, closeRet, "Close = %v, want nil", closeRet)
		for i, call := range []func() (*Operation, error){
			func() (*Operation, error) { return inst.Connect(connectInput) },
			func() (*Operation, error) { return inst.Click(1, 1) },
			func() (*Operation, error) { return inst.Screencap() },
		} {
			op, err := call()
			assert.Nil(t, op, "entry %d after closed = (%+v, %v), want ErrClosed", i, op, err)
			assert.ErrorIs(t, err, native.ErrClosed, "entry %d after closed = (%+v, %v), want ErrClosed", i, op, err)
		}
		assert.Equal(t, 0, rig.caller.total(), "business call count after closed = %d, want 0", rig.caller.total())
	})

	t.Run("faulted phase rejects all async entries with the first cause reachable along the chain", func(t *testing.T) {
		inst, rig := newAsyncRig(t, 0xF192, nil)
		cause := fmt.Errorf("fake: first fault: %w", native.ErrProtocol)
		inst.inst.state.recordFault(cause)
		for i, call := range []func() (*Operation, error){
			func() (*Operation, error) { return inst.Connect(connectInput) },
			func() (*Operation, error) { return inst.Click(1, 1) },
			func() (*Operation, error) { return inst.Screencap() },
		} {
			op, err := call()
			assert.Nil(t, op, "entry %d after faulted = (%+v, %v), want the first cause reachable along the chain", i, op, err)
			assert.ErrorIs(t, err, cause, "entry %d after faulted = (%+v, %v), want the first cause reachable along the chain", i, op, err)
			assert.ErrorIs(t, err, native.ErrProtocol, "entry %d after faulted = (%+v, %v), want the first cause reachable along the chain", i, op, err)
		}
		assert.Equal(t, 0, rig.caller.total(), "business call count after faulted = %d, want 0", rig.caller.total())
	})

	t.Run("closing phase rejects all async entries", func(t *testing.T) {
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
			op, err := call()
			assert.Nil(t, op, "entry %d during closing = (%+v, %v), want ErrClosing", i, op, err)
			assert.ErrorIs(t, err, native.ErrClosing, "entry %d during closing = (%+v, %v), want ErrClosing", i, op, err)
		}
		assert.Equal(t, 0, rig.caller.total(), "business call count during closing = %d, want 0", rig.caller.total())
		close(release)
		select {
		case err := <-closeErr:
			require.NoError(t, err, "Close = %v, want nil", err)
		case <-time.After(hangGuard):
			require.Fail(t, "Close did not complete within the hang-guard deadline")
		}
	})
}
