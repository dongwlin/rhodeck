// 本文件是注册事务的故障注入与发布语义用例:经 loaderPrimitives 与
// symbolRegistrar 两个注入边界构造缺符号、空地址、可选缺失、平台
// 排除与注册 panic 等路径,不进入系统 loader,不替换生产包的全局
// 函数变量。适用符号期望表手写内联、与 symbolSpecs 相互独立,避免
// 用被检对象自身生成期望。
//
// 平台说明:期望表按 runtime.GOOS 取用。首期支持且实际运行测试的
// 平台是 Linux amd64,对应设计 §3.1 的 25 个适用项——26 条冻结声明
// 去掉 windowsOnly 的 AsstAsyncAttachWindow。

package native

import (
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/ebitengine/purego"
)

// fakeRegisterCall 记录一次注册调用:target 是传入注册器的字段指针,
// addr 是随之绑定的已解析地址。
type fakeRegisterCall struct {
	target any
	addr   uintptr
}

// fakeRegistrar 是可记录调用序列、可注入第 N 次 panic 的假注册器;
// panicOnNth 为 1 基序号,0 表示从不 panic。记录与 panic 注入之后
// 委托生产注册原语完成绑定:事务用例对「已注册字段非 nil」的断言
// 必须反映真实的发布状态,纯记录的假注册器无法产生该状态。panic
// 在绑定之前触发,中止项保持未绑定。
type fakeRegistrar struct {
	calls      []fakeRegisterCall // 按顺序记录 target/addr
	panicOnNth int                // 1 基;0 表示不 panic
	panicValue any
}

func (f *fakeRegistrar) registerFunc(target any, addr uintptr) {
	f.calls = append(f.calls, fakeRegisterCall{target: target, addr: addr})
	if f.panicOnNth > 0 && len(f.calls) == f.panicOnNth {
		panic(f.panicValue)
	}
	purego.RegisterFunc(target, addr)
}

// expectedApplicableSymbols 是手写内联的适用符号期望表,与
// symbolSpecs 相互独立:非 Windows 平台为 25 项,Windows 为全部
// 26 项。AsstConnect 与 AsstAttachWindow 是排除项,任何平台都不在
// 表内。
var expectedApplicableSymbols = map[string][]string{
	"linux": {
		"AsstSetUserDir", "AsstLoadResource", "AsstSetStaticOption", "AsstCreate",
		"AsstCreateEx", "AsstDestroy", "AsstSetInstanceOption", "AsstAppendTask",
		"AsstSetTaskParams", "AsstStart", "AsstStop", "AsstRunning",
		"AsstConnected", "AsstBackToHome", "AsstAsyncConnect", "AsstSetConnectionExtras",
		"AsstAsyncClick", "AsstAsyncScreencap", "AsstGetImage", "AsstGetImageBgr",
		"AsstGetUUID", "AsstGetTasksList", "AsstGetNullSize", "AsstGetVersion",
		"AsstLog",
	},
	"darwin": {
		"AsstSetUserDir", "AsstLoadResource", "AsstSetStaticOption", "AsstCreate",
		"AsstCreateEx", "AsstDestroy", "AsstSetInstanceOption", "AsstAppendTask",
		"AsstSetTaskParams", "AsstStart", "AsstStop", "AsstRunning",
		"AsstConnected", "AsstBackToHome", "AsstAsyncConnect", "AsstSetConnectionExtras",
		"AsstAsyncClick", "AsstAsyncScreencap", "AsstGetImage", "AsstGetImageBgr",
		"AsstGetUUID", "AsstGetTasksList", "AsstGetNullSize", "AsstGetVersion",
		"AsstLog",
	},
	"windows": {
		"AsstSetUserDir", "AsstLoadResource", "AsstSetStaticOption", "AsstCreate",
		"AsstCreateEx", "AsstDestroy", "AsstSetInstanceOption", "AsstAppendTask",
		"AsstSetTaskParams", "AsstStart", "AsstStop", "AsstRunning",
		"AsstConnected", "AsstBackToHome", "AsstAsyncConnect", "AsstSetConnectionExtras",
		"AsstAsyncClick", "AsstAsyncScreencap", "AsstGetImage", "AsstGetImageBgr",
		"AsstGetUUID", "AsstGetTasksList", "AsstGetNullSize", "AsstGetVersion",
		"AsstLog", "AsstAsyncAttachWindow",
	},
}

