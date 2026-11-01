// extract_test.go 覆盖受限 C 提取:真实形状正例(Port 全部宏与条件、
// typedef 链、函数指针、GetVersion 拼写、deprecated 注释、Windows
// enum、Extra 结构体)、Linux/Windows 两种目标的不同解析结果、
// ASST_WITH_EXTRA_CALLERS 0/1 两种发现,以及的 C 提取
// 失败向量。全部 fixture 为 testdata 下的合成头文件,自包含。

package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	require.NotNil(t, f, "function %s not found", name)
	return *f
}

func findTypedef(t *testing.T, tds []CTypedef, name string) CTypedef {
	t.Helper()
	for _, td := range tds {
		if td.Name == name {
			return td
		}
	}
	require.FailNowf(t, "typedef not found", "typedef %s not found", name)
	return CTypedef{}
}

func findEnum(t *testing.T, enums []CEnum, tag string) CEnum {
	t.Helper()
	for _, e := range enums {
		if e.Tag == tag {
			return e
		}
	}
	require.FailNowf(t, "enum not found", "enum %s not found", tag)
	return CEnum{}
}

func mustExtract(t *testing.T, files map[string]string, target Target) *CExtraction {
	t.Helper()
	dir, _ := writeCInput(t, files)
	input := testLoadCInput(t, dir)
	res, diags := ExtractC(input, target)
	require.Empty(t, diags, "ExtractC unexpectedly failed: %v", diags)
	return res
}

