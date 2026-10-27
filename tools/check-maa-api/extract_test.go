// extract_test.go 覆盖受限 C 提取:真实形状正例(Port 全部宏与条件、
// typedef 链、函数指针、GetVersion 拼写、deprecated 注释、Windows
// enum、Extra 结构体)、Linux/Windows 两种目标的不同解析结果、
// ASST_WITH_EXTRA_CALLERS 0/1 两种发现,以及的 C 提取
// 失败向量。全部 fixture 为 testdata 下的合成头文件,自包含。

package main

import (
	"strings"
	"testing"
)

var (
	linuxTarget   = Target{GOOS: "linux", GOARCH: "amd64"}
	windowsTarget = Target{GOOS: "windows", GOARCH: "amd64"}
)

func functionByName(fns []CFunction, name string) *CFunction {
	for i := range fns {
		if fns[i].Name == name {
			return &fns[i]
		}
	}
	return nil
}

func findFunction(t *testing.T, fns []CFunction, name string) CFunction {
	t.Helper()
	f := functionByName(fns, name)
	if f == nil {
		t.Fatalf("function %s not found", name)
	}
	return *f
}

func findTypedef(t *testing.T, tds []CTypedef, name string) CTypedef {
	t.Helper()
	for _, td := range tds {
		if td.Name == name {
			return td
		}
	}
	t.Fatalf("typedef %s not found", name)
	return CTypedef{}
}

func findEnum(t *testing.T, enums []CEnum, tag string) CEnum {
	t.Helper()
	for _, e := range enums {
		if e.Tag == tag {
			return e
		}
	}
	t.Fatalf("enum %s not found", tag)
	return CEnum{}
}

func mustExtract(t *testing.T, files map[string]string, target Target) *CExtraction {
	t.Helper()
	dir, _ := writeCInput(t, files)
	input := testLoadCInput(t, dir)
	res, diags := ExtractC(input, target)
	if len(diags) != 0 {
		t.Fatalf("ExtractC unexpectedly failed: %v", diags)
	}
	return res
}

