// goextract_test.go 用合成 native 包校验冻结形式的 Go 提取:
// 正例记录字段/条目/排除/callback 及来源位置;形式、词表、遮蔽、
// 重复声明等失败向量均以 go-syntax 类诊断失败并指向声明位置。

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// syntheticNativeDir 是符合冻结形式的最小 native 包:四个 raw
// 字段、四条符号条目、一条排除与两个 callback 类型。
var syntheticNativeDir = map[string]string{
	"api.go": `package native

import "unsafe"

type api struct {
	CreateEx          func(callback uintptr, customArg uintptr) uintptr
	GetImage          func(handle uintptr, buff unsafe.Pointer, buffSize uint64) uint64
	Log               func(level *byte, message *byte)
	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32
}
`,
	"symbols.go": `package native

type symbolPolicy uint8

const (
	required symbolPolicy = iota
	optional
)

type symbolPlatform uint8

const (
	allPlatforms symbolPlatform = iota
	windowsOnly
)

type symbolSpec struct {
	Name     string
	Target   any
	Policy   symbolPolicy
	Platform symbolPlatform
}

func symbolSpecs(table *api) []symbolSpec {
	return []symbolSpec{
		{Name: "AsstCreateEx", Target: &table.CreateEx, Policy: required, Platform: allPlatforms},
		{Name: "AsstGetImage", Target: &table.GetImage, Policy: required, Platform: allPlatforms},
		{Name: "AsstLog", Target: &table.Log, Policy: required, Platform: allPlatforms},
		{Name: "AsstAsyncAttachWindow", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},
	}
}

type exclusionSpec struct {
	Name   string
	Header string
	Reason string
}

var exclusions = []exclusionSpec{
	{Name: "AsstConnect", Header: "AsstCaller.h", Reason: "deprecated"},
	{Name: "AsstGetMapLevelKey", Header: "AsstCallerExtra.h", Reason: "experimental; struct return"},
	{Name: "AsstGetItemName", Header: "AsstCallerExtra.h", Reason: "experimental; no stability guarantee"},
}
`,
	"callback_types.go": `package native

type unixCallback func(msg int32, detailsJSON *byte, customArg uintptr)

type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr) uintptr
`,
}

// syntheticSymbolsGo 与 syntheticCallbacksGo 供拼接重复声明用。
const syntheticSymbolsBody = `package native

type symbolPolicy uint8

const (
	required symbolPolicy = iota
	optional
)

type symbolPlatform uint8

const (
	allPlatforms symbolPlatform = iota
	windowsOnly
)

func symbolSpecs(table *api) []symbolSpec {
	return []symbolSpec{
		{Name: "AsstCreateEx", Target: &table.CreateEx, Policy: required, Platform: allPlatforms},
	}
}

var exclusions = []exclusionSpec{
	{Name: "AsstConnect", Header: "AsstCaller.h", Reason: "deprecated"},
}
`

// writeGoRoot 把文件集合写成 go-root 布局并返回 go-root 绝对路径。
func writeGoRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(goNativeRelPath), name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// mustSelect 在目标上执行文件选择;任何失败都 Fatal。
func mustSelect(t *testing.T, goRoot string, target Target) *GoSelection {
	t.Helper()
	sel, diags := SelectGoFiles(goRoot, target)
	if len(diags) != 0 {
		t.Fatalf("SelectGoFiles unexpectedly failed: %v", diags)
	}
	return sel
}

// manualSelection 跳过文件选择,直接按写入的文件集合构造选择结果:
// 供缺失声明与解析错误等不经由选择的向量使用。
func manualSelection(t *testing.T, files map[string]string) *GoSelection {
	t.Helper()
	root := writeGoRoot(t, files)
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	dir := filepath.Join(root, filepath.FromSlash(goNativeRelPath))
	sel := &GoSelection{Dir: dir}
	for _, n := range names {
		sel.Files = append(sel.Files, GoSourceFile{Name: n, Path: filepath.Join(dir, n)})
	}
	return sel
}

// mustExtractGo 对合成包执行完整 Go 提取;任何失败都 Fatal。
func mustExtractGo(t *testing.T, files map[string]string) *GoExtraction {
	t.Helper()
	sel := mustSelect(t, writeGoRoot(t, files), linuxTarget)
	ext, diags := ExtractGo(sel)
	if len(diags) != 0 {
		t.Fatalf("ExtractGo unexpectedly failed: %v", diags)
	}
	return ext
}

