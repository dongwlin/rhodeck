// compare.go 在 link 与锁定期望比对成功之后执行宽度级 C↔Go 逐参数
// 语义比对:对每条绑定条目——link.Bound 的目标适用绑定与
// link.NonTarget 的非目标平台 windowsOnly 绑定——取 C 函数声明与对应
// Go api 字段做逐位置具名序列比对(参数个数、参数名映射、类型映射、
// 返回值映射),并对两个 Go callback 类型与 C 的 AsstApiCallback
// typedef 做平台适配比对。失败以 signature 类诊断报第一条,定位到
// 过错方(Go 参数名/类型错误定位到 Go 字段或 callback 声明;C 类型无
// 许可映射定位到 C 声明),含符号、参数位置与名称、expected/actual
// (C 侧类型用 CType.String())。
//
// 类型映射是绑定设计 §3 冻结的常量表,不做任何猜测(不实现通用 C
// 编译器/数据流分析);其余一切 C 类型形状无许可映射,出现即失败:
//   - uint8 标量 → uint8(byte 是同类型别名,同样接受);int32 标量
//     → int32;uint64 标量 → uint64;const 标量与 char 标量无许可
//     映射;
//   - pointee 为 char 的指针(const char* 与 char*)→ *byte,但
//     pointee 的 const 标志必须与冻结分布一致:非 const 仅许可
//     AsstGetUUID 的 buff(UTF-8 输出缓冲区),其余任何位置丢
//     const 或该位置加 const 均无许可映射(设计 §9.1.2 改变
//     const 失败);pointee 为 int32 的指针(任务列表)→ *int32;
//     其余 pointee(含 uint8*、二级指针)无许可映射;
//   - struct AsstExtAPI*(opaque handle)→ uintptr;
//   - callback 函数指针 → uintptr;
//   - void* 仅三种冻结上下文许可:C 参数名恰为 custom_arg → uintptr
//     (token);恰为 hwnd 且符号为被绑定的窗口符号
//     AsstAsyncAttachWindow → uintptr(窗口句柄);恰为 buff 且符号
//     为 AsstGetImage/AsstGetImageBgr → unsafe.Pointer(图像缓冲区)。
//     其余任何 void* 位置失败。
//
// 参数名映射仅允许设计 §3 冻结的 8 个重命名,其余参数必须原样同名;
// 匿名与空白参数名、未批准重命名以及同类型参数调序(x/y、
// level/message、callback/customArg、窗口 method)因此失败。返回值:
// C void → Go 必须无结果列表(不虚构成功码);其余按同一类型映射,
// 返回位置没有参数名,故 void* 返回无许可映射。结果个数恰为 0 或 1
// 由 Go 提取层保证,此处按单一结果值比对。
//
// callback 平台适配(设计 §4 与 §9.1):C 的 AsstApiCallback 在两
// 平台都声明为 void(ASST_CALL*)(AsstMsgId, const char*, void*),
// 无条件分支。unixCallback 无返回值(C void 原样);windowsCallback
// 参数与 unix 相同,返回值必须恰为 uintptr——这是 purego 装配层的
// 许可适配(C 仍是 void,trampoline 函数体显式返回常量 0),不是
// C↔Go 不一致。两个 callback 声明无平台条件,无论当前目标是什么都
// 检查两个。

package main

import (
	"fmt"
	"sort"
	"strings"
)

// approvedParamNames 是设计 §3 冻结的参数名映射(C 名 → Go 名);仅
// 此 8 个允许重命名,其余参数必须原样同名。
var approvedParamNames = map[string]string{
	"type":             "taskType",
	"adb_path":         "adbPath",
	"custom_arg":       "customArg",
	"buff_size":        "buffSize",
	"details_json":     "detailsJSON",
	"screencap_method": "screencapMethod",
	"mouse_method":     "mouseMethod",
	"keyboard_method":  "keyboardMethod",
}

// approvedParamName 返回 C 参数名经批准映射后的 Go 参数名;未批准
// 重命名的 C 名原样保留(必须同名)。
func approvedParamName(cName string) string {
	if goName, ok := approvedParamNames[cName]; ok {
		return goName
	}
	return cName
}

// void* 指针用途的冻结符号上下文:窗口句柄仅 AsstAsyncAttachWindow
// (唯一被绑定的窗口符号;deprecated 的 AsstAttachWindow 被精确排除、
// 永无绑定,到不了比对层,不构成许可上下文),图像缓冲区仅
// AsstGetImage/AsstGetImageBgr;custom_arg token 按参数名许可(带它
// 的符号集合由 C 输入的锁定导出集合约束)。
var (
	voidPtrWindowSymbols = newSet("AsstAsyncAttachWindow")
	voidPtrImageSymbols  = newSet("AsstGetImage", "AsstGetImageBgr")
)

