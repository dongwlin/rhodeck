// 本文件是 Runtime.Close 与创建/关闭竞争的受控用例:验证创建准入关闭、
// 未发布实例回滚等待、唯一 Destroy、并发关闭的稳定结果,以及终止后的
// Open/NewInstance 行为。

package maa

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// runRuntimeClose 以防挂死上限兜底执行一次 Runtime.Close(对照
// runClose/runNewInstance 先例):正常路径必须有限步完成;若实现挂
// 死,按超时失败而不是用例挂死。
func runRuntimeClose(t *testing.T, rt *Runtime) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- rt.Close() }()
	return waitErr(t, errCh, "Runtime.Close")
}

// waitErr 以防挂死上限兜底等待一个错误结果通道(对照 runClose 先例)。
func waitErr(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(hangGuard):
		require.FailNow(t, what+" did not complete within the hang-guard limit: suspected hang")
		return nil
	}
}

// assertRuntimeCloseNotReturned 在负面观察窗内断言 Runtime.Close 尚
// 未返回(窗口内被测方被屏障确定性挂起,调度延迟与被测行为分离;
// 提前返回即违反「回滚/关闭完成之前不返回」契约)。
func assertRuntimeCloseNotReturned(t *testing.T, ch <-chan error, what string) {
	t.Helper()
	select {
	case err := <-ch:
		require.FailNow(t, fmt.Sprintf("%s: Runtime.Close returned early: %v", what, err))
	case <-time.After(notWithin):
	}
}

// assertNoStopDestroy 断言关闭接缝尚未进入 native(Stop/Destroy 均
// 未被调用)。
func assertNoStopDestroy(t *testing.T, closer *fakeInstanceCloser, what string) {
	t.Helper()
	assert.Equal(t, 0, closer.count("stop"), "%s: Stop count = %d, want 0", what, closer.count("stop"))
	assert.Equal(t, 0, closer.count("destroy"), "%s: Destroy count = %d, want 0", what, closer.count("destroy"))
}

// assertSingleStopDestroy 断言恰一次 Stop、一次 Destroy 且顺序固定,
// Destroy 收到给定 handle(一次 Stop 再一次 Destroy)。
func assertSingleStopDestroy(t *testing.T, closer *fakeInstanceCloser, handle uintptr) {
	t.Helper()
	require.Equal(t, []string{"stop", "destroy"}, closer.calls(), "native call sequence = %v, want [stop destroy]", closer.calls())
	assert.Equal(t, handle, closer.destroyHandles[0], "Destroy received handle %#x, want %#x", closer.destroyHandles[0], handle)
}

// TestRuntimeCloseWithoutInstanceTerminatesLifecycle 钉住无任何实例时
// 的 Close 契约:无 ready 实例也无创建 lease 时 Close 仍完整终止使用
// (不能因尚无 ready Instance 而提前完成——本例无实例可等,直接
// 终止),不进入 native;重复 Close 取得同一稳定结果(nil)且不再进
// 入 native;同门 Open 被终止使用契约拒绝且不进入原生加载。
func TestRuntimeCloseWithoutInstanceTerminatesLifecycle(t *testing.T) {
	rig := newCreateRig(t, 0xEA00)
	err := runRuntimeClose(t, rig.rt)
	require.NoError(t, err, "Runtime.Close = %v, want nil (a normal close with no instance)", err)
	v := gateSnapshot(rig.gate)
	assert.True(t, v.state == lifecycleTerminated && v.closeRequested && !v.hasActive && !v.hasLease,
		"gate view after Close = %+v, want terminated/creation admission closed/no active instance/no lease", v)
	assertNoStopDestroy(t, rig.closer, "close with no instance")

	// 重复 Close:同一稳定最终结果,不再进入 native。
	repeatErr := runRuntimeClose(t, rig.rt)
	assert.NoError(t, repeatErr, "repeated Runtime.Close = %v, want the same result nil as the first", repeatErr)
	assertNoStopDestroy(t, rig.closer, "repeated close")

	// 终止使用契约:同门 Open 一律拒绝,不进入原生加载。
	env := &fakeEnv{session: newValidFakeSession()}
	_, openErr := open(validRuntimeConfig(), env, rig.gate)
	assert.ErrorIs(t, openErr, native.ErrRuntimeTerminated, "Open after termination = %v, want errors.Is native.ErrRuntimeTerminated", openErr)
	assert.Equal(t, 0, env.loadCount(), "Open after termination entered native loading %d times, want 0", env.loadCount())
}