func TestExtractLinuxPositive(t *testing.T) {
	files := positiveFiles(t)
	// (void) 的规范零参数形式对任何函数可识别。
	files[FileAsstCaller] = mustReplace(t, files[FileAsstCaller],
		"AsstHandle ASSTAPI AsstCreate();", "AsstHandle ASSTAPI AsstCreate(void);")
	res := mustExtract(t, files, linuxTarget)

	if len(res.Functions) != 30 {
		t.Fatalf("full set has %d functions, want 30", len(res.Functions))
	}
	if len(res.Applicable) != 26 {
		t.Fatalf("applicable set on linux has %d functions, want 26", len(res.Applicable))
	}
	if len(res.Discovery) != 2 {
		t.Fatalf("discovery has %d entries, want 2", len(res.Discovery))
	}
	if len(res.Typedefs) != 11 {
		t.Fatalf("typedefs = %d, want 11", len(res.Typedefs))
	}
	if len(res.Enums) != 2 {
		t.Fatalf("enums = %d, want 2", len(res.Enums))
	}
	if len(res.Structs) != 1 {
		t.Fatalf("structs = %d, want 1", len(res.Structs))
	}

	// AsstSetUserDir:AsstBool→uint8 保留原名;const char* 参数;
	// linux 下调用约定为空、导出修饰为 visibility(default)。
	sud := findFunction(t, res.Functions, "AsstSetUserDir")
	if sud.Header != FileAsstCaller || sud.Line <= 0 {
		t.Errorf("AsstSetUserDir header/line = %s/%d", sud.Header, sud.Line)
	}
	if sud.Platform != PlatformAll {
		t.Errorf("AsstSetUserDir platform = %q, want all", sud.Platform)
	}
	if sud.CallConv != CallConvNone || sud.Linkage != LinkageVisibilityDefault {
		t.Errorf("linux modifiers = %q/%q, want empty/%q", sud.CallConv, sud.Linkage, LinkageVisibilityDefault)
	}
	wantRet := CType{Kind: CKindScalar, Name: "uint8", Bits: 8, Via: "AsstBool"}
	if !sud.Ret.Equal(wantRet) {
		t.Errorf("AsstSetUserDir ret = %s (via %s), want uint8 via AsstBool", sud.Ret.String(), sud.Ret.Via)
	}
	if len(sud.Params) != 1 || sud.Params[0].Name != "path" {
		t.Fatalf("AsstSetUserDir params = %+v", sud.Params)
	}
	wantParam := CParam{Name: "path", Type: CType{Kind: CKindPointer,
		Pointee: &CType{Kind: CKindScalar, Name: "char", Bits: 8, Const: true}}}
	if !sud.Params[0].Equal(wantParam) || sud.Params[0].Type.String() != "const char*" {
		t.Errorf("AsstSetUserDir param = %s, want const char* path", sud.Params[0].Type.String())
	}

	// 多行声明与 deprecated 注释:注释不得误作属性。
	sio := findFunction(t, res.Functions, "AsstSetInstanceOption")
	if len(sio.Params) != 3 || sio.Params[1].Name != "key" {
		t.Errorf("AsstSetInstanceOption params = %+v", sio.Params)
	}
	con := findFunction(t, res.Functions, "AsstConnect")
	if len(con.Params) != 4 || con.Params[1].Name != "adb_path" {
		t.Errorf("AsstConnect params = %+v", con.Params)
	}

	// AsstCreateEx:函数指针参数与回调签名。
	ce := findFunction(t, res.Functions, "AsstCreateEx")
	if len(ce.Params) != 2 || ce.Params[0].Name != "callback" || ce.Params[1].Name != "custom_arg" {
		t.Fatalf("AsstCreateEx params = %+v", ce.Params)
	}
	cbType := ce.Params[0].Type
	if cbType.Kind != CKindFuncPtr || cbType.Via != "AsstApiCallback" {
		t.Fatalf("callback param kind/via = %v/%q", cbType.Kind, cbType.Via)
	}
	sig := cbType.Sig
	if sig.Ret.Kind != CKindVoid || sig.CallConv != CallConvNone || len(sig.Params) != 3 {
		t.Fatalf("callback signature = %+v", sig)
	}
	if sig.Params[0].Name != "msg" || sig.Params[0].Type.String() != "int32" || sig.Params[0].Type.Via != "AsstMsgId" {
		t.Errorf("callback msg param = %+v (%s)", sig.Params[0], sig.Params[0].Type.String())
	}
	if sig.Params[1].Name != "details_json" || sig.Params[1].Type.String() != "const char*" {
		t.Errorf("callback details param = %+v", sig.Params[1])
	}
	if sig.Params[2].Name != "custom_arg" || sig.Params[2].Type.String() != "void*" {
		t.Errorf("callback custom_arg param = %+v", sig.Params[2])
	}

	// 输出缓冲区角色:void*、AsstSize、char*、AsstTaskId*。
	gi := findFunction(t, res.Functions, "AsstGetImage")
	if gi.Params[1].Type.String() != "void*" || gi.Params[1].Name != "buff" {
		t.Errorf("AsstGetImage buff = %+v", gi.Params[1])
	}
	if gi.Params[2].Type.String() != "uint64" || gi.Params[2].Type.Via != "AsstSize" {
		t.Errorf("AsstGetImage buff_size = %+v", gi.Params[2])
	}
	if gi.Ret.String() != "uint64" || gi.Ret.Via != "AsstSize" {
		t.Errorf("AsstGetImage ret = %s (via %s)", gi.Ret.String(), gi.Ret.Via)
	}
	gl := findFunction(t, res.Functions, "AsstGetTasksList")
	if gl.Params[1].Type.String() != "int32*" ||
		gl.Params[1].Type.Pointee == nil || gl.Params[1].Type.Pointee.Via != "AsstTaskId" {
		t.Errorf("AsstGetTasksList buff = %s (pointee via %s)", gl.Params[1].Type.String(),
			gl.Params[1].Type.Pointee.Via)
	}

	// void 返回。
	de := findFunction(t, res.Functions, "AsstDestroy")
	if de.Ret.Kind != CKindVoid {
		t.Errorf("AsstDestroy ret kind = %v, want void", de.Ret.Kind)
	}
	lg := findFunction(t, res.Functions, "AsstLog")
	if lg.Ret.Kind != CKindVoid || lg.CallConv != CallConvNone {
		t.Errorf("AsstLog ret/callconv = %v/%q", lg.Ret.Kind, lg.CallConv)
	}

	// GetVersion 特殊拼写。
	gv := findFunction(t, res.Functions, "AsstGetVersion")
	if gv.Linkage != LinkageVisibilityDefault || gv.CallConv != CallConvNone || len(gv.Params) != 0 {
		t.Errorf("AsstGetVersion linkage/callconv/params = %q/%q/%d",
			gv.Linkage, gv.CallConv, len(gv.Params))
	}
	if gv.Ret.String() != "const char*" || gv.Spelling != frozenGetVersionCanon {
		t.Errorf("AsstGetVersion ret/spelling = %s/%q", gv.Ret.String(), gv.Spelling)
	}

	// Windows 符号:在完整集合中标注 windows,Linux 适用集合不含它们。
	for _, name := range []string{"AsstAttachWindow", "AsstAsyncAttachWindow"} {
		aw := findFunction(t, res.Functions, name)
		if aw.Platform != PlatformWindows {
			t.Errorf("%s platform = %q, want windows", name, aw.Platform)
		}
		if functionByName(res.Applicable, name) != nil {
			t.Errorf("%s must not be applicable on linux", name)
		}
	}
	aw := findFunction(t, res.Functions, "AsstAttachWindow")
	if len(aw.Params) != 5 || aw.Params[1].Type.String() != "void*" ||
		aw.Params[2].Type.String() != "uint64" || aw.Params[2].Type.Via != "" {
		t.Errorf("AsstAttachWindow params = %+v", aw.Params)
	}

	// 实验发现:恰为两个实验导出,平台 extra,不在适用集合。
	if res.Discovery[0].Name != "AsstGetMapLevelKey" || res.Discovery[1].Name != "AsstGetItemName" {
		t.Fatalf("discovery = %v", discoveryNames(res.Discovery))
	}
	mlk := res.Discovery[0]
	if mlk.Platform != PlatformExtra {
		t.Errorf("AsstGetMapLevelKey platform = %q, want extra", mlk.Platform)
	}
	if mlk.Ret.Kind != CKindStruct || mlk.Ret.Tag != frozenStructTag {
		t.Errorf("AsstGetMapLevelKey ret = %+v, want struct %s by value", mlk.Ret, frozenStructTag)
	}
	if len(mlk.Params) != 1 || mlk.Params[0].Name != "key" || mlk.Params[0].Type.String() != "const char*" {
		t.Errorf("AsstGetMapLevelKey params = %+v", mlk.Params)
	}
	if functionByName(res.Applicable, "AsstGetMapLevelKey") != nil {
		t.Error("experimental export must not be in the applicable set")
	}

	// Extra 结构体核验:四个 const char* 成员,顺序固定。
	st := res.Structs[0]
	if st.Tag != frozenStructTag || st.Platform != PlatformExtra || len(st.Members) != 4 {
		t.Fatalf("struct = %+v", st)
	}
	for i, want := range frozenStructMembers {
		if st.Members[i].Name != want || st.Members[i].Type.String() != "const char*" {
			t.Errorf("member %d = %+v (%s), want const char* %s",
				i, st.Members[i], st.Members[i].Type.String(), want)
		}
	}

	// Windows enum 命名值提取(1 << n 展开,尾逗号容忍)。
	sc := findEnum(t, res.Enums, "AsstWin32ScreencapMethodEnum")
	wantScreencap := []CEnumValue{
		{"AsstWin32ScreencapMethod_None", 0}, {"AsstWin32ScreencapMethod_GDI", 1},
		{"AsstWin32ScreencapMethod_FramePool", 2}, {"AsstWin32ScreencapMethod_DXGI_DesktopDup", 4},
		{"AsstWin32ScreencapMethod_DXGI_DesktopDup_Window", 8},
		{"AsstWin32ScreencapMethod_PrintWindow", 16}, {"AsstWin32ScreencapMethod_ScreenDC", 32},
	}
	if len(sc.Values) != len(wantScreencap) {
		t.Fatalf("screencap enum has %d values, want %d", len(sc.Values), len(wantScreencap))
	}
	for i, w := range wantScreencap {
		if sc.Values[i] != w {
			t.Errorf("screencap value %d = %+v, want %+v", i, sc.Values[i], w)
		}
	}
	im := findEnum(t, res.Enums, "AsstWin32InputMethodEnum")
	if len(im.Values) != 12 {
		t.Fatalf("input enum has %d values, want 12", len(im.Values))
	}
	last := im.Values[len(im.Values)-1]
	if last.Name != "AsstWin32InputMethod_AnchoredTouch" || last.Value != 1024 {
		t.Errorf("last input value = %+v, want AnchoredTouch=1024", last)
	}

	// typedef 链与指针语义。
	td := findTypedef(t, res.Typedefs, "AsstTaskId")
	if !td.Type.Equal(CType{Kind: CKindScalar, Name: "int32", Bits: 32, Signed: true, Via: "AsstId"}) {
		t.Errorf("AsstTaskId type = %s (via %s), want int32 via AsstId", td.Type.String(), td.Type.Via)
	}
	h := findTypedef(t, res.Typedefs, "AsstHandle")
	if h.Type.Kind != CKindPointer || h.Type.Pointee == nil ||
		h.Type.Pointee.Kind != CKindStruct || h.Type.Pointee.Tag != "AsstExtAPI" {
		t.Errorf("AsstHandle type = %s, want pointer to struct AsstExtAPI", h.Type.String())
	}
	if h.Type.String() != "struct AsstExtAPI*" {
		t.Errorf("AsstHandle String() = %q", h.Type.String())
	}
	cbtd := findTypedef(t, res.Typedefs, "AsstApiCallback")
	if cbtd.Type.Kind != CKindFuncPtr || cbtd.Type.Sig == nil {
		t.Fatalf("AsstApiCallback typedef = %+v", cbtd.Type)
	}
	if !cbtd.Type.Sig.Equal(*cbType.Sig) {
		t.Error("AsstApiCallback typedef signature differs from the callback parameter type")
	}
}

