package main

import (
	"go/ast"
	"go/parser"
	"go/scanner"
	gotoken "go/token"
	"path/filepath"
	"strconv"
	"strings"
)

type GoSourceType string

const GoTypeUnsafePointer GoSourceType = "unsafe.Pointer"

const GoTypeMapLevelKey GoSourceType = "mapLevelKey"

var goRawScalarTypes = map[string]bool{
	"uint8": true, "int32": true, "uint64": true, "uintptr": true, "byte": true,
}

var goShadowNames = map[string]bool{
	"uint8": true, "int32": true, "uint64": true, "uintptr": true, "byte": true,
	"any": true, "iota": true, "unsafe": true,
}

const symbolSpecsParamName = "table"

type GoParam struct {
	Name string
	Type GoSourceType
}

type GoField struct {
	Name   string
	File   string
	Line   int
	Params []GoParam
	Result GoSourceType
}

type GoSymbolSpec struct {
	Name     string
	Field    string
	Policy   string
	Platform string
	File     string
	Line     int
}

type GoExclusion struct {
	Name   string
	Header string
	Reason string
	File   string
	Line   int
}

type GoCallback struct {
	Name   string
	File   string
	Line   int
	Params []GoParam
	Result GoSourceType
}

type GoStruct struct {
	Name   string
	File   string
	Line   int
	Fields []GoParam
}

type GoExtraction struct {
	Files           []GoSourceFile
	PackageName     string
	Fields          []GoField
	Specs           []GoSymbolSpec
	Exclusions      []GoExclusion
	UnixCallback    *GoCallback
	WindowsCallback *GoCallback
	MapLevelKey     *GoStruct
}

