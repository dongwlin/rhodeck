// cextract.go 把受限预处理后的声明项解释为规范化提取结果:标量
// typedef 链、AsstHandle、AsstApiCallback 函数指针、Caller 两个
// Windows enum 的命名值、Extra 的 AsstMapLevelKey 四指针成员核验,
// 以及普通 `返回类型 ASSTAPI 名称(参数列表);` 声明与唯一的
// ASSTAPI_PORT AsstGetVersion 拼写。类型规范化保留宽度、符号位、
// const、指针层级与 pointee 语义;ASSTAPI 必须展开为 import/export
// 修饰加正确调用约定。提取结果暴露完整集合、目标适用集合与实验
// 发现三个视图,供下一段做 Go 关联。

package main

import "strconv"

// Platform 标注声明来源条件:all 为无条件,windows 来自 _WIN32 分支,
// extra 来自 ASST_WITH_EXTRA_CALLERS 分支。
const (
	PlatformAll     = "all"
	PlatformWindows = "windows"
	PlatformExtra   = "extra"
)

// CallConv 是展开后的调用约定;Linkage 是展开后的导出修饰。
const (
	CallConvNone    = ""
	CallConvStdcall = "__stdcall"
	CallConvSysVABI = "sysv_abi"

	LinkageNone              = ""
	LinkageDLLExport         = "dllexport"
	LinkageDLLImport         = "dllimport"
	LinkageVisibilityDefault = "visibility-default"
	LinkageVisibilityHidden  = "visibility-hidden"
)

// CFunction 是一条提取的导出函数声明:Platform 取 all/windows/extra,
// Spelling 为声明的原始 token 拼写(规范化空白,宏不展开)。
type CFunction struct {
	Name     string
	Header   string
	Line     int
	Ret      CType
	Params   []CParam
	CallConv string
	Linkage  string
	Platform string
	Spelling string
}

// CTypedef 是一条 typedef 记录;Type 为规范化展开结果。
type CTypedef struct {
	Name     string
	Header   string
	Line     int
	Type     CType
	Platform string
}

// CEnumValue 是 enum 的一个命名值。
type CEnumValue struct {
	Name  string
	Value int64
}

// CEnum 是一条 enum 记录(仅 Caller 两个 Windows enum)。
type CEnum struct {
	Tag      string
	Header   string
	Line     int
	Values   []CEnumValue
	Platform string
}

// CStructMember 是结构体的一个成员。
type CStructMember struct {
	Name string
	Type CType
}

// CStruct 是一条结构体定义记录(仅 Extra 的 AsstMapLevelKey)。
type CStruct struct {
	Tag      string
	Header   string
	Line     int
	Members  []CStructMember
	Platform string
}

// CExtraction 是 C 提取结果:Functions 为完整声明集合(Caller 28 项
// 加 ASST_WITH_EXTRA_CALLERS=1 发现的 Extra 2 项),Applicable 为按
// 目标过滤的适用集合(非 Windows 不含窗口符号,extra 恒不在内),
// Discovery 为实验导出发现(=0 时为空,=1 时恰为两个实验导出)。
type CExtraction struct {
	Functions  []CFunction
	Applicable []CFunction
	Discovery  []CFunction
	Typedefs   []CTypedef
	Enums      []CEnum
	Structs    []CStruct
}

type passResult struct {
	Functions []CFunction
	Typedefs  []CTypedef
	Enums     []CEnum
	Structs   []CStruct
}

// 冻结的 Caller 导出名(28 项,归档顺序)与实验导出名(2 项)。
var frozenCallerOrder = []string{
	"AsstSetUserDir", "AsstLoadResource", "AsstSetStaticOption",
	"AsstCreate", "AsstCreateEx", "AsstDestroy", "AsstSetInstanceOption",
	"AsstConnect", "AsstAppendTask", "AsstSetTaskParams",
	"AsstStart", "AsstStop", "AsstRunning", "AsstConnected", "AsstBackToHome",
	"AsstAsyncConnect", "AsstSetConnectionExtras",
	"AsstAttachWindow", "AsstAsyncAttachWindow",
	"AsstAsyncClick", "AsstAsyncScreencap",
	"AsstGetImage", "AsstGetImageBgr", "AsstGetUUID", "AsstGetTasksList",
	"AsstGetNullSize", "AsstGetVersion", "AsstLog",
}

var frozenCallerSet = newSet(frozenCallerOrder...)

var frozenExperimentalOrder = []string{"AsstGetMapLevelKey", "AsstGetItemName"}

var frozenExperimental = newSet(frozenExperimentalOrder...)

// 仅这三个符号允许以归档空 () 按零参数解释;(void) 形式任何函数
// 都可识别。
var frozenZeroArgFunctions = newSet("AsstCreate", "AsstGetNullSize", "AsstGetVersion")

