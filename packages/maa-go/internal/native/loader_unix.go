//go:build linux || darwin

package native

import "github.com/ebitengine/purego"

const loadFlags = purego.RTLD_NOW | purego.RTLD_LOCAL

type dlopenPrimitives struct{}

func (dlopenPrimitives) open(path string) (uintptr, error) {
	return purego.Dlopen(path, loadFlags)
}

func (dlopenPrimitives) lookup(handle uintptr, name string) (uintptr, error) {
	return purego.Dlsym(handle, name)
}

func NewLoader() *Loader {
	return newLoader(dlopenPrimitives{}, abiProfile)
}