func TestExtractLinuxPositive(t *testing.T) {
	files := positiveFiles(t)
	// (void) 的规范零参数形式对任何函数可识别。
	files[FileAsstCaller] = mustReplace(t, files[FileAsstCaller],
		"AsstHandle ASSTAPI AsstCreate();", "AsstHandle ASSTAPI AsstCreate(void);")
	res := mustExtract(t, files, linuxTarget)

	require.Len(t, res.Functions, 30, "full set has %d functions, want 30", len(res.Functions))
	require.Len(t, res.Applicable, 26, "applicable set on linux has %d functions, want 26", len(res.Applicable))
	require.Len(t, res.Discovery, 2, "discovery has %d entries, want 2", len(res.Discovery))
	require.Len(t, res.Typedefs, 11, "typedefs = %d, want 11", len(res.Typedefs))
	require.Len(t, res.Enums, 2, "enums = %d, want 2", len(res.Enums))
	require.Len(t, res.Structs, 1, "structs = %d, want 1", len(res.Structs))

	// AsstSetUserDir:AsstBool→uint8 保留原名;const char* 参数;
	// linux 下调用约定为空、导出修饰为 visibility(default)。
	sud := findFunction(t, res.Functions, "AsstSetUserDir")
	assert.False(t, sud.Header != FileAsstCaller || sud.Line <= 0,
		"AsstSetUserDir header/line = %s/%d", sud.Header, sud.Line)
	assert.Equal(t, PlatformAll, sud.Platform, "AsstSetUserDir platform = %q, want all", sud.Platform)
	assert.False(t, sud.CallConv != CallConvNone || sud.Linkage != LinkageVisibilityDefault,
		"linux modifiers = %q/%q, want empty/%q", sud.CallConv, sud.Linkage, LinkageVisibilityDefault)
	wantRet := CType{Kind: CKindScalar, Name: "uint8", Bits: 8, Via: "AsstBool"}
	assert.True(t, sud.Ret.Equal(wantRet),
		"AsstSetUserDir ret = %s (via %s), want uint8 via AsstBool", sud.Ret.String(), sud.Ret.Via)
	require.False(t, len(sud.Params) != 1 || sud.Params[0].Name != "path",
		"AsstSetUserDir params = %+v", sud.Params)
	wantParam := CParam{Name: "path", Type: CType{Kind: CKindPointer,
		Pointee: &CType{Kind: CKindScalar, Name: "char", Bits: 8, Const: true}}}
	assert.False(t, !sud.Params[0].Equal(wantParam) || sud.Params[0].Type.String() != "const char*",
		"AsstSetUserDir param = %s, want const char* path", sud.Params[0].Type.String())

	// 多行声明与 deprecated 注释:注释不得误作属性。
	sio := findFunction(t, res.Functions, "AsstSetInstanceOption")
	assert.False(t, len(sio.Params) != 3 || sio.Params[1].Name != "key",
		"AsstSetInstanceOption params = %+v", sio.Params)
	con := findFunction(t, res.Functions, "AsstConnect")
	assert.False(t, len(con.Params) != 4 || con.Params[1].Name != "adb_path",
		"AsstConnect params = %+v", con.Params)

	// AsstCreateEx:函数指针参数与回调签名。
	ce := findFunction(t, res.Functions, "AsstCreateEx")
	require.False(t, len(ce.Params) != 2 || ce.Params[0].Name != "callback" || ce.Params[1].Name != "custom_arg",
		"AsstCreateEx params = %+v", ce.Params)
	cbType := ce.Params[0].Type
	require.False(t, cbType.Kind != CKindFuncPtr || cbType.Via != "AsstApiCallback",
		"callback param kind/via = %v/%q", cbType.Kind, cbType.Via)
	sig := cbType.Sig
	require.False(t, sig.Ret.Kind != CKindVoid || sig.CallConv != CallConvNone || len(sig.Params) != 3,
		"callback signature = %+v", sig)
	assert.False(t, sig.Params[0].Name != "msg" || sig.Params[0].Type.String() != "int32" ||
		sig.Params[0].Type.Via != "AsstMsgId",
		"callback msg param = %+v (%s)", sig.Params[0], sig.Params[0].Type.String())
	assert.False(t, sig.Params[1].Name != "details_json" || sig.Params[1].Type.String() != "const char*",
		"callback details param = %+v", sig.Params[1])
	assert.False(t, sig.Params[2].Name != "custom_arg" || sig.Params[2].Type.String() != "void*",
		"callback custom_arg param = %+v", sig.Params[2])

	// 输出缓冲区角色:void*、AsstSize、char*、AsstTaskId*。
	gi := findFunction(t, res.Functions, "AsstGetImage")
	assert.False(t, gi.Params[1].Type.String() != "void*" || gi.Params[1].Name != "buff",
		"AsstGetImage buff = %+v", gi.Params[1])
	assert.False(t, gi.Params[2].Type.String() != "uint64" || gi.Params[2].Type.Via != "AsstSize",
		"AsstGetImage buff_size = %+v", gi.Params[2])
	assert.False(t, gi.Ret.String() != "uint64" || gi.Ret.Via != "AsstSize",
		"AsstGetImage ret = %s (via %s)", gi.Ret.String(), gi.Ret.Via)
	gl := findFunction(t, res.Functions, "AsstGetTasksList")
	assert.False(t, gl.Params[1].Type.String() != "int32*" ||
		gl.Params[1].Type.Pointee == nil || gl.Params[1].Type.Pointee.Via != "AsstTaskId",
		"AsstGetTasksList buff = %s (pointee via %s)", gl.Params[1].Type.String(),
		gl.Params[1].Type.Pointee.Via)

	// void 返回。
	de := findFunction(t, res.Functions, "AsstDestroy")
	assert.Equal(t, CKindVoid, de.Ret.Kind, "AsstDestroy ret kind = %v, want void", de.Ret.Kind)
	lg := findFunction(t, res.Functions, "AsstLog")
	assert.False(t, lg.Ret.Kind != CKindVoid || lg.CallConv != CallConvNone,
		"AsstLog ret/callconv = %v/%q", lg.Ret.Kind, lg.CallConv)

	// GetVersion 特殊拼写。
	gv := findFunction(t, res.Functions, "AsstGetVersion")
	assert.False(t, gv.Linkage != LinkageVisibilityDefault || gv.CallConv != CallConvNone || len(gv.Params) != 0,
		"AsstGetVersion linkage/callconv/params = %q/%q/%d",
		gv.Linkage, gv.CallConv, len(gv.Params))
	assert.False(t, gv.Ret.String() != "const char*" || gv.Spelling != frozenGetVersionCanon,
		"AsstGetVersion ret/spelling = %s/%q", gv.Ret.String(), gv.Spelling)

	// Windows 符号:在完整集合中标注 windows,Linux 适用集合不含它们。
	for _, name := range []string{"AsstAttachWindow", "AsstAsyncAttachWindow"} {
		aw := findFunction(t, res.Functions, name)
		assert.Equal(t, PlatformWindows, aw.Platform, "%s platform = %q, want windows", name, aw.Platform)
		assert.Nil(t, functionByName(res.Applicable, name), "%s must not be applicable on linux", name)
	}
	aw := findFunction(t, res.Functions, "AsstAttachWindow")
	assert.False(t, len(aw.Params) != 5 || aw.Params[1].Type.String() != "void*" ||
		aw.Params[2].Type.String() != "uint64" || aw.Params[2].Type.Via != "",
		"AsstAttachWindow params = %+v", aw.Params)

	// 实验发现:恰为两个实验导出,平台 extra,不在适用集合。
	require.False(t, res.Discovery[0].Name != "AsstGetMapLevelKey" || res.Discovery[1].Name != "AsstGetItemName",
		"discovery = %v", discoveryNames(res.Discovery))
	mlk := res.Discovery[0]
	assert.Equal(t, PlatformExtra, mlk.Platform, "AsstGetMapLevelKey platform = %q, want extra", mlk.Platform)
	assert.False(t, mlk.Ret.Kind != CKindStruct || mlk.Ret.Tag != frozenStructTag,
		"AsstGetMapLevelKey ret = %+v, want struct %s by value", mlk.Ret, frozenStructTag)
	assert.False(t, len(mlk.Params) != 1 || mlk.Params[0].Name != "key" || mlk.Params[0].Type.String() != "const char*",
		"AsstGetMapLevelKey params = %+v", mlk.Params)
	assert.Nil(t, functionByName(res.Applicable, "AsstGetMapLevelKey"),
		"experimental export must not be in the applicable set")

	// Extra 结构体核验:四个 const char* 成员,顺序固定。
	st := res.Structs[0]
	require.False(t, st.Tag != frozenStructTag || st.Platform != PlatformExtra || len(st.Members) != 4,
		"struct = %+v", st)
	for i, want := range frozenStructMembers {
		assert.False(t, st.Members[i].Name != want || st.Members[i].Type.String() != "const char*",
			"member %d = %+v (%s), want const char* %s",
			i, st.Members[i], st.Members[i].Type.String(), want)
	}

	// Windows enum 命名值提取(1 << n 展开,尾逗号容忍)。
	sc := findEnum(t, res.Enums, "AsstWin32ScreencapMethodEnum")
	wantScreencap := []CEnumValue{
		{"AsstWin32ScreencapMethod_None", 0}, {"AsstWin32ScreencapMethod_GDI", 1},
		{"AsstWin32ScreencapMethod_FramePool", 2}, {"AsstWin32ScreencapMethod_DXGI_DesktopDup", 4},
		{"AsstWin32ScreencapMethod_DXGI_DesktopDup_Window", 8},
		{"AsstWin32ScreencapMethod_PrintWindow", 16}, {"AsstWin32ScreencapMethod_ScreenDC", 32},
	}
	require.Len(t, sc.Values, len(wantScreencap),
		"screencap enum has %d values, want %d", len(sc.Values), len(wantScreencap))
	for i, w := range wantScreencap {
		assert.Equal(t, w, sc.Values[i], "screencap value %d = %+v, want %+v", i, sc.Values[i], w)
	}
	im := findEnum(t, res.Enums, "AsstWin32InputMethodEnum")
	require.Len(t, im.Values, 12, "input enum has %d values, want 12", len(im.Values))
	last := im.Values[len(im.Values)-1]
	assert.False(t, last.Name != "AsstWin32InputMethod_AnchoredTouch" || last.Value != 1024,
		"last input value = %+v, want AnchoredTouch=1024", last)

	// typedef 链与指针语义。
	td := findTypedef(t, res.Typedefs, "AsstTaskId")
	assert.True(t, td.Type.Equal(CType{Kind: CKindScalar, Name: "int32", Bits: 32, Signed: true, Via: "AsstId"}),
		"AsstTaskId type = %s (via %s), want int32 via AsstId", td.Type.String(), td.Type.Via)
	h := findTypedef(t, res.Typedefs, "AsstHandle")
	assert.False(t, h.Type.Kind != CKindPointer || h.Type.Pointee == nil ||
		h.Type.Pointee.Kind != CKindStruct || h.Type.Pointee.Tag != "AsstExtAPI",
		"AsstHandle type = %s, want pointer to struct AsstExtAPI", h.Type.String())
	assert.Equal(t, "struct AsstExtAPI*", h.Type.String(), "AsstHandle String() = %q", h.Type.String())
	cbtd := findTypedef(t, res.Typedefs, "AsstApiCallback")
	require.False(t, cbtd.Type.Kind != CKindFuncPtr || cbtd.Type.Sig == nil,
		"AsstApiCallback typedef = %+v", cbtd.Type)
	assert.True(t, cbtd.Type.Sig.Equal(*cbType.Sig),
		"AsstApiCallback typedef signature differs from the callback parameter type")
}

