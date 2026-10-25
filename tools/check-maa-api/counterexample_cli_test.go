// counterexample_cli_test.go 把绑定设计 §9.1.2 反例目录中尚未达到完整
// 命令入口层的向量补齐为 Run() 级端到端反例,按目录类别以子测试归组:
// C 输入变体经测试 profile 注册表为改写后的文件计算独立摘要注册合成
// profile(生产注册表不动)后走完整 CLI 路径,Go 变体经复制后的真实
// native 目录改写构造。每个向量断言非零退出、stdout 无内容且无 PASS、
// stderr 含预期诊断类别与可定位信息。已由 main_test.go/realnative_test
// .go/compare_test.go 覆盖的向量不重复;对在更早层(摘要校验、
// c-syntax、go-syntax)即被拒绝的向量,如实钉死该拒绝层归属,不强行
// 移到比对层。feature=0 漏做 Extra 发现的路径由「实验发现发现集冻结」
// 关闭:发现 pass 恒以 ASST_WITH_EXTRA_CALLERS=1 运行且结果集被钉死,
// 任何扰动都以 c-syntax 拒绝(本文件实验发现组),正例基线的
// discovery=2 断言见 main_test.go TestRunSuccessBothTargets。

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mustFailThroughCLI 断言一次完整命令入口的反例契约:非零退出、stdout
// 无内容且无 PASS、stderr 含 profile/目标头、预期诊断类别与全部定位
// 片段。
func mustFailThroughCLI(t *testing.T, goos, cInput, goRoot, wantCat string, wantLocs ...string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run([]string{
		"--profile", testProfileID, "--goos", goos, "--goarch", "amd64",
		"--c-input", cInput, "--go-root", goRoot,
	}, &stdout, &stderr)
	if code == 0 {
		t.Fatalf("counterexample unexpectedly passed:\n%s", stdout.String())
	}
	if stdout.Len() != 0 || strings.Contains(stdout.String()+stderr.String(), "PASS") {
		t.Errorf("failure output must stay off stdout without PASS:\nstdout=%q stderr=%q",
			stdout.String(), stderr.String())
	}
	errOut := stderr.String()
	if !strings.Contains(errOut, wantCat) {
		t.Errorf("stderr must carry category %q:\n%s", wantCat, errOut)
	}
	for _, want := range wantLocs {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr must locate %q:\n%s", want, errOut)
		}
	}
	if !strings.Contains(errOut, testProfileID) || !strings.Contains(errOut, "("+goos+", amd64)") {
		t.Errorf("stderr must print the profile and target header:\n%s", errOut)
	}
}

// positiveCInput 构造正例合成 c 输入并注册对应测试 profile,供 Go 侧
// 变体组共用。
func positiveCInput(t *testing.T) string {
	t.Helper()
	files := positiveFiles(t)
	registerTestProfile(t, testProfileFromFiles(files))
	return writeCInputDir(t, files, nil)
}

// mustFailWithCMutation 对正例 C 文件做一处改写,为改写后的文件注册
// 独立摘要的测试 profile,再走完整命令入口断言失败契约。
func mustFailWithCMutation(t *testing.T, goos string, mutate func(t *testing.T, fs map[string]string),
	wantCat string, wantLocs ...string) {
	t.Helper()
	files := positiveFiles(t)
	mutate(t, files)
	registerTestProfile(t, testProfileFromFiles(files))
	dir := writeCInputDir(t, files, nil)
	mustFailThroughCLI(t, goos, dir, repoRoot(t), wantCat, wantLocs...)
}

// mustFailWithGoMutation 复制真实 native 目录、做一处改写后作为
// go-root 走完整命令入口断言失败契约。
func mustFailWithGoMutation(t *testing.T, goos, cInput string, mutate func(t *testing.T, fs map[string]string),
	wantCat string, wantLocs ...string) {
	t.Helper()
	files := copyRealNative(t)
	mutate(t, files)
	goRoot := writeMutatedGoRoot(t, files)
	mustFailThroughCLI(t, goos, cInput, goRoot, wantCat, wantLocs...)
}

// ---- 输入/目标类:摘要校验层未在 CLI 级覆盖的向量 ----

