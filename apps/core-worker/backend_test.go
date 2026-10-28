package main

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	maa "github.com/dongwlin/rhodeck/packages/maa-go"
)

func TestNativeErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{maa.ErrInvalidInput, protocol.InvalidInput},
		{maa.ErrNativeRejected, protocol.NativeRejected},
		{maa.ErrUnsupported, protocol.Unsupported},
		{maa.ErrOperationLimit, protocol.LimitExceeded},
		{maa.ErrBufferLimit, protocol.LimitExceeded},
		{maa.ErrEventOverflow, protocol.EventOverflow},
		{maa.ErrProtocol, protocol.ProtocolError},
		{maa.ErrClosed, protocol.InvalidState},
		{maa.ErrRuntimeTerminated, protocol.InvalidState},
		{&maa.LibraryLoadError{Err: errors.New("fixture")}, protocol.LoadFailed},
		{&maa.SymbolLookupError{Err: errors.New("fixture")}, protocol.SymbolMissing},
		{&maa.FunctionRegistrationError{Err: errors.New("fixture")}, protocol.RegistrationFailed},
	} {
		t.Run(tc.code, func(t *testing.T) {
			if got := classify(fmt.Errorf("context: %w", tc.err)); got.Code != tc.code {
				t.Fatalf("code = %s, want %s", got.Code, tc.code)
			}
		})
	}
	if got := classify(errors.New(strings.Repeat("界", 4096))); len(got.Message) > 4096 {
		t.Fatalf("diagnostic exceeds bound: %d", len(got.Message))
	}
}