// replaceGoBlock 把以 header 开头的顶层块(函数/struct/var 字面量,
// 至下一个行首的 "}")整体替换为 newBlock;header 不存在即失败。
func replaceGoBlock(t *testing.T, content, header, newBlock string) string {
	t.Helper()
	i := strings.Index(content, header)
	if i < 0 {
		t.Fatalf("block header %q not found", header)
	}
	j := strings.Index(content[i:], "\n}\n")
	if j < 0 {
		t.Fatalf("block closing brace after %q not found", header)
	}
	return content[:i] + newBlock + content[i+j+len("\n}\n"):]
}

// copySynthetic 返回合成包的深拷贝。
func copySynthetic() map[string]string {
	files := make(map[string]string, len(syntheticNativeDir))
	for k, v := range syntheticNativeDir {
		files[k] = v
	}
	return files
}

// TestExtractGoPositive 覆盖正例记录:签名、规范化类型、条目、排除、
// callback 与来源位置。
func TestExtractGoPositive(t *testing.T) {
	ext := mustExtractGo(t, copySynthetic())
	if ext.PackageName != "native" {
		t.Errorf("package = %q, want native", ext.PackageName)
	}
	if len(ext.Fields) != 4 {
		t.Fatalf("fields = %d, want 4", len(ext.Fields))
	}
	ce := ext.Fields[0]
	if ce.Name != "CreateEx" || ce.File != "api.go" || ce.Line <= 0 {
		t.Errorf("CreateEx record = %+v", ce)
	}
	if len(ce.Params) != 2 || ce.Params[0].Name != "callback" || ce.Params[0].Type != "uintptr" ||
		ce.Params[1].Name != "customArg" || ce.Result != "uintptr" {
		t.Errorf("CreateEx signature = %+v", ce)
	}
	gi := ext.Fields[1]
	if gi.Params[1].Type != GoTypeUnsafePointer || gi.Result != "uint64" {
		t.Errorf("GetImage signature = %+v", gi)
	}
	lg := ext.Fields[2]
	if lg.Result != "" || lg.Params[0].Type != "*byte" {
		t.Errorf("Log signature = %+v (result %q)", lg, lg.Result)
	}

	if len(ext.Specs) != 4 {
		t.Fatalf("specs = %d, want 4", len(ext.Specs))
	}
	first := ext.Specs[0]
	if first.Name != "AsstCreateEx" || first.Field != "CreateEx" ||
		first.Policy != "required" || first.Platform != "allPlatforms" ||
		first.File != "symbols.go" || first.Line <= 0 {
		t.Errorf("first spec = %+v", first)
	}
	last := ext.Specs[3]
	if last.Name != "AsstAsyncAttachWindow" || last.Policy != "optional" || last.Platform != "windowsOnly" {
		t.Errorf("last spec = %+v", last)
	}
	if len(ext.Exclusions) != 3 || ext.Exclusions[0].Name != "AsstConnect" ||
		ext.Exclusions[0].Header != "AsstCaller.h" || ext.Exclusions[0].Reason == "" {
		t.Errorf("exclusions = %+v", ext.Exclusions)
	}
	if ext.UnixCallback == nil || ext.UnixCallback.Result != "" ||
		len(ext.UnixCallback.Params) != 3 || ext.UnixCallback.Params[1].Type != "*byte" {
		t.Errorf("unixCallback = %+v", ext.UnixCallback)
	}
	if ext.WindowsCallback == nil || ext.WindowsCallback.Result != "uintptr" {
		t.Errorf("windowsCallback = %+v", ext.WindowsCallback)
	}
}

// goMutation 表驱动向量:mutate 修改合成包(或删除文件),expect 为
// 诊断必含片段。除标注 manual 的向量外都经由真实文件选择。
type goMutation struct {
	name   string
	mutate func(t *testing.T, files map[string]string)
	expect string
	manual bool // 绕过 SelectGoFiles(缺失声明/解析错误向量)
}