// frozenNonConstCharPointees 是 char pointee 冻结 const 分布中许可非
// const 的 (符号, 参数名) 集合:锁定 profile 全集(与归档
// upstream/AsstCaller.h 逐一核对一致)仅 AsstGetUUID 的 buff 是
// UTF-8 输出缓冲区 char*,其余 char pointee(各 UTF-8 输入参数、
// callback 的 details_json、AsstGetVersion 的返回值)全部为 const
// char*。返回位置没有参数名,不在集合内。
var frozenNonConstCharPointees = map[string]map[string]bool{
	"AsstGetUUID": newSet("buff"),
}

// permittedGoTypes 返回 C 规范化类型在给定符号/参数上下文中许可的 Go
// raw 类型拼写(固定顺序;uint8 的 byte 别名随后);空切片表示无
// 许可映射。paramName 为返回位置传入空串,void* 的三个上下文许可都
// 依赖参数名,故返回位置的 void* 无许可映射。
func permittedGoTypes(symbol, paramName string, ct CType) []GoSourceType {
	switch ct.Kind {
	case CKindScalar:
		if ct.Const {
			return nil // const 标量无许可映射
		}
		switch ct.Name {
		case "uint8":
			return []GoSourceType{"uint8", "byte"}
		case "int32":
			return []GoSourceType{"int32"}
		case "uint64":
			return []GoSourceType{"uint64"}
		}
		return nil // char 等其余标量无许可映射
	case CKindPointer:
		pt := ct.Pointee
		if pt == nil {
			return nil
		}
		switch pt.Kind {
		case CKindScalar:
			switch pt.Name {
			case "char":
				// const char*(UTF-8 输入/返回)与 char*(UUID 输出
				// 缓冲区)都映射 *byte,但 pointee 的 const 标志必须
				// 与冻结分布一致:pointee 已 const 而此处要求非
				// const(UUID buff 加 const),或 pointee 非 const 而
				// 此处要求 const(其余位置丢 const),均无许可映射。
				if pt.Const == frozenNonConstCharPointees[symbol][paramName] {
					return nil
				}
				return []GoSourceType{"*byte"}
			case "int32":
				// 任务列表 AsstTaskId*。
				return []GoSourceType{"*int32"}
			}
			return nil // uint8* 等其余 pointee 无许可映射
		case CKindVoid:
			switch {
			case paramName == "custom_arg":
				return []GoSourceType{"uintptr"}
			case paramName == "hwnd" && voidPtrWindowSymbols[symbol]:
				return []GoSourceType{"uintptr"}
			case paramName == "buff" && voidPtrImageSymbols[symbol]:
				return []GoSourceType{GoTypeUnsafePointer}
			}
			return nil
		case CKindStruct:
			if pt.Tag == "AsstExtAPI" {
				return []GoSourceType{"uintptr"} // opaque handle
			}
			return nil
		}
		return nil // 二级指针等无许可映射
	case CKindFuncPtr:
		return []GoSourceType{"uintptr"} // callback 函数指针
	}
	return nil
}

// containsGoType 判断拼写是否在许可集合内。
func containsGoType(list []GoSourceType, t GoSourceType) bool {
	for _, v := range list {
		if v == t {
			return true
		}
	}
	return false
}

// joinGoTypes 以固定顺序输出许可拼写,供诊断 expected 使用。
func joinGoTypes(list []GoSourceType) string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, string(v))
	}
	return strings.Join(out, " or ")
}

// charPointeeConstDiag 在 C 类型是单层 char 指针且 pointee 的 const
// 标志偏离冻结分布时,给出定位到 C 声明的 signature 诊断:expected
// 取该位置的冻结拼写,actual 用 CType.String()(自带 const 前缀),
// 两侧均显式表达 const 语义。类型形状不符或 const 一致时返回 nil,
// 由调用方走通用无许可映射诊断。position 从 1 起为参数位置,0 表示
// 返回位置。
func charPointeeConstDiag(symbol, paramName string, position int, ct CType, cLoc sigLocation) *Diagnostic {
	if ct.Kind != CKindPointer || ct.Pointee == nil ||
		ct.Pointee.Kind != CKindScalar || ct.Pointee.Name != "char" {
		return nil
	}
	nonConstOK := frozenNonConstCharPointees[symbol][paramName]
	// 触发条件:本函数仅被 permittedGoTypes 因 const 失配返回无许可
	// 映射的调用方调用,故此处 Const == nonConstOK 恰为偏离冻结分布
	// (const 要求恒为 nonConstOK 的补集);一致时返回 nil 交回通用
	// 无许可映射诊断。
	if ct.Pointee.Const != nonConstOK {
		return nil
	}
	want := "const char*"
	if nonConstOK {
		want = "char*"
	}
	what := "the return type"
	if position > 0 {
		what = fmt.Sprintf("parameter %d (%s)", position, paramName)
	}
	return &Diagnostic{Category: CatSignature, File: cLoc.file, Line: cLoc.line, Symbol: symbol,
		Expected: want,
		Actual:   ct.String(),
		Message: fmt.Sprintf("%s: the char pointee constness must match the frozen distribution; "+
			"dropping or adding const on the c side fails", what)}
}