func ExtractGo(sel *GoSelection) (*GoExtraction, []Diagnostic) {
	fail := func(d Diagnostic) (*GoExtraction, []Diagnostic) { return nil, []Diagnostic{d} }
	if sel == nil || len(sel.Files) == 0 {
		return fail(Diagnostic{Category: CatInput, Symbol: "go files",
			Expected: "selected go files", Actual: "none",
			Message: "go extraction requires a validated file selection"})
	}

	fset := gotoken.NewFileSet()
	type parsedFile struct {
		rel  string
		file *ast.File
	}
	var parsed []parsedFile
	pkgName := ""
	for _, sf := range sel.Files {
		astFile, err := parser.ParseFile(fset, sf.Path, nil, parser.SkipObjectResolution)
		if err != nil {
			line := firstParseErrorLine(err)
			return fail(Diagnostic{Category: CatGoSyntax, File: sf.Name, Line: line, Symbol: sf.Name,
				Expected: "parseable go source", Actual: err.Error(),
				Message: "selected go file does not parse"})
		}
		rel := sf.Name
		if r, err := filepath.Rel(sel.Dir, sf.Path); err == nil {
			rel = filepath.ToSlash(r)
		}
		if pkgName == "" {
			pkgName = astFile.Name.Name
		} else if astFile.Name.Name != pkgName {
			return fail(Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(astFile.Name.Pos()).Line,
				Symbol: "package", Expected: pkgName, Actual: astFile.Name.Name,
				Message: "selected go files disagree on the package name"})
		}
		parsed = append(parsed, parsedFile{rel: rel, file: astFile})
	}

	ext := &GoExtraction{Files: append([]GoSourceFile(nil), sel.Files...), PackageName: pkgName}
	seenAPI, seenSpecs, seenExclusions := 0, 0, 0
	seenUnixCb, seenWindowsCb, seenMapLevelKey := 0, 0, 0
	consts := map[string]constDecl{}

	for _, pf := range parsed {
		if d := checkFileImports(pf.rel, pf.file, fset); d != nil {
			return fail(*d)
		}
		collectConsts(fset, pf.file, consts)
		for _, decl := range pf.file.Decls {
			switch gd := decl.(type) {
			case *ast.GenDecl:
				switch gd.Tok {
				case gotoken.TYPE:
					for _, spec := range gd.Specs {
						ts := spec.(*ast.TypeSpec)
						if d := checkShadowName(fset, pf.rel, ts.Pos(), ts.Name.Name); d != nil {
							return fail(*d)
						}
						switch ts.Name.Name {
						case "unixCallback":
							seenUnixCb++
							if seenUnixCb > 1 {
								return fail(duplicateDeclDiag(fset, pf.rel, ts.Pos(), "unixCallback"))
							}
							cb, d := extractCallback(fset, pf.rel, ts, false)
							if d != nil {
								return fail(*d)
							}
							ext.UnixCallback = cb
						case "windowsCallback":
							seenWindowsCb++
							if seenWindowsCb > 1 {
								return fail(duplicateDeclDiag(fset, pf.rel, ts.Pos(), "windowsCallback"))
							}
							cb, d := extractCallback(fset, pf.rel, ts, true)
							if d != nil {
								return fail(*d)
							}
							ext.WindowsCallback = cb
						case "mapLevelKey":
							seenMapLevelKey++
							if seenMapLevelKey > 1 {
								return fail(duplicateDeclDiag(fset, pf.rel, ts.Pos(), "mapLevelKey"))
							}
							st, d := extractMapLevelKeyStruct(fset, pf.rel, ts)
							if d != nil {
								return fail(*d)
							}
							ext.MapLevelKey = st
						case "api":
							if ts.TypeParams != nil {
								return fail(Diagnostic{Category: CatGoSyntax, File: pf.rel,
									Line: fset.Position(ts.Pos()).Line, Symbol: "api",
									Expected: "non-generic type", Actual: "type parameters",
									Message: "api struct must not be generic"})
							}
							st, ok := unwrapParen(ts.Type).(*ast.StructType)
							if !ok {
								return fail(Diagnostic{Category: CatGoSyntax, File: pf.rel,
									Line: fset.Position(ts.Pos()).Line, Symbol: "api",
									Expected: "type api struct", Actual: typeNameForDiag(ts.Type),
									Message: "api must be declared as a struct"})
							}
							seenAPI++
							if seenAPI > 1 {
								return fail(duplicateDeclDiag(fset, pf.rel, ts.Pos(), "type api struct"))
							}
							fields, d := extractAPIFields(fset, pf.rel, st)
							if d != nil {
								return fail(*d)
							}
							ext.Fields = fields
						}
					}
				case gotoken.VAR:
					for _, spec := range gd.Specs {
						vs := spec.(*ast.ValueSpec)
						for _, name := range vs.Names {
							if d := checkShadowName(fset, pf.rel, vs.Pos(), name.Name); d != nil {
								return fail(*d)
							}
						}
						if len(vs.Names) == 1 && vs.Names[0].Name == "exclusions" {
							seenExclusions++
							if seenExclusions > 1 {
								return fail(duplicateDeclDiag(fset, pf.rel, vs.Pos(), "exclusions"))
							}
							excls, d := extractExclusions(fset, pf.rel, vs)
							if d != nil {
								return fail(*d)
							}
							ext.Exclusions = excls
						}
					}
				}
			case *ast.FuncDecl:
				if gd.Recv != nil {
					continue
				}
				if d := checkShadowName(fset, pf.rel, gd.Pos(), gd.Name.Name); d != nil {
					return fail(*d)
				}
				if gd.Name.Name == "symbolSpecs" {
					seenSpecs++
					if seenSpecs > 1 {
						return fail(duplicateDeclDiag(fset, pf.rel, gd.Pos(), "symbolSpecs"))
					}
					specs, d := extractSymbolSpecs(fset, pf.rel, gd, consts)
					if d != nil {
						return fail(*d)
					}
					ext.Specs = specs
				}
			}
		}
	}

	switch {
	case seenAPI == 0:
		return fail(Diagnostic{Category: CatGoSyntax, File: "api.go", Symbol: "api",
			Expected: "type api struct", Actual: "missing",
			Message: "api struct declaration is missing (empty symbol table source)"})
	case seenSpecs == 0:
		return fail(Diagnostic{Category: CatGoSyntax, File: "symbols.go", Symbol: "symbolSpecs",
			Expected: "symbolSpecs declaration", Actual: "missing",
			Message: "symbolSpecs declaration is missing (empty symbol list)"})
	case seenExclusions == 0:
		return fail(Diagnostic{Category: CatGoSyntax, File: "symbols.go", Symbol: "exclusions",
			Expected: "exclusions declaration", Actual: "missing",
			Message: "exclusions declaration is missing (empty exclusion list)"})
	case seenUnixCb == 0:
		return fail(Diagnostic{Category: CatGoSyntax, File: "callback_types.go", Symbol: "unixCallback",
			Expected: "type unixCallback", Actual: "missing",
			Message: "unixCallback declaration is missing"})
	case seenWindowsCb == 0:
		return fail(Diagnostic{Category: CatGoSyntax, File: "callback_types.go", Symbol: "windowsCallback",
			Expected: "type windowsCallback", Actual: "missing",
			Message: "windowsCallback declaration is missing"})
	case seenMapLevelKey == 0:
		return fail(Diagnostic{Category: CatGoSyntax, File: "api.go", Symbol: "mapLevelKey",
			Expected: "type mapLevelKey", Actual: "missing",
			Message: "mapLevelKey struct declaration is missing (AsstGetMapLevelKey return isomorphism)"})
	}
	return ext, nil
}

