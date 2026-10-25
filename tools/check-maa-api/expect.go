// expect.go 冻结锁定 profile 的独立 R/O 策略期望表并执行期望比对。
// 期望表是 checker 内部常量,逐项转写自绑定设计 §3 的冻结矩阵(23 项
// required+allPlatforms、2 项 optional+allPlatforms、1 项
// optional+windowsOnly、4 项 excluded,共 30 项,与锁定完整导出集合
// 28 项 Caller + 2 项 Extra 一一对应),不从被检查的 Go 文件推导——
// 同时改写 Go 符号表与其派生清单使二者一致,只要偏离本表仍失败。
//
// 期望比对在 LinkCGo 成功之后执行,与目标无关地覆盖全部 spec 声明与
// 全部排除条目(linux/windows 两目标行为一致):
//   - 绑定符号无期望条目 → 失败(期望表必须完整覆盖,不存在未冻结
//     的绑定);
//   - 期望 excluded 的符号被绑定(D/实验项改绑)→ 失败;
//   - Policy 与期望不符(required↔optional 互换)→ 失败;
//   - Platform 与期望不符(通用项改 windowsOnly、Windows 项改
//     allPlatforms)→ 失败,独立成立,不依赖 link 的平台分类;
//   - 期望绑定的符号出现在 exclusions 中 → 失败(link 对适用导出被
//     排除是静默分类,本比对补上该缺口);
//   - 期望绑定的符号既无绑定也无排除 → 失败(失败关闭,不静默)。
//
// 比对失败返回单条 policy 诊断(定位到过错条目,含 expected/actual),
// 与 LinkCGo 的单诊断风格一致。

package main

import "sort"

// 期望表取值词表:Policy 为 required/optional/excluded,Bound 条目的
// Platform 为 allPlatforms/windowsOnly(excluded 条目无绑定平台)。
const (
	ExpectPolicyRequired  = "required"
	ExpectPolicyOptional  = "optional"
	ExpectPolicyExcluded  = "excluded"
	ExpectPlatformAll     = "allPlatforms"
	ExpectPlatformWindows = "windowsOnly"
)

// Expectation 是冻结期望表的一条条目:Policy 取 required/optional/
// excluded;required/optional 条目的 Platform 为 allPlatforms 或
// windowsOnly,excluded 条目不携带平台。
type Expectation struct {
	Policy   string
	Platform string
}

