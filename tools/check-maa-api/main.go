// Command check-maa-api 是 maa-go 的最小静态 API checker 命令入口:
// 解析五个必需 flag → profile → 目标 → LoadCInput → ExtractC → Go 文件
// 选择 → Go 提取 → C↔Go 关联 → 锁定 profile 独立策略期望比对 → 宽度
// 级 C↔Go 逐参数签名比对与 callback 平台适配比对。成功退出 0,并在
// stdout 输出键序稳定的 JSON 结果;任何失败都以非零退出码结束,stderr
// 输出 profile、目标与诊断(Diagnostic.String()),绝不输出 PASS。生产
// 入口不提供跳过摘要、未锁定 profile 或其他逃逸参数。

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
)

const usageText = `usage: check-maa-api --profile maa-v6.20.1 --goos linux --goarch amd64 ` +
	`--c-input /path/to/maa-c-input --go-root /path/to/rhodeck

flags:
  --profile   locked profile id (only maa-v6.20.1 is registered)
  --goos      static target GOOS: linux or windows
  --goarch    static target GOARCH: amd64
  --c-input   directory containing inputs.json and its relative-path files
  --go-root   repository root holding packages/maa-go/internal/native
`

func main() {
	os.Exit(Run(os.Args[1:], os.Stdout, os.Stderr))
}

// reportJSON 是成功输出的顶层 JSON 结构:字段顺序即键序,列表一律
// 按名称排序,保证输出确定性。
type reportJSON struct {
	Profile    string          `json:"profile"`
	Target     targetJSON      `json:"target"`
	C          cReport         `json:"c"`
	Go         goReport        `json:"go"`
	Link       linkReport      `json:"link"`
	Expect     expectReport    `json:"expect"`
	Signatures signatureReport `json:"signatures"`
}

type targetJSON struct {
	GOOS   string `json:"goos"`
	GOARCH string `json:"goarch"`
}

type cParamJSON struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type cFuncJSON struct {
	Name   string       `json:"name"`
	Header string       `json:"header"`
	Line   int          `json:"line"`
	Ret    string       `json:"ret"`
	Params []cParamJSON `json:"params"`
}

type cReport struct {
	Full       []string    `json:"full_exports"`
	Applicable []string    `json:"applicable_exports"`
	Discovery  []string    `json:"discovery_exports"`
	Functions  []cFuncJSON `json:"functions"`
}

type goParamJSON struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

type goFieldJSON struct {
	Name   string        `json:"name"`
	File   string        `json:"file"`
	Line   int           `json:"line"`
	Params []goParamJSON `json:"params"`
	Result string        `json:"result"`
}

type goSpecJSON struct {
	Name     string `json:"name"`
	Field    string `json:"field"`
	Policy   string `json:"policy"`
	Platform string `json:"platform"`
}

type goExclJSON struct {
	Name   string `json:"name"`
	Header string `json:"header"`
	Reason string `json:"reason"`
}

type goCallbackJSON struct {
	File   string        `json:"file"`
	Line   int           `json:"line"`
	Params []goParamJSON `json:"params"`
	Result string        `json:"result"`
}

type goReport struct {
	GOOS       string                    `json:"goos"`
	GOARCH     string                    `json:"goarch"`
	Files      []string                  `json:"files"`
	Fields     []goFieldJSON             `json:"api_fields"`
	Specs      []goSpecJSON              `json:"symbol_specs"`
	Exclusions []goExclJSON              `json:"exclusions"`
	Callbacks  map[string]goCallbackJSON `json:"callbacks"`
}

type linkBoundJSON struct {
	Symbol   string `json:"symbol"`
	Field    string `json:"field"`
	Policy   string `json:"policy"`
	Platform string `json:"platform"`
}

type linkReport struct {
	Bound     []linkBoundJSON `json:"bound"`
	NonTarget []string        `json:"nontarget_bindings"`
	Excluded  []string        `json:"excluded"`
}

type expectSideJSON struct {
	Policy   string `json:"policy"`
	Platform string `json:"platform"`
}

