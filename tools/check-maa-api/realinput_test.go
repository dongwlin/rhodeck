// 本文件对有来源的未版本化头快照运行完整声明与签名检查。
// CHECK_MAA_API_INPUT 可显式选择另一份输入，错误输入必须失败。

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func requireRealCInput(t *testing.T) string {
	t.Helper()
	if input := os.Getenv("CHECK_MAA_API_INPUT"); input != "" {
		return input
	}
	return filepath.Join(repoRoot(t), "tools", "check-maa-api", "testdata", "snapshot")
}

// TestRealHeaderCheckerEndToEnd 用真实独立 C 头对两个目标各跑一次完整
// 命令链路,断言:
//   - 两目标均退出 0,stdout 为 JSON,无 PASS;
//   - Linux 适用集合不含窗口符号,Windows 适用集合包含之;
//   - 完整集合 28 项 caller 导出(+2 项实验发现)在两目标一致;
//   - 规范化类型保留宽度/符号位/指针区分:GetImage 的 unsafe.Pointer
//     对应 C void*,GetTasksList 的 *int32 对应 AsstTaskId*,Log 无返回
//     值对应 C void,GetVersion 的 *byte 对应 const char*,宽度类型
//     uint8/int32/uint64 互不合并。
func TestRealHeaderCheckerEndToEnd(t *testing.T) {
	cInput := requireRealCInput(t)
	goRoot := repoRoot(t)

	type outcome struct {
		code   int
		stdout string
		stderr string
	}
	run := func(goos string) outcome {
		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"--goos", goos, "--goarch", "amd64",
			"--c-input", cInput, "--go-root", goRoot,
		}, &stdout, &stderr)
		return outcome{code, stdout.String(), stderr.String()}
	}

	parse := func(o outcome, goos string) reportJSON {
		t.Helper()
		require.Equal(t, 0, o.code, "%s run exited %d:\n%s", goos, o.code, o.stderr)
		require.True(t, o.stderr == "" && !strings.Contains(o.stdout, "PASS"),
			"%s run must print JSON only:\n%s\n%s", goos, o.stdout, o.stderr)
		var r reportJSON
		err := json.Unmarshal([]byte(o.stdout), &r)
		require.NoError(t, err, "%s stdout is not valid JSON: %v\n%s", goos, err, o.stdout)
		return r
	}

	linuxRep := parse(run("linux"), "linux")
	winRep := parse(run("windows"), "windows")

	// 完整集合:28 项 caller 导出 + 2 项实验发现,与目标无关。
	got := len(linuxRep.C.Full) - len(linuxRep.C.Discovery)
	assert.Equal(t, 28, got, "linux caller exports = %d, want 28 (full %d, discovery %d)",
		got, len(linuxRep.C.Full), len(linuxRep.C.Discovery))
	assert.Equal(t, strings.Join(linuxRep.C.Full, ","), strings.Join(winRep.C.Full, ","),
		"full export set must be identical across targets")

	// 适用集合:Linux 不含窗口符号,Windows 包含之。
	applicable := func(r reportJSON) map[string]bool {
		m := map[string]bool{}
		for _, s := range r.C.Applicable {
			m[s] = true
		}
		return m
	}
	linuxApp, winApp := applicable(linuxRep), applicable(winRep)
	for _, name := range []string{"AsstAttachWindow", "AsstAsyncAttachWindow"} {
		assert.False(t, linuxApp[name], "%s must not be applicable on linux", name)
		assert.True(t, winApp[name], "%s must be applicable on windows", name)
	}
	assert.True(t, len(linuxApp) == 26 && len(winApp) == 28,
		"applicable sizes linux=%d windows=%d, want 26/28", len(linuxApp), len(winApp))

	// 关联分类表:Linux 绑定 25 项 + 1 项非目标 windowsOnly + 4 项排除;
	// Windows 绑定 26 项。
	assert.True(t, len(linuxRep.Link.Bound) == 27 && len(linuxRep.Link.Excluded) == 2 &&
		strings.Join(linuxRep.Link.NonTarget, ",") == "AsstAsyncAttachWindow",
		"linux link = bound %d excluded %d nontarget %v",
		len(linuxRep.Link.Bound), len(linuxRep.Link.Excluded), linuxRep.Link.NonTarget)
	assert.True(t, len(winRep.Link.Bound) == 28 && len(winRep.Link.NonTarget) == 0,
		"windows link = bound %d nontarget %v", len(winRep.Link.Bound), winRep.Link.NonTarget)

	// 期望比对段:真实独立 C 头 + 真实 Go 树下,30 项冻结
	// 期望逐符号 expected=actual,两目标一致——证明冻结期望表与生产
	// symbols.go/exclusions 一致。
	for goos, r := range map[string]reportJSON{"linux": linuxRep, "windows": winRep} {
		if len(r.Expect.Symbols) != 30 {
			assert.Equal(t, 30, len(r.Expect.Symbols), "%s expect entries = %d, want 30", goos, len(r.Expect.Symbols))
			continue
		}
		for i, e := range r.Expect.Symbols {
			assert.Equal(t, sortedExpectationSymbols()[i], e.Symbol,
				"%s expect[%d].Symbol = %q, want %q (must be sorted)",
				goos, i, e.Symbol, sortedExpectationSymbols()[i])
			assert.Equal(t, e.Expected, e.Actual,
				"%s expect[%s] actual = %+v, want %+v", goos, e.Symbol, e.Actual, e.Expected)
		}
	}
	assert.Equal(t, fmtJSON(t, linuxRep.Expect), fmtJSON(t, winRep.Expect),
		"expect section must be identical across targets")

	// 规范化类型:宽度、符号位与指针用途按位置保留。
	cFunc := func(r reportJSON, name string) cFuncJSON {
		t.Helper()
		for _, f := range r.C.Functions {
			if f.Name == name {
				return f
			}
		}
		require.FailNow(t, fmt.Sprintf("c function %s missing from report", name))
		return cFuncJSON{}
	}
	gField := func(r reportJSON, name string) goFieldJSON {
		t.Helper()
		for _, f := range r.Go.Fields {
			if f.Name == name {
				return f
			}
		}
		require.FailNow(t, fmt.Sprintf("go field %s missing from report", name))
		return goFieldJSON{}
	}
	cParamType := func(f cFuncJSON, param string) string {
		t.Helper()
		for _, p := range f.Params {
			if p.Name == param {
				return p.Type
			}
		}
		require.FailNow(t, fmt.Sprintf("c function %s has no parameter %s", f.Name, param))
		return ""
	}
	gParamType := func(f goFieldJSON, param string) string {
		t.Helper()
		for _, p := range f.Params {
			if p.Name == param {
				return p.Type
			}
		}
		require.FailNow(t, fmt.Sprintf("go field %s has no parameter %s", f.Name, param))
		return ""
	}

	// checkParam 断言一个具名参数位置的 C↔Go 类型对应。
	checkParam := func(cName, cParamName, cWant, gName, gParamName, gWant string) {
		t.Helper()
		gotC := cParamType(cFunc(linuxRep, cName), cParamName)
		assert.Equal(t, cWant, gotC, "C %s.%s = %q, want %q", cName, cParamName, gotC, cWant)
		gotG := gParamType(gField(linuxRep, gName), gParamName)
		assert.Equal(t, gWant, gotG, "Go %s.%s = %q, want %q", gName, gParamName, gotG, gWant)
	}
	// checkRet 断言返回位置的 C↔Go 类型对应(gWant 为 "" 表示无结果
	// 列表,对应 C void)。
	checkRet := func(cName, cWant, gName, gWant string) {
		t.Helper()
		gotRet := cFunc(linuxRep, cName).Ret
		assert.Equal(t, cWant, gotRet, "C %s ret = %q, want %q", cName, gotRet, cWant)
		gotResult := gField(linuxRep, gName).Result
		assert.Equal(t, gWant, gotResult, "Go %s result = %q, want %q", gName, gotResult, gWant)
	}

	// 图像缓冲区:C void* ↔ unsafe.Pointer。
	checkParam("AsstGetImage", "buff", "void*", "GetImage", "buff", "unsafe.Pointer")
	// 任务列表:C AsstTaskId* (int32) ↔ *int32。
	checkParam("AsstGetTasksList", "buff", "int32*", "GetTasksList", "buff", "*int32")
	// 日志:C void ↔ 无结果列表。
	checkRet("AsstLog", "void", "Log", "")
	// 版本:C const char* ↔ *byte。
	checkRet("AsstGetVersion", "const char*", "GetVersion", "*byte")
	// 布尔:C AsstBool (uint8) ↔ uint8,与 int32/uint64 不合并。
	checkRet("AsstSetUserDir", "uint8", "SetUserDir", "uint8")
	checkRet("AsstGetNullSize", "uint64", "GetNullSize", "uint64")
	checkRet("AsstAppendTask", "int32", "AppendTask", "int32")
	// 句柄:C AsstHandle (opaque) ↔ uintptr。
	checkParam("AsstDestroy", "handle", "struct AsstExtAPI*", "Destroy", "handle", "uintptr")
}

