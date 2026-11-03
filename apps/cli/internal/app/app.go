// Package app owns the process lifetime of the CLI: the signals, the streams,
// the command tree and the exit code. It is the only place that decides what
// a failed command prints.
package app

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/dongwlin/rhodeck/apps/cli/internal/cmd"
	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
)

// Run handles process signals and executes the command with the supplied build
// version, process arguments and standard streams, returning its exit code.
func Run(version string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runCLI(version, ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func runCLI(version string, parent context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	sink := output.NewWriter(stdout)
	defer sink.Close()
	diagnostics := output.NewDiagnostics(stderr)
	app := cmd.New(version, stdin, sink)
	root := app.Root()
	if args == nil {
		args = []string{}
	}
	root.SetArgs(args)
	_, err := root.ExecuteContextC(parent)
	if err == nil {
		err = app.HelpError()
	}
	if err == nil {
		return output.ExitSuccess
	}
	return report(sink, diagnostics, err)
}

func report(sink *output.Writer, diagnostics *output.Diagnostics, err error) int {
	var failure *output.Failure
	if !errors.As(err, &failure) {
		failure = output.Reject(output.CodeInvalidArguments)
	}
	if failure.Emission == output.EmitEnvelope {
		if writeErr := sink.Emit(context.Background(), failure.Envelope, nil, nil); writeErr != nil {
			failure = &output.Failure{
				Code:     output.WriteFailureCode(writeErr),
				Exit:     output.ExitUnknown,
				Emission: output.EmitDiagnostic,
			}
		}
	}
	if failure.Emission != output.EmitNothing {
		diagnostics.Write(failure.Code)
	}
	return failure.Exit
}
