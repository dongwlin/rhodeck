package main

import (
	"fmt"
	"sort"
	"strings"
)

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

var approvedStructFieldNames = map[string]string{
	"stage_id": "StageID",
	"code":     "Code",
	"level_id": "LevelID",
	"name":     "Name",
}

func approvedParamName(cName string) string {
	if goName, ok := approvedParamNames[cName]; ok {
		return goName
	}
	return cName
}

var (
	voidPtrWindowSymbols = newSet("AsstAsyncAttachWindow")
	voidPtrImageSymbols  = newSet("AsstGetImage", "AsstGetImageBgr")
)

// 冻结输入中唯一的非 const char* 是 AsstGetUUID 的 buff
// (testdata/snapshot/upstream/AsstCaller.h),其余 char pointee 均为 const。
var frozenNonConstCharPointees = map[string]map[string]bool{
	"AsstGetUUID": newSet("buff"),
}

func permittedGoTypes(symbol, paramName string, ct CType) []GoSourceType {
	switch ct.Kind {
	case CKindScalar:
		if ct.Const {
			return nil
		}
		switch ct.Name {
		case "uint8":
			return []GoSourceType{"uint8", "byte"}
		case "int32":
			return []GoSourceType{"int32"}
		case "uint64":
			return []GoSourceType{"uint64"}
		}
		return nil
	case CKindPointer:
		pt := ct.Pointee
		if pt == nil {
			return nil
		}
		switch pt.Kind {
		case CKindScalar:
			switch pt.Name {
			case "char":
				if pt.Const == frozenNonConstCharPointees[symbol][paramName] {
					return nil
				}
				return []GoSourceType{"*byte"}
			case "int32":
				return []GoSourceType{"*int32"}
			}
			return nil
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
				return []GoSourceType{"uintptr"}
			}
			return nil
		}
		return nil
	case CKindFuncPtr:
		return []GoSourceType{"uintptr"}
	case CKindStruct:
		if ct.Tag == frozenStructTag && paramName == "" {
			return []GoSourceType{GoTypeMapLevelKey}
		}
		return nil
	}
	return nil
}

func containsGoType(list []GoSourceType, t GoSourceType) bool {
	for _, v := range list {
		if v == t {
			return true
		}
	}
	return false
}

func joinGoTypes(list []GoSourceType) string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, string(v))
	}
	return strings.Join(out, " or ")
}

func charPointeeConstDiag(symbol, paramName string, position int, ct CType, cLoc sigLocation) *Diagnostic {
	if ct.Kind != CKindPointer || ct.Pointee == nil ||
		ct.Pointee.Kind != CKindScalar || ct.Pointee.Name != "char" {
		return nil
	}
	nonConstOK := frozenNonConstCharPointees[symbol][paramName]
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

type sigLocation struct {
	file string
	line int
}

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

type SignatureParam struct {
	Position int
	CName    string
	GoName   string
	CType    string
	GoType   string
}

type SignatureReturn struct {
	CType  string
	GoType string
}

type SignatureField struct {
	Position int
	CName    string
	GoName   string
	CType    string
	GoType   string
}

type SignatureEntry struct {
	Symbol string
	Field  string
	Header string
	CLine  int
	GoFile string
	GoLine int
	Params []SignatureParam
	Fields []SignatureField
	Return SignatureReturn
}

type SignatureCallbackEntry struct {
	Name   string
	GoFile string
	GoLine int
	Params []SignatureParam
	Return SignatureReturn
}

type SignatureResult struct {
	Symbols   []SignatureEntry
	Callbacks []SignatureCallbackEntry
}

func compareStructFields(symbol string, def CStruct, gs *GoStruct, cLoc, goLoc sigLocation) ([]SignatureField, *Diagnostic) {
	if len(def.Members) != len(gs.Fields) {
		return nil, &Diagnostic{Category: CatSignature, File: goLoc.file, Line: goLoc.line, Symbol: gs.Name,
			Expected: fmt.Sprintf("%d fields matching the frozen struct members", len(def.Members)),
			Actual:   fmt.Sprintf("%d fields", len(gs.Fields)),
			Message:  "struct isomorphism field count differs from the c definition"}
	}
	out := make([]SignatureField, 0, len(def.Members))
	for i := range def.Members {
		cm, gm := def.Members[i], gs.Fields[i]
		wantName := approvedStructFieldNames[cm.Name]
		if wantName == "" {
			wantName = cm.Name
		}
		if gm.Name != wantName {
			return nil, &Diagnostic{Category: CatSignature, File: goLoc.file, Line: goLoc.line, Symbol: gs.Name,
				Expected: fmt.Sprintf("field %d name %s", i+1, wantName),
				Actual:   fmt.Sprintf("field %d name %s", i+1, gm.Name),
				Message: fmt.Sprintf("c member %q: the go field name must equal the member name after the approved mapping; "+
					"unapproved renames and reordering fail", cm.Name)}
		}
		permitted := permittedStructMemberTypes(cm.Type)
		if !containsGoType(permitted, gm.Type) {
			return nil, &Diagnostic{Category: CatSignature, File: goLoc.file, Line: goLoc.line, Symbol: gs.Name,
				Expected: joinGoTypes(permitted),
				Actual:   string(gm.Type),
				Message: fmt.Sprintf("field %d (%s): the go raw type must match the frozen c member mapping",
					i+1, cm.Name)}
		}
		out = append(out, SignatureField{Position: i + 1, CName: cm.Name, GoName: gm.Name,
			CType: cm.Type.String(), GoType: string(gm.Type)})
	}
	return out, nil
}

func permittedStructMemberTypes(ct CType) []GoSourceType {
	if ct.Kind == CKindPointer && ct.Pointee != nil &&
		ct.Pointee.Kind == CKindScalar && ct.Pointee.Name == "char" && ct.Pointee.Const {
		return []GoSourceType{"*byte"}
	}
	return nil
}

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
	structByTag := map[string]CStruct{}
	for i := range c.Structs {
		structByTag[c.Structs[i].Tag] = c.Structs[i]
	}
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
		if cf.Ret.Kind == CKindStruct && cf.Ret.Tag == frozenStructTag {
			def, ok := structByTag[frozenStructTag]
			if !ok {
				return &Diagnostic{Category: CatSignature, File: cLoc.file, Line: cLoc.line, Symbol: symbol,
					Expected: "the frozen struct definition", Actual: "missing",
					Message: "signature comparison requires the c struct definition of a struct return"}
			}
			if g.MapLevelKey == nil {
				return &Diagnostic{Category: CatSignature, File: goLoc.file, Line: goLoc.line, Symbol: symbol,
					Expected: "type mapLevelKey (the go struct isomorphism)", Actual: "missing",
					Message: "signature comparison requires the go struct isomorphism of a struct return"}
			}
			fields, d := compareStructFields(symbol, def, g.MapLevelKey,
				sigLocation{file: def.Header, line: def.Line},
				sigLocation{file: g.MapLevelKey.File, line: g.MapLevelKey.Line})
			if d != nil {
				return d
			}
			entry.Fields = fields
		}
		res.Symbols = append(res.Symbols, entry)
		return nil
	}
	for _, b := range link.Bound {
		if d := compareEntry(b.Symbol, b.Field); d != nil {
			return fail(*d)
		}
	}
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
	sort.Slice(res.Symbols, func(i, j int) bool { return res.Symbols[i].Symbol < res.Symbols[j].Symbol })

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
