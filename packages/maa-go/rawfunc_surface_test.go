// 本文件是公开包的静态守卫:公开包不得导出可复制的 raw 函数变量。
// 解析本目录(测试运行时 os.Getwd 即 packages/maa-go)全部非
// _test.go 文件,断言:无导出包级 var/const 的显式类型为函数类型、
// 无导出结构体的导出字段为函数类型、无导出函数/方法返回函数类型。
//
// 与 internal/native 的同名静态检查各自独立成文:两处都是廉价静态
// 检查,重复换取双向守卫——公开包与 raw 边界层任何一侧都不出现
// 可复制的 raw 函数面。raw 函数只能经 Runtime 持有的未导出 API 表
// 到达,类型别名再导出的错误结构体不含函数字段,由定义处的同类
// 检查覆盖。

package maa

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicPackageExportsNoRawFuncVariables(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd failed, cannot locate the public package directory: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read the public package directory %s: %v", dir, err)
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		t.Fatal("no non-test Go source files found in the public package directory")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
			}
			for _, decl := range file.Decls {
				switch d := decl.(type) {
				case *ast.GenDecl:
					switch d.Tok {
					case token.VAR, token.CONST:
						for _, spec := range d.Specs {
							vs, ok := spec.(*ast.ValueSpec)
							if !ok || vs.Type == nil {
								continue
							}
							if _, isFunc := vs.Type.(*ast.FuncType); !isFunc {
								continue
							}
							kind := "var"
							if d.Tok == token.CONST {
								kind = "const"
							}
							for _, id := range vs.Names {
								if ast.IsExported(id.Name) {
									t.Errorf("exported package-level %s %q has an explicit function type; the public package must not export copyable raw function variables", kind, id.Name)
								}
							}
						}
					case token.TYPE:
						for _, spec := range d.Specs {
							ts := spec.(*ast.TypeSpec)
							st, ok := ts.Type.(*ast.StructType)
							if !ok || !ast.IsExported(ts.Name.Name) {
								continue
							}
							for _, field := range st.Fields.List {
								if _, isFunc := field.Type.(*ast.FuncType); !isFunc || len(field.Names) == 0 {
									continue
								}
								for _, id := range field.Names {
									if ast.IsExported(id.Name) {
										t.Errorf("exported field %q of exported struct %s has a function type; copyable raw functions must not be exported", ts.Name.Name, id.Name)
									}
								}
							}
						}
					}
				case *ast.FuncDecl:
					if !ast.IsExported(d.Name.Name) || d.Type.Results == nil {
						continue
					}
					for _, result := range d.Type.Results.List {
						if _, isFunc := result.Type.(*ast.FuncType); isFunc {
							t.Errorf("exported function/method %q has a function result type; copyable raw functions must not be exported", d.Name.Name)
						}
					}
				}
			}
		})
	}
}
