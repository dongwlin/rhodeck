// golink_test.go 用合成 C 提取结果与合成 Go 提取结果直接校验 C↔Go
// 一对一关联:正例给出 linux/windows 两种目标的分类表;未知导出、
// 漏绑、陈旧/重复排除、重复绑定、未知字段、多余字段、平台矛盾与
// 实验发现处置错误均以 relation 类诊断失败。

package main

import (
	"strings"
	"testing"
)

// linkCExtraction 构造与合成 Go 包对应的 C 提取结果:4 个 all 平台
// 导出、1 个 windows 条件导出、2 个实验导出与 1 个 all 平台排除项。
// 适用集合按目标过滤(与 ExtractC 的语义一致)。
func linkCExtraction(target Target) *CExtraction {
	all := func(name string) CFunction {
		return CFunction{Name: name, Header: "upstream/AsstCaller.h", Line: 10,
			Platform: PlatformAll}
	}
	functions := []CFunction{
		all("AsstCreateEx"), all("AsstGetImage"), all("AsstLog"), all("AsstConnect"),
		{Name: "AsstAsyncAttachWindow", Header: "upstream/AsstCaller.h", Line: 20,
			Platform: PlatformWindows},
		{Name: "AsstGetMapLevelKey", Header: "upstream/AsstCallerExtra.h", Line: 30,
			Platform: PlatformExtra},
		{Name: "AsstGetItemName", Header: "upstream/AsstCallerExtra.h", Line: 40,
			Platform: PlatformExtra},
	}
	discovery := []CFunction{
		{Name: "AsstGetMapLevelKey", Header: "upstream/AsstCallerExtra.h", Line: 30,
			Platform: PlatformExtra},
		{Name: "AsstGetItemName", Header: "upstream/AsstCallerExtra.h", Line: 40,
			Platform: PlatformExtra},
	}
	var applicable []CFunction
	for _, f := range functions {
		if f.Platform == PlatformAll || (f.Platform == PlatformWindows && target.GOOS == "windows") {
			applicable = append(applicable, f)
		}
	}
	return &CExtraction{Functions: functions, Applicable: applicable, Discovery: discovery}
}

func mustLink(t *testing.T, c *CExtraction, g *GoExtraction, target Target) *LinkResult {
	t.Helper()
	res, diags := LinkCGo(c, g, target)
	if len(diags) != 0 {
		t.Fatalf("LinkCGo unexpectedly failed: %v", diags)
	}
	return res
}

// TestLinkPositive 验证两个目标下的分类:linux 适用绑定为 3 条、
// windowsOnly 条目归入非目标清单、排除 3 条;windows 下 4 条全部绑定。
func TestLinkPositive(t *testing.T) {
	g := mustExtractGo(t, copySynthetic())

	linuxRes := mustLink(t, linkCExtraction(linuxTarget), g, linuxTarget)
	bound := make([]string, 0, len(linuxRes.Bound))
	for _, b := range linuxRes.Bound {
		bound = append(bound, b.Symbol)
	}
	if strings.Join(bound, ",") != "AsstCreateEx,AsstGetImage,AsstLog" {
		t.Errorf("linux bound = %v", bound)
	}
	if strings.Join(linuxRes.NonTarget, ",") != "AsstAsyncAttachWindow" {
		t.Errorf("linux nontarget = %v", linuxRes.NonTarget)
	}
	if strings.Join(linuxRes.Excluded, ",") != "AsstConnect,AsstGetItemName,AsstGetMapLevelKey" {
		t.Errorf("linux excluded = %v", linuxRes.Excluded)
	}
	if linuxRes.Bound[0].Policy != "required" || linuxRes.Bound[0].Field != "CreateEx" {
		t.Errorf("bound record = %+v", linuxRes.Bound[0])
	}

	winRes := mustLink(t, linkCExtraction(windowsTarget), g, windowsTarget)
	if len(winRes.Bound) != 4 || strings.Join(winRes.NonTarget, ",") != "" {
		t.Errorf("windows result = bound %d nontarget %v", len(winRes.Bound), winRes.NonTarget)
	}
}

// mustLinkFail 断言关联以 relation 类诊断失败且诊断指向期望片段。
func mustLinkFail(t *testing.T, c *CExtraction, g *GoExtraction, target Target, want string) Diagnostic {
	t.Helper()
	res, diags := LinkCGo(c, g, target)
	if len(diags) == 0 {
		t.Fatalf("LinkCGo unexpectedly passed (bound=%d); want failure mentioning %q",
			len(res.Bound), want)
	}
	d := diags[0]
	if d.Category != CatRelation {
		t.Errorf("category = %q, want %q", d.Category, CatRelation)
	}
	joined := d.Symbol + "\n" + d.Expected + "\n" + d.Message
	if !strings.Contains(joined, want) {
		t.Errorf("diagnostic %+v does not mention %q", d, want)
	}
	return d
}

