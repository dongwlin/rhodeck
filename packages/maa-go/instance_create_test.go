// 本文件是实例创建事务与统一失败回滚的受控用例(MAA-21;绑定设计
// §4、§8):白盒同包验证 NewInstance 的完整创建事务——预占前纯 Go
// 校验失败不消耗 Runtime、唯一实例名额与创建 lease、token/callback
// 门/事件队列先于 CreateEx 可用且早到消息不遗失、CreateEx=0 不调用
// 无效 handle、取得 handle 后选项失败与早到故障走单次关闭、ready 发
// 布与 Runtime 状态复核在同一状态门内(已 closing 时由创建者完成回
// 滚再释放 lease),以及失败后不保留活动 token、Runtime 终止使用不
// 允许同进程重建。native 创建以 fakeInstanceCreator 注入(对照
// instance_test.go 的 fakeInstanceCloser 先例),早到消息经真实
// token 注册表查表投递(LookupCallbackDelivery → target.deliver),
// 钉住 §4「注册表、事件队列先于 CreateEx 建立」的时序。全部用例以
// 受控屏障(channel/轮询等待)确定性同步,不加载任何原生库、不触达
// loader/真实 API 表,CGO_ENABLED=0 可跑;防挂死只用 hangGuard 兜底,
// 不作同步手段。
//
// maa→native 的 token 注册表是进程级共享状态:本文件用例互不并行
// (不调 t.Parallel);成功创建的实例由用例显式 Close 或经 rig.cleanup
// 兜底收尾,不留悬挂 goroutine 与未注销 token 影响 -race。
//
// 早到故障的时序边界:发布点(publishInstance 内 tryPublishReady 的
// 状态锁段)是创建期故障与业务期故障的线性化边界——发布前到达的早
// 到故障使创建失败并回滚,发布后到达的故障进入已发布实例的业务期
// 故障路径(§7.1),两者由同一状态锁保证互斥,不存在「已 faulted 仍
// 发布」的窗口。

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

// fakeOptionCall 记录一次 SetInstanceOption 调用:value 是 *byte 参数
// 解码出的 C 字符串内容(空串编码为单个 NUL,解码回空串)。
type fakeOptionCall struct {
	handle uintptr
	key    int32
	value  string
}

// fakeInstanceCreator 是创建事务的受控 fake native:逐次记录 CreateEx
// 与 SetInstanceOption 调用(顺序、参数、handle),支持注入返回值
// (handle=0 模拟 CreateEx 失败,rejectOptionN 模拟第 N 次选项拒绝)
// 与动作(onCreateEx 在 CreateEx 返回前执行,供早到消息/屏障注入;
// onOption 在第 N 次选项应用时执行)。不加载任何原生库。
type fakeInstanceCreator struct {
	mu            sync.Mutex
	sequence      []string
	exCallbacks   []uintptr
	exTokens      []uintptr
	optionCalls   []fakeOptionCall
	handle        uintptr
	rejectOptionN int
	onCreateEx    func(callback, customArg uintptr)
	onOption      func(n int, handle uintptr, key int32, value string)
}

// newFakeInstanceCreator 构造 fake creator:CreateEx 默认返回给定
// handle,选项全部成功。
func newFakeInstanceCreator(handle uintptr) *fakeInstanceCreator {
	return &fakeInstanceCreator{handle: handle}
}

func (f *fakeInstanceCreator) createEx(callback, customArg uintptr) uintptr {
	f.mu.Lock()
	f.sequence = append(f.sequence, "createEx")
	f.exCallbacks = append(f.exCallbacks, callback)
	f.exTokens = append(f.exTokens, customArg)
	f.mu.Unlock()
	if f.onCreateEx != nil {
		f.onCreateEx(callback, customArg)
	}
	return f.handle
}

func (f *fakeInstanceCreator) setInstanceOption(handle uintptr, key int32, value *byte) uint8 {
	f.mu.Lock()
	n := len(f.sequence) // createEx 已记录,此处序号即第 n 次选项调用
	f.sequence = append(f.sequence, "setInstanceOption")
	f.optionCalls = append(f.optionCalls, fakeOptionCall{handle: handle, key: key, value: cArgString(value)})
	reject := f.rejectOptionN == n
	f.mu.Unlock()
	if f.onOption != nil {
		f.onOption(n, handle, key, cArgString(value))
	}
	if reject {
		return 0
	}
	return 1
}

func (f *fakeInstanceCreator) count(name string) int {
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

func (f *fakeInstanceCreator) optionRecord(i int) fakeOptionCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.optionCalls[i]
}

func (f *fakeInstanceCreator) optionCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.optionCalls)
}

func (f *fakeInstanceCreator) exToken(i int) uintptr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exTokens[i]
}

func (f *fakeInstanceCreator) exCallback(i int) uintptr {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exCallbacks[i]
}

// fakeAppendCall 记录一次 AppendTask 调用:taskType 与 params 是
// *byte 参数解码出的 C 字符串内容(逐字节还原,含未知字段原文)。
type fakeAppendCall struct {
	handle   uintptr
	taskType string
	params   string
}

// fakeSetParamsCall 记录一次 SetTaskParams 调用。
type fakeSetParamsCall struct {
	handle uintptr
	id     int32
	params string
}

// fakeBoolCall 记录一次 bool 返回类业务调用(Start/Stop/BackToHome/
// Running/Connected)。
type fakeBoolCall struct {
	name   string
	handle uintptr
}

