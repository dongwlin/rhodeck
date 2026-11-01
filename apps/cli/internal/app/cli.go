// Package app runs explicit MaaCore task plans through control.
package app

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/dongwlin/rhodeck/packages/control"
)

// Run handles process signals and executes the command with the supplied build
// version, process arguments and standard streams, returning its exit code.
func Run(version string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runCLI(version, ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func runCLI(version string, parent context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	app := newCLIApp(version, stdin, stdout, stderr)
	root := app.newRootCommand()
	if args == nil {
		// A nil argument list makes cobra fall back to os.Args[1:]; an empty
		// list keeps the caller's explicit invocation.
		args = []string{}
	}
	root.SetArgs(args)
	_, err := root.ExecuteContextC(parent)
	if err == nil {
		// The help renderer cannot return an error, so it reports a failed
		// write through the app.
		err = app.helpErr
	}
	if err == nil {
		return exitSuccess
	}
	var cliErr *cliError
	if !errors.As(err, &cliErr) {
		cliErr = &cliError{code: "invalid_arguments", exit: exitRejected}
	}
	if !cliErr.reported {
		// The machine envelope and the diagnostic line are the whole output of
		// a rejected command.
		_ = app.encoder.Encode(envelope{SchemaVersion: control.SchemaVersion, Type: "error", Code: cliErr.code})
		// Never echo input values or raw worker diagnostics to the diagnostic stream.
		_, _ = io.WriteString(stderr, "rhodeck-cli: "+cliErr.code+"\n")
	}
	return cliErr.exit
}
