// 本文件验收仓库内的锁定 C 输入:不依赖用户缓存中的发行包,直接对
// tools/check-maa-api/testdata/locked 运行完整 checker 命令入口,并核
// 对两个目标的策略计数与签名记录。它使 CI 与离线环境也能执行真实的
// 锁定头比对,而不是只跑合成正例;发行包缺失时的 SKIP 仅影响
// realinput_linux_test.go 的归档来源测试,不影响本用例。

package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestVendoredLockedInputRun(t *testing.T) {
	root := repoRoot(t)
	input := filepath.Join(root, "tools", "check-maa-api", "testdata", "locked")
	tests := []struct {
		goos      string
		bound     int
		nonTarget int
	}{
		{goos: "linux", bound: 27, nonTarget: 1},
		{goos: "windows", bound: 28, nonTarget: 0},
	}
	for _, tc := range tests {
		t.Run(tc.goos, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := Run([]string{
				"--profile", "maa-v6.20.1",
				"--goos", tc.goos,
				"--goarch", "amd64",
				"--c-input", input,
				"--go-root", root,
			}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("Run(%s) exit = %d, want 0; stderr:\n%s", tc.goos, code, stderr.String())
			}
			var rep reportJSON
			if err := json.Unmarshal(stdout.Bytes(), &rep); err != nil {
				t.Fatalf("decode report: %v", err)
			}
			if rep.Profile != "maa-v6.20.1" {
				t.Fatalf("profile = %q, want maa-v6.20.1", rep.Profile)
			}
			if rep.Target.GOOS != tc.goos || rep.Target.GOARCH != "amd64" {
				t.Fatalf("target = %s/%s, want %s/amd64", rep.Target.GOOS, rep.Target.GOARCH, tc.goos)
			}
			if len(rep.Link.Bound) != tc.bound {
				t.Fatalf("bound = %d, want %d", len(rep.Link.Bound), tc.bound)
			}
			if len(rep.Link.NonTarget) != tc.nonTarget {
				t.Fatalf("nontarget bindings = %d, want %d", len(rep.Link.NonTarget), tc.nonTarget)
			}
			if len(rep.Link.Excluded) != 2 {
				t.Fatalf("excluded = %d, want 2", len(rep.Link.Excluded))
			}
			if len(rep.Signatures.Symbols) != 28 || len(rep.Signatures.Callbacks) != 2 {
				t.Fatalf("signatures = %d symbols/%d callbacks, want 28/2",
					len(rep.Signatures.Symbols), len(rep.Signatures.Callbacks))
			}
			if len(rep.Expect.Symbols) != 30 {
				t.Fatalf("expect symbols = %d, want 30", len(rep.Expect.Symbols))
			}
		})
	}
}