// frozenTypedefs 是锁定 typedef 链:名称 → 规范拼写(忽略空白)。
var frozenTypedefs = map[string]string{
	"AsstBool":              "typedefuint8_tAsstBool",
	"AsstSize":              "typedefuint64_tAsstSize",
	"AsstId":                "typedefint32_tAsstId",
	"AsstMsgId":             "typedefAsstIdAsstMsgId",
	"AsstTaskId":            "typedefAsstIdAsstTaskId",
	"AsstAsyncCallId":       "typedefAsstIdAsstAsyncCallId",
	"AsstOptionKey":         "typedefint32_tAsstOptionKey",
	"AsstStaticOptionKey":   "typedefAsstOptionKeyAsstStaticOptionKey",
	"AsstInstanceOptionKey": "typedefAsstOptionKeyAsstInstanceOptionKey",
	"AsstHandle":            "typedefstructAsstExtAPI*AsstHandle",
}

const frozenCallbackCanon = "typedefvoid(ASST_CALL*AsstApiCallback)(AsstMsgIdmsg,constchar*details_json,void*custom_arg)"

const frozenCallbackReadable = "typedef void(ASST_CALL* AsstApiCallback)(AsstMsgId msg, const char* details_json, void* custom_arg)"

const frozenGetVersionCanon = "ASSTAPI_PORTconstchar*ASST_CALLAsstGetVersion()"

const frozenGetVersionReadable = "ASSTAPI_PORT const char* ASST_CALL AsstGetVersion()"

var frozenEnumTags = newSet("AsstWin32ScreencapMethodEnum", "AsstWin32InputMethodEnum")

var frozenStructTag = "AsstMapLevelKey"

var frozenStructMembers = []string{"stage_id", "code", "level_id", "name"}

// runParsePass 以给定宏环境完整解析一次 c 输入:Caller 为根(引入
// Port),Extra 为第二根;两个 pass 都检查全部分支结构,区别仅在
// extra 内容是否进入结果集合。
func runParsePass(input *CInput, env macroEnv) (*passResult, *Diagnostic) {
	p := newCParser(input, env)
	if d := p.parseFile(FileAsstCaller); d != nil {
		return nil, d
	}
	if d := p.parseFile(FileAsstCallerExtra); d != nil {
		return nil, d
	}
	res := p.result
	if env.intValue("ASST_WITH_EXTRA_CALLERS") == 0 {
		// =0 pass:extra 内容已做全分支结构检查,但不进入结果集合。
		res = &passResult{}
		for _, f := range p.result.Functions {
			if f.Platform != PlatformExtra {
				res.Functions = append(res.Functions, f)
			}
		}
		for _, td := range p.result.Typedefs {
			if td.Platform != PlatformExtra {
				res.Typedefs = append(res.Typedefs, td)
			}
		}
		for _, e := range p.result.Enums {
			if e.Platform != PlatformExtra {
				res.Enums = append(res.Enums, e)
			}
		}
		for _, s := range p.result.Structs {
			if s.Platform != PlatformExtra {
				res.Structs = append(res.Structs, s)
			}
		}
	}
	return res, nil
}

// ExtractC 对已校验的 c 输入执行完整提取:先以 ASST_WITH_EXTRA_CALLERS=0
// 跑生产 profile(实验导出必须不脱离守卫出现),再以 =1 做实验导出
// 发现(必须恰为两个锁定实验符号),随后给出完整/适用/发现三个视图。
func ExtractC(input *CInput, target Target) (*CExtraction, []Diagnostic) {
	fail := func(d Diagnostic) (*CExtraction, []Diagnostic) { return nil, []Diagnostic{d} }
	if input == nil {
		return fail(Diagnostic{Category: CatInput, Symbol: "c-input",
			Expected: "validated c input", Actual: "nil",
			Message: "extraction requires a validated c input"})
	}
	if diags := ValidateTarget(target); len(diags) > 0 {
		return nil, diags
	}

	res0, d := runParsePass(input, newMacroEnv(target, 0))
	if d != nil {
		return fail(*d)
	}
	res1, d := runParsePass(input, newMacroEnv(target, 1))
	if d != nil {
		return fail(*d)
	}
	for _, res := range []*passResult{res0, res1} {
		if d := checkFunctionSet(res); d != nil {
			return fail(*d)
		}
	}
	for _, f := range res0.Functions {
		if f.Platform != PlatformExtra && frozenExperimental[f.Name] {
			return fail(Diagnostic{Category: CatCSyntax, File: f.Header, Line: f.Line, Symbol: f.Name,
				Expected: "declared under #if ASST_WITH_EXTRA_CALLERS",
				Actual:   "declared without the experimental guard",
				Message:  "experimental export found outside the ASST_WITH_EXTRA_CALLERS block"})
		}
	}
	var discovery []CFunction
	for _, f := range res1.Functions {
		if f.Platform == PlatformExtra {
			discovery = append(discovery, f)
		}
	}
	if len(discovery) != len(frozenExperimentalOrder) {
		return fail(Diagnostic{Category: CatCSyntax, File: FileAsstCallerExtra, Symbol: "discovery",
			Expected: sprintfNames(frozenExperimentalOrder), Actual: discoveryNames(discovery),
			Message: "experimental export discovery does not match the locked profile"})
	}
	for i, want := range frozenExperimentalOrder {
		if discovery[i].Name != want {
			return fail(Diagnostic{Category: CatCSyntax, File: discovery[i].Header, Line: discovery[i].Line,
				Symbol: discovery[i].Name, Expected: want, Actual: discovery[i].Name,
				Message: "experimental export discovery does not match the locked profile"})
		}
	}

	full := res1.Functions
	applicable := make([]CFunction, 0, len(full))
	for _, f := range full {
		if f.Platform == PlatformAll || (f.Platform == PlatformWindows && target.GOOS == "windows") {
			applicable = append(applicable, f)
		}
	}
	return &CExtraction{
		Functions:  full,
		Applicable: applicable,
		Discovery:  discovery,
		Typedefs:   res1.Typedefs,
		Enums:      res1.Enums,
		Structs:    res1.Structs,
	}, nil
}

