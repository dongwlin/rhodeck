// 本文件是任务与同步实例方法的受控用例(MAA-22;绑定设计 §3、§5、
// §6、§7):白盒同包验证公开同步入口的行为契约——AppendTask 原样透
// 传未知任务类型与 JSON 字段、TaskID 0 明确表示提交失败;SetTaskParams
// /Start/Stop/BackToHome 的 nil 只表示请求被接受,不赋予任务执行语
// 义;Running/Connected 的正常 false 与生命周期错误分离;内嵌 NUL、
// 非法 JSON 等输入在 raw 前失败且无原生副作用;Log 等 void 入口只有
// 输入或生命周期错误;全部实例原生入口经过同一 lease 与同一 raw 串
// 行门禁,closing/faulted/closed 后不绕过状态调用,关闭等待覆盖门禁
// 前排队的同步调用。native 业务调用以 fakeInstanceCaller 注入
// (instance_create_test.go 的接缝先例),fake closer/creator 复用既有
// 注入路径。全部用例以受控屏障(channel/轮询等待)确定性同步,不加
// 载任何原生库、不触达 loader/真实 API 表,CGO_ENABLED=0 可跑;防挂
// 死只用 hangGuard 兜底,不作同步手段。
//
// maa→native 的 token 注册表是进程级共享状态:本文件用例互不并行
// (不调 t.Parallel);成功创建的实例由用例显式 Close 或经 rig.cleanup
// 兜底收尾,不留悬挂 goroutine 与未注销 token 影响 -race。

package maa

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// newSyncRig 构造带 fake caller 的受控实例:实例经 runNewInstance 的
// 生产创建事务接线(公开同步方法在创建成功后立即可用),handle 为
// 给定占位值。在途调用计数等白盒诊断读取复用 state_test.go 的
// inFlightCalls。
func newSyncRig(t *testing.T, handle uintptr) (*Instance, *createRig) {
	t.Helper()
	rig := newCreateRig(t, handle)
	inst, err := runNewInstance(t, rig.rt, rig.creator, rig.closer, rig.caller, InstanceConfig{})
	if err != nil {
		t.Fatalf("NewInstance = %v, want nil", err)
	}
	rig.track(inst)
	return inst, rig
}

// TestAppendTaskPassesUnknownTypeAndJSONVerbatim 钉住验收条件「
// AppendTask 保留未知任务类型与 JSON 字段,TaskID 0 明确表示提交失
// 败」:未知任务类型与含未知字段、超过 2^53 的整数、\u0000 转义的
// params 按原文字节透传(解码回的 C 字符串与输入逐字节一致,证明不
// 丢字段、不经浮点往返),handle 来自已准入 lease;原生返回 0 时得
// 到 TaskID 0 与包装 ErrNativeRejected 的错误。
func TestAppendTaskPassesUnknownTypeAndJSONVerbatim(t *testing.T) {
	const handle = uintptr(0xE100)
	inst, rig := newSyncRig(t, handle)

	const taskType = "MyCustomTask"
	raw := json.RawMessage(`{"unknown_field":1,"big":9007199254740993,"nested":{"note":"a\u0000b"}}`)
	rig.caller.appendRet = 123
	id, err := inst.AppendTask(taskType, raw)
	if err != nil {
		t.Fatalf("AppendTask = %v, want nil", err)
	}
	if id != TaskID(123) {
		t.Errorf("AppendTask id = %d, want 123", id)
	}
	if got := rig.caller.count("appendTask"); got != 1 {
		t.Fatalf("AppendTask 调用次数 = %d, want 1", got)
	}
	rec := rig.caller.appendRecord(0)
	if rec.handle != handle {
		t.Errorf("AppendTask 收到 handle %#x, want %#x(实例唯一受门控句柄)", rec.handle, handle)
	}
	if rec.taskType != taskType {
		t.Errorf("AppendTask taskType = %q, want %q(未知任务类型原样透传)", rec.taskType, taskType)
	}
	if rec.params != string(raw) {
		t.Errorf("AppendTask params 逐字节透传被破坏:\n got %q\nwant %q", rec.params, string(raw))
	}
	for _, want := range []string{"unknown_field", "9007199254740993", `a\u0000b`} {
		if !strings.Contains(rec.params, want) {
			t.Errorf("AppendTask params 应保留 %q:\n%s", want, rec.params)
		}
	}

	// 原生返回 0:TaskID 0 明确表示提交失败,错误包装 ErrNativeRejected。
	rig.caller.appendRet = 0
	id, err = inst.AppendTask(taskType, json.RawMessage(`{}`))
	if id != 0 {
		t.Errorf("提交失败时 id = %d, want 0", id)
	}
	if !errors.Is(err, native.ErrNativeRejected) {
		t.Errorf("提交失败 err = %v, want errors.Is native.ErrNativeRejected", err)
	}
	if !strings.Contains(err.Error(), "AsstAppendTask") {
		t.Errorf("错误应指明接口:\n%s", err.Error())
	}
	if got := rig.caller.count("appendTask"); got != 2 {
		t.Errorf("AppendTask 调用次数 = %d, want 2", got)
	}
}

