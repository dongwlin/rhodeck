// Package cli runs explicit MaaCore task plans through control.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// Run handles process signals and executes the command with the supplied build
// version, process arguments and standard streams, returning its exit code.
func Run(version string) int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	return runCLI(version, ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr)
}

func runCLI(version string, parent context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	encoder := json.NewEncoder(stdout)
	encoder.SetEscapeHTML(false)
	fail := func(code string, exit int) int {
		_ = encoder.Encode(envelope{SchemaVersion: control.SchemaVersion, Type: "error", Code: code})
		// Never echo input values or raw worker diagnostics to the diagnostic stream.
		_, _ = io.WriteString(stderr, "rhodeck-cli: "+code+"\n")
		return exit
	}
	if len(args) == 0 {
		return fail("command_required", exitRejected)
	}
	command := args[0]
	if command == "help" || command == "--help" || command == "-h" ||
		((command == "run" || command == "dry-run" || command == "doctor") && len(args) == 2 && (args[1] == "--help" || args[1] == "-h")) {
		if err := encoder.Encode(envelope{SchemaVersion: control.SchemaVersion, Type: "help",
			Usage: "rhodeck-cli capabilities|version; rhodeck-cli run|dry-run|doctor --input FILE|- --worker ABS_PATH --data-dir ABS_PATH [--timeout DURATION] [--startup-timeout 10s] [--request-timeout 30s] [--stop-timeout 5s] [--shutdown-timeout 5s] [--output-timeout 5s] [--include-raw-events]; run also accepts --dry-run. Explicit task adapters " + cliAdapterSummary() + " require task.adapter; tasks without it stay raw. dry-run compiles selected adapters and previews each task's actual objective policy and unobserved targets without side effects. Adapters are locally implemented without authorized device acceptance."}); err != nil {
			return fail("output_failed", exitUnknown)
		}
		return exitSuccess
	}
	if command == "version" || command == "capabilities" {
		if len(args) != 1 {
			return fail("unexpected_arguments", exitRejected)
		}
		var output any = envelope{SchemaVersion: control.SchemaVersion, Type: "version", Version: version}
		if command == "capabilities" {
			output = capabilities{
				SchemaVersion: control.SchemaVersion, Type: "capabilities", Version: version,
				ProtocolVersion: protocol.Version, OS: runtime.GOOS, Arch: runtime.GOARCH,
				Commands:     []string{"run", "dry-run", "doctor", "capabilities", "version", "help"},
				TaskInput:    "string task type and raw JSON object parameters",
				TaskAdapters: cliAdapterCapabilities(),
				AdapterBasis: adapterBasis, CoreVersionPolicy: coreVersionPolicy,
				InstallationFacts: installationFacts, DeviceAcceptance: deviceAcceptance,
				TaskAcceptance: "determined by the explicitly selected Core and resources at execution",
			}
		}
		if err := encoder.Encode(output); err != nil {
			return fail("output_failed", exitUnknown)
		}
		return exitSuccess
	}
	if command != "run" && command != "dry-run" && command != "doctor" {
		return fail("unknown_command", exitRejected)
	}
	opts, err := parseOptions(command, args[1:])
	if err != nil {
		return fail("invalid_arguments", exitRejected)
	}
	plan, err := readPlan(opts.input, stdin)
	if err != nil {
		return fail("invalid_plan", exitRejected)
	}
	adapters := cliAdapters()
	if command == "dry-run" || opts.dryRun {
		// Preparation compiles selected adapters and creates an unobserved target
		// preview. It creates no directory, takes no lock, launches no worker,
		// loads no native code and connects no device.
		prepared, err := control.PreparePlan(plan, adapters...)
		if err != nil {
			return fail("invalid_plan", exitRejected)
		}
		unverified := []string{"Core task acceptance and semantics", "installation and native dependencies", "device connection and task preconditions"}
		unverified = append(unverified, adapterUnverified(plan)...)
		output := envelope{
			SchemaVersion: control.SchemaVersion, Type: "plan", Plan: &prepared.Plan, Tasks: prepared.Tasks,
			Sources:    &planSources{Input: opts.input, Worker: opts.worker, DataDir: opts.dataDir},
			Unverified: unverified,
		}
		if err := encoder.Encode(output); err != nil {
			return fail("output_failed", exitUnknown)
		}
		return exitSuccess
	}
	if command == "run" {
		// Reject unknown adapter selections and strict input before New creates
		// or locks the data directory. Run prepares again for the submission.
		if _, err := control.PreparePlan(plan, adapters...); err != nil {
			return fail("invalid_plan", exitRejected)
		}
	}
	checks, pathsOK, nativeReady := checkLocalPaths(opts, plan)
	if command != "doctor" && !pathsOK {
		return fail("environment_rejected", exitRejected)
	}
	ctx, cancel := context.WithCancel(parent)
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
	if command == "doctor" {
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
		if err := encoder.Encode(output); err != nil {
			return fail("output_failed", exitUnknown)
		}
		return exit
	}
	output := newRunOutput(encoder)
	defer output.close()
	runFailure := func(code string, exit int) int {
		if err := output.write(ctx, nil, envelope{SchemaVersion: control.SchemaVersion, Type: "error", Code: code}, opts.outputTimeout, func() {}); err != nil {
			output.close()
			code, exit = "output_failed", exitUnknown
			if errors.Is(err, errOutputTimeout) {
				code = "output_timeout"
			}
		}
		_, _ = io.WriteString(stderr, "rhodeck-cli: "+code+"\n")
		return exit
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
	outputFailed := func(err error) int {
		// No more output is queued after a failed write. In particular, do
		// not use fail: the same stdout sink may be permanently blocked.
		output.close()
		cancel()
		closeRuntime()
		code := "output_failed"
		if errors.Is(err, errOutputTimeout) {
			code = "output_timeout"
		}
		_, _ = io.WriteString(stderr, "rhodeck-cli: "+code+"\n")
		return exitUnknown
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
	return resultExit(result)
}
