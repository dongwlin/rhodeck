package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRunAcceptsCallerInputWithoutVersionRegistry(t *testing.T) {
	files := positiveFiles(t)
	files[FileAsstCaller] += "\n// caller-selected header snapshot\n"
	dir := writeCInputDir(t, files, func(m *manifest) {
		m.SnapshotID = "caller-selected-snapshot"
		m.Source.Repository = "https://example.com/new-upstream"
		m.Source.Tag = "candidate-at-execution"
		m.Source.Commit = ""
	})
	code, out, errOut := mustRun(t, []string{"--goos", "linux", "--goarch", "amd64", "--c-input", dir, "--go-root", repoRoot(t)})
	require.Equal(t, 0, code, "caller-selected input rejected: %s", errOut)
	var report reportJSON
	require.NoError(t, json.Unmarshal([]byte(out), &report))
	require.False(t, report.Input.SnapshotID != "caller-selected-snapshot" ||
		report.Input.Source.Tag != "candidate-at-execution" || len(report.Input.Files) != 3,
		"input provenance missing from report: %+v", report.Input)
	for _, file := range report.Input.Files {
		require.False(t, len(file.SHA256) != 64 || file.SizeBytes <= 0,
			"input digest missing: %+v", file)
	}
}

func TestRunRejectsIncompleteInputProvenance(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*manifest)
		symbol string
	}{
		{"identity", func(m *manifest) { m.SnapshotID = "" }, "snapshot_id"},
		{"repository", func(m *manifest) { m.Source.Repository = "" }, "source.repository"},
		{"revision", func(m *manifest) { m.Source.Tag = ""; m.Source.Commit = "" }, "source"},
		{"invalid digest", func(m *manifest) { m.Files[0].SHA256 = "not-a-digest" }, "sha256"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := writeCInputDir(t, positiveFiles(t), tc.mutate)
			code, out, errOut := mustRun(t, []string{"--goos", "linux", "--goarch", "amd64", "--c-input", dir, "--go-root", repoRoot(t)})
			require.False(t, code == 0 || out != "" || !strings.Contains(errOut, tc.symbol),
				"invalid input admitted: code=%d stdout=%q stderr=%q", code, out, errOut)
		})
	}
}