type expectEntryJSON struct {
	Symbol   string         `json:"symbol"`
	Expected expectSideJSON `json:"expected"`
	Actual   expectSideJSON `json:"actual"`
}

type expectReport struct {
	Symbols []expectEntryJSON `json:"symbols"`
}

type signatureParamJSON struct {
	Position int    `json:"position"`
	CName    string `json:"c_name"`
	GoName   string `json:"go_name"`
	CType    string `json:"c_type"`
	GoType   string `json:"go_type"`
}

type signatureReturnJSON struct {
	CType  string `json:"c_type"`
	GoType string `json:"go_type"`
}

type signatureSymbolJSON struct {
	Symbol  string               `json:"symbol"`
	Field   string               `json:"field"`
	CHeader string               `json:"c_header"`
	CLine   int                  `json:"c_line"`
	GoFile  string               `json:"go_file"`
	GoLine  int                  `json:"go_line"`
	Params  []signatureParamJSON `json:"params"`
	Fields  []signatureParamJSON `json:"fields,omitempty"`
	Return  signatureReturnJSON  `json:"return"`
}

type signatureCallbackJSON struct {
	Name   string               `json:"name"`
	GoFile string               `json:"go_file"`
	GoLine int                  `json:"go_line"`
	Params []signatureParamJSON `json:"params"`
	Return signatureReturnJSON  `json:"return"`
}

type signatureReport struct {
	Symbols   []signatureSymbolJSON   `json:"symbols"`
	Callbacks []signatureCallbackJSON `json:"callbacks"`
}

// Run 承载全部命令逻辑;返回进程退出码。stdout 只在成功时收到 JSON。
func Run(args []string, stdout, stderr io.Writer) int {
	profileID := ""
	goosArg, goarchArg := "", ""
	cInputArg, goRootArg := "", ""

	fs := flag.NewFlagSet("check-maa-api", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usageText) }
	fs.StringVar(&profileID, "profile", "", "locked profile id (required)")
	fs.StringVar(&goosArg, "goos", "", "static target GOOS (required)")
	fs.StringVar(&goarchArg, "goarch", "", "static target GOARCH (required)")
	fs.StringVar(&cInputArg, "c-input", "", "c input directory (required)")
	fs.StringVar(&goRootArg, "go-root", "", "repository root (required)")

	target := Target{GOOS: goosArg, GOARCH: goarchArg}
	fail := func(diags []Diagnostic) int {
		printFailure(stderr, profileID, target, diags)
		return 1
	}
	if err := fs.Parse(args); err != nil {
		return fail([]Diagnostic{{Category: CatInput, Symbol: "arguments",
			Expected: "the frozen five-flag command line", Actual: err.Error(),
			Message: "argument parsing failed; see usage above"}})
	}
	target = Target{GOOS: goosArg, GOARCH: goarchArg}
	var missing []string
	for name, value := range map[string]string{
		"--profile": profileID, "--goos": goosArg, "--goarch": goarchArg,
		"--c-input": cInputArg, "--go-root": goRootArg,
	} {
		if value == "" {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		return fail([]Diagnostic{{Category: CatInput, Symbol: missing[0],
			Expected: "all five flags provided", Actual: "missing " + strings.Join(missing, ", "),
			Message: "every flag is required"}})
	}
	if fs.NArg() > 0 {
		return fail([]Diagnostic{{Category: CatInput, Symbol: "arguments",
			Expected: "no positional arguments", Actual: strings.Join(fs.Args(), " "),
			Message: "unexpected positional arguments"}})
	}

	profile, ok := LookupProfile(profileID)
	if !ok {
		return fail([]Diagnostic{{Category: CatInput, Symbol: "profile",
			Expected: "registered profile id", Actual: profileID,
			Message: "unknown profile; the production registry contains only the locked profile"}})
	}
	if diags := ValidateTarget(target); len(diags) > 0 {
		return fail(diags)
	}

	goRoot, d := goRootAbs(goRootArg)
	if d != nil {
		return fail([]Diagnostic{*d})
	}

	input, diags := LoadCInput(cInputArg, profile)
	if len(diags) > 0 {
		return fail(diags)
	}
	cext, diags := ExtractC(input, target)
	if len(diags) > 0 {
		return fail(diags)
	}
	sel, diags := SelectGoFiles(goRoot, target)
	if len(diags) > 0 {
		return fail(diags)
	}
	gext, diags := ExtractGo(sel)
	if len(diags) > 0 {
		return fail(diags)
	}
	link, diags := LinkCGo(cext, gext, target)
	if len(diags) > 0 {
		return fail(diags)
	}
	expect, diags := CompareExpectations(gext)
	if len(diags) > 0 {
		return fail(diags)
	}
	signatures, diags := CompareSignatures(cext, gext, link)
	if len(diags) > 0 {
		return fail(diags)
	}

	report := buildReport(profileID, target, cext, gext, sel, link, expect, signatures)
	enc, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fail([]Diagnostic{{Category: CatInput, Symbol: "report",
			Expected: "serializable report", Actual: err.Error(),
			Message: "internal error encoding the success report"}})
	}
	fmt.Fprintf(stdout, "%s\n", enc)
	return 0
}

