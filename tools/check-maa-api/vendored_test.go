// 本文件对仓库内有来源的未版本化头快照执行两目标静态检查。

package main

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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
			require.Equal(t, 0, code, "Run(%s) exit = %d, want 0; stderr:\n%s", tc.goos, code, stderr.String())
			var rep reportJSON
			err := json.Unmarshal(stdout.Bytes(), &rep)
			require.NoError(t, err, "decode report: %v", err)
			require.Equal(t, "maacore-headers", rep.Input.SnapshotID,
				"profile = %q, want maacore-headers", rep.Input.SnapshotID)
			require.True(t, rep.Target.GOOS == tc.goos && rep.Target.GOARCH == "amd64",
				"target = %s/%s, want %s/amd64", rep.Target.GOOS, rep.Target.GOARCH, tc.goos)
			require.Equal(t, tc.bound, len(rep.Link.Bound), "bound = %d, want %d", len(rep.Link.Bound), tc.bound)
			require.Equal(t, tc.nonTarget, len(rep.Link.NonTarget),
				"nontarget bindings = %d, want %d", len(rep.Link.NonTarget), tc.nonTarget)
			require.Equal(t, 2, len(rep.Link.Excluded), "excluded = %d, want 2", len(rep.Link.Excluded))
			require.True(t, len(rep.Signatures.Symbols) == 28 && len(rep.Signatures.Callbacks) == 2,
				"signatures = %d symbols/%d callbacks, want 28/2",
				len(rep.Signatures.Symbols), len(rep.Signatures.Callbacks))
			require.Equal(t, 30, len(rep.Expect.Symbols), "expect symbols = %d, want 30", len(rep.Expect.Symbols))
		})
	}
}
