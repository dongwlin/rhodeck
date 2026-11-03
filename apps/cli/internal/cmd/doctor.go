package cmd

import (
	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/spf13/cobra"
)

func (a *App) doctorCommand() *cobra.Command {
	var opts Options
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose an explicit installation",
		Long: "Check the explicit files, directories and executable bits, then initialize Core in a supervised worker and " +
			"close it. It never connects a device or submits a task.",
		Args: rejectPositional(output.CodeInvalidArguments),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.diagnose(cmd, &opts)
		},
	}
	opts.register(cmd)
	return cmd
}

func (a *App) diagnose(cmd *cobra.Command, opts *Options) error {
	if err := opts.validate(); err != nil {
		return output.Reject(output.CodeInvalidArguments)
	}
	p, err := a.service.Load(opts.input, a.stdin)
	if err != nil {
		return output.Reject(output.CodeInvalidPlan)
	}
	report := a.service.Diagnose(cmd.Context(), p, opts.planOptions())
	envelope := output.Doctor{
		Header:      output.NewHeader(output.KindDoctor),
		Code:        report.Code,
		Unverified:  report.Unverified,
		RuntimeInfo: report.RuntimeInfo,
		Checks:      report.Checks,
	}
	if err := a.sink.Emit(cmd.Context(), envelope, nil, nil); err != nil {
		return output.Broken(output.WriteFailureCode(err))
	}
	if report.Exit != output.ExitSuccess {
		return output.Reported(report.Code, report.Exit)
	}
	return nil
}
