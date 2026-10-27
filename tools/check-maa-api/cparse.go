// cparse.go 实现受限 C 预处理:只支持归档已有的语法,未知相关内容
// 即失败并给出文件与行号,不扩展为通用 C 编译器。覆盖:#pragma once、
// 行/块注释、固定 include(从 c 输入目录内相对解析,<stdint.h> 只用
// 固定标量映射)、 冻结的条件拼写(#ifdef/#if/#elif/#else/
// #endif,非活跃分支同样检查)、AsstPort.h 的锁定宏表(名称与右值都
// 必须精确匹配,defines 不允许出现在其他头文件)、extern "C" 链接
// 包装,以及按顶层分号/花括号累积声明项。

package main

import (
	"fmt"
	"path"
	"strings"
)

// maxIncludeDepth 与 maxMacroDepth 防御 include 环与宏别名环。
const (
	maxIncludeDepth = 8
	maxMacroDepth   = 16
)

type token struct {
	text string
	line int
}

func (t token) is(text string) bool { return t.text == text }

// tokenCanon 把 token 序列压缩为忽略空白/注释差异的规范拼写。
func tokenCanon(toks []token) string {
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(t.text)
	}
	return b.String()
}

var punctuators = []string{
	"<<", ">>", "&&", "||", "==", "!=", ">=", "<=", "->", "++", "--",
	"+", "-", "*", "/", "%", "&", "|", "^", "~", "!", "<", ">",
	"=", "(", ")", "{", "}", "[", "]", ";", ",", ".", "?", ":",
}

func matchPunct(s string) (string, bool) {
	for _, op := range punctuators {
		if strings.HasPrefix(s, op) {
			return op, true
		}
	}
	return "", false
}

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// isIdentSpell 判断拼写是否为完整标识符(用于参数名与函数名)。
func isIdentSpell(s string) bool {
	if s == "" || !isIdentStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isIdentPart(s[i]) {
			return false
		}
	}
	return true
}

// typeKeywords 是不得用作参数名/函数名的类型关键字与标量拼写。
var typeKeywords = map[string]bool{
	"const": true, "struct": true, "enum": true, "typedef": true, "extern": true,
	"void": true, "char": true, "uint8_t": true, "int32_t": true, "uint64_t": true,
}