func firstParseErrorLine(err error) int {
	if el, ok := err.(scanner.ErrorList); ok && len(el) > 0 {
		return el[0].Pos.Line
	}
	return 0
}

func duplicateDeclDiag(fset *gotoken.FileSet, rel string, pos gotoken.Pos, name string) Diagnostic {
	return Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(pos).Line, Symbol: name,
		Expected: "unique package-level declaration", Actual: "duplicate",
		Message: "declaration is duplicated across the selected files"}
}

func checkShadowName(fset *gotoken.FileSet, rel string, pos gotoken.Pos, name string) *Diagnostic {
	if !goShadowNames[name] {
		return nil
	}
	return &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(pos).Line, Symbol: name,
		Expected: "package-level name outside the raw vocabulary shadow set", Actual: name,
		Message: "package-level declaration shadows a raw type name (uint8/int32/uint64/uintptr/byte/any/iota/unsafe)"}
}

func checkFileImports(rel string, f *ast.File, fset *gotoken.FileSet) *Diagnostic {
	importsUnsafe := false
	for _, imp := range f.Imports {
		path, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		if imp.Name != nil {
			if goShadowNames[imp.Name.Name] {
				return &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(imp.Pos()).Line,
					Symbol:   imp.Name.Name,
					Expected: "import name outside the raw vocabulary shadow set", Actual: imp.Name.Name,
					Message: "aliased import shadows a raw type name (uint8/int32/uint64/uintptr/byte/any/iota/unsafe)"}
			}
		} else if path != "unsafe" && goShadowNames[importPathBase(path)] {
			return &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(imp.Pos()).Line,
				Symbol:   importPathBase(path),
				Expected: "import name outside the raw vocabulary shadow set", Actual: importPathBase(path),
				Message: "import shadows a raw type name (uint8/int32/uint64/uintptr/byte/any/iota/unsafe)"}
		}
		if path == "unsafe" && imp.Name != nil {
			return &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(imp.Pos()).Line,
				Symbol: "unsafe", Expected: `exact import "unsafe"`,
				Actual:  "aliased import of unsafe",
				Message: "unsafe must be imported without alias/blank/dot name; a faked unsafe import is rejected"}
		}
		if path == "unsafe" {
			importsUnsafe = true
		}
	}
	usesUnsafe := false
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "unsafe" && sel.Sel.Name == "Pointer" {
			usesUnsafe = true
		}
		return true
	})
	if usesUnsafe && !importsUnsafe {
		return &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(f.Pos()).Line, Symbol: "unsafe",
			Expected: `exact import "unsafe"`, Actual: "missing",
			Message: "file uses unsafe.Pointer without importing unsafe exactly"}
	}
	return nil
}

func importPathBase(path string) string {
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		path = path[i+1:]
	}
	return path
}

type constDecl struct {
	typeName string
	file     string
	line     int
}

func collectConsts(fset *gotoken.FileSet, f *ast.File, out map[string]constDecl) {
	for _, decl := range f.Decls {
		gd, ok := decl.(*ast.GenDecl)
		if !ok || gd.Tok != gotoken.CONST {
			continue
		}
		prev := ""
		for _, spec := range gd.Specs {
			vs := spec.(*ast.ValueSpec)
			tname := ""
			switch {
			case vs.Type != nil:
				if id, ok := unwrapParen(vs.Type).(*ast.Ident); ok {
					tname = id.Name
				}
				prev = tname
			case len(vs.Values) == 0:
				tname = prev
			default:
				prev = ""
			}
			for _, name := range vs.Names {
				if _, dup := out[name.Name]; !dup {
					out[name.Name] = constDecl{typeName: tname, file: f.Name.Name,
						line: fset.Position(vs.Pos()).Line}
				}
			}
		}
	}
}

func extractAPIFields(fset *gotoken.FileSet, rel string, st *ast.StructType) ([]GoField, *Diagnostic) {
	if st.Fields == nil || len(st.Fields.List) == 0 {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(st.Pos()).Line,
			Symbol: "api", Expected: "at least one raw func field", Actual: "empty struct",
			Message: "api struct must declare the R/O raw fields"}
	}
	var fields []GoField
	for _, fl := range st.Fields.List {
		pos := fset.Position(fl.Pos())
		if len(fl.Names) != 1 {
			kind := "embedded field"
			if len(fl.Names) > 1 {
				kind = "grouped field names"
			}
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line,
				Symbol:   "api field",
				Expected: "exactly one explicit field name per raw field", Actual: kind,
				Message: "raw api fields must not be embedded or grouped"}
		}
		name := fl.Names[0].Name
		ft, ok := unwrapParen(fl.Type).(*ast.FuncType)
		if !ok {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: name,
				Expected: "raw field type must be exactly one func type", Actual: typeNameForDiag(fl.Type),
				Message: "api fields must each declare a single func type"}
		}
		params, results, d := extractSignature(fset, rel, name, ft)
		if d != nil {
			return nil, d
		}
		fields = append(fields, GoField{Name: name, File: rel, Line: pos.Line,
			Params: params, Result: results})
	}
	return fields, nil
}