// checkFunctionSet 要求非 extra 声明恰好构成锁定的 28 项 caller 导出
// (无缺失、无未知、无重复),全部声明名称唯一。
func checkFunctionSet(res *passResult) *Diagnostic {
	seen := map[string]bool{}
	inCaller := map[string]bool{}
	for _, f := range res.Functions {
		if seen[f.Name] {
			return &Diagnostic{Category: CatCSyntax, File: f.Header, Line: f.Line, Symbol: f.Name,
				Expected: "unique declaration", Actual: "duplicate declaration",
				Message: "exported symbol declared more than once"}
		}
		seen[f.Name] = true
		if f.Platform == PlatformExtra {
			continue
		}
		if !frozenCallerSet[f.Name] {
			return &Diagnostic{Category: CatCSyntax, File: f.Header, Line: f.Line, Symbol: f.Name,
				Expected: "one of the 28 locked caller exports", Actual: f.Name,
				Message: "unexpected exported declaration"}
		}
		inCaller[f.Name] = true
	}
	for _, name := range frozenCallerOrder {
		if !inCaller[name] {
			return &Diagnostic{Category: CatCSyntax, File: FileAsstCaller, Symbol: name,
				Expected: "declared", Actual: "missing",
				Message: "locked caller export missing from the c input"}
		}
	}
	return nil
}

func sprintfNames(names []string) string {
	out := ""
	for i, n := range names {
		if i > 0 {
			out += " "
		}
		out += n
	}
	return "[" + out + "]"
}

func discoveryNames(fns []CFunction) string {
	names := make([]string, 0, len(fns))
	for _, f := range fns {
		names = append(names, f.Name)
	}
	return sprintfNames(names)
}

// processItem 按首 token 分派声明项。
func (p *cparser) processItem(item []token, line int) *Diagnostic {
	plat := p.platformLabel()
	switch {
	case item[0].is("typedef"):
		return p.processTypedef(item, line, plat)
	case item[0].is("struct"):
		switch {
		case itemHasBrace(item):
			return p.processStructDef(item, line, plat)
		case len(item) == 2:
			return p.processForwardDecl(item, line)
		}
		return p.processFunction(item, line, plat)
	default:
		return p.processFunction(item, line, plat)
	}
}

func itemHasBrace(item []token) bool {
	for _, t := range item {
		if t.is("{") || t.is("}") {
			return true
		}
	}
	return false
}

// processTypedef 处理标量/指针 typedef:先按当前 typedef 表解析底层
// 类型(未知名字或环即失败),再对照锁定拼写链。
func (p *cparser) processTypedef(item []token, line int, plat string) *Diagnostic {
	if len(item) < 3 {
		return p.diag(line, "typedef", "typedef with underlying type and name", tokenCanon(item),
			"malformed typedef")
	}
	if item[1].is("enum") {
		return p.processEnum(item, line, plat)
	}
	if item[1].is("void") {
		return p.processCallbackTypedef(item, line, plat)
	}
	name := item[len(item)-1].text
	underlying := item[1 : len(item)-1]
	resolved, d := p.resolveType(underlying, line)
	if d != nil {
		return d
	}
	expectedCanon, frozen := frozenTypedefs[name]
	if !frozen {
		return p.diag(line, name, "one of the locked typedef names", tokenCanon(item),
			"unknown typedef (unknown or circular alias)")
	}
	if canon := tokenCanon(item); canon != expectedCanon {
		return p.diag(line, name, expectedCanon, canon,
			"typedef spelling does not match the locked alias chain")
	}
	if _, dup := p.typedefs[name]; dup {
		return p.diag(line, name, "unique typedef", "redefinition", "typedef declared twice")
	}
	p.typedefs[name] = resolved
	p.result.Typedefs = append(p.result.Typedefs, CTypedef{Name: name, Header: p.curFile,
		Line: line, Type: resolved, Platform: plat})
	return nil
}

