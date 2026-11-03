//go:build linux

package native

import (
	"fmt"
	"sync"

	"github.com/ebitengine/purego"
)

func nativeCallback(msg int32, detailsJSON *byte, customArg uintptr) {
	defer func() {
		if r := recover(); r != nil {
			recordCallbackFault(callbackFault{
				Kind:   callbackFaultPanic,
				Msg:    msg,
				Token:  customArg,
				Detail: fmt.Sprintf("Go panic inside the callback recovered at the native boundary (no claim of recovering native crashes or C++ exceptions): %v", r),
			})
		}
	}()

	dispatchCallback(msg, detailsJSON, customArg)
}

var nativeCallbackTrampoline = sync.OnceValue(func() uintptr {
	return purego.NewCallback(nativeCallback)
})

func CallbackTrampoline() uintptr {
	return nativeCallbackTrampoline()
}

var _ unixCallback = nativeCallback
