// 本文件对仓库内有来源的未版本化头快照执行两目标静态检查。

package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestVendoredInputRun(t *testing.T) {
	root := repoRoot(t)
	input := filepath.Join(root, "tools", "check-maa-api", "testdata", "snapshot")
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
			if rep.Input.SnapshotID != "maacore-headers" {
				t.Fatalf("profile = %q, want maacore-headers", rep.Input.SnapshotID)
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
