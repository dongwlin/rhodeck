// 本文件是 Runtime 初始化事务的用例:顺序、并发预占与逐步失败
// 注入。经私有的 open(cfg, env, gate) 注入假环境与假会话构造全部
// 失败路径,不进入系统 loader、不加载 MaaCore、不替换生产包的全局
// 函数变量;公开 Open 的接线用无效配置冒烟(校验失败发生在预占前,
// 不会消耗进程级生命周期门)。

package maa

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/dongwlin/rhodeck/packages/maa-go/internal/native"
)

// errStepSentinel 是 loadLibrary 阶段的注入失败:open 必须原样返回。
var errStepSentinel = errors.New("fake env: library load rejected")

// fakeCall 记录一次会话步骤调用:value/extra 是 *byte 参数解码出的
// C 字符串内容;key 只用于 setStaticOption。
type fakeCall struct {
	step  string // version/nullSize/setUserDir/setStaticOption/loadResource/setConnectionExtras
	key   int32
	value string
	extra string
}

// fakeSession 是可记录调用序列、可注入逐步失败的假会话。全部状态
// 经 mu 保护,并发安全。
type fakeSession struct {
	mu    sync.Mutex
	calls []fakeCall

	// version 注入项。
	versionNull    bool   // version() 返回空指针
	versionNoNUL   bool   // version() 返回界内无 NUL 的 buffer
	versionInvalid bool   // version() 返回非法 UTF-8
	versionEmpty   bool   // version() 返回空串
	versionText    string // 正常 Version 内容;空串时默认 "6.20.1"

	// nullSize 注入项:newValidFakeSession 预置 profileNullSize,
	// 注入其他值构造协议错误。
	nullSizeValue uint64

	// 逐步失败注入:userDirRejected 使 setUserDir 返回 0;
	// staticOptionRejectN/resourceRejectN 为 1 基的第 N 次调用拒绝
	// 序号,0 表示从不拒绝。
	userDirRejected     bool
	staticOptionRejectN int
	staticOptionCalls   int
	resourceRejectN     int
	resourceCalls       int

	caps native.Capabilities
}

// newValidFakeSession 构造全步骤成功的假会话。
func newValidFakeSession() *fakeSession {
	return &fakeSession{nullSizeValue: profileNullSize}
}

func (f *fakeSession) record(call fakeCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

// recorded 返回调用序列的副本,避免调用方与后续调用共享底层数组。
func (f *fakeSession) recorded() []fakeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fakeCall(nil), f.calls...)
}

func (f *fakeSession) capabilities() native.Capabilities {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.caps
}

// table 返回 nil:假会话不持有 raw 表,本用例的用例不触达 raw 调用。
func (f *fakeSession) table() *native.APITable { return nil }

func (f *fakeSession) version() *byte {
	f.record(fakeCall{step: "version"})
	if f.versionNull {
		return nil
	}
	// 假 buffer 始终分配满扫描上限(versionBytesLimit+1 字节):真实
	// Core 的版本指针指向.rodata,字符串之后仍有可读数据,冻结契约
	// 允许 readVersion 的扫描跨出字符串本身;Go 堆上按实际文本长度
	// 分配的短 buffer 无法诚实地模拟该契约,且会在 -race(checkptr)
	// 下被误判为跨分配。零值填充即合法的 NUL 终止。
	buf := make([]byte, versionBytesLimit+1)
	switch {
	case f.versionNoNUL:
		for i := range buf {
			buf[i] = 'v' // 扫描范围内全程无 NUL
		}
	case f.versionInvalid:
		buf[0], buf[1], buf[2] = 'v', 0xff, 0xfe
	case f.versionEmpty:
		// buf[0] 保持 0:空串。
	default:
		text := f.versionText
		if text == "" {
			text = "6.20.1"
		}
		copy(buf, text) // copy 之后 buf[len(text)] 为 NUL
	}
	return &buf[0]
}

func (f *fakeSession) nullSize() uint64 {
	f.record(fakeCall{step: "nullSize"})
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.nullSizeValue
}