// processCallbackTypedef 处理唯一的 AsstApiCallback 函数指针 typedef。
func (p *cparser) processCallbackTypedef(item []token, line int, plat string) *Diagnostic {
	canon := tokenCanon(item)
	if canon != frozenCallbackCanon {
		return p.diag(line, "AsstApiCallback", frozenCallbackReadable, canon,
			"callback typedef does not match the locked spelling")
	}
	// 形状固定:typedef void ( ASST_CALL * AsstApiCallback ) ( params )
	closeIdx := -1
	for i := 3; i < len(item); i++ {
		if item[i].is(")") {
			closeIdx = i
			break
		}
	}
	if closeIdx < 0 || closeIdx+1 >= len(item) || !item[closeIdx+1].is("(") || !item[len(item)-1].is(")") {
		return p.diag(line, "AsstApiCallback", frozenCallbackReadable, canon,
			"malformed callback declarator")
	}
	decl := item[3:closeIdx]
	if len(decl) != 3 || !decl[0].is("ASST_CALL") || !decl[1].is("*") || decl[2].text != "AsstApiCallback" {
		return p.diag(line, "AsstApiCallback", frozenCallbackReadable, tokenCanon(decl),
			"malformed callback declarator")
	}
	cc, d := p.expandCallConvMacro("ASST_CALL", line)
	if d != nil {
		return d
	}
	params, d := p.parseParams(item[closeIdx+2:len(item)-1], line, "AsstApiCallback")
	if d != nil {
		return d
	}
	if len(params) == 0 {
		return p.diag(line, "AsstApiCallback", "named callback parameters", "empty",
			"callback typedef must declare its parameters")
	}
	ct := CType{Kind: CKindFuncPtr, Sig: &CFuncSig{Ret: CType{Kind: CKindVoid}, Params: params, CallConv: cc}}
	if _, dup := p.typedefs["AsstApiCallback"]; dup {
		return p.diag(line, "AsstApiCallback", "unique typedef", "redefinition", "typedef declared twice")
	}
	p.typedefs["AsstApiCallback"] = ct
	p.result.Typedefs = append(p.result.Typedefs, CTypedef{Name: "AsstApiCallback", Header: p.curFile,
		Line: line, Type: ct, Platform: plat})
	return nil
}

// processEnum 处理 Caller 的两个 Windows enum:仅整数常量与 1 << n,
// 允许尾逗号;其余表达式失败。
func (p *cparser) processEnum(item []token, line int, plat string) *Diagnostic {
	if len(item) < 6 || !item[3].is("{") || !item[len(item)-2].is("}") {
		return p.diag(line, "enum", "typedef enum Tag { entries } Name", tokenCanon(item),
			"malformed enum declaration")
	}
	tag := item[2].text
	if !frozenEnumTags[tag] {
		return p.diag(line, tag, "one of the two locked Windows enums", tag,
			"unknown enum declaration")
	}
	name := item[len(item)-1].text
	if name != tag {
		return p.diag(line, name, tag, name, "enum typedef name must equal the tag")
	}
	for _, e := range p.result.Enums {
		if e.Tag == tag {
			return p.diag(line, tag, "unique enum", "redeclaration", "enum declared twice")
		}
	}
	values, d := p.parseEnumEntries(item[4 : len(item)-2])
	if d != nil {
		return d
	}
	p.result.Enums = append(p.result.Enums, CEnum{Tag: tag, Header: p.curFile, Line: line,
		Values: values, Platform: plat})
	return nil
}

func (p *cparser) parseEnumEntries(entries []token) ([]CEnumValue, *Diagnostic) {
	for _, t := range entries {
		if t.is("(") {
			return nil, p.diag(t.line, "enum", "constant entries", "nested parentheses",
				"parentheses are not supported in enum entries")
		}
	}
	groups := splitTopLevel(entries, ",")
	var out []CEnumValue
	seen := map[string]bool{}
	for gi, g := range groups {
		if len(g) == 0 {
			if gi == len(groups)-1 {
				continue // 尾逗号
			}
			return nil, p.diag(0, "enum entry", "Name = value", "empty entry",
				"empty enum entry")
		}
		if len(g) < 3 || !g[1].is("=") {
			return nil, p.diag(g[0].line, g[0].text, "Name = value", tokenCanon(g),
				"enum entries must assign a value")
		}
		name := g[0].text
		if !isIdentSpell(name) || typeKeywords[name] {
			return nil, p.diag(g[0].line, name, "enum constant name", name,
				"enum constant must be a plain identifier")
		}
		if seen[name] {
			return nil, p.diag(g[0].line, name, "unique enum constant", "duplicate", "enum constant declared twice")
		}
		seen[name] = true
		val, d := p.parseEnumValue(g[2:])
		if d != nil {
			d.Symbol = name
			return nil, d
		}
		out = append(out, CEnumValue{Name: name, Value: val})
	}
	return out, nil
}