// fakeInstanceCaller 是同步业务调用的受控 fake native(对照
// fakeInstanceCreator 先例):逐次记录全部业务调用(方法名、顺序、
// handle 与解码参数),支持注入返回值(缺省全部成功)与动作
// (onStart 在 start 返回前执行,供屏障注入)。不加载任何原生库。
type fakeInstanceCaller struct {
	mu             sync.Mutex
	sequence       []string
	appendCalls    []fakeAppendCall
	setParamsCalls []fakeSetParamsCall
	boolCalls      []fakeBoolCall
	appendRet      int32
	setParamsRet   uint8
	boolRets       map[string]uint8
	onStart        func(handle uintptr)
}

// newFakeInstanceCaller 构造 fake caller:全部调用缺省成功(appendTask
// 返回 1,其余 bool 类返回 1)。
func newFakeInstanceCaller() *fakeInstanceCaller {
	return &fakeInstanceCaller{appendRet: 1, setParamsRet: 1}
}

// boolRet 返回指定 bool 类调用的注入返回值:未注入的名称缺省 1。
// 要求持有 mu。
func (f *fakeInstanceCaller) boolRetLocked(name string) uint8 {
	if ret, ok := f.boolRets[name]; ok {
		return ret
	}
	return 1
}

func (f *fakeInstanceCaller) appendTask(handle uintptr, taskType *byte, params *byte) int32 {
	f.mu.Lock()
	f.sequence = append(f.sequence, "appendTask")
	f.appendCalls = append(f.appendCalls, fakeAppendCall{handle: handle, taskType: cArgString(taskType), params: cArgString(params)})
	ret := f.appendRet
	f.mu.Unlock()
	return ret
}

func (f *fakeInstanceCaller) setTaskParams(handle uintptr, id int32, params *byte) uint8 {
	f.mu.Lock()
	f.sequence = append(f.sequence, "setTaskParams")
	f.setParamsCalls = append(f.setParamsCalls, fakeSetParamsCall{handle: handle, id: id, params: cArgString(params)})
	ret := f.setParamsRet
	f.mu.Unlock()
	return ret
}

func (f *fakeInstanceCaller) start(handle uintptr) uint8 {
	f.mu.Lock()
	f.sequence = append(f.sequence, "start")
	f.boolCalls = append(f.boolCalls, fakeBoolCall{name: "start", handle: handle})
	f.mu.Unlock()
	if f.onStart != nil {
		f.onStart(handle)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boolRetLocked("start")
}

func (f *fakeInstanceCaller) stop(handle uintptr) uint8 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sequence = append(f.sequence, "stop")
	f.boolCalls = append(f.boolCalls, fakeBoolCall{name: "stop", handle: handle})
	return f.boolRetLocked("stop")
}

func (f *fakeInstanceCaller) backToHome(handle uintptr) uint8 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sequence = append(f.sequence, "backToHome")
	f.boolCalls = append(f.boolCalls, fakeBoolCall{name: "backToHome", handle: handle})
	return f.boolRetLocked("backToHome")
}

func (f *fakeInstanceCaller) running(handle uintptr) uint8 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sequence = append(f.sequence, "running")
	f.boolCalls = append(f.boolCalls, fakeBoolCall{name: "running", handle: handle})
	return f.boolRetLocked("running")
}

func (f *fakeInstanceCaller) connected(handle uintptr) uint8 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sequence = append(f.sequence, "connected")
	f.boolCalls = append(f.boolCalls, fakeBoolCall{name: "connected", handle: handle})
	return f.boolRetLocked("connected")
}

// count 返回指定方法名的累计调用次数。
func (f *fakeInstanceCaller) count(name string) int {
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

// total 返回全部业务调用的累计次数(断言「无原生副作用」)。
func (f *fakeInstanceCaller) total() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sequence)
}

// calls 返回调用序列的副本。
func (f *fakeInstanceCaller) calls() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sequence...)
}

func (f *fakeInstanceCaller) appendRecord(i int) fakeAppendCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.appendCalls[i]
}

func (f *fakeInstanceCaller) setParamsRecord(i int) fakeSetParamsCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.setParamsCalls[i]
}

func (f *fakeInstanceCaller) boolRecord(i int) fakeBoolCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.boolCalls[i]
}

// gateView 是生命周期门创建相关字段的白盒快照(锁内读取,不绕锁)。
type gateView struct {
	state          lifecycleState
	slot           instanceSlotState
	closeRequested bool
	hasLease       bool // creationDone 已建立且尚未释放
	hasActive      bool // 已发布活动实例
}

func gateSnapshot(g *lifecycleGate) gateView {
	g.mu.Lock()
	defer g.mu.Unlock()
	return gateView{
		state:          g.state,
		slot:           g.slot,
		closeRequested: g.closeRequested,
		hasLease:       g.creationDone != nil && !g.creationDoneClosed,
		hasActive:      g.activeInstance != nil,
	}
}

// createRig 组装受控创建环境:独立生命周期门(不触碰进程级门)的
// ready Runtime、fake creator、closer 与 caller。成功创建的实例经
// track 登记,cleanup 兜底 Close(幂等),不留未注销 token 与悬挂
// 分发循环。
type createRig struct {
	rt      *Runtime
	gate    *lifecycleGate
	creator *fakeInstanceCreator
	closer  *fakeInstanceCloser
	caller  *fakeInstanceCaller

	mu   sync.Mutex
	inst *Instance
}

