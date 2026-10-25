// realnative_test.go 以真实仓库 native 目录(复制到临时目录构造变体
// go-root)做最小破坏变体,走完整命令入口 Run 验收失败行为:空清单、
// 漏绑、错误 Target、多余字段、陈旧/重复排除、Name 拼错、重复绑定、
// 共用文件缺失/被约束隐藏与平台矛盾,各断言非零退出、诊断类别、定位
// 与绝不输出 PASS。R/O 策略与锁定 profile 的独立期望比对属 MAA-10,
// required↔optional 互换变体在本任务跳过并留待 MAA-10。

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// copyRealNative 读取真实仓库 native 目录的全部 .go 文件内容(键为
// 文件名),供变体改写。
func copyRealNative(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), filepath.FromSlash(goNativeRelPath))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		files[e.Name()] = string(b)
	}
	return files
}

// writeMutatedGoRoot 把改写后的文件集合写成 go-root。
func writeMutatedGoRoot(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, content := range files {
		full := filepath.Join(root, filepath.FromSlash(goNativeRelPath), name)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// realNativeNames 列出真实 native 目录可参与构建的 .go 文件名(排除
// _test.go,排序),供选择结果断言。
func realNativeNames(t *testing.T) []string {
	t.Helper()
	names := make([]string, 0)
	for name := range copyRealNative(t) {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// TestRealNativeVariantsFailThroughCLI 是真实目录破坏变体的端到端
// 验收:每个变体都断言非零退出、stdout 无 PASS、stderr 含诊断类别
// 与定位片段。C 输入用测试 profile 注册表的合成正例(与锁定形状
// 一致),Go 侧为改写后的真实目录副本。
func TestRealNativeVariantsFailThroughCLI(t *testing.T) {
	cFiles := positiveFiles(t)
	registerTestProfile(t, testProfileFromFiles(cFiles))
	cInput := writeCInputDir(t, cFiles, nil)

	cases := []struct {
		name    string
		goos    string
		mutate  func(t *testing.T, fs map[string]string)
		wantCat string
		wantLoc string // stderr 必含的定位/符号片段
	}{
		{"empty symbolSpecs", "linux", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = replaceGoBlock(t, fs["symbols.go"],
				"func symbolSpecs(table *api) []symbolSpec {",
				"func symbolSpecs(table *api) []symbolSpec {\n\treturn []symbolSpec{}\n}")
		}, CatGoSyntax, "empty symbol list"},
		{"empty exclusions", "linux", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = replaceGoBlock(t, fs["symbols.go"],
				"var exclusions = []exclusionSpec{",
				"var exclusions = []exclusionSpec{}")
		}, CatGoSyntax, "empty exclusion list"},
		{"empty api struct", "linux", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = replaceGoBlock(t, fs["api.go"], "type api struct {", "type api struct {}")
		}, CatGoSyntax, "empty struct"},
		{"missing binding for one symbol", "linux", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"		{Name: \"AsstBackToHome\", Target: &table.BackToHome, Policy: required, Platform: allPlatforms},\n", "")
		}, CatRelation, "BackToHome"},
		{"target points to nonexistent field", "linux", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Target: &table.BackToHome,",
				"Target: &table.BackToHoma,")
		}, CatRelation, "BackToHoma"},
		{"extra raw func field without entry", "linux", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = mustReplace(t, fs["api.go"],
				"AsyncAttachWindow   func(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32",
				"AsyncAttachWindow   func(handle uintptr, hwnd uintptr, screencapMethod uint64, mouseMethod uint64, keyboardMethod uint64, block uint8) int32\n\tCheckerExtra        func(handle uintptr) uint8")
		}, CatRelation, "CheckerExtra"},
		{"renamed exclusion is stale", "linux", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Name: \"AsstConnect\",",
				"Name: \"AsstConnectOld\",")
		}, CatRelation, "stale exclusion"},
		{"typo in entry name", "linux", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], "Name: \"AsstStart\",",
				"Name: \"AsstStartt\",")
		}, CatRelation, "AsstStartt"},
		{"duplicate binding", "linux", func(t *testing.T, fs map[string]string) {
			line := "		{Name: \"AsstStop\", Target: &table.Stop, Policy: required, Platform: allPlatforms},\n"
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"], line, line+line)
		}, CatRelation, "more than once"},
		{"windows entry platform widened", "linux", func(t *testing.T, fs map[string]string) {
			fs["symbols.go"] = mustReplace(t, fs["symbols.go"],
				"Policy: optional, Platform: windowsOnly},",
				"Policy: optional, Platform: allPlatforms},")
		}, CatRelation, "windows-conditional"},
		{"shared file hidden by constraint", "linux", func(t *testing.T, fs map[string]string) {
			fs["api.go"] = "//go:build windows\n\n" + fs["api.go"]
		}, CatInput, "hidden"},
		{"shared file missing", "linux", func(t *testing.T, fs map[string]string) {
			delete(fs, "symbols.go")
		}, CatInput, "symbols.go"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files := copyRealNative(t)
			tc.mutate(t, files)
			goRoot := writeMutatedGoRoot(t, files)
			var stdout, stderr bytes.Buffer
			code := Run([]string{
				"--profile", testProfileID, "--goos", tc.goos, "--goarch", "amd64",
				"--c-input", cInput, "--go-root", goRoot,
			}, &stdout, &stderr)
			if code == 0 {
				t.Fatalf("mutated variant unexpectedly passed:\n%s", stdout.String())
			}
			if stdout.Len() != 0 || strings.Contains(stdout.String()+stderr.String(), "PASS") {
				t.Errorf("failure output must stay on stderr without PASS:\nstdout=%q stderr=%q",
					stdout.String(), stderr.String())
			}
			errOut := stderr.String()
			if !strings.Contains(errOut, tc.wantCat) {
				t.Errorf("stderr must carry category %q:\n%s", tc.wantCat, errOut)
			}
			if !strings.Contains(errOut, tc.wantLoc) {
				t.Errorf("stderr must locate %q:\n%s", tc.wantLoc, errOut)
			}
			if !strings.Contains(errOut, testProfileID) || !strings.Contains(errOut, "("+tc.goos+", amd64)") {
				t.Errorf("stderr must print the profile and target header:\n%s", errOut)
			}
		})
	}
}

