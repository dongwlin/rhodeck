package cmd

import (
	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/spf13/cobra"
)

// runCommand builds the run command. It executes one explicit task batch
// through the shared control runtime and accepts the run-only preview switch.
func (a *App) runCommand() *cobra.Command {
	var opts Options
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute one explicit task batch",
		Long: "Execute one explicit task batch through the shared control runtime. The runtime owns the worker, task " +
			"association, observations, durable record and cleanup. The plan is read from --input; nothing is inferred " +
			"from the host.",
		Args: rejectPositional(output.CodeInvalidArguments),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.dryRun {
				return a.preview(&opts)
			}
			return a.execute(cmd, &opts)
		},
	}
	opts.register(cmd)
	opts.registerDryRun(cmd)
	return cmd
}

// execute runs one explicit batch through the shared control runtime. The
// machine output carries the run events and the terminal snapshot; the
// terminal snapshot is written only after the run finished and the runtime
// released its directory lock.
func (a *App) execute(cmd *cobra.Command, opts *Options) error {
	if err := opts.validate(); err != nil {
		return output.Reject(output.CodeInvalidArguments)
	}
	p, err := a.service.Load(opts.input, a.stdin)
	if err != nil {
		return output.Reject(output.CodeInvalidPlan)
	}
	a.sink.SetGrace(opts.outputTimeout)
	result, err := a.service.Execute(cmd.Context(), p, opts.planOptions(), a.sink)
	if err != nil {
		return err
	}
	terminal := output.Terminal{Header: output.NewHeader(output.KindTerminal), Result: &result}
	if err := a.sink.Emit(cmd.Context(), terminal, nil, nil); err != nil {
		return output.Broken(output.WriteFailureCode(err))
	}
	return output.Reported(result.Code, output.ExitForResult(result))
}