// expectApplicableNames 取当前平台的适用符号期望表;无期望表的平台
// 直接失败,不静默放行。
func expectApplicableNames(t *testing.T) []string {
	t.Helper()
	names, ok := expectedApplicableSymbols[runtime.GOOS]
	if !ok {
		t.Fatalf("平台 %s 缺少独立的适用符号期望表", runtime.GOOS)
	}
	return names
}

// mustOpenLibrary 以给定的假原语打开假库:openHandle 与 lookupAddr
// 由调用方保证非零,各用例在此基础上按需注入失败。
func mustOpenLibrary(t *testing.T, fake *fakePrimitives) *Library {
	t.Helper()
	const path = "/opt/maa/libMaaCore.so"
	lib, err := newFakeLoader(fake).Open(path)
	if err != nil {
		t.Fatalf("Open(%q) 返回意外错误: %v", path, err)
	}
	return lib
}

// apiNilFields 经 reflect 返回 api 表中为 nil 的字段名集合:raw
// 字段不向外导出,用例只经反射检查 nil/非 nil。
func apiNilFields(t *testing.T, table *api) map[string]bool {
	t.Helper()
	v := reflect.ValueOf(table).Elem()
	nilFields := make(map[string]bool)
	typ := v.Type()
	for i := 0; i < typ.NumField(); i++ {
		if v.Field(i).IsNil() {
			nilFields[typ.Field(i).Name] = true
		}
	}
	return nilFields
}

// TestResolveReportsAllMissingRequiredSymbols 验证:同时缺少两个
// 必需符号时,聚合错误恰好两项、可分别识别到各自的 *SymbolLookupError
// 与注入的底层原因,注册器零调用,取不到 API 表。
func TestResolveReportsAllMissingRequiredSymbols(t *testing.T) {
	sentinelCreateEx := errors.New("fake dlsym: AsstCreateEx missing")
	sentinelVersion := errors.New("fake dlsym: AsstGetVersion missing")
	fake := &fakePrimitives{
		openHandle: 0x1000,
		lookupAddr: 0x00c0ffee,
		lookupFailures: map[string]error{
			"AsstCreateEx":   sentinelCreateEx,
			"AsstGetVersion": sentinelVersion,
		},
	}
	lib := mustOpenLibrary(t, fake)

	reg := &fakeRegistrar{}
	table, err := resolveAndRegister(lib, reg)
	if table != nil {
		t.Fatalf("必需符号缺失时不得发布 API 表, got %+v", table)
	}
	if err == nil {
		t.Fatal("必需符号缺失时必须返回错误")
	}
	var agg AggregateError
	if !errors.As(err, &agg) {
		t.Fatalf("errors.As 未得到 AggregateError, got %T: %v", err, err)
	}
	if len(agg) != 2 {
		t.Fatalf("聚合错误项数 = %d, want 2:\n%v", len(agg), err)
	}
	want := map[string]error{
		"AsstCreateEx":   sentinelCreateEx,
		"AsstGetVersion": sentinelVersion,
	}
	for _, item := range agg {
		var sle *SymbolLookupError
		if !errors.As(item, &sle) {
			t.Fatalf("聚合项 %T 不是 *SymbolLookupError: %v", item, item)
		}
		wantErr, ok := want[sle.Symbol]
		if !ok {
			t.Errorf("聚合项符号 %q 不在期望的缺失集合内", sle.Symbol)
			continue
		}
		if !errors.Is(item, wantErr) {
			t.Errorf("符号 %q 的错误链未到达注入的底层 sentinel", sle.Symbol)
		}
		delete(want, sle.Symbol)
	}
	if len(want) != 0 {
		keys := make([]string, 0, len(want))
		for symbol := range want {
			keys = append(keys, symbol)
		}
		t.Errorf("聚合错误缺少符号: %v", keys)
	}
	if n := len(reg.calls); n != 0 {
		t.Errorf("存在未满足必需项时注册器不得被调用, got %d 次", n)
	}
}

