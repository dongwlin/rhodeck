package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Kind string

const (
	KindEntry      Kind = "entry"
	KindCapability Kind = "capability"
	KindType       Kind = "type"
	KindField      Kind = "field"
	KindConst      Kind = "const"
	KindError      Kind = "error"
)

type Symbol struct {
	Kind   Kind
	Owner  string
	Name   string
	Detail string
}

func (s Symbol) Key() string {
	if s.Owner == "" {
		return fmt.Sprintf("%s %s", s.Kind, s.Name)
	}
	return fmt.Sprintf("%s %s.%s", s.Kind, s.Owner, s.Name)
}

func (s Symbol) String() string {
	if s.Owner == "" {
		return fmt.Sprintf("%s %s: %s", s.Kind, s.Name, s.Detail)
	}
	return fmt.Sprintf("%s %s.%s: %s", s.Kind, s.Owner, s.Name, s.Detail)
}

const (
	bindingDir      = "packages/maa-go"
	nativeDir       = "packages/maa-go/internal/native"
	protocolDir     = "packages/control/worker/protocol"
	workerDir       = "apps/worker/internal/app"
	clientDir       = "packages/control/worker"
	entryRecvTypes  = "Runtime,Instance,Operation"
	capabilityRecv  = "Capabilities"
	packageFunction = "Open"
)

func ScanBinding(root string) ([]Symbol, error) {
	symbols, err := scanDir(filepath.Join(root, bindingDir))
	if err != nil {
		return nil, err
	}
	caps, err := scanCapabilityMethods(filepath.Join(root, nativeDir))
	if err != nil {
		return nil, err
	}
	symbols = append(symbols, caps...)
	return symbols, nil
}

func scanDir(dir string) ([]Symbol, error) {
	files, err := goFiles(dir)
	if err != nil {
		return nil, err
	}
	var symbols []Symbol
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		symbols = append(symbols, scanFile(file)...)
	}
	return symbols, nil
}

func scanCapabilityMethods(dir string) ([]Symbol, error) {
	files, err := goFiles(dir)
	if err != nil {
		return nil, err
	}
	var symbols []Symbol
	fset := token.NewFileSet()
	for _, path := range files {
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, decl := range file.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Recv == nil || len(fd.Recv.List) == 0 || !fd.Name.IsExported() {
				continue
			}
			if receiverBase(fd.Recv.List[0].Type) != capabilityRecv {
				continue
			}
			symbols = append(symbols, Symbol{
				Kind:   KindCapability,
				Owner:  capabilityRecv,
				Name:   fd.Name.Name,
				Detail: renderFuncDetail(fd),
			})
		}
	}
	return symbols, nil
}

func scanFile(file *ast.File) []Symbol {
	var symbols []Symbol
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			if !d.Name.IsExported() {
				continue
			}
			if d.Recv == nil || len(d.Recv.List) == 0 {
				symbols = append(symbols, Symbol{Kind: KindEntry, Name: d.Name.Name, Detail: renderFuncDetail(d)})
				continue
			}
			recv := receiverBase(d.Recv.List[0].Type)
			switch recv {
			case entryRecvTypesSplit[0], entryRecvTypesSplit[1], entryRecvTypesSplit[2]:
				symbols = append(symbols, Symbol{Kind: KindEntry, Owner: recv, Name: d.Name.Name, Detail: renderFuncDetail(d)})
			case capabilityRecv:
				symbols = append(symbols, Symbol{Kind: KindCapability, Owner: recv, Name: d.Name.Name, Detail: renderFuncDetail(d)})
			default:
				symbols = append(symbols, Symbol{Kind: KindEntry, Owner: recv, Name: d.Name.Name, Detail: renderFuncDetail(d)})
			}
		case *ast.GenDecl:
			for _, spec := range d.Specs {
				switch s := spec.(type) {
				case *ast.TypeSpec:
					if !s.Name.IsExported() {
						continue
					}
					symbols = append(symbols, Symbol{Kind: KindType, Name: s.Name.Name, Detail: renderTypeDetail(s)})
					if st, ok := s.Type.(*ast.StructType); ok {
						symbols = append(symbols, scanFields(s.Name.Name, st)...)
					}
				case *ast.ValueSpec:
					symbols = append(symbols, scanValues(d.Tok, s)...)
				}
			}
		}
	}
	return symbols
}

func scanFields(owner string, st *ast.StructType) []Symbol {
	var symbols []Symbol
	for _, field := range st.Fields.List {
		if len(field.Names) == 0 {
			symbols = append(symbols, Symbol{Kind: KindField, Owner: owner, Name: "(embedded)", Detail: exprString(field.Type)})
			continue
		}
		for _, name := range field.Names {
			if !name.IsExported() {
				continue
			}
			symbols = append(symbols, Symbol{Kind: KindField, Owner: owner, Name: name.Name, Detail: exprString(field.Type)})
		}
	}
	return symbols
}

func scanValues(tok token.Token, spec *ast.ValueSpec) []Symbol {
	var symbols []Symbol
	kind := KindError
	if tok == token.CONST {
		kind = KindConst
	}
	for i, name := range spec.Names {
		if !name.IsExported() {
			continue
		}
		detail := ""
		if spec.Type != nil {
			detail = exprString(spec.Type)
		}
		if i < len(spec.Values) {
			if detail != "" {
				detail += " = "
			}
			detail += exprString(spec.Values[i])
		}
		if detail == "" {
			detail = "(implicit)"
		}
		symbols = append(symbols, Symbol{Kind: kind, Name: name.Name, Detail: detail})
	}
	return symbols
}

var entryRecvTypesSplit = strings.Split(entryRecvTypes, ",")

func renderFuncDetail(fd *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")
	if fd.Recv != nil && len(fd.Recv.List) > 0 {
		b.WriteString("(")
		b.WriteString(exprString(fd.Recv.List[0].Type))
		b.WriteString(") ")
	}
	b.WriteString(fd.Name.Name)
	b.WriteString(renderSignature(fd.Type))
	return b.String()
}

func renderSignature(ft *ast.FuncType) string {
	var b strings.Builder
	b.WriteString("(")
	b.WriteString(strings.Join(fieldStrings(ft.Params), ", "))
	b.WriteString(")")
	switch {
	case ft.Results == nil:
	case len(ft.Results.List) == 1 && len(ft.Results.List[0].Names) == 0:
		b.WriteString(" ")
		b.WriteString(exprString(ft.Results.List[0].Type))
	default:
		b.WriteString(" (")
		b.WriteString(strings.Join(fieldStrings(ft.Results), ", "))
		b.WriteString(")")
	}
	return b.String()
}

func fieldStrings(fl *ast.FieldList) []string {
	if fl == nil {
		return nil
	}
	var out []string
	for _, field := range fl.List {
		item := ""
		for i, name := range field.Names {
			if i > 0 {
				item += ", "
			}
			item += name.Name
		}
		if len(field.Names) > 0 {
			item += " "
		}
		item += exprString(field.Type)
		out = append(out, item)
	}
	return out
}

func renderTypeDetail(spec *ast.TypeSpec) string {
	if spec.Assign.IsValid() {
		return "alias " + exprString(spec.Type)
	}
	switch spec.Type.(type) {
	case *ast.StructType:
		return "struct"
	default:
		return exprString(spec.Type)
	}
}

func exprString(x ast.Expr) string { return types.ExprString(x) }

func receiverBase(x ast.Expr) string {
	if star, ok := x.(*ast.StarExpr); ok {
		x = star.X
	}
	if id, ok := x.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func goFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files = append(files, filepath.Join(dir, name))
	}
	sort.Strings(files)
	return files, nil
}