// TestCounterexampleCInputThroughCLI 钉死输入类的完整命令入口行为:
// 空文件、盘上内容与摘要不符、清单路径越界都在 input 类诊断下非零退
// 出并定位到具体文件/条目。
func TestCounterexampleCInputThroughCLI(t *testing.T) {
	t.Run("empty checker_input file", func(t *testing.T) {
		files := positiveFiles(t)
		files[FileAsstCallerExtra] = ""
		registerTestProfile(t, testProfileFromFiles(files))
		dir := writeCInputDir(t, files, nil)
		mustFailThroughCLI(t, "linux", dir, repoRoot(t), CatInput, "size", FileAsstCallerExtra)
	})
	t.Run("on-disk digest mismatch", func(t *testing.T) {
		files := positiveFiles(t)
		registerTestProfile(t, testProfileFromFiles(files))
		dir := writeCInputDir(t, files, nil)
		full := filepath.Join(dir, filepath.FromSlash(FileAsstCaller))
		orig, err := os.ReadFile(full)
		if err != nil {
			t.Fatal(err)
		}
		orig[len(orig)/2] ^= 0x20 // 同长度改写:只破坏摘要
		if err := os.WriteFile(full, orig, 0o644); err != nil {
			t.Fatal(err)
		}
		mustFailThroughCLI(t, "linux", dir, repoRoot(t), CatInput, "sha256", FileAsstCaller)
	})
	t.Run("manifest path escape", func(t *testing.T) {
		files := positiveFiles(t)
		registerTestProfile(t, testProfileFromFiles(files))
		dir := writeCInputDir(t, files, func(m *manifest) {
			for i := range m.Files {
				if m.Files[i].ArchivePath == FileAsstCaller {
					m.Files[i].ArchivePath = "../outside/AsstCaller.h"
				}
			}
		})
		mustFailThroughCLI(t, "linux", dir, repoRoot(t), CatInput, "archive_path")
	})
}

// ---- C 提取类:解析破坏样例经测试 profile 注册表走完整命令路径 ----

// TestCounterexampleCExtractionThroughCLI 是 C 提取类的 CLI 级验收:
// 未知 typedef/循环别名/未知条件(含非活跃分支)/GetVersion 特殊拼
// 写/ASST_CALL 改动/零参数函数加参/外部 ASST_CALL 预定义/多级指针
// 全部在 c-syntax 类诊断下非零退出并指向声明位置——不是停在摘要校验。
func TestCounterexampleCExtractionThroughCLI(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(t *testing.T, fs map[string]string)
		wantLoc string
	}{
		{"related typedef renamed to an unknown name", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"typedef uint64_t AsstSize;", "typedef uint64_t AsstSize;\ntypedef AsstSize AsstWeird;")
		}, "AsstWeird"},
		{"circular typedef alias", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"typedef int32_t AsstId;", "typedef AsstMsgId AsstId;")
		}, "AsstMsgId"},
		{"unknown condition in an inactive branch", func(t *testing.T, fs map[string]string) {
			fs[FileAsstPort] = mustReplace(t, fs[FileAsstPort],
				"#if defined(__WINE__) && defined(__x86_64__)",
				"#if defined(__WINE__) && defined(__i386__)")
		}, "condition"},
		{"GetVersion special spelling broken", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"ASSTAPI_PORT const char* ASST_CALL AsstGetVersion();",
				"ASSTAPI_PORT const char* AsstGetVersion();")
		}, "AsstGetVersion"},
		{"ASST_CALL right value changed", func(t *testing.T, fs map[string]string) {
			fs[FileAsstPort] = mustReplace(t, fs[FileAsstPort],
				"#define ASST_CALL __stdcall", "#define ASST_CALL __fastcall")
		}, "ASST_CALL"},
		{"known zero-arg function gains a parameter", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"AsstHandle ASSTAPI AsstCreate();", "AsstHandle ASSTAPI AsstCreate(AsstHandle handle);")
		}, "AsstCreate"},
		{"external ASST_CALL predefine", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = "#define ASST_CALL __stdcall\n\n" + fs[FileAsstCaller]
		}, "ASST_CALL"},
		{"multi-level pointer char**", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"char* buff", "char** buff")
		}, "char**"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustFailWithCMutation(t, "linux", tc.mutate, CatCSyntax, tc.wantLoc)
		})
	}
}

// ---- 覆盖/关系类:helper/计算构造与伪 unsafe 导入的 CLI 级向量 ----

