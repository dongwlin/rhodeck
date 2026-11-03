package native

import (
	"fmt"
	"reflect"
	"runtime"

	"github.com/ebitengine/purego"
)

type symbolRegistrar interface {
	registerFunc(target any, addr uintptr)
}

type puregoRegistrar struct{}

func (puregoRegistrar) registerFunc(target any, addr uintptr) {
	purego.RegisterFunc(target, addr)
}

type Capabilities struct {
	create            bool
	imageBgr          bool
	asyncAttachWindow bool
	mapLevelKey       bool
	itemName          bool
}

// Create 报告 AsstCreate 是否在场。
func (c Capabilities) Create() bool { return c.create }

// ImageBgr 报告 AsstGetImageBgr 是否在场。
func (c Capabilities) ImageBgr() bool { return c.imageBgr }

// AsyncAttachWindow 报告 AsstAsyncAttachWindow 是否在场；非 Windows 平台恒为 false。
func (c Capabilities) AsyncAttachWindow() bool { return c.asyncAttachWindow }

// MapLevelKey 报告 AsstGetMapLevelKey 是否在场。
func (c Capabilities) MapLevelKey() bool { return c.mapLevelKey }

// ItemName 报告 AsstGetItemName 是否在场。
func (c Capabilities) ItemName() bool { return c.itemName }

func CapabilitiesForTest(create, imageBgr, asyncAttachWindow, mapLevelKey, itemName bool) Capabilities {
	return Capabilities{
		create:            create,
		imageBgr:          imageBgr,
		asyncAttachWindow: asyncAttachWindow,
		mapLevelKey:       mapLevelKey,
		itemName:          itemName,
	}
}

type APITable struct {
	api  *api
	caps Capabilities
}

func (t *APITable) funcs() *api { return t.api }

func (t *APITable) Capabilities() Capabilities { return t.caps }

func ResolveAndRegister(lib *Library) (*APITable, error) {
	return resolveAndRegister(lib, puregoRegistrar{})
}

func resolveAndRegister(lib *Library, reg symbolRegistrar) (*APITable, error) {
	table := new(api)
	caps := Capabilities{}
	specs := symbolSpecs(table)

	addrs := make([]uintptr, len(specs))
	resolved := make([]bool, len(specs))

	var failures []error
	for i, spec := range specs {
		if !specApplicable(spec) {
			continue
		}
		addr, err := lib.Lookup(spec.Name)
		if err != nil {
			if spec.Policy == required {
				failures = append(failures, err)
			}
			continue
		}
		addrs[i] = addr
		resolved[i] = true
		if spec.Policy == optional {
			switch spec.Name {
			case "AsstCreate":
				caps.create = true
			case "AsstGetImageBgr":
				caps.imageBgr = true
			case "AsstAsyncAttachWindow":
				caps.asyncAttachWindow = true
			case "AsstGetMapLevelKey":
				caps.mapLevelKey = true
			case "AsstGetItemName":
				caps.itemName = true
			}
		}
	}
	if len(failures) > 0 {
		return nil, AggregateError(failures)
	}

	for i, spec := range specs {
		if !specApplicable(spec) || !resolved[i] {
			continue
		}
		if err := registerOne(reg, spec, addrs[i]); err != nil {
			return nil, err
		}
	}

	return &APITable{api: table, caps: caps}, nil
}

func specApplicable(spec symbolSpec) bool {
	return spec.Platform == allPlatforms ||
		(spec.Platform == windowsOnly && runtime.GOOS == "windows")
}

func registerOne(reg symbolRegistrar, spec symbolSpec, addr uintptr) (err error) {
	signature := reflect.TypeOf(spec.Target).Elem().String()
	defer func() {
		if r := recover(); r != nil {
			err = registrationPanicError(spec.Name, signature, r)
		}
	}()
	reg.registerFunc(spec.Target, addr)
	return nil
}

func registrationPanicError(symbol, signature string, recovered any) error {
	var cause error
	if e, ok := recovered.(error); ok {
		cause = e
	} else {
		cause = fmt.Errorf("panic recovered during registration: %v", recovered)
	}
	return &FunctionRegistrationError{
		Symbol:    symbol,
		Signature: signature,
		Err:       cause,
	}
}