// TestAcceptedRequestsAreNotTaskCompletion 钉住验收条件「SetTaskParams
// 、Start 等接受请求不会被包装成任务已完成」:C bool=true 的请求类
// 入口返回恰为 nil(nil 只表示请求被接受,执行语义归事件流,见各方
// 法注释);C bool=false 包装 ErrNativeRejected。SetTaskParams 的 id
// 保留 int32 符号位原样透传(§3)。
func TestAcceptedRequestsAreNotTaskCompletion(t *testing.T) {
	const handle = uintptr(0xE200)
	inst, rig := newSyncRig(t, handle)
	params := json.RawMessage(`{"stage":"1-7"}`)

	t.Run("SetTaskParams 接受为恰 nil,拒绝为 ErrNativeRejected", func(t *testing.T) {
		if err := inst.SetTaskParams(7, params); err != nil {
			t.Fatalf("SetTaskParams = %v, want 恰 nil(接受≠完成)", err)
		}
		rec := rig.caller.setParamsRecord(0)
		if rec.handle != handle || rec.id != 7 || rec.params != string(params) {
			t.Errorf("SetTaskParams 调用记录 = %+v, want handle %#x、id 7、params 逐字节透传", rec, handle)
		}
		if err := inst.SetTaskParams(-5, params); err != nil {
			t.Fatalf("SetTaskParams(-5) = %v, want 恰 nil", err)
		}
		if got := rig.caller.setParamsRecord(1).id; got != -5 {
			t.Errorf("SetTaskParams id = %d, want -5(int32 符号位保留)", got)
		}
		rig.caller.setParamsRet = 0
		if err := inst.SetTaskParams(7, params); !errors.Is(err, native.ErrNativeRejected) {
			t.Errorf("被拒 SetTaskParams = %v, want errors.Is native.ErrNativeRejected", err)
		}
	})
	t.Run("Start/Stop/BackToHome 接受为恰 nil,拒绝为 ErrNativeRejected", func(t *testing.T) {
		type entry struct {
			name  string
			call  func() error
			calls func() int
		}
		entries := []entry{
			{"Start", inst.Start, func() int { return rig.caller.count("start") }},
			{"Stop", inst.Stop, func() int { return rig.caller.count("stop") }},
			{"BackToHome", inst.BackToHome, func() int { return rig.caller.count("backToHome") }},
		}
		for i, e := range entries {
			if err := e.call(); err != nil {
				t.Fatalf("%s = %v, want 恰 nil(接受≠任务已完成)", e.name, err)
			}
			if got := e.calls(); got != 1 {
				t.Errorf("%s 调用次数 = %d, want 1", e.name, got)
			}
			if rec := rig.caller.boolRecord(i); rec.handle != handle {
				t.Errorf("%s 收到 handle %#x, want %#x", e.name, rec.handle, handle)
			}
		}
		for _, name := range []string{"start", "stop", "backToHome"} {
			rig.caller.boolRets = map[string]uint8{name: 0}
			var err error
			switch name {
			case "start":
				err = inst.Start()
			case "stop":
				err = inst.Stop()
			case "backToHome":
				err = inst.BackToHome()
			}
			if !errors.Is(err, native.ErrNativeRejected) {
				t.Errorf("被拒 %s = %v, want errors.Is native.ErrNativeRejected", name, err)
			}
		}
		delete(rig.caller.boolRets, "start")
		delete(rig.caller.boolRets, "stop")
		delete(rig.caller.boolRets, "backToHome")
	})
}