// sigLocation 是比对双方的声明位置(C 头或 Go 文件 + 行号)。
type sigLocation struct {
	file string
	line int
}

// compareSignatureParams 逐位置比对具名参数序列:个数必须相等;Go
// 参数名必须等于 C 名经批准映射的结果;Go raw 类型必须落在 C 规范化
// 类型的许可映射内。返回第一条失败诊断,无失败返回 nil。
func compareSignatureParams(symbol string, cParams []CParam, goParams []GoParam, cLoc, goLoc sigLocation) *Diagnostic {
	if len(cParams) != len(goParams) {
		return &Diagnostic{Category: CatSignature, File: cLoc.file, Line: cLoc.line, Symbol: symbol,
			Expected: fmt.Sprintf("%d named parameters", len(cParams)),
			Actual:   fmt.Sprintf("%d named parameters", len(goParams)),
			Message:  "parameter count differs; the go declaration must carry exactly the frozen c parameter sequence"}
	}
	for i := range cParams {
		cp, gp := cParams[i], goParams[i]
		wantName := approvedParamName(cp.Name)
		if gp.Name != wantName {
			return &Diagnostic{Category: CatSignature, File: goLoc.file, Line: goLoc.line, Symbol: symbol,
				Expected: fmt.Sprintf("parameter %d name %s", i+1, wantName),
				Actual:   fmt.Sprintf("parameter %d name %s", i+1, gp.Name),
				Message: fmt.Sprintf("c parameter %q: the go name must equal the c name after the approved mapping; "+
					"unapproved renames and same-type reordering fail", cp.Name)}
		}
		permitted := permittedGoTypes(symbol, cp.Name, cp.Type)
		if len(permitted) == 0 {
			if d := charPointeeConstDiag(symbol, cp.Name, i+1, cp.Type, cLoc); d != nil {
				return d
			}
			return &Diagnostic{Category: CatSignature, File: cLoc.file, Line: cLoc.line, Symbol: symbol,
				Expected: "a c parameter type with a frozen go mapping",
				Actual:   cp.Type.String(),
				Message: fmt.Sprintf("parameter %d (%s): no approved go mapping for this c type; "+
					"unapproved pointer uses, const scalars and unsupported widths fail", i+1, cp.Name)}
		}
		if !containsGoType(permitted, gp.Type) {
			return &Diagnostic{Category: CatSignature, File: goLoc.file, Line: goLoc.line, Symbol: symbol,
				Expected: joinGoTypes(permitted),
				Actual:   string(gp.Type),
				Message: fmt.Sprintf("parameter %d (%s): the go raw type must match the frozen c mapping",
					i+1, cp.Name)}
		}
	}
	return nil
}

// compareSignatureReturn 比对返回值映射:C void → Go 必须无结果;
// 其余按同一类型映射(返回位置无参数名,void* 返回无许可映射)。
func compareSignatureReturn(symbol string, cRet CType, goResult GoSourceType, cLoc, goLoc sigLocation) *Diagnostic {
	if cRet.Kind == CKindVoid {
		if goResult != "" {
			return &Diagnostic{Category: CatSignature, File: goLoc.file, Line: goLoc.line, Symbol: symbol,
				Expected: "no result (the c function returns void)",
				Actual:   string(goResult),
				Message:  "a void c function must not gain a go result; invented success codes fail"}
		}
		return nil
	}
	permitted := permittedGoTypes(symbol, "", cRet)
	if len(permitted) == 0 {
		if d := charPointeeConstDiag(symbol, "", 0, cRet, cLoc); d != nil {
			return d
		}
		return &Diagnostic{Category: CatSignature, File: cLoc.file, Line: cLoc.line, Symbol: symbol,
			Expected: "a c return type with a frozen go mapping",
			Actual:   cRet.String(),
			Message:  "no approved go mapping for this c return type"}
	}
	if !containsGoType(permitted, goResult) {
		return &Diagnostic{Category: CatSignature, File: goLoc.file, Line: goLoc.line, Symbol: symbol,
			Expected: joinGoTypes(permitted),
			Actual:   string(goResult),
			Message:  "the go result type must match the frozen c return mapping"}
	}
	return nil
}