func TestExtractWindowsPositive(t *testing.T) {
	res := mustExtract(t, positiveFiles(t), windowsTarget)

	if len(res.Functions) != 30 {
		t.Fatalf("full set has %d functions, want 30", len(res.Functions))
	}
	if len(res.Applicable) != 28 {
		t.Fatalf("applicable set on windows has %d functions, want 28", len(res.Applicable))
	}
	for _, name := range []string{"AsstAttachWindow", "AsstAsyncAttachWindow"} {
		if functionByName(res.Applicable, name) == nil {
			t.Errorf("%s must be applicable on windows", name)
		}
	}

	sud := findFunction(t, res.Functions, "AsstSetUserDir")
	if sud.CallConv != CallConvStdcall || sud.Linkage != LinkageDLLImport {
		t.Errorf("windows modifiers = %q/%q, want __stdcall/%q", sud.CallConv, sud.Linkage, LinkageDLLImport)
	}
	gv := findFunction(t, res.Functions, "AsstGetVersion")
	if gv.CallConv != CallConvStdcall || gv.Linkage != LinkageDLLImport {
		t.Errorf("AsstGetVersion windows modifiers = %q/%q", gv.CallConv, gv.Linkage)
	}
	ce := findFunction(t, res.Functions, "AsstCreateEx")
	if sig := ce.Params[0].Type.Sig; sig.CallConv != CallConvStdcall {
		t.Errorf("windows callback callconv = %q, want __stdcall", sig.CallConv)
	}
	// discovery 与 linux 一致:与目标无关。
	if len(res.Discovery) != 2 || res.Discovery[0].Name != "AsstGetMapLevelKey" {
		t.Fatalf("discovery = %v", discoveryNames(res.Discovery))
	}
}