func extractSignature(fset *gotoken.FileSet, rel, owner string, ft *ast.FuncType) ([]GoParam, GoSourceType, *Diagnostic) {
	params, d := extractParams(fset, rel, owner, ft.Params)
	if d != nil {
		return nil, "", d
	}
	result, d := extractResults(fset, rel, owner, ft.Results)
	if d != nil {
		return nil, "", d
	}
	return params, result, nil
}

func extractParams(fset *gotoken.FileSet, rel, owner string, fl *ast.FieldList) ([]GoParam, *Diagnostic) {
	if fl == nil || len(fl.List) == 0 {
		return nil, nil
	}
	var params []GoParam
	for _, pf := range fl.List {
		pos := fset.Position(pf.Pos())
		if len(pf.Names) != 1 {
			kind := "anonymous parameter"
			if len(pf.Names) > 1 {
				kind = "grouped parameter names"
			}
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: owner,
				Expected: "exactly one explicit name per parameter", Actual: kind,
				Message: "raw signatures must not use anonymous or grouped parameters"}
		}
		typ, d := goTypeOfExpr(fset, rel, owner, pf.Type)
		if d != nil {
			return nil, d
		}
		params = append(params, GoParam{Name: pf.Names[0].Name, Type: typ})
	}
	return params, nil
}

func extractResults(fset *gotoken.FileSet, rel, owner string, fl *ast.FieldList) (GoSourceType, *Diagnostic) {
	if fl == nil || len(fl.List) == 0 {
		return "", nil
	}
	if len(fl.List) != 1 {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(fl.Pos()).Line,
			Symbol: owner, Expected: "no result or exactly one unnamed result",
			Actual:  "multiple results",
			Message: "raw signatures must not declare multiple return values"}
	}
	rf := fl.List[0]
	if len(rf.Names) != 0 {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(rf.Pos()).Line,
			Symbol: owner, Expected: "unnamed result", Actual: "named result",
			Message: "raw signatures must use a single unnamed result"}
	}
	typ, d := goTypeOfExpr(fset, rel, owner, rf.Type)
	if d != nil {
		return "", d
	}
	return typ, nil
}

func goTypeOfExpr(fset *gotoken.FileSet, rel, owner string, e ast.Expr) (GoSourceType, *Diagnostic) {
	pos := fset.Position(e.Pos())
	if _, variadic := e.(*ast.Ellipsis); variadic {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: owner,
			Expected: "fixed parameter", Actual: "variadic ...",
			Message: "raw signatures must not use variadic parameters"}
	}
	switch t := unwrapParen(e).(type) {
	case *ast.Ident:
		if goRawScalarTypes[t.Name] {
			return GoSourceType(t.Name), nil
		}
		if t.Name == string(GoTypeMapLevelKey) {
			return GoTypeMapLevelKey, nil
		}
	case *ast.SelectorExpr:
		if x, ok := t.X.(*ast.Ident); ok && x.Name == "unsafe" && t.Sel.Name == "Pointer" {
			return GoTypeUnsafePointer, nil
		}
	case *ast.StarExpr:
		if x, ok := unwrapParen(t.X).(*ast.Ident); ok && (x.Name == "byte" || x.Name == "int32") {
			return GoSourceType("*" + x.Name), nil
		}
	}
	return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: owner,
		Expected: "one of uint8/int32/uint64/uintptr/byte, unsafe.Pointer, *byte, *int32 or mapLevelKey",
		Actual:   typeNameForDiag(e),
		Message:  "raw declaration uses a type outside the frozen vocabulary"}
}

