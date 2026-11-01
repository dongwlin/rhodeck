// 本文件覆盖完整命令入口的显式 C 输入、Go 声明、输出和失败诊断。

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// repoRoot 经 runtime.Caller 定位仓库根,不硬编码任何用户绝对路径。
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "cannot locate the test source file")
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
			"--goos", goos, "--goarch", "amd64",
			"--c-input", dir, "--go-root", goRoot,
		})
		return result{code, out, errOut}
	}

	linux := run("linux")
	require.Equal(t, 0, linux.code, "linux run exited %d:\n%s", linux.code, linux.err)
	assert.False(t, strings.Contains(linux.out, "PASS") || linux.err != "",
		"linux output must be JSON only without PASS:\n%s\n%s", linux.out, linux.err)
	var report reportJSON
	require.NoError(t, json.Unmarshal([]byte(linux.out), &report), "linux stdout is not valid JSON:\n%s", linux.out)
	assert.False(t, report.Input.SnapshotID != testProfileID || report.Target.GOOS != "linux" || report.Target.GOARCH != "amd64",
		"report header = %+v", report.Target)
	assert.False(t, len(report.C.Full) != 30 || len(report.C.Discovery) != 2,
		"full=%d discovery=%d, want 30/2", len(report.C.Full), len(report.C.Discovery))
	assert.Len(t, report.C.Applicable, 26, "linux applicable = %d, want 26", len(report.C.Applicable))
	for _, name := range []string{"AsstAttachWindow", "AsstAsyncAttachWindow"} {
		assert.NotContains(t, report.C.Applicable, name, "%s must not be applicable on linux", name)
	}
	assert.False(t, len(report.Go.Fields) != 28 || len(report.Go.Specs) != 28 || len(report.Go.Exclusions) != 2,
		"go summary = fields %d specs %d exclusions %d, want 28/28/2",
		len(report.Go.Fields), len(report.Go.Specs), len(report.Go.Exclusions))
	assert.False(t, report.Go.Callbacks["unixCallback"].Result != "" ||
		report.Go.Callbacks["windowsCallback"].Result != "uintptr",
		"callbacks = %+v", report.Go.Callbacks)
	assert.False(t, len(report.Link.Bound) != 27 || len(report.Link.Excluded) != 2,
		"linux link = bound %d excluded %d, want 27/2",
		len(report.Link.Bound), len(report.Link.Excluded))
	assert.Equal(t, "AsstAsyncAttachWindow", strings.Join(report.Link.NonTarget, ","),
		"linux nontarget = %v", report.Link.NonTarget)

	windows := run("windows")
	require.Equal(t, 0, windows.code, "windows run exited %d:\n%s", windows.code, windows.err)
	var winReport reportJSON
	require.NoError(t, json.Unmarshal([]byte(windows.out), &winReport), "windows stdout is not valid JSON")
	assert.False(t, len(winReport.C.Applicable) != 28 || len(winReport.Link.Bound) != 28,
		"windows applicable=%d bound=%d, want 28/28",
		len(winReport.C.Applicable), len(winReport.Link.Bound))
	assert.Empty(t, winReport.Link.NonTarget, "windows nontarget = %v, want empty", winReport.Link.NonTarget)
	for _, name := range []string{"AsstAttachWindow", "AsstAsyncAttachWindow"} {
		assert.Contains(t, winReport.C.Applicable, name, "%s must be applicable on windows", name)
	}
	// 完整集合与发现与目标无关。
	assert.False(t, strings.Join(report.C.Full, ",") != strings.Join(winReport.C.Full, ",") ||
		strings.Join(report.C.Discovery, ",") != strings.Join(winReport.C.Discovery, ","),
		"full set/discovery must be identical across targets")

	// 确定性:同参数两次运行输出逐字节一致。
	again := run("linux")
	assert.Equal(t, linux.out, again.out, "success output must be deterministic across runs")
}

// TestRunArgumentFailures 参数层失败:缺 flag、未知 flag、多余位置
// 参数、未知 profile、未知目标组合,全部非零退出且不输出 PASS。
func TestRunArgumentFailures(t *testing.T) {
	files := positiveFiles(t)
	registerTestProfile(t, testProfileFromFiles(files))
	dir := writeCInputDir(t, files, nil)
	goRoot := repoRoot(t)
	base := []string{"--goos", "linux", "--goarch", "amd64",
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
		{"removed profile flag", []string{"--profile", "maa-v9.9.9", "--goos", "linux", "--goarch", "amd64",
			"--c-input", dir, "--go-root", goRoot}, []string{"input", "profile"}},
		{"unknown goos", []string{"--goos", "darwin", "--goarch", "amd64",
			"--c-input", dir, "--go-root", goRoot}, []string{"input", "darwin"}},
		{"unknown goarch", []string{"--goos", "linux", "--goarch", "arm64",
			"--c-input", dir, "--go-root", goRoot}, []string{"input", "arm64"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, out, errOut := mustRun(t, tc.args)
			require.NotEqual(t, 0, code, "expected nonzero exit, stdout:\n%s", out)
			assert.Empty(t, out, "failure must not write to stdout, got:\n%s", out)
			assert.NotContains(t, out+errOut, "PASS", "failure output must never contain PASS")
			for _, want := range tc.want {
				assert.Contains(t, errOut, want, "stderr = %q, want it to contain %q", errOut, want)
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
		require.NoError(t, os.Remove(filepath.Join(dir, filepath.FromSlash(FileAsstPort))))
		code, out, errOut := mustRun(t, []string{
			"--goos", "linux", "--goarch", "amd64",
			"--c-input", dir, "--go-root", goRoot,
		})
		require.False(t, code == 0 || out != "" || strings.Contains(out+errOut, "PASS"),
			"expected failed run without PASS: code=%d out=%q err=%q", code, out, errOut)
		assert.True(t, strings.Contains(errOut, "input") && strings.Contains(errOut, "AsstPort.h"),
			"stderr must carry the input category and file location:\n%s", errOut)
	})
	t.Run("missing inputs.json", func(t *testing.T) {
		code, out, errOut := mustRun(t, []string{
			"--goos", "linux", "--goarch", "amd64",
			"--c-input", t.TempDir(), "--go-root", goRoot,
		})
		require.False(t, code == 0 || out != "" || strings.Contains(out+errOut, "PASS"),
			"expected failed run without PASS: code=%d out=%q err=%q", code, out, errOut)
		assert.Contains(t, errOut, "inputs.json", "stderr must locate inputs.json:\n%s", errOut)
	})
	t.Run("go-root without native package", func(t *testing.T) {
		dir := writeCInputDir(t, files, nil)
		code, out, errOut := mustRun(t, []string{
			"--goos", "linux", "--goarch", "amd64",
			"--c-input", dir, "--go-root", t.TempDir(),
		})
		require.False(t, code == 0 || out != "" || strings.Contains(out+errOut, "PASS"),
			"expected failed run without PASS: code=%d out=%q err=%q", code, out, errOut)
		assert.Contains(t, errOut, goNativeRelPath, "stderr must mention the frozen relative path:\n%s", errOut)
	})
}
