//go:build darwin || windows

package native

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ebitengine/purego"

	"github.com/stretchr/testify/require"
)

// Exercise the production loader against an OS library without requiring a
// release artifact or compiler. Do not stat the Darwin path: libSystem can be
// present only in the dyld shared cache.
func TestNativeSystemLoader(t *testing.T) {
	path, symbol := systemLoaderProbe()
	ldr := NewLoader()
	require.Equal(t, abiProfile, ldr.profile, "loader profile = %q, want %q", ldr.profile, abiProfile)
	lib, err := ldr.Open(path)
	require.NoError(t, err)
	require.True(t, lib.Path() == path && lib.Name() == filepath.Base(path),
		"library path/name = %q/%q", lib.Path(), lib.Name())
	addr, err := lib.Lookup(symbol)
	require.NoError(t, err)
	var processID func() uint32
	purego.RegisterFunc(&processID, addr)
	got := processID()
	require.Equal(t, uint32(os.Getpid()), got, "%s() = %d, want %d", symbol, got, os.Getpid())
	const absent = "RhodeckNonexistentLoaderSymbol"
	addr, err = lib.Lookup(absent)
	var lookupErr *SymbolLookupError
	require.Equal(t, uintptr(0), addr, "Lookup(absent) = %#x, %v", addr, err)
	require.ErrorAs(t, err, &lookupErr, "Lookup(absent) = %#x, %v", addr, err)
	require.True(t, lookupErr.LibraryPath == path && lookupErr.Symbol == absent && lookupErr.Profile == abiProfile && lookupErr.Err != nil,
		"incomplete symbol error: %+v", lookupErr)
	require.NotErrorIs(t, err, ErrUnsupported, "production lookup still uses unsupported primitives")
}

func TestNativeSystemLoaderFailures(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid-library")
	err := os.WriteFile(invalid, []byte("not a native library"), 0o600)
	require.NoError(t, err)
	for _, path := range []string{filepath.Join(dir, "missing-library"), invalid} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			lib, err := NewLoader().Open(path)
			var loadErr *LibraryLoadError
			require.Nil(t, lib, "Open(%q) = %v, %v", path, lib, err)
			require.ErrorAs(t, err, &loadErr, "Open(%q) = %v, %v", path, lib, err)
			require.True(t, loadErr.Path == path && loadErr.GOOS == runtime.GOOS && loadErr.GOARCH == runtime.GOARCH && loadErr.Err != nil,
				"incomplete library error: %+v", loadErr)
			require.NotErrorIs(t, err, ErrUnsupported, "system failure misclassified: %v", err)
			require.NotErrorIs(t, err, ErrInvalidInput, "system failure misclassified: %v", err)
		})
	}
}