func (p *cparser) parseEnumValue(expr []token) (int64, *Diagnostic) {
	decLiteral := func(t token) (int64, *Diagnostic) {
		v, err := strconv.ParseInt(t.text, 10, 64)
		if err != nil || (len(t.text) > 1 && t.text[0] == '0') {
			return 0, p.diag(t.line, t.text, "decimal integer literal", t.text,
				"only decimal integer constants are supported in enums")
		}
		return v, nil
	}
	if len(expr) == 1 {
		return decLiteral(expr[0])
	}
	if len(expr) == 3 && expr[0].is("1") && expr[1].is("<<") {
		n, d := decLiteral(expr[2])
		if d != nil {
			return 0, d
		}
		if n < 0 || n > 62 {
			return 0, p.diag(expr[2].line, expr[2].text, "shift within int64 range", expr[2].text,
				"1 << n is out of range")
		}
		return int64(1) << uint(n), nil
	}
	return 0, p.diag(expr[0].line, tokenCanon(expr), "integer constant or 1 << n", tokenCanon(expr),
		"unsupported enum value expression")
}

// processForwardDecl 仅认 struct AsstExtAPI; 前置声明。
func (p *cparser) processForwardDecl(item []token, line int) *Diagnostic {
	if len(item) == 2 && item[1].is("AsstExtAPI") {
		return nil
	}
	return p.diag(line, tokenCanon(item), "struct AsstExtAPI;", tokenCanon(item),
		"unknown forward declaration")
}

// processStructDef 仅认 AsstMapLevelKey 的四个 const char* 成员
// (stage_id/code/level_id/name,顺序固定);新增成员或类型变化失败。
func (p *cparser) processStructDef(item []token, line int, plat string) *Diagnostic {
	if len(item) < 5 || !item[2].is("{") || !item[len(item)-1].is("}") {
		return p.diag(line, "struct", "struct definition", tokenCanon(item),
			"malformed struct definition")
	}
	tag := item[1].text
	if tag != frozenStructTag {
		return p.diag(line, tag, "struct "+frozenStructTag, tag,
			"unknown struct definition; struct parameters/returns are outside the binding scope")
	}
	memberRegion := item[3 : len(item)-1]
	groups := splitTopLevel(memberRegion, ";")
	if len(groups) > 0 && len(groups[len(groups)-1]) == 0 {
		groups = groups[:len(groups)-1]
	}
	if len(groups) != len(frozenStructMembers) {
		return p.diag(line, tag, strconv.Itoa(len(frozenStructMembers))+" members with frozen order",
			strconv.Itoa(len(groups)), "struct member count mismatch")
	}
	members := make([]CStructMember, 0, len(groups))
	for i, g := range groups {
		if len(g) != 4 || !g[0].is("const") || !g[1].is("char") || !g[2].is("*") {
			return p.diag(line, tag, "const char* "+frozenStructMembers[i], tokenCanon(g),
				"struct member spelling mismatch")
		}
		if g[3].text != frozenStructMembers[i] {
			return p.diag(line, tag, "member "+frozenStructMembers[i]+" at position "+strconv.Itoa(i+1),
				g[3].text, "struct member name or order mismatch")
		}
		mt, d := p.resolveType(g[:3], line)
		if d != nil {
			return d
		}
		members = append(members, CStructMember{Name: g[3].text, Type: mt})
	}
	p.result.Structs = append(p.result.Structs, CStruct{Tag: tag, Header: p.curFile, Line: line,
		Members: members, Platform: plat})
	return nil
}

// processFunction 分派普通 ASSTAPI 声明与唯一的 ASSTAPI_PORT
// AsstGetVersion 拼写;ASSTLOCAL 等其他宏在声明中的出现一律失败。
func (p *cparser) processFunction(item []token, line int, plat string) *Diagnostic {
	apiCount, portCount, otherMacro := scanFunctionMacros(item)
	switch {
	case portCount > 0:
		if apiCount > 0 {
			return p.diag(line, "ASSTAPI_PORT", "frozen AsstGetVersion spelling only", tokenCanon(item),
				"ASSTAPI_PORT conflicts with ASSTAPI in this declaration")
		}
		return p.processGetVersion(item, line, plat)
	case apiCount == 1:
		if otherMacro != "" {
			return p.diag(line, otherMacro, "ASSTAPI as the only export macro", otherMacro,
				"macro is not accepted in this declaration position")
		}
		return p.processStandardFunction(item, line, plat)
	case apiCount > 1:
		return p.diag(line, "ASSTAPI", "single ASSTAPI per declaration", "multiple",
			"declaration carries more than one ASSTAPI")
	default:
		return p.diag(line, tokenCanon(item), "export declaration with ASSTAPI", tokenCanon(item),
			"declaration without the ASSTAPI export modifier")
	}
}