func (f *fakeSession) setUserDir(path *byte) uint8 {
	f.record(fakeCall{step: "setUserDir", value: cArgString(path)})
	if f.userDirRejected {
		return 0
	}
	return 1
}

func (f *fakeSession) setStaticOption(key int32, value *byte) uint8 {
	f.record(fakeCall{step: "setStaticOption", key: key, value: cArgString(value)})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.staticOptionCalls++
	if f.staticOptionRejectN > 0 && f.staticOptionCalls == f.staticOptionRejectN {
		return 0
	}
	return 1
}

func (f *fakeSession) loadResource(path *byte) uint8 {
	f.record(fakeCall{step: "loadResource", value: cArgString(path)})
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resourceCalls++
	if f.resourceRejectN > 0 && f.resourceCalls == f.resourceRejectN {
		return 0
	}
	return 1
}

func (f *fakeSession) setConnectionExtras(name *byte, extras *byte) {
	f.record(fakeCall{step: "setConnectionExtras", value: cArgString(name), extra: cArgString(extras)})
}

// fakeEnv 是可计数、可注入失败的假环境:记录 loadLibrary 收到的
// 路径,按需返回错误或挂接的假会话。并发安全。
type fakeEnv struct {
	mu        sync.Mutex
	loadPaths []string
	session   *fakeSession
	err       error
}

var _ initEnv = (*fakeEnv)(nil)

func (e *fakeEnv) loadLibrary(path string) (initSession, error) {
	e.mu.Lock()
	e.loadPaths = append(e.loadPaths, path)
	e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	return e.session, nil
}

func (e *fakeEnv) loadCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.loadPaths)
}

// cArgString 解码 NUL 终止的 C 字符串参数,供断言比对传参内容。
// 空指针记为 "<nil>"。
func cArgString(p *byte) string {
	if p == nil {
		return "<nil>"
	}
	var out []byte
	for i := 0; ; i++ {
		b := *(*byte)(unsafe.Add(unsafe.Pointer(p), i))
		if b == 0 {
			return string(out)
		}
		out = append(out, b)
	}
}

// TestOpenValidationFailureDoesNotConsumeLifecycle 验证:纯 Go 校验
// 失败不触碰生命周期门——fake env 的 loadLibrary 零调用,校验失败
// 后同一 gate 仍可成功 Open。
func TestOpenValidationFailureDoesNotConsumeLifecycle(t *testing.T) {
	env := &fakeEnv{session: newValidFakeSession()}
	gate := &lifecycleGate{}

	cfg := validRuntimeConfig()
	cfg.LibraryPath = "relative/libMaaCore.so"
	rt, err := open(cfg, env, gate)
	if rt != nil {
		t.Fatal("校验失败不得返回 Runtime")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "LibraryPath") {
		t.Errorf("错误文本 %q 未包含字段路径", err.Error())
	}
	if n := env.loadCount(); n != 0 {
		t.Errorf("loadLibrary 被调用 %d 次, want 0(校验失败不得进入原生加载)", n)
	}

	rt, err = open(validRuntimeConfig(), env, gate)
	if err != nil || rt == nil {
		t.Fatalf("校验失败后同一 gate 必须仍可成功 Open: rt=%v err=%v", rt, err)
	}
	if gate.state != lifecycleReady || gate.active != rt {
		t.Errorf("gate 状态 = %d active=%v, want ready 且记录该 Runtime", gate.state, gate.active)
	}
}

// TestPublicOpenRejectsInvalidConfig 经公开 Open 冒烟验证接线:
// 无效配置返回 ErrInvalidInput(校验在预占前完成,进程级门不受影响)。
func TestPublicOpenRejectsInvalidConfig(t *testing.T) {
	cfg := validRuntimeConfig()
	cfg.UserDir = "relative/dir"
	rt, err := Open(cfg)
	if rt != nil {
		t.Fatal("公开 Open 对无效配置不得返回 Runtime")
	}
	if !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("errors.Is(err, ErrInvalidInput) = false; err = %v", err)
	}
	if !strings.Contains(err.Error(), "UserDir") {
		t.Errorf("错误文本 %q 未包含字段路径", err.Error())
	}
}

