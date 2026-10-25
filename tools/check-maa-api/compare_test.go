// compare_test.go 校验宽度级 C↔Go 逐参数语义比对与 callback 平台适
// 配比对:冻结参数名映射与类型映射表逐条钉死(接受侧含 byte 别名,
// 拒绝侧含设计 §9.1.2 的全部宽度/指针/角色向量,含 char pointee 的
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
	"encoding/json"
	"strings"
	"testing"
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
// 后的比对基线(linux 目标,25 条 Bound)。
func mustSignatureBase(t *testing.T) (*CExtraction, *GoExtraction, *LinkResult) {
	t.Helper()
	c := mustExtract(t, positiveFiles(t), linuxTarget)
	g := mustExtractGo(t, copyRealNative(t))
	link, diags := LinkCGo(c, g, linuxTarget)
	if len(diags) != 0 {
		t.Fatalf("LinkCGo unexpectedly failed: %v", diags)
	}
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
	t.Fatalf("api field %s not found", name)
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
	t.Fatalf("c function %s not found", name)
}

// mustSigFail 断言签名比对以 signature 类诊断失败,且诊断定位到期望
// 的文件/行号/符号并含全部期望片段。
func mustSigFail(t *testing.T, c *CExtraction, g *GoExtraction, link *LinkResult,
	wantFile string, wantLine int, wantSymbol string, wantTexts ...string) Diagnostic {
	t.Helper()
	res, diags := CompareSignatures(c, g, link)
	if len(diags) == 0 {
		t.Fatalf("CompareSignatures unexpectedly passed (%d symbols); want failure mentioning %v",
			len(res.Symbols), wantTexts)
	}
	d := diags[0]
	if d.Category != CatSignature {
		t.Errorf("category = %q, want %q", d.Category, CatSignature)
	}
	if d.File != wantFile || (wantLine > 0 && d.Line != wantLine) {
		t.Errorf("location = %s:%d, want %s:%d", d.File, d.Line, wantFile, wantLine)
	}
	if d.Symbol != wantSymbol {
		t.Errorf("symbol = %q, want %q", d.Symbol, wantSymbol)
	}
	joined := d.Expected + "\n" + d.Actual + "\n" + d.Message
	for _, want := range wantTexts {
		if !strings.Contains(joined, want) {
			t.Errorf("diagnostic %s does not mention %q", d.String(), want)
		}
	}
	return d
}

// ---- 冻结映射表钉死 ----

// TestApprovedParamNameMapping 钉死设计 §3 冻结的 8 个参数名重命名,
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
	if len(approvedParamNames) != len(want) {
		t.Fatalf("approved param names = %d entries, want %d", len(approvedParamNames), len(want))
	}
	for c, g := range want {
		if got := approvedParamName(c); got != g {
			t.Errorf("approvedParamName(%q) = %q, want %q", c, got, g)
		}
	}
	// 未批准重命名的名字必须原样同名。
	for _, c := range []string{"handle", "path", "key", "value", "id", "params", "block",
		"x", "y", "buff", "name", "level", "message", "msg", "address", "config",
		"callback", "hwnd", "screencap", "details", "customArg"} {
		if got := approvedParamName(c); got != c {
			t.Errorf("approvedParamName(%q) = %q, want the identity mapping", c, got)
		}
	}
}

// TestPermittedGoTypesTable 逐条钉死类型映射表:接受侧给出许可拼写
// (固定顺序),拒绝侧为空——覆盖 §9.1.2 的宽度/指针/角色拒绝向量
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
			if len(got) != len(tc.want) {
				t.Fatalf("permittedGoTypes = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("permittedGoTypes = %v, want %v (order is part of the frozen table)", got, tc.want)
				}
			}
		})
	}
}

// ---- 正例:冻结映射与生产声明逐字段一致 ----