// stripComments 把行/块注释替换为空白并保留换行结构(行号不变),
// 拒绝行续接与未闭合注释;字符串字面量原样保留。
func stripComments(file string, src []byte) (string, *Diagnostic) {
	var b strings.Builder
	i, n := 0, len(src)
	line := 1
	fail := func(msg string) *Diagnostic {
		return &Diagnostic{Category: CatCSyntax, File: file, Line: line,
			Expected: "supported C syntax", Actual: msg,
			Message: "comment or lexical structure outside the restricted C syntax"}
	}
	for i < n {
		c := src[i]
		switch {
		case c == '\r':
			i++
		case c == '/' && i+1 < n && src[i+1] == '/':
			for i < n && src[i] != '\n' {
				i++
			}
			b.WriteByte(' ')
		case c == '/' && i+1 < n && src[i+1] == '*':
			i += 2
			b.WriteByte(' ')
			closed := false
			for i < n {
				if src[i] == '*' && i+1 < n && src[i+1] == '/' {
					i += 2
					closed = true
					break
				}
				if src[i] == '\n' {
					b.WriteByte('\n')
					line++
				}
				i++
			}
			if !closed {
				return "", fail("unterminated block comment")
			}
		case c == '"':
			j := i + 1
			for j < n && src[j] != '"' && src[j] != '\n' {
				if src[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				j++
			}
			if j >= n || src[j] == '\n' {
				return "", fail("unterminated string literal")
			}
			b.Write(src[i : j+1])
			i = j + 1
		case c == '\\' && i+1 < n && src[i+1] == '\n':
			return "", fail("line continuation is not supported")
		default:
			if c == '\n' {
				line++
			}
			b.WriteByte(c)
			i++
		}
	}
	return b.String(), nil
}

// tokenize 按受限词法切分一行:标识符、十进制数字串、字符串字面量
// 与已知 punctuator;其他字符失败。
func tokenize(file, s string, line int) ([]token, *Diagnostic) {
	var out []token
	i, n := 0, len(s)
	for i < n {
		c := s[i]
		switch {
		case c == ' ' || c == '\t':
			i++
		case c == '"':
			j := i + 1
			for j < n && s[j] != '"' {
				if s[j] == '\\' && j+1 < n {
					j += 2
					continue
				}
				j++
			}
			if j >= n {
				return nil, &Diagnostic{Category: CatCSyntax, File: file, Line: line,
					Expected: "terminated string literal", Actual: "unterminated",
					Message: "string literal is not closed"}
			}
			out = append(out, token{s[i : j+1], line})
			i = j + 1
		case isIdentStart(c):
			j := i + 1
			for j < n && isIdentPart(s[j]) {
				j++
			}
			out = append(out, token{s[i:j], line})
			i = j
		case c >= '0' && c <= '9':
			j := i + 1
			for j < n && isIdentPart(s[j]) {
				j++
			}
			out = append(out, token{s[i:j], line})
			i = j
		default:
			op, ok := matchPunct(s[i:])
			if !ok {
				return nil, &Diagnostic{Category: CatCSyntax, File: file, Line: line,
					Expected: "supported C syntax", Actual: fmt.Sprintf("unexpected character %q", c),
					Message: "character outside the restricted C syntax"}
			}
			out = append(out, token{op, line})
			i += len(op)
		}
	}
	return out, nil
}

// condKind 区分条件框架的语义:_WIN32 标记 windows 内容,
// ASST_WITH_EXTRA_CALLERS 标记实验内容,__cplusplus 只作链接包装,
// 其余为 Port 内部的宏分支。
type condKind int

const (
	condPlain condKind = iota
	condWindow
	condExtra
	condCPlusPlus
)

type condSpec struct {
	kind condKind
	eval func(macroEnv) bool
}

// 冻结的条件拼写集(忽略空白/注释差异);未知条件一律失败,含当前
// 目标不活跃分支中的未知条件。
var knownIfdefNames = map[string]condSpec{
	"_WIN32":           {condWindow, func(e macroEnv) bool { return e.isDefined("_WIN32") }},
	"__cplusplus":      {condCPlusPlus, func(e macroEnv) bool { return e.isDefined("__cplusplus") }},
	"ASST_DLL_EXPORTS": {condPlain, func(e macroEnv) bool { return e.isDefined("ASST_DLL_EXPORTS") }},
}

var knownIfConditions = map[string]condSpec{
	"ASST_WITH_EXTRA_CALLERS": {condExtra,
		func(e macroEnv) bool { return e.intValue("ASST_WITH_EXTRA_CALLERS") != 0 }},
	"!defined(ASST_CALL)": {condPlain,
		func(e macroEnv) bool { return !e.isDefined("ASST_CALL") }},
	"defined(__WINE__)&&defined(__x86_64__)": {condPlain,
		func(e macroEnv) bool { return e.isDefined("__WINE__") && e.isDefined("__x86_64__") }},
	"(defined(_WIN32)||defined(__CYGWIN__))&&!defined(__WINE__)": {condPlain,
		func(e macroEnv) bool {
			return (e.isDefined("_WIN32") || e.isDefined("__CYGWIN__")) && !e.isDefined("__WINE__")
		}},
	"__GNUC__>=4": {condPlain,
		func(e macroEnv) bool { return e.intValue("__GNUC__") >= 4 }},
}

var knownElifConditions = map[string]condSpec{
	"defined(_WIN32)": {condWindow, func(e macroEnv) bool { return e.isDefined("_WIN32") }},
}

// frozenIncludes 是每个 checker_input 的冻结 include 拼写集(按序)。
var frozenIncludes = map[string][]string{
	FileAsstCaller:      {`"AsstPort.h"`, "<stdint.h>"},
	FileAsstPort:        {},
	FileAsstCallerExtra: {`"AsstCaller.h"`},
}

// frozenDefines 是 AsstPort.h 锁定宏的名称与允许右值(规范拼写,
// 忽略空白差异);其他宏名或其他右值一律失败。
var frozenDefines = map[string]map[string]bool{
	"ASST_CALL":       newSet("", "__stdcall", "__attribute__((sysv_abi))"),
	"ASST_DLL_IMPORT": newSet("", "__declspec(dllimport)", `__attribute__((visibility("default")))`),
	"ASST_DLL_EXPORT": newSet("", "__declspec(dllexport)", `__attribute__((visibility("default")))`),
	"ASST_DLL_LOCAL":  newSet("", `__attribute__((visibility("hidden")))`),
	"ASSTAPI_PORT":    newSet("ASST_DLL_EXPORT", "ASST_DLL_IMPORT"),
	"ASSTAPI":         newSet("ASSTAPI_PORTASST_CALL"),
	"ASSTLOCAL":       newSet("ASST_DLL_LOCALASST_CALL"),
}

func newSet(values ...string) map[string]bool {
	m := make(map[string]bool, len(values))
	for _, v := range values {
		m[v] = true
	}
	return m
}

type condFrame struct {
	kind   condKind
	parent bool
	taken  bool
	active bool
	inElse bool
}

// cparser 是单个提取 pass 的解析状态:宏表、typedef 表与条件栈都
// 按 pass 隔离(ASST_WITH_EXTRA_CALLERS 0/1 各跑一次)。
type cparser struct {
	input        *CInput
	env          macroEnv
	macros       map[string][]token
	typedefs     map[string]CType
	once         map[string]bool
	stack        []condFrame
	externC      bool
	pending      []token
	pendLine     int
	braceDepth   int
	includeDepth int
	curFile      string
	includes     []string
	result       *passResult
}

func newCParser(input *CInput, env macroEnv) *cparser {
	return &cparser{
		input:    input,
		env:      env,
		macros:   map[string][]token{},
		typedefs: map[string]CType{},
		once:     map[string]bool{},
		result:   &passResult{},
	}
}

func (p *cparser) activeNow() bool {
	for _, f := range p.stack {
		if !f.active {
			return false
		}
	}
	return true
}

// platformLabel 返回最内层平台框架的平台标注。
func (p *cparser) platformLabel() string {
	for i := len(p.stack) - 1; i >= 0; i-- {
		switch p.stack[i].kind {
		case condWindow:
			return PlatformWindows
		case condExtra:
			return PlatformExtra
		}
	}
	return PlatformAll
}

func (p *cparser) diag(line int, symbol, expected, actual, msg string) *Diagnostic {
	return &Diagnostic{Category: CatCSyntax, File: p.curFile, Line: line,
		Symbol: symbol, Expected: expected, Actual: actual, Message: msg}
}

func (p *cparser) isMacroName(s string) bool {
	_, ok := p.macros[s]
	return ok
}

// parseFile 处理单个归档文件:注释剥离、逐行指令/文本分发、EOF 处
// 的条件栈与声明完整性检查、冻结 include 集合核对。
func (p *cparser) parseFile(archivePath string) *Diagnostic {
	p.includeDepth++
	defer func() { p.includeDepth-- }()
	if p.includeDepth > maxIncludeDepth {
		return &Diagnostic{Category: CatCSyntax, File: archivePath, Symbol: "include",
			Expected: "include depth within limit", Actual: "exceeded",
			Message: "include cycle or excessive nesting"}
	}
	if p.once[archivePath] {
		return nil
	}
	p.once[archivePath] = true
	content, ok := p.input.Contents[archivePath]
	if !ok {
		return &Diagnostic{Category: CatInput, File: archivePath, Symbol: "file",
			Expected: "validated checker_input content", Actual: "missing",
			Message: "included file is not part of the validated c input"}
	}
	cleaned, d := stripComments(archivePath, content)
	if d != nil {
		return d
	}
	lines := strings.Split(cleaned, "\n")

	savedFile, savedIncludes := p.curFile, p.includes
	p.curFile = archivePath
	p.includes = nil
	for idx, ln := range lines {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			if d := p.directive(trimmed, idx+1); d != nil {
				return d
			}
			continue
		}
		toks, d := tokenize(archivePath, trimmed, idx+1)
		if d != nil {
			return d
		}
		if len(toks) == 0 {
			continue
		}
		if d := p.feedTokens(toks); d != nil {
			return d
		}
	}
	if len(p.stack) != 0 {
		return p.diag(len(lines), "#if", "balanced conditional block", "unterminated",
			"conditional block is not closed at end of file")
	}
	if len(p.pending) != 0 || p.braceDepth != 0 || p.externC {
		return p.diag(len(lines), "declaration", "complete declaration",
			"unterminated text at end of file",
			"declaration or extern \"C\" wrapper is not closed at end of file")
	}
	if d := p.checkIncludes(); d != nil {
		return d
	}
	p.curFile = savedFile
	p.includes = savedIncludes
	return nil
}