// SignatureParam 是一个参数位置的核验记录:位置从 1 起,C 名与批准
// 映射后的 Go 名、C 规范化拼写与 Go raw 拼写。
type SignatureParam struct {
	Position int
	CName    string
	GoName   string
	CType    string
	GoType   string
}

// SignatureReturn 是返回位置的核验记录;C void 与 Go 无结果都以空
// 串表达。
type SignatureReturn struct {
	CType  string
	GoType string
}

// SignatureEntry 是一个绑定符号的逐参数核验记录。
type SignatureEntry struct {
	Symbol string
	Field  string
	Header string // C 声明来源头
	CLine  int
	GoFile string
	GoLine int
	Params []SignatureParam
	Return SignatureReturn
}

// SignatureCallbackEntry 是一个 callback 类型的核验记录。
type SignatureCallbackEntry struct {
	Name   string
	GoFile string
	GoLine int
	Params []SignatureParam
	Return SignatureReturn
}

// SignatureResult 是签名比对结果:Symbols 覆盖全部绑定条目
// (link.Bound 的目标适用绑定与 link.NonTarget 的非目标 windowsOnly
// 绑定,两目标均为全部 26 个生产字段,按符号名排序,与目标无关),
// Callbacks 覆盖两个 callback 类型(unixCallback、windowsCallback
// 固定顺序)。
type SignatureResult struct {
	Symbols   []SignatureEntry
	Callbacks []SignatureCallbackEntry
}

