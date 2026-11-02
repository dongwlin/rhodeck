// compare_test.go 校验宽度级 C↔Go 逐参数语义比对与 callback 平台适
// 配比对:冻结参数名映射与类型映射表逐条钉死(接受侧含 byte 别名,
// 拒绝侧含的全部宽度/指针/角色向量,含 char pointee 的
// const 语义:丢 const 与 UUID buff 加 const 都拒绝);正例以真实生产
// native 包加合成 C 输入证明冻结映射与生产 api.go/callback_types.go
// 逐字段一致;失败向量直接构造改动并断言 signature 类诊断的过错方定
// 位(Go 侧定位到字段/callback 声明行,C 侧定位到 C 声明行);端到端
// 变体走完整命令入口 Run 断言非零退出与符号定位。C callback 的
// void 改非 void 与丢 ASST_CALL 由 c-syntax 层拒绝,本文件同时钉死
// 该归属。

package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---- 直接构造用的规范化 C 类型小构造器 ----

func sigCScalar(name string, bits int, signed bool) CType {
	return CType{Kind: CKindScalar, Name: name, Bits: bits, Signed: signed}
}

func sigCPtr(pt CType) CType {
	return CType{Kind: CKindPointer, Pointee: &pt}
}

func sigCStruct(tag string) CType {
	return CType{Kind: CKindStruct, Tag: tag}
}

func sigCFuncPtr() CType {
	return CType{Kind: CKindFuncPtr, Sig: &CFuncSig{
		Ret:    CType{Kind: CKindVoid},
		Params: []CParam{{Name: "msg", Type: sigCScalar("int32", 32, true)}},
	}}
}

// ---- 提取基线与改动辅助 ----

// mustSignatureBase 用合成 C 正例加真实生产 native 包构造 link 成功
// 后的比对基线(linux 目标,25 条 Bound + 1 条 NonTarget 的
// windowsOnly 绑定,共 26 个被比对符号)。
func mustSignatureBase(t *testing.T) (*CExtraction, *GoExtraction, *LinkResult) {
	t.Helper()
	c := mustExtract(t, positiveFiles(t), linuxTarget)
	g := mustExtractGo(t, copyRealNative(t))
	link, diags := LinkCGo(c, g, linuxTarget)
	require.Empty(t, diags, "LinkCGo unexpectedly failed: %v", diags)
	return c, g, link
}

// sigField 取真实提取结果中的一个 api 字段。
func sigField(t *testing.T, g *GoExtraction, name string) *GoField {
	t.Helper()
	for i := range g.Fields {
		if g.Fields[i].Name == name {
			return &g.Fields[i]
		}
	}
	require.FailNowf(t, "api field not found", "api field %s not found", name)
	return nil
}

// mutateField 原地修改一个 api 字段。
func mutateField(t *testing.T, g *GoExtraction, name string, f func(*GoField)) {
	t.Helper()
	fl := sigField(t, g, name)
	f(fl)
}

// mutateCFunc 原地修改一个 C 函数声明。
func mutateCFunc(t *testing.T, c *CExtraction, name string, f func(*CFunction)) {
	t.Helper()
	for i := range c.Functions {
		if c.Functions[i].Name == name {
			f(&c.Functions[i])
			return
		}
	}
	require.FailNowf(t, "c function not found", "c function %s not found", name)
}

// mustSigFail 断言签名比对以 signature 类诊断失败,且诊断定位到期望
// 的文件/行号/符号并含全部期望片段。
func mustSigFail(t *testing.T, c *CExtraction, g *GoExtraction, link *LinkResult,
	wantFile string, wantLine int, wantSymbol string, wantTexts ...string) Diagnostic {
	t.Helper()
	res, diags := CompareSignatures(c, g, link)
	// 失败路径的 res 为 nil,符号计数须留在原 if 守卫内惰性求值。
	if len(diags) == 0 {
		require.FailNowf(t, "CompareSignatures unexpectedly passed",
			"CompareSignatures unexpectedly passed (%d symbols); want failure mentioning %v",
			len(res.Symbols), wantTexts)
	}
	d := diags[0]
	assert.Equal(t, CatSignature, d.Category, "category = %q, want %q", d.Category, CatSignature)
	assert.False(t, d.File != wantFile || (wantLine > 0 && d.Line != wantLine),
		"location = %s:%d, want %s:%d", d.File, d.Line, wantFile, wantLine)
	assert.Equal(t, wantSymbol, d.Symbol, "symbol = %q, want %q", d.Symbol, wantSymbol)
	joined := d.Expected + "\n" + d.Actual + "\n" + d.Message
	for _, want := range wantTexts {
		assert.Contains(t, joined, want, "diagnostic %s does not mention %q", d.String(), want)
	}
	return d
}

// ---- 冻结映射表钉死 ----