// TestRealHeaderSignatureSection 用真实独立 C 头核验宽度级签名比对
// 的成功报告段:两目标下全部 26 条绑定(含 linux 目标下属
// NonTarget 的 windowsOnly 绑定)全部通过,即冻结映射与生产
// api.go/callback_types.go 逐字段一致——任何一处宽度/符号位/指针
// 用途/参数名偏离都会使整个命令失败。callback 平台适配两条均核验;
// 同参数两次运行 stdout 逐字节一致(signatures 段键序稳定)。
func TestRealHeaderSignatureSection(t *testing.T) {
	cInput := requireRealCInput(t)
	goRoot := repoRoot(t)

	run := func(goos string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"--goos", goos, "--goarch", "amd64",
			"--c-input", cInput, "--go-root", goRoot,
		}, &stdout, &stderr)
		require.Equal(t, 0, code, "%s run exited %d:\n%s", goos, code, stderr.String())
		return stdout.String()
	}
	parse := func(out, goos string) reportJSON {
		t.Helper()
		var r reportJSON
		err := json.Unmarshal([]byte(out), &r)
		require.NoError(t, err, "%s stdout is not valid JSON: %v", goos, err)
		return r
	}
	symbol := func(r reportJSON, name string) signatureSymbolJSON {
		t.Helper()
		for _, s := range r.Signatures.Symbols {
			if s.Symbol == name {
				return s
			}
		}
		require.FailNow(t, fmt.Sprintf("signature symbol %s missing from the %s report", name, name))
		return signatureSymbolJSON{}
	}
	param := func(s signatureSymbolJSON, index int) signatureParamJSON {
		t.Helper()
		require.True(t, index < len(s.Params), "%s has %d params, want index %d", s.Symbol, len(s.Params), index)
		return s.Params[index]
	}

	linuxOut := run("linux")
	again := run("linux")
	assert.Equal(t, linuxOut, again, "success output including the signatures section must be byte-identical across runs")
	linuxRep := parse(linuxOut, "linux")
	require.Equal(t, 28, len(linuxRep.Signatures.Symbols),
		"linux signature symbols = %d, want 28 (all bindings, including the non-target windowsOnly binding)", len(linuxRep.Signatures.Symbols))
	for i := 1; i < len(linuxRep.Signatures.Symbols); i++ {
		assert.Less(t, linuxRep.Signatures.Symbols[i-1].Symbol, linuxRep.Signatures.Symbols[i].Symbol,
			"signature symbols must be strictly sorted: %q >= %q",
			linuxRep.Signatures.Symbols[i-1].Symbol, linuxRep.Signatures.Symbols[i].Symbol)
	}

	// 关键映射逐点抽查(与冻结矩阵逐条对应):
	// 图像缓冲区 void* ↔ unsafe.Pointer,容量/返回 uint64。
	gi := symbol(linuxRep, "AsstGetImage")
	assert.True(t, gi.Field == "GetImage" && gi.CHeader == FileAsstCaller && gi.CLine > 0 &&
		gi.GoFile == "api.go" && gi.GoLine > 0, "AsstGetImage record = %+v", gi)
	p := param(gi, 1)
	assert.Equal(t, signatureParamJSON{Position: 2, CName: "buff", GoName: "buff",
		CType: "void*", GoType: "unsafe.Pointer"}, p, "AsstGetImage buff = %+v", p)
	p = param(gi, 2)
	assert.Equal(t, signatureParamJSON{Position: 3, CName: "buff_size", GoName: "buffSize",
		CType: "uint64", GoType: "uint64"}, p, "AsstGetImage buff_size = %+v", p)
	assert.Equal(t, signatureReturnJSON{CType: "uint64", GoType: "uint64"}, gi.Return,
		"AsstGetImage return = %+v", gi.Return)
	// 任务列表 AsstTaskId* ↔ *int32。
	p = param(symbol(linuxRep, "AsstGetTasksList"), 1)
	assert.Equal(t, signatureParamJSON{Position: 2,
		CName: "buff", GoName: "buff", CType: "int32*", GoType: "*int32"}, p, "AsstGetTasksList buff = %+v", p)
	// 批准重命名 type→taskType 与 const char* ↔ *byte。
	p = param(symbol(linuxRep, "AsstAppendTask"), 1)
	assert.Equal(t, signatureParamJSON{Position: 2,
		CName: "type", GoName: "taskType", CType: "const char*", GoType: "*byte"}, p, "AsstAppendTask type = %+v", p)
	// opaque handle ↔ uintptr 与 callback 函数指针 ↔ uintptr、
	// custom_arg token ↔ uintptr。
	p = param(symbol(linuxRep, "AsstDestroy"), 0)
	assert.Equal(t, signatureParamJSON{Position: 1,
		CName: "handle", GoName: "handle", CType: "struct AsstExtAPI*", GoType: "uintptr"}, p, "AsstDestroy handle = %+v", p)
	ce := symbol(linuxRep, "AsstCreateEx")
	p = param(ce, 0)
	assert.True(t, p.GoType == "uintptr" && strings.HasPrefix(p.CType, "void("),
		"AsstCreateEx callback = %+v", p)
	p = param(ce, 1)
	assert.Equal(t, signatureParamJSON{Position: 2, CName: "custom_arg",
		GoName: "customArg", CType: "void*", GoType: "uintptr"}, p, "AsstCreateEx custom_arg = %+v", p)
	// void 返回与 const char* 返回。
	lg := symbol(linuxRep, "AsstLog")
	assert.Equal(t, signatureReturnJSON{CType: "void", GoType: ""}, lg.Return,
		"AsstLog return = %+v", lg.Return)
	gv := symbol(linuxRep, "AsstGetVersion")
	assert.Equal(t, signatureReturnJSON{
		CType: "const char*", GoType: "*byte"}, gv.Return, "AsstGetVersion return = %+v", gv.Return)

	// callback 平台适配:unix 原样 void,windows 许可适配 uintptr。
	require.Equal(t, 2, len(linuxRep.Signatures.Callbacks), "callbacks = %d, want 2", len(linuxRep.Signatures.Callbacks))
	uc, wc := linuxRep.Signatures.Callbacks[0], linuxRep.Signatures.Callbacks[1]
	assert.True(t, uc.Name == "unixCallback" && uc.GoFile == "callback_types.go" && uc.GoLine > 0,
		"unixCallback record = %+v", uc)
	wantCallbackParams := []signatureParamJSON{
		{Position: 1, CName: "msg", GoName: "msg", CType: "int32", GoType: "int32"},
		{Position: 2, CName: "details_json", GoName: "detailsJSON", CType: "const char*", GoType: "*byte"},
		{Position: 3, CName: "custom_arg", GoName: "customArg", CType: "void*", GoType: "uintptr"},
	}
	for i, w := range wantCallbackParams {
		assert.Equal(t, w, uc.Params[i], "unixCallback param %d = %+v, want %+v", i, uc.Params[i], w)
		assert.Equal(t, w, wc.Params[i], "windowsCallback param %d = %+v, want %+v", i, wc.Params[i], w)
	}
	assert.Equal(t, signatureReturnJSON{CType: "void", GoType: ""}, uc.Return,
		"unixCallback return = %+v", uc.Return)
	assert.True(t, wc.Name == "windowsCallback" && wc.Return == (signatureReturnJSON{CType: "void", GoType: "uintptr"}),
		"windowsCallback record = %+v", wc)

	// linux 目标:窗口符号属 NonTarget 的 windowsOnly 绑定,同样被
	// 逐参数核验并出现在核验记录里。
	awl := symbol(linuxRep, "AsstAsyncAttachWindow")
	p = param(awl, 1)
	assert.Equal(t, signatureParamJSON{Position: 2, CName: "hwnd", GoName: "hwnd",
		CType: "void*", GoType: "uintptr"}, p, "linux AsstAsyncAttachWindow hwnd = %+v", p)
	p = param(awl, 2)
	assert.Equal(t, signatureParamJSON{Position: 3, CName: "screencap_method",
		GoName: "screencapMethod", CType: "uint64", GoType: "uint64"}, p, "linux AsstAsyncAttachWindow screencap_method = %+v", p)

	// windows 目标:26 条绑定全部通过,窗口符号经 Bound 分类核验,
	// hwnd/method 映射与 linux 目标一致,不得重复报错或行为回退。
	winRep := parse(run("windows"), "windows")
	require.Equal(t, 28, len(winRep.Signatures.Symbols),
		"windows signature symbols = %d, want 28", len(winRep.Signatures.Symbols))
	aw := symbol(winRep, "AsstAsyncAttachWindow")
	p = param(aw, 1)
	assert.Equal(t, signatureParamJSON{Position: 2, CName: "hwnd", GoName: "hwnd",
		CType: "void*", GoType: "uintptr"}, p, "AsstAsyncAttachWindow hwnd = %+v", p)
	p = param(aw, 2)
	assert.Equal(t, signatureParamJSON{Position: 3, CName: "screencap_method",
		GoName: "screencapMethod", CType: "uint64", GoType: "uint64"}, p, "AsstAsyncAttachWindow screencap_method = %+v", p)
	p = param(aw, 5)
	assert.Equal(t, signatureParamJSON{Position: 6, CName: "block", GoName: "block",
		CType: "uint8", GoType: "uint8"}, p, "AsstAsyncAttachWindow block = %+v", p)
}
