//go:build windows

package native

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func systemLoaderProbe() (string, string) {
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "kernel32.dll"), "GetCurrentProcessId"
}

func TestWindowsLoadFlags(t *testing.T) {
	if windowsLoadFlags != 0x900 {
		t.Fatalf("windowsLoadFlags = %#x, want DLL_LOAD_DIR|SYSTEM32 (0x900)", windowsLoadFlags)
	}
}

func TestWindowsLoaderPreservesSystemErrors(t *testing.T) {
	_, err := NewLoader().Open(filepath.Join(t.TempDir(), "missing.dll"))
	if !errors.Is(err, syscall.ERROR_MOD_NOT_FOUND) {
		t.Fatalf("missing DLL error = %v, want ERROR_MOD_NOT_FOUND", err)
	}
	path, _ := systemLoaderProbe()
	lib, err := NewLoader().Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = lib.Lookup("RhodeckNonexistentLoaderSymbol")
	if !errors.Is(err, syscall.ERROR_PROC_NOT_FOUND) {
		t.Fatalf("missing symbol error = %v, want ERROR_PROC_NOT_FOUND", err)
	}
}