// TestApprovedParamNameMapping 钉死冻结的 8 个参数名重命名,
// 并确认其余名字原样保留(同名规则)。
func TestApprovedParamNameMapping(t *testing.T) {
	want := map[string]string{
		"type":             "taskType",
		"adb_path":         "adbPath",
		"custom_arg":       "customArg",
		"buff_size":        "buffSize",
		"details_json":     "detailsJSON",
		"screencap_method": "screencapMethod",
		"mouse_method":     "mouseMethod",
		"keyboard_method":  "keyboardMethod",
	}
	require.Len(t, approvedParamNames, len(want),
		"approved param names = %d entries, want %d", len(approvedParamNames), len(want))
	for c, g := range want {
		got := approvedParamName(c)
		assert.Equal(t, g, got, "approvedParamName(%q) = %q, want %q", c, got, g)
	}
	// 未批准重命名的名字必须原样同名。
	for _, c := range []string{"handle", "path", "key", "value", "id", "params", "block",
		"x", "y", "buff", "name", "level", "message", "msg", "address", "config",
		"callback", "hwnd", "screencap", "details", "customArg"} {
		got := approvedParamName(c)
		assert.Equal(t, c, got, "approvedParamName(%q) = %q, want the identity mapping", c, got)
	}
}

// TestPermittedGoTypesTable 逐条钉死类型映射表:接受侧给出许可拼写
// (固定顺序),拒绝侧为空——覆盖的宽度/指针/角色拒绝向量
// 与 void* 的三种上下文许可。
func TestPermittedGoTypesTable(t *testing.T) {
	cUint8 := func() CType { return sigCScalar("uint8", 8, false) }
	cInt32 := func() CType { return sigCScalar("int32", 32, true) }
	cUint64 := func() CType { return sigCScalar("uint64", 64, false) }
	cChar := func() CType { return sigCScalar("char", 8, false) }
	cConstCharPtr := func() CType { return sigCPtr(CType{Kind: CKindScalar, Name: "char", Bits: 8, Const: true}) }
	cCharPtr := func() CType { return sigCPtr(cChar()) }
	cVoidPtr := func() CType { return sigCPtr(CType{Kind: CKindVoid}) }
	cInt32Ptr := func() CType { return sigCPtr(cInt32()) }

	cases := []struct {
		name   string
		symbol string
		param  string
		ct     func() CType
		want   []GoSourceType
	}{
		{"uint8 scalar", "AsstSetUserDir", "", cUint8, []GoSourceType{"uint8", "byte"}},
		{"int32 scalar", "AsstAsyncClick", "x", cInt32, []GoSourceType{"int32"}},
		{"uint64 scalar", "AsstGetNullSize", "", cUint64, []GoSourceType{"uint64"}},
		{"const scalar rejected", "AsstSetStaticOption", "key",
			func() CType { return CType{Kind: CKindScalar, Name: "int32", Bits: 32, Signed: true, Const: true} }, nil},
		{"char scalar rejected", "AsstSetUserDir", "path", cChar, nil},
		{"unknown scalar rejected", "AsstDestroy", "handle",
			func() CType { return sigCScalar("int64", 64, true) }, nil},
		{"const char pointer", "AsstSetUserDir", "path", cConstCharPtr, []GoSourceType{"*byte"}},
		{"char pointer uuid buff", "AsstGetUUID", "buff", cCharPtr, []GoSourceType{"*byte"}},
		{"non-const char pointer outside uuid buff rejected", "AsstSetUserDir", "path", cCharPtr, nil},
		{"non-const char pointer on another output rejected", "AsstGetUUID", "path", cCharPtr, nil},
		{"const char pointer on uuid buff rejected", "AsstGetUUID", "buff", cConstCharPtr, nil},
		{"non-const char pointer return rejected", "AsstGetVersion", "", cCharPtr, nil},
		{"non-const char pointer on callback rejected", "unixCallback", "details_json", cCharPtr, nil},
		{"uint8 pointee rejected", "AsstGetVersion", "", func() CType { return sigCPtr(cUint8()) }, nil},
		{"int32 pointer task list", "AsstGetTasksList", "buff", cInt32Ptr, []GoSourceType{"*int32"}},
		{"opaque handle", "AsstDestroy", "handle",
			func() CType { return sigCPtr(sigCStruct("AsstExtAPI")) }, []GoSourceType{"uintptr"}},
		{"other struct pointee rejected", "AsstDestroy", "handle",
			func() CType { return sigCPtr(sigCStruct("AsstMapLevelKey")) }, nil},
		{"callback function pointer", "AsstCreateEx", "callback", sigCFuncPtr, []GoSourceType{"uintptr"}},
		{"void* custom_arg token", "AsstCreateEx", "custom_arg", cVoidPtr, []GoSourceType{"uintptr"}},
		{"void* hwnd on window symbol", "AsstAsyncAttachWindow", "hwnd", cVoidPtr, []GoSourceType{"uintptr"}},
		{"void* hwnd on non-window symbol rejected", "AsstDestroy", "hwnd", cVoidPtr, nil},
		{"void* buff on AsstGetImage", "AsstGetImage", "buff", cVoidPtr, []GoSourceType{GoTypeUnsafePointer}},
		{"void* buff on AsstGetImageBgr", "AsstGetImageBgr", "buff", cVoidPtr, []GoSourceType{GoTypeUnsafePointer}},
		{"void* buff on other symbol rejected", "AsstGetUUID", "buff", cVoidPtr, nil},
		{"void* unapproved name rejected", "AsstDestroy", "data", cVoidPtr, nil},
		{"void* return position rejected", "AsstGetImage", "", cVoidPtr, nil},
		{"struct value rejected", "AsstDestroy", "handle",
			func() CType { return sigCStruct("AsstExtAPI") }, nil},
		{"void rejected", "AsstDestroy", "handle",
			func() CType { return CType{Kind: CKindVoid} }, nil},
		{"multi-level pointer rejected", "AsstGetUUID", "buff",
			func() CType { return sigCPtr(cCharPtr()) }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := permittedGoTypes(tc.symbol, tc.param, tc.ct())
			require.Len(t, got, len(tc.want), "permittedGoTypes = %v, want %v", got, tc.want)
			for i := range got {
				require.Equal(t, tc.want[i], got[i],
					"permittedGoTypes = %v, want %v (order is part of the frozen table)", got, tc.want)
			}
		})
	}
}