// TestOpenSessionStepsAndMetadata 验证顺序 happy path:调用序列恰为
// version → nullSize → userDir → 静态选项(按序)→ 资源根(按序)→
// extras(按序),各恰一次;发布后元数据与注入值一致,重复读取不
// 增加原生调用。
func TestOpenSessionStepsAndMetadata(t *testing.T) {
	sess := newValidFakeSession()
	env := &fakeEnv{session: sess}
	gate := &lifecycleGate{}

	cfg := validRuntimeConfig()
	cfg.StaticOptions = []StaticOption{{Key: 1, Value: "cpu"}, {Key: 2, Value: ""}}
	cfg.ResourceRoots = []string{testAbsolutePath("resource"), testAbsolutePath("extra")}
	cfg.ConnectionExtras = []ConnectionExtra{
		{Name: "sink", JSON: json.RawMessage(`{"a":1}`)},
		{Name: "other", JSON: json.RawMessage(`{}`)},
	}

	rt, err := open(cfg, env, gate)
	if err != nil {
		t.Fatalf("happy path 不得失败: %v", err)
	}
	if rt == nil {
		t.Fatal("happy path 必须返回 Runtime")
	}

	want := []fakeCall{
		{step: "version"},
		{step: "nullSize"},
		{step: "setUserDir", value: testAbsolutePath("user")},
		{step: "setStaticOption", key: 1, value: "cpu"},
		{step: "setStaticOption", key: 2, value: ""},
		{step: "loadResource", value: testAbsolutePath("resource")},
		{step: "loadResource", value: testAbsolutePath("extra")},
		{step: "setConnectionExtras", value: "sink", extra: `{"a":1}`},
		{step: "setConnectionExtras", value: "other", extra: `{}`},
	}
	got := sess.recorded()
	if len(got) != len(want) {
		t.Fatalf("会话调用数 = %d, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("会话调用 #%d = %+v, want %+v(顺序与内容必须一致)", i, got[i], want[i])
		}
	}

	// 元数据与注入值一致;全部来自初始化时复制的不可变数据。
	if v := rt.Version(); v != "6.20.1" {
		t.Errorf("Version() = %q, want %q", v, "6.20.1")
	}
	if ns := rt.NullSize(); ns != profileNullSize {
		t.Errorf("NullSize() = %#x, want 锁定 profile 哨兵 %#x", ns, uint64(profileNullSize))
	}
	caps := rt.Capabilities()
	if caps.Create() || caps.ImageBgr() || caps.AsyncAttachWindow() {
		t.Errorf("Capabilities() = (Create=%v ImageBgr=%v AsyncAttachWindow=%v), want 假会话注入的零值快照",
			caps.Create(), caps.ImageBgr(), caps.AsyncAttachWindow())
	}

	// 重复读取元数据不得增加原生调用。
	before := len(sess.recorded())
	for i := 0; i < 3; i++ {
		_ = rt.Version()
		_ = rt.NullSize()
		_ = rt.Capabilities()
	}
	if after := len(sess.recorded()); after != before {
		t.Errorf("重复读取元数据使会话调用数从 %d 增加到 %d", before, after)
	}

	// 白盒:发布后的 Runtime 持有限制快照;gate 处于 ready 并记录
	// 该 Runtime。
	if rt.limits != defaultLimits() {
		t.Errorf("Runtime 的限制快照 = %+v, want 默认值", rt.limits)
	}
	if gate.state != lifecycleReady || gate.active != rt {
		t.Errorf("gate 状态 = %d active=%v, want ready 且记录该 Runtime", gate.state, gate.active)
	}
}

