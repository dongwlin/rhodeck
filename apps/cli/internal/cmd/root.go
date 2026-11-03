// Package cmd owns the CLI command surface: the cobra tree, one file per
// command, and the flags the plan commands share. It depends on the plan
// package for the work and on the output package for the machine contract.
package cmd

import (
	"fmt"
	"io"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/dongwlin/rhodeck/apps/cli/internal/plan"
	"github.com/spf13/cobra"
)

const (
	formatText = "text"
	formatJSON = "json"
)

// App owns the command tree and the machine output sink.
type App struct {
	version string
	stdin   io.Reader
	sink    *output.Writer
	service *plan.Service

	format  *formatValue
	helpCmd *cobra.Command
	helpErr error
	root    *cobra.Command
}

// New builds the command tree of one process.
func New(version string, stdin io.Reader, sink *output.Writer) *App {
	a := &App{version: version, stdin: stdin, sink: sink, service: plan.NewService()}
	a.root = a.newRootCommand()
	return a
}

// Root returns the command tree.
func (a *App) Root() *cobra.Command { return a.root }

// HelpError returns the error of a failed help write. The cobra help callback
// cannot return one.
func (a *App) HelpError() error { return a.helpErr }

type formatValue struct {
	value string
}

func (f *formatValue) String() string { return f.value }

func (f *formatValue) Set(value string) error {
	switch value {
	case formatText, formatJSON:
		f.value = value
		return nil
	default:
		return fmt.Errorf("unsupported format %q", value)
	}
}

func (f *formatValue) Type() string { return "string" }

func (a *App) usageText() string {
	return "rhodeck-cli capabilities|version; rhodeck-cli run|dry-run|doctor --input FILE|- --worker ABS_PATH --data-dir ABS_PATH " +
		"[--timeout DURATION] [--startup-timeout 10s] [--request-timeout 30s] [--stop-timeout 5s] [--shutdown-timeout 5s] " +
		"[--output-timeout 5s] [--include-raw-events]; run also accepts --dry-run. Explicit task adapters " +
		plan.Summary() + " require task.adapter; tasks without it stay raw. dry-run compiles selected adapters and " +
		"previews each task's actual objective policy and unobserved targets without side effects. capabilities describes " +
		"adapter inputs, objective policies and result interpretation."
}

func (a *App) newRootCommand() *cobra.Command {
	cobra.EnableCommandSorting = false

	root := &cobra.Command{
		Use:           "rhodeck-cli",
		Short:         "Run explicit MaaCore task plans through control",
		Long:          a.usageText(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          rejectPositional(output.CodeUnknownCommand),
		RunE: func(cmd *cobra.Command, args []string) error {
			return output.Reject(output.CodeCommandRequired)
		},
	}
	// Nothing cobra prints may reach the machine output stream.
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return output.Reject(output.CodeInvalidArguments)
	})
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		if err := a.renderHelp(cmd, a.formatFor(cmd)); err != nil {
			a.helpErr = err
		}
	})

	a.format = &formatValue{value: formatText}
	root.PersistentFlags().Var(a.format, "format",
		"help and usage output format: text or json; command results are always schema version 1 JSON")

	root.AddCommand(a.runCommand())
	root.AddCommand(a.dryRunCommand())
	root.AddCommand(a.doctorCommand())
	root.AddCommand(a.capabilitiesCommand(root))
	root.AddCommand(a.versionCommand())
	a.helpCmd = a.helpCommand()
	root.SetHelpCommand(a.helpCmd)
	// The shell completion helper is not part of the command surface. It is
	// rejected like any other unknown word so that it can never write to the
	// machine output stream.
	root.AddCommand(&cobra.Command{
		Use:                cobra.ShellCompRequestCmd + " [command-line]",
		Hidden:             true,
		DisableFlagParsing: true,
		Args:               rejectPositional(output.CodeUnknownCommand),
		RunE: func(cmd *cobra.Command, args []string) error {
			return output.Reject(output.CodeUnknownCommand)
		},
	})
	return root
}

func rejectPositional(code string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return output.Reject(code)
		}
		return nil
	}
}