func extractSymbolSpecs(fset *gotoken.FileSet, rel string, fn *ast.FuncDecl, consts map[string]constDecl) ([]GoSymbolSpec, *Diagnostic) {
	line := fset.Position(fn.Pos()).Line
	formDiag := func(expected, actual, msg string) *Diagnostic {
		return &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: "symbolSpecs",
			Expected: expected, Actual: actual, Message: msg}
	}
	if fn.Type.TypeParams != nil {
		return nil, formDiag("non-generic function", "type parameters",
			"symbolSpecs must not be generic")
	}
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 1 ||
		len(fn.Type.Params.List[0].Names) != 1 {
		return nil, formDiag("exactly one named *api parameter", "unexpected parameter list",
			"symbolSpecs must take exactly one named *api parameter")
	}
	p := fn.Type.Params.List[0]
	if p.Names[0].Name != symbolSpecsParamName {
		return nil, formDiag("parameter named "+symbolSpecsParamName, p.Names[0].Name,
			"symbolSpecs parameter name is frozen by the shared declaration file")
	}
	star, ok := unwrapParen(p.Type).(*ast.StarExpr)
	if !ok {
		return nil, formDiag("*api parameter", typeNameForDiag(p.Type),
			"symbolSpecs must take a *api parameter")
	}
	if id, ok := unwrapParen(star.X).(*ast.Ident); !ok || id.Name != "api" {
		return nil, formDiag("*api parameter", typeNameForDiag(p.Type),
			"symbolSpecs must take a *api parameter")
	}
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 ||
		len(fn.Type.Results.List[0].Names) != 0 {
		return nil, formDiag("single unnamed []symbolSpec result", "unexpected result list",
			"symbolSpecs must return exactly one unnamed []symbolSpec")
	}
	if !isSliceOfIdent(fn.Type.Results.List[0].Type, "symbolSpec") {
		return nil, formDiag("[]symbolSpec result", typeNameForDiag(fn.Type.Results.List[0].Type),
			"symbolSpecs must return []symbolSpec")
	}

	bodyMsg := "symbolSpecs must contain exactly one return statement; helpers, loops and conditions are rejected"
	if fn.Body == nil || len(fn.Body.List) != 1 {
		return nil, formDiag("single return statement of a []symbolSpec literal",
			"other statements or helpers", bodyMsg)
	}
	ret, ok := fn.Body.List[0].(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 1 {
		return nil, formDiag("single return statement of a []symbolSpec literal",
			"non-return statement", bodyMsg)
	}
	lit, ok := unwrapParen(ret.Results[0]).(*ast.CompositeLit)
	if !ok || !isSliceOfIdent(lit.Type, "symbolSpec") {
		return nil, formDiag("return []symbolSpec{...} composite literal",
			typeNameForDiag(ret.Results[0]),
			"symbolSpecs must return a direct []symbolSpec composite literal; computation and indirection are rejected")
	}
	if len(lit.Elts) == 0 {
		return nil, formDiag("non-empty symbol list", "empty literal",
			"empty symbol list is rejected")
	}
	var specs []GoSymbolSpec
	for _, elt := range lit.Elts {
		spec, d := extractSymbolSpecEntry(fset, rel, elt, consts)
		if d != nil {
			return nil, d
		}
		specs = append(specs, *spec)
	}
	return specs, nil
}

func extractSymbolSpecEntry(fset *gotoken.FileSet, rel string, elt ast.Expr, consts map[string]constDecl) (*GoSymbolSpec, *Diagnostic) {
	pos := fset.Position(elt.Pos())
	lit, ok := elt.(*ast.CompositeLit)
	if !ok || lit.Type != nil {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: "symbolSpec entry",
			Expected: "elided-type composite literal {Name: ..., Target: ..., Policy: ..., Platform: ...}",
			Actual:   typeNameForDiag(elt),
			Message:  "symbolSpec entries must be plain composite literals with an elided type"}
	}
	keys := map[string]ast.Expr{}
	for _, e := range lit.Elts {
		kv, ok := e.(*ast.KeyValueExpr)
		if !ok {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(e.Pos()).Line,
				Symbol: "symbolSpec entry", Expected: "keyed fields", Actual: "positional element",
				Message: "symbolSpec entries must set every key by name"}
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(kv.Key.Pos()).Line,
				Symbol: "symbolSpec entry", Expected: "plain identifier key", Actual: typeNameForDiag(kv.Key),
				Message: "symbolSpec entry keys must be plain identifiers"}
		}
		if _, dup := keys[key.Name]; dup {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(kv.Key.Pos()).Line,
				Symbol: key.Name, Expected: "each key exactly once", Actual: "duplicate key",
				Message: "symbolSpec entry sets a key twice"}
		}
		keys[key.Name] = kv.Value
	}
	for _, want := range []string{"Name", "Target", "Policy", "Platform"} {
		if _, ok := keys[want]; !ok {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: want,
				Expected: "all four keys Name/Target/Policy/Platform", Actual: "missing key " + want,
				Message: "symbolSpec entries must set all four named keys"}
		}
	}
	if len(keys) != 4 {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: "symbolSpec entry",
			Expected: "exactly the keys Name/Target/Policy/Platform", Actual: "unknown extra key",
			Message: "symbolSpec entries must not carry extra keys"}
	}

	nameLit, ok := keys["Name"].(*ast.BasicLit)
	if !ok || nameLit.Kind != gotoken.STRING {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(keys["Name"].Pos()).Line,
			Symbol: "Name", Expected: "verbatim string literal C symbol", Actual: typeNameForDiag(keys["Name"]),
			Message: "Name must be a plain string literal; computation and concatenation are rejected"}
	}
	symbol, err := strconv.Unquote(nameLit.Value)
	if err != nil {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(nameLit.Pos()).Line,
			Symbol: "Name", Expected: "quotable string literal", Actual: nameLit.Value,
			Message: "Name string literal cannot be decoded"}
	}

	target, d := extractSpecTarget(fset, rel, keys["Target"])
	if d != nil {
		return nil, d
	}

	policy, d := extractPolicyIdent(fset, rel, keys["Policy"], consts, "Policy",
		map[string]bool{"required": true, "optional": true}, "symbolPolicy")
	if d != nil {
		return nil, d
	}
	platform, d := extractPolicyIdent(fset, rel, keys["Platform"], consts, "Platform",
		map[string]bool{"allPlatforms": true, "windowsOnly": true}, "symbolPlatform")
	if d != nil {
		return nil, d
	}

	return &GoSymbolSpec{Name: symbol, Field: target, Policy: policy, Platform: platform,
		File: rel, Line: pos.Line}, nil
}