// ---- 正例:冻结映射与生产声明逐字段一致 ----

// TestCompareSignaturesPositiveRealNative 用真实生产 native 包加合成
// C 正例证明冻结映射与生产 api.go/callback_types.go 逐字段一致:两
// 目标全部绑定条目(目标适用绑定与非目标 windowsOnly 绑定,共 26
// 项)通过,符号按名排序,callback 两条均核验;关键映射逐点抽查。
// byte 别名同样接受。
func TestCompareSignaturesPositiveRealNative(t *testing.T) {
	c, g, link := mustSignatureBase(t)
	res, diags := CompareSignatures(c, g, link)
	require.Empty(t, diags, "CompareSignatures unexpectedly failed: %v", diags)
	require.Len(t, res.Symbols, len(link.Bound)+len(link.NonTarget),
		"symbols = %d, want %d (one entry per binding, including non-target windowsOnly bindings)",
		len(res.Symbols), len(link.Bound)+len(link.NonTarget))
	covered := map[string]bool{}
	for i, e := range res.Symbols {
		if i > 0 {
			// res.Symbols[i-1] 须留在 i>0 守卫内,避免越界求值。
			require.False(t, e.Symbol <= res.Symbols[i-1].Symbol,
				"symbols must be strictly sorted: %q after %q", e.Symbol, res.Symbols[i-1].Symbol)
		}
		require.False(t, covered[e.Symbol], "symbol %s compared more than once", e.Symbol)
		covered[e.Symbol] = true
	}
	for _, b := range link.Bound {
		assert.True(t, covered[b.Symbol], "bound symbol %s missing from the signature result", b.Symbol)
	}
	for _, n := range link.NonTarget {
		assert.True(t, covered[n], "non-target windowsOnly binding %s missing from the signature result", n)
	}
	entry := func(symbol string) SignatureEntry {
		t.Helper()
		for _, e := range res.Symbols {
			if e.Symbol == symbol {
				return e
			}
		}
		require.FailNowf(t, "signature entry not found", "signature entry %s not found", symbol)
		return SignatureEntry{}
	}

	// 图像缓冲区:C void* ↔ unsafe.Pointer;容量/返回 uint64。
	gi := entry("AsstGetImage")
	assert.False(t, gi.Field != "GetImage" || gi.GoFile != "api.go" || gi.GoLine <= 0 ||
		gi.Header != FileAsstCaller || gi.CLine <= 0, "AsstGetImage record = %+v", gi)
	assert.False(t, gi.Params[1].Position != 2 || gi.Params[1].CName != "buff" || gi.Params[1].GoName != "buff" ||
		gi.Params[1].CType != "void*" || gi.Params[1].GoType != "unsafe.Pointer",
		"AsstGetImage buff = %+v", gi.Params[1])
	assert.False(t, gi.Params[2].CName != "buff_size" || gi.Params[2].GoName != "buffSize" ||
		gi.Params[2].CType != "uint64" || gi.Params[2].GoType != "uint64",
		"AsstGetImage buff_size = %+v", gi.Params[2])
	assert.Equal(t, SignatureReturn{CType: "uint64", GoType: "uint64"}, gi.Return,
		"AsstGetImage return = %+v", gi.Return)
	// 批准重命名:type→taskType;opaque handle 与 callback 函数指针。
	at := entry("AsstAppendTask")
	assert.False(t, at.Params[1].CName != "type" || at.Params[1].GoName != "taskType" ||
		at.Params[1].CType != "const char*" || at.Params[1].GoType != "*byte",
		"AsstAppendTask type = %+v", at.Params[1])
	ce := entry("AsstCreateEx")
	assert.False(t, ce.Params[0].CName != "callback" || ce.Params[0].GoName != "callback" ||
		ce.Params[0].GoType != "uintptr" || !strings.HasPrefix(ce.Params[0].CType, "void("),
		"AsstCreateEx callback = %+v", ce.Params[0])
	assert.False(t, ce.Params[1].CName != "custom_arg" || ce.Params[1].GoName != "customArg" ||
		ce.Params[1].CType != "void*" || ce.Params[1].GoType != "uintptr",
		"AsstCreateEx custom_arg = %+v", ce.Params[1])
	assert.Equal(t, "uintptr", ce.Return.GoType, "AsstCreateEx return = %+v", ce.Return)
	de := entry("AsstDestroy")
	assert.False(t, de.Params[0].CName != "handle" || de.Params[0].GoName != "handle" ||
		de.Params[0].CType != "struct AsstExtAPI*" || de.Params[0].GoType != "uintptr",
		"AsstDestroy handle = %+v", de.Params[0])
	tl := entry("AsstGetTasksList")
	assert.False(t, tl.Params[1].CType != "int32*" || tl.Params[1].GoType != "*int32",
		"AsstGetTasksList buff = %+v", tl.Params[1])
	// void 返回与 const char* 返回。
	lg := entry("AsstLog")
	assert.Equal(t, SignatureReturn{CType: "void", GoType: ""}, lg.Return, "AsstLog return = %+v", lg.Return)
	gv := entry("AsstGetVersion")
	assert.Equal(t, SignatureReturn{CType: "const char*", GoType: "*byte"}, gv.Return,
		"AsstGetVersion return = %+v", gv.Return)

	// callback 平台适配:unix 原样 void,windows 许可适配 uintptr;
	// 参数逐位置核验(批准映射 details_json→detailsJSON、
	// custom_arg→customArg)。
	require.Len(t, res.Callbacks, 2, "callbacks = %d, want 2", len(res.Callbacks))
	uc, wc := res.Callbacks[0], res.Callbacks[1]
	assert.False(t, uc.Name != "unixCallback" || uc.GoFile != "callback_types.go" || uc.GoLine <= 0,
		"unixCallback record = %+v", uc)
	wantParams := []SignatureParam{
		{Position: 1, CName: "msg", GoName: "msg", CType: "int32", GoType: "int32"},
		{Position: 2, CName: "details_json", GoName: "detailsJSON", CType: "const char*", GoType: "*byte"},
		{Position: 3, CName: "custom_arg", GoName: "customArg", CType: "void*", GoType: "uintptr"},
	}
	for i, w := range wantParams {
		assert.Equal(t, w, uc.Params[i], "unixCallback param %d = %+v, want %+v", i, uc.Params[i], w)
		assert.Equal(t, w, wc.Params[i], "windowsCallback param %d = %+v, want %+v", i, wc.Params[i], w)
	}
	assert.Equal(t, SignatureReturn{CType: "void", GoType: ""}, uc.Return,
		"unixCallback return = %+v", uc.Return)
	assert.False(t, wc.Name != "windowsCallback" || wc.Return != (SignatureReturn{CType: "void", GoType: "uintptr"}),
		"windowsCallback record = %+v", wc)

	// byte 别名接受:把 uint8 位置的拼写换成 byte 仍通过。
	mutateField(t, g, "SetUserDir", func(f *GoField) { f.Result = "byte" })
	mutateField(t, g, "AsyncClick", func(f *GoField) { f.Params[3].Type = "byte" })
	_, diags = CompareSignatures(c, g, link)
	require.Empty(t, diags, "byte alias must be accepted: %v", diags)

	// linux 目标:窗口符号属 NonTarget 的 windowsOnly 绑定,同样被
	// 逐参数核验并出现在核验记录里(hwnd 与 method 映射与 windows
	// 目标一致)。
	awl := entry("AsstAsyncAttachWindow")
	assert.False(t, awl.Params[1].CName != "hwnd" || awl.Params[1].GoName != "hwnd" ||
		awl.Params[1].CType != "void*" || awl.Params[1].GoType != "uintptr",
		"linux AsstAsyncAttachWindow hwnd = %+v", awl.Params[1])
	assert.False(t, awl.Params[2].CName != "screencap_method" || awl.Params[2].GoName != "screencapMethod" ||
		awl.Params[2].CType != "uint64" || awl.Params[2].GoType != "uint64",
		"linux AsstAsyncAttachWindow screencap_method = %+v", awl.Params[2])

	// windows 目标:26 条绑定全部通过,窗口符号经 Bound 分类核验,
	// hwnd/method 映射同上,不得重复报错或行为回退。
	cw := mustExtract(t, positiveFiles(t), windowsTarget)
	linkw, diags := LinkCGo(cw, g, windowsTarget)
	require.Empty(t, diags, "windows LinkCGo unexpectedly failed: %v", diags)
	resw, diags := CompareSignatures(cw, g, linkw)
	require.Empty(t, diags, "windows CompareSignatures unexpectedly failed: %v", diags)
	require.Len(t, resw.Symbols, 28, "windows symbols = %d, want 28", len(resw.Symbols))
	var aw *SignatureEntry
	for i := range resw.Symbols {
		if resw.Symbols[i].Symbol == "AsstAsyncAttachWindow" {
			aw = &resw.Symbols[i]
		}
	}
	require.NotNil(t, aw, "windows signatures must cover AsstAsyncAttachWindow")
	assert.False(t, aw.Params[1].CName != "hwnd" || aw.Params[1].GoName != "hwnd" ||
		aw.Params[1].CType != "void*" || aw.Params[1].GoType != "uintptr",
		"AsstAsyncAttachWindow hwnd = %+v", aw.Params[1])
	assert.False(t, aw.Params[2].CName != "screencap_method" || aw.Params[2].GoName != "screencapMethod" ||
		aw.Params[2].CType != "uint64" || aw.Params[2].GoType != "uint64",
		"AsstAsyncAttachWindow screencap_method = %+v", aw.Params[2])
}