// TestLinkFailures 是关联阶段的失败向量。
func TestLinkFailures(t *testing.T) {
	cases := []struct {
		name   string
		mutC   func(t *testing.T, c *CExtraction)
		mutGo  func(t *testing.T, files map[string]string)
		target Target
		want   string
	}{
		{"applicable export without binding", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"		{Name: \"AsstGetImage\", Target: &table.GetImage, Policy: required, Platform: allPlatforms},\n", "")
			}, linuxTarget, "no symbolSpec entry"},
		{"unknown export on the c side only", func(t *testing.T, c *CExtraction) {
			c.Functions = append(c.Functions, CFunction{Name: "AsstNewThing",
				Header: "upstream/AsstCaller.h", Line: 99, Platform: PlatformAll})
			c.Applicable = append(c.Applicable, CFunction{Name: "AsstNewThing",
				Header: "upstream/AsstCaller.h", Line: 99, Platform: PlatformAll})
		}, func(t *testing.T, fs map[string]string) {}, linuxTarget, "no symbolSpec binding"},
		{"bound symbol missing from c", func(t *testing.T, c *CExtraction) {
			var out []CFunction
			for _, f := range c.Functions {
				if f.Name != "AsstGetImage" {
					out = append(out, f)
				}
			}
			c.Functions = out
		}, func(t *testing.T, fs map[string]string) {}, linuxTarget, "no C export"},
		{"target references unknown field", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Target: &table.Log,",
					"Target: &table.Nope,")
			}, linuxTarget, "does not exist"},
		{"target field name breaks the prefix rule", func(t *testing.T, c *CExtraction) {
			c.Functions = append(c.Functions, CFunction{Name: "AsstOtherThing",
				Header: "upstream/AsstCaller.h", Line: 50, Platform: PlatformAll})
			c.Applicable = append(c.Applicable, CFunction{Name: "AsstOtherThing",
				Header: "upstream/AsstCaller.h", Line: 50, Platform: PlatformAll})
		}, func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"	CreateEx          func(callback uintptr, customArg uintptr) uintptr",
				"	Weird             func(callback uintptr, customArg uintptr) uintptr\n	CreateEx          func(callback uintptr, customArg uintptr) uintptr")
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"		{Name: \"AsstCreateEx\", Target: &table.CreateEx, Policy: required, Platform: allPlatforms},",
				"		{Name: \"AsstOtherThing\", Target: &table.Weird, Policy: required, Platform: allPlatforms},")
		}, linuxTarget, "Asst prefix"},
		{"field without entry", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["api.go"] = mustReplace(t, fs["api.go"],
					"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32",
					"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32\n\tExtra             func(handle uintptr) uint8")
			}, linuxTarget, "no symbolSpec entry"},
		{"stale exclusion", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Name: \"AsstConnect\",",
					"Name: \"AsstConnectOld\",")
			}, linuxTarget, "stale exclusion"},
		{"exclusion header mismatch", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Header: \"AsstCaller.h\"",
					"Header: \"AsstPort.h\"")
			}, linuxTarget, "does not match the C declaration source header"},
		{"duplicate spec entry", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"		{Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms},",
					"		{Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms},\n		{Name: \"AsstLog\", Target: &table.Log, Policy: required, Platform: allPlatforms},")
			}, linuxTarget, "more than once"},
		{"duplicate exclusion", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"	{Name: \"AsstConnect\", Header: \"AsstCaller.h\", Reason: \"deprecated\"},",
					"	{Name: \"AsstConnect\", Header: \"AsstCaller.h\", Reason: \"deprecated\"},\n	{Name: \"AsstConnect\", Header: \"AsstCaller.h\", Reason: \"deprecated again\"},")
			}, linuxTarget, "more than once"},
		{"excluded export also bound", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["api.go"] = mustReplace(t, fs["api.go"],
					"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32",
					"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32\n\tConnect           func(path *byte) uint8")
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"		{Name: \"AsstAsyncAttachWindow\", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},",
					"		{Name: \"AsstAsyncAttachWindow\", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},\n		{Name: \"AsstConnect\", Target: &table.Connect, Policy: required, Platform: allPlatforms},")
			}, linuxTarget, "must not additionally appear"},
		{"windows export bound as all-platform", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"Policy: optional, Platform: windowsOnly},",
					"Policy: optional, Platform: allPlatforms},")
			}, linuxTarget, "windows-conditional"},
		{"all-platform export bound as windows-only", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"{Name: \"AsstCreateEx\", Target: &table.CreateEx, Policy: required, Platform: allPlatforms},",
					"{Name: \"AsstCreateEx\", Target: &table.CreateEx, Policy: required, Platform: windowsOnly},")
			}, linuxTarget, "windows-only"},
		{"experimental discovery without exclusion", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"	{Name: \"AsstGetMapLevelKey\", Header: \"AsstCallerExtra.h\", Reason: \"experimental; struct return\"},\n", "")
			}, linuxTarget, "neither bound nor covered"},
		{"experimental export bound as required", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["api.go"] = mustReplace(t, fs["api.go"],
					"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32",
					"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32\n\tGetMapLevelKey    func(key *byte) mapLevelKey")
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"		{Name: \"AsstAsyncAttachWindow\", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},",
					"		{Name: \"AsstAsyncAttachWindow\", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},\n		{Name: \"AsstGetMapLevelKey\", Target: &table.GetMapLevelKey, Policy: required, Platform: allPlatforms},")
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"	{Name: \"AsstGetMapLevelKey\", Header: \"AsstCallerExtra.h\", Reason: \"experimental; struct return\"},\n", "")
			}, linuxTarget, "must bind optional"},
		{"experimental export bound as windows-only", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["api.go"] = mustReplace(t, fs["api.go"],
					"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32",
					"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32\n\tGetItemName       func(id *byte) *byte")
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"		{Name: \"AsstAsyncAttachWindow\", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},",
					"		{Name: \"AsstAsyncAttachWindow\", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},\n		{Name: \"AsstGetItemName\", Target: &table.GetItemName, Policy: optional, Platform: windowsOnly},")
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"	{Name: \"AsstGetItemName\", Header: \"AsstCallerExtra.h\", Reason: \"experimental; no stability guarantee\"},\n", "")
			}, linuxTarget, "option-conditional, not platform-conditional"},
		{"windows export without any entry", func(t *testing.T, c *CExtraction) {},
			func(t *testing.T, fs map[string]string) {
				fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
					"		{Name: \"AsstAsyncAttachWindow\", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},\n", "")
				fs["api.go"] = mustReplace(t, fs["api.go"],
					"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32\n", "")
			}, linuxTarget, "neither excluded nor bound"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := linkCExtraction(tc.target)
			tc.mutC(t, c)
			files := copySynthetic()
			tc.mutGo(t, files)
			g := mustExtractGo(t, files)
			mustLinkFail(t, c, g, tc.target, tc.want)
		})
	}
}