func extractSpecTarget(fset *gotoken.FileSet, rel string, e ast.Expr) (string, *Diagnostic) {
	pos := fset.Position(e.Pos())
	un, ok := unwrapParen(e).(*ast.UnaryExpr)
	if !ok || un.Op != gotoken.AND {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: "Target",
			Expected: "&table.<field> unary address-of selector", Actual: typeNameForDiag(e),
			Message: "Target must be a direct &table.<field> expression; indirect targets are rejected"}
	}
	sel, ok := unwrapParen(un.X).(*ast.SelectorExpr)
	if !ok {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: "Target",
			Expected: "&table.<field> unary address-of selector", Actual: typeNameForDiag(un.X),
			Message: "Target must select a field of the symbolSpecs parameter"}
	}
	x, ok := sel.X.(*ast.Ident)
	if !ok || x.Name != symbolSpecsParamName {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: "Target",
			Expected: "field selector on the " + symbolSpecsParamName + " parameter",
			Actual:   typeNameForDiag(sel.X),
			Message:  "Target must select a field of the symbolSpecs parameter; reflection and indirection are rejected"}
	}
	return sel.Sel.Name, nil
}

func extractPolicyIdent(fset *gotoken.FileSet, rel string, e ast.Expr, consts map[string]constDecl,
	key string, allowed map[string]bool, wantType string) (string, *Diagnostic) {
	pos := fset.Position(e.Pos())
	id, ok := unwrapParen(e).(*ast.Ident)
	if !ok {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: key,
			Expected: "identifier reference to a frozen constant", Actual: typeNameForDiag(e),
			Message: key + " must reference the frozen constants; computation is rejected"}
	}
	if !allowed[id.Name] {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: id.Name,
			Expected: allowedList(allowed), Actual: id.Name,
			Message: key + " must reference one of the frozen constants"}
	}
	cd, ok := consts[id.Name]
	if !ok {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: id.Name,
			Expected: "package-level const declaration", Actual: "missing",
			Message: key + " constant is not declared in the package"}
	}
	if cd.typeName != wantType {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: id.Name,
			Expected: "constant of type " + wantType, Actual: cd.typeName,
			Message: key + " constant is declared with an unexpected type"}
	}
	return id.Name, nil
}

func allowedList(m map[string]bool) string {
	order := []string{"required", "optional", "allPlatforms", "windowsOnly"}
	var out []string
	for _, o := range order {
		if m[o] {
			out = append(out, o)
		}
	}
	if len(out) == 0 {
		return "(none)"
	}
	s := out[0]
	for _, n := range out[1:] {
		s += "/" + n
	}
	return s
}