func (p *cparser) checkIncludes() *Diagnostic {
	want := frozenIncludes[p.curFile]
	same := len(p.includes) == len(want)
	if same {
		for i := range want {
			if p.includes[i] != want[i] {
				same = false
				break
			}
		}
	}
	if same {
		return nil
	}
	join := func(ss []string) string {
		if len(ss) == 0 {
			return "(none)"
		}
		return strings.Join(ss, ", ")
	}
	return p.diag(0, "#include", "frozen include set: "+join(want),
		"actual includes: "+join(p.includes),
		"include set of this header does not match the locked profile")
}

func (p *cparser) directive(trimmed string, line int) *Diagnostic {
	toks, d := tokenize(p.curFile, trimmed[1:], line)
	if d != nil {
		return d
	}
	if len(toks) == 0 {
		return p.diag(line, "directive", "supported directive", "empty directive",
			"empty preprocessor directive")
	}
	switch toks[0].text {
	case "pragma":
		if len(toks) == 2 && toks[1].is("once") {
			return nil
		}
		return p.diag(line, "#pragma", "pragma once", tokenCanon(toks), "unsupported pragma")
	case "include":
		// 尖括号拼写(<stdint.h>)在受限词法下是多个 token,include
		// 须在分词前整体截取原样拼写。
		return p.includeDirective(trimmed, line)
	case "ifdef":
		if len(toks) != 2 {
			return p.diag(line, "#ifdef", "single macro name", tokenCanon(toks),
				"malformed #ifdef")
		}
		spec, ok := knownIfdefNames[toks[1].text]
		if !ok {
			return p.unknownCondition(line, "#ifdef "+toks[1].text)
		}
		return p.pushConditional(spec)
	case "ifndef":
		return p.diag(line, "#ifndef", "supported directive", "#ifndef",
			"conditional negation is not part of the locked syntax")
	case "if":
		cond := tokenCanon(toks[1:])
		spec, ok := knownIfConditions[cond]
		if !ok {
			return p.unknownCondition(line, "#if "+cond)
		}
		return p.pushConditional(spec)
	case "elif":
		cond := tokenCanon(toks[1:])
		spec, ok := knownElifConditions[cond]
		if !ok {
			return p.unknownCondition(line, "#elif "+cond)
		}
		return p.elifConditional(line, cond, spec)
	case "else":
		if len(toks) != 1 {
			return p.diag(line, "#else", "bare #else", tokenCanon(toks), "malformed #else")
		}
		return p.elseConditional(line)
	case "endif":
		if len(toks) != 1 {
			return p.diag(line, "#endif", "bare #endif", tokenCanon(toks), "malformed #endif")
		}
		return p.endConditional(line)
	case "define":
		return p.defineDirective(toks, line)
	default:
		return p.diag(line, toks[0].text, "supported directive", "#"+toks[0].text,
			"unsupported preprocessor directive")
	}
}