// TestRuntimeCloseWaitsUnpublishedRollback 验证「未发布实例
// 完成回滚之前 Runtime.Close 不返回」与调度点竞争:创建事务进行中
// (lease 已登记)时,Close 关闭创建准入并等待 lease——创建者在发布
// 处被拒(ErrClosing),完成统一回滚(一次 Stop 一次 Destroy、token
// 移除)并释放 lease 之后 Close 才返回,且与创建者取得同一错误值。
// 子用例分别把竞争调度在 CreateEx 返回前(创建中)与返回后(选项应
// 用中);发布前纯预留拒绝与 CreateEx=0、早到故障叠加形态单列。
func TestRuntimeCloseWaitsUnpublishedRollback(t *testing.T) {
	t.Run("paused before CreateEx returns: Close waits for the rollback, new creations in the window rejected with ErrClosing", func(t *testing.T) {
		const handle = uintptr(0xEB00)
		rig := newCreateRig(t, handle)
		entered := make(chan struct{})
		release := make(chan struct{})
		rig.creator.onCreateEx = func(_, _ uintptr) {
			close(entered) // 此刻 lease 必已登记(CreateEx 先于发布)
			<-release
		}

		// 创建者:事务进行中,CreateEx 挂起。
		creationErr := make(chan error, 1)
		go func() {
			_, err := createInstance(rig.rt,
				InstanceConfig{Options: []InstanceOption{{Key: 51, Value: "v"}}},
				rig.creator, rig.closer, rig.caller)
			creationErr <- err
		}()
		<-entered

		// Runtime.Close:关闭创建准入后等待 lease。
		closeErr := make(chan error, 1)
		go func() { closeErr <- rig.rt.Close() }()
		waitUntil(t, "Runtime.Close closed creation admission", func() bool {
			return gateSnapshot(rig.gate).closeRequested
		})

		// 未发布实例回滚完成之前 Close 不返回;Close 也不代行 Destroy。
		assertRuntimeCloseNotReturned(t, closeErr, "while the creation transaction is suspended")
		assertNoStopDestroy(t, rig.closer, "while the creation transaction is suspended")

		// 窗口内的新创建:预留处被拒 ErrClosing,不触达 CreateEx(
		// Runtime.Close 期间创建者不得再发布实例;准入检查先于名额检查)。
		lateErr := make(chan error, 1)
		go func() {
			_, err := createInstance(rig.rt, InstanceConfig{}, rig.creator, rig.closer, rig.caller)
			lateErr <- err
		}()
		lateResult := waitErr(t, lateErr, "new creation within the window")
		assert.ErrorIs(t, lateResult, native.ErrClosing, "new creation within the window = %v, want errors.Is native.ErrClosing", lateResult)
		assert.Equal(t, 1, rig.creator.count("createEx"), "CreateEx count = %d, want still 1 (a rejected creation does not enter native)", rig.creator.count("createEx"))

		// 放行:创建者发布被拒,完成统一回滚并释放 lease。
		close(release)
		creatorResult := waitErr(t, creationErr, "creation transaction wrap-up")
		assert.ErrorIs(t, creatorResult, native.ErrClosing, "creator error = %v, want errors.Is native.ErrClosing (publish pre-empted by Close)", creatorResult)
		closeResult := waitErr(t, closeErr, "Runtime.Close")
		assert.True(t, closeResult == creatorResult, "Runtime.Close = %v, want the same stored value as the creator %v", closeResult, creatorResult)

		// 回滚恰一次 Stop/Destroy,handle 不遗留;token 已移除;lease
		// 释放(等待返回即证明回滚先于其完成);Runtime 终止使用。
		assertSingleStopDestroy(t, rig.closer, handle)
		assertTokenGone(t, rig.creator.exToken(0))
		waitLeaseReleased(t, rig.gate)
		assertRuntimeTerminated(t, rig.rt, rig.gate, "close")
	})

	t.Run("paused while options apply (CreateEx returned): publish pre-empted, Destroy exactly once", func(t *testing.T) {
		const handle = uintptr(0xEB01)
		rig := newCreateRig(t, handle)
		entered := make(chan struct{})
		release := make(chan struct{})
		rig.creator.onOption = func(n int, _ uintptr, _ int32, _ string) {
			if n == 1 {
				close(entered) // CreateEx 已返回、句柄已登记,首个选项应用中
				<-release
			}
		}

		creationErr := make(chan error, 1)
		go func() {
			_, err := createInstance(rig.rt,
				InstanceConfig{Options: []InstanceOption{{Key: 61, Value: "a"}, {Key: 62, Value: "b"}}},
				rig.creator, rig.closer, rig.caller)
			creationErr <- err
		}()
		<-entered

		closeErr := make(chan error, 1)
		go func() { closeErr <- rig.rt.Close() }()
		waitUntil(t, "Runtime.Close closed creation admission", func() bool {
			return gateSnapshot(rig.gate).closeRequested
		})
		assertRuntimeCloseNotReturned(t, closeErr, "while option application is suspended")
		assertNoStopDestroy(t, rig.closer, "while option application is suspended")

		close(release)
		creatorResult := waitErr(t, creationErr, "creation transaction wrap-up")
		assert.ErrorIs(t, creatorResult, native.ErrClosing, "creator error = %v, want errors.Is native.ErrClosing", creatorResult)
		closeResult := waitErr(t, closeErr, "Runtime.Close")
		assert.True(t, closeResult == creatorResult, "Runtime.Close = %v, want the same stored value as the creator %v", closeResult, creatorResult)

		//  步骤顺序:选项应用先于发布复核——中途关闭不中断剩余选项。
		assert.Equal(t, 2, rig.creator.optionCount(), "option call count = %d, want 2", rig.creator.optionCount())
		assertSingleStopDestroy(t, rig.closer, handle)
		assertTokenGone(t, rig.creator.exToken(0))
		waitLeaseReleased(t, rig.gate)
		assertRuntimeTerminated(t, rig.rt, rig.gate, "close")
	})

	t.Run("CreateEx returns 0: only the Go bridge state is cleaned up, no Destroy on the 0 handle", func(t *testing.T) {
		rig := newCreateRig(t, 0) // handle=0:CreateEx 失败
		entered := make(chan struct{})
		release := make(chan struct{})
		rig.creator.onCreateEx = func(_, _ uintptr) {
			close(entered)
			<-release
		}

		creationErr := make(chan error, 1)
		go func() {
			_, err := createInstance(rig.rt, InstanceConfig{}, rig.creator, rig.closer, rig.caller)
			creationErr <- err
		}()
		<-entered

		closeErr := make(chan error, 1)
		go func() { closeErr <- rig.rt.Close() }()
		waitUntil(t, "Runtime.Close closed creation admission", func() bool {
			return gateSnapshot(rig.gate).closeRequested
		})
		// 回滚完成之前 Close 不返回(零 handle 回滚也是创建事务收尾)。
		assertRuntimeCloseNotReturned(t, closeErr, "before the CreateEx=0 rollback completes")

		close(release)
		creatorResult := waitErr(t, creationErr, "creation transaction wrap-up")
		assert.ErrorIs(t, creatorResult, native.ErrNativeRejected, "creator error = %v, want errors.Is native.ErrNativeRejected", creatorResult)
		assert.Contains(t, creatorResult.Error(), "AsstCreateEx", "the creator error should contain the AsstCreateEx context:\n%s", creatorResult.Error())
		closeResult := waitErr(t, closeErr, "Runtime.Close")
		assert.True(t, closeResult == creatorResult, "Runtime.Close = %v, want the same stored value as the creator %v", closeResult, creatorResult)

		// CreateEx=0 的路径不调用 Stop/Destroy(不对 0 handle 销
		// 毁);Go 桥接状态(token/门/分发循环)照常清理。
		assertNoStopDestroy(t, rig.closer, "CreateEx=0 path")
		assertTokenGone(t, rig.creator.exToken(0))
		waitLeaseReleased(t, rig.gate)
		assertRuntimeTerminated(t, rig.rt, rig.gate, "AsstCreateEx")
	})

	t.Run("early fault combined with a pre-empted publish: the first cause is the primary cause, the publish rejection does not overwrite", func(t *testing.T) {
		const handle = uintptr(0xEB02)
		rig := newCreateRig(t, handle)
		rig.rt.limits.CallbackBytes = 16 // 收紧单条上限:32 字节无 NUL 即溢出故障
		entered := make(chan struct{})
		release := make(chan struct{})
		rig.creator.onCreateEx = func(_, token uintptr) {
			if deliver := native.LookupCallbackDelivery(token); deliver != nil {
				// CreateEx 期间的早到故障(有界扫描溢出):先于发布到达,
				// 成为 sticky 首因。
				deliver(82, callbackCBytes(strings.Repeat("x", 32)))
			}
			close(entered)
			<-release
		}

		creationErr := make(chan error, 1)
		go func() {
			_, err := createInstance(rig.rt, InstanceConfig{}, rig.creator, rig.closer, rig.caller)
			creationErr <- err
		}()
		<-entered

		closeErr := make(chan error, 1)
		go func() { closeErr <- rig.rt.Close() }()
		waitUntil(t, "Runtime.Close closed creation admission", func() bool {
			return gateSnapshot(rig.gate).closeRequested
		})
		assertRuntimeCloseNotReturned(t, closeErr, "before the rollback completes")

		close(release)
		creatorResult := waitErr(t, creationErr, "creation transaction wrap-up")
		// 发布被抢先返回 ErrClosing,但早到故障是主因(已有的首
		// 次故障/创建失败是主因):错误身份为 ErrEventOverflow 且不再可
		// 辨 ErrClosing。
		assert.ErrorIs(t, creatorResult, native.ErrEventOverflow, "creator error = %v, want errors.Is native.ErrEventOverflow (first cause dominates)", creatorResult)
		assert.NotErrorIs(t, creatorResult, native.ErrClosing, "the creator error must not overwrite the first cause with the publish rejection: %v", creatorResult)
		closeResult := waitErr(t, closeErr, "Runtime.Close")
		assert.True(t, closeResult == creatorResult, "Runtime.Close = %v, want the same stored value as the creator %v", closeResult, creatorResult)

		assertSingleStopDestroy(t, rig.closer, handle)
		assertTokenGone(t, rig.creator.exToken(0))
		waitLeaseReleased(t, rig.gate)
		assertRuntimeTerminated(t, rig.rt, rig.gate, "CallbackBytes")
	})
}

