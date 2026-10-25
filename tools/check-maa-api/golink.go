// golink.go 用第一段的 C 提取结果(完整集合/适用集合/实验发现)与
// Go 提取结果建立一对一关系,任何无法建立的关系失败并给出双方可定位
// 的 relation 类诊断:
//   - 每个发现的 C 导出必须恰属其一:适用 R/O 绑定(Name 相等、Target
//     指向存在字段且字段名=符号去 Asst 前缀)、已知非目标平台条目
//     (Linux 目标下的 windowsOnly 绑定)或精确排除(Name 相等且 Header
//     与 C 来源头一致);
//   - 反向:每条 symbolSpec 必须有同名 C 导出;每条 exclusion 必须对应
//     真实存在的 C 声明(陈旧排除失败);同一清单内 Name 重复失败;
//   - 实验发现不得被绑定,必须被排除精确覆盖;windows 条件导出与
//     allPlatforms 绑定、all 平台导出与 windowsOnly 绑定的平台矛盾失败;
//   - api 字段必须恰被一条条目指向,多余 raw func 字段失败。
//
// R/O 策略与锁定 profile 的独立期望比对在 link 成功之后执行,
// 见 expect.go 的 CompareExpectations(policy 类诊断);宽度级逐参数
// 签名比对与 callback 平台适配比对在其后执行,见 compare.go 的
// CompareSignatures(signature 类诊断)。

package main

import (
	"path/filepath"
	"sort"
	"strings"
)

// LinkBound 是一条适用 R/O 绑定分类。
type LinkBound struct {
	Symbol   string
	Field    string
	Policy   string
	Platform string
}

// LinkResult 是 C↔Go 关联结果:Bound 为目标适用的 R/O 绑定,NonTarget
// 为本目标不适用但已有 windowsOnly 绑定的符号,Excluded 为被精确排除
// 覆盖的符号。三个列表均按符号名排序。
type LinkResult struct {
	Bound     []LinkBound
	NonTarget []string
	Excluded  []string
}