// TestPassZeroAndHostileEnv 验证 =0 pass 恰含 28 项且无实验导出,以及
// 外部预定义 ASST_CALL 的敌意环境必须失败。
func TestPassZeroAndHostileEnv(t *testing.T) {
	dir, _ := writeCInput(t, positiveFiles(t))
	input := testLoadCInput(t, dir)

	res, d := runParsePass(input, newMacroEnv(linuxTarget, 0))
	if d != nil {
		t.Fatalf("pass0 failed: %v", d)
	}
	if len(res.Functions) != 28 {
		t.Fatalf("pass0 functions = %d, want 28", len(res.Functions))
	}
	for _, f := range res.Functions {
		if frozenExperimental[f.Name] {
			t.Errorf("pass0 unexpectedly contains experimental export %s", f.Name)
		}
	}

	// ASST_CALL 在进入 AsstPort.h 前已被定义:Port 分支被跳过,声明
	// 展开必须失败而不是沿用外部值。
	bad := macroEnv{
		defined: map[string]bool{"__x86_64__": true, "ASST_CALL": true},
		ints:    map[string]int64{"__GNUC__": 4, "ASST_WITH_EXTRA_CALLERS": 0},
	}
	_, d = runParsePass(input, bad)
	if d == nil {
		t.Fatal("runParsePass unexpectedly passed with externally predefined ASST_CALL")
	}
	if d.Category != CatCSyntax || !strings.Contains(d.Symbol, "ASST_CALL") {
		t.Errorf("hostile env diagnostic = %+v", d)
	}
}