// TestRuntimeCloseClosesActiveInstanceOnce 钉住已发布实例路径与验收
// 条件「多个 Runtime.Close/Instance.Close 收到同一稳定最终结果,重复
// 关闭不再进入 native」:发布先于 Close(ready 发布竞争的胜出侧)时,
// 并发的 Runtime.Close 与 Instance.Close 共享同一 closeDone,全部取
// 得 nil,Stop/Destroy 恰好一次;终止后的重复关闭(两种入口)取得同
// 一结果且不再进入 native。
func TestRuntimeCloseClosesActiveInstanceOnce(t *testing.T) {
	const handle = uintptr(0xEC00)
	rig := newCreateRig(t, handle)
	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, InstanceConfig{})
	require.NoError(t, err, "NewInstance = %v, want nil", err)
	rig.track(inst)
	token := rig.creator.exToken(0)

	// 并发 Runtime.Close ×4 与 Instance.Close ×4:同一最终结果。
	const n = 4
	errs := make([]error, 2*n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			errs[2*i] = rig.rt.Close()
		}(i)
		go func(i int) {
			defer wg.Done()
			errs[2*i+1] = inst.Close()
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		assert.NoError(t, e, "Close %d = %v, want nil (the stable terminal state of a normal close)", i, e)
	}
	assertSingleStopDestroy(t, rig.closer, handle)

	// 终止后的重复关闭:同一结果,不再进入 native。
	repeatRtErr := runRuntimeClose(t, rig.rt)
	assert.NoError(t, repeatRtErr, "repeated Runtime.Close = %v, want nil", repeatRtErr)
	repeatInstErr := inst.Close()
	assert.NoError(t, repeatInstErr, "repeated Instance.Close = %v, want nil", repeatInstErr)
	assert.Equal(t, 1, rig.closer.count("destroy"), "Destroy count after repeated closes = %d, want still 1", rig.closer.count("destroy"))
	assertTokenGone(t, token)
	assertRuntimeTerminated(t, rig.rt, rig.gate, "")
}

