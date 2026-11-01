package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	"github.com/spf13/cobra"
)

const (
	formatText = "text"
	formatJSON = "json"
)

// cliApp owns the process streams and the single machine-output encoder. The
// encoder writes one JSON object per line and never escapes HTML.
type cliApp struct {
	version string
	stdin   io.Reader
	stdout  io.Writer
	stderr  io.Writer
	encoder *json.Encoder

	format  *formatValue
	helpCmd *cobra.Command
	helpErr error
}

// formatValue is the --format flag value. Only the accepted formats parse;
// every other value is a flag error.
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

func newCLIApp(version string, stdin io.Reader, stdout, stderr io.Writer) *cliApp {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	return &cliApp{version: version, stdin: stdin, stdout: stdout, stderr: stderr, encoder: encoder}
}

// cliUsageText is the root command's long description. It restates the
// command surface, the explicit adapter selection rule and the preview and
// acceptance boundaries.
func cliUsageText() string {
	return "rhodeck-cli capabilities|version; rhodeck-cli run|dry-run|doctor --input FILE|- --worker ABS_PATH --data-dir ABS_PATH " +
		"[--timeout DURATION] [--startup-timeout 10s] [--request-timeout 30s] [--stop-timeout 5s] [--shutdown-timeout 5s] " +
		"[--output-timeout 5s] [--include-raw-events]; run also accepts --dry-run. Explicit task adapters " +
		cliAdapterSummary() + " require task.adapter; tasks without it stay raw. dry-run compiles selected adapters and " +
		"previews each task's actual objective policy and unobserved targets without side effects. Adapters are locally " +
		"implemented without authorized device acceptance."
}

// newRootCommand builds the command tree. The declaration order of the
// subcommands is the reported command order, so cobra must not sort them.
func (a *cliApp) newRootCommand() *cobra.Command {
	cobra.EnableCommandSorting = false

	root := &cobra.Command{
		Use:           "rhodeck-cli",
		Short:         "Run explicit MaaCore task plans through control",
		Long:          cliUsageText(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          rejectPositional("unknown_command"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return &cliError{code: "command_required", exit: exitRejected}
		},
	}
	// Nothing cobra prints may reach the machine output stream.
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		return &cliError{code: "invalid_arguments", exit: exitRejected}
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
		Args:               rejectPositional("unknown_command"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return &cliError{code: "unknown_command", exit: exitRejected}
		},
	})
	return root
}

// rejectPositional rejects every leftover positional argument with one stable
// diagnostic code.
func rejectPositional(code string) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if len(args) > 0 {
			return &cliError{code: code, exit: exitRejected}
		}
		return nil
	}
}

func (a *cliApp) runCommand() *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Execute one explicit task batch",
		Long: "Execute one explicit task batch through the shared control runtime. The runtime owns the worker, task " +
			"association, observations, durable record and cleanup. The plan is read from --input; nothing is inferred " +
			"from the host.",
		Args: rejectPositional("invalid_arguments"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if opts.dryRun {
				return a.previewTask(&opts)
			}
			return a.executeTask(cmd, &opts)
		},
	}
	opts.register(cmd)
	opts.registerDryRun(cmd)
	return cmd
}

func (a *cliApp) dryRunCommand() *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   "dry-run",
		Short: "Preview one explicit plan without side effects",
		Long: "Compile the selected adapters and preview each task's actual objective policy and unobserved targets. " +
			"It creates no directory, takes no lock, starts no worker, loads no native code and connects no device.",
		Args: rejectPositional("invalid_arguments"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.previewTask(&opts)
		},
	}
	opts.register(cmd)
	return cmd
}

func (a *cliApp) doctorCommand() *cobra.Command {
	var opts options
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose an explicit installation",
		Long: "Check the explicit files, directories and executable bits, then initialize Core in a supervised worker and " +
			"close it. It never connects a device or submits a task.",
		Args: rejectPositional("invalid_arguments"),
		RunE: func(cmd *cobra.Command, args []string) error {
			return a.doctorTask(cmd, &opts)
		},
	}
	opts.register(cmd)
	return cmd
}

func (a *cliApp) capabilitiesCommand(root *cobra.Command) *cobra.Command {
	return &cobra.Command{
		Use:   "capabilities",
		Short: "Report the CLI capability surface",
		Long: "Report the platform, worker protocol version, registered task adapters and the task input contract. The " +
			"report performs no installation discovery and loads no native code.",
		Args: rejectPositional("unexpected_arguments"),
		RunE: func(cmd *cobra.Command, args []string) error {
			output := capabilities{
				SchemaVersion: control.SchemaVersion, Type: "capabilities", Version: a.version,
				ProtocolVersion: protocol.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
				Commands:     a.commandNames(root),
				TaskInput:    "string task type and raw JSON object parameters",
				TaskAdapters: cliAdapterCapabilities(),
				AdapterBasis: adapterBasis, CoreVersionPolicy: coreVersionPolicy,
				InstallationFacts: installationFacts, DeviceAcceptance: deviceAcceptance,
				TaskAcceptance: "determined by the explicitly selected Core and resources at execution",
			}
			if err := a.encoder.Encode(output); err != nil {
				return &cliError{code: "output_failed", exit: exitUnknown}
			}
			return nil
		},
	}
}