// ---- 失败向量:Go 侧改动定位到 Go 声明,C 侧定位到 C 声明 ----

// TestCompareSignatureGoSideFailures 是 Go 侧破坏向量:每个变体从真实
// 生产提取结果出发改一处,断言 signature 诊断定位到过错方并给出参数
// 位置/名称与期望类型。参数个数变化没有过错方位置,定位到作为 ABI
// 参照的 C 声明。
func TestCompareSignatureGoSideFailures(t *testing.T) {
	cases := []struct {
		name      string
		symbol    string
		goSide    bool // true: 定位到 api.go 字段行;false: 定位到 C 声明行
		mutate    func(t *testing.T, c *CExtraction, g *GoExtraction)
		wantTexts []string
	}{
		{"uint8 return becomes bool", "AsstSetUserDir", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "SetUserDir", func(f *GoField) { f.Result = "bool" })
		}, []string{"uint8 or byte", "result type must match"}},
		{"int32 return becomes int", "AsstAppendTask", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "AppendTask", func(f *GoField) { f.Result = "int" })
		}, []string{"int32"}},
		{"int32 param becomes uint32", "AsstSetTaskParams", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "SetTaskParams", func(f *GoField) { f.Params[1].Type = "uint32" })
		}, []string{"parameter 2 (id)", "int32"}},
		{"uint64 param becomes uintptr", "AsstGetImage", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "GetImage", func(f *GoField) { f.Params[2].Type = "uintptr" })
		}, []string{"parameter 3 (buff_size)", "uint64"}},
		{"uint64 return becomes uint32", "AsstGetNullSize", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "GetNullSize", func(f *GoField) { f.Result = "uint32" })
		}, []string{"uint64"}},
		{"void return gains uint8", "AsstLog", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "Log", func(f *GoField) { f.Result = "uint8" })
		}, []string{"no result", "must not gain a go result"}},
		{"char pointer return becomes uintptr", "AsstGetVersion", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "GetVersion", func(f *GoField) { f.Result = "uintptr" })
		}, []string{"*byte"}},
		{"task list pointer becomes unsafe.Pointer", "AsstGetTasksList", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "GetTasksList", func(f *GoField) { f.Params[1].Type = GoTypeUnsafePointer })
		}, []string{"parameter 2 (buff)", "*int32"}},
		{"image buffer becomes uintptr", "AsstGetImage", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "GetImage", func(f *GoField) { f.Params[1].Type = "uintptr" })
		}, []string{"parameter 2 (buff)", "unsafe.Pointer"}},
		{"unapproved parameter rename", "AsstAppendTask", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "AppendTask", func(f *GoField) { f.Params[2].Name = "param" })
		}, []string{"parameter 3 name params"}},
		{"approved rename not applied", "AsstAppendTask", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "AppendTask", func(f *GoField) { f.Params[1].Name = "type" })
		}, []string{"parameter 2 name taskType"}},
		{"same-type reorder x/y", "AsstAsyncClick", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "AsyncClick", func(f *GoField) {
				f.Params[1].Name, f.Params[2].Name = f.Params[2].Name, f.Params[1].Name
			})
		}, []string{"parameter 2 name x"}},
		{"callback token reorder", "AsstCreateEx", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "CreateEx", func(f *GoField) {
				f.Params[0].Name, f.Params[1].Name = f.Params[1].Name, f.Params[0].Name
			})
		}, []string{"parameter 1 name callback"}},
		{"level/message reorder", "AsstLog", true, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "Log", func(f *GoField) {
				f.Params[0].Name, f.Params[1].Name = f.Params[1].Name, f.Params[0].Name
			})
		}, []string{"parameter 1 name level"}},
		{"parameter added", "AsstDestroy", false, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "Destroy", func(f *GoField) {
				f.Params = append(f.Params, GoParam{Name: "extra", Type: "uintptr"})
			})
		}, []string{"1 named parameters", "parameter count differs"}},
		{"parameter removed", "AsstLog", false, func(t *testing.T, c *CExtraction, g *GoExtraction) {
			mutateField(t, g, "Log", func(f *GoField) { f.Params = f.Params[1:] })
		}, []string{"2 named parameters", "parameter count differs"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, g, link := mustSignatureBase(t)
			tc.mutate(t, c, g)
			if tc.goSide {
				fl := sigField(t, g, strings.TrimPrefix(tc.symbol, "Asst"))
				mustSigFail(t, c, g, link, "api.go", fl.Line, tc.symbol, tc.wantTexts...)
			} else {
				cf := findFunction(t, c.Functions, tc.symbol)
				mustSigFail(t, c, g, link, FileAsstCaller, cf.Line, tc.symbol, tc.wantTexts...)
			}
		})
	}

	// 窗口 method 调序:该符号的声明无平台条件、真实参与两目标构建,
	// linux 目标下作为 NonTarget 的 windowsOnly 绑定同样参与比对,
	// windows 目标下经 Bound 分类核验——两目标都必须被拒绝。
	t.Run("window method reorder on linux target (non-target binding)", func(t *testing.T) {
		c, g, link := mustSignatureBase(t)
		mutateField(t, g, "AsyncAttachWindow", func(f *GoField) {
			f.Params[2].Name, f.Params[3].Name = f.Params[3].Name, f.Params[2].Name
		})
		fl := sigField(t, g, "AsyncAttachWindow")
		mustSigFail(t, c, g, link, "api.go", fl.Line, "AsstAsyncAttachWindow",
			"parameter 3 name screencapMethod")
	})
	t.Run("window method reorder on windows target", func(t *testing.T) {
		c := mustExtract(t, positiveFiles(t), windowsTarget)
		g := mustExtractGo(t, copyRealNative(t))
		link, diags := LinkCGo(c, g, windowsTarget)
		require.Empty(t, diags, "LinkCGo unexpectedly failed: %v", diags)
		mutateField(t, g, "AsyncAttachWindow", func(f *GoField) {
			f.Params[2].Name, f.Params[3].Name = f.Params[3].Name, f.Params[2].Name
		})
		fl := sigField(t, g, "AsyncAttachWindow")
		mustSigFail(t, c, g, link, "api.go", fl.Line, "AsstAsyncAttachWindow",
			"parameter 3 name screencapMethod")
	})
}

