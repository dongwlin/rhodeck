// main_test.go 覆盖完整命令入口 Run:合成 profile + 真实仓库 go-root
// 的端到端成功(两目标、确定性输出),以及参数解析、profile/目标、
// c 输入与 go 根目录各层失败的端到端行为(非零退出、stderr 类别与
// 定位、绝不输出 PASS)。测试 profile 注入只存在于本 _test.go 可达
// 的注册表接口。

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// repoRoot 经 runtime.Caller 定位仓库根,不硬编码任何用户绝对路径。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate the test source file")
	}
	// thisFile = <root>/tools/check-maa-api/main_test.go
	return filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
}

// mustRun 执行一次 Run 并返回退出码与输出。
func mustRun(t *testing.T, args []string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := Run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestRunSuccessBothTargets 端到端成功:合成 c 输入 + 真实 go-root,
// 两个目标的退出码、JSON 结构与分类表全部符合预期。
func TestRunSuccessBothTargets(t *testing.T) {
	files := positiveFiles(t)
	registerTestProfile(t, testProfileFromFiles(files))
	dir := writeCInputDir(t, files, nil)
	goRoot := repoRoot(t)

	type result struct {
		code int
		out  string
		err  string
	}
	run := func(goos string) result {
		code, out, errOut := mustRun(t, []string{
			"--profile", testProfileID, "--goos", goos, "--goarch", "amd64",
			"--c-input", dir, "--go-root", goRoot,
		})
		return result{code, out, errOut}
	}

	linux := run("linux")
	if linux.code != 0 {
		t.Fatalf("linux run exited %d:\n%s", linux.code, linux.err)
	}
	if strings.Contains(linux.out, "PASS") || linux.err != "" {
		t.Errorf("linux output must be JSON only without PASS:\n%s\n%s", linux.out, linux.err)
	}
	var report reportJSON
	if err := json.Unmarshal([]byte(linux.out), &report); err != nil {
		t.Fatalf("linux stdout is not valid JSON: %v\n%s", err, linux.out)
	}
	if report.Profile != testProfileID || report.Target.GOOS != "linux" || report.Target.GOARCH != "amd64" {
		t.Errorf("report header = %+v", report.Target)
	}
	if len(report.C.Full) != 30 || len(report.C.Discovery) != 2 {
		t.Errorf("full=%d discovery=%d, want 30/2", len(report.C.Full), len(report.C.Discovery))
	}
	if len(report.C.Applicable) != 26 {
		t.Errorf("linux applicable = %d, want 26", len(report.C.Applicable))
	}
	for _, name := range []string{"AsstAttachWindow", "AsstAsyncAttachWindow"} {
		for _, s := range report.C.Applicable {
			if s == name {
				t.Errorf("%s must not be applicable on linux", name)
			}
		}
	}
	if len(report.Go.Fields) != 28 || len(report.Go.Specs) != 28 || len(report.Go.Exclusions) != 2 {
		t.Errorf("go summary = fields %d specs %d exclusions %d, want 28/28/2",
			len(report.Go.Fields), len(report.Go.Specs), len(report.Go.Exclusions))
	}
	if report.Go.Callbacks["unixCallback"].Result != "" ||
		report.Go.Callbacks["windowsCallback"].Result != "uintptr" {
		t.Errorf("callbacks = %+v", report.Go.Callbacks)
	}
	if len(report.Link.Bound) != 27 || len(report.Link.Excluded) != 2 {
		t.Errorf("linux link = bound %d excluded %d, want 27/2",
			len(report.Link.Bound), len(report.Link.Excluded))
	}
	if strings.Join(report.Link.NonTarget, ",") != "AsstAsyncAttachWindow" {
		t.Errorf("linux nontarget = %v", report.Link.NonTarget)
	}

	windows := run("windows")
	if windows.code != 0 {
		t.Fatalf("windows run exited %d:\n%s", windows.code, windows.err)
	}
	var winReport reportJSON
	if err := json.Unmarshal([]byte(windows.out), &winReport); err != nil {
		t.Fatalf("windows stdout is not valid JSON: %v", err)
	}
	if len(winReport.C.Applicable) != 28 || len(winReport.Link.Bound) != 28 {
		t.Errorf("windows applicable=%d bound=%d, want 28/28",
			len(winReport.C.Applicable), len(winReport.Link.Bound))
	}
	if len(winReport.Link.NonTarget) != 0 {
		t.Errorf("windows nontarget = %v, want empty", winReport.Link.NonTarget)
	}
	for _, name := range []string{"AsstAttachWindow", "AsstAsyncAttachWindow"} {
		found := false
		for _, s := range winReport.C.Applicable {
			if s == name {
				found = true
			}
		}
		if !found {
			t.Errorf("%s must be applicable on windows", name)
		}
	}
	// 完整集合与发现与目标无关。
	if strings.Join(report.C.Full, ",") != strings.Join(winReport.C.Full, ",") ||
		strings.Join(report.C.Discovery, ",") != strings.Join(winReport.C.Discovery, ",") {
		t.Errorf("full set/discovery must be identical across targets")
	}

	// 确定性:同参数两次运行输出逐字节一致。
	again := run("linux")
	if again.out != linux.out {
		t.Error("success output must be deterministic across runs")
	}
}

// TestRunArgumentFailures 参数层失败:缺 flag、未知 flag、多余位置
// 参数、未知 profile、未知目标组合,全部非零退出且不输出 PASS。
func TestRunArgumentFailures(t *testing.T) {
	files := positiveFiles(t)
	registerTestProfile(t, testProfileFromFiles(files))
	dir := writeCInputDir(t, files, nil)
	goRoot := repoRoot(t)
	base := []string{"--profile", testProfileID, "--goos", "linux", "--goarch", "amd64",
		"--c-input", dir, "--go-root", goRoot}

	cases := []struct {
		name string
		args []string
		want []string // stderr 必含片段
	}{
		{"no arguments", nil, []string{"input", "missing"}},
		{"unknown flag", append(append([]string{}, base...), "--bogus"), []string{"input", "bogus"}},
		{"extra positional argument", append(append([]string{}, base...), "extra"),
			[]string{"input", "positional"}},
		{"unknown profile", []string{"--profile", "maa-v9.9.9", "--goos", "linux", "--goarch", "amd64",
			"--c-input", dir, "--go-root", goRoot}, []string{"input", "maa-v9.9.9"}},
		{"unknown goos", []string{"--profile", testProfileID, "--goos", "darwin", "--goarch", "amd64",
			"--c-input", dir, "--go-root", goRoot}, []string{"input", "darwin"}},
		{"unknown goarch", []string{"--profile", testProfileID, "--goos", "linux", "--goarch", "arm64",
			"--c-input", dir, "--go-root", goRoot}, []string{"input", "arm64"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := mustRun(t, tc.args)
			if code == 0 {
				t.Fatalf("expected nonzero exit, stdout:\n%s", out)
			}
			if out != "" {
				t.Errorf("failure must not write to stdout, got:\n%s", out)
			}
			if strings.Contains(out+errOut, "PASS") {
				t.Error("failure output must never contain PASS")
			}
			for _, want := range tc.want {
				if !strings.Contains(errOut, want) {
					t.Errorf("stderr = %q, want it to contain %q", errOut, want)
				}
			}
		})
	}
}

// TestRunInputFailures 端到端输入失败:c 输入缺必需文件、缺清单、
// go-root 缺 native 目录;stderr 带类别与定位。
func TestRunInputFailures(t *testing.T) {
	files := positiveFiles(t)
	registerTestProfile(t, testProfileFromFiles(files))
	goRoot := repoRoot(t)

	t.Run("missing AsstPort.h on disk", func(t *testing.T) {
		dir := writeCInputDir(t, files, nil)
		if err := os.Remove(filepath.Join(dir, filepath.FromSlash(FileAsstPort))); err != nil {
			t.Fatal(err)
		}
		code, out, errOut := mustRun(t, []string{
			"--profile", testProfileID, "--goos", "linux", "--goarch", "amd64",
			"--c-input", dir, "--go-root", goRoot,
		})
		if code == 0 || out != "" || strings.Contains(out+errOut, "PASS") {
			t.Fatalf("expected failed run without PASS: code=%d out=%q err=%q", code, out, errOut)
		}
		if !strings.Contains(errOut, "input") || !strings.Contains(errOut, "AsstPort.h") {
			t.Errorf("stderr must carry the input category and file location:\n%s", errOut)
		}
	})
	t.Run("missing inputs.json", func(t *testing.T) {
		code, out, errOut := mustRun(t, []string{
			"--profile", testProfileID, "--goos", "linux", "--goarch", "amd64",
			"--c-input", t.TempDir(), "--go-root", goRoot,
		})
		if code == 0 || out != "" || strings.Contains(out+errOut, "PASS") {
			t.Fatalf("expected failed run without PASS: code=%d out=%q err=%q", code, out, errOut)
		}
		if !strings.Contains(errOut, "inputs.json") {
			t.Errorf("stderr must locate inputs.json:\n%s", errOut)
		}
	})
	t.Run("go-root without native package", func(t *testing.T) {
		dir := writeCInputDir(t, files, nil)
		code, out, errOut := mustRun(t, []string{
			"--profile", testProfileID, "--goos", "linux", "--goarch", "amd64",
			"--c-input", dir, "--go-root", t.TempDir(),
		})
		if code == 0 || out != "" || strings.Contains(out+errOut, "PASS") {
			t.Fatalf("expected failed run without PASS: code=%d out=%q err=%q", code, out, errOut)
		}
		if !strings.Contains(errOut, goNativeRelPath) {
			t.Errorf("stderr must mention the frozen relative path:\n%s", errOut)
		}
	})
}