func scanFunctionMacros(item []token) (apiCount, portCount int, otherMacro string) {
	for _, t := range item {
		switch {
		case t.is("ASSTAPI"):
			apiCount++
		case t.is("ASSTAPI_PORT"):
			portCount++
		case t.is("ASSTLOCAL") || t.is("ASST_DLL_IMPORT") || t.is("ASST_DLL_EXPORT") ||
			t.is("ASST_DLL_LOCAL") || t.is("ASST_CALL"):
			if otherMacro == "" {
				otherMacro = t.text
			}
		}
	}
	return apiCount, portCount, otherMacro
}

// processGetVersion 处理唯一允许的 ASSTAPI_PORT 特殊拼写。
func (p *cparser) processGetVersion(item []token, line int, plat string) *Diagnostic {
	canon := tokenCanon(item)
	if canon != frozenGetVersionCanon {
		return p.diag(line, "AsstGetVersion", frozenGetVersionReadable, canon,
			"ASSTAPI_PORT is only accepted for the frozen AsstGetVersion spelling")
	}
	// token 形状固定:[ASSTAPI_PORT, const, char, *, ASST_CALL, AsstGetVersion, (, )]
	if len(item) != 8 {
		return p.diag(line, "AsstGetVersion", frozenGetVersionReadable, canon,
			"malformed AsstGetVersion declaration")
	}
	linkage, ccPort, d := p.expandExportMacro("ASSTAPI_PORT", line)
	if d != nil {
		return d
	}
	if ccPort != CallConvNone {
		return p.diag(line, "ASSTAPI_PORT", "no call convention from ASSTAPI_PORT", ccPort,
			"ASSTAPI_PORT must not introduce a call convention")
	}
	ret, d := p.resolveType(item[1:4], line)
	if d != nil {
		return d
	}
	cc, d := p.expandCallConvMacro(item[4].text, line)
	if d != nil {
		return d
	}
	name := item[5].text
	if name != "AsstGetVersion" {
		return p.diag(line, name, "AsstGetVersion", name, "malformed AsstGetVersion declaration")
	}
	p.result.Functions = append(p.result.Functions, CFunction{Name: name, Header: p.curFile,
		Line: line, Ret: ret, Params: nil, CallConv: cc, Linkage: linkage, Platform: plat, Spelling: canon})
	return nil
}

// processStandardFunction 处理 `返回类型 ASSTAPI 名称(参数列表);`。
func (p *cparser) processStandardFunction(item []token, line int, plat string) *Diagnostic {
	apiIdx := -1
	for i, t := range item {
		if t.is("ASSTAPI") {
			apiIdx = i
			break
		}
	}
	retToks := item[:apiIdx]
	if len(retToks) == 0 {
		return p.diag(line, "ASSTAPI", "return type before ASSTAPI", "missing",
			"return type is missing")
	}
	ret, d := p.resolveType(retToks, line)
	if d != nil {
		return d
	}
	linkage, callconv, d := p.expandExportMacro("ASSTAPI", line)
	if d != nil {
		return d
	}
	rest := item[apiIdx+1:]
	if len(rest) < 3 {
		return p.diag(line, "declaration", "Name(params)", tokenCanon(rest),
			"declaration is missing its name or parameter list")
	}
	name := rest[0].text
	if !isIdentSpell(name) || typeKeywords[name] || p.isMacroName(name) {
		return p.diag(rest[0].line, name, "function name", name,
			"function name must be a plain identifier")
	}
	if !rest[1].is("(") {
		return p.diag(rest[1].line, name, "("+" after function name", rest[1].text,
			"parameter list must follow the function name")
	}
	if !rest[len(rest)-1].is(")") {
		return p.diag(rest[len(rest)-1].line, name, ")"+" at end of declaration", rest[len(rest)-1].text,
			"parameter list must be closed")
	}
	if name == "AsstGetVersion" {
		return p.diag(line, name, frozenGetVersionReadable, tokenCanon(item),
			"AsstGetVersion must keep the frozen ASSTAPI_PORT spelling")
	}
	params, d := p.parseParamRegion(rest[2:len(rest)-1], name, line)
	if d != nil {
		return d
	}
	if frozenZeroArgFunctions[name] && len(params) != 0 {
		return p.diag(line, name, "zero-parameter export", "parameter list present",
			"known zero-arg function gained parameters")
	}
	p.result.Functions = append(p.result.Functions, CFunction{Name: name, Header: p.curFile,
		Line: line, Ret: ret, Params: params, CallConv: callconv, Linkage: linkage,
		Platform: plat, Spelling: tokenCanon(item)})
	return nil
}

