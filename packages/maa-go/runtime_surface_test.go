// 本文件是公开面的静态守卫(仿 rawfunc_surface_test.go 风格):把
// 当前里程碑冻结的公开 API 表面钉死为 AST 断言。MAA-21 交付实例创
// 建接线(Runtime.NewInstance 与公开 Instance.Close,绑定设计 §5、
// §8)时有意识地更新本守卫;后续里程碑交付新公开面时,同样必须有
// 意识地更新。
//
// 断言:
//   - *Runtime 的导出方法集恰为 {Version, Capabilities, NullSize,
//     NewInstance}(只读元数据访问 + 唯一的实例创建事务入口);
//   - *Instance 的导出方法集恰为 {Close}(单次关闭的公开入口;业务
//     方法由 M3 后续任务接入);
//   - 除 *Runtime 的 NewInstance 与 *Instance 的 Close 外,不存在其
//     他名为 NewInstance/Close 的导出函数或方法;
//   - 包级导出函数恰为 {Open}(唯一的进程初始化入口)。

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
	instanceMethods := map[string]bool{}
	packageFuncs := map[string]bool{}
	seenRuntime := false
	seenInstance := false

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
						t.Errorf("包级导出函数 %q 属于实例生命周期入口,只能作为类型方法存在", fd.Name.Name)
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
				if ast.IsExported(fd.Name.Name) && (fd.Name.Name == "NewInstance" || fd.Name.Name == "Close") {
					switch {
					case recv == "Runtime" && fd.Name.Name == "NewInstance", recv == "Instance" && fd.Name.Name == "Close":
						// MAA-21 交付的两个生命周期入口。
					default:
						t.Errorf("类型 %s 的导出方法 %q 不在生命周期入口冻结集合内", recv, fd.Name.Name)
					}
				}
			}
		})
	}

	if !seenRuntime {
		t.Fatal("未找到 Runtime 类型的方法声明")
	}
	if !seenInstance {
		t.Fatal("未找到 Instance 类型的方法声明")
	}

	// *Runtime 的导出方法集恰为冻结集合。
	want := map[string]bool{"Version": true, "Capabilities": true, "NullSize": true, "NewInstance": true}
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

	// *Instance 的导出方法集恰为 {Close}。
	wantInstance := map[string]bool{"Close": true}
	for m := range wantInstance {
		if !instanceMethods[m] {
			t.Errorf("*Instance 缺少冻结的导出方法 %s", m)
		}
	}
	for m := range instanceMethods {
		if !wantInstance[m] {
			t.Errorf("*Instance 出现冻结集合之外的导出方法 %s", m)
		}
	}

	// 包级导出函数恰为 {Open}:唯一的进程初始化入口。
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