// TestCounterexampleCoverageRelationThroughCLI 补齐覆盖/关系类在 CLI
// 级的缺口:Target 指向另一存在字段、helper/计算构造的清单、计算拼
// 接的符号名与别名 unsafe 导入均失败;漏绑/重复绑定/Name 拼错/不存在
// 字段/字段无条目的 Run() 级向量见 realnative_test.go,不在此重复。
func TestCounterexampleCoverageRelationThroughCLI(t *testing.T) {
	cInput := positiveCInput(t)
	cases := []struct {
		name    string
		mutate  func(t *testing.T, fs map[string]string)
		wantCat string
		wantLoc string
	}{
		// Target 指向另一个存在字段:字段存在性通过,前缀规则(字段
		// 名=符号去 Asst 前缀,锚定独立 C 输入)拒绝。
		{"target points to another existing field", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstStart\", Target: &table.Start,",
				"{Name: \"AsstStart\", Target: &table.Stop,")
		}, CatRelation, "Target field Start"},
		{"exclusion list built by a helper call", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = replaceGoBlock(t, fs["symbols.go"],
				"var exclusions = []exclusionSpec{", "var exclusions = exclusionList()")
		}, CatGoSyntax, "exclusions"},
		{"symbolSpecs body with statements before the return", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"func symbolSpecs(table *api) []symbolSpec {\n\treturn []symbolSpec{",
				"func symbolSpecs(table *api) []symbolSpec {\n\tlist := []symbolSpec{}\n\t_ = list\n\treturn []symbolSpec{")
		}, CatGoSyntax, "exactly one return"},
		{"entry name built by string concatenation", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstStart\",", "{Name: \"Asst\" + \"Start\",")
		}, CatGoSyntax, "Name"},
		{"aliased unsafe import", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "import \"unsafe\"", "import u \"unsafe\"")
			fs["api.go"] = mustReplace(t, fs["api.go"], "buff unsafe.Pointer", "buff u.Pointer")
		}, CatGoSyntax, "aliased import of unsafe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustFailWithGoMutation(t, "linux", cInput, tc.mutate, tc.wantCat, tc.wantLoc)
		})
	}
}

// ---- 精确排除类:重复/空原因/通配符/排除已绑定项与实验发现 ----

// TestCounterexampleExclusionThroughCLI 钉死精确排除类的完整命令入口
// 反例:重复排除、空原因、通配符样式排除、排除已绑定项,以及实验导
// 出删除/新增/脱离守卫——发现集被冻结,任何扰动都以 c-syntax 拒绝,
// 因此「feature=0 时漏做 Extra 发现」没有可通过的输入路径(发现 pass
// 恒以 =1 运行,正例基线的 discovery=2 断言见 main_test.go)。
func TestCounterexampleExclusionThroughCLI(t *testing.T) {
	cInput := positiveCInput(t)
	cases := []struct {
		name    string
		mutate  func(t *testing.T, fs map[string]string)
		wantCat string
		wantLoc string
	}{
		{"duplicate exclusion", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstConnect\", Header: \"AsstCaller.h\", Reason: \"上游 deprecated 注释；同步连接由 AsyncConnect 代替\"},",
				"{Name: \"AsstConnect\", Header: \"AsstCaller.h\", Reason: \"上游 deprecated 注释；同步连接由 AsyncConnect 代替\"},\n"+
					"{Name: \"AsstConnect\", Header: \"AsstCaller.h\", Reason: \"重复条目\"},")
		}, CatRelation, "excluded more than once"},
		{"empty exclusion reason", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"Reason: \"上游 deprecated 注释；同步连接由 AsyncConnect 代替\"", "Reason: \"\"")
		}, CatGoSyntax, "Reason"},
		{"wildcard-style exclusion", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"var exclusions = []exclusionSpec{",
				"var exclusions = []exclusionSpec{\n\t{Name: \"Asst*\", Header: \"AsstCaller.h\", Reason: \"通配符兜底\"},")
		}, CatRelation, "stale exclusion"},
		{"exclusion of an already-bound symbol", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"Log                 func(level *byte, message *byte)",
				"Log                 func(level *byte, message *byte)\n\tConnect             func(handle uintptr, adbPath *byte, address *byte, config *byte) uint8")
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms},",
				"{Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms},\n"+
					"{Name: \"AsstConnect\", Target: &table.Connect, Policy: required, Platform: allPlatforms},")
		}, CatRelation, "must not additionally appear"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustFailWithGoMutation(t, "linux", cInput, tc.mutate, tc.wantCat, tc.wantLoc)
		})
	}

	extraCases := []struct {
		name    string
		mutate  func(t *testing.T, fs map[string]string)
		wantLoc string
	}{
		{"experimental export deleted from the extra header", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCallerExtra] = mustReplace(t, fs[FileAsstCallerExtra],
				"    const char* ASSTAPI AsstGetItemName(const char* id);\n", "")
		}, "discovery"},
		{"new experimental export added inside the guard", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCallerExtra] = mustReplace(t, fs[FileAsstCallerExtra],
				"    const char* ASSTAPI AsstGetItemName(const char* id);",
				"    const char* ASSTAPI AsstGetItemName(const char* id);\n"+
					"    const char* ASSTAPI AsstGetItemCode(const char* id);")
		}, "discovery"},
		{"experimental guard removed", func(t *testing.T, fs map[string]string) {
			extra := fs[FileAsstCallerExtra]
			extra = strings.Replace(extra, "#if ASST_WITH_EXTRA_CALLERS\n\n", "", 1)
			extra = strings.Replace(extra, "#endif // ASST_WITH_EXTRA_CALLERS\n", "", 1)
			if strings.Contains(extra, "ASST_WITH_EXTRA_CALLERS") {
				t.Fatal("guard removal mutation did not apply")
			}
			fs[FileAsstCallerExtra] = extra
		}, "AsstGetMapLevelKey"},
	}
	for _, tc := range extraCases {
		t.Run(tc.name, func(t *testing.T) {
			mustFailWithCMutation(t, "linux", tc.mutate, CatCSyntax, tc.wantLoc)
		})
	}
}