// newCreateRig 构造受控创建环境并登记 t.Cleanup 兜底。
func newCreateRig(t *testing.T, handle uintptr) *createRig {
	t.Helper()
	gate := &lifecycleGate{state: lifecycleReady}
	rt := &Runtime{
		caps:     native.Capabilities{},
		version:  "fake",
		nullSize: profileNullSize,
		limits:   defaultLimits(),
		gate:     gate,
	}
	creator := newFakeInstanceCreator(handle)
	closer := newFakeInstanceCloser()
	caller := newFakeInstanceCaller()
	rig := &createRig{rt: rt, gate: gate, creator: creator, closer: closer, caller: caller}
	t.Cleanup(func() { rig.cleanup() })
	return rig
}

// track 登记创建成功的实例供 cleanup 兜底(nil 是被拒/失败路径,忽略)。
func (r *createRig) track(inst *Instance) {
	if inst == nil {
		return
	}
	r.mu.Lock()
	r.inst = inst
	r.mu.Unlock()
}

// cleanup 兜底收尾:成功创建且未关闭的实例按幂等 Close 收敛(关闭流
// 程注销 token 并终结分发循环);失败路径的 token 已由回滚注销,实例
// 为 nil,无需处理。
func (r *createRig) cleanup() {
	r.mu.Lock()
	in := r.inst
	r.mu.Unlock()
	if in != nil {
		_ = in.Close()
	}
}

// runNewInstance 以防挂死上限兜底执行一次创建事务:经私有
// createInstance 注入 fake creator/closer/caller(对照
// open(cfg, env, gate) 先例——公开 NewInstance 固定使用生产接缝,
// 触达 native 的用例必须走注入路径)。正常路径必须有限步完成;若实
// 现挂死,按超时失败而不是用例挂死。
func runNewInstance(t *testing.T, rt *Runtime, creator instanceCreator, closer instanceCloser, caller instanceCaller, cfg InstanceConfig) (*Instance, error) {
	t.Helper()
	type outcome struct {
		inst *Instance
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		inst, err := createInstance(rt, cfg, creator, closer, caller)
		ch <- outcome{inst, err}
	}()
	select {
	case o := <-ch:
		return o.inst, o.err
	case <-time.After(hangGuard):
		t.Fatal("NewInstance 未在防挂死上限内完成:创建事务疑似挂死")
		return nil, nil
	}
}

// runPublicNewInstance 以防挂死上限兜底执行一次公开 NewInstance:仅
// 用于不触达 native 的路径(纯 Go 校验失败、预留被拒——生产 creator
// 绑定真实 API 表,到达 CreateEx 即编程错误)。
func runPublicNewInstance(t *testing.T, rt *Runtime, cfg InstanceConfig) (*Instance, error) {
	t.Helper()
	type outcome struct {
		inst *Instance
		err  error
	}
	ch := make(chan outcome, 1)
	go func() {
		inst, err := rt.NewInstance(cfg)
		ch <- outcome{inst, err}
	}()
	select {
	case o := <-ch:
		return o.inst, o.err
	case <-time.After(hangGuard):
		t.Fatal("NewInstance 未在防挂死上限内完成:创建事务疑似挂死")
		return nil, nil
	}
}

// waitLeaseReleased 以防挂死上限兜底等待创建 lease 释放。
func waitLeaseReleased(t *testing.T, g *lifecycleGate) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		g.waitCreationLease()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(hangGuard):
		t.Fatal("创建 lease 未在防挂死上限内释放:创建事务疑似未收尾")
	}
}

// assertTokenGone 以「重复注销按注册表契约 panic」正向证明 token 已
// 由流程移除,失败后的活动 token 不被保留(§4:token 永不复用)。
func assertTokenGone(t *testing.T, token uintptr) {
	t.Helper()
	assertPanics(t, "token 已移除后重复注销", func() {
		native.UnregisterCallbackTarget(token)
	})
}

// assertRuntimeTerminated 断言失败回滚/实例关闭后的终止使用契约:门
// 已 terminated 且不再有活动实例;同进程重建被拒——NewInstance 与
// Open 侧的 reserve 都返回 ErrRuntimeTerminated,附首个失败原因文本。
func assertRuntimeTerminated(t *testing.T, rt *Runtime, gate *lifecycleGate, firstFailureText string) {
	t.Helper()
	v := gateSnapshot(gate)
	if v.state != lifecycleTerminated {
		t.Errorf("门状态 = %d, want terminated", v.state)
	}
	if v.hasActive {
		t.Error("终止使用后不应存在活动实例登记")
	}
	// 公开 NewInstance(生产 creator):终止使用在预留处拒绝,先于一
	// 切 creator 使用,无表可触碰;同时覆盖公开方法的委托路径。
	_, err := runPublicNewInstance(t, rt, InstanceConfig{})
	if !errors.Is(err, native.ErrRuntimeTerminated) {
		t.Errorf("终止后 NewInstance = %v, want errors.Is native.ErrRuntimeTerminated", err)
	}
	if err := gate.reserve(); !errors.Is(err, native.ErrRuntimeTerminated) {
		t.Errorf("终止后 gate.reserve = %v, want errors.Is native.ErrRuntimeTerminated", err)
	}
	if firstFailureText != "" && !strings.Contains(err.Error(), firstFailureText) {
		t.Errorf("终止诊断应附首个失败原因 %q:\n%s", firstFailureText, err.Error())
	}
}