// TestCompareSignatureCSideFailures 是 C 侧破坏向量:pointee/const/
// 指针层级与 void* 上下文的语义改变无许可映射,诊断定位到 C 声明行
// 并以 CType.String() 给出实际类型。
func TestCompareSignatureCSideFailures(t *testing.T) {
	cases := []struct {
		name      string
		symbol    string
		mutate    func(t *testing.T, c *CExtraction, g *GoExtraction)
		wantTexts []string
	}{
		{"const char* becomes const uint8_t* (pointee change)", "AsstGetVersion",
			func(t *testing.T, c *CExtraction, g *GoExtraction) {
				mutateCFunc(t, c, "AsstGetVersion", func(f *CFunction) {
					f.Ret = sigCPtr(CType{Kind: CKindScalar, Name: "uint8", Bits: 8, Const: true})
				})
			}, []string{"const uint8*", "no approved go mapping for this c return type"}},
		{"char* becomes char** (pointer level)", "AsstGetUUID",
			func(t *testing.T, c *CExtraction, g *GoExtraction) {
				mutateCFunc(t, c, "AsstGetUUID", func(f *CFunction) {
					f.Params[1].Type = sigCPtr(sigCPtr(sigCScalar("char", 8, false)))
				})
			}, []string{"char**", "parameter 2 (buff)"}},
		{"void* buff outside the image context", "AsstGetUUID",
			func(t *testing.T, c *CExtraction, g *GoExtraction) {
				mutateCFunc(t, c, "AsstGetUUID", func(f *CFunction) {
					f.Params[1].Type = sigCPtr(CType{Kind: CKindVoid})
				})
			}, []string{"void*", "unapproved pointer uses"}},
		{"void* hwnd on a non-window symbol", "AsstDestroy",
			func(t *testing.T, c *CExtraction, g *GoExtraction) {
				mutateCFunc(t, c, "AsstDestroy", func(f *CFunction) {
					f.Params[0].Name = "hwnd"
					f.Params[0].Type = sigCPtr(CType{Kind: CKindVoid})
				})
				mutateField(t, g, "Destroy", func(f *GoField) { f.Params[0].Name = "hwnd" })
			}, []string{"void*", "unapproved pointer uses"}},
		{"renamed image buffer loses the licensed context", "AsstGetImage",
			func(t *testing.T, c *CExtraction, g *GoExtraction) {
				mutateCFunc(t, c, "AsstGetImage", func(f *CFunction) { f.Params[1].Name = "data" })
				mutateField(t, g, "GetImage", func(f *GoField) { f.Params[1].Name = "data" })
			}, []string{"void*", "no approved go mapping"}},
		{"const scalar parameter", "AsstSetStaticOption",
			func(t *testing.T, c *CExtraction, g *GoExtraction) {
				mutateCFunc(t, c, "AsstSetStaticOption", func(f *CFunction) {
					f.Params[0].Type = CType{Kind: CKindScalar, Name: "int32", Bits: 32, Signed: true, Const: true}
				})
			}, []string{"const int32", "const scalars"}},
		{"char scalar parameter", "AsstSetUserDir",
			func(t *testing.T, c *CExtraction, g *GoExtraction) {
				mutateCFunc(t, c, "AsstSetUserDir", func(f *CFunction) {
					f.Params[0].Type = sigCScalar("char", 8, false)
				})
			}, []string{"char", "unsupported widths"}},
		{"uint8 pointee parameter", "AsstLog",
			func(t *testing.T, c *CExtraction, g *GoExtraction) {
				mutateCFunc(t, c, "AsstLog", func(f *CFunction) {
					f.Params[0].Type = sigCPtr(sigCScalar("uint8", 8, false))
				})
			}, []string{"uint8*", "parameter 1 (level)"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, g, link := mustSignatureBase(t)
			tc.mutate(t, c, g)
			cf := findFunction(t, c.Functions, tc.symbol)
			mustSigFail(t, c, g, link, FileAsstCaller, cf.Line, tc.symbol, tc.wantTexts...)
		})
	}
}