// TestExtractFailures 是的 C 提取失败向量:每条都必须以
// c-syntax 诊断失败,且诊断指向期望符号。
func TestExtractFailures(t *testing.T) {
	cases := []struct {
		name     string
		target   Target
		mutate   func(t *testing.T, fs map[string]string)
		sym      string
		wantLine bool // 语法级诊断必须带行号;集合级缺失定位到头文件即可
	}{
		{"unknown typedef name", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"typedef uint64_t AsstSize;", "typedef uint64_t AsstSize;\ntypedef AsstSize AsstWeird;")
		}, "AsstWeird", true},
		{"typedef spelling mismatch", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"typedef uint8_t AsstBool;", "typedef uint64_t AsstBool;")
		}, "AsstBool", true},
		{"circular alias", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"typedef int32_t AsstId;", "typedef AsstMsgId AsstId;")
		}, "AsstMsgId", true},
		{"unknown condition in inactive branch", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstPort] = mustReplace(t, fs[FileAsstPort],
				"#if defined(__WINE__) && defined(__x86_64__)",
				"#if defined(__WINE__) && defined(__i386__)")
		}, "condition", true},
		{"unknown condition in inactive branch windows", windowsTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstPort] = mustReplace(t, fs[FileAsstPort],
				"#if defined(__WINE__) && defined(__x86_64__)",
				"#if defined(__WINE__) && defined(__arm__)")
		}, "condition", true},
		{"unknown ifdef in inactive branch", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"#ifdef _WIN32", "#ifdef _WIN64")
		}, "condition", true},
		{"unknown macro right value in inactive branch", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstPort] = mustReplace(t, fs[FileAsstPort],
				"#define ASST_CALL __stdcall", "#define ASST_CALL __fastcall")
		}, "ASST_CALL", true},
		{"unknown macro name", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstPort] = mustReplace(t, fs[FileAsstPort],
				"#define ASSTLOCAL ASST_DLL_LOCAL ASST_CALL", "#define ASSTLOCALX ASST_DLL_LOCAL ASST_CALL")
		}, "ASSTLOCALX", true},
		{"getversion spelling broken", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"ASSTAPI_PORT const char* ASST_CALL AsstGetVersion();",
				"ASSTAPI_PORT const char* AsstGetVersion();")
		}, "AsstGetVersion", true},
		{"getversion rewritten as standard shape", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"ASSTAPI_PORT const char* ASST_CALL AsstGetVersion();",
				"const char* ASSTAPI AsstGetVersion();")
		}, "AsstGetVersion", true},
		{"known zero-arg function gains parameter", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"AsstHandle ASSTAPI AsstCreate();", "AsstHandle ASSTAPI AsstCreate(AsstHandle handle);")
		}, "AsstCreate", true},
		{"second zero-arg function gains parameter", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"AsstSize ASSTAPI AsstGetNullSize();", "AsstSize ASSTAPI AsstGetNullSize(AsstSize out);")
		}, "AsstGetNullSize", true},
		{"unknown empty-parens function", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"AsstBool ASSTAPI AsstLoadResource(const char* path);",
				"AsstBool ASSTAPI AsstLoadResource(const char* path);\n    AsstBool ASSTAPI AsstNewThing();")
		}, "AsstNewThing", true},
		{"unknown export", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"AsstBool ASSTAPI AsstLoadResource(const char* path);",
				"AsstBool ASSTAPI AsstLoadResource(const char* path);\n    AsstBool ASSTAPI AsstNewThing(const char* x);")
		}, "AsstNewThing", true},
		{"external ASST_CALL predefine", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = "#define ASST_CALL __stdcall\n\n" + fs[FileAsstCaller]
		}, "ASST_CALL", true},
		{"enum unknown expression", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"AsstWin32ScreencapMethod_FramePool = 1 << 1,",
				"AsstWin32ScreencapMethod_FramePool = 1 | 2,")
		}, "AsstWin32ScreencapMethod_FramePool", true},
		{"extra struct gains member", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCallerExtra] = mustReplace(t, fs[FileAsstCallerExtra],
				"    const char* name;\n};", "    const char* name;\n    const char* extra;\n};")
		}, frozenStructTag, true},
		{"extra struct member type change", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCallerExtra] = mustReplace(t, fs[FileAsstCallerExtra],
				"const char* code;", "char* code;")
		}, frozenStructTag, true},
		{"extra guard removed", linuxTarget, func(t *testing.T, fs map[string]string) {
			extra := fs[FileAsstCallerExtra]
			extra = strings.Replace(extra, "#if ASST_WITH_EXTRA_CALLERS\n\n", "", 1)
			extra = strings.Replace(extra, "#endif // ASST_WITH_EXTRA_CALLERS\n", "", 1)
			if strings.Contains(extra, "ASST_WITH_EXTRA_CALLERS") {
				t.Fatal("guard removal mutation did not apply")
			}
			fs[FileAsstCallerExtra] = extra
		}, "AsstGetMapLevelKey", true},
		{"unknown include", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"#include \"AsstPort.h\"\n", "#include \"AsstPort.h\"\n#include \"AsstTypes.h\"\n")
		}, "#include", true},
		{"missing include", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"#include \"AsstPort.h\"\n", "")
		}, "ASST_CALL", true},
		{"variadic parameter", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"void ASSTAPI AsstLog(const char* level, const char* message);",
				"void ASSTAPI AsstLog(const char* level, ...);")
		}, "AsstLog", true},
		{"multi-level pointer", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"char* buff", "char** buff")
		}, "char**", true},
		{"missing locked export", linuxTarget, func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"    AsstBool ASSTAPI AsstStart(AsstHandle handle);\n", "")
		}, "AsstStart", false},
		{"duplicate declaration", linuxTarget, func(t *testing.T, fs map[string]string) {
			line := "    AsstBool ASSTAPI AsstStop(AsstHandle handle);\n"
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller], line, line+line)
		}, "AsstStop", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := positiveFiles(t)
			tc.mutate(t, files)
			dir, _ := writeCInput(t, files)
			input := testLoadCInput(t, dir)
			res, diags := ExtractC(input, tc.target)
			if len(diags) == 0 {
				t.Fatalf("ExtractC unexpectedly passed; want failure mentioning %q (result: %d functions)",
					tc.sym, len(res.Functions))
			}
			d := diags[0]
			if d.Category != CatCSyntax {
				t.Errorf("category = %q, want %q", d.Category, CatCSyntax)
			}
			if d.File == "" {
				t.Errorf("diagnostic must locate a file, got %+v", d)
			}
			if tc.wantLine && d.Line <= 0 {
				t.Errorf("syntax diagnostic must carry a line, got %+v", d)
			}
			if !strings.Contains(d.Symbol, tc.sym) {
				t.Errorf("symbol = %q, want it to contain %q", d.Symbol, tc.sym)
			}
		})
	}
}