// TestNewInstanceGateReservationContract 钉住预留/复核接口自身契约
// (§8:预留与复核在 Runtime 同一状态门内):非 ready 拒绝预留;预留
// 幂等拒绝第二次;发布只在 creating 名额上进行,slot 不符即编程错误
// panic;创建准入关闭后预留与发布都被拒;lease 未预留即释放或重复
// 释放是编程错误 panic;无可等待 lease 的等待立即返回。
func TestNewInstanceGateReservationContract(t *testing.T) {
	t.Run("非 ready 状态拒绝预留", func(t *testing.T) {
		g := &lifecycleGate{}
		if err := g.reserveInstance(); !errors.Is(err, native.ErrRuntimeInUse) {
			t.Errorf("idle 门预留 = %v, want errors.Is native.ErrRuntimeInUse", err)
		}
	})
	t.Run("预留唯一名额,二次预留拒绝", func(t *testing.T) {
		g := &lifecycleGate{state: lifecycleReady}
		if err := g.reserveInstance(); err != nil {
			t.Fatalf("首次预留不得失败: %v", err)
		}
		if err := g.reserveInstance(); !errors.Is(err, native.ErrRuntimeInUse) {
			t.Errorf("二次预留 = %v, want errors.Is native.ErrRuntimeInUse", err)
		}
	})
	t.Run("名额不符时发布是编程错误", func(t *testing.T) {
		g := &lifecycleGate{state: lifecycleReady}
		assertPanics(t, "slot=none 时发布", func() {
			_ = g.publishInstance(&instance{})
		})
	})
	t.Run("创建准入关闭后预留与发布都拒绝", func(t *testing.T) {
		g := &lifecycleGate{state: lifecycleReady}
		if !g.closeCreationAdmission() {
			t.Fatal("首个创建准入关闭申请必须生效")
		}
		if g.closeCreationAdmission() {
			t.Error("重复关闭创建准入应幂等返回 false")
		}
		if err := g.reserveInstance(); !errors.Is(err, native.ErrClosing) {
			t.Errorf("准入关闭后预留 = %v, want errors.Is native.ErrClosing", err)
		}
		g.mu.Lock()
		g.slot = slotCreating
		g.creationDone = make(chan struct{})
		g.mu.Unlock()
		if err := g.publishInstance(&instance{}); !errors.Is(err, native.ErrClosing) {
			t.Errorf("准入关闭后发布 = %v, want errors.Is native.ErrClosing", err)
		}
	})
	t.Run("lease 释放纪律", func(t *testing.T) {
		g := &lifecycleGate{state: lifecycleReady}
		assertPanics(t, "未预留即释放 lease", func() { g.releaseCreationLease() })
		if err := g.reserveInstance(); err != nil {
			t.Fatalf("预留不得失败: %v", err)
		}
		g.releaseCreationLease()
		assertPanics(t, "重复释放 lease", func() { g.releaseCreationLease() })
	})
	t.Run("无可等待 lease 的等待立即返回", func(t *testing.T) {
		g := &lifecycleGate{state: lifecycleReady}
		done := make(chan struct{})
		go func() {
			g.waitCreationLease()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(notWithin):
			t.Fatal("无 lease 的等待应立即返回")
		}
	})
}

// TestNewInstanceValidationFailureDoesNotReserve 钉住验收条件 1 前半
// 「预占前纯 Go 校验失败不消耗 Runtime」:实例配置校验发生在触碰生
// 命周期门之前,失败返回带字段路径的 ErrInvalidInput,名额、lease
// 与生命周期门原样保留,修正输入后同门仍可成功创建。
func TestNewInstanceValidationFailureDoesNotReserve(t *testing.T) {
	rig := newCreateRig(t, 0xCA00)
	bad := []struct {
		what string
		cfg  InstanceConfig
	}{
		{"option key 0(Invalid)", InstanceConfig{Options: []InstanceOption{{Key: 0, Value: "x"}}}},
		{"option key 重复", InstanceConfig{Options: []InstanceOption{{Key: 3, Value: "a"}, {Key: 3, Value: "b"}}}},
		{"value 内嵌 NUL", InstanceConfig{Options: []InstanceOption{{Key: 3, Value: "a\x00b"}}}},
		{"value 非法 UTF-8", InstanceConfig{Options: []InstanceOption{{Key: 3, Value: string([]byte{0xff, 0xfe})}}}},
		{"value 超单字符串预算", InstanceConfig{Options: []InstanceOption{{Key: 3, Value: strings.Repeat("x", 65_537)}}}},
	}
	for _, tc := range bad {
		_, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, tc.cfg)
		if !errors.Is(err, native.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want errors.Is native.ErrInvalidInput", tc.what, err)
		}
		if rig.creator.count("createEx") != 0 {
			t.Fatalf("%s: 校验失败不得触达 CreateEx", tc.what)
		}
		v := gateSnapshot(rig.gate)
		if v.state != lifecycleReady || v.slot != slotNone || v.closeRequested || v.hasLease {
			t.Errorf("%s: 校验失败消耗了生命周期门: %+v", tc.what, v)
		}
	}
	// 修正输入后同门仍可成功创建:校验失败不消耗 Runtime。
	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, InstanceConfig{Options: []InstanceOption{{Key: 3, Value: "ok"}}})
	if err != nil {
		t.Fatalf("校验失败后同门创建不得失败: %v", err)
	}
	rig.track(inst)
	waitLeaseReleased(t, rig.gate)
}