// TestResolveTreatsNullAddressAsLookupFailure 验证:必需符号解析出
// 零地址与查找失败同样阻止注册;两类原因在同一聚合中恰好各一项,
// 保持可分别识别,注册器零调用,不发布 API 表。
func TestResolveTreatsNullAddressAsLookupFailure(t *testing.T) {
	sentinel := errors.New("fake dlsym: AsstGetVersion missing")
	fake := &fakePrimitives{
		openHandle:      0x1000,
		lookupAddr:      0x00c0ffee,
		lookupNullAddrs: map[string]bool{"AsstCreateEx": true},
		lookupFailures:  map[string]error{"AsstGetVersion": sentinel},
	}
	lib := mustOpenLibrary(t, fake)

	reg := &fakeRegistrar{}
	table, err := resolveAndRegister(lib, reg)
	if table != nil {
		t.Fatalf("必需符号不可用时不得发布 API 表, got %+v", table)
	}
	if err == nil {
		t.Fatal("必需符号不可用时必须返回错误")
	}
	var agg AggregateError
	if !errors.As(err, &agg) {
		t.Fatalf("errors.As 未得到 AggregateError, got %T: %v", err, err)
	}
	if len(agg) != 2 {
		t.Fatalf("聚合错误项数 = %d, want 2:\n%v", len(agg), err)
	}

	bySymbol := make(map[string]error, len(agg))
	for _, item := range agg {
		var sle *SymbolLookupError
		if !errors.As(item, &sle) {
			t.Fatalf("聚合项 %T 不是 *SymbolLookupError: %v", item, item)
		}
		bySymbol[sle.Symbol] = item
	}
	nullItem, ok := bySymbol["AsstCreateEx"]
	if !ok {
		t.Fatalf("聚合缺少零地址项 AsstCreateEx:\n%v", err)
	}
	if !errors.Is(nullItem, errSymbolNullAddress) {
		t.Errorf("AsstCreateEx 的错误链未识别空地址原因")
	}
	versionItem, ok := bySymbol["AsstGetVersion"]
	if !ok {
		t.Fatalf("聚合缺少查找失败项 AsstGetVersion:\n%v", err)
	}
	if !errors.Is(versionItem, sentinel) {
		t.Errorf("AsstGetVersion 的错误链未到达注入的 sentinel")
	}
	if errors.Is(nullItem, sentinel) || errors.Is(versionItem, errSymbolNullAddress) {
		t.Errorf("零地址与查找失败两类原因不得相互混淆")
	}
	if n := len(reg.calls); n != 0 {
		t.Errorf("存在未满足必需项时注册器不得被调用, got %d 次", n)
	}
}

