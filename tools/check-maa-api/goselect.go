// goselect.go 按 §9.1.1 冻结规则选择真实参与目标构建的 Go 源文件:
// Go 输入固定为 --go-root/packages/maa-go/internal/native,按目标
// GOOS/GOARCH、CGO_ENABLED=0 与工具链 release tags 处理文件名后缀、
// go:build 与旧式 build 行,排除 _test.go 与 _/. 前缀文件;不接受
// 任何自定义 tags。§3.1 的三个共用声明文件(api.go、symbols.go、
// callback_types.go)必须无条件参与:文件缺失、被约束隐藏(存在于
// 目录但未入选)或自带 build 约束注释均失败。选择失败以 input 类
// 诊断报告,共用文件自带约束以 go-syntax 类诊断报告。

package main

import (
	"fmt"
	"go/build"
	"go/build/constraint"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// goNativeRelPath 是冻结的 Go 输入相对路径(相对 --go-root)。
const goNativeRelPath = "packages/maa-go/internal/native"

// sharedDeclFiles 是 §3.1 冻结的三个共用声明文件;不得带任何 build
// 约束,必须参与所有目标的选择。
var sharedDeclFiles = []string{"api.go", "symbols.go", "callback_types.go"}

// GoSourceFile 是一个入选源文件:Name 为目录内文件名,Path 为绝对
// 路径。
type GoSourceFile struct {
	Name string
	Path string
}

// GoSelection 是一次文件选择结果:Dir 为 native 目录绝对路径,
// Files 为入选文件(按文件名排序)。
type GoSelection struct {
	Dir   string
	Files []GoSourceFile
}

// names 返回入选文件名切片(顺序同 Files)。
func (s *GoSelection) names() []string {
	out := make([]string, len(s.Files))
	for i, f := range s.Files {
		out[i] = f.Name
	}
	return out
}

func (s *GoSelection) contains(name string) bool {
	for _, f := range s.Files {
		if f.Name == name {
			return true
		}
	}
	return false
}

// SelectGoFiles 对 goRoot 下的冻结 native 目录执行目标感知的文件
// 选择;目录缺失、无入选文件或共用文件被隐藏/缺失都以 input 类
// 诊断失败。release tags 取本工具链默认值;BuildTags/ToolTags 保持
// 为空,不接受自定义 tags。
func SelectGoFiles(goRoot string, target Target) (*GoSelection, []Diagnostic) {
	fail := func(d Diagnostic) (*GoSelection, []Diagnostic) { return nil, []Diagnostic{d} }
	dir := filepath.Join(goRoot, filepath.FromSlash(goNativeRelPath))
	st, err := os.Stat(dir)
	if err != nil {
		return fail(Diagnostic{Category: CatInput, File: dir, Symbol: goNativeRelPath,
			Expected: "existing go package directory", Actual: "missing",
			Message: "go input directory does not exist under --go-root"})
	}
	if !st.IsDir() {
		return fail(Diagnostic{Category: CatInput, File: dir, Symbol: goNativeRelPath,
			Expected: "directory", Actual: "not a directory",
			Message: "go input path is not a directory"})
	}

	// build.Context 只设置目标相关字段:CGO_ENABLED=0、工具链
	// release tags;不继承任何自定义 build/tool tags。
	ctx := build.Context{
		GOOS:        target.GOOS,
		GOARCH:      target.GOARCH,
		GOROOT:      runtime.GOROOT(),
		GOPATH:      build.Default.GOPATH,
		Dir:         build.Default.Dir,
		Compiler:    build.Default.Compiler,
		CgoEnabled:  false, // 生产构建固定 CGO_ENABLED=0
		ReleaseTags: build.Default.ReleaseTags,
	}
	pkg, err := ctx.ImportDir(dir, build.IgnoreVendor)
	if err != nil {
		return fail(Diagnostic{Category: CatInput, File: dir, Symbol: "go files",
			Expected: "buildable go package", Actual: err.Error(),
			Message: "go input directory does not select any buildable file for this target"})
	}
	if len(pkg.GoFiles) == 0 {
		return fail(Diagnostic{Category: CatInput, File: dir, Symbol: "go files",
			Expected: "at least one selected go file", Actual: "none",
			Message: "no go file participates in the target build"})
	}

	names := append([]string(nil), pkg.GoFiles...)
	sort.Strings(names)
	sel := &GoSelection{Dir: dir}
	for _, name := range names {
		sel.Files = append(sel.Files, GoSourceFile{Name: name, Path: filepath.Join(dir, name)})
	}

	// 三个共用声明文件:必须存在、必须入选、不得自带任何 build
	// 约束注释(即使约束对当前目标为真)。
	for _, name := range sharedDeclFiles {
		full := filepath.Join(dir, name)
		st, err := os.Stat(full)
		switch {
		case err != nil || st.IsDir():
			return fail(Diagnostic{Category: CatInput, File: full, Symbol: name,
				Expected: "existing shared declaration file", Actual: "missing",
				Message: "frozen shared declaration file (§3.1) is missing"})
		case !sel.contains(name):
			return fail(Diagnostic{Category: CatInput, File: full, Symbol: name,
				Expected: "selected for the target build", Actual: "hidden by build constraints",
				Message: "shared declaration file must not carry build constraints; it exists but is not selected"})
		}
		if line, kind := firstBuildConstraint(full); line > 0 {
			return fail(Diagnostic{Category: CatGoSyntax, File: full, Line: line, Symbol: name,
				Expected: "shared declaration file without build constraints", Actual: kind,
				Message: "shared declaration file must be unconditionally part of every target"})
		}
	}
	return sel, nil
}

// firstBuildConstraint 扫描文件头部(package 子句之前)的行,返回
// 第一个 build 约束行(//go:build 或旧 +build)的行号与拼写;无约
// 束返回 0。与 go/build 的语义一致:package 子句之后的约束行不算。
func firstBuildConstraint(path string) (int, string) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, ""
	}
	for i, ln := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(ln)
		if trimmed == "" || strings.HasPrefix(trimmed, "//") {
			if constraint.IsGoBuild(trimmed) {
				return i + 1, trimmed
			}
			if constraint.IsPlusBuild(trimmed) {
				return i + 1, trimmed
			}
			continue
		}
		if strings.HasPrefix(trimmed, "package ") || trimmed == "package" {
			return 0, "" // 到达 package 子句,头部结束
		}
		// 其他非注释非空行(如块注释中间行)继续扫描,package 行终止。
		if strings.HasPrefix(trimmed, "/*") || strings.HasPrefix(trimmed, "*") {
			continue
		}
		return 0, ""
	}
	return 0, ""
}

// goRootAbs 把 --go-root 规范化为绝对路径;失败以 input 类诊断报告。
func goRootAbs(goRoot string) (string, *Diagnostic) {
	abs, err := filepath.Abs(goRoot)
	if err != nil {
		return "", &Diagnostic{Category: CatInput, File: goRoot, Symbol: "--go-root",
			Expected: "resolvable go root path", Actual: err.Error(),
			Message: "--go-root cannot be resolved to an absolute path"}
	}
	return abs, nil
}

func fmtTarget(t Target) string {
	return fmt.Sprintf("(%s, %s)", t.GOOS, t.GOARCH)
}