// TestNewInstanceSuccessPublishesReady 钉住成功路径:CreateEx 收到进
// 程唯一 trampoline 与已登记 token,实例选项按序应用(含空 value 编
// 码为空 C 字符串),ready 发布、名额转 active、创建 lease 释放;唯
// 一名额已占用时再次创建被拒(同进程不重建实例,§8);实例关闭后
// Runtime 终止使用且 Stop/Destroy 恰好一次。
func TestNewInstanceSuccessPublishesReady(t *testing.T) {
	const handle = uintptr(0xCB00)
	rig := newCreateRig(t, handle)
	cfg := InstanceConfig{Options: []InstanceOption{{Key: 123, Value: "opt-a"}, {Key: -5, Value: ""}}}

	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, cfg)
	if err != nil {
		t.Fatalf("NewInstance = %v, want nil", err)
	}
	rig.track(inst)
	token := rig.creator.exToken(0)

	// CreateEx 参数:进程唯一 trampoline + 已登记 token(§4)。
	if got := rig.creator.exCallback(0); got != native.CallbackTrampoline() {
		t.Errorf("CreateEx callback = %#x, want 进程唯一 trampoline %#x", got, native.CallbackTrampoline())
	}
	if token == 0 {
		t.Fatal("CreateEx custom_arg 不得为 0")
	}
	// 实例选项按序应用,value 逐字透传(空串编码为空 C 字符串)。
	if got := rig.creator.optionCount(); got != 2 {
		t.Fatalf("SetInstanceOption 次数 = %d, want 2", got)
	}
	for i, want := range []fakeOptionCall{{handle: handle, key: 123, value: "opt-a"}, {handle: handle, key: -5, value: ""}} {
		if got := rig.creator.optionRecord(i); got != want {
			t.Errorf("选项调用 %d = %+v, want %+v", i, got, want)
		}
	}
	// ready 发布与名额状态:状态 ready、句柄已登记、名额 active、活
	// 动实例已登记、创建 lease 已释放(§8 NewInstance 第 6、7 步)。
	if got := inst.inst.state.currentPhase(); got != phaseReady {
		t.Errorf("实例阶段 = %d, want ready", got)
	}
	if got := stateHandle(inst.inst.state); got != handle {
		t.Errorf("实例 handle = %#x, want %#x", got, handle)
	}
	if v := gateSnapshot(rig.gate); v.state != lifecycleReady || v.slot != slotActive || !v.hasActive || v.hasLease {
		t.Errorf("发布后门视图 = %+v, want ready/active/有活动实例/lease 已释放", v)
	}
	waitLeaseReleased(t, rig.gate)

	// 唯一名额已占用:再次创建被拒(§8:首期单活动实例)。
	if _, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, InstanceConfig{}); !errors.Is(err, native.ErrRuntimeInUse) {
		t.Errorf("名额占用后再次创建 = %v, want errors.Is native.ErrRuntimeInUse", err)
	}
	if got := rig.creator.count("createEx"); got != 1 {
		t.Errorf("CreateEx 次数 = %d, want 1(被拒不触达 native)", got)
	}

	// 公开 Close:单次关闭流程,Stop/Destroy 恰好一次,Runtime 终止
	// 使用(§8:一个实例关闭后 Runtime 进入终止态)。
	if err := inst.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
		t.Fatalf("native 调用序列 = %v, want [stop destroy]", seq)
	}
	if got := rig.closer.destroyHandles[0]; got != handle {
		t.Errorf("Destroy 收到 handle %#x, want %#x", got, handle)
	}
	assertTokenGone(t, token)
	if got := inst.inst.state.currentPhase(); got != phaseClosed {
		t.Errorf("关闭后实例阶段 = %d, want closed", got)
	}
	// terminated 附首个失败原因?正常关闭无失败原因,裸哨兵即可。
	assertRuntimeTerminated(t, rig.rt, rig.gate, "")
}

// TestNewInstanceEarlyMessagesDuringCreateEx 钉住验收条件 2「CreateEx
// 调用前 token、callback 门和队列已可用,早到消息不会遗失状态」:
// CreateEx 期间与选项应用期间经真实注册表查表投递的早到消息照常准
// 入入队,发布后在公开事件缓冲按序可读——查表未命中即证明 token 未
// 先行登记,准入失败即证明门未开放。
func TestNewInstanceEarlyMessagesDuringCreateEx(t *testing.T) {
	rig := newCreateRig(t, 0xCC00)
	deliverEarly := func(token uintptr, msg int32, payload string, failures *[]string) {
		deliver := native.LookupCallbackDelivery(token)
		if deliver == nil {
			*failures = append(*failures, fmt.Sprintf("token %d 未在注册表登记:注册表未先于 CreateEx 建立", token))
			return
		}
		deliver(msg, callbackCBytes(payload))
	}
	var failures []string
	rig.creator.onCreateEx = func(_, token uintptr) {
		// CreateEx 期间(返回之前)的早到消息:门与队列已可用。
		deliverEarly(token, 77, `{"what":"EarlyDuringCreateEx"}`, &failures)
	}
	rig.creator.onOption = func(n int, _ uintptr, _ int32, _ string) {
		if n == 1 {
			// 选项应用期间的早到消息:同样照常准入(与 onCreateEx 同一
			// 创建 goroutine 顺序执行,无数据竞争)。
			deliverEarly(rig.creator.exToken(0), 78, `{"what":"EarlyDuringOption"}`, &failures)
		}
	}

	// 至少一个实例选项,使选项应用期间的早到消息钩子得以执行。
	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller,
		InstanceConfig{Options: []InstanceOption{{Key: 41, Value: "anchor"}}})
	if err != nil {
		t.Fatalf("NewInstance = %v, want nil", err)
	}
	rig.track(inst)
	if len(failures) > 0 {
		t.Fatalf("早到消息投递失败: %v", failures)
	}
	// 早到消息不遗失:按投递顺序在公开事件缓冲可读(本地序号单调)。
	ev1 := mustReadEvent(t, inst.inst.disp.events())
	ev2 := mustReadEvent(t, inst.inst.disp.events())
	if ev1.MessageID != 77 || ev2.MessageID != 78 {
		t.Errorf("早到事件 = %d, %d, want 77, 78(按序不遗失)", ev1.MessageID, ev2.MessageID)
	}
	if ev1.Sequence >= ev2.Sequence {
		t.Errorf("本地事件序号非单调: %d >= %d", ev1.Sequence, ev2.Sequence)
	}
	// 失败后不保留活动 token 的对照面:成功实例的 token 在 Close 前仍
	// 在注册表(查表可命中)。
	token := rig.creator.exToken(0)
	if native.LookupCallbackDelivery(token) == nil {
		t.Error("已发布实例的 token 应仍在注册表中")
	}
	if err := inst.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	assertTokenGone(t, token)
}

