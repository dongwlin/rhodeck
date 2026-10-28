// 本文件是任务与同步实例方法的受控用例:验证 AppendTask 原样透传、
// TaskID 0 语义、SetTaskParams/Start/Stop/BackToHome 的 bool 契约、
// Running/Connected 查询,以及输入与生命周期拒绝路径。

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

// TestAppendTaskPassesUnknownTypeAndJSONVerbatim 验证「
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
		t.Fatalf("AppendTask call count = %d, want 1", got)
	}
	rec := rig.caller.appendRecord(0)
	if rec.handle != handle {
		t.Errorf("AppendTask received handle %#x, want %#x (the unique gated handle of the instance)", rec.handle, handle)
	}
	if rec.taskType != taskType {
		t.Errorf("AppendTask taskType = %q, want %q (unknown task type passed through as-is)", rec.taskType, taskType)
	}
	if rec.params != string(raw) {
		t.Errorf("AppendTask params byte-for-byte passthrough broken:\n got %q\nwant %q", rec.params, string(raw))
	}
	for _, want := range []string{"unknown_field", "9007199254740993", `a\u0000b`} {
		if !strings.Contains(rec.params, want) {
			t.Errorf("AppendTask params should preserve %q:\n%s", want, rec.params)
		}
	}

	// 原生返回 0:TaskID 0 明确表示提交失败,错误包装 ErrNativeRejected。
	rig.caller.appendRet = 0
	id, err = inst.AppendTask(taskType, json.RawMessage(`{}`))
	if id != 0 {
		t.Errorf("id on submission failure = %d, want 0", id)
	}
	if !errors.Is(err, native.ErrNativeRejected) {
		t.Errorf("submission failure err = %v, want errors.Is native.ErrNativeRejected", err)
	}
	if !strings.Contains(err.Error(), "AsstAppendTask") {
		t.Errorf("the error should name the interface:\n%s", err.Error())
	}
	if got := rig.caller.count("appendTask"); got != 2 {
		t.Errorf("AppendTask call count = %d, want 2", got)
	}
}

// TestAcceptedRequestsAreNotTaskCompletion 验证「SetTaskParams
// Start 等接受请求不会被包装成任务已完成」:C bool=true 的请求类
// 入口返回恰为 nil(nil 只表示请求被接受,执行语义归事件流,见各方
// 法注释);C bool=false 包装 ErrNativeRejected。SetTaskParams 的 id
// 保留 int32 符号位原样透传。
func TestAcceptedRequestsAreNotTaskCompletion(t *testing.T) {
	const handle = uintptr(0xE200)
	inst, rig := newSyncRig(t, handle)
	params := json.RawMessage(`{"stage":"1-7"}`)

	t.Run("SetTaskParams accepts as exactly nil, rejects with ErrNativeRejected", func(t *testing.T) {
		if err := inst.SetTaskParams(7, params); err != nil {
			t.Fatalf("SetTaskParams = %v, want exactly nil (accepted != completed)", err)
		}
		rec := rig.caller.setParamsRecord(0)
		if rec.handle != handle || rec.id != 7 || rec.params != string(params) {
			t.Errorf("SetTaskParams call record = %+v, want handle %#x, id 7, params passed through byte-for-byte", rec, handle)
		}
		if err := inst.SetTaskParams(-5, params); err != nil {
			t.Fatalf("SetTaskParams(-5) = %v, want exactly nil", err)
		}
		if got := rig.caller.setParamsRecord(1).id; got != -5 {
			t.Errorf("SetTaskParams id = %d, want -5 (int32 sign bit preserved)", got)
		}
		rig.caller.setParamsRet = 0
		if err := inst.SetTaskParams(7, params); !errors.Is(err, native.ErrNativeRejected) {
			t.Errorf("rejected SetTaskParams = %v, want errors.Is native.ErrNativeRejected", err)
		}
	})
	t.Run("Start/Stop/BackToHome accept as exactly nil, reject with ErrNativeRejected", func(t *testing.T) {
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
				t.Fatalf("%s = %v, want exactly nil (accepted != task completed)", e.name, err)
			}
			if got := e.calls(); got != 1 {
				t.Errorf("%s call count = %d, want 1", e.name, got)
			}
			if rec := rig.caller.boolRecord(i); rec.handle != handle {
				t.Errorf("%s received handle %#x, want %#x", e.name, rec.handle, handle)
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
				t.Errorf("rejected %s = %v, want errors.Is native.ErrNativeRejected", name, err)
			}
		}
		delete(rig.caller.boolRets, "start")
		delete(rig.caller.boolRets, "stop")
		delete(rig.caller.boolRets, "backToHome")
	})
}

