//go:build !linux && !darwin && !windows

package native

import (
	"fmt"
	"runtime"
)

type unsupportedPrimitives struct{}

func (unsupportedPrimitives) open(path string) (uintptr, error) {
	return 0, fmt.Errorf("native loader is not implemented for %s/%s: %w",
		runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

func (unsupportedPrimitives) lookup(handle uintptr, name string) (uintptr, error) {
	return 0, fmt.Errorf("native loader is not implemented for %s/%s: %w",
		runtime.GOOS, runtime.GOARCH, ErrUnsupported)
}

func NewLoader() *Loader {
	return newLoader(unsupportedPrimitives{}, abiProfile)
}