// TestNewInstanceCreateExZeroRollback 钉住验收条件 3「CreateEx 返回 0
// 时关闭临时 callback 准入、等待在途回调并移除 token,不调用无效
// handle」:零 handle 回滚不执行 Stop/Destroy,关门、退空、移除
// token、排空与终态发布照常,Runtime 终止使用。
func TestNewInstanceCreateExZeroRollback(t *testing.T) {
	rig := newCreateRig(t, 0) // handle=0:CreateEx 失败
	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, InstanceConfig{})
	if inst != nil {
		t.Fatalf("CreateEx=0 不得返回实例: %+v", inst)
	}
	if !errors.Is(err, native.ErrNativeRejected) {
		t.Fatalf("err = %v, want errors.Is native.ErrNativeRejected", err)
	}
	for _, want := range []string{"AsstCreateEx", "0 (null handle)"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应含 %q:\n%s", want, err.Error())
		}
	}
	// 不调用无效 handle(§8.1:CreateEx=0 的路径不调用 Stop/Destroy)。
	if got := rig.closer.count("stop"); got != 0 {
		t.Errorf("CreateEx=0 路径的 Stop 次数 = %d, want 0", got)
	}
	if got := rig.closer.count("destroy"); got != 0 {
		t.Errorf("CreateEx=0 路径的 Destroy 次数 = %d, want 0", got)
	}
	// 临时 callback 准入已关闭、token 已移除、分发循环已收敛、终态已
	// 发布(§8:清理临时 token 和 Go 状态)。
	token := rig.creator.exToken(0)
	assertTokenGone(t, token)
	if got := rig.creator.optionCount(); got != 0 {
		t.Errorf("CreateEx=0 路径的选项调用次数 = %d, want 0", got)
	}
	waitLeaseReleased(t, rig.gate)
	assertRuntimeTerminated(t, rig.rt, rig.gate, "AsstCreateEx")
}

// TestNewInstanceOptionFailureRollsBackSingleClose 钉住验收条件 4 前
// 半「取得 handle 后选项失败走单次关闭,不向调用者返回半初始化
// Instance」:首个原生拒绝即停止按序应用,回滚执行一次 Stop、一次
// Destroy(§8.1:已创建 handle 后失败执行同一销毁流程),错误保留选
// 项失败主因,Runtime 终止使用。
func TestNewInstanceOptionFailureRollsBackSingleClose(t *testing.T) {
	const handle = uintptr(0xCD00)
	rig := newCreateRig(t, handle)
	rig.creator.rejectOptionN = 2
	cfg := InstanceConfig{Options: []InstanceOption{{Key: 11, Value: "first"}, {Key: -7, Value: "second"}, {Key: 13, Value: "third"}}}

	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, cfg)
	if inst != nil {
		t.Fatalf("选项失败不得返回实例: %+v", inst)
	}
	if !errors.Is(err, native.ErrNativeRejected) {
		t.Fatalf("err = %v, want errors.Is native.ErrNativeRejected", err)
	}
	for _, want := range []string{"SetInstanceOption", "key=-7"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误应含 %q:\n%s", want, err.Error())
		}
	}
	// 按序应用在首个拒绝处停止:第 1 项成功,第 2 项被拒,第 3 项不
	// 再调用。
	if got := rig.creator.optionCount(); got != 2 {
		t.Errorf("选项调用次数 = %d, want 2(首个拒绝后停止)", got)
	}
	// 单次关闭:一次 Stop、一次 Destroy,handle 逐字转发。
	if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
		t.Fatalf("native 调用序列 = %v, want [stop destroy]", seq)
	}
	if got := rig.closer.destroyHandles[0]; got != handle {
		t.Errorf("Destroy 收到 handle %#x, want %#x", got, handle)
	}
	// 回滚完成才释放 lease;token 已移除;Runtime 终止使用。
	token := rig.creator.exToken(0)
	waitLeaseReleased(t, rig.gate)
	assertTokenGone(t, token)
	assertRuntimeTerminated(t, rig.rt, rig.gate, "SetInstanceOption")
}

