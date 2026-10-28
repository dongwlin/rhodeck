// 本文件是公开面的静态守卫:把当前公开 API 表面钉死为 AST 断言,新增或
// 删除导出符号都必须有意识地更新本守卫。

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
		t.Fatalf("os.Getwd failed, cannot locate the public package directory: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("failed to read the public package directory %s: %v", dir, err)
	}

	runtimeMethods := map[string]bool{}
	instanceMethods := map[string]bool{}
	operationMethods := map[string]bool{}
	packageFuncs := map[string]bool{}
	seenRuntime := false
	seenInstance := false
	seenOperation := false

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, name)
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
			if err != nil {
				t.Fatalf("failed to parse %s: %v", path, err)
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
						t.Errorf("package-level exported function %q belongs to the instance lifecycle entries and may only exist as a type method", fd.Name.Name)
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
				if recv == "Instance" {
					seenInstance = true
					if ast.IsExported(fd.Name.Name) {
						instanceMethods[fd.Name.Name] = true
					}
				}
				if recv == "Operation" {
					seenOperation = true
					if ast.IsExported(fd.Name.Name) {
						operationMethods[fd.Name.Name] = true
					}
				}
				if ast.IsExported(fd.Name.Name) && (fd.Name.Name == "NewInstance" || fd.Name.Name == "Close") {
					switch {
					case recv == "Runtime" && fd.Name.Name == "NewInstance",
						recv == "Instance" && fd.Name.Name == "Close",
						recv == "Runtime" && fd.Name.Name == "Close":
						//  交付的三个生命周期入口。
					default:
						t.Errorf("exported method %q of type %s is not in the frozen lifecycle entry set", recv, fd.Name.Name)
					}
				}
			}
		})
	}

	if !seenRuntime {
		t.Fatal("no method declarations found for the Runtime type")
	}
	if !seenInstance {
		t.Fatal("no method declarations found for the Instance type")
	}
	if !seenOperation {
		t.Fatal("no method declarations found for the Operation type")
	}

	// *Runtime 的导出方法集恰为冻结集合(加入 Close 与两个实验查询)。
	want := map[string]bool{"Version": true, "Capabilities": true, "NullSize": true, "Log": true,
		"MapLevelKey": true, "ItemName": true, "NewInstance": true, "Close": true}
	for m := range want {
		if !runtimeMethods[m] {
			t.Errorf("*Runtime is missing the frozen exported method %s", m)
		}
	}
	for m := range runtimeMethods {
		if !want[m] {
			t.Errorf("*Runtime has an exported method outside the frozen set: %s", m)
		}
	}

	// *Instance 的导出方法集恰为 {Close} + 同步方法 +
	// 异步提交方法(含仅 Windows 生效的 AttachWindow) + 输出读取方
	// 法(含可选能力门下的 GetImageBGR) + 事件与故障观察方法。
	wantInstance := map[string]bool{
		"Close":         true,
		"AppendTask":    true,
		"SetTaskParams": true,
		"Start":         true,
		"Stop":          true,
		"BackToHome":    true,
		"Running":       true,
		"Connected":     true,
		"Connect":       true,
		"Click":         true,
		"Screencap":     true,
		"AttachWindow":  true,
		"GetImagePNG":   true,
		"GetImageBGR":   true,
		"UUID":          true,
		"Tasks":         true,
		"Events":        true,
		"Done":          true,
		"Err":           true,
		"Fault":         true,
		"Diagnostics":   true,
	}
	for m := range wantInstance {
		if !instanceMethods[m] {
			t.Errorf("*Instance is missing the frozen exported method %s", m)
		}
	}
	for m := range instanceMethods {
		if !wantInstance[m] {
			t.Errorf("*Instance has an exported method outside the frozen set: %s", m)
		}
	}

	// *Operation 的导出方法集恰为 {ID, Wait}()。
	wantOperation := map[string]bool{"ID": true, "Wait": true}
	for m := range wantOperation {
		if !operationMethods[m] {
			t.Errorf("*Operation is missing the frozen exported method %s", m)
		}
	}
	for m := range operationMethods {
		if !wantOperation[m] {
			t.Errorf("*Operation has an exported method outside the frozen set: %s", m)
		}
	}

	// 包级导出函数恰为 {Open}:唯一的进程初始化入口。
	wantFuncs := map[string]bool{"Open": true}
	for f := range wantFuncs {
		if !packageFuncs[f] {
			t.Errorf("the package level is missing the exported function %s", f)
		}
	}
	for f := range packageFuncs {
		if !wantFuncs[f] {
			t.Errorf("the package level has an exported function outside the frozen set: %s", f)
		}
	}
}
