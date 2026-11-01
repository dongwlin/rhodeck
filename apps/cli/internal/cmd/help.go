package cmd

import (
	"context"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/spf13/cobra"
)

// helpCommand builds the help command. The command surface has no --help
// flag of its own; the root help function serves every command.
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

// findSubCommand resolves one help topic by name. Hidden helper commands are
// not help topics.
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

// commandNames reports the live command tree in declaration order. The help
// command is not an available command, so it is added explicitly.
func (a *App) commandNames(root *cobra.Command) []string {
	names := make([]string, 0, len(root.Commands()))
	for _, cmd := range root.Commands() {
		if cmd == a.helpCmd || cmd.IsAvailableCommand() {
			names = append(names, cmd.Name())
		}
	}
	return names
}

// formatFor reads the --format value that was parsed for the running command.
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

// renderHelp writes the help of one command. The text format is human
// readable; the json format returns the same text in the machine envelope.
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

// helpText is the long description of one command followed by its usage block.
func helpText(cmd *cobra.Command) string {
	usage := cmd.UsageString()
	if cmd.Long != "" {
		return cmd.Long + "\n\n" + usage
	}
	return usage
}