// parseParamRegion 处理参数区:空 () 仅对三个锁定零参数符号按零参数
// 解释,(void) 对任何函数可识别;其余交给通用参数解析。
func (p *cparser) parseParamRegion(region []token, fnName string, line int) ([]CParam, *Diagnostic) {
	if len(region) == 0 {
		if !frozenZeroArgFunctions[fnName] {
			return nil, p.diag(line, fnName,
				"parameter list; empty () is only interpreted as zero parameters for AsstCreate, AsstGetNullSize and AsstGetVersion",
				"empty ()", "new empty () declarations require a profile extension")
		}
		return nil, nil
	}
	if len(region) == 1 && region[0].is("void") {
		return nil, nil
	}
	return p.parseParams(region, line, fnName)
}

// parseParams 解析逗号分隔的具名参数;每个参数必须带普通标识符名,
// 不接受匿名参数、变参与嵌套声明器。
func (p *cparser) parseParams(region []token, line int, context string) ([]CParam, *Diagnostic) {
	for _, t := range region {
		if t.is("(") {
			return nil, p.diag(t.line, context, "plain parameter types", "nested parentheses",
				"function-pointer parameters and nested declarators are not supported")
		}
	}
	groups := splitTopLevel(region, ",")
	params := make([]CParam, 0, len(groups))
	for _, g := range groups {
		if len(g) == 0 {
			return nil, p.diag(line, "parameter", "type and name", "empty parameter",
				"empty parameter in the parameter list")
		}
		nameTok := g[len(g)-1]
		if !isIdentSpell(nameTok.text) || typeKeywords[nameTok.text] || p.isMacroName(nameTok.text) {
			return nil, p.diag(nameTok.line, context, "parameter name", nameTok.text,
				"parameter must end with a plain identifier name")
		}
		typeToks := g[:len(g)-1]
		if len(typeToks) == 0 {
			return nil, p.diag(nameTok.line, context, "parameter type", "missing",
				"parameter type is missing")
		}
		ct, d := p.resolveType(typeToks, line)
		if d != nil {
			return nil, d
		}
		if ct.Kind == CKindVoid {
			return nil, p.diag(nameTok.line, nameTok.text, "non-void parameter type", "void",
				"void is only valid as the single (void) zero-parameter marker")
		}
		params = append(params, CParam{Name: nameTok.text, Type: ct})
	}
	return params, nil
}

// resolveType 解析受限类型拼写:const 前缀、void、stdint 固定标量、
// char、struct Tag、已知 typedef;至多一层指针,指针后不得再带
// const 或其他 token。经别名展开时在 Via 保留使用处原名。
func (p *cparser) resolveType(toks []token, line int) (CType, *Diagnostic) {
	i := 0
	isConst := false
	if i < len(toks) && toks[i].is("const") {
		isConst = true
		i++
	}
	if i >= len(toks) {
		return CType{}, p.diag(line, "type", "type spelling", "empty", "type is missing")
	}
	var base CType
	tok := toks[i]
	switch {
	case tok.is("struct"):
		if i+1 >= len(toks) {
			return CType{}, p.diag(line, "struct", "struct tag", "missing", "struct tag is missing")
		}
		tag := toks[i+1].text
		if tag != "AsstExtAPI" && tag != frozenStructTag {
			return CType{}, p.diag(line, tag, "AsstExtAPI or "+frozenStructTag, tag,
				"unknown struct tag")
		}
		base = CType{Kind: CKindStruct, Tag: tag}
		i += 2
	case tok.is("void"):
		base = CType{Kind: CKindVoid}
		i++
	default:
		if sc, ok := scalarCType(tok.text); ok {
			base = sc
			i++
			break
		}
		if def, ok := p.typedefs[tok.text]; ok {
			base = def
			base.Via = tok.text
			i++
			break
		}
		if p.isMacroName(tok.text) {
			return CType{}, p.diag(line, tok.text, "type name", "macro",
				"macro is not valid in a type position")
		}
		return CType{}, p.diag(line, tok.text, "known scalar, alias or struct", tok.text,
			"unknown type name (unknown typedef)")
	}
	base.Const = isConst
	if i < len(toks) && toks[i].is("*") {
		i++
		if i < len(toks) {
			return CType{}, p.diag(line, tokenCanon(toks), "single pointer level", tokenCanon(toks[i:]),
				"multi-level pointers, pointer const or trailing tokens are not supported")
		}
		inner := base
		base = CType{Kind: CKindPointer, Pointee: &inner}
		return base, nil
	}
	if i < len(toks) {
		return CType{}, p.diag(line, toks[i].text, "end of type", tokenCanon(toks[i:]),
			"unexpected tokens after the type")
	}
	return base, nil
}