// ---- 宽度/符号、指针/角色与参数次序类 ----

// TestCounterexampleSignatureThroughCLI 是宽度/符号、指针/角色与参数
// 次序类的 CLI 级补充向量:Go 侧改动用真实目录副本,C 侧改动经测试
// profile 注册表。词表外的拼写(uint8→bool、int32→int/uint32)在
// go-syntax 层被冻结词表拒绝;词表内但语义错误的改动到达签名比对层以
// signature 类拒绝。图像缓冲区→uintptr、const char*→uint8_t*、丢
// const、x/y 调序已由 compare_test.go 的 CLI 向量覆盖,不重复。窗口
// 符号的 method 调序在两目标下都必须以 signature 拒绝:linux 目标下
// 该绑定属 NonTarget,签名比对同样逐参数核验。
func TestCounterexampleSignatureThroughCLI(t *testing.T) {
	cInput := positiveCInput(t)
	cases := []struct {
		name    string
		mutate  func(t *testing.T, fs map[string]string)
		wantCat string
		wantLoc string
	}{
		{"uint8 scalar replaced by bool", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"SetUserDir          func(path *byte) uint8",
				"SetUserDir          func(path *byte) bool")
		}, CatGoSyntax, "frozen vocabulary"},
		{"int32 scalar replaced by int", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"SetTaskParams       func(handle uintptr, id int32, params *byte) uint8",
				"SetTaskParams       func(handle uintptr, id int, params *byte) uint8")
		}, CatGoSyntax, "frozen vocabulary"},
		{"int32 scalar replaced by uint32", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"SetTaskParams       func(handle uintptr, id int32, params *byte) uint8",
				"SetTaskParams       func(handle uintptr, id uint32, params *byte) uint8")
		}, CatGoSyntax, "frozen vocabulary"},
		{"void return fabricated as uint8", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"Log                 func(level *byte, message *byte)",
				"Log                 func(level *byte, message *byte) uint8")
		}, CatSignature, "AsstLog"},
		{"parameter added", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"Destroy             func(handle uintptr)",
				"Destroy             func(handle uintptr, extra uintptr)")
		}, CatSignature, "AsstDestroy"},
		{"parameter removed", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"Log                 func(level *byte, message *byte)",
				"Log                 func(level *byte)")
		}, CatSignature, "AsstLog"},
		{"char pointer return replaced by uintptr", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"GetVersion          func() *byte",
				"GetVersion          func() uintptr")
		}, CatSignature, "AsstGetVersion"},
		{"task list pointer replaced by unsafe.Pointer", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"GetTasksList        func(handle uintptr, buff *int32, buffSize uint64) uint64",
				"GetTasksList        func(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64")
		}, CatSignature, "AsstGetTasksList"},
		{"level/message reorder", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"Log                 func(level *byte, message *byte)",
				"Log                 func(message *byte, level *byte)")
		}, CatSignature, "parameter 1 name level"},
		{"callback/customArg reorder", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"CreateEx            func(callback uintptr, customArg uintptr) uintptr",
				"CreateEx            func(customArg uintptr, callback uintptr) uintptr")
		}, CatSignature, "parameter 1 name callback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustFailWithGoMutation(t, "linux", cInput, tc.mutate, tc.wantCat, tc.wantLoc)
		})
	}

	t.Run("window method reorder on the linux target (non-target binding)", func(t *testing.T) {
		mustFailWithGoMutation(t, "linux", cInput, func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"AsyncAttachWindow   func(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32",
				"AsyncAttachWindow   func(handle uintptr, hwnd uintptr, mouseMethod uint64, screencapMethod uint64, keyboardMethod uint64, block uint8) int32")
		}, CatSignature, "parameter 3 name screencapMethod")
	})

	t.Run("window method reorder on the windows target", func(t *testing.T) {
		mustFailWithGoMutation(t, "windows", cInput, func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"AsyncAttachWindow   func(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32",
				"AsyncAttachWindow   func(handle uintptr, hwnd uintptr, mouseMethod uint64, screencapMethod uint64, keyboardMethod uint64, block uint8) int32")
		}, CatSignature, "parameter 3 name screencapMethod")
	})

	t.Run("char* widened to char** on the c side", func(t *testing.T) {
		mustFailWithCMutation(t, "linux", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"char* buff", "char** buff")
		}, CatCSyntax, "char**")
	})
}

