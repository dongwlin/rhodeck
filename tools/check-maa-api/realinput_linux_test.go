//go:build linux

// realinput_linux_test.go 是真实头正例集成测试:从锁定发行包缓存解包
// 三个 C 头,校验整包与成员 SHA-256 后写出 inputs.json,并对 linux 与
// windows 两个目标跑完整 checker;归档缺失时 SKIP 并写明受阻原因。

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 锁定基线(证据冻结):发行包整包 SHA-256。成员级摘要取自
// checker 内部 profile 常量(newLockedProfile),不在本文件重复转录。
const lockedReleaseTarballSHA256 = "814a128d24c8183b38003284facd56789fe8346b8a66932f9ff885df48d48bc1"

// checkerTarballOverrideEnv 允许测试环境覆盖锁定归档缓存路径。
const checkerTarballOverrideEnv = "CHECK_MAA_API_TARBALL"

// errTarballMissing 表示锁定发行包缓存缺失:真实头验收受阻,计为
// SKIP——跳过不是通过,不得冒充真实环境验收。
var errTarballMissing = errors.New("locked release tarball missing")

// checkerTarballPath 返回锁定发行包缓存路径:默认用户缓存,可经
// CHECK_MAA_API_TARBALL 覆盖(测试专用)。
func checkerTarballPath() (string, error) {
	if p := os.Getenv(checkerTarballOverrideEnv); p != "" {
		return p, nil
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", fmt.Errorf("%w: cannot locate the user cache directory: %v", errTarballMissing, err)
	}
	return filepath.Join(cache, "maa", "MAA-v6.20.1-linux-x86_64.tar.gz"), nil
}