// TestCompareSignatureCharPointeeConstFailures 钉死的
// "改变 const" 反例:C 侧 char pointee 丢 const(UTF-8 输入参数、
// callback 的 details_json、AsstGetVersion 返回值)与 AsstGetUUID
// buff 加 const 都拒绝,诊断定位到 C 声明行,expected/actual 以冻结
// 与实际拼写显式给出两侧 const 语义。callback typedef 的拼写冻结在
// c-syntax 层,真实 C 输入到不了比对,故该向量直接改动提取结果,
// 钉死比对层自身的规则(两 callback 共用同一 C typedef,诊断按
// callbackCases 顺序以 unixCallback 报出)。
func TestCompareSignatureCharPointeeConstFailures(t *testing.T) {
	cases := []struct {
		name         string
		symbol       string
		mutate       func(t *testing.T, c *CExtraction)
		wantExpected string
		wantActual   string
	}{
		{"append task type drops const", "AsstAppendTask",
			func(t *testing.T, c *CExtraction) {
				mutateCFunc(t, c, "AsstAppendTask", func(f *CFunction) { f.Params[1].Type.Pointee.Const = false })
			},
			"const char*", "char*"},
		{"get uuid buff gains const", "AsstGetUUID",
			func(t *testing.T, c *CExtraction) {
				mutateCFunc(t, c, "AsstGetUUID", func(f *CFunction) { f.Params[1].Type.Pointee.Const = true })
			},
			"char*", "const char*"},
		{"get version return drops const", "AsstGetVersion",
			func(t *testing.T, c *CExtraction) {
				mutateCFunc(t, c, "AsstGetVersion", func(f *CFunction) { f.Ret.Pointee.Const = false })
			},
			"const char*", "char*"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, g, link := mustSignatureBase(t)
			tc.mutate(t, c)
			cf := findFunction(t, c.Functions, tc.symbol)
			d := mustSigFail(t, c, g, link, FileAsstCaller, cf.Line, tc.symbol,
				tc.wantExpected, tc.wantActual, "char pointee constness")
			assert.False(t, d.Expected != tc.wantExpected || d.Actual != tc.wantActual,
				"expected/actual = %q/%q, want %q/%q",
				d.Expected, d.Actual, tc.wantExpected, tc.wantActual)
		})
	}

	t.Run("callback details_json drops const", func(t *testing.T) {
		c, g, link := mustSignatureBase(t)
		var td *CTypedef
		for i := range c.Typedefs {
			if c.Typedefs[i].Name == "AsstApiCallback" {
				td = &c.Typedefs[i]
			}
		}
		require.NotNil(t, td, "AsstApiCallback typedef missing from the c extraction")
		td.Type.Sig.Params[1].Type.Pointee.Const = false
		d := mustSigFail(t, c, g, link, FileAsstCaller, td.Line, "unixCallback",
			"const char*", "char*", "parameter 2 (details_json)", "char pointee constness")
		assert.False(t, d.Expected != "const char*" || d.Actual != "char*",
			"expected/actual = %q/%q, want %q/%q", d.Expected, d.Actual, "const char*", "char*")
	})
}