// ---- callback 类 ----

// TestCounterexampleCallbackThroughCLI 钉死 callback 类的完整命令入口
// 反例与拒绝层归属:Go 侧返回值形态破坏在 go-syntax 层被冻结形式拒
// 绝(比对层的等价向量由 compare_test.go 直接改动提取结果钉死);参数
// 调序/错类型到达签名比对层;C 侧 void 改非 void 与丢 ASST_CALL 在
// c-syntax 层被冻结拼写拒绝。
func TestCounterexampleCallbackThroughCLI(t *testing.T) {
	cInput := positiveCInput(t)
	cases := []struct {
		name    string
		mutate  func(t *testing.T, fs map[string]string)
		wantCat string
		wantLoc string
	}{
		{"unix callback gains a return", func(t *testing.T, fs map[string]string) {
			fs["callback_types.go"] = mustReplace(t, fs["callback_types.go"],
				"type unixCallback func(msg int32, detailsJSON *byte, customArg uintptr)",
				"type unixCallback func(msg int32, detailsJSON *byte, customArg uintptr) uint8")
		}, CatGoSyntax, "unixCallback"},
		{"windows callback loses its return", func(t *testing.T, fs map[string]string) {
			fs["callback_types.go"] = mustReplace(t, fs["callback_types.go"],
				"type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr) uintptr",
				"type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr)")
		}, CatGoSyntax, "windowsCallback"},
		{"windows callback returns non-uintptr", func(t *testing.T, fs map[string]string) {
			fs["callback_types.go"] = mustReplace(t, fs["callback_types.go"],
				"type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr) uintptr",
				"type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr) int32")
		}, CatGoSyntax, "windowsCallback"},
		{"callback parameter reorder", func(t *testing.T, fs map[string]string) {
			fs["callback_types.go"] = mustReplace(t, fs["callback_types.go"],
				"type unixCallback func(msg int32, detailsJSON *byte, customArg uintptr)",
				"type unixCallback func(msg int32, customArg uintptr, detailsJSON *byte)")
		}, CatSignature, "parameter 2 name detailsJSON"},
		{"callback parameter type error", func(t *testing.T, fs map[string]string) {
			fs["callback_types.go"] = mustReplace(t, fs["callback_types.go"],
				"type unixCallback func(msg int32, detailsJSON *byte, customArg uintptr)",
				"type unixCallback func(msg uint64, detailsJSON *byte, customArg uintptr)")
		}, CatSignature, "parameter 1 (msg)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mustFailWithGoMutation(t, "linux", cInput, tc.mutate, tc.wantCat, tc.wantLoc)
		})
	}

	cCases := []struct {
		name   string
		mutate func(t *testing.T, fs map[string]string)
	}{
		{"c callback return changed from void", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"typedef void(ASST_CALL* AsstApiCallback)",
				"typedef uint8_t(ASST_CALL* AsstApiCallback)")
		}},
		{"c callback drops ASST_CALL", func(t *testing.T, fs map[string]string) {
			fs[FileAsstCaller] = mustReplace(t, fs[FileAsstCaller],
				"typedef void(ASST_CALL* AsstApiCallback)",
				"typedef void(* AsstApiCallback)")
		}},
	}
	for _, tc := range cCases {
		t.Run(tc.name, func(t *testing.T) {
			mustFailWithCMutation(t, "linux", tc.mutate, CatCSyntax, "AsstApiCallback")
		})
	}
}