func TestExtractWindowsPositive(t *testing.T) {
	res := mustExtract(t, positiveFiles(t), windowsTarget)

	require.Len(t, res.Functions, 30, "full set has %d functions, want 30", len(res.Functions))
	require.Len(t, res.Applicable, 28, "applicable set on windows has %d functions, want 28", len(res.Applicable))
	for _, name := range []string{"AsstAttachWindow", "AsstAsyncAttachWindow"} {
		assert.NotNil(t, functionByName(res.Applicable, name), "%s must be applicable on windows", name)
	}

	sud := findFunction(t, res.Functions, "AsstSetUserDir")
	assert.False(t, sud.CallConv != CallConvStdcall || sud.Linkage != LinkageDLLImport,
		"windows modifiers = %q/%q, want __stdcall/%q", sud.CallConv, sud.Linkage, LinkageDLLImport)
	gv := findFunction(t, res.Functions, "AsstGetVersion")
	assert.False(t, gv.CallConv != CallConvStdcall || gv.Linkage != LinkageDLLImport,
		"AsstGetVersion windows modifiers = %q/%q", gv.CallConv, gv.Linkage)
	ce := findFunction(t, res.Functions, "AsstCreateEx")
	sig := ce.Params[0].Type.Sig
	assert.Equal(t, CallConvStdcall, sig.CallConv,
		"windows callback callconv = %q, want __stdcall", sig.CallConv)
	// discovery 与 linux 一致:与目标无关。
	require.False(t, len(res.Discovery) != 2 || res.Discovery[0].Name != "AsstGetMapLevelKey",
		"discovery = %v", discoveryNames(res.Discovery))
}