// ---- 失败向量:callback 平台适配 ----

// TestCompareSignatureCallbackFailures 是 callback 侧破坏向量:平台
// 适配规则(unix 无返回、windows 恰返回 uintptr、参数逐位置具名映
// 射)被破坏时诊断定位到 callback_types.go 的类型声明行。
func TestCompareSignatureCallbackFailures(t *testing.T) {
	cases := []struct {
		name      string
		mutate    func(t *testing.T, g *GoExtraction) (string, int, string)
		wantTexts []string
	}{
		{"unix callback gains a return", func(t *testing.T, g *GoExtraction) (string, int, string) {
			line := g.UnixCallback.Line
			g.UnixCallback.Result = "uintptr"
			return "callback_types.go", line, "unixCallback"
		}, []string{"unix adaptation", `""`}},
		{"windows callback loses its return", func(t *testing.T, g *GoExtraction) (string, int, string) {
			line := g.WindowsCallback.Line
			g.WindowsCallback.Result = ""
			return "callback_types.go", line, "windowsCallback"
		}, []string{"constant 0 uintptr"}},
		{"windows callback returns non-uintptr", func(t *testing.T, g *GoExtraction) (string, int, string) {
			line := g.WindowsCallback.Line
			g.WindowsCallback.Result = "int32"
			return "callback_types.go", line, "windowsCallback"
		}, []string{"uintptr", "constant 0"}},
		{"callback parameter reorder", func(t *testing.T, g *GoExtraction) (string, int, string) {
			line := g.UnixCallback.Line
			p := g.UnixCallback.Params
			p[1].Name, p[2].Name = p[2].Name, p[1].Name
			return "callback_types.go", line, "unixCallback"
		}, []string{"parameter 2 name detailsJSON"}},
		{"callback parameter type error", func(t *testing.T, g *GoExtraction) (string, int, string) {
			line := g.UnixCallback.Line
			g.UnixCallback.Params[0].Type = "uint32"
			return "callback_types.go", line, "unixCallback"
		}, []string{"parameter 1 (msg)", "int32"}},
		{"callback parameter unapproved rename", func(t *testing.T, g *GoExtraction) (string, int, string) {
			line := g.UnixCallback.Line
			g.UnixCallback.Params[0].Name = "message"
			return "callback_types.go", line, "unixCallback"
		}, []string{"parameter 1 name msg"}},
		{"windows callback parameter type error", func(t *testing.T, g *GoExtraction) (string, int, string) {
			line := g.WindowsCallback.Line
			g.WindowsCallback.Params[1].Type = "uint8"
			return "callback_types.go", line, "windowsCallback"
		}, []string{"parameter 2 (details_json)", "*byte"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, g, link := mustSignatureBase(t)
			wantFile, wantLine, wantSymbol := tc.mutate(t, g)
			mustSigFail(t, c, g, link, wantFile, wantLine, wantSymbol, tc.wantTexts...)
		})
	}
}