// printFailure 输出 profile/目标头信息与诊断;失败路径绝不输出 PASS。
func printFailure(stderr io.Writer, profileID string, target Target, diags []Diagnostic) {
	if profileID != "" {
		fmt.Fprintf(stderr, "check-maa-api: profile: %s\n", profileID)
	}
	if target.GOOS != "" || target.GOARCH != "" {
		fmt.Fprintf(stderr, "check-maa-api: target: %s\n", fmtTarget(target))
	}
	for _, d := range diags {
		fmt.Fprintln(stderr, d.String())
	}
}

// buildReport 组装确定性成功输出:所有列表按名称排序;空列表输出
// 为 [],不输出 null。expect 段给出冻结期望表的逐符号 policy/platform
// 期望与实际对照(30 项,按符号名排序,与目标无关);signatures 段
// 给出宽度级逐参数核验记录(全部绑定符号——目标适用绑定与非目标平
// 台的 windowsOnly 绑定,两目标均为 26 项——按符号名排序,两个
// callback 固定顺序),含每个参数位置的 C/Go 名称与类型、返回映射与
// 双方声明位置。
func buildReport(profileID string, target Target, cext *CExtraction, gext *GoExtraction,
	sel *GoSelection, link *LinkResult, expect *ExpectResult, signatures *SignatureResult) reportJSON {
	report := reportJSON{
		Profile: profileID,
		Target:  targetJSON{GOOS: target.GOOS, GOARCH: target.GOARCH},
	}
	report.C = cReport{
		Full:       sortedFunctionNames(cext.Functions),
		Applicable: sortedFunctionNames(cext.Applicable),
		Discovery:  sortedFunctionNames(cext.Discovery),
		Functions:  cFunctionReports(cext.Functions),
	}

	report.Go = goReport{
		GOOS:       target.GOOS,
		GOARCH:     target.GOARCH,
		Files:      sel.names(),
		Fields:     []goFieldJSON{},
		Specs:      []goSpecJSON{},
		Exclusions: []goExclJSON{},
		Callbacks:  map[string]goCallbackJSON{},
	}
	for _, f := range gext.Fields {
		report.Go.Fields = append(report.Go.Fields, goFieldJSON{Name: f.Name, File: f.File,
			Line: f.Line, Params: goParamReports(f.Params), Result: string(f.Result)})
	}
	for _, s := range gext.Specs {
		report.Go.Specs = append(report.Go.Specs, goSpecJSON{Name: s.Name, Field: s.Field,
			Policy: s.Policy, Platform: s.Platform})
	}
	for _, e := range gext.Exclusions {
		report.Go.Exclusions = append(report.Go.Exclusions, goExclJSON{Name: e.Name,
			Header: e.Header, Reason: e.Reason})
	}
	for _, cb := range []*GoCallback{gext.UnixCallback, gext.WindowsCallback} {
		if cb == nil {
			continue
		}
		report.Go.Callbacks[cb.Name] = goCallbackJSON{File: cb.File, Line: cb.Line,
			Params: goParamReports(cb.Params), Result: string(cb.Result)}
	}

	report.Link = linkReport{Bound: []linkBoundJSON{},
		NonTarget: []string{}, Excluded: []string{}}
	for _, b := range link.Bound {
		report.Link.Bound = append(report.Link.Bound, linkBoundJSON{Symbol: b.Symbol,
			Field: b.Field, Policy: b.Policy, Platform: b.Platform})
	}
	report.Link.NonTarget = append(report.Link.NonTarget, link.NonTarget...)
	report.Link.Excluded = append(report.Link.Excluded, link.Excluded...)

	report.Expect = expectReport{Symbols: []expectEntryJSON{}}
	for _, e := range expect.Entries {
		report.Expect.Symbols = append(report.Expect.Symbols, expectEntryJSON{Symbol: e.Symbol,
			Expected: expectSideJSON{Policy: e.Expected.Policy, Platform: e.Expected.Platform},
			Actual:   expectSideJSON{Policy: e.Actual.Policy, Platform: e.Actual.Platform}})
	}

	report.Signatures = signatureReport{Symbols: []signatureSymbolJSON{}, Callbacks: []signatureCallbackJSON{}}
	for _, e := range signatures.Symbols {
		entry := signatureSymbolJSON{
			Symbol: e.Symbol, Field: e.Field, CHeader: e.Header, CLine: e.CLine,
			GoFile: e.GoFile, GoLine: e.GoLine,
			Params: signatureParamReports(e.Params),
			Return: signatureReturnJSON{CType: e.Return.CType, GoType: e.Return.GoType}}
		if len(e.Fields) > 0 {
			entry.Fields = make([]signatureParamJSON, 0, len(e.Fields))
			for _, f := range e.Fields {
				entry.Fields = append(entry.Fields, signatureParamJSON{Position: f.Position,
					CName: f.CName, GoName: f.GoName, CType: f.CType, GoType: f.GoType})
			}
		}
		report.Signatures.Symbols = append(report.Signatures.Symbols, entry)
	}
	for _, cb := range signatures.Callbacks {
		report.Signatures.Callbacks = append(report.Signatures.Callbacks, signatureCallbackJSON{
			Name: cb.Name, GoFile: cb.GoFile, GoLine: cb.GoLine,
			Params: signatureParamReports(cb.Params),
			Return: signatureReturnJSON{CType: cb.Return.CType, GoType: cb.Return.GoType}})
	}
	return report
}

