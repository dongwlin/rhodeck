package cmd

import (
	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/spf13/cobra"
)

// versionCommand builds the version command. It reports the build version as
// one machine envelope.
func (a *App) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI build version",
		Long:  "Print the CLI build version as a schema version 1 JSON object.",
		Args:  rejectPositional(output.CodeUnexpectedArguments),
		RunE: func(cmd *cobra.Command, args []string) error {
			report := output.Version{Header: output.NewHeader(output.KindVersion), Version: a.version}
			if err := a.sink.Emit(cmd.Context(), report, nil, nil); err != nil {
				return output.Broken(output.WriteFailureCode(err))
			}
			return nil
		},
	}
}
