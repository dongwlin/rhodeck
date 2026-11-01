// expect_test.go 校验冻结的独立 R/O 策略期望表与期望比对:期望表 30
// 项逐个钉死 policy/platform(钉死风格同 TestLockedProfileDigests);
// 比对正例以真实生产 native 包证明冻结表与生产 symbols.go/exclusions
// 一致且与目标无关;未冻结绑定、D/实验项改绑、required↔optional 互
// 换、平台互换与期望绑定符号被排除均以 policy 类诊断失败并指向过错
// 条目。绑定未冻结符号无法经完整 CLI 构造(C 输入被锁定为 30 项导
// 出且 link 先拒绝未知符号),故在本文件以直接构造的提取结果覆盖。

package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLockedPolicyExpectations 钉死冻结期望表的全部 30 项:23 项
// required+allPlatforms、2 项 optional+allPlatforms、1 项
// optional+windowsOnly、4 项 excluded,逐项与冻结矩阵一致。
func TestLockedPolicyExpectations(t *testing.T) {
	want := map[string]Expectation{
		// required + allPlatforms(23)。
		"AsstSetUserDir":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstLoadResource":        {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstSetStaticOption":     {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstCreateEx":            {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstDestroy":             {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstSetInstanceOption":   {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstAppendTask":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstSetTaskParams":       {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstStart":               {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstStop":                {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstRunning":             {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstConnected":           {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstBackToHome":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstAsyncConnect":        {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstSetConnectionExtras": {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstAsyncClick":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstAsyncScreencap":      {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstGetImage":            {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstGetUUID":             {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstGetTasksList":        {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstGetNullSize":         {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstGetVersion":          {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		"AsstLog":                 {Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll},
		// optional + allPlatforms(4)。
		"AsstCreate":         {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
		"AsstGetImageBgr":    {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
		"AsstGetMapLevelKey": {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
		"AsstGetItemName":    {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
		// optional + windowsOnly(1)。
		"AsstAsyncAttachWindow": {Policy: ExpectPolicyOptional, Platform: ExpectPlatformWindows},
		// excluded(2):两个 deprecated D 项。
		"AsstConnect":      {Policy: ExpectPolicyExcluded},
		"AsstAttachWindow": {Policy: ExpectPolicyExcluded},
	}
	require.Len(t, policyExpectations, 30,
		"locked policy expectations has %d entries, want 30", len(policyExpectations))
	require.Len(t, policyExpectations, len(want),
		"locked policy expectations has %d entries, want %d",
		len(policyExpectations), len(want))
	for symbol, w := range want {
		got, ok := policyExpectations[symbol]
		require.True(t, ok, "locked policy expectations is missing %s", symbol)
		assert.Equal(t, w, got, "%s = %+v, want %+v", symbol, got, w)
	}
	// 词表冻结:policy/platform 取值只允许期望常量。
	for symbol, e := range policyExpectations {
		assert.True(t, e.Policy == ExpectPolicyRequired || e.Policy == ExpectPolicyOptional ||
			e.Policy == ExpectPolicyExcluded, "%s has unexpected policy %q", symbol, e.Policy)
		assert.True(t, e.Platform == "" || e.Platform == ExpectPlatformAll ||
			e.Platform == ExpectPlatformWindows, "%s has unexpected platform %q", symbol, e.Platform)
		assert.False(t, e.Policy == ExpectPolicyExcluded && e.Platform != "",
			"%s is excluded but carries platform %q", symbol, e.Platform)
		assert.False(t, e.Policy != ExpectPolicyExcluded && e.Platform == "",
			"%s is bound-expected but has no platform", symbol)
	}
}

// TestCompareExpectationsPositive 用真实生产 native 包证明冻结期望表
// 与生产 symbols.go/exclusions 逐项一致:30 条对照、按符号名排序、
// 全部 expected=actual,且比对不依赖目标(函数不接收 Target)。
func TestCompareExpectationsPositive(t *testing.T) {
	g := mustExtractGo(t, copyRealNative(t))
	res, diags := CompareExpectations(g)
	require.Empty(t, diags, "CompareExpectations unexpectedly failed: %v", diags)
	require.Len(t, res.Entries, 30, "entries = %d, want 30", len(res.Entries))
	for i, e := range res.Entries {
		wantSym := sortedExpectationSymbols()[i]
		require.Equal(t, wantSym, e.Symbol,
			"entries[%d].Symbol = %q, want %q (must be sorted)", i, e.Symbol, wantSym)
		want := policyExpectations[e.Symbol]
		assert.Equal(t, want, Expectation(e.Expected), "%s expected = %+v, want %+v", e.Symbol, e.Expected, want)
		assert.Equal(t, e.Expected, e.Actual,
			"%s actual = %+v, want %+v (frozen table must match the production table)",
			e.Symbol, e.Actual, e.Expected)
	}
}

// mustCompareFail 断言期望比对以 policy 类诊断失败,且诊断定位到期望
// 的文件/行号/符号并含期望片段。
func mustCompareFail(t *testing.T, g *GoExtraction, wantFile string, wantLine int, wantSymbol, wantText string) Diagnostic {
	t.Helper()
	res, diags := CompareExpectations(g)
	// 失败路径的 res 为 nil,计数须留在原 if 守卫内惰性求值。
	if len(diags) == 0 {
		require.FailNowf(t, "CompareExpectations unexpectedly passed",
			"CompareExpectations unexpectedly passed (%d entries); want failure mentioning %q",
			len(res.Entries), wantText)
	}
	d := diags[0]
	assert.Equal(t, CatPolicy, d.Category, "category = %q, want %q", d.Category, CatPolicy)
	assert.False(t, d.File != wantFile || (wantLine > 0 && d.Line != wantLine),
		"location = %s:%d, want %s:%d", d.File, d.Line, wantFile, wantLine)
	assert.Equal(t, wantSymbol, d.Symbol, "symbol = %q, want %q", d.Symbol, wantSymbol)
	joined := d.Expected + "\n" + d.Actual + "\n" + d.Message
	assert.Contains(t, joined, wantText, "diagnostic %+v does not mention %q", d, wantText)
	return d
}

// specLine 取真实生产包中某符号条目的声明位置,供定位断言。
func specLine(t *testing.T, g *GoExtraction, symbol string) int {
	t.Helper()
	for _, s := range g.Specs {
		if s.Name == symbol {
			return s.Line
		}
	}
	require.FailNowf(t, "spec not found", "spec %s not found", symbol)
	return 0
}

// TestCompareExpectationsFailures 是期望比对的失败向量:每个变体从
// 真实生产提取结果出发做一处改动,断言 policy 诊断与条目定位。
func TestCompareExpectationsFailures(t *testing.T) {
	base := func(t *testing.T) *GoExtraction {
		return mustExtractGo(t, copyRealNative(t))
	}
	t.Run("bound symbol without frozen expectation", func(t *testing.T) {
		g := base(t)
		g.Specs = append(g.Specs, GoSymbolSpec{Name: "AsstUnfrozen", Field: "Unfrozen",
			Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll,
			File: "symbols.go", Line: 999})
		d := mustCompareFail(t, g, "symbols.go", 999, "AsstUnfrozen", "not frozen")
		assert.Contains(t, d.Expected, "expectation table",
			"expected = %q, want it to reference the expectation table", d.Expected)
	})
	t.Run("deprecated export rebound", func(t *testing.T) {
		g := base(t)
		g.Specs = append(g.Specs, GoSymbolSpec{Name: "AsstConnect", Field: "Connect",
			Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll,
			File: "symbols.go", Line: 999})
		mustCompareFail(t, g, "symbols.go", 999, "AsstConnect", "excluded")
	})
	t.Run("experimental export rebound", func(t *testing.T) {
		g := base(t)
		g.Specs = append(g.Specs, GoSymbolSpec{Name: "AsstGetMapLevelKey", Field: "GetMapLevelKey",
			Policy: ExpectPolicyRequired, Platform: ExpectPlatformAll,
			File: "symbols.go", Line: 999})
		mustCompareFail(t, g, "symbols.go", 999, "AsstGetMapLevelKey", "Policy optional")
	})
	t.Run("required downgraded to optional", func(t *testing.T) {
		g := base(t)
		for i := range g.Specs {
			if g.Specs[i].Name == "AsstStart" {
				g.Specs[i].Policy = ExpectPolicyOptional
			}
		}
		mustCompareFail(t, g, "symbols.go", specLine(t, g, "AsstStart"), "AsstStart",
			"Policy required")
	})
	t.Run("optional upgraded to required", func(t *testing.T) {
		g := base(t)
		for i := range g.Specs {
			if g.Specs[i].Name == "AsstCreate" {
				g.Specs[i].Policy = ExpectPolicyRequired
			}
		}
		mustCompareFail(t, g, "symbols.go", specLine(t, g, "AsstCreate"), "AsstCreate",
			"Policy optional")
	})
	t.Run("all-platform entry narrowed to windowsOnly", func(t *testing.T) {
		g := base(t)
		for i := range g.Specs {
			if g.Specs[i].Name == "AsstStart" {
				g.Specs[i].Platform = ExpectPlatformWindows
			}
		}
		mustCompareFail(t, g, "symbols.go", specLine(t, g, "AsstStart"), "AsstStart",
			"Platform windowsOnly")
	})
	t.Run("windows entry widened to allPlatforms", func(t *testing.T) {
		g := base(t)
		for i := range g.Specs {
			if g.Specs[i].Name == "AsstAsyncAttachWindow" {
				g.Specs[i].Platform = ExpectPlatformAll
			}
		}
		mustCompareFail(t, g, "symbols.go", specLine(t, g, "AsstAsyncAttachWindow"),
			"AsstAsyncAttachWindow", "Platform allPlatforms")
	})
	t.Run("required export excluded instead of bound", func(t *testing.T) {
		g := base(t)
		g.Exclusions = append(g.Exclusions, GoExclusion{Name: "AsstStart",
			Header: "AsstCaller.h", Reason: "wrongly excluded",
			File: "symbols.go", Line: 999})
		mustCompareFail(t, g, "symbols.go", 999, "AsstStart", "excluding an applicable R/O export")
	})
	t.Run("expected binding missing entirely", func(t *testing.T) {
		g := base(t)
		var specs []GoSymbolSpec
		for _, s := range g.Specs {
			if s.Name != "AsstBackToHome" {
				specs = append(specs, s)
			}
		}
		g.Specs = specs
		mustCompareFail(t, g, "", 0, "AsstBackToHome", "unbound")
	})
	t.Run("nil extraction", func(t *testing.T) {
		mustCompareFail(t, nil, "", 0, "extraction", "validated go extraction")
	})
}