// TestOpenAcceptsVersionAtScanWindowBoundary 验证 Version 扫描窗的
// 恰达边界接受侧:Version 恰
// 为 4,096 字节、NUL 落在扫描窗(4,097 字节)末端时被接受,复制结果
// 逐字节正确。fake 假 buffer 恒分配满 4,097 字节(见 fakeSession.version
// 的注释),4,096 字节文本经 versionText 注入后 NUL 恰在窗口末字节;
// 若扫描窗被缩短到 4,096 字节,本用例将因界内无 NUL 而失败。「窗口
// 内无 NUL → ErrProtocol」的拒绝侧见 TestOpenStepFailuresTerminateLifecycle
// 的「version 界内无 NUL」。本用例使用 fake,不验证真实库行为。
func TestOpenAcceptsVersionAtScanWindowBoundary(t *testing.T) {
	sess := newValidFakeSession()
	env := &fakeEnv{session: sess}
	gate := &lifecycleGate{}

	text := strings.Repeat("v", versionBytesLimit)
	sess.versionText = text

	rt, err := open(validRuntimeConfig(), env, gate)
	if err != nil || rt == nil {
		t.Fatalf("扫描窗末端 NUL 的 Version 应当被接受: rt=%v err=%v", rt, err)
	}
	if got := rt.Version(); got != text {
		t.Errorf("Version() 长度 = %d, want %d,且内容必须逐字节一致", len(got), versionBytesLimit)
	}
}

// TestOpenSnapshotIsDeepCopy 验证:Open 返回后修改原配置的一切可变
// 内容,白盒断言 Runtime 私有限制快照与假会话收到的 *byte 内容仍
// 为原始值。
func TestOpenSnapshotIsDeepCopy(t *testing.T) {
	sess := newValidFakeSession()
	env := &fakeEnv{session: sess}
	gate := &lifecycleGate{}

	cfg := validRuntimeConfig()
	cfg.StaticOptions = []StaticOption{{Key: 3, Value: "orig"}}
	cfg.ResourceRoots = []string{testAbsolutePath("resource")}
	cfg.ConnectionExtras = []ConnectionExtra{{Name: "sink", JSON: json.RawMessage(`{"orig":1}`)}}
	explicit := validLimits()
	explicit.InputStringBytes = 12_345
	cfg.Limits = &explicit
	// wantLimits 是显式 Limits 的值复制:cfg.Limits 与 explicit 共享
	// 同一变量,下面对 cfg.Limits 的原地修改会改写 explicit,但不得
	// 改写快照。
	wantLimits := explicit

	rt, err := open(cfg, env, gate)
	if err != nil || rt == nil {
		t.Fatalf("Open 意外失败: rt=%v err=%v", rt, err)
	}

	// Open 返回后修改原配置。
	cfg.UserDir = testAbsolutePath("tampered")
	cfg.StaticOptions[0].Value = "tampered"
	cfg.StaticOptions = append(cfg.StaticOptions, StaticOption{Key: 9, Value: "extra"})
	cfg.ResourceRoots[0] = testAbsolutePath("tampered")
	cfg.ResourceRoots = append(cfg.ResourceRoots, testAbsolutePath("tampered2"))
	cfg.ConnectionExtras[0].Name = "tampered"
	cfg.ConnectionExtras[0].JSON = json.RawMessage(`{"tampered":true}`)
	cfg.ConnectionExtras = append(cfg.ConnectionExtras, ConnectionExtra{Name: "t2", JSON: json.RawMessage(`{}`)})
	cfg.Limits.InputStringBytes = 1

	if rt.limits != wantLimits || rt.limits.InputStringBytes != 12_345 {
		t.Errorf("Runtime 的限制快照被改写: got %+v, want %+v", rt.limits, wantLimits)
	}
	want := []fakeCall{
		{step: "version"},
		{step: "nullSize"},
		{step: "setUserDir", value: testAbsolutePath("user")},
		{step: "setStaticOption", key: 3, value: "orig"},
		{step: "loadResource", value: testAbsolutePath("resource")},
		{step: "setConnectionExtras", value: "sink", extra: `{"orig":1}`},
	}
	got := sess.recorded()
	if len(got) != len(want) {
		t.Fatalf("会话调用数 = %d, want %d:\n%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("会话调用 #%d = %+v, want %+v(快照不得被后续修改影响)", i, got[i], want[i])
		}
	}
}