// ---- 文件选择类:正确副本不能掩盖生产错误 ----

// TestCounterexampleFileSelectionThroughCLI 钉死文件选择类的反例:生
// 产错误声明与不参与目标构建的正确副本并存(loader_linux/loader_other
// 形态)、正确副本隐藏于 tag、正确类型只放在 _test.go——生产文件照常
// 入选,错误照常以完整命令路径报出,绝不借正确副本通过。
func TestCounterexampleFileSelectionThroughCLI(t *testing.T) {
	cInput := positiveCInput(t)
	breakLog := func(t *testing.T, fs map[string]string) {
		fs["api.go"] = mustReplace(t, fs["api.go"],
			"Log                 func(level *byte, message *byte)",
			"Log                 func(level *byte, message *byte) int32")
	}
	// correctAPIStruct 取真实 api struct 原文(补回精确 unsafe 导入),
	// 供正确副本文件使用。
	correctAPIStruct := func(t *testing.T, api string) string {
		t.Helper()
		i := strings.Index(api, "type api struct {")
		j := strings.Index(api[i:], "\n}\n")
		if i < 0 || j < 0 {
			t.Fatal("cannot slice the production api struct")
		}
		return "package native\n\nimport \"unsafe\"\n\n" + api[i:i+j+len("\n}\n")]
	}

	t.Run("correct copy behind a platform tag does not rescue the linux target", func(t *testing.T) {
		files := copyRealNative(t)
		breakLog(t, files)
		files["api_other.go"] = "//go:build !linux\n\n" + correctAPIStruct(t, files["api.go"])
		goRoot := writeMutatedGoRoot(t, files)
		// linux:api_other.go 不入选,坏声明单独入选 → signature;
		// windows:两份 api struct 同时入选 → duplicate(go-syntax)。
		mustFailThroughCLI(t, "linux", cInput, goRoot, CatSignature, "AsstLog")
		mustFailThroughCLI(t, "windows", cInput, goRoot, CatGoSyntax, "duplicate")
	})
	t.Run("correct copy hidden by ignore tag does not rescue", func(t *testing.T) {
		files := copyRealNative(t)
		breakLog(t, files)
		files["zzfix.go"] = "//go:build ignore\n\n" + correctAPIStruct(t, files["api.go"])
		goRoot := writeMutatedGoRoot(t, files)
		mustFailThroughCLI(t, "linux", cInput, goRoot, CatSignature, "AsstLog")
	})
	t.Run("correct type only in a _test.go file does not rescue", func(t *testing.T) {
		files := copyRealNative(t)
		breakLog(t, files)
		files["zzfix_test.go"] = correctAPIStruct(t, files["api.go"])
		goRoot := writeMutatedGoRoot(t, files)
		mustFailThroughCLI(t, "linux", cInput, goRoot, CatSignature, "AsstLog")
	})
}

// ---- 独立性类:Go 侧自洽不能替代独立 C 输入 ----

// TestCounterexampleIndependenceThroughCLI 钉死独立性类:同时修改 Go
// 字段与其符号条目使 Go 侧内部一致(字段名/Target 同步改名、字段签名
// 与条目无冲突),只要偏离独立 C 输入与 checker 冻结的内部常量仍然失
// 败——期望表与签名映射不从被检查的 Go 文件推导。策略方向(R↔O)的
// 同类向量已由 realnative_test.go 的 policy 组覆盖,不重复。
func TestCounterexampleIndependenceThroughCLI(t *testing.T) {
	cInput := positiveCInput(t)
	t.Run("field and target renamed consistently still fail on the c-anchored prefix rule", func(t *testing.T) {
		mustFailWithGoMutation(t, "linux", cInput, func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"Log                 func(level *byte, message *byte)",
				"Logger              func(level *byte, message *byte)")
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstLog\", Target: &table.Log,",
				"{Name: \"AsstLog\", Target: &table.Logger,")
		}, CatRelation, "Target field Log")
	})
	t.Run("go-consistent parameter reorder still fails the frozen signature mapping", func(t *testing.T) {
		mustFailWithGoMutation(t, "linux", cInput, func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"GetImage            func(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64",
				"GetImage            func(handle uintptr, buffSize uint64, buff unsafe.Pointer) uint64")
		}, CatSignature, "parameter 2 name buff")
	})
}
