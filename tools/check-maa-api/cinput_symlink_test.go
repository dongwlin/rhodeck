//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestLoadCInputRejectsSymlinkEscape(t *testing.T) {
	files := positiveFiles(t)
	dir := writeCInputDir(t, files, nil)
	outside := filepath.Join(t.TempDir(), "AsstCaller.h")
	require.NoError(t, os.WriteFile(outside, []byte(files[FileAsstCaller]), 0600))
	path := filepath.Join(dir, filepath.FromSlash(FileAsstCaller))
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.Symlink(outside, path), "create symlink")
	_, diags := LoadCInput(dir)
	require.True(t, len(diags) != 0 && strings.Contains(diags[0].Message, "escapes"),
		"escaped input admitted: %v", diags)
}