// expandExportMacro 展开导出宏并收集修饰:必须得到非空 import/export
// 修饰(不允许靠剥掉大写词蒙混)且调用约定只能为空或 __stdcall;
// sysv_abi(Wine)不属于本 profile。
func (p *cparser) expandExportMacro(name string, line int) (string, string, *Diagnostic) {
	linkage, callconv, d := p.collectModifiersFromMacro(name, line)
	if d != nil {
		return "", "", d
	}
	if linkage == LinkageNone {
		return "", "", p.diag(line, name, "import/export decoration", "none",
			name+" must expand to an export/import modifier plus the correct call convention")
	}
	if callconv != CallConvNone && callconv != CallConvStdcall {
		return "", "", p.diag(line, name, "empty or __stdcall call convention", callconv,
			"call convention outside the locked profile (Wine/Cygwin are rejected)")
	}
	return linkage, callconv, nil
}

// expandCallConvMacro 展开纯调用约定宏(ASST_CALL)。
func (p *cparser) expandCallConvMacro(name string, line int) (string, *Diagnostic) {
	linkage, callconv, d := p.collectModifiersFromMacro(name, line)
	if d != nil {
		return "", d
	}
	if linkage != LinkageNone {
		return "", p.diag(line, name, "call-convention-only expansion", linkage,
			"macro must not introduce an export/import modifier here")
	}
	return callconv, nil
}

func (p *cparser) collectModifiersFromMacro(name string, line int) (string, string, *Diagnostic) {
	exp, d := p.expandMacro(name, line, 0)
	if d != nil {
		return "", "", d
	}
	linkage, callconv, rest, d := p.collectModifiers(exp, line)
	if d != nil {
		return "", "", d
	}
	if len(rest) != 0 {
		return "", "", p.diag(line, name,
			"expansion to import/export and call-convention modifiers", tokenCanon(rest),
			"macro expansion contains unexpected tokens")
	}
	return linkage, callconv, nil
}

// expandMacro 展开 Port 宏(含嵌套引用,带环防御);未定义的宏失败。
func (p *cparser) expandMacro(name string, line int, depth int) ([]token, *Diagnostic) {
	if depth > maxMacroDepth {
		return nil, p.diag(line, name, "macro expansion without cycles", "expansion depth exceeded",
			"macro alias cycle detected")
	}
	value, ok := p.macros[name]
	if !ok {
		return nil, p.diag(line, name, "defined by "+FileAsstPort, "undefined",
			"macro is not defined by AsstPort.h; external predefinitions are rejected")
	}
	var out []token
	for _, t := range value {
		if p.isMacroName(t.text) {
			sub, d := p.expandMacro(t.text, line, depth+1)
			if d != nil {
				return nil, d
			}
			out = append(out, sub...)
			continue
		}
		out = append(out, t)
	}
	return out, nil
}

// collectModifiers 从展开 token 中收集导出修饰与调用约定,遇到首个
// 非 modifier token 停止。
func (p *cparser) collectModifiers(toks []token, line int) (string, string, []token, *Diagnostic) {
	linkage, callconv := LinkageNone, CallConvNone
	match := func(pos int, want ...string) bool {
		if pos+len(want) > len(toks) {
			return false
		}
		for k, w := range want {
			if !toks[pos+k].is(w) {
				return false
			}
		}
		return true
	}
	dup := func(kind, actual string) *Diagnostic {
		return p.diag(line, kind, "single "+kind+" modifier", actual, "duplicate modifier in expansion")
	}
	for i := 0; i < len(toks); {
		switch {
		case toks[i].is("__declspec") && match(i+1, "(", "dllimport", ")"):
			if linkage != LinkageNone {
				return "", "", nil, dup("linkage", toks[i].text)
			}
			linkage = LinkageDLLImport
			i += 4
		case toks[i].is("__declspec") && match(i+1, "(", "dllexport", ")"):
			if linkage != LinkageNone {
				return "", "", nil, dup("linkage", toks[i].text)
			}
			linkage = LinkageDLLExport
			i += 4
		case toks[i].is("__attribute__") && match(i+1, "(", "(", "visibility", "(", `"default"`, ")", ")", ")"):
			if linkage != LinkageNone {
				return "", "", nil, dup("linkage", toks[i].text)
			}
			linkage = LinkageVisibilityDefault
			i += 9
		case toks[i].is("__attribute__") && match(i+1, "(", "(", "visibility", "(", `"hidden"`, ")", ")", ")"):
			if linkage != LinkageNone {
				return "", "", nil, dup("linkage", toks[i].text)
			}
			linkage = LinkageVisibilityHidden
			i += 9
		case toks[i].is("__attribute__") && match(i+1, "(", "(", "sysv_abi", ")", ")"):
			if callconv != CallConvNone {
				return "", "", nil, dup("call convention", toks[i].text)
			}
			callconv = CallConvSysVABI
			i += 6
		case toks[i].is("__stdcall"):
			if callconv != CallConvNone {
				return "", "", nil, dup("call convention", toks[i].text)
			}
			callconv = CallConvStdcall
			i++
		default:
			return linkage, callconv, toks[i:], nil
		}
	}
	return linkage, callconv, nil, nil
}
