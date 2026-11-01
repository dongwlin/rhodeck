package app

import (
	"context"
	"errors"
	"strings"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

const (
	exitSuccess  = 0
	exitRejected = 2
	exitBusy     = 3
	exitFailed   = 4
	exitStopped  = 5
	exitUnknown  = 6
)

func resultExit(r control.Result) int {
	// A persistence failure is not an input rejection even when the run entered
	// the rejected state before any device work. A failed cleanup or timeout
	// likewise maps to exit 6 without changing the run's business state.
	if r.Cleanup != "complete" || r.Code == "record_write_failed" || r.CleanupCode == "record_write_failed" ||
		timeoutCode(r.Code) || timeoutCode(r.CleanupCode) {
		return exitUnknown
	}
	switch r.State {
	case control.Succeeded:
		return exitSuccess
	case control.Rejected:
		return exitRejected
	case control.Failed:
		return exitFailed
	case control.Stopped:
		return exitStopped
	default:
		return exitUnknown
	}
}

func timeoutCode(code string) bool {
	return code == "timeout" || code == "deadline_exceeded" || strings.HasSuffix(code, "_timeout")
}

func errorExit(err error) int {
	if errors.Is(err, control.ErrBusy) {
		return exitBusy
	}
	var failure *worker.Failure
	if errors.Is(err, control.ErrRecordWrite) || errors.As(err, &failure) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return exitUnknown
	}
	return exitRejected
}

func errorCode(err error) string {
	if errors.Is(err, control.ErrBusy) {
		return "busy"
	}
	if errors.Is(err, control.ErrRecordWrite) {
		return "record_write_failed"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var failure *worker.Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	var remote *protocol.Error
	if errors.As(err, &remote) {
		return remote.Code
	}
	if errors.Is(err, control.ErrInvalidPlan) {
		return "invalid_plan"
	}
	return "environment_rejected"
}