// TestRuntimeCloseSharesStableFaultedResult 钉住非 nil 最终结果的共享
// 纪律:Stop 拒绝(ErrNativeRejected 诊断)成为最终错误时,并发与重
// 复的 Runtime.Close/Instance.Close 取得同一存储值(多次 Close
// 返回同一终态),销毁仍恰好一次。
func TestRuntimeCloseSharesStableFaultedResult(t *testing.T) {
	const handle = uintptr(0xEC01)
	rig := newCreateRig(t, handle)
	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, InstanceConfig{})
	require.NoError(t, err, "NewInstance = %v, want nil", err)
	rig.track(inst)
	rig.closer.stopRet = 0 // Stop 拒绝:最终错误非 nil,可观察同一性

	rtResult := runRuntimeClose(t, rig.rt)
	require.NotNil(t, rtResult, "Runtime.Close must not return nil when Stop returns 0")
	assert.ErrorIs(t, rtResult, native.ErrNativeRejected, "errors.Is(rtResult, native.ErrNativeRejected) = false; err = %v", rtResult)
	for _, want := range []string{"AsstStop", "0 (false)"} {
		assert.Contains(t, rtResult.Error(), want, "the final error should contain %q:\n%s", want, rtResult.Error())
	}
	// 并发(此处为后到)Instance.Close 与终止后的重复 Runtime.Close:
	// 同一存储值。
	instResult := inst.Close()
	assert.True(t, instResult == rtResult, "Instance.Close = %v, want the same stored value as Runtime.Close %v", instResult, rtResult)
	rtRepeat := runRuntimeClose(t, rig.rt)
	assert.True(t, rtRepeat == rtResult, "repeated Runtime.Close = %v, want the same stored value %v", rtRepeat, rtResult)
	assertSingleStopDestroy(t, rig.closer, handle)
	assertTokenGone(t, rig.creator.exToken(0))
	assertRuntimeTerminated(t, rig.rt, rig.gate, "AsstStop")
}

