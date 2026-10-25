// 本文件是公开面的静态守卫(仿 rawfunc_surface_test.go 风格):把
// 本里程碑冻结的公开 API 表面钉死为 AST 断言。公开面必须仍然没有
// 可创建半成品 Instance 的入口——实例创建与完整关闭由 M3 承接;
// 后续里程碑交付新公开面时,必须有意识地更新本守卫。
//
// 断言:
//   - *Runtime 的导出方法集恰为 {Version, Capabilities, NullSize}
//     (全部只读元数据访问,没有触发业务操作的方法);
//   - 包内不存在名为 NewInstance/Close 的导出函数或方法(实例生命
//     周期入口);
//   - 包级导出函数恰为 {Open}(唯一的生命周期入口)。

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

// receiverBaseName 返回方法接收者的基类型名;无接收者或无法识别的
// 形式返回空串。
func receiverBaseName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	expr := recv.List[0].Type
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	if id, ok := expr.(*ast.Ident); ok {
		return id.Name
	}
	return ""
}

func TestPublicRuntimeSurfaceIsFrozen(t *testing.T) {
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd 失败,无法定位公开包目录: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取公开包目录 %s 失败: %v", dir, err)
	}

	runtimeMethods := map[string]bool{}
	packageFuncs := map[string]bool{}
	seenRuntime := false

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatalf("解析 %s 失败: %v", path, err)
			}
			for _, decl := range file.Decls {
				fd, ok := decl.(*ast.FuncDecl)
				if !ok {
					continue
				}
				if fd.Recv == nil {
					if !ast.IsExported(fd.Name.Name) {
						continue
					}
					packageFuncs[fd.Name.Name] = true
					if fd.Name.Name == "NewInstance" || fd.Name.Name == "Close" {
						t.Errorf("包级导出函数 %q 属于实例生命周期入口,本里程碑不得存在", fd.Name.Name)
					}
					continue
				}
				recv := receiverBaseName(fd.Recv)
				if recv == "Runtime" {
					seenRuntime = true
					if ast.IsExported(fd.Name.Name) {
						runtimeMethods[fd.Name.Name] = true
					}
				}
				if ast.IsExported(fd.Name.Name) && (fd.Name.Name == "NewInstance" || fd.Name.Name == "Close") {
					t.Errorf("类型 %s 的导出方法 %q 属于实例生命周期入口,本里程碑不得存在", recv, fd.Name.Name)
				}
			}
		})
	}

	if !seenRuntime {
		t.Fatal("未找到 Runtime 类型的方法声明")
	}

	// *Runtime 的导出方法集恰为冻结集合。
	want := map[string]bool{"Version": true, "Capabilities": true, "NullSize": true}
	for m := range want {
		if !runtimeMethods[m] {
			t.Errorf("*Runtime 缺少冻结的导出方法 %s", m)
		}
	}
	for m := range runtimeMethods {
		if !want[m] {
			t.Errorf("*Runtime 出现冻结集合之外的导出方法 %s", m)
		}
	}

	// 包级导出函数恰为 {Open}:唯一的生命周期入口。
	wantFuncs := map[string]bool{"Open": true}
	for f := range wantFuncs {
		if !packageFuncs[f] {
			t.Errorf("包级缺少导出函数 %s", f)
		}
	}
	for f := range packageFuncs {
		if !wantFuncs[f] {
			t.Errorf("包级出现冻结集合之外的导出函数 %s", f)
		}
	}
}