func (p *cparser) unknownCondition(line int, text string) *Diagnostic {
	return p.diag(line, "condition", "one of the locked conditional expressions", text,
		"conditional expression is not part of the locked profile syntax")
}

func (p *cparser) pushConditional(spec condSpec) *Diagnostic {
	parent := p.activeNow()
	eval := spec.eval(p.env)
	p.stack = append(p.stack, condFrame{kind: spec.kind, parent: parent, taken: eval, active: parent && eval})
	return nil
}

func (p *cparser) elifConditional(line int, cond string, spec condSpec) *Diagnostic {
	if len(p.stack) == 0 {
		return p.diag(line, "#elif", "#elif inside an open #if block", cond,
			"#elif without a matching #if")
	}
	f := &p.stack[len(p.stack)-1]
	if f.inElse {
		return p.diag(line, "#elif", "#elif before #else", cond, "#elif after #else")
	}
	if f.taken {
		f.active = false
		return nil
	}
	eval := spec.eval(p.env)
	f.active = f.parent && eval
	if eval {
		f.taken = true
		f.kind = spec.kind
	}
	return nil
}

func (p *cparser) elseConditional(line int) *Diagnostic {
	if len(p.stack) == 0 {
		return p.diag(line, "#else", "#else inside an open #if block", "#else",
			"#else without a matching #if")
	}
	f := &p.stack[len(p.stack)-1]
	if f.inElse {
		return p.diag(line, "#else", "single #else per block", "#else", "duplicate #else")
	}
	f.inElse = true
	f.active = f.parent && !f.taken
	f.taken = true
	return nil
}

func (p *cparser) endConditional(line int) *Diagnostic {
	if len(p.stack) == 0 {
		return p.diag(line, "#endif", "#endif inside an open #if block", "#endif",
			"#endif without a matching #if")
	}
	p.stack = p.stack[:len(p.stack)-1]
	return nil
}