// TestRuntimeCloseNativeWithoutHeldLocks 记录「不持锁进入 native」检
// 查(交付物;对照 TestInstanceCloseSyncCallbackDuringNativeCalls
// 先例):Runtime.Close 驱动的 native stop 挂起期间,生命周期门锁、
// 实例状态锁与 callback 准入投递都能限时完成——实现若持任一锁跨
// native 调用,对应探针即超时失败。
func TestRuntimeCloseNativeWithoutHeldLocks(t *testing.T) {
	const handle = uintptr(0xED00)
	rig := newCreateRig(t, handle)
	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, InstanceConfig{})
	require.NoError(t, err, "NewInstance = %v, want nil", err)
	rig.track(inst)
	token := rig.creator.exToken(0)

	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	rig.closer.onStop = func(handle uintptr) {
		close(stopStarted)
		<-releaseStop
	}
	closeErr := make(chan error, 1)
	go func() { closeErr <- rig.rt.Close() }()
	<-stopStarted

	// 探针一:生命周期门锁操作(native stop 挂起期间)必须能完成。
	gateCh := make(chan gateView, 1)
	go func() { gateCh <- gateSnapshot(rig.gate) }()
	select {
	case v := <-gateCh:
		assert.True(t, v.closeRequested, "creation admission should already be closed in the destroy phase")
	case <-time.After(hangGuard):
		require.Fail(t, "the lifecycle gate lock operation did not complete during the native stop: the implementation appears to hold the gate lock across native calls")
	}

	// 探针二:实例状态锁操作必须能完成(closing 阶段可观察)。
	phaseCh := make(chan instancePhase, 1)
	go func() { phaseCh <- inst.inst.state.currentPhase() }()
	select {
	case got := <-phaseCh:
		assert.Equal(t, phaseClosing, got, "instance phase during the native stop = %d, want closing", got)
	case <-time.After(hangGuard):
		require.Fail(t, "the state lock operation did not complete during the native stop: the implementation appears to hold the state lock across native calls")
	}

	// 探针三:callback 准入 + 入队(closing 阶段门仍开放)必须能
	// 完成——注册表、callback 门与分发器锁均不跨 native 持有。
	delivered := make(chan struct{})
	go func() {
		if deliver := native.LookupCallbackDelivery(token); deliver != nil {
			deliver(83, callbackCBytes(`{"what":"DuringRuntimeCloseStop"}`))
		}
		close(delivered)
	}()
	select {
	case <-delivered:
	case <-time.After(hangGuard):
		require.Fail(t, "callback delivery did not complete during the native stop: the implementation appears to hold the callback/dispatcher/registry lock across native calls")
	}

	close(releaseStop)
	err = waitErr(t, closeErr, "Runtime.Close")
	require.NoError(t, err, "Runtime.Close = %v, want nil", err)
	// stop 期间投递的事件关闭后仍可读,Events 随后已关闭。
	ev := mustReadEvent(t, inst.inst.disp.events())
	assert.Equal(t, MessageID(83), ev.MessageID, "event MessageID = %d, want 83", ev.MessageID)
	_, ok := <-inst.inst.disp.events()
	assert.False(t, ok, "Events should be closed after Close")
	assertSingleStopDestroy(t, rig.closer, handle)
}

