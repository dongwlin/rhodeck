package cmd

import (
	"context"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/spf13/cobra"
)

func (a *App) helpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Show help for a command",
		Long: "Show the help text of a command. With --format json the same text is returned as a schema version 1 JSON " +
			"envelope.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return output.Reject(output.CodeUnexpectedArguments)
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target := cmd.Root()
			if len(args) == 1 {
				found := findSubCommand(cmd.Root(), args[0])
				if found == nil {
					return output.Reject(output.CodeUnknownCommand)
				}
				target = found
			}
			return a.renderHelp(target, a.formatFor(cmd))
		},
	}
}

func findSubCommand(root *cobra.Command, name string) *cobra.Command {
	for _, cmd := range root.Commands() {
		if cmd.Hidden {
			continue
		}
		if cmd.Name() == name || cmd.HasAlias(name) {
			return cmd
		}
	}
	return nil
}

func (a *App) commandNames(root *cobra.Command) []string {
	names := make([]string, 0, len(root.Commands()))
	for _, cmd := range root.Commands() {
		if cmd == a.helpCmd || cmd.IsAvailableCommand() {
			names = append(names, cmd.Name())
		}
	}
	return names
}

func (a *App) formatFor(cmd *cobra.Command) string {
	if cmd != nil {
		if value, err := cmd.Flags().GetString("format"); err == nil && value != "" {
			return value
		}
	}
	if a.format != nil && a.format.value != "" {
		return a.format.value
	}
	return formatText
}

func (a *App) renderHelp(cmd *cobra.Command, format string) error {
	cmd.InitDefaultHelpFlag()
	text := helpText(cmd)
	if format == formatJSON {
		envelope := output.Help{Header: output.NewHeader(output.KindHelp), Usage: text}
		if err := a.sink.Emit(context.Background(), envelope, nil, nil); err != nil {
			return output.Broken(output.WriteFailureCode(err))
		}
		return nil
	}
	if err := a.sink.EmitText(context.Background(), text); err != nil {
		return output.Broken(output.WriteFailureCode(err))
	}
	return nil
}

func helpText(cmd *cobra.Command) string {
	usage := cmd.UsageString()
	if cmd.Long != "" {
		return cmd.Long + "\n\n" + usage
	}
	return usage
}
