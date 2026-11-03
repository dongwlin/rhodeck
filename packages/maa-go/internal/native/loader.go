package native

import (
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf8"
)

const abiProfile = "maacore-c-api"

var errNullHandle = errors.New("native loader returned a null handle without an error")

var errSymbolNullAddress = errors.New("symbol resolved to a null address")

type loaderPrimitives interface {
	open(path string) (uintptr, error)

	lookup(handle uintptr, name string) (uintptr, error)
}

type Loader struct {
	prims   loaderPrimitives
	profile string
}

func newLoader(prims loaderPrimitives, profile string) *Loader {
	return &Loader{prims: prims, profile: profile}
}

func (ldr *Loader) Open(path string) (*Library, error) {
	normalized, verr := validateLibraryPath(path)
	if verr != nil {
		return nil, libraryLoadError(normalized, verr)
	}
	handle, err := ldr.prims.open(normalized)
	if err != nil {
		return nil, libraryLoadError(normalized, err)
	}
	if handle == 0 {
		return nil, libraryLoadError(normalized, errNullHandle)
	}
	return &Library{ldr: ldr, path: normalized, handle: handle}, nil
}

func validateLibraryPath(path string) (string, error) {
	normalized := ""
	if path != "" {
		normalized = filepath.Clean(path)
	}
	switch {
	case path == "":
		return normalized, fmt.Errorf("library path is empty: %w", ErrInvalidInput)
	case !utf8.ValidString(path):
		return normalized, fmt.Errorf("library path is not valid UTF-8: %w", ErrInvalidInput)
	case strings.IndexByte(path, '\x00') >= 0:
		return normalized, fmt.Errorf("library path contains an embedded NUL byte: %w", ErrInvalidInput)
	case !filepath.IsAbs(normalized):
		return normalized, fmt.Errorf("library path %q is not absolute: %w", normalized, ErrInvalidInput)
	}
	return normalized, nil
}

func libraryLoadError(path string, cause error) *LibraryLoadError {
	return &LibraryLoadError{
		LibraryName: libraryBaseName(path),
		Path:        path,
		GOOS:        runtime.GOOS,
		GOARCH:      runtime.GOARCH,
		Err:         cause,
	}
}

func libraryBaseName(path string) string {
	if path == "" {
		return ""
	}
	return filepath.Base(path)
}

type Library struct {
	ldr    *Loader
	path   string
	handle uintptr
}

func (lib *Library) Path() string { return lib.path }

func (lib *Library) Name() string { return libraryBaseName(lib.path) }

func (lib *Library) Lookup(name string) (uintptr, error) {
	addr, err := lib.ldr.prims.lookup(lib.handle, name)
	if err != nil {
		return 0, lib.lookupError(name, err)
	}
	if addr == 0 {
		return 0, lib.lookupError(name, errSymbolNullAddress)
	}
	return addr, nil
}

func (lib *Library) lookupError(name string, cause error) *SymbolLookupError {
	return &SymbolLookupError{
		LibraryPath: lib.path,
		Symbol:      name,
		Profile:     lib.ldr.profile,
		Err:         cause,
	}
}