func (p *cparser) includeDirective(trimmed string, line int) *Diagnostic {
	rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "#"))
	if !strings.HasPrefix(rest, "include") {
		return p.diag(line, "#include", "include directive", trimmed, "malformed #include")
	}
	spelling := strings.TrimSpace(strings.TrimPrefix(rest, "include"))
	inner := ""
	switch {
	case len(spelling) >= 2 && strings.HasPrefix(spelling, `"`) && strings.HasSuffix(spelling, `"`):
		inner = spelling[1 : len(spelling)-1]
	case len(spelling) >= 2 && strings.HasPrefix(spelling, "<") && strings.HasSuffix(spelling, ">"):
		inner = spelling[1 : len(spelling)-1]
	default:
		return p.diag(line, "#include", `quoted or <angled> include spelling`, spelling,
			"malformed #include spelling")
	}
	if inner == "" || strings.ContainsAny(inner, `"<>`) || strings.Contains(inner, " ") {
		return p.diag(line, "#include", "single include spelling", spelling,
			"malformed #include spelling")
	}
	p.includes = append(p.includes, spelling)
	allowed := map[string]bool{}
	for _, s := range frozenIncludes[p.curFile] {
		allowed[s] = true
	}
	if !allowed[spelling] {
		return p.diag(line, "#include", "one of the locked include spellings for this header", spelling,
			"include is not part of the locked include set")
	}
	if !p.activeNow() {
		return nil // 非活跃分支:拼写已校验,不跟随
	}
	if spelling == "<stdint.h>" {
		return nil // 固定标量映射,不读 host 头文件
	}
	target := path.Join(path.Dir(p.curFile), strings.Trim(spelling, `"`))
	return p.parseFile(target)
}

func (p *cparser) defineDirective(toks []token, line int) *Diagnostic {
	if p.curFile != FileAsstPort {
		name := "(unknown)"
		if len(toks) > 1 {
			name = toks[1].text
		}
		return p.diag(line, name, "macro definitions only in "+FileAsstPort,
			"#define in "+p.curFile,
			"macro definitions outside AsstPort.h are rejected; external predefinition of ASST_CALL is not allowed")
	}
	if len(toks) < 2 {
		return p.diag(line, "#define", "macro name", tokenCanon(toks), "malformed #define")
	}
	name := toks[1].text
	allowedVals, ok := frozenDefines[name]
	if !ok {
		return p.diag(line, name, "one of the locked AsstPort.h macros", name,
			"unknown macro name in AsstPort.h")
	}
	vcanon := tokenCanon(toks[2:])
	if !allowedVals[vcanon] {
		return p.diag(line, name, "locked macro right-value spelling", vcanon,
			"macro right-value is not part of the locked set")
	}
	if !p.activeNow() {
		return nil // 非活跃分支:拼写已校验,不进入宏表
	}
	if prev, exists := p.macros[name]; exists {
		if tokenCanon(prev) != vcanon {
			return p.diag(line, name, tokenCanon(prev), vcanon, "conflicting macro redefinition")
		}
		return nil
	}
	p.macros[name] = toks[2:]
	return nil
}

// feedTokens 把一行文本 token 送入声明累积器:顶层分号结束声明项,
// 花括号跟踪块结构,extern "C" { … } 仅识别为链接包装。
func (p *cparser) feedTokens(toks []token) *Diagnostic {
	for _, tok := range toks {
		if d := p.feedToken(tok); d != nil {
			return d
		}
	}
	return nil
}

func (p *cparser) feedToken(tok token) *Diagnostic {
	switch {
	case tok.is("{"):
		if p.braceDepth == 0 && len(p.pending) == 2 &&
			p.pending[0].is("extern") && p.pending[1].is(`"C"`) {
			p.pending = nil
			p.externC = true
			return nil
		}
		p.braceDepth++
		p.appendPending(tok)
		return nil
	case tok.is("}"):
		if p.braceDepth == 0 {
			if len(p.pending) == 0 && p.externC {
				p.externC = false
				return nil
			}
			return p.diag(tok.line, "}", "declaration item or extern \"C\" close", "stray '}'",
				"unexpected '}' outside any block")
		}
		p.braceDepth--
		p.appendPending(tok)
		return nil
	case tok.is(";"):
		if p.braceDepth == 0 {
			if len(p.pending) == 0 {
				return p.diag(tok.line, ";", "declaration item", "stray ';'",
					"unexpected ';' outside any declaration")
			}
			item := p.pending
			line := p.pendLine
			p.pending = nil
			return p.processItem(item, line)
		}
		p.appendPending(tok)
		return nil
	default:
		p.appendPending(tok)
		return nil
	}
}

func (p *cparser) appendPending(tok token) {
	if len(p.pending) == 0 {
		p.pendLine = tok.line
	}
	p.pending = append(p.pending, tok)
}

// splitTopLevel 按分隔符切分 token 序列(受限语法内无嵌套括号)。
func splitTopLevel(toks []token, sep string) [][]token {
	var out [][]token
	cur := []token{}
	for _, t := range toks {
		if t.is(sep) {
			out = append(out, cur)
			cur = []token{}
			continue
		}
		cur = append(cur, t)
	}
	return append(out, cur)
}