func TestExtractGoFormFailures(t *testing.T) {
	cases := []goMutation{
		{"anonymous parameter", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "CreateEx          func(callback uintptr",
				"CreateEx          func(uintptr")
		}, "exactly one explicit name", false},
		{"grouped field names", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"	CreateEx          func(callback uintptr, customArg uintptr) uintptr",
				"	CreateEx, GetImage func(callback uintptr, customArg uintptr) uintptr")
		}, "grouped field names", false},
		{"embedded api field", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "	CreateEx          func",
				"	otherSymbolSpec\n	CreateEx          func")
		}, "embedded", false},
		{"variadic parameter", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "Log               func(level *byte, message *byte)",
				"Log               func(level *byte, messages ...*byte)")
		}, "variadic", false},
		{"named result", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"buff unsafe.Pointer, buffSize uint64) uint64",
				"buff unsafe.Pointer, buffSize uint64) (n uint64)")
		}, "named result", false},
		{"multiple results", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"buff unsafe.Pointer, buffSize uint64) uint64",
				"buff unsafe.Pointer, buffSize uint64) (uint64, uint8)")
		}, "multiple results", false},
		{"bool parameter", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "callback uintptr", "callback bool")
		}, "frozen vocabulary", false},
		{"pointer to uint64 parameter", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "buffSize uint64) uint64", "buffSize *uint64) uint64")
		}, "frozen vocabulary", false},
		{"uint16 parameter", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "handle uintptr, buff unsafe.Pointer",
				"handle uint16, buff unsafe.Pointer")
		}, "frozen vocabulary", false},
		{"bool result", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "customArg uintptr) uintptr", "customArg uintptr) bool")
		}, "frozen vocabulary", false},
		{"unsafe usage without import", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "import \"unsafe\"\n\n", "")
		}, "unsafe", false},
		{"aliased unsafe import", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "import \"unsafe\"", "import u \"unsafe\"")
			fs["api.go"] = mustReplace(t, fs["api.go"], "buff unsafe.Pointer", "buff u.Pointer")
		}, "aliased import of unsafe", false},
		{"import alias shadows raw name", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"], "import \"unsafe\"", "import unsafe \"strings\"")
		}, "shadows", false},
		{"package-level shadow of byte", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "type symbolPolicy uint8",
				"type byte int8\n\ntype symbolPolicy uint8")
		}, "byte", false},
		{"package clause mismatch", func(t *testing.T, fs map[string]string) {
			fs["extra.go"] = "package other\n"
		}, "package", true},
		{"parse error", func(t *testing.T, fs map[string]string) {
			fs["extra.go"] = "package native\n\nfunc broken(\n"
		}, "does not parse", true},
		{"symbolSpecs renamed parameter", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "func symbolSpecs(table *api)",
				"func symbolSpecs(t *api)")
		}, "table", false},
		{"symbolSpecs non-api parameter", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "func symbolSpecs(table *api)",
				"func symbolSpecs(table *exclusionSpec)")
		}, "*api parameter", false},
		{"symbolSpecs wrong return", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "func symbolSpecs(table *api) []symbolSpec {",
				"func symbolSpecs(table *api) []exclusionSpec {")
		}, "[]symbolSpec", false},
		{"symbolSpecs extra statement", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "	return []symbolSpec{",
				"	_ = table\n	return []symbolSpec{")
		}, "exactly one return", false},
		{"symbolSpecs empty literal", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = replaceGoBlock(t, fs["symbols.go"], "func symbolSpecs(table *api) []symbolSpec {",
				"func symbolSpecs(table *api) []symbolSpec {\n\treturn []symbolSpec{}\n}")
		}, "empty symbol list", false},
		{"symbolSpecs missing key", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms},",
				"{Name: \"AsstLog\", Target: &table.Log, Policy: required},")
		}, "Platform", false},
		{"symbolSpecs extra key", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms},",
				"{Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms, Extra: 1},")
		}, "extra key", false},
		{"symbolSpecs duplicate key", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms},",
				"{Name: \"AsstLog\", Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms},")
		}, "twice", false},
		{"symbolSpecs name not literal", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "{Name: \"AsstLog\",",
				"{Name: asstLogName,")
		}, "Name", false},
		{"symbolSpecs name concatenation", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "{Name: \"AsstLog\",",
				"{Name: \"Asst\" + \"Log\",")
		}, "Name", false},
		{"symbolSpecs target without address-of", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Target: &table.Log,", "Target: table.Log,")
		}, "Target", false},
		{"symbolSpecs target indirect receiver", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Target: &table.Log,", "Target: &other.Log,")
		}, "Target", false},
		{"symbolSpecs policy string literal", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Policy: required,", "Policy: \"required\",")
		}, "Policy", false},
		{"symbolSpecs unknown policy constant", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Policy: required,", "Policy: mandatory,")
		}, "mandatory", false},
		{"symbolSpecs policy without declared type", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "	required symbolPolicy = iota", "	required = iota")
		}, "unexpected type", false},
		{"symbolSpecs platform uses policy constant", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Policy: required, Platform: allPlatforms},",
				"Policy: required, Platform: required},")
		}, "Platform", false},
		{"exclusions not a literal", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "var exclusions = []exclusionSpec{",
				"var exclusions = exclusionList()\n\nvar unused = []exclusionSpec{")
		}, "exclusions", false},
		{"exclusions declared type mismatch", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "var exclusions = []exclusionSpec{",
				"var exclusions = []symbolSpec{")
		}, "[]exclusionSpec", false},
		{"exclusions empty literal", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = replaceGoBlock(t, fs["symbols.go"], "var exclusions = []exclusionSpec{",
				"var exclusions = []exclusionSpec{}")
		}, "empty exclusion list", false},
		{"exclusions header with path", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Header: \"AsstCaller.h\"",
				"Header: \"upstream/AsstCaller.h\"")
		}, "bare file name", false},
		{"exclusions empty reason", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Reason: \"deprecated\"", "Reason: \"\"")
		}, "Reason", false},
		{"exclusions missing key", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstConnect\", Header: \"AsstCaller.h\", Reason: \"deprecated\"},",
				"{Name: \"AsstConnect\", Header: \"AsstCaller.h\"},")
		}, "Reason", false},
		{"exclusions positional element", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"{Name: \"AsstConnect\", Header: \"AsstCaller.h\", Reason: \"deprecated\"},",
				"{Name: \"AsstConnect\", \"AsstCaller.h\", \"deprecated\"},")
		}, "keyed fields", false},
		{"unix callback with result", func(t *testing.T, fs map[string]string) {
			fs["callback_types.go"] = mustReplace(t, fs["callback_types.go"],
				"type unixCallback func(msg int32, detailsJSON *byte, customArg uintptr)",
				"type unixCallback func(msg int32, detailsJSON *byte, customArg uintptr) uint8")
		}, "unixCallback", false},
		{"windows callback without result", func(t *testing.T, fs map[string]string) {
			fs["callback_types.go"] = mustReplace(t, fs["callback_types.go"],
				"type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr) uintptr",
				"type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr)")
		}, "windowsCallback", false},
		{"windows callback wrong result", func(t *testing.T, fs map[string]string) {
			fs["callback_types.go"] = mustReplace(t, fs["callback_types.go"],
				"type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr) uintptr",
				"type windowsCallback func(msg int32, detailsJSON *byte, customArg uintptr) int32")
		}, "windowsCallback", false},
		{"duplicate api struct", func(t *testing.T, fs map[string]string) {
			fs["extra.go"] = "package native\n\ntype api struct {\n\tX func() uintptr\n}\n"
		}, "duplicate", false},
		{"duplicate symbolSpecs", func(t *testing.T, fs map[string]string) {
			fs["extra.go"] = "package native\n\n" + strings.ReplaceAll(
				syntheticSymbolsBody, "package native\n\n", "")
		}, "duplicate", false},
		{"duplicate exclusions", func(t *testing.T, fs map[string]string) {
			fs["extra.go"] = "package native\n\nvar exclusions = []exclusionSpec{\n\t{Name: \"X\", Header: \"Y.h\", Reason: \"z\"},\n}\n"
		}, "duplicate", false},
		{"missing callback_types.go", func(t *testing.T, fs map[string]string) {
			delete(fs, "callback_types.go")
		}, "unixCallback", true},
		{"missing symbols.go", func(t *testing.T, fs map[string]string) {
			delete(fs, "symbols.go")
		}, "symbolSpecs", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := copySynthetic()
			tc.mutate(t, files)
			var sel *GoSelection
			if tc.manual {
				sel = manualSelection(t, files)
			} else {
				sel = mustSelect(t, writeGoRoot(t, files), linuxTarget)
			}
			ext, diags := ExtractGo(sel)
			if len(diags) == 0 {
				t.Fatalf("ExtractGo unexpectedly passed (fields=%d specs=%d); want failure mentioning %q",
					len(ext.Fields), len(ext.Specs), tc.expect)
			}
			d := diags[0]
			if d.Category != CatGoSyntax {
				t.Errorf("category = %q, want %q", d.Category, CatGoSyntax)
			}
			if d.File == "" {
				t.Errorf("diagnostic must locate a file, got %+v", d)
			}
			joined := d.Symbol + "\n" + d.Expected + "\n" + d.Actual + "\n" + d.Message
			if !strings.Contains(joined, tc.expect) {
				t.Errorf("diagnostic %+v does not mention %q", d, tc.expect)
			}
		})
	}
}

// TestExtractGoEmptyAPIStruct 空 api struct 是形式失败。
func TestExtractGoEmptyAPIStruct(t *testing.T) {
	files := copySynthetic()
	files["api.go"] = replaceGoBlock(t, files["api.go"], "type api struct {", "type api struct {}")
	sel := manualSelection(t, files)
	_, diags := ExtractGo(sel)
	if len(diags) == 0 || diags[0].Category != CatGoSyntax {
		t.Fatalf("empty api struct must fail with go-syntax, got %v", diags)
	}
	if !strings.Contains(diags[0].Actual, "empty struct") {
		t.Errorf("diagnostic = %+v", diags[0])
	}
}