// TestNewInstanceEarlyFaultFailsCreation 钉住验收条件 4 后半「早到故
// 障走单次关闭,不向调用者返回半初始化 Instance」:CreateEx 期间与
// 选项应用期间到达的早到故障(有界扫描溢出,ErrEventOverflow 链)使
// 发布被拒,回滚执行一次 Stop、一次 Destroy,错误为首因,丢失计数与
// 故障通知就位,Runtime 终止使用。
func TestNewInstanceEarlyFaultFailsCreation(t *testing.T) {
	t.Run("CreateEx 期间的早到故障", func(t *testing.T) {
		const handle = uintptr(0xCE00)
		rig := newCreateRig(t, handle)
		rig.rt.limits.CallbackBytes = 16 // 收紧单条上限:32 字节无 NUL 即溢出故障
		rig.creator.onCreateEx = func(_, token uintptr) {
			if deliver := native.LookupCallbackDelivery(token); deliver != nil {
				deliver(79, callbackCBytes(strings.Repeat("x", 32)))
			}
		}
		cfg := InstanceConfig{Options: []InstanceOption{{Key: 21, Value: "applied"}}}

		inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, cfg)
		if inst != nil {
			t.Fatalf("早到故障不得返回实例: %+v", inst)
		}
		if !errors.Is(err, native.ErrEventOverflow) {
			t.Fatalf("err = %v, want errors.Is native.ErrEventOverflow", err)
		}
		// 单次关闭照常执行(§8:callback 已可接收,早到故障会使创建失
		// 败并按既定规则清理)。
		if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
			t.Fatalf("native 调用序列 = %v, want [stop destroy]", seq)
		}
		if got := rig.closer.destroyHandles[0]; got != handle {
			t.Errorf("Destroy 收到 handle %#x, want %#x", got, handle)
		}
		if got := rig.creator.optionCount(); got != 1 {
			t.Errorf("选项调用次数 = %d, want 1(选项先于发布复核应用)", got)
		}
		// 溢出消息未投递、按溢出契约计丢失:错误身份(ErrEventOverflow
		// 链)已钉住故障路径;丢失计数与首因前缀的可见面归公开诊断接线。
		token := rig.creator.exToken(0)
		waitLeaseReleased(t, rig.gate)
		assertTokenGone(t, token)
		assertRuntimeTerminated(t, rig.rt, rig.gate, "CallbackBytes")
	})

	t.Run("选项应用中途的早到故障", func(t *testing.T) {
		const handle = uintptr(0xCE01)
		rig := newCreateRig(t, handle)
		rig.rt.limits.CallbackBytes = 16
		rig.creator.onOption = func(n int, _ uintptr, _ int32, _ string) {
			if n == 1 {
				if deliver := native.LookupCallbackDelivery(rig.creator.exToken(0)); deliver != nil {
					deliver(80, callbackCBytes(strings.Repeat("y", 32)))
				}
			}
		}
		cfg := InstanceConfig{Options: []InstanceOption{{Key: 22, Value: "one"}, {Key: 23, Value: "two"}}}

		inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, cfg)
		if inst != nil {
			t.Fatalf("早到故障不得返回实例: %+v", inst)
		}
		if !errors.Is(err, native.ErrEventOverflow) {
			t.Fatalf("err = %v, want errors.Is native.ErrEventOverflow", err)
		}
		// 中途故障不中断剩余选项的应用(§8 步骤顺序:应用选项 → 复核
		// 发布),发布点原子复核拦下故障。
		if got := rig.creator.optionCount(); got != 2 {
			t.Errorf("选项调用次数 = %d, want 2", got)
		}
		if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
			t.Fatalf("native 调用序列 = %v, want [stop destroy]", seq)
		}
		waitLeaseReleased(t, rig.gate)
		assertTokenGone(t, rig.creator.exToken(0))
		assertRuntimeTerminated(t, rig.rt, rig.gate, "CallbackBytes")
	})
}