func signatureParamReports(params []SignatureParam) []signatureParamJSON {
	out := make([]signatureParamJSON, 0, len(params))
	for _, p := range params {
		out = append(out, signatureParamJSON{Position: p.Position, CName: p.CName, GoName: p.GoName,
			CType: p.CType, GoType: p.GoType})
	}
	return out
}

func sortedFunctionNames(fns []CFunction) []string {
	out := make([]string, 0, len(fns))
	for i := range fns {
		out = append(out, fns[i].Name)
	}
	sort.Strings(out)
	return out
}

func cFunctionReports(fns []CFunction) []cFuncJSON {
	sorted := append([]CFunction(nil), fns...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	out := make([]cFuncJSON, 0, len(sorted))
	for i := range sorted {
		fn := sorted[i]
		params := make([]cParamJSON, 0, len(fn.Params))
		for _, p := range fn.Params {
			params = append(params, cParamJSON{Name: p.Name, Type: p.Type.String()})
		}
		out = append(out, cFuncJSON{Name: fn.Name, Header: fn.Header, Line: fn.Line,
			Ret: fn.Ret.String(), Params: params})
	}
	return out
}

func goParamReports(params []GoParam) []goParamJSON {
	out := make([]goParamJSON, 0, len(params))
	for _, p := range params {
		out = append(out, goParamJSON{Name: p.Name, Type: string(p.Type)})
	}
	return out
}