// TestCompareSignaturesPositiveRealNative 用真实生产 native 包加合成
// C 正例证明冻结映射与生产 api.go/callback_types.go 逐字段一致:两
// 目标全部 Bound 条目通过,符号按名排序,callback 两条均核验;关键映
// 射逐点抽查。byte 别名同样接受。
func TestCompareSignaturesPositiveRealNative(t *testing.T) {
	c, g, link := mustSignatureBase(t)
	res, diags := CompareSignatures(c, g, link)
	if len(diags) != 0 {
		t.Fatalf("CompareSignatures unexpectedly failed: %v", diags)
	}
	if len(res.Symbols) != len(link.Bound) {
		t.Fatalf("symbols = %d, want %d (one entry per bound symbol)", len(res.Symbols), len(link.Bound))
	}
	for i, e := range res.Symbols {
		if e.Symbol != link.Bound[i].Symbol {
			t.Fatalf("symbols[%d] = %q, want %q (must follow the sorted bound order)",
				i, e.Symbol, link.Bound[i].Symbol)
		}
	}
	entry := func(symbol string) SignatureEntry {
		t.Helper()
		for _, e := range res.Symbols {
			if e.Symbol == symbol {
				return e
			}
		}
		t.Fatalf("signature entry %s not found", symbol)
		return SignatureEntry{}
	}

	// 图像缓冲区:C void* ↔ unsafe.Pointer;容量/返回 uint64。
	gi := entry("AsstGetImage")
	if gi.Field != "GetImage" || gi.GoFile != "api.go" || gi.GoLine <= 0 ||
		gi.Header != FileAsstCaller || gi.CLine <= 0 {
		t.Errorf("AsstGetImage record = %+v", gi)
	}
	if p := gi.Params[1]; p.Position != 2 || p.CName != "buff" || p.GoName != "buff" ||
		p.CType != "void*" || p.GoType != "unsafe.Pointer" {
		t.Errorf("AsstGetImage buff = %+v", p)
	}
	if p := gi.Params[2]; p.CName != "buff_size" || p.GoName != "buffSize" ||
		p.CType != "uint64" || p.GoType != "uint64" {
		t.Errorf("AsstGetImage buff_size = %+v", p)
	}
	if gi.Return != (SignatureReturn{CType: "uint64", GoType: "uint64"}) {
		t.Errorf("AsstGetImage return = %+v", gi.Return)
	}
	// 批准重命名:type→taskType;opaque handle 与 callback 函数指针。
	at := entry("AsstAppendTask")
	if p := at.Params[1]; p.CName != "type" || p.GoName != "taskType" ||
		p.CType != "const char*" || p.GoType != "*byte" {
		t.Errorf("AsstAppendTask type = %+v", p)
	}
	ce := entry("AsstCreateEx")
	if p := ce.Params[0]; p.CName != "callback" || p.GoName != "callback" ||
		p.GoType != "uintptr" || !strings.HasPrefix(p.CType, "void(") {
		t.Errorf("AsstCreateEx callback = %+v", p)
	}
	if p := ce.Params[1]; p.CName != "custom_arg" || p.GoName != "customArg" ||
		p.CType != "void*" || p.GoType != "uintptr" {
		t.Errorf("AsstCreateEx custom_arg = %+v", p)
	}
	if ce.Return.GoType != "uintptr" {
		t.Errorf("AsstCreateEx return = %+v", ce.Return)
	}
	de := entry("AsstDestroy")
	if p := de.Params[0]; p.CName != "handle" || p.GoName != "handle" ||
		p.CType != "struct AsstExtAPI*" || p.GoType != "uintptr" {
		t.Errorf("AsstDestroy handle = %+v", p)
	}
	tl := entry("AsstGetTasksList")
	if p := tl.Params[1]; p.CType != "int32*" || p.GoType != "*int32" {
		t.Errorf("AsstGetTasksList buff = %+v", p)
	}
	// void 返回与 const char* 返回。
	lg := entry("AsstLog")
	if lg.Return != (SignatureReturn{CType: "void", GoType: ""}) {
		t.Errorf("AsstLog return = %+v", lg.Return)
	}
	gv := entry("AsstGetVersion")
	if gv.Return != (SignatureReturn{CType: "const char*", GoType: "*byte"}) {
		t.Errorf("AsstGetVersion return = %+v", gv.Return)
	}

	// callback 平台适配:unix 原样 void,windows 许可适配 uintptr;
	// 参数逐位置核验(批准映射 details_json→detailsJSON、
	// custom_arg→customArg)。
	if len(res.Callbacks) != 2 {
		t.Fatalf("callbacks = %d, want 2", len(res.Callbacks))
	}
	uc, wc := res.Callbacks[0], res.Callbacks[1]
	if uc.Name != "unixCallback" || uc.GoFile != "callback_types.go" || uc.GoLine <= 0 {
		t.Errorf("unixCallback record = %+v", uc)
	}
	wantParams := []SignatureParam{
		{Position: 1, CName: "msg", GoName: "msg", CType: "int32", GoType: "int32"},
		{Position: 2, CName: "details_json", GoName: "detailsJSON", CType: "const char*", GoType: "*byte"},
		{Position: 3, CName: "custom_arg", GoName: "customArg", CType: "void*", GoType: "uintptr"},
	}
	for i, w := range wantParams {
		if uc.Params[i] != w {
			t.Errorf("unixCallback param %d = %+v, want %+v", i, uc.Params[i], w)
		}
		if wc.Params[i] != w {
			t.Errorf("windowsCallback param %d = %+v, want %+v", i, wc.Params[i], w)
		}
	}
	if uc.Return != (SignatureReturn{CType: "void", GoType: ""}) {
		t.Errorf("unixCallback return = %+v", uc.Return)
	}
	if wc.Name != "windowsCallback" || wc.Return != (SignatureReturn{CType: "void", GoType: "uintptr"}) {
		t.Errorf("windowsCallback record = %+v", wc)
	}

	// byte 别名接受:把 uint8 位置的拼写换成 byte 仍通过。
	mutateField(t, g, "SetUserDir", func(f *GoField) { f.Result = "byte" })
	mutateField(t, g, "AsyncClick", func(f *GoField) { f.Params[3].Type = "byte" })
	if _, diags := CompareSignatures(c, g, link); len(diags) != 0 {
		t.Fatalf("byte alias must be accepted: %v", diags)
	}

	// windows 目标:26 条绑定全部通过,含窗口符号的 hwnd/method 映射。
	cw := mustExtract(t, positiveFiles(t), windowsTarget)
	linkw, diags := LinkCGo(cw, g, windowsTarget)
	if len(diags) != 0 {
		t.Fatalf("windows LinkCGo unexpectedly failed: %v", diags)
	}
	resw, diags := CompareSignatures(cw, g, linkw)
	if len(diags) != 0 {
		t.Fatalf("windows CompareSignatures unexpectedly failed: %v", diags)
	}
	if len(resw.Symbols) != 26 {
		t.Fatalf("windows symbols = %d, want 26", len(resw.Symbols))
	}
	var aw *SignatureEntry
	for i := range resw.Symbols {
		if resw.Symbols[i].Symbol == "AsstAsyncAttachWindow" {
			aw = &resw.Symbols[i]
		}
	}
	if aw == nil {
		t.Fatal("windows signatures must cover AsstAsyncAttachWindow")
	}
	if p := aw.Params[1]; p.CName != "hwnd" || p.GoName != "hwnd" ||
		p.CType != "void*" || p.GoType != "uintptr" {
		t.Errorf("AsstAsyncAttachWindow hwnd = %+v", p)
	}
	if p := aw.Params[2]; p.CName != "screencap_method" || p.GoName != "screencapMethod" ||
		p.CType != "uint64" || p.GoType != "uint64" {
		t.Errorf("AsstAsyncAttachWindow screencap_method = %+v", p)
	}
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

	// 窗口 method 调序:linux 目标下该符号是非目标绑定,不参与比对;
	// 在 windows 目标下必须被拒绝。
	t.Run("window method reorder on windows target", func(t *testing.T) {
		c := mustExtract(t, positiveFiles(t), windowsTarget)
		g := mustExtractGo(t, copyRealNative(t))
		link, diags := LinkCGo(c, g, windowsTarget)
		if len(diags) != 0 {
			t.Fatalf("LinkCGo unexpectedly failed: %v", diags)
		}
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

// TestCompareSignatureCharPointeeConstFailures 钉死设计 §9.1.2 的
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
			if d.Expected != tc.wantExpected || d.Actual != tc.wantActual {
				t.Errorf("expected/actual = %q/%q, want %q/%q",
					d.Expected, d.Actual, tc.wantExpected, tc.wantActual)
			}
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
		if td == nil {
			t.Fatal("AsstApiCallback typedef missing from the c extraction")
		}
		td.Type.Sig.Params[1].Type.Pointee.Const = false
		d := mustSigFail(t, c, g, link, FileAsstCaller, td.Line, "unixCallback",
			"const char*", "char*", "parameter 2 (details_json)", "char pointee constness")
		if d.Expected != "const char*" || d.Actual != "char*" {
			t.Errorf("expected/actual = %q/%q, want %q/%q", d.Expected, d.Actual, "const char*", "char*")
		}
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
			if len(diags) == 0 {
				t.Fatalf("%s unexpectedly passed with %d symbols", name, len(res.Symbols))
			}
			if diags[0].Category != CatSignature {
				t.Errorf("category = %q, want %q", diags[0].Category, CatSignature)
			}
		})
	}
}