// TestOptionalMissingClosesCapabilityOnly 验证:可选符号缺失(查找
// 失败或零地址两种情形)不阻塞加载,只关闭对应 capability;其余
// 适用符号照常注册,缺失项字段保持 nil。
func TestOptionalMissingClosesCapabilityOnly(t *testing.T) {
	// 两个缺失的可选项(AsstCreate、AsstGetImageBgr)为全平台条目,
	// 注册数 = 本平台适用项数 − 2(Linux 上即 25 − 2 = 23)。
	const missingO = 2
	cases := []struct {
		name  string
		fails map[string]error
		nulls map[string]bool
	}{
		{
			"查找失败",
			map[string]error{
				"AsstCreate":      errors.New("fake dlsym: AsstCreate missing"),
				"AsstGetImageBgr": errors.New("fake dlsym: AsstGetImageBgr missing"),
			},
			nil,
		},
		{"零地址", nil, map[string]bool{"AsstCreate": true, "AsstGetImageBgr": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePrimitives{
				openHandle:      0x1000,
				lookupAddr:      0x00c0ffee,
				lookupFailures:  tc.fails,
				lookupNullAddrs: tc.nulls,
			}
			lib := mustOpenLibrary(t, fake)

			reg := &fakeRegistrar{}
			table, err := resolveAndRegister(lib, reg)
			if err != nil {
				t.Fatalf("可选缺失不得使事务失败: %v", err)
			}
			if table == nil {
				t.Fatal("可选缺失时仍必须发布 API 表")
			}

			caps := table.Capabilities()
			if caps.Create() || caps.ImageBgr() || caps.AsyncAttachWindow() {
				t.Errorf("缺失的可选能力必须全部关闭, got Create=%v ImageBgr=%v AsyncAttachWindow=%v",
					caps.Create(), caps.ImageBgr(), caps.AsyncAttachWindow())
			}
			wantCalls := len(expectApplicableNames(t)) - missingO
			if n := len(reg.calls); n != wantCalls {
				t.Errorf("注册器调用次数 = %d, want %d(适用 %d 项减 %d 个缺失可选项)",
					n, wantCalls, len(expectApplicableNames(t)), missingO)
			}

			// 缺失的两个可选项字段为 nil;非 Windows 平台上不适用
			// 的 AsyncAttachWindow 同样为 nil,其余适用字段全部非 nil。
			expectNil := map[string]bool{"Create": true, "GetImageBgr": true}
			if runtime.GOOS != "windows" {
				expectNil["AsyncAttachWindow"] = true
			}
			nilFields := apiNilFields(t, table.funcs())
			if len(nilFields) != len(expectNil) {
				t.Errorf("nil 字段数 = %d, want %d: %v", len(nilFields), len(expectNil), nilFields)
			}
			for field := range expectNil {
				if !nilFields[field] {
					t.Errorf("字段 %s 必须为 nil", field)
				}
			}
		})
	}
}

// TestExcludedAndForeignPlatformSymbolsNotLookedUp 验证:全部符号
// 在场时,查找恰好覆盖本平台适用的符号集合——排除项(AsstConnect、
// AsstAttachWindow)与非本平台的 AsstAsyncAttachWindow 不被查找;
// 注册恰好适用数量的符号,能力快照如实反映在场的可选项。
func TestExcludedAndForeignPlatformSymbolsNotLookedUp(t *testing.T) {
	const addr = uintptr(0x00c0ffee)
	fake := &fakePrimitives{openHandle: 0x1000, lookupAddr: addr}
	lib := mustOpenLibrary(t, fake)

	want := expectApplicableNames(t)
	reg := &fakeRegistrar{}
	table, err := resolveAndRegister(lib, reg)
	if err != nil {
		t.Fatalf("全部符号在场时事务不得失败: %v", err)
	}
	if table == nil {
		t.Fatal("全部符号在场时必须发布 API 表")
	}

	// 查找集合:恰好适用符号数量,不多不少、无重复。
	got := fake.lookupNames
	if len(got) != len(want) {
		t.Fatalf("lookup 次数 = %d, want %d(实际 %v)", len(got), len(want), got)
	}
	gotSet := make(map[string]bool, len(got))
	for _, name := range got {
		gotSet[name] = true
	}
	for _, name := range want {
		if !gotSet[name] {
			t.Errorf("适用符号 %s 未被查找", name)
		}
	}
	for _, excluded := range []string{"AsstConnect", "AsstAttachWindow"} {
		if gotSet[excluded] {
			t.Errorf("排除项 %s 不得被查找", excluded)
		}
	}
	if runtime.GOOS != "windows" && gotSet["AsstAsyncAttachWindow"] {
		t.Errorf("非 Windows 平台不得查找 AsstAsyncAttachWindow")
	}

	caps := table.Capabilities()
	if !caps.Create() || !caps.ImageBgr() {
		t.Errorf("在场的可选项必须置位能力, got Create=%v ImageBgr=%v", caps.Create(), caps.ImageBgr())
	}
	if caps.AsyncAttachWindow() != (runtime.GOOS == "windows") {
		t.Errorf("AsyncAttachWindow 能力 = %v, want %v(平台 %s)",
			caps.AsyncAttachWindow(), runtime.GOOS == "windows", runtime.GOOS)
	}

	if n := len(reg.calls); n != len(want) {
		t.Fatalf("注册器调用次数 = %d, want %d", n, len(want))
	}
	targets := make(map[any]bool, len(reg.calls))
	for _, call := range reg.calls {
		if call.addr != addr {
			t.Errorf("注册地址 = %#x, want %#x(解析出的地址必须原样传递)", call.addr, addr)
		}
		if targets[call.target] {
			t.Errorf("同一字段被注册两次: %v", call.target)
		}
		targets[call.target] = true
	}

	// 字段填充:非 Windows 平台 AsyncAttachWindow 为 nil,其余适用
	// 字段全部非 nil。
	expectNil := map[string]bool{}
	if runtime.GOOS != "windows" {
		expectNil["AsyncAttachWindow"] = true
	}
	nilFields := apiNilFields(t, table.funcs())
	if len(nilFields) != len(expectNil) {
		t.Errorf("nil 字段数 = %d, want %d: %v", len(nilFields), len(expectNil), nilFields)
	}
	for field := range expectNil {
		if !nilFields[field] {
			t.Errorf("字段 %s 必须为 nil", field)
		}
	}
}