// TestConcurrentOpenSingleReserve 验证并发预占:N 个 goroutine 对同
// 一 gate 并发 open(配置各异且都有效),恰好 1 个成功进入原生初始
// 化(fake env 原子计数 == 1),其余得到 ErrRuntimeInUse;胜者的配置
// 被使用。gate 进入 ready 后,重复 open 同样返回 ErrRuntimeInUse。
func TestConcurrentOpenSingleReserve(t *testing.T) {
	env := &fakeEnv{session: newValidFakeSession()}
	gate := &lifecycleGate{}

	const n = 8
	cfgs := make([]RuntimeConfig, n)
	for i := range cfgs {
		cfgs[i] = validRuntimeConfig()
		cfgs[i].UserDir = testAbsolutePath(fmt.Sprintf("user-%d", i))
	}
	rts := make([]*Runtime, n)
	errs := make([]error, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rt, err := open(cfgs[i], env, gate)
			rts[i] = rt
			errs[i] = err
		}(i)
	}
	wg.Wait()

	winners := 0
	winIdx := -1
	for i := range errs {
		switch {
		case rts[i] != nil:
			winners++
			winIdx = i
		case errors.Is(errs[i], ErrRuntimeInUse):
			// 其余调用必须得到占用错误。
		default:
			t.Errorf("goroutine %d 的错误 = %v, want ErrRuntimeInUse 或成功", i, errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("成功进入原生初始化的数量 = %d, want 1", winners)
	}
	if got := env.loadCount(); got != 1 {
		t.Errorf("loadLibrary 调用次数 = %d, want 1", got)
	}
	for _, c := range env.session.recorded() {
		if c.step == "setUserDir" && c.value != cfgs[winIdx].UserDir {
			t.Fatalf("胜者的配置未被使用: setUserDir(%q), want %q", c.value, cfgs[winIdx].UserDir)
		}
	}

	// gate 已 ready:重复 open 被拒绝,不复用已有对象。
	rt, err := open(cfgs[0], env, gate)
	if rt != nil {
		t.Fatal("ready 后重复 open 不得返回 Runtime")
	}
	if !errors.Is(err, ErrRuntimeInUse) {
		t.Fatalf("errors.Is(err, ErrRuntimeInUse) = false; err = %v", err)
	}
	if got := env.loadCount(); got != 1 {
		t.Errorf("被拒绝的 open 不得进入原生加载, loadLibrary 累计 = %d, want 1", got)
	}
}

// TestOpenStepFailuresTerminateLifecycle 逐步失败注入:每个初始化
// 步骤一个子测试。断言返回 nil Runtime、原始错误类别正确、gate 随
// 后 terminated,后续 open(有效配置)返回 errors.Is
// ErrRuntimeTerminated 且不再进入原生加载。
func TestOpenStepFailuresTerminateLifecycle(t *testing.T) {
	cases := []struct {
		name   string
		inject func(env *fakeEnv, sess *fakeSession)
		config func(cfg *RuntimeConfig)
		wantIs error
		check  func(t *testing.T, env *fakeEnv, err error)
	}{
		{
			name: "loadLibrary 失败原样返回",
			inject: func(env *fakeEnv, sess *fakeSession) {
				env.err = errStepSentinel
			},
			wantIs: errStepSentinel,
			check: func(t *testing.T, env *fakeEnv, err error) {
				if err != errStepSentinel {
					t.Errorf("loadLibrary 失败必须原样返回 sentinel, got %T: %v", err, err)
				}
			},
		},
		{
			name:   "version 空指针",
			inject: func(env *fakeEnv, sess *fakeSession) { sess.versionNull = true },
			wantIs: ErrProtocol,
		},
		{
			name:   "version 界内无 NUL",
			inject: func(env *fakeEnv, sess *fakeSession) { sess.versionNoNUL = true },
			wantIs: ErrProtocol,
		},
		{
			name:   "version 非法 UTF-8",
			inject: func(env *fakeEnv, sess *fakeSession) { sess.versionInvalid = true },
			wantIs: ErrProtocol,
		},
		{
			name:   "version 空串",
			inject: func(env *fakeEnv, sess *fakeSession) { sess.versionEmpty = true },
			wantIs: ErrProtocol,
		},
		{
			name:   "nullSize 非 profile 哨兵",
			inject: func(env *fakeEnv, sess *fakeSession) { sess.nullSizeValue = 42 },
			wantIs: ErrProtocol,
		},
		{
			name:   "SetUserDir 拒绝",
			inject: func(env *fakeEnv, sess *fakeSession) { sess.userDirRejected = true },
			wantIs: ErrNativeRejected,
		},
		{
			name: "第 2 个静态选项拒绝",
			inject: func(env *fakeEnv, sess *fakeSession) {
				sess.staticOptionRejectN = 2
			},
			config: func(cfg *RuntimeConfig) {
				cfg.StaticOptions = []StaticOption{{Key: 1, Value: "a"}, {Key: 2, Value: "b"}}
			},
			wantIs: ErrNativeRejected,
			check: func(t *testing.T, env *fakeEnv, err error) {
				calls := 0
				for _, c := range env.session.recorded() {
					if c.step == "setStaticOption" {
						calls++
					}
				}
				if calls != 2 {
					t.Errorf("setStaticOption 调用数 = %d, want 2(第 1 个成功、第 2 个拒绝)", calls)
				}
			},
		},
		{
			name: "第 1 个资源根拒绝",
			inject: func(env *fakeEnv, sess *fakeSession) {
				sess.resourceRejectN = 1
			},
			config: func(cfg *RuntimeConfig) {
				cfg.ResourceRoots = []string{testAbsolutePath("resource"), testAbsolutePath("extra")}
			},
			wantIs: ErrNativeRejected,
			check: func(t *testing.T, env *fakeEnv, err error) {
				calls := 0
				for _, c := range env.session.recorded() {
					if c.step == "loadResource" {
						calls++
					}
				}
				if calls != 1 {
					t.Errorf("loadResource 调用数 = %d, want 1(第 1 个拒绝后即中止)", calls)
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sess := newValidFakeSession()
			env := &fakeEnv{session: sess}
			gate := &lifecycleGate{}
			tc.inject(env, sess)

			cfg := validRuntimeConfig()
			if tc.config != nil {
				tc.config(&cfg)
			}

			rt, err := open(cfg, env, gate)
			if rt != nil {
				t.Fatal("失败不得发布 Runtime")
			}
			if err == nil {
				t.Fatal("必须返回错误")
			}
			if !errors.Is(err, tc.wantIs) {
				t.Fatalf("errors.Is(err, %v) = false; err = %v", tc.wantIs, err)
			}
			// 错误类别不得与 terminated 混淆:原始错误直接返回,
			// terminated 只影响后续 Open。
			if errors.Is(err, ErrRuntimeTerminated) && tc.wantIs != errStepSentinel {
				t.Errorf("原始失败不得包成 ErrRuntimeTerminated: %v", err)
			}
			if tc.check != nil {
				tc.check(t, env, err)
			}

			// gate 已 terminated:后续 open(无论配置如何有效)返回
			// ErrRuntimeTerminated,且不再进入原生加载。
			rt2, err2 := open(validRuntimeConfig(), env, gate)
			if rt2 != nil {
				t.Fatal("terminated 后不得再发布 Runtime")
			}
			if !errors.Is(err2, ErrRuntimeTerminated) {
				t.Fatalf("后续 open 的错误 = %v, want errors.Is ErrRuntimeTerminated", err2)
			}
			if env.loadCount() != 1 {
				t.Errorf("loadLibrary 累计调用 = %d, want 1(terminated 后不得重试)", env.loadCount())
			}
			// terminated 错误附首个失败原因作诊断。
			if !strings.Contains(err2.Error(), "first failure") {
				t.Errorf("terminated 错误文本 %q 未附首个失败原因诊断", err2.Error())
			}
		})
	}
}