func (a *cliApp) versionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the CLI build version",
		Long:  "Print the CLI build version as a schema version 1 JSON object.",
		Args:  rejectPositional("unexpected_arguments"),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := a.encoder.Encode(envelope{SchemaVersion: control.SchemaVersion, Type: "version", Version: a.version}); err != nil {
				return &cliError{code: "output_failed", exit: exitUnknown}
			}
			return nil
		},
	}
}

func (a *cliApp) helpCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "help [command]",
		Short: "Show help for a command",
		Long: "Show the help text of a command. With --format json the same text is returned as a schema version 1 JSON " +
			"envelope.",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) > 1 {
				return &cliError{code: "unexpected_arguments", exit: exitRejected}
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			target := cmd.Root()
			if len(args) == 1 {
				found := findSubCommand(cmd.Root(), args[0])
				if found == nil {
					return &cliError{code: "unknown_command", exit: exitRejected}
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
func (a *cliApp) commandNames(root *cobra.Command) []string {
	names := make([]string, 0, len(root.Commands()))
	for _, cmd := range root.Commands() {
		if cmd == a.helpCmd || cmd.IsAvailableCommand() {
			names = append(names, cmd.Name())
		}
	}
	return names
}

// formatFor reads the --format value that was parsed for the running command.
func (a *cliApp) formatFor(cmd *cobra.Command) string {
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
func (a *cliApp) renderHelp(cmd *cobra.Command, format string) error {
	cmd.InitDefaultHelpFlag()
	text := helpText(cmd)
	if format == formatJSON {
		if err := a.encoder.Encode(envelope{SchemaVersion: control.SchemaVersion, Type: "help", Usage: text}); err != nil {
			return &cliError{code: "output_failed", exit: exitUnknown}
		}
		return nil
	}
	if _, err := io.WriteString(a.stdout, text); err != nil {
		return &cliError{code: "output_failed", exit: exitUnknown}
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

// previewTask compiles the selected adapters and reports the preview. It
// creates no directory, takes no lock, launches no worker, loads no native
// code and connects no device.
func (a *cliApp) previewTask(opts *options) error {
	if err := opts.validate(); err != nil {
		return &cliError{code: "invalid_arguments", exit: exitRejected}
	}
	plan, err := readPlan(opts.input, a.stdin)
	if err != nil {
		return &cliError{code: "invalid_plan", exit: exitRejected}
	}
	prepared, err := control.PreparePlan(plan, cliAdapters()...)
	if err != nil {
		return &cliError{code: "invalid_plan", exit: exitRejected}
	}
	unverified := []string{"Core task acceptance and semantics", "installation and native dependencies", "device connection and task preconditions"}
	unverified = append(unverified, adapterUnverified(plan)...)
	output := envelope{
		SchemaVersion: control.SchemaVersion, Type: "plan", Plan: &prepared.Plan, Tasks: prepared.Tasks,
		Sources:    &planSources{Input: opts.input, Worker: opts.worker, DataDir: opts.dataDir},
		Unverified: unverified,
	}
	if err := a.encoder.Encode(output); err != nil {
		return &cliError{code: "output_failed", exit: exitUnknown}
	}
	return nil
}

// executeTask runs one explicit batch through the shared control runtime.
func (a *cliApp) executeTask(cmd *cobra.Command, opts *options) error {
	if err := opts.validate(); err != nil {
		return &cliError{code: "invalid_arguments", exit: exitRejected}
	}
	plan, err := readPlan(opts.input, a.stdin)
	if err != nil {
		return &cliError{code: "invalid_plan", exit: exitRejected}
	}
	adapters := cliAdapters()
	// Reject unknown adapter selections and strict input before New creates
	// or locks the data directory. Run prepares again for the submission.
	if _, err := control.PreparePlan(plan, adapters...); err != nil {
		return &cliError{code: "invalid_plan", exit: exitRejected}
	}
	_, pathsOK, _ := checkLocalPaths(*opts, plan)
	if !pathsOK {
		return &cliError{code: "environment_rejected", exit: exitRejected}
	}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	if opts.timeout != 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, opts.timeout)
		defer stop()
	}
	cfg := control.Config{
		DataDir: opts.dataDir, StopTimeout: opts.stopTimeout, IncludeRawEvents: opts.includeRaw,
		Adapters: adapters,
		Worker: worker.Config{Executable: opts.worker, StartupTimeout: opts.startupTimeout,
			RequestTimeout: opts.requestTimeout, ShutdownTimeout: opts.shutdownTimeout},
	}
	output := newRunOutput(a.encoder)
	defer output.close()
	runFailure := func(code string, exit int) error {
		if err := output.write(ctx, nil, envelope{SchemaVersion: control.SchemaVersion, Type: "error", Code: code}, opts.outputTimeout, func() {}); err != nil {
			output.close()
			code, exit = "output_failed", exitUnknown
			if errors.Is(err, errOutputTimeout) {
				code = "output_timeout"
			}
		}
		a.diagnostic(code)
		return &cliError{code: code, exit: exit, reported: true}
	}
	rt, err := control.New(cfg)
	if err != nil {
		return runFailure(errorCode(err), errorExit(err))
	}
	defer rt.Close(context.Background())
	run, err := rt.Run(ctx, plan)
	if err != nil {
		_ = rt.Close(context.Background())
		return runFailure(errorCode(err), errorExit(err))
	}
	var runtimeCloseErr error
	closeRuntime := func() {
		if err := rt.Close(context.Background()); err != nil {
			runtimeCloseErr = err
		}
	}
	outputFailed := func(err error) error {
		// No more output is queued after a failed write. In particular, do
		// not report through the caller: the same stdout sink may be
		// permanently blocked.
		output.close()
		cancel()
		closeRuntime()
		code := "output_failed"
		if errors.Is(err, errOutputTimeout) {
			code = "output_timeout"
		}
		a.diagnostic(code)
		return &cliError{code: code, exit: exitUnknown, reported: true}
	}
	// Done includes final persistence and worker cleanup; events alone never
	// establish a final result. A failed output stream still triggers cleanup.
	events, done := run.Events(), run.Done()
	for events != nil || done != nil {
		select {
		case event, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if event.Type == "terminal" {
				continue
			}
			if err := output.write(ctx, run.Done(), event, opts.outputTimeout, closeRuntime); err != nil {
				return outputFailed(err)
			}
		case <-done:
			done = nil
		}
	}
	result := run.Snapshot()
	closeRuntime()
	if runtimeCloseErr != nil {
		result.Cleanup, result.CleanupCode = "failed", "runtime_close_failed"
	}
	if err := output.write(ctx, run.Done(), envelope{SchemaVersion: control.SchemaVersion, Type: "terminal", Result: &result}, opts.outputTimeout, closeRuntime); err != nil {
		return outputFailed(err)
	}
	return &cliError{code: result.Code, exit: resultExit(result), reported: true}
}

// doctorTask reports the explicit installation. It prepares no adapter and
// submits no device action, but it may create the data directory, lock it and
// initialise Core.
func (a *cliApp) doctorTask(cmd *cobra.Command, opts *options) error {
	if err := opts.validate(); err != nil {
		return &cliError{code: "invalid_arguments", exit: exitRejected}
	}
	plan, err := readPlan(opts.input, a.stdin)
	if err != nil {
		return &cliError{code: "invalid_plan", exit: exitRejected}
	}
	checks, pathsOK, nativeReady := checkLocalPaths(*opts, plan)
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	if opts.timeout != 0 {
		var stop context.CancelFunc
		ctx, stop = context.WithTimeout(ctx, opts.timeout)
		defer stop()
	}
	cfg := control.Config{
		DataDir: opts.dataDir, StopTimeout: opts.stopTimeout, IncludeRawEvents: opts.includeRaw,
		Adapters: cliAdapters(),
		Worker: worker.Config{Executable: opts.worker, StartupTimeout: opts.startupTimeout,
			RequestTimeout: opts.requestTimeout, ShutdownTimeout: opts.shutdownTimeout},
	}
	output := envelope{SchemaVersion: control.SchemaVersion, Type: "doctor", Checks: checks,
		Unverified: []string{"device connection", "task acceptance and task preconditions"}}
	exit := exitSuccess
	if !pathsOK {
		output.Code, exit = "environment_rejected", exitRejected
	}
	if nativeReady {
		info, err := control.Doctor(ctx, cfg, plan.Initialize)
		if err != nil {
			output.Checks = append(output.Checks, localCheck{Field: "native_initialize_and_cleanup", Code: errorCode(err)})
			output.Code, exit = errorCode(err), errorExit(err)
		} else {
			output.RuntimeInfo = &info
			output.Checks = append(output.Checks, localCheck{Field: "native_initialize_and_cleanup", OK: true})
		}
	}
	if err := a.encoder.Encode(output); err != nil {
		return &cliError{code: "output_failed", exit: exitUnknown}
	}
	if exit != exitSuccess {
		// The report already carries the failure; no separate error envelope
		// and no diagnostic line follow it.
		return &cliError{code: output.Code, exit: exit, reported: true}
	}
	return nil
}

// diagnostic writes the one stable diagnostic line. It never echoes input
// values or raw worker diagnostics.
func (a *cliApp) diagnostic(code string) {
	_, _ = io.WriteString(a.stderr, "rhodeck-cli: "+code+"\n")
}