// TestRunningConnectedFalseIsNotAnError 钉住验收条件前半「Running/
// Connected 返回 false 时无额外错误」:false 是正常查询结果,不转换
// 成错误;true 原样返回。
func TestRunningConnectedFalseIsNotAnError(t *testing.T) {
	const handle = uintptr(0xE210)
	inst, rig := newSyncRig(t, handle)

	cases := []struct {
		what        string
		ret         uint8
		wantRunning bool
		wantErr     bool
	}{
		{"running=false", 0, false, false},
		{"running=true", 1, true, false},
	}
	for _, tc := range cases {
		rig.caller.boolRets = map[string]uint8{"running": tc.ret}
		got, err := inst.Running()
		if got != tc.wantRunning || (err != nil) != tc.wantErr {
			t.Errorf("Running(%s) = (%v, %v), want (%v, nil)", tc.what, got, err, tc.wantRunning)
		}
	}
	rig.caller.boolRets = map[string]uint8{"connected": 0}
	if got, err := inst.Connected(); got || err != nil {
		t.Errorf("Connected(false) = (%v, %v), want (false, nil)", got, err)
	}
	rig.caller.boolRets = map[string]uint8{"connected": 1}
	if got, err := inst.Connected(); !got || err != nil {
		t.Errorf("Connected(true) = (%v, %v), want (true, nil)", got, err)
	}
}