// TestRegistrationPanicKeepsContextAndDiscardsTable 验证:注册期
// panic 被转换为带符号与签名上下文的 *FunctionRegistrationError——
// panic 值本身是 error 时直接作为 Err(不再包一层),非 error 值
// 转换为诊断错误且不与任何 error 哨兵建立错误链关联;首个 panic 即
// 中止事务,已写临时字段不外泄。
func TestRegistrationPanicKeepsContextAndDiscardsTable(t *testing.T) {
	// 冻结清单(Linux 适用序)的第 1 个适用项是 AsstSetUserDir,第 3
	// 个是 AsstSetStaticOption,其冻结签名为 func(key int32, value
	// *byte) uint8。reflect 渲染的文本不保留参数名,且把 byte 别名
	// 渲染为底层类型 uint8,故期望文本为 reflect 的实际输出。
	const (
		firstSymbol = "AsstSetUserDir"
		firstSig    = "func(*uint8) uint8"
		thirdSymbol = "AsstSetStaticOption"
		thirdSig    = "func(int32, *uint8) uint8"
	)
	panicSentinel := errors.New("fake registrar rejected the binding")

	cases := []struct {
		name       string
		panicOnNth int
		panicValue any
		wantSymbol string
		wantSig    string
	}{
		{"error panic 保留原因", 3, panicSentinel, thirdSymbol, thirdSig},
		{"非 error panic 转诊断", 3, "boom", thirdSymbol, thirdSig},
		{"首个注册即中止", 1, panicSentinel, firstSymbol, firstSig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePrimitives{openHandle: 0x1000, lookupAddr: 0x00c0ffee}
			lib := mustOpenLibrary(t, fake)
			reg := &fakeRegistrar{panicOnNth: tc.panicOnNth, panicValue: tc.panicValue}

			table, err := resolveAndRegister(lib, reg)
			if table != nil {
				t.Fatalf("注册 panic 后不得发布 API 表, got %+v", table)
			}
			if err == nil {
				t.Fatal("注册 panic 必须返回错误")
			}
			var fre *FunctionRegistrationError
			if !errors.As(err, &fre) {
				t.Fatalf("errors.As 未得到 *FunctionRegistrationError, got %T: %v", err, err)
			}
			if n := len(reg.calls); n != tc.panicOnNth {
				t.Errorf("注册器调用次数 = %d, want %d(中止在第 %d 项)", n, tc.panicOnNth, tc.panicOnNth)
			}
			if fre.Symbol != tc.wantSymbol {
				t.Errorf("Symbol = %q, want %q", fre.Symbol, tc.wantSymbol)
			}
			if fre.Signature != tc.wantSig {
				t.Errorf("Signature = %q, want %q", fre.Signature, tc.wantSig)
			}
			if fre.Err == nil {
				t.Fatal("Err 必须非 nil")
			}
			if wantErr, isErr := tc.panicValue.(error); isErr {
				if !errors.Is(err, wantErr) {
					t.Errorf("error panic 值必须保留在错误链上")
				}
				if fre.Err != wantErr {
					t.Errorf("error panic 值必须直接作为 Err(不再包一层), got %T: %v", fre.Err, fre.Err)
				}
			} else {
				if !strings.Contains(fre.Err.Error(), tc.panicValue.(string)) {
					t.Errorf("诊断文本 %q 未包含 panic 值 %q", fre.Err.Error(), tc.panicValue)
				}
				if errors.Is(err, panicSentinel) {
					t.Errorf("非 error panic 转换的诊断错误不得与 sentinel 关联")
				}
			}
		})
	}
}