// ---- 端到端:成功报告的 signatures 段 ----

// reportSignatureSymbol 从成功报告中取一个符号的核验记录。
func reportSignatureSymbol(t *testing.T, r reportJSON, symbol string) signatureSymbolJSON {
	t.Helper()
	for _, s := range r.Signatures.Symbols {
		if s.Symbol == symbol {
			return s
		}
	}
	t.Fatalf("signature symbol %s missing from the report", symbol)
	return signatureSymbolJSON{}
}

// reportSignatureCallback 从成功报告中取一个 callback 的核验记录。
func reportSignatureCallback(t *testing.T, r reportJSON, name string) signatureCallbackJSON {
	t.Helper()
	for _, c := range r.Signatures.Callbacks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("signature callback %s missing from the report", name)
	return signatureCallbackJSON{}
}

// assertReportParam 断言报告中一个参数位置的核验记录。
func assertReportParam(t *testing.T, owner string, p signatureParamJSON,
	position int, cName, goName, cType, goType string) {
	t.Helper()
	want := signatureParamJSON{Position: position, CName: cName, GoName: goName,
		CType: cType, GoType: goType}
	if p != want {
		t.Errorf("%s param %d = %+v, want %+v", owner, position, p, want)
	}
}

// TestRealNativeSignatureBaseline 通过完整命令入口核验成功报告的
// signatures 段:真实生产树加合成 C 输入,两目标的符号数与排序、
// callback 记录与关键映射逐点一致;同参数两次运行 stdout 逐字节
// 一致(段内键序稳定)。
func TestRealNativeSignatureBaseline(t *testing.T) {
	cFiles := positiveFiles(t)
	registerTestProfile(t, testProfileFromFiles(cFiles))
	cInput := writeCInputDir(t, cFiles, nil)
	goRoot := repoRoot(t)

	run := func(goos string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := Run([]string{"--profile", testProfileID, "--goos", goos, "--goarch", "amd64",
			"--c-input", cInput, "--go-root", goRoot}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%s run exited %d:\n%s", goos, code, stderr.String())
		}
		return stdout.String()
	}
	parse := func(out, goos string) reportJSON {
		t.Helper()
		var r reportJSON
		if err := json.Unmarshal([]byte(out), &r); err != nil {
			t.Fatalf("%s stdout is not valid JSON: %v", goos, err)
		}
		return r
	}

	linuxOut := run("linux")
	if again := run("linux"); again != linuxOut {
		t.Error("success output including the signatures section must be byte-identical across runs")
	}
	linuxRep := parse(linuxOut, "linux")
	if len(linuxRep.Signatures.Symbols) != 25 {
		t.Fatalf("linux signature symbols = %d, want 25", len(linuxRep.Signatures.Symbols))
	}
	for i := 1; i < len(linuxRep.Signatures.Symbols); i++ {
		if linuxRep.Signatures.Symbols[i-1].Symbol >= linuxRep.Signatures.Symbols[i].Symbol {
			t.Errorf("signature symbols must be strictly sorted: %q >= %q",
				linuxRep.Signatures.Symbols[i-1].Symbol, linuxRep.Signatures.Symbols[i].Symbol)
		}
	}
	gi := reportSignatureSymbol(t, linuxRep, "AsstGetImage")
	assertReportParam(t, "AsstGetImage", gi.Params[1], 2, "buff", "buff", "void*", "unsafe.Pointer")
	assertReportParam(t, "AsstGetImage", gi.Params[2], 3, "buff_size", "buffSize", "uint64", "uint64")
	if gi.Return != (signatureReturnJSON{CType: "uint64", GoType: "uint64"}) {
		t.Errorf("AsstGetImage return = %+v", gi.Return)
	}
	if gi.CHeader != FileAsstCaller || gi.CLine <= 0 || gi.GoFile != "api.go" || gi.GoLine <= 0 {
		t.Errorf("AsstGetImage locations = %+v", gi)
	}
	lg := reportSignatureSymbol(t, linuxRep, "AsstLog")
	if lg.Return != (signatureReturnJSON{CType: "void", GoType: ""}) {
		t.Errorf("AsstLog return = %+v", lg.Return)
	}

	if len(linuxRep.Signatures.Callbacks) != 2 {
		t.Fatalf("callbacks = %d, want 2", len(linuxRep.Signatures.Callbacks))
	}
	uc := reportSignatureCallback(t, linuxRep, "unixCallback")
	assertReportParam(t, "unixCallback", uc.Params[1], 2, "details_json", "detailsJSON", "const char*", "*byte")
	assertReportParam(t, "unixCallback", uc.Params[2], 3, "custom_arg", "customArg", "void*", "uintptr")
	if uc.Return != (signatureReturnJSON{CType: "void", GoType: ""}) {
		t.Errorf("unixCallback return = %+v", uc.Return)
	}
	wc := reportSignatureCallback(t, linuxRep, "windowsCallback")
	if wc.Return != (signatureReturnJSON{CType: "void", GoType: "uintptr"}) {
		t.Errorf("windowsCallback return = %+v", wc.Return)
	}

	winRep := parse(run("windows"), "windows")
	if len(winRep.Signatures.Symbols) != 26 {
		t.Fatalf("windows signature symbols = %d, want 26", len(winRep.Signatures.Symbols))
	}
	aw := reportSignatureSymbol(t, winRep, "AsstAsyncAttachWindow")
	assertReportParam(t, "AsstAsyncAttachWindow", aw.Params[1], 2, "hwnd", "hwnd", "void*", "uintptr")
	assertReportParam(t, "AsstAsyncAttachWindow", aw.Params[2], 3,
		"screencap_method", "screencapMethod", "uint64", "uint64")
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
				"--profile", testProfileID, "--goos", "linux", "--goarch", "amd64",
				"--c-input", cInput, "--go-root", goRoot,
			}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("mutated variant unexpectedly passed:\n%s", stdout.String())
			}
			if stdout.Len() != 0 || strings.Contains(stdout.String()+stderr.String(), "PASS") {
				t.Errorf("failure output must stay on stderr without PASS:\nstdout=%q stderr=%q",
					stdout.String(), stderr.String())
			}
			errOut := stderr.String()
			if !strings.Contains(errOut, CatSignature) {
				t.Errorf("stderr must carry the %q category:\n%s", CatSignature, errOut)
			}
			if !strings.Contains(errOut, tc.wantLoc) {
				t.Errorf("stderr must locate %q:\n%s", tc.wantLoc, errOut)
			}
			if !strings.Contains(errOut, testProfileID) || !strings.Contains(errOut, "(linux, amd64)") {
				t.Errorf("stderr must print the profile and target header:\n%s", errOut)
			}
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
			if len(diags) == 0 {
				t.Fatal("ExtractC unexpectedly passed; the c callback variant must be rejected")
			}
			if diags[0].Category != CatCSyntax {
				t.Errorf("category = %q, want %q", diags[0].Category, CatCSyntax)
			}
		})
	}
}
