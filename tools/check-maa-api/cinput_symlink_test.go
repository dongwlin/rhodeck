//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadCInputRejectsSymlinkEscape(t *testing.T) {
	files := positiveFiles(t)
	dir := writeCInputDir(t, files, nil)
	outside := filepath.Join(t.TempDir(), "AsstCaller.h")
	if err := os.WriteFile(outside, []byte(files[FileAsstCaller]), 0600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, filepath.FromSlash(FileAsstCaller))
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	_, diags := LoadCInput(dir)
	if len(diags) == 0 || !strings.Contains(diags[0].Message, "escapes") {
		t.Fatalf("escaped input admitted: %v", diags)
	}
}