// TestEachTransactionPublishesDistinctReadOnlyTable 验证:同一 lib
// 上连续两次成功事务各自发布独立结果:两个 APITable 指针不同,各自
// 持有独立的 raw 表,能力快照可读且互不影响。
func TestEachTransactionPublishesDistinctReadOnlyTable(t *testing.T) {
	fake := &fakePrimitives{openHandle: 0x1000, lookupAddr: 0x00c0ffee}
	lib := mustOpenLibrary(t, fake)

	first, err := resolveAndRegister(lib, &fakeRegistrar{})
	if err != nil {
		t.Fatalf("第一次事务意外失败: %v", err)
	}
	second, err := resolveAndRegister(lib, &fakeRegistrar{})
	if err != nil {
		t.Fatalf("第二次事务意外失败: %v", err)
	}
	if first == second {
		t.Fatal("两次事务必须发布不同的 APITable 指针")
	}
	if first.funcs() == second.funcs() {
		t.Fatal("两次事务必须各自构造独立的 raw API 表")
	}
	for i, table := range []*APITable{first, second} {
		if table.funcs() == nil {
			t.Errorf("第 %d 个结果必须持有 raw API 表", i+1)
		}
		caps := table.Capabilities()
		if !caps.Create() || !caps.ImageBgr() {
			t.Errorf("第 %d 个结果的能力快照应反映在场的可选项: Create=%v ImageBgr=%v",
				i+1, caps.Create(), caps.ImageBgr())
		}
		if caps.AsyncAttachWindow() != (runtime.GOOS == "windows") {
			t.Errorf("第 %d 个结果的 AsyncAttachWindow 能力 = %v, want %v",
				i+1, caps.AsyncAttachWindow(), runtime.GOOS == "windows")
		}
	}
}

// TestPuregoRegistrarBindsResolvedAddresses 用生产注册原语与全成功
// 假原语走完整事务:证明本平台适用的全部冻结声明都能被
// purego.RegisterFunc 接受并绑定。假地址只被包装、从不调用,注册期
// 不解引用地址;非 Windows 平台 AsstAsyncAttachWindow 保持 nil。
func TestPuregoRegistrarBindsResolvedAddresses(t *testing.T) {
	fake := &fakePrimitives{openHandle: 0x1000, lookupAddr: 0x00c0ffee}
	lib := mustOpenLibrary(t, fake)

	table, err := resolveAndRegister(lib, puregoRegistrar{})
	if err != nil {
		t.Fatalf("生产注册原语在假地址上绑定失败: %v", err)
	}
	if table == nil {
		t.Fatal("事务必须发布 API 表")
	}

	want := expectApplicableNames(t)
	v := reflect.ValueOf(table.funcs()).Elem()
	typ := v.Type()
	if typ.NumField() != 26 {
		t.Fatalf("api 字段数 = %d, want 26(冻结声明)", typ.NumField())
	}
	registered := 0
	for i := 0; i < typ.NumField(); i++ {
		name := typ.Field(i).Name
		symbol := "Asst" + name
		applicable := false
		for _, wantName := range want {
			if wantName == symbol {
				applicable = true
				break
			}
		}
		if v.Field(i).IsNil() {
			if applicable {
				t.Errorf("适用符号 %s 的字段 %s 必须被绑定", symbol, name)
			}
			continue
		}
		if !applicable {
			t.Errorf("非适用符号 %s 的字段 %s 不得被绑定", symbol, name)
			continue
		}
		registered++
	}
	if registered != len(want) {
		t.Errorf("被绑定的字段数 = %d, want %d", registered, len(want))
	}

	caps := table.Capabilities()
	if !caps.Create() || !caps.ImageBgr() {
		t.Errorf("能力快照应反映在场的可选项: Create=%v ImageBgr=%v", caps.Create(), caps.ImageBgr())
	}
}