// lockedPolicyExpectations 是 maa-v6.20.1 的冻结独立期望表:键为 C
// 符号名,值转写自设计 §3 逐符号矩阵的分类列(R/O/D 与平台条件),
// 不随被检查输入变化。
var lockedPolicyExpectations = map[string]Expectation{
	// required + allPlatforms(23 项)。
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
	// optional + allPlatforms(2 项)。
	"AsstCreate":      {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
	"AsstGetImageBgr": {Policy: ExpectPolicyOptional, Platform: ExpectPlatformAll},
	// optional + windowsOnly(1 项)。
	"AsstAsyncAttachWindow": {Policy: ExpectPolicyOptional, Platform: ExpectPlatformWindows},
	// excluded(4 项):两个 D 项与两个实验导出;不携带绑定平台。
	"AsstConnect":        {Policy: ExpectPolicyExcluded},
	"AsstAttachWindow":   {Policy: ExpectPolicyExcluded},
	"AsstGetItemName":    {Policy: ExpectPolicyExcluded},
	"AsstGetMapLevelKey": {Policy: ExpectPolicyExcluded},
}

// sortedExpectationSymbols 按符号名排序返回期望表全部键,供成功输出
// 与比对收尾使用,保证确定性。
func sortedExpectationSymbols() []string {
	out := make([]string, 0, len(lockedPolicyExpectations))
	for name := range lockedPolicyExpectations {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ExpectSide 是期望比对单侧的 policy/platform 对照值;excluded 侧
// Platform 为空,未绑定侧 Policy 为 unbound。
type ExpectSide struct {
	Policy   string
	Platform string
}

// ExpectEntry 是一个符号的期望/实际对照记录。
type ExpectEntry struct {
	Symbol   string
	Expected ExpectSide
	Actual   ExpectSide
}

// ExpectResult 是期望比对结果:Entries 覆盖冻结期望表全部条目,按
// 符号名排序。
type ExpectResult struct {
	Entries []ExpectEntry
}

// CompareExpectations 把 Go 提取结果与冻结期望表逐项比对;与目标无关
// (不接收 Target,两目标行为一致),失败返回单条 policy 诊断,定位到
// 过错的 spec/exclusion 条目。
func CompareExpectations(g *GoExtraction) (*ExpectResult, []Diagnostic) {
	fail := func(d Diagnostic) (*ExpectResult, []Diagnostic) { return nil, []Diagnostic{d} }
	if g == nil {
		return fail(Diagnostic{Category: CatPolicy, Symbol: "extraction",
			Expected: "validated go extraction", Actual: "nil",
			Message: "expectation comparison requires a validated go extraction"})
	}

	specByName := map[string]GoSymbolSpec{}
	for _, s := range g.Specs {
		// 同一符号重复绑定已由 link 失败;此处取首条即可。
		if _, dup := specByName[s.Name]; !dup {
			specByName[s.Name] = s
		}
	}
	excludedNames := map[string]bool{}
	for _, e := range g.Exclusions {
		excludedNames[e.Name] = true
	}

	// 绑定侧:每条 spec 声明都必须落在冻结期望上;遍历保持声明顺序,
	// 失败报告第一条。
	for _, s := range g.Specs {
		want, ok := lockedPolicyExpectations[s.Name]
		if !ok {
			return fail(Diagnostic{Category: CatPolicy, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "an entry in the locked profile expectation table",
				Actual:   "no frozen expectation for this symbol",
				Message:  "bound symbol is not frozen in the locked profile expectation table; unfrozen bindings are rejected"})
		}
		if want.Policy == ExpectPolicyExcluded {
			return fail(Diagnostic{Category: CatPolicy, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "excluded (D and experimental exports must not be bound)",
				Actual:   "bound with Policy " + s.Policy + " and Platform " + s.Platform,
				Message:  "frozen expectation marks this symbol as excluded; rebinding a deprecated or experimental export fails"})
		}
		if s.Policy != want.Policy {
			return fail(Diagnostic{Category: CatPolicy, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Policy " + want.Policy, Actual: "Policy " + s.Policy,
				Message: "symbol policy does not match the frozen expectation"})
		}
		if s.Platform != want.Platform {
			return fail(Diagnostic{Category: CatPolicy, File: s.File, Line: s.Line, Symbol: s.Name,
				Expected: "Platform " + want.Platform, Actual: "Platform " + s.Platform,
				Message: "symbol platform does not match the frozen expectation"})
		}
	}

	// 排除侧:期望绑定的符号不得被排除(期望 excluded 的符号必须被
	// 排除由 link 的精确排除检查保证,此处独立复核绑定方向)。
	for _, e := range g.Exclusions {
		want, ok := lockedPolicyExpectations[e.Name]
		if ok && want.Policy != ExpectPolicyExcluded {
			return fail(Diagnostic{Category: CatPolicy, File: e.File, Line: e.Line, Symbol: e.Name,
				Expected: want.Policy + "/" + want.Platform + " binding without an exclusion",
				Actual:   "excluded",
				Message:  "frozen expectation requires this symbol to be bound; excluding an applicable R/O export fails"})
		}
	}

	// 收尾:期望绑定的符号必须存在绑定(既无绑定也无排除即失败)。
	for _, name := range sortedExpectationSymbols() {
		want := lockedPolicyExpectations[name]
		if want.Policy == ExpectPolicyExcluded {
			continue
		}
		if _, bound := specByName[name]; bound {
			continue
		}
		return fail(Diagnostic{Category: CatPolicy, Symbol: name,
			Expected: want.Policy + "/" + want.Platform + " binding",
			Actual:   "unbound",
			Message:  "frozen expectation requires this symbol to be bound; a missing binding is rejected"})
	}

	// 组装全部 30 项的期望/实际对照,按符号名排序。
	res := &ExpectResult{Entries: make([]ExpectEntry, 0, len(lockedPolicyExpectations))}
	for _, name := range sortedExpectationSymbols() {
		want := lockedPolicyExpectations[name]
		var actual ExpectSide
		switch {
		case want.Policy == ExpectPolicyExcluded:
			actual = ExpectSide{Policy: ExpectPolicyExcluded}
			if !excludedNames[name] {
				actual.Policy = "unbound"
			}
		default:
			if s, bound := specByName[name]; bound {
				actual = ExpectSide{Policy: s.Policy, Platform: s.Platform}
			} else {
				actual = ExpectSide{Policy: "unbound"}
			}
		}
		res.Entries = append(res.Entries, ExpectEntry{Symbol: name, Expected: ExpectSide(want), Actual: actual})
	}
	return res, nil
}