// TestPolicySwapDeferredToM10 记录 MAA-09/10 的任务边界:R/O 策略与
// 锁定 profile 独立期望的比对属 MAA-10;required↔optional 互换不构成
// 关联或形式失败,本变体在此跳过,由 MAA-10 的期望比对接管。
func TestPolicySwapDeferredToM10(t *testing.T) {
	t.Skip("MAA-09 不实现 R/O 策略与锁定 profile 期望的比对;required↔optional 互换反例由 MAA-10 接管")
}

// TestRealNativeSelectionBaseline 真实目录在两个目标下经完整命令入口
// 均可成功(合成 c 输入),作为变体测试的正例基线。
func TestRealNativeSelectionBaseline(t *testing.T) {
	cFiles := positiveFiles(t)
	registerTestProfile(t, testProfileFromFiles(cFiles))
	cInput := writeCInputDir(t, cFiles, nil)
	for _, goos := range []string{"linux", "windows"} {
		var stdout, stderr bytes.Buffer
		code := Run([]string{
			"--profile", testProfileID, "--goos", goos, "--goarch", "amd64",
			"--c-input", cInput, "--go-root", repoRoot(t),
		}, &stdout, &stderr)
		if code != 0 {
			t.Fatalf("%s baseline unexpectedly failed:\n%s", goos, stderr.String())
		}
		var report reportJSON
		if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
			t.Fatalf("%s baseline stdout is not JSON: %v", goos, err)
		}
		want := realNativeNames(t)
		// 平台条件文件按目标切换:linux 不含 loader_other.go,windows
		// 不含 loader_linux.go(独立期望,不复用选择逻辑)。
		switch goos {
		case "linux":
			want = withoutName(want, "loader_other.go")
		case "windows":
			want = withoutName(want, "loader_linux.go")
		}
		if strings.Join(report.Go.Files, ",") != strings.Join(want, ",") {
			t.Errorf("%s selected files = %v, want %v", goos, report.Go.Files, want)
		}
	}
}

// withoutName 返回去掉指定文件名的副本。
func withoutName(names []string, drop string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		if n != drop {
			out = append(out, n)
		}
	}
	return out
}