// TestSyncEntriesRejectByLifecycle 钉住验收条件后半「生命周期失败仍
// 可单独识别」与执行步骤「closing/faulted 后的拒绝行为」:closed 阶
// 段全部入口包装 ErrClosed 且无原生副作用;faulted 阶段包装首因(错
// 误链可达原生哨兵),关闭仍可完成;closing 阶段(关闭流程执行中)
// 全部入口包装 ErrClosing。
func TestSyncEntriesRejectByLifecycle(t *testing.T) {
	// allEntries 以合法输入触达全部同步入口:拒绝必须来自生命周期而
	// 非输入校验。
	allEntries := func(inst *Instance) []error {
		_, errRunning := inst.Running()
		_, errConnected := inst.Connected()
		_, errAppend := inst.AppendTask("Fighting", json.RawMessage(`{}`))
		return []error{
			errRunning, errConnected, errAppend,
			inst.SetTaskParams(1, json.RawMessage(`{}`)),
			inst.Start(), inst.Stop(), inst.BackToHome(),
		}
	}

	t.Run("closed 阶段拒绝全部入口且无原生副作用", func(t *testing.T) {
		inst, rig := newSyncRig(t, 0xE220)
		if err := inst.Close(); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		for i, err := range allEntries(inst) {
			if !errors.Is(err, native.ErrClosed) {
				t.Errorf("入口 %d = %v, want errors.Is native.ErrClosed", i, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("closed 后业务调用次数 = %d, want 0(不绕过状态调用)", got)
		}
	})
	t.Run("faulted 阶段拒绝全部入口且错误链可达首因", func(t *testing.T) {
		inst, rig := newSyncRig(t, 0xE221)
		cause := fmt.Errorf("fake: first fault: %w", native.ErrProtocol)
		inst.inst.state.recordFault(cause)
		for i, err := range allEntries(inst) {
			if !errors.Is(err, cause) || !errors.Is(err, native.ErrProtocol) {
				t.Errorf("入口 %d = %v, want 沿错误链可达首因与 native.ErrProtocol", i, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("faulted 后业务调用次数 = %d, want 0(不绕过状态调用)", got)
		}
		// faulted 后关闭仍可完成(§8:异常路径进入 faulted 后关闭)。
		if err := inst.Close(); !errors.Is(err, cause) {
			t.Errorf("faulted 后 Close = %v, want 首因", err)
		}
	})
	t.Run("closing 阶段拒绝全部入口", func(t *testing.T) {
		inst, rig := newSyncRig(t, 0xE222)
		entered := make(chan struct{})
		release := make(chan struct{})
		rig.closer.onStop = func(uintptr) {
			close(entered)
			<-release
		}
		closeErr := make(chan error, 1)
		go func() { closeErr <- inst.Close() }()
		// 关闭所有者已进入一次 Stop(closing 已生效、在途调用已清零)。
		<-entered
		for i, err := range allEntries(inst) {
			if !errors.Is(err, native.ErrClosing) {
				t.Errorf("closing 中入口 %d = %v, want errors.Is native.ErrClosing", i, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("closing 中业务调用次数 = %d, want 0(不绕过状态调用)", got)
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

// TestInvalidInputFailsBeforeNative 钉住验收条件「内嵌 NUL、非法
// JSON 或非法输入在 raw 前失败,不发生原生副作用」:全部输入校验失
// 败包装 ErrInvalidInput 且业务 fake 与日志 fake 的调用次数为 0——
// 校验先于生命周期门与 raw 门禁,不进入 native。
func TestInvalidInputFailsBeforeNative(t *testing.T) {
	const handle = uintptr(0xE230)
	inst, rig := newSyncRig(t, handle)

	badParams := []struct {
		what string
		raw  json.RawMessage
	}{
		{"nil", nil},
		{"空白", json.RawMessage("   ")},
		{"null", json.RawMessage(`null`)},
		{"数组", json.RawMessage(`[1,2]`)},
		{"标量", json.RawMessage(`42`)},
		{"截断", json.RawMessage(`{"a":`)},
		{"尾随第二个值", json.RawMessage(`{"a":1} {"b":2}`)},
		{"实际 NUL", json.RawMessage("{\"a\":\"\x00\"}")},
		{"非法 UTF-8", json.RawMessage("{\"a\":\"\xff\xfe\"}")},
	}
	t.Run("AppendTask", func(t *testing.T) {
		badTypes := []struct {
			what string
			typ  string
		}{
			{"空 taskType", ""},
			{"内嵌 NUL", "a\x00b"},
			{"非法 UTF-8", string([]byte{0xff, 0xfe})},
		}
		for _, tc := range badTypes {
			_, err := inst.AppendTask(tc.typ, json.RawMessage(`{}`))
			if !errors.Is(err, native.ErrInvalidInput) {
				t.Errorf("%s: err = %v, want errors.Is native.ErrInvalidInput", tc.what, err)
			}
		}
		for _, tc := range badParams {
			_, err := inst.AppendTask("Fighting", tc.raw)
			if !errors.Is(err, native.ErrInvalidInput) {
				t.Errorf("params %s: err = %v, want errors.Is native.ErrInvalidInput", tc.what, err)
			}
		}
	})
	t.Run("SetTaskParams", func(t *testing.T) {
		for _, tc := range badParams {
			if err := inst.SetTaskParams(1, tc.raw); !errors.Is(err, native.ErrInvalidInput) {
				t.Errorf("params %s: err = %v, want errors.Is native.ErrInvalidInput", tc.what, err)
			}
		}
	})
	// 全部非法输入(含正确输入的对照组之外)都未触达任何原生业务调用。
	if got := rig.caller.total(); got != 0 {
		t.Errorf("非法输入的业务调用次数 = %d, want 0(raw 前失败)", got)
	}
}

// TestLogReturnsOnlyInputOrLifecycleErrors 钉住验收条件「Log 等 void
// 函数只返回输入或生命周期错误,不伪造原生成功确认」:合法输入返回
// nil——nil 只表示调用已发出并返回,不构成原生确认(C void 无成功
// 码);非法输入在 native 前失败;Runtime 终止后返回包装
// ErrRuntimeTerminated 的生命周期错误。level 与 message 原样透传,
// 允许空串。
func TestLogReturnsOnlyInputOrLifecycleErrors(t *testing.T) {
	gate := &lifecycleGate{state: lifecycleReady}
	logger := newFakeRuntimeLogger()
	rt := &Runtime{limits: defaultLimits(), gate: gate, logger: logger}

	if err := rt.Log("info", "hello binding"); err != nil {
		t.Fatalf("Log = %v, want nil(无输入/生命周期错误即无错误)", err)
	}
	rec := logger.record(0)
	if rec.level != "info" || rec.message != "hello binding" {
		t.Errorf("Log 记录 = %+v, want level/message 逐字节透传", rec)
	}
	// 空串允许,由 Core 解释。
	if err := rt.Log("", ""); err != nil {
		t.Errorf("Log(空串) = %v, want nil", err)
	}
	if rec := logger.record(1); rec.level != "" || rec.message != "" {
		t.Errorf("Log 空串记录 = %+v, want 空串透传", rec)
	}

	// 生命周期终止后:错误包装 ErrRuntimeTerminated,native 不再触达。
	cause := fmt.Errorf("fake: terminated cause: %w", native.ErrProtocol)
	gate.terminate(cause)
	err := rt.Log("info", "after termination")
	if !errors.Is(err, native.ErrRuntimeTerminated) {
		t.Errorf("终止后 Log = %v, want errors.Is native.ErrRuntimeTerminated", err)
	}
	if !strings.Contains(err.Error(), "terminated cause") {
		t.Errorf("终止诊断应附首个失败原因文本:\n%s", err.Error())
	}
	if got := logger.count(); got != 2 {
		t.Errorf("终止后 Log 调用次数 = %d, want 仍为 2(生命周期前失败)", got)
	}
}

// TestLogInputFailsBeforeNative 钉住 Log 的输入预算:单字符串预算、
// 合计 ConfigBytes 预算、UTF-8 与 NUL 校验都在 native 前失败。
func TestLogInputFailsBeforeNative(t *testing.T) {
	gate := &lifecycleGate{state: lifecycleReady}
	logger := newFakeRuntimeLogger()
	rt := &Runtime{
		limits: Limits{InputStringBytes: 16, ConfigBytes: 20},
		gate:   gate,
		logger: logger,
	}
	cases := []struct {
		what    string
		level   string
		message string
	}{
		{"level 内嵌 NUL", "in\x00fo", "x"},
		{"message 非法 UTF-8", "info", string([]byte{0xff, 0xfe})},
		{"message 超单字符串预算", "info", strings.Repeat("a", 17)},
		{"合计超 ConfigBytes", "ab", strings.Repeat("b", 19)},
	}
	for _, tc := range cases {
		if err := rt.Log(tc.level, tc.message); !errors.Is(err, native.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want errors.Is native.ErrInvalidInput", tc.what, err)
		}
		if got := logger.count(); got != 0 {
			t.Fatalf("%s: Log 调用次数 = %d, want 0(raw 前失败)", tc.what, got)
		}
	}
}

// fakeLogCall 记录一次 Runtime.Log 调用:level 与 message 是 *byte 参
// 数解码出的 C 字符串内容。
type fakeLogCall struct {
	level   string
	message string
}

// fakeRuntimeLogger 是 Runtime 全局日志的受控 fake native(对照
// fakeInstanceCaller 先例):逐次记录 Log 调用与解码参数。不加载任
// 何原生库。
type fakeRuntimeLogger struct {
	mu    sync.Mutex
	calls []fakeLogCall
}

func newFakeRuntimeLogger() *fakeRuntimeLogger { return &fakeRuntimeLogger{} }

func (l *fakeRuntimeLogger) log(level *byte, message *byte) {
	l.mu.Lock()
	l.calls = append(l.calls, fakeLogCall{level: cArgString(level), message: cArgString(message)})
	l.mu.Unlock()
}

func (l *fakeRuntimeLogger) count() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.calls)
}

func (l *fakeRuntimeLogger) record(i int) fakeLogCall {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls[i]
}

// TestSyncEntriesShareSingleRawGate 钉住验收条件「所有实例原生入口
// 经过同一 lease 和串行门禁」:Start 占据 raw 串行门禁并在 fake 内
// 挂起时,已准入的 AppendTask 在同一门禁前排队,不得并发进入原生;
// Start 放行后 AppendTask 才进入。负面断言以「B 已准入(在途计数
// 为 2)」为前提,排除调度延迟造成的假阴性。
func TestSyncEntriesShareSingleRawGate(t *testing.T) {
	const handle = uintptr(0xE300)
	inst, rig := newSyncRig(t, handle)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rig.caller.onStart = func(uintptr) {
		once.Do(func() { close(entered) })
		<-release
	}
	errStart := make(chan error, 1)
	go func() { errStart <- inst.Start() }()
	<-entered // Start 已在 fake 内挂起,占据 raw 门禁

	errAppend := make(chan error, 1)
	go func() {
		_, err := inst.AppendTask("Fighting", json.RawMessage(`{}`))
		errAppend <- err
	}()
	waitUntil(t, "AppendTask 已准入并在门禁前排队", func() bool { return inFlightCalls(inst.inst.state) == 2 })
	<-time.After(notWithin)
	if got := rig.caller.count("appendTask"); got != 0 {
		t.Fatalf("同一 raw 门禁被 Start 占据时 AppendTask 已进入 native %d 次", got)
	}

	close(release)
	for _, wait := range []struct {
		what string
		ch   chan error
	}{
		{"Start", errStart},
		{"AppendTask", errAppend},
	} {
		select {
		case err := <-wait.ch:
			if err != nil {
				t.Fatalf("%s = %v, want nil", wait.what, err)
			}
		case <-time.After(hangGuard):
			t.Fatalf("%s 未在防挂死上限内完成", wait.what)
		}
	}
	if got := rig.caller.count("appendTask"); got != 1 {
		t.Errorf("放行后 AppendTask 调用次数 = %d, want 1", got)
	}
	if got := rig.caller.count("start"); got != 1 {
		t.Errorf("放行后 Start 调用次数 = %d, want 1", got)
	}
}

// TestCloseWaitsQueuedSyncEntries 钉住验收条件「closing/faulted 后不
// 会绕过状态调用」的关闭侧契约(§8 第 2 步、§9.1 验收向量):已准入
// 但尚在 raw 门禁前排队的同步调用被关闭等待覆盖——执行中的 Start 与
// 排队中的 AppendTask 都在途时,Stop/Destroy 不得发生;两者退出后关
// 闭才前进到一次 Stop、一次 Destroy;先于关闭准入的排队调用照常完
// 成,新调用被拒绝(见 TestSyncEntriesRejectByLifecycle)。
func TestCloseWaitsQueuedSyncEntries(t *testing.T) {
	const handle = uintptr(0xE400)
	inst, rig := newSyncRig(t, handle)

	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	rig.caller.onStart = func(uintptr) {
		once.Do(func() { close(entered) })
		<-release
	}
	errStart := make(chan error, 1)
	go func() { errStart <- inst.Start() }()
	<-entered

	errAppend := make(chan error, 1)
	go func() {
		_, err := inst.AppendTask("Fighting", json.RawMessage(`{}`))
		errAppend <- err
	}()
	waitUntil(t, "排队调用已准入", func() bool { return inFlightCalls(inst.inst.state) == 2 })

	closeErr := make(chan error, 1)
	go func() { closeErr <- inst.Close() }()
	waitUntil(t, "close 完成选主转入 closing", func() bool { return inst.inst.state.currentPhase() == phaseClosing })
	<-time.After(notWithin)
	if got := rig.closer.count("stop"); got != 0 {
		t.Fatalf("在途同步调用未退出时 Stop 已被调用 %d 次", got)
	}
	if got := rig.closer.count("destroy"); got != 0 {
		t.Fatalf("在途同步调用未退出时 Destroy 已被调用 %d 次", got)
	}

	// 放行 Start:排队中的 AppendTask 转入门禁段照常完成(先于关闭
	// 准入的调用不被撤销),随后关闭前进。
	close(release)
	select {
	case err := <-errStart:
		if err != nil {
			t.Fatalf("Start = %v, want nil", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("Start 未在防挂死上限内完成")
	}
	select {
	case err := <-errAppend:
		if err != nil {
			t.Fatalf("排队中的 AppendTask = %v, want nil(已准入调用不被关闭撤销)", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("AppendTask 未在防挂死上限内完成")
	}
	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("Close 未在防挂死上限内完成")
	}
	if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
		t.Fatalf("native 调用序列 = %v, want [stop destroy]", seq)
	}
	if got := rig.closer.destroyHandles[0]; got != handle {
		t.Errorf("Destroy 收到 handle %#x, want %#x", got, handle)
	}
}
