package native

import (
	"errors"
	"fmt"
	"strings"
)

var (
	ErrClosed = errors.New("maa: instance or runtime is closed")

	ErrClosing = errors.New("maa: close in progress")

	ErrRuntimeInUse = errors.New("maa: runtime already reserved or active in this process")

	ErrRuntimeTerminated = errors.New("maa: runtime terminated for this process")

	ErrUnsupported = errors.New("maa: not supported on this platform or capability set")

	ErrInvalidInput = errors.New("maa: invalid input")

	ErrNativeRejected = errors.New("maa: native call returned failure")

	ErrBufferLimit = errors.New("maa: output buffer capacity probing exhausted")

	ErrEventOverflow = errors.New("maa: event queue capacity exceeded")

	ErrOperationLimit = errors.New("maa: pending operation limit exceeded")

	ErrProtocol = errors.New("maa: binding protocol violation")
)

type LibraryLoadError struct {
	LibraryName string
	Path        string
	GOOS        string
	GOARCH      string
	Err         error
}

func (e *LibraryLoadError) Error() string {
	return fmt.Sprintf("maa: load library %q at %s for %s/%s failed: %v",
		e.LibraryName, e.Path, e.GOOS, e.GOARCH, e.Err)
}

func (e *LibraryLoadError) Unwrap() error { return e.Err }

type SymbolLookupError struct {
	LibraryPath string
	Symbol      string
	Profile     string
	Err         error
}

func (e *SymbolLookupError) Error() string {
	return fmt.Sprintf("maa: symbol %q not found in %s (profile %s): %v",
		e.Symbol, e.LibraryPath, e.Profile, e.Err)
}

func (e *SymbolLookupError) Unwrap() error { return e.Err }

type FunctionRegistrationError struct {
	Symbol    string
	Signature string
	Err       error
}

func (e *FunctionRegistrationError) Error() string {
	return fmt.Sprintf("maa: register function %q with signature %s failed: %v",
		e.Symbol, e.Signature, e.Err)
}

func (e *FunctionRegistrationError) Unwrap() error { return e.Err }

type AggregateError []error

func (e AggregateError) Error() string {
	if len(e) == 0 {
		return "maa: aggregate error (0 items)"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "maa: %d errors occurred:", len(e))
	for i, err := range e {
		fmt.Fprintf(&b, "\n\t[%d] %v", i+1, err)
	}
	return b.String()
}

func (e AggregateError) Unwrap() []error { return e }