// TestRuntimeCloseTerminatedUseContract 验证「Close 后
// NewInstance/Open 遵守终止使用契约;预占前的纯 Go 配置错误不消耗生
// 命周期」:创建前纯 Go 校验失败返回 ErrInvalidInput 且门原样保留
// (ready、未请求关闭),修正前的失败不妨碍随后 Close 正常终止使用;
// 终止后有效配置的 NewInstance 在预留处被拒(ErrRuntimeTerminated,
// 不触达 native),无效配置仍在预占前被拒(ErrInvalidInput)。
func TestRuntimeCloseTerminatedUseContract(t *testing.T) {
	rig := newCreateRig(t, 0xEE00)
	badCfg := InstanceConfig{Options: []InstanceOption{{Key: 0, Value: "invalid"}}}

	// 预占前纯 Go 配置错误:ErrInvalidInput,不触碰生命周期门。
	_, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, badCfg)
	require.ErrorIs(t, err, native.ErrInvalidInput, "config error before reservation = %v, want errors.Is native.ErrInvalidInput", err)
	v := gateSnapshot(rig.gate)
	require.True(t, v.state == lifecycleReady && !v.closeRequested && !v.hasLease, "the config error consumed the lifecycle gate: %+v", v)
	require.Equal(t, 0, rig.creator.count("createEx"), "a validation failure must not reach CreateEx")

	// Close 正常终止使用(配置错误未预消耗生命周期)。
	err = runRuntimeClose(t, rig.rt)
	require.NoError(t, err, "Runtime.Close = %v, want nil", err)

	// 终止后 Open:一律拒绝,不进入原生加载。
	env := &fakeEnv{session: newValidFakeSession()}
	_, openErr := open(validRuntimeConfig(), env, rig.gate)
	assert.ErrorIs(t, openErr, native.ErrRuntimeTerminated, "Open after termination = %v, want errors.Is native.ErrRuntimeTerminated", openErr)
	assert.Equal(t, 0, env.loadCount(), "Open after termination entered native loading %d times, want 0", env.loadCount())

	// 终止后 NewInstance:有效配置在预留处被拒(公开入口,生产接缝在
	// 触达 native 前被拒);无效配置仍在预占前被拒,不消耗生命周期。
	_, err = runPublicNewInstance(t, rig.rt, InstanceConfig{})
	assert.ErrorIs(t, err, native.ErrRuntimeTerminated, "NewInstance after termination (valid config) = %v, want errors.Is native.ErrRuntimeTerminated", err)
	_, err = runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, badCfg)
	assert.ErrorIs(t, err, native.ErrInvalidInput, "NewInstance after termination (invalid config) = %v, want errors.Is native.ErrInvalidInput", err)
	assert.Equal(t, 0, rig.creator.count("createEx"), "CreateEx count after termination = %d, want 0", rig.creator.count("createEx"))
}