// TestCompareSignatureNilInputs 防御性输入:任何提取/关联结果缺失即
// 失败关闭,绝不静默跳过。
func TestCompareSignatureNilInputs(t *testing.T) {
	c, g, link := mustSignatureBase(t)
	for name, fn := range map[string]func() (*SignatureResult, []Diagnostic){
		"nil c":    func() (*SignatureResult, []Diagnostic) { return CompareSignatures(nil, g, link) },
		"nil go":   func() (*SignatureResult, []Diagnostic) { return CompareSignatures(c, nil, link) },
		"nil link": func() (*SignatureResult, []Diagnostic) { return CompareSignatures(c, g, nil) },
		"all nil":  func() (*SignatureResult, []Diagnostic) { return CompareSignatures(nil, nil, nil) },
		"nil callbacks": func() (*SignatureResult, []Diagnostic) {
			g2 := mustExtractGo(t, copyRealNative(t))
			g2.UnixCallback = nil
			return CompareSignatures(c, g2, link)
		},
	} {
		t.Run(name, func(t *testing.T) {
			res, diags := fn()
			// 失败路径的 res 为 nil,符号计数须留在原 if 守卫内惰性求值。
			if len(diags) == 0 {
				require.FailNowf(t, "unexpectedly passed",
					"%s unexpectedly passed with %d symbols", name, len(res.Symbols))
			}
			assert.Equal(t, CatSignature, diags[0].Category,
				"category = %q, want %q", diags[0].Category, CatSignature)
		})
	}
}

// ---- 端到端:完整命令入口的破坏变体 ----

// TestRealNativeSignatureVariantsFailThroughCLI 是签名比对的端到端
// 验收:Go 侧与 C 侧破坏变体都走完整命令入口 Run,断言非零退出、
// signature 类别、符号定位与绝不输出 PASS。C 侧变体经测试 profile
// 注册表为改写后的合成头固定独立摘要。
func TestRealNativeSignatureVariantsFailThroughCLI(t *testing.T) {
	cases := []struct {
		name    string
		mutateC func(t *testing.T, fs map[string]string) // nil = 正例 C 输入
		mutate  func(t *testing.T, fs map[string]string)
		wantLoc string
	}{
		{"image buffer widened to uintptr", nil, func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"buff unsafe.Pointer", "buff uintptr")
		}, "AsstGetImage"},
		{"same-type parameter reorder", nil, func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"x int32, y int32", "y int32, x int32")
		}, "AsstAsyncClick"},
		{"uint64 buff size narrowed to uintptr", nil, func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"buff *byte, buffSize uint64) uint64", "buff *byte, buffSize uintptr) uint64")
		}, "AsstGetUUID"},
		{"unapproved parameter rename", nil, func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"taskType *byte, params *byte) int32", "taskType *byte, param *byte) int32")
		}, "AsstAppendTask"},
		{"c-side pointee change on AsstLog", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"AsstLog(const char* level", "AsstLog(const uint8_t* level")
		}, func(t *testing.T, fs map[string]string) {}, "AsstLog"},
		{"c-side dropped const on AsstAppendTask", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"AsstAppendTask(AsstHandle handle, const char* type",
				"AsstAppendTask(AsstHandle handle, char* type")
		}, func(t *testing.T, fs map[string]string) {}, "AsstAppendTask"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cFiles := positiveFiles(t)
			if tc.mutateC != nil {
				tc.mutateC(t, cFiles)
			}
			registerTestProfile(t, testProfileFromFiles(cFiles))
			cInput := writeCInputDir(t, cFiles, nil)
			files := copyRealNative(t)
			tc.mutate(t, files)
			goRoot := writeMutatedGoRoot(t, files)
			var stdout, stderr bytes.Buffer
			code := Run([]string{
				"--goos", "linux", "--goarch", "amd64",
				"--c-input", cInput, "--go-root", goRoot,
			}, &stdout, &stderr)
			require.NotEqual(t, 0, code, "mutated variant unexpectedly passed:\n%s", stdout.String())
			assert.False(t, stdout.Len() != 0 || strings.Contains(stdout.String()+stderr.String(), "PASS"),
				"failure output must stay on stderr without PASS:\nstdout=%q stderr=%q",
				stdout.String(), stderr.String())
			errOut := stderr.String()
			assert.Contains(t, errOut, CatSignature, "stderr must carry the %q category:\n%s", CatSignature, errOut)
			assert.Contains(t, errOut, tc.wantLoc, "stderr must locate %q:\n%s", tc.wantLoc, errOut)
			assert.True(t, strings.Contains(errOut, testProfileID) && strings.Contains(errOut, "(linux, amd64)"),
				"stderr must print the profile and target header:\n%s", errOut)
		})
	}
}

// TestCallbackCVariantsRejectedAtCSyntax 钉死 C 侧 callback 破坏的拒
// 绝归属:c callback 从 void 改非 void 与丢 ASST_CALL 都在 c-syntax
// 层被冻结拼写检查拒绝,到不了签名比对。
func TestCallbackCVariantsRejectedAtCSyntax(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(t *testing.T, fs map[string]string)
	}{
		{"callback return changed from void", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"typedef void(ASST_CALL* AsstApiCallback)",
				"typedef uint8_t(ASST_CALL* AsstApiCallback)")
		}},
		{"callback drops ASST_CALL", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"typedef void(ASST_CALL* AsstApiCallback)",
				"typedef void(* AsstApiCallback)")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := positiveFiles(t)
			tc.mutate(t, files)
			dir, _ := writeCInput(t, files)
			input := testLoadCInput(t, dir)
			_, diags := ExtractC(input, linuxTarget)
			require.NotEmpty(t, diags, "ExtractC unexpectedly passed; the c callback variant must be rejected")
			assert.Equal(t, CatCSyntax, diags[0].Category,
				"category = %q, want %q", diags[0].Category, CatCSyntax)
		})
	}
}
