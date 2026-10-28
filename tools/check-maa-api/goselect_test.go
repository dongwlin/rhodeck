// goselect_test.go 校验 Go 输入文件选择:真实 native 目录在两个目标下
// 各自入选正确的平台文件;共用声明文件缺失、被约束隐藏或自带约束注释
// 均失败;_test.go 不参与选择。

package main

import (
	"strings"
	"testing"
)

// TestSelectRealNativeDir 使用仓库真实 native 目录验证两个目标的入选
// 集合:平台条件文件按目标切换,三个共用声明文件无条件入选,测试文件
// 永不入选。
func TestSelectRealNativeDir(t *testing.T) {
	goRoot := repoRoot(t)
	linuxSel := mustSelect(t, goRoot, linuxTarget)
	windowsSel := mustSelect(t, goRoot, windowsTarget)

	linuxNames := strings.Join(linuxSel.names(), ",")
	windowsNames := strings.Join(windowsSel.names(), ",")

	for _, shared := range sharedDeclFiles {
		if !linuxSel.contains(shared) || !windowsSel.contains(shared) {
			t.Errorf("shared file %s must be selected on both targets", shared)
		}
	}
	if !linuxSel.contains("loader_unix.go") || linuxSel.contains("loader_other.go") || linuxSel.contains("loader_windows.go") {
		t.Errorf("linux selection = %s", linuxNames)
	}
	if !windowsSel.contains("loader_windows.go") || windowsSel.contains("loader_other.go") || windowsSel.contains("loader_unix.go") {
		t.Errorf("windows selection = %s", windowsNames)
	}
	for _, sel := range []*GoSelection{linuxSel, windowsSel} {
		for _, name := range sel.names() {
			if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "_") || strings.HasPrefix(name, ".") {
				t.Errorf("file %s must not participate in the build", name)
			}
		}
	}
}

// TestSelectMissingSharedFile 共用声明文件缺失以 input 类诊断失败。
func TestSelectMissingSharedFile(t *testing.T) {
	files := copySynthetic()
	delete(files, "symbols.go")
	sel, diags := SelectGoFiles(writeGoRoot(t, files), linuxTarget)
	if len(diags) != 1 || sel != nil {
		t.Fatalf("expected one diagnostic, got sel=%v diags=%v", sel, diags)
	}
	if diags[0].Category != CatInput || !strings.Contains(diags[0].Message, "missing") {
		t.Errorf("diagnostic = %+v", diags[0])
	}
	if !strings.Contains(diags[0].File, "symbols.go") {
		t.Errorf("diagnostic must locate symbols.go, got %+v", diags[0])
	}
}

// TestSelectHiddenSharedFile 共用文件被约束隐藏(存在于目录但未入选)
// 以 input 类诊断失败。
func TestSelectHiddenSharedFile(t *testing.T) {
	files := copySynthetic()
	files["api.go"] = "//go:build windows\n\n" + files["api.go"]
	sel, diags := SelectGoFiles(writeGoRoot(t, files), linuxTarget)
	if len(diags) != 1 || sel != nil {
		t.Fatalf("expected one diagnostic, got sel=%v diags=%v", sel, diags)
	}
	if diags[0].Category != CatInput || !strings.Contains(diags[0].Actual, "hidden") {
		t.Errorf("diagnostic = %+v", diags[0])
	}
}

// TestSelectConstrainedSharedFile 共用文件即使对当前目标被选中,自带
// build 约束注释同样失败(go-syntax)。
func TestSelectConstrainedSharedFile(t *testing.T) {
	files := copySynthetic()
	files["api.go"] = "//go:build windows\n\n" + files["api.go"]
	sel, diags := SelectGoFiles(writeGoRoot(t, files), windowsTarget)
	if len(diags) != 1 || sel != nil {
		t.Fatalf("expected one diagnostic, got sel=%v diags=%v", sel, diags)
	}
	if diags[0].Category != CatGoSyntax {
		t.Errorf("category = %q, want %q", diags[0].Category, CatGoSyntax)
	}
	if diags[0].Line <= 0 || !strings.Contains(diags[0].Actual, "go:build") {
		t.Errorf("diagnostic must locate the constraint line, got %+v", diags[0])
	}
}

// TestSelectConstrainedPlusBuild 旧 +build 行同样视为约束注释。
func TestSelectConstrainedPlusBuild(t *testing.T) {
	files := copySynthetic()
	files["symbols.go"] = "// +build windows\n\n" + files["symbols.go"]
	_, diags := SelectGoFiles(writeGoRoot(t, files), windowsTarget)
	if len(diags) != 1 || diags[0].Category != CatGoSyntax {
		t.Fatalf("expected one go-syntax diagnostic, got %v", diags)
	}
	if !strings.Contains(diags[0].Actual, "+build") {
		t.Errorf("diagnostic must locate the +build line, got %+v", diags[0])
	}
}

// TestSelectMissingGoRoot go-root 下没有 native 目录以 input 类诊断
// 失败并指向冻结的相对路径。
func TestSelectMissingGoRoot(t *testing.T) {
	_, diags := SelectGoFiles(t.TempDir(), linuxTarget)
	if len(diags) != 1 || diags[0].Category != CatInput {
		t.Fatalf("expected one input diagnostic, got %v", diags)
	}
	if !strings.Contains(diags[0].Symbol, goNativeRelPath) && !strings.Contains(diags[0].Message, goNativeRelPath) {
		t.Errorf("diagnostic must mention the frozen relative path, got %+v", diags[0])
	}
}