// TestLinkExperimentalOptionalBinding 验证实验导出以 optional +
// allPlatforms 绑定的正例:两个目标的分类都是 Bound,不再要求排除。
func TestLinkExperimentalOptionalBinding(t *testing.T) {
	files := copySynthetic()
	files["api.go"] = mustReplace(t, files["api.go"],
		"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32",
		"	AsyncAttachWindow func(handle uintptr, hwnd uintptr, block uint8) int32\n\tGetMapLevelKey    func(key *byte) mapLevelKey\n\tGetItemName       func(id *byte) *byte")
	files["symbols.go"] = mustReplace(t, files["symbols.go"],
		"		{Name: \"AsstAsyncAttachWindow\", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},",
		"		{Name: \"AsstAsyncAttachWindow\", Target: &table.AsyncAttachWindow, Policy: optional, Platform: windowsOnly},"+
			"\n		{Name: \"AsstGetMapLevelKey\", Target: &table.GetMapLevelKey, Policy: optional, Platform: allPlatforms},"+
			"\n		{Name: \"AsstGetItemName\", Target: &table.GetItemName, Policy: optional, Platform: allPlatforms},")
	files["symbols.go"] = mustReplace(t, files["symbols.go"],
		"	{Name: \"AsstGetMapLevelKey\", Header: \"AsstCallerExtra.h\", Reason: \"experimental; struct return\"},\n", "")
	files["symbols.go"] = mustReplace(t, files["symbols.go"],
		"	{Name: \"AsstGetItemName\", Header: \"AsstCallerExtra.h\", Reason: \"experimental; no stability guarantee\"},\n", "")
	g := mustExtractGo(t, files)

	for _, target := range []Target{linuxTarget, windowsTarget} {
		res := mustLink(t, linkCExtraction(target), g, target)
		bound := make([]string, 0, len(res.Bound))
		for _, b := range res.Bound {
			bound = append(bound, b.Symbol)
		}
		if strings.Join(res.Excluded, ",") != "AsstConnect" {
			t.Errorf("%s excluded = %v, want only AsstConnect", target.GOOS, res.Excluded)
		}
		want := "AsstCreateEx,AsstGetImage,AsstGetItemName,AsstGetMapLevelKey,AsstLog"
		if target.GOOS == "windows" {
			want = "AsstAsyncAttachWindow," + want
		}
		if strings.Join(bound, ",") != want {
			t.Errorf("%s bound = %v, want %v", target.GOOS, bound, want)
		}
		for _, b := range res.Bound {
			if b.Symbol == "AsstGetMapLevelKey" || b.Symbol == "AsstGetItemName" {
				if b.Policy != "optional" || b.Platform != "allPlatforms" {
					t.Errorf("%s experimental bound record = %+v", target.GOOS, b)
				}
			}
		}
	}
}

// TestLinkNilInputs 空输入以 relation 诊断失败。
func TestLinkNilInputs(t *testing.T) {
	if _, diags := LinkCGo(nil, nil, linuxTarget); len(diags) == 0 || diags[0].Category != CatRelation {
		t.Fatalf("nil inputs must fail with relation diagnostic, got %v", diags)
	}
}
