package output

import (
	"context"
	"errors"
	"strings"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// The process exit codes. They are part of the CLI contract.
const (
	ExitSuccess  = 0
	ExitRejected = 2
	ExitBusy     = 3
	ExitFailed   = 4
	ExitStopped  = 5
	ExitUnknown  = 6
)

// The stable diagnostic codes. A diagnostic line carries exactly one of them
// and never carries input values or raw worker diagnostics.
const (
	CodeInvalidArguments    = "invalid_arguments"
	CodeInvalidPlan         = "invalid_plan"
	CodeEnvironmentRejected = "environment_rejected"
	CodeUnknownCommand      = "unknown_command"
	CodeUnexpectedArguments = "unexpected_arguments"
	CodeCommandRequired     = "command_required"
	CodeOutputFailed        = "output_failed"
	CodeOutputTimeout       = "output_timeout"
	CodeBusy                = "busy"
	CodeRecordWriteFailed   = "record_write_failed"
	CodeDeadlineExceeded    = "deadline_exceeded"
	CodeCancelled           = "cancelled"
)

// Emission states what the process shell still prints after a failed command.
type Emission int

const (
	// EmitEnvelope prints the error envelope and the diagnostic line.
	EmitEnvelope Emission = iota
	// EmitDiagnostic prints only the diagnostic line. The machine output
	// stream is broken and must not be written again.
	EmitDiagnostic
	// EmitNothing prints nothing. The command already wrote its own machine
	// output.
	EmitNothing
)

// Failure is the only error a command returns. It carries the stable
// diagnostic code, the process exit code and the machine output the shell
// still has to print. The command never writes the diagnostic line itself.
type Failure struct {
	Code     string
	Exit     int
	Envelope *Error
	Emission Emission
}

func (f *Failure) Error() string { return f.Code }

// Reject reports an input or environment rejection.
func Reject(code string) *Failure {
	return &Failure{Code: code, Exit: ExitRejected, Envelope: NewError(code), Emission: EmitEnvelope}
}

// Failed reports a failure with an explicit exit code.
func Failed(code string, exit int) *Failure {
	return &Failure{Code: code, Exit: exit, Envelope: NewError(code), Emission: EmitEnvelope}
}

// Reported reports a failure whose machine output the command already wrote.
func Reported(code string, exit int) *Failure {
	return &Failure{Code: code, Exit: exit, Emission: EmitNothing}
}

// Broken reports a broken machine output stream. The stream must not be
// written again.
func Broken(code string) *Failure {
	return &Failure{Code: code, Exit: ExitUnknown, Emission: EmitDiagnostic}
}

// ExitForResult maps a finished run to the process exit code. A persistence
// failure is not an input rejection even when the run entered the rejected
// state before any device work. A failed cleanup or timeout likewise maps to
// the unknown exit without changing the run's business state.
func ExitForResult(r control.Result) int {
	if r.Cleanup != "complete" || r.Code == CodeRecordWriteFailed || r.CleanupCode == CodeRecordWriteFailed ||
		timeoutCode(r.Code) || timeoutCode(r.CleanupCode) {
		return ExitUnknown
	}
	switch r.State {
	case control.Succeeded:
		return ExitSuccess
	case control.Rejected:
		return ExitRejected
	case control.Failed:
		return ExitFailed
	case control.Stopped:
		return ExitStopped
	default:
		return ExitUnknown
	}
}

func timeoutCode(code string) bool {
	return code == "timeout" || code == "deadline_exceeded" || strings.HasSuffix(code, "_timeout")
}

// ExitFor maps a runtime error to the process exit code.
func ExitFor(err error) int {
	if errors.Is(err, control.ErrBusy) {
		return ExitBusy
	}
	var failure *worker.Failure
	if errors.Is(err, control.ErrRecordWrite) || errors.As(err, &failure) ||
		errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return ExitUnknown
	}
	return ExitRejected
}

// CodeFor maps a runtime error to a stable diagnostic code.
func CodeFor(err error) string {
	if errors.Is(err, control.ErrBusy) {
		return CodeBusy
	}
	if errors.Is(err, control.ErrRecordWrite) {
		return CodeRecordWriteFailed
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeDeadlineExceeded
	}
	if errors.Is(err, context.Canceled) {
		return CodeCancelled
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
		return CodeInvalidPlan
	}
	return CodeEnvironmentRejected
}

// WriteFailureCode maps a failed machine output write to a stable code.
func WriteFailureCode(err error) string {
	if errors.Is(err, ErrTimeout) {
		return CodeOutputTimeout
	}
	return CodeOutputFailed
}
