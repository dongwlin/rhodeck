// 本文件是公开面的静态守卫(仿 rawfunc_surface_test.go 风格):把
// 当前里程碑冻结的公开 API 表面钉死为 AST 断言。MAA-21 交付实例创
// 建接线(Runtime.NewInstance 与公开 Instance.Close,绑定设计 §5、
// §8)、MAA-22 交付任务与同步实例方法(Runtime.Log 与 Instance 的
// AppendTask/SetTaskParams/Start/Stop/BackToHome/Running/Connected,
// 绑定设计 §3、§5)、MAA-23 交付异步提交与结果关联(Instance 的
// Connect/Click/Screencap 与公开 Operation 的 ID/Wait,绑定设计 §5、
// §7)、MAA-24 交付图像/UUID/任务列表读取(Instance 的 GetImagePNG/
// UUID/Tasks,绑定设计 §5、§6;可选 BGR 不公开读取方法,冻结集合
// 因此不含任何 BGR 入口)、MAA-25 交付 Runtime.Close(关闭创建准入、
// 等待创建 lease、活动实例唯一关闭与终止使用,绑定设计 §5、§8)时
// 有意识地更新本守卫;后续里程碑交付新公开面时,同样必须有意识地
// 更新。
//
// 断言:
//   - *Runtime 的导出方法集恰为 {Version, Capabilities, NullSize,
//     Log, NewInstance, Close}(只读元数据访问 + 全局日志 + 唯一的
//     实例创建事务入口 + 幂等的绑定使用关闭);
//   - *Instance 的导出方法集恰为 {Close, AppendTask, SetTaskParams,
//     Start, Stop, BackToHome, Running, Connected, Connect, Click,
//     Screencap, GetImagePNG, UUID, Tasks}(单次关闭的公开入口 + 同
//     步任务/请求/查询方法 + 异步提交 + 输出读取;事件流公开面
//     Events/Done/Err/Fault/Diagnostics 尚未接入);
//   - *Operation 的导出方法集恰为 {ID, Wait}(关联键读取 + 可重复
//     等待,MAA-23);
//   - 除 *Runtime 的 NewInstance/Close 与 *Instance 的 Close 外,不
//     存在其他名为 NewInstance/Close 的导出函数或方法;
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
						// MAA-21/25 交付的三个生命周期入口。
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
	if !seenOperation {
		t.Fatal("未找到 Operation 类型的方法声明")
	}

	// *Runtime 的导出方法集恰为冻结集合(MAA-25 加入 Close)。
	want := map[string]bool{"Version": true, "Capabilities": true, "NullSize": true, "Log": true, "NewInstance": true, "Close": true}
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

	// *Instance 的导出方法集恰为 {Close} + MAA-22 同步方法 + MAA-23
	// 异步提交方法 + MAA-24 输出读取方法(不含任何 BGR 读取入口:可
	// 选 BGR 首期只报告 capability,§6 冻结决策)。
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
		"GetImagePNG":   true,
		"UUID":          true,
		"Tasks":         true,
	}
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

	// *Operation 的导出方法集恰为 {ID, Wait}(MAA-23)。
	wantOperation := map[string]bool{"ID": true, "Wait": true}
	for m := range wantOperation {
		if !operationMethods[m] {
			t.Errorf("*Operation 缺少冻结的导出方法 %s", m)
		}
	}
	for m := range operationMethods {
		if !wantOperation[m] {
			t.Errorf("*Operation 出现冻结集合之外的导出方法 %s", m)
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
