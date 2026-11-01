package cmd

import (
	"runtime"

	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"github.com/dongwlin/rhodeck/apps/cli/internal/plan"
	"github.com/spf13/cobra"
)

// coreVersionPolicy describes installation selection without claiming that a
// source review verifies any installed library, resources or task capability.
const coreVersionPolicy = "rolling; use the latest caller-provided Core and resources; no fixed Core version or old-version compatibility guarantee; no automatic downloads or updates; only the actual combination verified by doctor/run/record is evidence"

// installationFacts states what capability discovery knows about an installed
// Core. Discovery takes no plan and never loads native code.
const installationFacts = "not inspected; capabilities performs no installation discovery or native loading; run doctor with an explicit plan to inspect an installation"

// capabilitiesCommand builds the capabilities command. The report performs no
// installation discovery and loads no native code.
func (a *App) capabilitiesCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "Report the CLI capability surface",
		Long: "Report the platform, worker protocol version, registered task adapters and the task input contract. The " +
			"report performs no installation discovery and loads no native code.",
		Args: rejectPositional(output.CodeUnexpectedArguments),
		RunE: func(cmd *cobra.Command, args []string) error {
			report := output.Capabilities{
				Header:            output.NewHeader(output.KindCapabilities),
				Version:           a.version,
				ProtocolVersion:   output.ProtocolVersion,
				OS:                runtime.GOOS,
				Arch:              runtime.GOARCH,
				Commands:          a.commandNames(root),
				TaskInput:         "string task type and raw JSON object parameters",
				TaskAdapters:      plan.Rows(),
				AdapterBasis:      plan.AdapterBasis,
				CoreVersionPolicy: coreVersionPolicy,
				InstallationFacts: installationFacts,
				DeviceAcceptance:  plan.DeviceAcceptance,
				TaskAcceptance:    "determined by the explicitly selected Core and resources at execution",
			}
			if err := a.sink.Emit(cmd.Context(), report, nil, nil); err != nil {
				return output.Broken(output.WriteFailureCode(err))
			}
			return nil
		},
	}
}