// TestRunningConnectedFalseIsNotAnError 验证前半「Running/
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

// TestSyncEntriesRejectByLifecycle 验证后半「生命周期失败仍
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

	t.Run("closed phase rejects all entries with no native side effects", func(t *testing.T) {
		inst, rig := newSyncRig(t, 0xE220)
		if err := inst.Close(); err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
		for i, err := range allEntries(inst) {
			if !errors.Is(err, native.ErrClosed) {
				t.Errorf("entry %d = %v, want errors.Is native.ErrClosed", i, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("business call count after closed = %d, want 0 (does not bypass the state calls)", got)
		}
	})
	t.Run("faulted phase rejects all entries with the first cause reachable along the chain", func(t *testing.T) {
		inst, rig := newSyncRig(t, 0xE221)
		cause := fmt.Errorf("fake: first fault: %w", native.ErrProtocol)
		inst.inst.state.recordFault(cause)
		for i, err := range allEntries(inst) {
			if !errors.Is(err, cause) || !errors.Is(err, native.ErrProtocol) {
				t.Errorf("entry %d = %v, want the first cause and native.ErrProtocol reachable along the error chain", i, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("business call count after faulted = %d, want 0 (does not bypass the state calls)", got)
		}
		// faulted 后关闭仍可完成(异常路径进入 faulted 后关闭)。
		if err := inst.Close(); !errors.Is(err, cause) {
			t.Errorf("Close after faulted = %v, want the first cause", err)
		}
	})
	t.Run("closing phase rejects all entries", func(t *testing.T) {
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
				t.Errorf("entry %d during closing = %v, want errors.Is native.ErrClosing", i, err)
			}
		}
		if got := rig.caller.total(); got != 0 {
			t.Errorf("business call count during closing = %d, want 0 (does not bypass the state calls)", got)
		}
		close(release)
		select {
		case err := <-closeErr:
			if err != nil {
				t.Fatalf("Close = %v, want nil", err)
			}
		case <-time.After(hangGuard):
			t.Fatal("Close did not complete within the hang-guard deadline")
		}
	})
}

// TestInvalidInputFailsBeforeNative 验证「内嵌 NUL、非法
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
		{"blank", json.RawMessage("   ")},
		{"null", json.RawMessage(`null`)},
		{"array", json.RawMessage(`[1,2]`)},
		{"scalar", json.RawMessage(`42`)},
		{"truncated", json.RawMessage(`{"a":`)},
		{"trailing second value", json.RawMessage(`{"a":1} {"b":2}`)},
		{"actual NUL", json.RawMessage("{\"a\":\"\x00\"}")},
		{"invalid UTF-8", json.RawMessage("{\"a\":\"\xff\xfe\"}")},
	}
	t.Run("AppendTask", func(t *testing.T) {
		badTypes := []struct {
			what string
			typ  string
		}{
			{"empty taskType", ""},
			{"embedded NUL", "a\x00b"},
			{"invalid UTF-8", string([]byte{0xff, 0xfe})},
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
		t.Errorf("business call count with invalid input = %d, want 0 (fails before raw)", got)
	}
}

// TestLogReturnsOnlyInputOrLifecycleErrors 验证「Log 等 void
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
		t.Fatalf("Log = %v, want nil (no input/lifecycle error means no error)", err)
	}
	rec := logger.record(0)
	if rec.level != "info" || rec.message != "hello binding" {
		t.Errorf("Log record = %+v, want level/message passed through byte-for-byte", rec)
	}
	// 空串允许,由 Core 解释。
	if err := rt.Log("", ""); err != nil {
		t.Errorf("Log(empty string) = %v, want nil", err)
	}
	if rec := logger.record(1); rec.level != "" || rec.message != "" {
		t.Errorf("Log empty-string record = %+v, want empty-string passthrough", rec)
	}

	// 生命周期终止后:错误包装 ErrRuntimeTerminated,native 不再触达。
	cause := fmt.Errorf("fake: terminated cause: %w", native.ErrProtocol)
	gate.terminate(cause)
	err := rt.Log("info", "after termination")
	if !errors.Is(err, native.ErrRuntimeTerminated) {
		t.Errorf("Log after termination = %v, want errors.Is native.ErrRuntimeTerminated", err)
	}
	if !strings.Contains(err.Error(), "terminated cause") {
		t.Errorf("termination diagnostics should attach the first failure cause text:\n%s", err.Error())
	}
	if got := logger.count(); got != 2 {
		t.Errorf("Log call count after termination = %d, want still 2 (fails before the lifecycle)", got)
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
		{"embedded NUL in level", "in\x00fo", "x"},
		{"invalid UTF-8 in message", "info", string([]byte{0xff, 0xfe})},
		{"message exceeds the single-string budget", "info", strings.Repeat("a", 17)},
		{"total exceeds ConfigBytes", "ab", strings.Repeat("b", 19)},
	}
	for _, tc := range cases {
		if err := rt.Log(tc.level, tc.message); !errors.Is(err, native.ErrInvalidInput) {
			t.Errorf("%s: err = %v, want errors.Is native.ErrInvalidInput", tc.what, err)
		}
		if got := logger.count(); got != 0 {
			t.Fatalf("%s: Log call count = %d, want 0 (fails before raw)", tc.what, got)
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

// TestSyncEntriesShareSingleRawGate 验证「所有实例原生入口
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
	waitUntil(t, "AppendTask admitted and queued before the gate", func() bool { return inFlightCalls(inst.inst.state) == 2 })
	<-time.After(notWithin)
	if got := rig.caller.count("appendTask"); got != 0 {
		t.Fatalf("AppendTask entered native %d times while the same raw gate is held by Start", got)
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
			t.Fatalf("%s did not complete within the hang-guard deadline", wait.what)
		}
	}
	if got := rig.caller.count("appendTask"); got != 1 {
		t.Errorf("AppendTask call count after release = %d, want 1", got)
	}
	if got := rig.caller.count("start"); got != 1 {
		t.Errorf("Start call count after release = %d, want 1", got)
	}
}

