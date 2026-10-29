// Command rhodeck-cli runs explicit MaaCore task plans through control.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

var version = "dev"

const (
	exitSuccess  = 0
	exitRejected = 2
	exitBusy     = 3
	exitFailed   = 4
	exitStopped  = 5
	exitUnknown  = 6
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	os.Exit(runCLI(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

type envelope struct {
	SchemaVersion int                   `json:"schema_version"`
	Type          string                `json:"type"`
	Code          string                `json:"code,omitempty"`
	Version       string                `json:"version,omitempty"`
	Usage         string                `json:"usage,omitempty"`
	Result        *control.Result       `json:"result,omitempty"`
	Plan          *control.Plan         `json:"plan,omitempty"`
	Sources       *planSources          `json:"sources,omitempty"`
	Unverified    []string              `json:"unverified,omitempty"`
	RuntimeInfo   *protocol.RuntimeInfo `json:"runtime_info,omitempty"`
	Checks        []localCheck          `json:"checks,omitempty"`
}

type localCheck struct {
	Field string `json:"field"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
}

type planSources struct {
	Input   string `json:"input"`
	Worker  string `json:"worker"`
	DataDir string `json:"data_dir"`
}

type capabilities struct {
	SchemaVersion       int      `json:"schema_version"`
	Type                string   `json:"type"`
	Version             string   `json:"cli_version"`
	ProtocolVersion     int      `json:"worker_protocol_version"`
	OS                  string   `json:"os"`
	Arch                string   `json:"arch"`
	Commands            []string `json:"commands"`
	TaskInput           string   `json:"task_input"`
	NativeTaskDiscovery bool     `json:"native_task_discovery"`
	TaskAcceptance      string   `json:"task_acceptance"`
}

type options struct {
	input           string
	worker          string
	dataDir         string
	timeout         time.Duration
	startupTimeout  time.Duration
	requestTimeout  time.Duration
	stopTimeout     time.Duration
	shutdownTimeout time.Duration
	outputTimeout   time.Duration
	dryRun          bool
	includeRaw      bool
}

func runCLI(parent context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
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
			Usage: "rhodeck-cli capabilities|version; rhodeck-cli run|dry-run|doctor --input FILE|- --worker ABS_PATH --data-dir ABS_PATH [--timeout DURATION] [--startup-timeout 10s] [--request-timeout 30s] [--stop-timeout 5s] [--shutdown-timeout 5s] [--output-timeout 5s] [--include-raw-events]; run also accepts --dry-run"}); err != nil {
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
				Commands:       []string{"run", "dry-run", "doctor", "capabilities", "version", "help"},
				TaskInput:      "string task type and raw JSON object parameters",
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
	if command == "dry-run" || opts.dryRun {
		output := envelope{
			SchemaVersion: control.SchemaVersion, Type: "plan", Plan: &plan,
			Sources:    &planSources{Input: opts.input, Worker: opts.worker, DataDir: opts.dataDir},
			Unverified: []string{"Core task acceptance and semantics", "installation and native dependencies", "device connection and task preconditions"},
		}
		if err := encoder.Encode(output); err != nil {
			return fail("output_failed", exitUnknown)
		}
		return exitSuccess
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

func parseOptions(command string, args []string) (options, error) {
	var o options
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.input, "input", "", "plan JSON file, or - for stdin")
	fs.StringVar(&o.worker, "worker", "", "absolute Core worker executable path")
	fs.StringVar(&o.dataDir, "data-dir", "", "absolute private control data directory")
	fs.DurationVar(&o.timeout, "timeout", 0, "run deadline; zero disables the deadline")
	fs.DurationVar(&o.startupTimeout, "startup-timeout", 10*time.Second, "worker startup deadline")
	fs.DurationVar(&o.requestTimeout, "request-timeout", 30*time.Second, "worker request deadline")
	fs.DurationVar(&o.stopTimeout, "stop-timeout", 5*time.Second, "graceful stop deadline")
	fs.DurationVar(&o.shutdownTimeout, "shutdown-timeout", 5*time.Second, "worker shutdown deadline")
	fs.DurationVar(&o.outputTimeout, "output-timeout", 5*time.Second, "run output write deadline")
	fs.BoolVar(&o.includeRaw, "include-raw-events", false, "include private raw Core callback JSON")
	if command == "run" {
		fs.BoolVar(&o.dryRun, "dry-run", false, "validate and preview without native or device actions")
	}
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() != 0 || o.input == "" || !validAbsolutePath(o.worker) || !validAbsolutePath(o.dataDir) ||
		o.timeout < 0 || o.startupTimeout <= 0 || o.requestTimeout <= 0 || o.stopTimeout <= 0 || o.shutdownTimeout <= 0 || o.outputTimeout <= 0 {
		return o, errors.New("invalid CLI options")
	}
	return o, nil
}

func validAbsolutePath(path string) bool {
	return filepath.IsAbs(path) && utf8.ValidString(path) && !strings.ContainsRune(path, 0)
}

func readPlan(path string, stdin io.Reader) (control.Plan, error) {
	if path == "-" {
		return control.DecodePlan(stdin)
	}
	f, err := os.Open(path)
	if err != nil {
		return control.Plan{}, err
	}
	defer f.Close()
	return control.DecodePlan(f)
}

func checkLocalPaths(o options, plan control.Plan) (checks []localCheck, allOK, nativeReady bool) {
	allOK, nativeReady = true, true
	add := func(field, code string) {
		ok := code == ""
		checks = append(checks, localCheck{Field: field, OK: ok, Code: code})
		if !ok {
			allOK = false
			if field != "connection.adb_path" {
				nativeReady = false
			}
		}
	}
	for _, file := range []struct {
		field, path string
		executable  bool
	}{
		{"worker", o.worker, true}, {"initialize.library_path", plan.Initialize.LibraryPath, false}, {"connection.adb_path", plan.Connection.ADBPath, true},
	} {
		info, err := os.Stat(file.path)
		code := ""
		switch {
		case err != nil:
			code = "file_unavailable"
		case !info.Mode().IsRegular():
			code = "not_regular_file"
		case file.executable && runtime.GOOS != "windows" && info.Mode().Perm()&0111 == 0:
			code = "not_executable"
		}
		add(file.field, code)
	}
	for i, path := range plan.Initialize.ResourceRoots {
		info, err := os.Stat(path)
		code := ""
		if err != nil || !info.IsDir() {
			code = "directory_unavailable"
		}
		add("initialize.resource_roots["+strconv.Itoa(i)+"]", code)
	}
	for _, dir := range []struct{ field, path string }{{"data_dir", o.dataDir}, {"initialize.user_dir", plan.Initialize.UserDir}} {
		info, err := os.Stat(dir.path)
		code := ""
		if err == nil && !info.IsDir() {
			code = "not_directory"
		} else if err != nil && !os.IsNotExist(err) {
			code = "directory_unavailable"
		}
		add(dir.field, code)
	}
	return checks, allOK, nativeReady
}

func resultExit(r control.Result) int {
	if r.Cleanup != "complete" || timeoutCode(r.Code) || timeoutCode(r.CleanupCode) {
		return exitUnknown
	}
	switch r.State {
	case control.Succeeded:
		return exitSuccess
	case control.Rejected:
		return exitRejected
	case control.Failed:
		return exitFailed
	case control.Stopped:
		return exitStopped
	default:
		return exitUnknown
	}
}

func timeoutCode(code string) bool {
	return code == "timeout" || code == "deadline_exceeded" || strings.HasSuffix(code, "_timeout")
}

func errorExit(err error) int {
	if errors.Is(err, control.ErrBusy) {
		return exitBusy
	}
	var failure *worker.Failure
	if errors.As(err, &failure) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return exitUnknown
	}
	return exitRejected
}

func errorCode(err error) string {
	if errors.Is(err, control.ErrBusy) {
		return "busy"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "deadline_exceeded"
	}
	if errors.Is(err, context.Canceled) {
		return "cancelled"
	}
	var failure *worker.Failure
	if errors.As(err, &failure) {
		return failure.Code
	}
	var remote *protocol.Error
	if errors.As(err, &remote) {
		return remote.Code
	}
	if errors.Is(err, control.ErrInvalidPlan) {
		return "invalid_plan"
	}
	return "environment_rejected"
}