func extractExclusions(fset *gotoken.FileSet, rel string, vs *ast.ValueSpec) ([]GoExclusion, *Diagnostic) {
	line := fset.Position(vs.Pos()).Line
	if len(vs.Values) != 1 {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: "exclusions",
			Expected: "direct []exclusionSpec literal", Actual: "unexpected value count",
			Message: "exclusions must be initialized with a single direct literal"}
	}
	if vs.Type != nil && !isSliceOfIdent(vs.Type, "exclusionSpec") {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: "exclusions",
			Expected: "[]exclusionSpec declared type", Actual: typeNameForDiag(vs.Type),
			Message: "exclusions must be declared as []exclusionSpec"}
	}
	lit, ok := unwrapParen(vs.Values[0]).(*ast.CompositeLit)
	if !ok || !isSliceOfIdent(lit.Type, "exclusionSpec") {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: "exclusions",
			Expected: "direct []exclusionSpec composite literal", Actual: typeNameForDiag(vs.Values[0]),
			Message: "exclusions must be a direct []exclusionSpec literal; append and helpers are rejected"}
	}
	if len(lit.Elts) == 0 {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: "exclusions",
			Expected: "non-empty exclusion list", Actual: "empty literal",
			Message: "empty exclusion list is rejected"}
	}
	var out []GoExclusion
	for _, elt := range lit.Elts {
		pos := fset.Position(elt.Pos())
		cl, ok := elt.(*ast.CompositeLit)
		if !ok || cl.Type != nil {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: "exclusion entry",
				Expected: "elided-type composite literal {Name: ..., Header: ..., Reason: ...}",
				Actual:   typeNameForDiag(elt),
				Message:  "exclusion entries must be plain composite literals with an elided type"}
		}
		keys := map[string]ast.Expr{}
		for _, e := range cl.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(e.Pos()).Line,
					Symbol: "exclusion entry", Expected: "keyed fields", Actual: "positional element",
					Message: "exclusion entries must set every key by name"}
			}
			key, ok := kv.Key.(*ast.Ident)
			if !ok {
				return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(kv.Key.Pos()).Line,
					Symbol: "exclusion entry", Expected: "plain identifier key", Actual: typeNameForDiag(kv.Key),
					Message: "exclusion entry keys must be plain identifiers"}
			}
			if _, dup := keys[key.Name]; dup {
				return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(kv.Key.Pos()).Line,
					Symbol: key.Name, Expected: "each key exactly once", Actual: "duplicate key",
					Message: "exclusion entry sets a key twice"}
			}
			keys[key.Name] = kv.Value
		}
		for _, want := range []string{"Name", "Header", "Reason"} {
			if _, ok := keys[want]; !ok {
				return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: want,
					Expected: "all three keys Name/Header/Reason", Actual: "missing key " + want,
					Message: "exclusion entries must set all three named keys"}
			}
		}
		if len(keys) != 3 {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: "exclusion entry",
				Expected: "exactly the keys Name/Header/Reason", Actual: "unknown extra key",
				Message: "exclusion entries must not carry extra keys"}
		}
		name, d := stringLiteralValue(fset, rel, keys["Name"], "Name")
		if d != nil {
			return nil, d
		}
		header, d := stringLiteralValue(fset, rel, keys["Header"], "Header")
		if d != nil {
			return nil, d
		}
		if !isBareFileName(header) {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(keys["Header"].Pos()).Line,
				Symbol: "Header", Expected: "bare file name", Actual: header,
				Message: "Header must be a bare source file name without path separators"}
		}
		reason, d := stringLiteralValue(fset, rel, keys["Reason"], "Reason")
		if d != nil {
			return nil, d
		}
		if strings.TrimSpace(reason) == "" {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(keys["Reason"].Pos()).Line,
				Symbol: "Reason", Expected: "non-empty reason", Actual: reason,
				Message: "exclusion Reason must not be empty"}
		}
		out = append(out, GoExclusion{Name: name, Header: header, Reason: reason,
			File: rel, Line: pos.Line})
	}
	return out, nil
}

func extractCallback(fset *gotoken.FileSet, rel string, ts *ast.TypeSpec, windows bool) (*GoCallback, *Diagnostic) {
	line := fset.Position(ts.Pos()).Line
	if ts.TypeParams != nil {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: ts.Name.Name,
			Expected: "non-generic type", Actual: "type parameters",
			Message: "callback types must not be generic"}
	}
	ft, ok := unwrapParen(ts.Type).(*ast.FuncType)
	if !ok {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: ts.Name.Name,
			Expected: "func type", Actual: typeNameForDiag(ts.Type),
			Message: "callback type must be declared as a func type"}
	}
	params, d := extractParams(fset, rel, ts.Name.Name, ft.Params)
	if d != nil {
		return nil, d
	}
	result, d := extractResults(fset, rel, ts.Name.Name, ft.Results)
	if d != nil {
		return nil, d
	}
	if !windows && result != "" {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: ts.Name.Name,
			Expected: "no result (C void contract)", Actual: string(result),
			Message: "unix callback must not return anything"}
	}
	if windows && result != "uintptr" {
		return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: ts.Name.Name,
			Expected: "uintptr result", Actual: string(result),
			Message: "windows callback must return exactly one uintptr"}
	}
	return &GoCallback{Name: ts.Name.Name, File: rel, Line: line, Params: params, Result: result}, nil
}