// TestCloseWaitsQueuedSyncEntries 验证「closing/faulted 后不
// 会绕过状态调用」的关闭侧契约(第 2 步、 验收向量):已准入
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
	waitUntil(t, "queued call admitted", func() bool { return inFlightCalls(inst.inst.state) == 2 })

	closeErr := make(chan error, 1)
	go func() { closeErr <- inst.Close() }()
	waitUntil(t, "close won the election and transitioned to closing", func() bool { return inst.inst.state.currentPhase() == phaseClosing })
	<-time.After(notWithin)
	if got := rig.closer.count("stop"); got != 0 {
		t.Fatalf("Stop called %d times while in-flight sync calls have not exited", got)
	}
	if got := rig.closer.count("destroy"); got != 0 {
		t.Fatalf("Destroy called %d times while in-flight sync calls have not exited", got)
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
		t.Fatal("Start did not complete within the hang-guard deadline")
	}
	select {
	case err := <-errAppend:
		if err != nil {
			t.Fatalf("queued AppendTask = %v, want nil (admitted calls are not revoked by close)", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("AppendTask did not complete within the hang-guard deadline")
	}
	select {
	case err := <-closeErr:
		if err != nil {
			t.Fatalf("Close = %v, want nil", err)
		}
	case <-time.After(hangGuard):
		t.Fatal("Close did not complete within the hang-guard deadline")
	}
	if seq := rig.closer.calls(); len(seq) != 2 || seq[0] != "stop" || seq[1] != "destroy" {
		t.Fatalf("native call sequence = %v, want [stop destroy]", seq)
	}
	if got := rig.closer.destroyHandles[0]; got != handle {
		t.Errorf("Destroy received handle %#x, want %#x", got, handle)
	}
}