// TestNewInstancePublishRejectedByRuntimeClose 钉住验收条件 5「ready
// 发布与 Runtime 状态复核在同一状态门内,已 closing 时由创建者完成回
// 滚再释放 lease」:创建准入关闭后完成的创建事务不发布实例,创建者
// 执行一次 Stop、一次 Destroy 后才释放 lease(lease 释放时回滚已完
// 成、token 已移除),返回 ErrClosing,Runtime 终止使用。
func TestNewInstancePublishRejectedByRuntimeClose(t *testing.T) {
	const handle = uintptr(0xCF00)
	rig := newCreateRig(t, handle)
	entered := make(chan struct{})
	release := make(chan struct{})
	rig.creator.onCreateEx = func(_, _ uintptr) {
		close(entered)
		<-release
	}

	errCh := make(chan error, 1)
	var inst *Instance
	go func() {
		i, err := createInstance(rig.rt, InstanceConfig{Options: []InstanceOption{{Key: 31, Value: "late"}}}, rig.creator, rig.closer, rig.caller)
		inst = i
		errCh <- err
	}()
	<-entered

	// Runtime.Close 侧(创建准入关闭,MAA-25 接入完整等待):创建准
	// 入关闭后,正在进行的创建事务不被打断,发布将被拒绝。
	if !rig.gate.closeCreationAdmission() {
		t.Fatal("首个创建准入关闭申请必须生效")
	}
	close(release)

	select {
	case err := <-errCh:
		if inst != nil {
			t.Fatalf("已 closing 时不得返回实例: %+v", inst)
		}
		if !errors.Is(err, native.ErrClosing) {
			t.Fatalf("err = %v, want errors.Is native.ErrClosing", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("创建事务未在防挂死上限内收尾")
	}

	// 由创建者完成回滚再释放 lease:lease 释放时(等待返回即证明)
	// Stop/Destroy 已各执行一次、token 已移除——不存在未回滚先释放。
	if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
		t.Fatalf("native 调用序列 = %v, want [stop destroy]", seq)
	}
	if got := rig.closer.destroyHandles[0]; got != handle {
		t.Errorf("Destroy 收到 handle %#x, want %#x", got, handle)
	}
	token := rig.creator.exToken(0)
	waitLeaseReleased(t, rig.gate)
	assertTokenGone(t, token)
	// 未发布:名额保持 creating(名额不复用),无活动实例。
	if v := gateSnapshot(rig.gate); v.hasActive || v.slot != slotCreating {
		t.Errorf("发布被拒后的门视图 = %+v, want 无活动实例且名额不复用", v)
	}
	assertRuntimeTerminated(t, rig.rt, rig.gate, "close")
}

// TestNewInstanceConcurrentCreationSingleReserve 钉住唯一实例名额的
// 并发纪律:并发 NewInstance 恰一个预留成功进入 CreateEx,其余在预
// 留处被拒(ErrRuntimeInUse),native 创建恰一次。
func TestNewInstanceConcurrentCreationSingleReserve(t *testing.T) {
	rig := newCreateRig(t, 0xD000)
	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	rig.creator.onCreateEx = func(_, _ uintptr) {
		enteredOnce.Do(func() { close(entered) })
		<-release
	}

	const n = 8
	errs := make([]error, n)
	insts := make([]*Instance, n)
	var rejected atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			inst, err := createInstance(rig.rt, InstanceConfig{}, rig.creator, rig.closer, rig.caller)
			insts[i], errs[i] = inst, err
			if errors.Is(err, native.ErrRuntimeInUse) {
				rejected.Add(1)
			}
		}(i)
	}
	// 恰一个创建者到达 CreateEx(唯一名额);其余申请在名额预留处被
	// 拒——被拒者不触达 native,等待其全部返回后再放行胜出者。
	<-entered
	waitUntil(t, "其余申请在名额预留处被拒", func() bool { return rejected.Load() == n-1 })
	if got := rig.creator.count("createEx"); got != 1 {
		t.Fatalf("CreateEx 次数 = %d, want 1(唯一名额)", got)
	}
	close(release)
	wg.Wait()

	won := 0
	for i, err := range errs {
		switch {
		case err == nil:
			won++
			if insts[i] == nil {
				t.Errorf("创建者 %d 返回 nil 实例与 nil 错误", i)
			}
		case errors.Is(err, native.ErrRuntimeInUse):
			if insts[i] != nil {
				t.Errorf("被拒创建者 %d 不得取得实例", i)
			}
		default:
			t.Errorf("创建者 %d = %v, want nil 或 ErrRuntimeInUse", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("成功创建数 = %d, want 1", won)
	}
	waitLeaseReleased(t, rig.gate)
	// 兜底收敛:胜出实例由 rig.cleanup 幂等 Close。
}

// TestInstanceCloseConcurrentTerminatesRuntime 钉住公开 Close 的幂等
// 与终止使用契约:并发 Close 只有一次 Stop/Destroy,全部申请取得同
// 一最终错误(nil),实例关闭后 Runtime 终止使用,重复 Close 不再进
// 入 native。
func TestInstanceCloseConcurrentTerminatesRuntime(t *testing.T) {
	rig := newCreateRig(t, 0xD100)
	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, InstanceConfig{})
	if err != nil {
		t.Fatalf("NewInstance = %v, want nil", err)
	}
	rig.track(inst)
	token := rig.creator.exToken(0)

	const n = 8
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = inst.Close()
		}(i)
	}
	wg.Wait()

	if got := rig.closer.count("destroy"); got != 1 {
		t.Errorf("Destroy 次数 = %d, want 恰 1", got)
	}
	for i, e := range errs {
		if e != nil {
			t.Errorf("Close %d = %v, want nil", i, e)
		}
	}
	// closed 后的幂等 Close:同一终态,不再触发 native。
	if err := inst.Close(); err != nil {
		t.Errorf("幂等 Close = %v, want nil", err)
	}
	if got := rig.closer.count("destroy"); got != 1 {
		t.Errorf("幂等 Close 后 Destroy 次数 = %d, want 仍为 1", got)
	}
	assertTokenGone(t, token)
	assertRuntimeTerminated(t, rig.rt, rig.gate, "")
}

// TestRollbackCreationWithoutTokenTail 钉住 token 登记失败路径的回滚
// 机制(token=0:无 instance 组装、注册表无本实例 token、不可能有回
// 调准入):创建者代行唯一关闭所有者走同一 closeTail 尾段——不执行
// Stop/Destroy、无 token 可移除,关门、排空与终态发布照常。登记失败
// 本身依赖 token 空间耗尽(进程级,不可注入),此处按 rollbackCreation
// 的 token=0 分支逐步复现其机制。
func TestRollbackCreationWithoutTokenTail(t *testing.T) {
	b := generousBudgets()
	s := newInstanceState(0, 128)
	d := newEventDispatcher(s, s.settleCompletion, b.callbackBytes, b.internalEvents, b.internalBytes, b.publicEvents, b.publicBytes)
	d.start()
	gate := &lifecycleGate{state: lifecycleReady}
	rt := &Runtime{limits: defaultLimits(), gate: gate}
	if err := gate.reserveInstance(); err != nil {
		t.Fatalf("预留不得失败: %v", err)
	}

	cause := fmt.Errorf("fake: token registration failed: %w", native.ErrProtocol)
	err := rollbackCreation(rt, s, d, 0, newFakeInstanceCloser(), cause)
	if err != cause {
		t.Fatalf("回滚错误 = %v, want 与创建失败主因同一存储值 %v", err, cause)
	}
	// 终态:无 native 调用(closer 未注入任何调用)、关门、Events 关
	// 闭且为空、lease 已释放、Runtime 终止使用。
	if _, ok := s.admitCallback(); ok {
		t.Error("回滚后 callback 准入门仍开放")
	}
	if _, ok := <-d.events(); ok {
		t.Error("回滚后 Events 应已关闭且为空")
	}
	if got := s.currentPhase(); got != phaseClosed {
		t.Errorf("回滚后实例阶段 = %d, want closed", got)
	}
	waitLeaseReleased(t, gate)
	assertRuntimeTerminated(t, rt, gate, "token registration failed")
}