func extractMapLevelKeyStruct(fset *gotoken.FileSet, rel string, ts *ast.TypeSpec) (*GoStruct, *Diagnostic) {
	line := fset.Position(ts.Pos()).Line
	failDiag := func(expected, actual, msg string) *Diagnostic {
		return &Diagnostic{Category: CatGoSyntax, File: rel, Line: line, Symbol: "mapLevelKey",
			Expected: expected, Actual: actual, Message: msg}
	}
	if ts.TypeParams != nil {
		return nil, failDiag("non-generic type", "type parameters",
			"mapLevelKey must not be generic")
	}
	if ts.Assign.IsValid() {
		return nil, failDiag("type definition", "alias declaration",
			"mapLevelKey must be a type definition, not an alias")
	}
	st, ok := unwrapParen(ts.Type).(*ast.StructType)
	if !ok {
		return nil, failDiag("struct type", typeNameForDiag(ts.Type),
			"mapLevelKey must be declared as a struct")
	}
	if st.Fields != nil {
		for _, fl := range st.Fields.List {
			if len(fl.Names) != 1 {
				return nil, failDiag("exactly one explicit field name per field", "embedded or grouped field",
					"mapLevelKey fields must not be embedded or grouped")
			}
		}
	}
	if st.Fields == nil || len(st.Fields.List) != len(frozenStructMembers) {
		return nil, failDiag(strconv.Itoa(len(frozenStructMembers))+" fields with frozen order",
			strconv.Itoa(structFieldCount(st)), "mapLevelKey field count mismatch")
	}
	out := &GoStruct{Name: ts.Name.Name, File: rel, Line: line}
	for i, fl := range st.Fields.List {
		pos := fset.Position(fl.Pos())
		wantField := approvedStructFieldNames[frozenStructMembers[i]]
		if fl.Names[0].Name != wantField {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: fl.Names[0].Name,
				Expected: "field " + wantField + " at position " + strconv.Itoa(i+1),
				Actual:   fl.Names[0].Name,
				Message:  "mapLevelKey field name or order does not match the frozen struct members"}
		}
		typ, d := goTypeOfExpr(fset, rel, "mapLevelKey", fl.Type)
		if d != nil {
			return nil, d
		}
		if typ != GoSourceType("*byte") {
			return nil, &Diagnostic{Category: CatGoSyntax, File: rel, Line: pos.Line, Symbol: wantField,
				Expected: "*byte (borrowed const char* member)", Actual: string(typ),
				Message: "mapLevelKey field type does not match the frozen const char* member mapping"}
		}
		out.Fields = append(out.Fields, GoParam{Name: fl.Names[0].Name, Type: typ})
	}
	return out, nil
}

func structFieldCount(st *ast.StructType) int {
	if st.Fields == nil {
		return 0
	}
	return len(st.Fields.List)
}

func stringLiteralValue(fset *gotoken.FileSet, rel string, e ast.Expr, key string) (string, *Diagnostic) {
	lit, ok := e.(*ast.BasicLit)
	if !ok || lit.Kind != gotoken.STRING {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(e.Pos()).Line,
			Symbol: key, Expected: "string literal", Actual: typeNameForDiag(e),
			Message: key + " must be a plain string literal"}
	}
	v, err := strconv.Unquote(lit.Value)
	if err != nil {
		return "", &Diagnostic{Category: CatGoSyntax, File: rel, Line: fset.Position(e.Pos()).Line,
			Symbol: key, Expected: "quotable string literal", Actual: lit.Value,
			Message: key + " string literal cannot be decoded"}
	}
	return v, nil
}

func isBareFileName(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, `/\:`)
}

func isSliceOfIdent(e ast.Expr, elem string) bool {
	arr, ok := unwrapParen(e).(*ast.ArrayType)
	if !ok || arr.Len != nil {
		return false
	}
	id, ok := unwrapParen(arr.Elt).(*ast.Ident)
	return ok && id.Name == elem
}

func unwrapParen(e ast.Expr) ast.Expr {
	for {
		p, ok := e.(*ast.ParenExpr)
		if !ok {
			return e
		}
		e = p.X
	}
}

func typeNameForDiag(e ast.Expr) string {
	switch t := e.(type) {
	case nil:
		return "(none)"
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		return "*" + typeNameForDiag(t.X)
	case *ast.ArrayType:
		if t.Len != nil {
			return "[...]" + typeNameForDiag(t.Elt)
		}
		return "[]" + typeNameForDiag(t.Elt)
	case *ast.SelectorExpr:
		return typeNameForDiag(t.X) + "." + t.Sel.Name
	case *ast.FuncType:
		return "func(...)"
	case *ast.BasicLit:
		return t.Value
	case *ast.UnaryExpr:
		return t.Op.String() + typeNameForDiag(t.X)
	case *ast.CompositeLit:
		return typeNameForDiag(t.Type) + "{...}"
	case *ast.Ellipsis:
		return "..." + typeNameForDiag(t.Elt)
	case *ast.ParenExpr:
		return "(" + typeNameForDiag(t.X) + ")"
	}
	return "(complex expression)"
}
