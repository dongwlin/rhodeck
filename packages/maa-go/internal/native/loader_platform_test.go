//go:build darwin || windows

package native

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/ebitengine/purego"
)

// Exercise the production loader against an OS library without requiring a
// release artifact or compiler. Do not stat the Darwin path: libSystem can be
// present only in the dyld shared cache.
func TestNativeSystemLoader(t *testing.T) {
	path, symbol := systemLoaderProbe()
	ldr := NewLoader()
	if ldr.profile != abiProfile {
		t.Fatalf("loader profile = %q, want %q", ldr.profile, abiProfile)
	}
	lib, err := ldr.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if lib.Path() != path || lib.Name() != filepath.Base(path) {
		t.Fatalf("library path/name = %q/%q", lib.Path(), lib.Name())
	}
	addr, err := lib.Lookup(symbol)
	if err != nil {
		t.Fatal(err)
	}
	var processID func() uint32
	purego.RegisterFunc(&processID, addr)
	if got := processID(); got != uint32(os.Getpid()) {
		t.Fatalf("%s() = %d, want %d", symbol, got, os.Getpid())
	}
	const absent = "RhodeckNonexistentLoaderSymbol"
	addr, err = lib.Lookup(absent)
	var lookupErr *SymbolLookupError
	if addr != 0 || !errors.As(err, &lookupErr) {
		t.Fatalf("Lookup(absent) = %#x, %v", addr, err)
	}
	if lookupErr.LibraryPath != path || lookupErr.Symbol != absent || lookupErr.Profile != abiProfile || lookupErr.Err == nil {
		t.Fatalf("incomplete symbol error: %+v", lookupErr)
	}
	if errors.Is(err, ErrUnsupported) {
		t.Fatal("production lookup still uses unsupported primitives")
	}
}

func TestNativeSystemLoaderFailures(t *testing.T) {
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid-library")
	if err := os.WriteFile(invalid, []byte("not a native library"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing-library"), invalid} {
		t.Run(filepath.Base(path), func(t *testing.T) {
			lib, err := NewLoader().Open(path)
			var loadErr *LibraryLoadError
			if lib != nil || !errors.As(err, &loadErr) {
				t.Fatalf("Open(%q) = %v, %v", path, lib, err)
			}
			if loadErr.Path != path || loadErr.GOOS != runtime.GOOS || loadErr.GOARCH != runtime.GOARCH || loadErr.Err == nil {
				t.Fatalf("incomplete library error: %+v", loadErr)
			}
			if errors.Is(err, ErrUnsupported) || errors.Is(err, ErrInvalidInput) {
				t.Fatalf("system failure misclassified: %v", err)
			}
		})
	}
}