// TestExtractUnknownTarget 未知目标组合在提取前即失败。
func TestExtractUnknownTarget(t *testing.T) {
	dir, _ := writeCInput(t, positiveFiles(t))
	input := testLoadCInput(t, dir)
	res, diags := ExtractC(input, Target{GOOS: "darwin", GOARCH: "arm64"})
	if res != nil || len(diags) != 1 || diags[0].Category != CatInput {
		t.Fatalf("ExtractC with unknown target = %+v, %v", res, diags)
	}
	if !strings.Contains(diags[0].Symbol, "target") {
		t.Errorf("symbol = %q, want it to mention target", diags[0].Symbol)
	}
}

// TestExtractCTypeString 覆盖规范化类型的可读拼写。
func TestExtractCTypeString(t *testing.T) {
	cases := map[string]CType{
		"void":                   {Kind: CKindVoid},
		"uint8":                  {Kind: CKindScalar, Name: "uint8", Bits: 8},
		"int32":                  {Kind: CKindScalar, Name: "int32", Bits: 32, Signed: true},
		"const char*":            {Kind: CKindPointer, Pointee: &CType{Kind: CKindScalar, Name: "char", Bits: 8, Const: true}},
		"char*":                  {Kind: CKindPointer, Pointee: &CType{Kind: CKindScalar, Name: "char", Bits: 8}},
		"void*":                  {Kind: CKindPointer, Pointee: &CType{Kind: CKindVoid}},
		"int32*":                 {Kind: CKindPointer, Pointee: &CType{Kind: CKindScalar, Name: "int32", Bits: 32, Signed: true}},
		"struct AsstExtAPI*":     {Kind: CKindPointer, Pointee: &CType{Kind: CKindStruct, Tag: "AsstExtAPI"}},
		"struct AsstMapLevelKey": {Kind: CKindStruct, Tag: "AsstMapLevelKey"},
	}
	for want, ct := range cases {
		if got := ct.String(); got != want {
			t.Errorf("String() = %q, want %q", got, want)
		}
	}
}
