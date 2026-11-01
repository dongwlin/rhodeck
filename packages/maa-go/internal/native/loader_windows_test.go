//go:build windows

package native

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func systemLoaderProbe() (string, string) {
	return filepath.Join(os.Getenv("SystemRoot"), "System32", "kernel32.dll"), "GetCurrentProcessId"
}

func TestWindowsLoadFlags(t *testing.T) {
	require.Equal(t, 0x900, windowsLoadFlags, "windowsLoadFlags = %#x, want DLL_LOAD_DIR|SYSTEM32 (0x900)", windowsLoadFlags)
}

func TestWindowsLoaderPreservesSystemErrors(t *testing.T) {
	_, err := NewLoader().Open(filepath.Join(t.TempDir(), "missing.dll"))
	require.ErrorIs(t, err, syscall.ERROR_MOD_NOT_FOUND, "missing DLL error = %v, want ERROR_MOD_NOT_FOUND", err)
	path, _ := systemLoaderProbe()
	lib, err := NewLoader().Open(path)
	require.NoError(t, err)
	_, err = lib.Lookup("RhodeckNonexistentLoaderSymbol")
	require.ErrorIs(t, err, syscall.ERROR_PROC_NOT_FOUND, "missing symbol error = %v, want ERROR_PROC_NOT_FOUND", err)
}