// LinkCGo 执行 C↔Go 关联;失败返回单条 relation 诊断(定位到过错方)。
func LinkCGo(c *CExtraction, g *GoExtraction, target Target) (*LinkResult, []Diagnostic) {
	fail := func(d Diagnostic) (*LinkResult, []Diagnostic) { return nil, []Diagnostic{d} }
	if c == nil || g == nil {
		return fail(Diagnostic{Category: CatRelation, Symbol: "extraction",
			Expected: "validated c and go extractions", Actual: "nil",
			Message: "linking requires validated c and go extraction results"})
	}

	// api 字段集合。
	fieldPos := map[string]GoField{}
	for _, f := range g.Fields {
		fieldPos[f.Name] = f
	}

	// 条目集合:Name 唯一、Target 指向存在字段、字段名=符号去前缀、
	// 同一字段不得被两条条目指向(重复绑定)。
	specByName := map[string]GoSymbolSpec{}
	boundFields := map[string]string{}
	for _, s := range g.Specs {
		if _, dup := specByName[s.Name]; dup {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "unique symbolSpec entry", Actual: "duplicate entry",
				Message: "the same C symbol is bound more than once in symbolSpecs"})
		}
		specByName[s.Name] = s
		if _, ok := fieldPos[s.Field]; !ok {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Target references an existing api field", Actual: s.Field,
				Message: "symbolSpec Target points to a field that does not exist in the api struct"})
		}
		wantField := strings.TrimPrefix(s.Name, "Asst")
		if s.Field != wantField {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Target field " + wantField + " (symbol without the Asst prefix)",
				Actual:   s.Field,
				Message:  "symbolSpec Target field must equal the C symbol without the Asst prefix"})
		}
		if prev, dup := boundFields[s.Field]; dup {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "each api field bound by exactly one entry", Actual: "field already bound by " + prev,
				Message: "two symbolSpec entries target the same api field"})
		}
		boundFields[s.Field] = s.Name
	}
	// 反向:每个 api 字段必须被恰一条条目指向(多余 raw func 字段失败)。
	for _, f := range g.Fields {
		if _, ok := boundFields[f.Name]; !ok {
			return fail(Diagnostic{Category: CatRelation, File: f.File, Line: f.Line, Symbol: f.Name,
				Expected: "api field referenced by a symbolSpec entry", Actual: "no entry",
				Message: "api struct has a raw func field with no symbolSpec entry"})
		}
	}

	// 排除集合:Name 唯一。
	exclByName := map[string]GoExclusion{}
	for _, e := range g.Exclusions {
		if _, dup := exclByName[e.Name]; dup {
			return fail(Diagnostic{Category: CatRelation, File: e.File, Line: e.Line, Symbol: e.Name,
				Expected: "unique exclusion entry", Actual: "duplicate entry",
				Message: "the same C symbol is excluded more than once"})
		}
		exclByName[e.Name] = e
	}

	// C 导出集合与适用集合。
	cByName := map[string]CFunction{}
	for i := range c.Functions {
		cByName[c.Functions[i].Name] = c.Functions[i]
	}
	applicable := map[string]bool{}
	for i := range c.Applicable {
		applicable[c.Applicable[i].Name] = true
	}

	// 每条 symbolSpec 必须有同名 C 导出(漏绑方向的 Go 侧失败)。
	for _, s := range g.Specs {
		if _, ok := cByName[s.Name]; !ok {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "C export with the same name", Actual: "no C declaration",
				Message: "symbolSpec binds a symbol that has no C export in the locked input"})
		}
	}

	// 被排除项不得同时被绑定(D/实验项改绑失败)。
	for _, s := range g.Specs {
		if _, excluded := exclByName[s.Name]; excluded {
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "excluded symbol without a binding", Actual: "also bound by symbolSpecs",
				Message: "an excluded export must not additionally appear in symbolSpecs"})
		}
	}

	// 每条 exclusion 必须对应真实存在的 C 声明(陈旧排除失败),且
	// Header 与 C 来源头一致(归档相对路径取基名比对)。
	for _, e := range g.Exclusions {
		fn, ok := cByName[e.Name]
		if !ok {
			return fail(Diagnostic{Category: CatRelation, File: e.File, Line: e.Line, Symbol: e.Name,
				Expected: "C declaration with the same name", Actual: "no C declaration",
				Message: "stale exclusion: no C export matches this exclusion entry"})
		}
		if got := filepath.Base(fn.Header); got != e.Header {
			return fail(Diagnostic{Category: CatRelation, File: e.File, Line: e.Line, Symbol: e.Name,
				Expected: "Header " + got + " (c declaration source header)", Actual: e.Header,
				Message: "exclusion Header does not match the C declaration source header"})
		}
	}

	// 平台一致性:windows 条件导出不得绑成 allPlatforms,all 平台导出
	// 不得绑成 windowsOnly,实验导出不得被绑定。
	for _, s := range g.Specs {
		fn := cByName[s.Name]
		switch {
		case fn.Platform == PlatformExtra:
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "experimental export covered by an exclusion", Actual: "bound by symbolSpecs",
				Message: "experimental export must be excluded, never bound"})
		case fn.Platform == PlatformWindows && s.Platform != "windowsOnly":
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Platform windowsOnly for a windows-conditional export", Actual: s.Platform,
				Message: "windows-conditional C export is bound with the wrong platform"})
		case fn.Platform == PlatformAll && s.Platform == "windowsOnly":
			return fail(Diagnostic{Category: CatRelation, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Platform allPlatforms for an unconditional export", Actual: s.Platform,
				Message: "unconditional C export is bound as windows-only"})
		}
	}

	// 正向分类:每个发现的 C 导出恰属其一。
	res := &LinkResult{}
	for i := range c.Functions {
		fn := c.Functions[i]
		name := fn.Name
		_, excluded := exclByName[name]
		switch {
		case excluded:
			res.Excluded = append(res.Excluded, name)
		case applicable[name]:
			s, ok := specByName[name]
			if !ok {
				return fail(Diagnostic{Category: CatRelation, File: fn.Header, Line: fn.Line, Symbol: name,
					Expected: "symbolSpec entry binding this export", Actual: "no entry",
					Message: "applicable C export has no symbolSpec binding"})
			}
			res.Bound = append(res.Bound, LinkBound{Symbol: name, Field: s.Field,
				Policy: s.Policy, Platform: s.Platform})
		case fn.Platform == PlatformWindows:
			s, ok := specByName[name]
			if !ok || s.Platform != "windowsOnly" {
				return fail(Diagnostic{Category: CatRelation, File: fn.Header, Line: fn.Line, Symbol: name,
					Expected: "windowsOnly symbolSpec entry for the non-target platform export",
					Actual:   "missing or wrongly platformed entry",
					Message:  "windows export is neither excluded nor bound as windowsOnly"})
			}
			res.NonTarget = append(res.NonTarget, name)
		default:
			// extra 平台且未被排除:实验发现漏排。
			return fail(Diagnostic{Category: CatRelation, File: fn.Header, Line: fn.Line, Symbol: name,
				Expected: "exclusion covering the experimental export", Actual: "no exclusion",
				Message: "experimental discovery is not covered by a precise exclusion"})
		}
	}
	sort.Slice(res.Bound, func(i, j int) bool { return res.Bound[i].Symbol < res.Bound[j].Symbol })
	sort.Strings(res.NonTarget)
	sort.Strings(res.Excluded)
	return res, nil
}