// CompareSignatures 执行宽度级签名比对:先对每条绑定条目(Bound 与
// NonTarget 的 windowsOnly 绑定)比对 C 函数与 Go 字段,再对两个
// callback 类型比对 C 的 AsstApiCallback typedef。失败返回单条
// signature 诊断,定位到过错方。
func CompareSignatures(c *CExtraction, g *GoExtraction, link *LinkResult) (*SignatureResult, []Diagnostic) {
	fail := func(d Diagnostic) (*SignatureResult, []Diagnostic) { return nil, []Diagnostic{d} }
	if c == nil || g == nil || link == nil {
		return fail(Diagnostic{Category: CatSignature, Symbol: "extraction",
			Expected: "validated c extraction, go extraction and link result", Actual: "nil",
			Message: "signature comparison requires validated extraction and link results"})
	}

	cByName := map[string]CFunction{}
	for i := range c.Functions {
		cByName[c.Functions[i].Name] = c.Functions[i]
	}
	fieldByName := map[string]GoField{}
	for _, f := range g.Fields {
		fieldByName[f.Name] = f
	}

	res := &SignatureResult{}
	// compareEntry 比对单个绑定符号的 C 声明与 Go 字段并追加核验记
	// 录;C 声明或 Go 字段缺失、参数或返回值失配都返回诊断,由调用方
	// 失败关闭。C 侧一律取完整声明集合(c.Functions 在两目标都含
	// windowsOnly 声明)。
	compareEntry := func(symbol, field string) *Diagnostic {
		cf, ok := cByName[symbol]
		if !ok {
			return &Diagnostic{Category: CatSignature, Symbol: symbol,
				Expected: "c declaration for the compared binding", Actual: "missing",
				Message: "signature comparison requires the c declaration of every compared binding"}
		}
		gf, ok := fieldByName[field]
		if !ok {
			return &Diagnostic{Category: CatSignature, Symbol: symbol,
				Expected: "api field for the compared binding", Actual: "missing",
				Message: "signature comparison requires the go field of every compared binding"}
		}
		cLoc := sigLocation{file: cf.Header, line: cf.Line}
		goLoc := sigLocation{file: gf.File, line: gf.Line}
		if d := compareSignatureParams(symbol, cf.Params, gf.Params, cLoc, goLoc); d != nil {
			return d
		}
		if d := compareSignatureReturn(symbol, cf.Ret, gf.Result, cLoc, goLoc); d != nil {
			return d
		}
		entry := SignatureEntry{Symbol: symbol, Field: field, Header: cf.Header, CLine: cf.Line,
			GoFile: gf.File, GoLine: gf.Line,
			Return: SignatureReturn{CType: cf.Ret.String(), GoType: string(gf.Result)}}
		for i := range cf.Params {
			entry.Params = append(entry.Params, SignatureParam{Position: i + 1,
				CName: cf.Params[i].Name, GoName: gf.Params[i].Name,
				CType: cf.Params[i].Type.String(), GoType: string(gf.Params[i].Type)})
		}
		res.Symbols = append(res.Symbols, entry)
		return nil
	}
	for _, b := range link.Bound {
		if d := compareEntry(b.Symbol, b.Field); d != nil {
			return fail(*d)
		}
	}
	// NonTarget 的 windowsOnly 绑定同样逐参数核验:api.go 与
	// symbols.go 无平台条件,其声明真实参与两目标构建,窗口符号的字段
	// 签名破坏(如参数调序)必须在任何目标下都报出,不能只靠 windows
	// 目标发现——与 policy 比对一致,signature 比对不接收 Target。
	// spec 条目由 link 保证存在(link 只在 windowsOnly 条目存在时才
	// 分类 NonTarget),此处防御性缺失仍失败关闭。
	specByName := map[string]GoSymbolSpec{}
	for _, s := range g.Specs {
		if _, dup := specByName[s.Name]; !dup {
			specByName[s.Name] = s
		}
	}
	for _, name := range link.NonTarget {
		s, ok := specByName[name]
		if !ok {
			return fail(Diagnostic{Category: CatSignature, Symbol: name,
				Expected: "symbolSpec entry for the non-target windowsOnly binding", Actual: "missing",
				Message: "signature comparison requires the spec entry of every compared binding"})
		}
		if d := compareEntry(name, s.Field); d != nil {
			return fail(*d)
		}
	}
	// link.Bound 与 link.NonTarget 已按符号名排序;此处独立排序,保证
	// 报告契约不依赖 link 内部实现。
	sort.Slice(res.Symbols, func(i, j int) bool { return res.Symbols[i].Symbol < res.Symbols[j].Symbol })

	// C 的 AsstApiCallback typedef:解析层冻结其精确拼写,提取成功后
	// 必然存在;防御性缺失即失败关闭。
	var cbTypedef *CTypedef
	for i := range c.Typedefs {
		if c.Typedefs[i].Name == "AsstApiCallback" {
			cbTypedef = &c.Typedefs[i]
			break
		}
	}
	if cbTypedef == nil || cbTypedef.Type.Kind != CKindFuncPtr || cbTypedef.Type.Sig == nil {
		return fail(Diagnostic{Category: CatSignature, Symbol: "AsstApiCallback",
			Expected: "the frozen callback function-pointer typedef", Actual: "missing or malformed",
			Message: "signature comparison requires the c callback typedef"})
	}
	cLoc := sigLocation{file: cbTypedef.Header, line: cbTypedef.Line}
	// 两个 callback 声明无平台条件,无论目标是什么都检查两个;返回值
	// 规则按平台适配:unix 原样 void,windows 许可适配为 uintptr。
	callbackCases := []struct {
		callback   *GoCallback
		wantResult GoSourceType
		contract   string
	}{
		{g.UnixCallback, "", "the c callback returns void; the unix adaptation keeps it"},
		{g.WindowsCallback, "uintptr", "the windows purego adaptation returns a constant 0 uintptr"},
	}
	for _, cc := range callbackCases {
		cb := cc.callback
		if cb == nil {
			return fail(Diagnostic{Category: CatSignature, Symbol: "AsstApiCallback",
				Expected: "both callback type declarations", Actual: "missing",
				Message: "unixCallback and windowsCallback must both be declared without platform conditions"})
		}
		goLoc := sigLocation{file: cb.File, line: cb.Line}
		if d := compareSignatureParams(cb.Name, cbTypedef.Type.Sig.Params, cb.Params, cLoc, goLoc); d != nil {
			return fail(*d)
		}
		if cb.Result != cc.wantResult {
			return fail(Diagnostic{Category: CatSignature, File: cb.File, Line: cb.Line, Symbol: cb.Name,
				Expected: fmt.Sprintf("result %s", quoteEmpty(string(cc.wantResult))),
				Actual:   string(cb.Result),
				Message:  cc.contract})
		}
		entry := SignatureCallbackEntry{Name: cb.Name, GoFile: cb.File, GoLine: cb.Line,
			Return: SignatureReturn{CType: cbTypedef.Type.Sig.Ret.String(), GoType: string(cb.Result)}}
		for i := range cb.Params {
			entry.Params = append(entry.Params, SignatureParam{Position: i + 1,
				CName: cbTypedef.Type.Sig.Params[i].Name, GoName: cb.Params[i].Name,
				CType: cbTypedef.Type.Sig.Params[i].Type.String(), GoType: string(cb.Params[i].Type)})
		}
		res.Callbacks = append(res.Callbacks, entry)
	}
	return res, nil
}