// sha256File 计算文件 SHA-256 十六进制串。
func sha256File(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// requireRealCInput 把锁定归档的三个 checker_input 头解包为经过校验的
// c 输入目录:整包哈希钉扎 → 流式解包三成员(逐边复制边算摘要)→
// 成员摘要与 checker 内部 profile 常量核对(失败即 Fatal)→ 写出
// inputs.json。归档缺失返回 errTarballMissing 供调用方 SKIP;其余错误
// 一律 Fatal(锁定基线被破坏不是环境受限)。
func requireRealCInput(t *testing.T) string {
	t.Helper()
	if runtime.GOARCH != "amd64" {
		t.Skipf("real-header acceptance blocked: architecture %s is outside the locked baseline (linux amd64)", runtime.GOARCH)
	}
	tarball, err := checkerTarballPath()
	if err != nil {
		t.Skipf("real-header acceptance blocked: %v", err)
	}
	if _, err := os.Stat(tarball); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			t.Skipf("real-header acceptance blocked: locked release tarball missing (%s)", tarball)
		}
		t.Fatalf("stat %s: %v", tarball, err)
	}
	got, err := sha256File(tarball)
	if err != nil {
		t.Fatalf("hash %s: %v", tarball, err)
	}
	if got != lockedReleaseTarballSHA256 {
		t.Fatalf("release tarball SHA256 = %s, want %s (locked baseline broken)", got, lockedReleaseTarballSHA256)
	}

	members := map[string]string{ // 归档成员名 → 归档相对路径
		"AsstCaller.h":      FileAsstCaller,
		"AsstPort.h":        FileAsstPort,
		"AsstCallerExtra.h": FileAsstCallerExtra,
	}
	profile := newLockedProfile()
	digests := map[string]string{}
	dir := t.TempDir()
	if err := extractHeaderMembers(tarball, dir, members, digests); err != nil {
		t.Fatalf("extracting the locked members failed: %v", err)
	}

	// 成员字节摘要必须等于 checker 内部 profile 常量;不符即 Fail,
	// 不是 skip。
	for _, key := range []string{FileAsstCaller, FileAsstPort, FileAsstCallerExtra} {
		want := profile.Files[key]
		if digests[key] != want.SHA256 {
			t.Fatalf("%s member SHA256 = %s, want %s (does not match the checker's internal profile constant)",
				key, digests[key], want.SHA256)
		}
	}

	// inputs.json 结构照 ops 归档 manifest 的字段;size/sha256 取实测
	// 值(已与内部常量核对一致)。
	m := manifest{
		SchemaVersion: intPtr(manifestSchemaVersion),
		ProfileID:     profile.ID,
		Source: manifestSource{
			Repository: profile.SourceRepository,
			Tag:        profile.SourceTag,
			Commit:     profile.SourceCommit,
		},
	}
	for _, member := range []string{"AsstCaller.h", "AsstPort.h", "AsstCallerExtra.h"} {
		key := members[member]
		data, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(key)))
		if err != nil {
			t.Fatalf("read extracted %s: %v", key, err)
		}
		m.Files = append(m.Files, manifestFile{
			ArchivePath: key,
			SourcePath:  "include/" + member,
			SizeBytes:   int64(len(data)),
			SHA256:      digests[key],
			Purpose:     PurposeCheckerInput,
			ReleasePath: member,
		})
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inputs.json"), append(b, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// extractHeaderMembers 流式解包锁定成员:tar+gzip 逐成员过滤,只保留
// 归档根的三个头文件,成员路径拒绝穿越;成员摘要就地计算。成员缺失即
// 失败,其余成员(库、资源等)跳过。
func extractHeaderMembers(tarball, dir string, members, digests map[string]string) error {
	f, err := os.Open(tarball)
	if err != nil {
		return fmt.Errorf("open %s: %w", tarball, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip %s: %w", tarball, err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read tar: %w", err)
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		key, wanted := members[name]
		if !wanted {
			continue
		}
		if hdr.Typeflag != tar.TypeReg {
			return fmt.Errorf("member %s has type %q outside the locked baseline (regular file expected)", name, hdr.Typeflag)
		}
		dest := filepath.Join(dir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return err
		}
		h := sha256.New()
		var buf bytes.Buffer
		if _, err := io.Copy(io.MultiWriter(&buf, h), tr); err != nil {
			return fmt.Errorf("extract %s: %w", name, err)
		}
		if err := os.WriteFile(dest, buf.Bytes(), 0o644); err != nil {
			return err
		}
		digests[key] = hex.EncodeToString(h.Sum(nil))
		seen[name] = true
	}
	for name := range members {
		if !seen[name] {
			return fmt.Errorf("member %s not found in the release tarball", name)
		}
	}
	return nil
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
			"--profile", LockedProfileID, "--goos", goos, "--goarch", "amd64",
			"--c-input", cInput, "--go-root", goRoot,
		}, &stdout, &stderr)
		return outcome{code, stdout.String(), stderr.String()}
	}

	parse := func(o outcome, goos string) reportJSON {
		t.Helper()
		if o.code != 0 {
			t.Fatalf("%s run exited %d:\n%s", goos, o.code, o.stderr)
		}
		if o.stderr != "" || strings.Contains(o.stdout, "PASS") {
			t.Fatalf("%s run must print JSON only:\n%s\n%s", goos, o.stdout, o.stderr)
		}
		var r reportJSON
		if err := json.Unmarshal([]byte(o.stdout), &r); err != nil {
			t.Fatalf("%s stdout is not valid JSON: %v\n%s", goos, err, o.stdout)
		}
		return r
	}

	linuxRep := parse(run("linux"), "linux")
	winRep := parse(run("windows"), "windows")

	// 完整集合:28 项 caller 导出 + 2 项实验发现,与目标无关。
	if got := len(linuxRep.C.Full) - len(linuxRep.C.Discovery); got != 28 {
		t.Errorf("linux caller exports = %d, want 28 (full %d, discovery %d)",
			got, len(linuxRep.C.Full), len(linuxRep.C.Discovery))
	}
	if strings.Join(linuxRep.C.Full, ",") != strings.Join(winRep.C.Full, ",") {
		t.Error("full export set must be identical across targets")
	}

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
		if linuxApp[name] {
			t.Errorf("%s must not be applicable on linux", name)
		}
		if !winApp[name] {
			t.Errorf("%s must be applicable on windows", name)
		}
	}
	if len(linuxApp) != 26 || len(winApp) != 28 {
		t.Errorf("applicable sizes linux=%d windows=%d, want 26/28", len(linuxApp), len(winApp))
	}

	// 关联分类表:Linux 绑定 25 项 + 1 项非目标 windowsOnly + 4 项排除;
	// Windows 绑定 26 项。
	if len(linuxRep.Link.Bound) != 27 || len(linuxRep.Link.Excluded) != 2 ||
		strings.Join(linuxRep.Link.NonTarget, ",") != "AsstAsyncAttachWindow" {
		t.Errorf("linux link = bound %d excluded %d nontarget %v",
			len(linuxRep.Link.Bound), len(linuxRep.Link.Excluded), linuxRep.Link.NonTarget)
	}
	if len(winRep.Link.Bound) != 28 || len(winRep.Link.NonTarget) != 0 {
		t.Errorf("windows link = bound %d nontarget %v", len(winRep.Link.Bound), winRep.Link.NonTarget)
	}

	// 期望比对段:真实独立 C 头 + 真实 Go 树下,30 项冻结
	// 期望逐符号 expected=actual,两目标一致——证明冻结期望表与生产
	// symbols.go/exclusions 一致。
	for goos, r := range map[string]reportJSON{"linux": linuxRep, "windows": winRep} {
		if len(r.Expect.Symbols) != 30 {
			t.Errorf("%s expect entries = %d, want 30", goos, len(r.Expect.Symbols))
			continue
		}
		for i, e := range r.Expect.Symbols {
			if e.Symbol != sortedExpectationSymbols()[i] {
				t.Errorf("%s expect[%d].Symbol = %q, want %q (must be sorted)",
					goos, i, e.Symbol, sortedExpectationSymbols()[i])
			}
			if e.Actual != e.Expected {
				t.Errorf("%s expect[%s] actual = %+v, want %+v", goos, e.Symbol, e.Actual, e.Expected)
			}
		}
	}
	if fmtJSON(t, linuxRep.Expect) != fmtJSON(t, winRep.Expect) {
		t.Error("expect section must be identical across targets")
	}

	// 规范化类型:宽度、符号位与指针用途按位置保留。
	cFunc := func(r reportJSON, name string) cFuncJSON {
		t.Helper()
		for _, f := range r.C.Functions {
			if f.Name == name {
				return f
			}
		}
		t.Fatalf("c function %s missing from report", name)
		return cFuncJSON{}
	}
	gField := func(r reportJSON, name string) goFieldJSON {
		t.Helper()
		for _, f := range r.Go.Fields {
			if f.Name == name {
				return f
			}
		}
		t.Fatalf("go field %s missing from report", name)
		return goFieldJSON{}
	}
	cParamType := func(f cFuncJSON, param string) string {
		t.Helper()
		for _, p := range f.Params {
			if p.Name == param {
				return p.Type
			}
		}
		t.Fatalf("c function %s has no parameter %s", f.Name, param)
		return ""
	}
	gParamType := func(f goFieldJSON, param string) string {
		t.Helper()
		for _, p := range f.Params {
			if p.Name == param {
				return p.Type
			}
		}
		t.Fatalf("go field %s has no parameter %s", f.Name, param)
		return ""
	}

	// checkParam 断言一个具名参数位置的 C↔Go 类型对应。
	checkParam := func(cName, cParamName, cWant, gName, gParamName, gWant string) {
		t.Helper()
		if got := cParamType(cFunc(linuxRep, cName), cParamName); got != cWant {
			t.Errorf("C %s.%s = %q, want %q", cName, cParamName, got, cWant)
		}
		if got := gParamType(gField(linuxRep, gName), gParamName); got != gWant {
			t.Errorf("Go %s.%s = %q, want %q", gName, gParamName, got, gWant)
		}
	}
	// checkRet 断言返回位置的 C↔Go 类型对应(gWant 为 "" 表示无结果
	// 列表,对应 C void)。
	checkRet := func(cName, cWant, gName, gWant string) {
		t.Helper()
		if got := cFunc(linuxRep, cName).Ret; got != cWant {
			t.Errorf("C %s ret = %q, want %q", cName, got, cWant)
		}
		if got := gField(linuxRep, gName).Result; got != gWant {
			t.Errorf("Go %s result = %q, want %q", gName, got, gWant)
		}
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
			"--profile", LockedProfileID, "--goos", goos, "--goarch", "amd64",
			"--c-input", cInput, "--go-root", goRoot,
		}, &stdout, &stderr)
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
	symbol := func(r reportJSON, name string) signatureSymbolJSON {
		t.Helper()
		for _, s := range r.Signatures.Symbols {
			if s.Symbol == name {
				return s
			}
		}
		t.Fatalf("signature symbol %s missing from the %s report", name, name)
		return signatureSymbolJSON{}
	}
	param := func(s signatureSymbolJSON, index int) signatureParamJSON {
		t.Helper()
		if index >= len(s.Params) {
			t.Fatalf("%s has %d params, want index %d", s.Symbol, len(s.Params), index)
		}
		return s.Params[index]
	}

	linuxOut := run("linux")
	if again := run("linux"); again != linuxOut {
		t.Error("success output including the signatures section must be byte-identical across runs")
	}
	linuxRep := parse(linuxOut, "linux")
	if len(linuxRep.Signatures.Symbols) != 28 {
		t.Fatalf("linux signature symbols = %d, want 28 (all bindings, including the non-target windowsOnly binding)", len(linuxRep.Signatures.Symbols))
	}
	for i := 1; i < len(linuxRep.Signatures.Symbols); i++ {
		if linuxRep.Signatures.Symbols[i-1].Symbol >= linuxRep.Signatures.Symbols[i].Symbol {
			t.Errorf("signature symbols must be strictly sorted: %q >= %q",
				linuxRep.Signatures.Symbols[i-1].Symbol, linuxRep.Signatures.Symbols[i].Symbol)
		}
	}

	// 关键映射逐点抽查(与冻结矩阵逐条对应):
	// 图像缓冲区 void* ↔ unsafe.Pointer,容量/返回 uint64。
	gi := symbol(linuxRep, "AsstGetImage")
	if gi.Field != "GetImage" || gi.CHeader != FileAsstCaller || gi.CLine <= 0 ||
		gi.GoFile != "api.go" || gi.GoLine <= 0 {
		t.Errorf("AsstGetImage record = %+v", gi)
	}
	if p := param(gi, 1); p != (signatureParamJSON{Position: 2, CName: "buff", GoName: "buff",
		CType: "void*", GoType: "unsafe.Pointer"}) {
		t.Errorf("AsstGetImage buff = %+v", p)
	}
	if p := param(gi, 2); p != (signatureParamJSON{Position: 3, CName: "buff_size", GoName: "buffSize",
		CType: "uint64", GoType: "uint64"}) {
		t.Errorf("AsstGetImage buff_size = %+v", p)
	}
	if gi.Return != (signatureReturnJSON{CType: "uint64", GoType: "uint64"}) {
		t.Errorf("AsstGetImage return = %+v", gi.Return)
	}
	// 任务列表 AsstTaskId* ↔ *int32。
	if p := param(symbol(linuxRep, "AsstGetTasksList"), 1); p != (signatureParamJSON{Position: 2,
		CName: "buff", GoName: "buff", CType: "int32*", GoType: "*int32"}) {
		t.Errorf("AsstGetTasksList buff = %+v", p)
	}
	// 批准重命名 type→taskType 与 const char* ↔ *byte。
	if p := param(symbol(linuxRep, "AsstAppendTask"), 1); p != (signatureParamJSON{Position: 2,
		CName: "type", GoName: "taskType", CType: "const char*", GoType: "*byte"}) {
		t.Errorf("AsstAppendTask type = %+v", p)
	}
	// opaque handle ↔ uintptr 与 callback 函数指针 ↔ uintptr、
	// custom_arg token ↔ uintptr。
	if p := param(symbol(linuxRep, "AsstDestroy"), 0); p != (signatureParamJSON{Position: 1,
		CName: "handle", GoName: "handle", CType: "struct AsstExtAPI*", GoType: "uintptr"}) {
		t.Errorf("AsstDestroy handle = %+v", p)
	}
	ce := symbol(linuxRep, "AsstCreateEx")
	if p := param(ce, 0); p.GoType != "uintptr" || !strings.HasPrefix(p.CType, "void(") {
		t.Errorf("AsstCreateEx callback = %+v", p)
	}
	if p := param(ce, 1); p != (signatureParamJSON{Position: 2, CName: "custom_arg",
		GoName: "customArg", CType: "void*", GoType: "uintptr"}) {
		t.Errorf("AsstCreateEx custom_arg = %+v", p)
	}
	// void 返回与 const char* 返回。
	if lg := symbol(linuxRep, "AsstLog"); lg.Return != (signatureReturnJSON{CType: "void", GoType: ""}) {
		t.Errorf("AsstLog return = %+v", lg.Return)
	}
	if gv := symbol(linuxRep, "AsstGetVersion"); gv.Return != (signatureReturnJSON{
		CType: "const char*", GoType: "*byte"}) {
		t.Errorf("AsstGetVersion return = %+v", gv.Return)
	}

	// callback 平台适配:unix 原样 void,windows 许可适配 uintptr。
	if len(linuxRep.Signatures.Callbacks) != 2 {
		t.Fatalf("callbacks = %d, want 2", len(linuxRep.Signatures.Callbacks))
	}
	uc, wc := linuxRep.Signatures.Callbacks[0], linuxRep.Signatures.Callbacks[1]
	if uc.Name != "unixCallback" || uc.GoFile != "callback_types.go" || uc.GoLine <= 0 {
		t.Errorf("unixCallback record = %+v", uc)
	}
	wantCallbackParams := []signatureParamJSON{
		{Position: 1, CName: "msg", GoName: "msg", CType: "int32", GoType: "int32"},
		{Position: 2, CName: "details_json", GoName: "detailsJSON", CType: "const char*", GoType: "*byte"},
		{Position: 3, CName: "custom_arg", GoName: "customArg", CType: "void*", GoType: "uintptr"},
	}
	for i, w := range wantCallbackParams {
		if uc.Params[i] != w {
			t.Errorf("unixCallback param %d = %+v, want %+v", i, uc.Params[i], w)
		}
		if wc.Params[i] != w {
			t.Errorf("windowsCallback param %d = %+v, want %+v", i, wc.Params[i], w)
		}
	}
	if uc.Return != (signatureReturnJSON{CType: "void", GoType: ""}) {
		t.Errorf("unixCallback return = %+v", uc.Return)
	}
	if wc.Name != "windowsCallback" || wc.Return != (signatureReturnJSON{CType: "void", GoType: "uintptr"}) {
		t.Errorf("windowsCallback record = %+v", wc)
	}

	// linux 目标:窗口符号属 NonTarget 的 windowsOnly 绑定,同样被
	// 逐参数核验并出现在核验记录里。
	awl := symbol(linuxRep, "AsstAsyncAttachWindow")
	if p := param(awl, 1); p != (signatureParamJSON{Position: 2, CName: "hwnd", GoName: "hwnd",
		CType: "void*", GoType: "uintptr"}) {
		t.Errorf("linux AsstAsyncAttachWindow hwnd = %+v", p)
	}
	if p := param(awl, 2); p != (signatureParamJSON{Position: 3, CName: "screencap_method",
		GoName: "screencapMethod", CType: "uint64", GoType: "uint64"}) {
		t.Errorf("linux AsstAsyncAttachWindow screencap_method = %+v", p)
	}

	// windows 目标:26 条绑定全部通过,窗口符号经 Bound 分类核验,
	// hwnd/method 映射与 linux 目标一致,不得重复报错或行为回退。
	winRep := parse(run("windows"), "windows")
	if len(winRep.Signatures.Symbols) != 28 {
		t.Fatalf("windows signature symbols = %d, want 28", len(winRep.Signatures.Symbols))
	}
	aw := symbol(winRep, "AsstAsyncAttachWindow")
	if p := param(aw, 1); p != (signatureParamJSON{Position: 2, CName: "hwnd", GoName: "hwnd",
		CType: "void*", GoType: "uintptr"}) {
		t.Errorf("AsstAsyncAttachWindow hwnd = %+v", p)
	}
	if p := param(aw, 2); p != (signatureParamJSON{Position: 3, CName: "screencap_method",
		GoName: "screencapMethod", CType: "uint64", GoType: "uint64"}) {
		t.Errorf("AsstAsyncAttachWindow screencap_method = %+v", p)
	}
	if p := param(aw, 5); p != (signatureParamJSON{Position: 6, CName: "block", GoName: "block",
		CType: "uint8", GoType: "uint8"}) {
		t.Errorf("AsstAsyncAttachWindow block = %+v", p)
	}
}