// TestPassZeroAndHostileEnv 验证 =0 pass 恰含 28 项且无实验导出,以及
// 外部预定义 ASST_CALL 的敌意环境必须失败。
func TestPassZeroAndHostileEnv(t *testing.T) {
	dir, _ := writeCInput(t, positiveFiles(t))
	input := testLoadCInput(t, dir)

	res, d := runParsePass(input, newMacroEnv(linuxTarget, 0))
	require.Nil(t, d, "pass0 failed: %v", d)
	require.Len(t, res.Functions, 28, "pass0 functions = %d, want 28", len(res.Functions))
	for _, f := range res.Functions {
		assert.False(t, frozenExperimental[f.Name],
			"pass0 unexpectedly contains experimental export %s", f.Name)
	}

	// ASST_CALL 在进入 AsstPort.h 前已被定义:Port 分支被跳过,声明
	// 展开必须失败而不是沿用外部值。
	bad := macroEnv{
		defined: map[string]bool{"__x86_64__": true, "ASST_CALL": true},
		ints:    map[string]int64{"__GNUC__": 4, "ASST_WITH_EXTRA_CALLERS": 0},
	}
	_, d = runParsePass(input, bad)
	require.NotNil(t, d, "runParsePass unexpectedly passed with externally predefined ASST_CALL")
	assert.False(t, d.Category != CatCSyntax || !strings.Contains(d.Symbol, "ASST_CALL"),
		"hostile env diagnostic = %+v", d)
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
			require.False(t, strings.Contains(extra, "ASST_WITH_EXTRA_CALLERS"),
				"guard removal mutation did not apply")
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
			// 失败路径的 res 为 nil,函数计数须留在原 if 守卫内惰性求值。
			if len(diags) == 0 {
				require.FailNowf(t, "ExtractC unexpectedly passed",
					"ExtractC unexpectedly passed; want failure mentioning %q (result: %d functions)",
					tc.sym, len(res.Functions))
			}
			d := diags[0]
			assert.Equal(t, CatCSyntax, d.Category, "category = %q, want %q", d.Category, CatCSyntax)
			assert.NotEmpty(t, d.File, "diagnostic must locate a file, got %+v", d)
			assert.False(t, tc.wantLine && d.Line <= 0,
				"syntax diagnostic must carry a line, got %+v", d)
			assert.Contains(t, d.Symbol, tc.sym, "symbol = %q, want it to contain %q", d.Symbol, tc.sym)
		})
	}
}

// TestExtractUnknownTarget 未知目标组合在提取前即失败。
func TestExtractUnknownTarget(t *testing.T) {
	dir, _ := writeCInput(t, positiveFiles(t))
	input := testLoadCInput(t, dir)
	res, diags := ExtractC(input, Target{GOOS: "darwin", GOARCH: "arm64"})
	require.False(t, res != nil || len(diags) != 1 || diags[0].Category != CatInput,
		"ExtractC with unknown target = %+v, %v", res, diags)
	assert.Contains(t, diags[0].Symbol, "target", "symbol = %q, want it to mention target", diags[0].Symbol)
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
		got := ct.String()
		assert.Equal(t, want, got, "String() = %q, want %q", got, want)
	}
}
