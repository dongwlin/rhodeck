// 本文件是注册事务的故障注入与发布语义用例:经注入边界覆盖缺符号、
// 空地址、可选缺失、平台排除与注册 panic,不进入系统 loader。

package native

import (
	"errors"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"

	"github.com/ebitengine/purego"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
// symbolSpecs 相互独立:非 Windows 平台为 27 项,Windows 为全部
// 28 项。AsstConnect 与 AsstAttachWindow 是排除项,任何平台都不在
// 表内;AsstGetMapLevelKey 与 AsstGetItemName 是 optional 全平台
// 条目(实验导出,缺席只关闭能力)。
var expectedApplicableSymbols = map[string][]string{
	"linux": {
		"AsstSetUserDir", "AsstLoadResource", "AsstSetStaticOption", "AsstCreate",
		"AsstCreateEx", "AsstDestroy", "AsstSetInstanceOption", "AsstAppendTask",
		"AsstSetTaskParams", "AsstStart", "AsstStop", "AsstRunning",
		"AsstConnected", "AsstBackToHome", "AsstAsyncConnect", "AsstSetConnectionExtras",
		"AsstAsyncClick", "AsstAsyncScreencap", "AsstGetImage", "AsstGetImageBgr",
		"AsstGetUUID", "AsstGetTasksList", "AsstGetNullSize", "AsstGetVersion",
		"AsstLog", "AsstGetMapLevelKey", "AsstGetItemName",
	},
	"darwin": {
		"AsstSetUserDir", "AsstLoadResource", "AsstSetStaticOption", "AsstCreate",
		"AsstCreateEx", "AsstDestroy", "AsstSetInstanceOption", "AsstAppendTask",
		"AsstSetTaskParams", "AsstStart", "AsstStop", "AsstRunning",
		"AsstConnected", "AsstBackToHome", "AsstAsyncConnect", "AsstSetConnectionExtras",
		"AsstAsyncClick", "AsstAsyncScreencap", "AsstGetImage", "AsstGetImageBgr",
		"AsstGetUUID", "AsstGetTasksList", "AsstGetNullSize", "AsstGetVersion",
		"AsstLog", "AsstGetMapLevelKey", "AsstGetItemName",
	},
	"windows": {
		"AsstSetUserDir", "AsstLoadResource", "AsstSetStaticOption", "AsstCreate",
		"AsstCreateEx", "AsstDestroy", "AsstSetInstanceOption", "AsstAppendTask",
		"AsstSetTaskParams", "AsstStart", "AsstStop", "AsstRunning",
		"AsstConnected", "AsstBackToHome", "AsstAsyncConnect", "AsstSetConnectionExtras",
		"AsstAsyncClick", "AsstAsyncScreencap", "AsstGetImage", "AsstGetImageBgr",
		"AsstGetUUID", "AsstGetTasksList", "AsstGetNullSize", "AsstGetVersion",
		"AsstLog", "AsstAsyncAttachWindow", "AsstGetMapLevelKey", "AsstGetItemName",
	},
}

// expectApplicableNames 取当前平台的适用符号期望表;无期望表的平台
// 直接失败,不静默放行。
func expectApplicableNames(t *testing.T) []string {
	t.Helper()
	names, ok := expectedApplicableSymbols[runtime.GOOS]
	if !ok {
		require.FailNow(t, "platform %s lacks a standalone applicable-symbol expectation table", runtime.GOOS)
	}
	return names
}

// mustOpenLibrary 以给定的假原语打开假库:openHandle 与 lookupAddr
// 由调用方保证非零,各用例在此基础上按需注入失败。
func mustOpenLibrary(t *testing.T, fake *fakePrimitives) *Library {
	t.Helper()
	path := filepath.Join(t.TempDir(), "libMaaCore.so")
	lib, err := newFakeLoader(fake).Open(path)
	require.NoError(t, err, "Open(%q) returned an unexpected error", path)
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
	require.Nil(t, table, "must not publish the API table when required symbols are missing, got %+v", table)
	require.Error(t, err, "an error must be returned when required symbols are missing")
	var agg AggregateError
	require.ErrorAs(t, err, &agg, "errors.As did not yield an AggregateError, got %T: %v", err, err)
	require.Len(t, agg, 2, "aggregate error item count = %d, want 2:\n%v", len(agg), err)
	want := map[string]error{
		"AsstCreateEx":   sentinelCreateEx,
		"AsstGetVersion": sentinelVersion,
	}
	for _, item := range agg {
		var sle *SymbolLookupError
		require.ErrorAs(t, item, &sle, "aggregate item %T is not a *SymbolLookupError: %v", item, item)
		wantErr, ok := want[sle.Symbol]
		if !assert.True(t, ok, "aggregate item symbol %q is not in the expected missing set", sle.Symbol) {
			continue
		}
		assert.ErrorIs(t, item, wantErr, "the error chain for symbol %q did not reach the injected underlying sentinel", sle.Symbol)
		delete(want, sle.Symbol)
	}
	keys := make([]string, 0, len(want))
	for symbol := range want {
		keys = append(keys, symbol)
	}
	assert.Empty(t, keys, "aggregate error is missing symbols: %v", keys)
	assert.Empty(t, reg.calls, "the registrar must not be called while required items are unsatisfied, got %d calls", len(reg.calls))
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
	require.Nil(t, table, "must not publish the API table when required symbols are unavailable, got %+v", table)
	require.Error(t, err, "an error must be returned when required symbols are unavailable")
	var agg AggregateError
	require.ErrorAs(t, err, &agg, "errors.As did not yield an AggregateError, got %T: %v", err, err)
	require.Len(t, agg, 2, "aggregate error item count = %d, want 2:\n%v", len(agg), err)

	bySymbol := make(map[string]error, len(agg))
	for _, item := range agg {
		var sle *SymbolLookupError
		require.ErrorAs(t, item, &sle, "aggregate item %T is not a *SymbolLookupError: %v", item, item)
		bySymbol[sle.Symbol] = item
	}
	nullItem, ok := bySymbol["AsstCreateEx"]
	require.True(t, ok, "the aggregate is missing the null-address item AsstCreateEx:\n%v", err)
	assert.ErrorIs(t, nullItem, errSymbolNullAddress, "the error chain for AsstCreateEx does not identify the null-address cause")
	versionItem, ok := bySymbol["AsstGetVersion"]
	require.True(t, ok, "the aggregate is missing the lookup-failure item AsstGetVersion:\n%v", err)
	assert.ErrorIs(t, versionItem, sentinel, "the error chain for AsstGetVersion did not reach the injected sentinel")
	assert.NotErrorIs(t, nullItem, sentinel, "null-address and lookup-failure causes must not be confused with each other")
	assert.NotErrorIs(t, versionItem, errSymbolNullAddress, "null-address and lookup-failure causes must not be confused with each other")
	assert.Empty(t, reg.calls, "the registrar must not be called while required items are unsatisfied, got %d calls", len(reg.calls))
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
			"lookup failure",
			map[string]error{
				"AsstCreate":      errors.New("fake dlsym: AsstCreate missing"),
				"AsstGetImageBgr": errors.New("fake dlsym: AsstGetImageBgr missing"),
			},
			nil,
		},
		{"null address", nil, map[string]bool{"AsstCreate": true, "AsstGetImageBgr": true}},
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
			require.NoError(t, err, "a missing optional must not fail the transaction")
			require.NotNil(t, table, "the API table must still be published when an optional is missing")

			caps := table.Capabilities()
			// Windows 的 AsstAsyncAttachWindow 在假库中仍然存在,
			// 只有本用例移除的两个可选项应关闭。
			wantAttachWindow := runtime.GOOS == "windows"
			assert.False(t, caps.Create(), "optional capabilities mismatch, got Create=%v, want false", caps.Create())
			assert.False(t, caps.ImageBgr(), "optional capabilities mismatch, got ImageBgr=%v, want false", caps.ImageBgr())
			assert.Equal(t, wantAttachWindow, caps.AsyncAttachWindow(), "optional capabilities mismatch, got AsyncAttachWindow=%v, want %v", caps.AsyncAttachWindow(), wantAttachWindow)
			wantCalls := len(expectApplicableNames(t)) - missingO
			assert.Len(t, reg.calls, wantCalls, "registrar call count = %d, want %d (%d applicable items minus %d missing optionals)", len(reg.calls), wantCalls, len(expectApplicableNames(t)), missingO)

			// 缺失的两个可选项字段为 nil;非 Windows 平台上不适用
			// 的 AsyncAttachWindow 同样为 nil,其余适用字段全部非 nil。
			expectNil := map[string]bool{"Create": true, "GetImageBgr": true}
			if runtime.GOOS != "windows" {
				expectNil["AsyncAttachWindow"] = true
			}
			nilFields := apiNilFields(t, table.funcs())
			assert.Len(t, nilFields, len(expectNil), "nil field count = %d, want %d: %v", len(nilFields), len(expectNil), nilFields)
			for field := range expectNil {
				assert.Contains(t, nilFields, field, "field %s must be nil", field)
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
	require.NoError(t, err, "the transaction must not fail when all symbols are present")
	require.NotNil(t, table, "the API table must be published when all symbols are present")

	// 查找集合:恰好适用符号数量,不多不少、无重复。
	got := fake.lookupNames
	require.Len(t, got, len(want), "lookup count = %d, want %d (actual %v)", len(got), len(want), got)
	gotSet := make(map[string]bool, len(got))
	for _, name := range got {
		gotSet[name] = true
	}
	for _, name := range want {
		assert.Contains(t, gotSet, name, "applicable symbol %s was not looked up", name)
	}
	for _, excluded := range []string{"AsstConnect", "AsstAttachWindow"} {
		assert.NotContains(t, gotSet, excluded, "excluded symbol %s must not be looked up", excluded)
	}
	if runtime.GOOS != "windows" {
		assert.NotContains(t, gotSet, "AsstAsyncAttachWindow", "non-Windows platforms must not look up AsstAsyncAttachWindow")
	}

	caps := table.Capabilities()
	assert.True(t, caps.Create(), "present optionals must set their capabilities, got Create=%v", caps.Create())
	assert.True(t, caps.ImageBgr(), "present optionals must set their capabilities, got ImageBgr=%v", caps.ImageBgr())
	assert.Equal(t, runtime.GOOS == "windows", caps.AsyncAttachWindow(), "AsyncAttachWindow capability = %v, want %v (platform %s)", caps.AsyncAttachWindow(), runtime.GOOS == "windows", runtime.GOOS)

	require.Len(t, reg.calls, len(want), "registrar call count = %d, want %d", len(reg.calls), len(want))
	targets := make(map[any]bool, len(reg.calls))
	for _, call := range reg.calls {
		assert.Equal(t, addr, call.addr, "registered address = %#x, want %#x (the resolved address must be passed through as-is)", call.addr, addr)
		assert.False(t, targets[call.target], "the same field was registered twice: %v", call.target)
		targets[call.target] = true
	}

	// 字段填充:非 Windows 平台 AsyncAttachWindow 为 nil,其余适用
	// 字段全部非 nil。
	expectNil := map[string]bool{}
	if runtime.GOOS != "windows" {
		expectNil["AsyncAttachWindow"] = true
	}
	nilFields := apiNilFields(t, table.funcs())
	assert.Len(t, nilFields, len(expectNil), "nil field count = %d, want %d: %v", len(nilFields), len(expectNil), nilFields)
	for field := range expectNil {
		assert.Contains(t, nilFields, field, "field %s must be nil", field)
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
		{"error panic keeps the cause", 3, panicSentinel, thirdSymbol, thirdSig},
		{"non-error panic becomes a diagnostic", 3, "boom", thirdSymbol, thirdSig},
		{"aborts on the first registration", 1, panicSentinel, firstSymbol, firstSig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakePrimitives{openHandle: 0x1000, lookupAddr: 0x00c0ffee}
			lib := mustOpenLibrary(t, fake)
			reg := &fakeRegistrar{panicOnNth: tc.panicOnNth, panicValue: tc.panicValue}

			table, err := resolveAndRegister(lib, reg)
			require.Nil(t, table, "must not publish the API table after a registration panic, got %+v", table)
			require.Error(t, err, "a registration panic must return an error")
			var fre *FunctionRegistrationError
			require.ErrorAs(t, err, &fre, "errors.As did not yield a *FunctionRegistrationError, got %T: %v", err, err)
			assert.Len(t, reg.calls, tc.panicOnNth, "registrar call count = %d, want %d (aborted at item %d)", len(reg.calls), tc.panicOnNth, tc.panicOnNth)
			assert.Equal(t, tc.wantSymbol, fre.Symbol, "Symbol = %q, want %q", fre.Symbol, tc.wantSymbol)
			assert.Equal(t, tc.wantSig, fre.Signature, "Signature = %q, want %q", fre.Signature, tc.wantSig)
			require.Error(t, fre.Err, "Err must be non-nil")
			if wantErr, isErr := tc.panicValue.(error); isErr {
				assert.ErrorIs(t, err, wantErr, "an error panic value must stay on the error chain")
				assert.True(t, fre.Err == wantErr, "an error panic value must be used directly as Err (no extra wrapping), got %T: %v", fre.Err, fre.Err)
			} else {
				assert.Contains(t, fre.Err.Error(), tc.panicValue.(string), "diagnostic text %q does not contain the panic value %q", fre.Err.Error(), tc.panicValue)
				assert.NotErrorIs(t, err, panicSentinel, "the diagnostic error converted from a non-error panic must not be associated with the sentinel")
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
	require.NoError(t, err, "the first transaction failed unexpectedly")
	second, err := resolveAndRegister(lib, &fakeRegistrar{})
	require.NoError(t, err, "the second transaction failed unexpectedly")
	require.NotSame(t, first, second, "the two transactions must publish different APITable pointers")
	require.NotSame(t, first.funcs(), second.funcs(), "the two transactions must each build an independent raw API table")
	for i, table := range []*APITable{first, second} {
		assert.NotNil(t, table.funcs(), "result #%d must hold a raw API table", i+1)
		caps := table.Capabilities()
		assert.True(t, caps.Create(), "result #%d capability snapshot should reflect the present optionals: Create=%v", i+1, caps.Create())
		assert.True(t, caps.ImageBgr(), "result #%d capability snapshot should reflect the present optionals: ImageBgr=%v", i+1, caps.ImageBgr())
		assert.Equal(t, runtime.GOOS == "windows", caps.AsyncAttachWindow(), "result #%d AsyncAttachWindow capability = %v, want %v", i+1, caps.AsyncAttachWindow(), runtime.GOOS == "windows")
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
	require.NoError(t, err, "the production registration primitive failed to bind fake addresses")
	require.NotNil(t, table, "the transaction must publish the API table")

	want := expectApplicableNames(t)
	v := reflect.ValueOf(table.funcs()).Elem()
	typ := v.Type()
	require.Equal(t, 28, typ.NumField(), "api field count = %d, want 28 (frozen declarations)", typ.NumField())
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
			assert.False(t, applicable, "field %s of applicable symbol %s must be bound", symbol, name)
			continue
		}
		if !assert.True(t, applicable, "field %s of non-applicable symbol %s must not be bound", symbol, name) {
			continue
		}
		registered++
	}
	assert.Equal(t, len(want), registered, "bound field count = %d, want %d", registered, len(want))

	caps := table.Capabilities()
	assert.True(t, caps.Create(), "capability snapshot should reflect the present optionals: Create=%v", caps.Create())
	assert.True(t, caps.ImageBgr(), "capability snapshot should reflect the present optionals: ImageBgr=%v", caps.ImageBgr())
}
