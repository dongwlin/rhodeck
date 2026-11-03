package cmd

import (
	"context"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/spf13/cobra"
)

func (a *App) dryRunCommand() *cobra.Command {
	var opts Options
	cmd := &cobra.Command{
		Use:   "dry-run",
		Short: "Preview one explicit plan without side effects",
		Long: "Compile the selected adapters and preview each task's actual objective policy and unobserved targets. " +
			"It creates no directory, takes no lock, starts no worker, loads no native code and connects no device.",
		Args: rejectPositional(output.CodeInvalidArguments),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.preview(&opts)
		},
	}
	opts.register(cmd)
	return cmd
}

func (a *App) preview(opts *Options) error {
	if err := opts.validate(); err != nil {
		return output.Reject(output.CodeInvalidArguments)
	}
	p, err := a.service.Load(opts.input, a.stdin)
	if err != nil {
		return output.Reject(output.CodeInvalidPlan)
	}
	prepared, unverified, err := a.service.Prepare(p)
	if err != nil {
		return output.Reject(output.CodeInvalidPlan)
	}
	preview := output.Plan{
		Header:     output.NewHeader(output.KindPlan),
		Plan:       &prepared.Plan,
		Tasks:      prepared.Tasks,
		Sources:    &output.Sources{Input: opts.input, Worker: opts.worker, DataDir: opts.dataDir},
		Unverified: unverified,
	}
	if err := a.sink.Emit(context.Background(), preview, nil, nil); err != nil {
		return output.Broken(output.WriteFailureCode(err))
	}
	return nil
}
