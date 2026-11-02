package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/dongwlin/rhodeck/apps/cli/internal/output"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	if os.Getenv("RHODECK_CLI_PROCESS") == "1" {
		_ = os.Unsetenv("RHODECK_CLI_PROCESS")
		os.Exit(Run("dev"))
	}
	if mode := os.Getenv("RHODECK_CLI_WORKER_MODE"); mode != "" {
		os.Exit(cliWorker(mode))
	}
	os.Exit(m.Run())
}

// cliFixtureTask records one accepted task so Start can emit callbacks for the
// exact Core task ID and type that were requested.
type cliFixtureTask struct {
	id     int32
	core   string
	params json.RawMessage
}

func cliWorker(mode string) int {
	// A test that verifies worker reaping records the fixture PID before any
	// other work, so a missing marker is a fixture failure rather than a false
	// pass.
	if path := os.Getenv("RHODECK_CLI_WORKER_PID"); path != "" {
		if err := os.WriteFile(path, fmt.Appendf(nil, "%d", os.Getpid()), 0600); err != nil {
			return 18
		}
	}
	if mode == "nohello" {
		time.Sleep(time.Hour)
		return 9
	}
	var sequence, coreSequence uint64
	emit := func(kind, name string, id uint64, body any, cause *protocol.Error) {
		message, err := protocol.NewMessage(kind, name, id, body)
		if err != nil {
			panic(err)
		}
		if kind == protocol.Event {
			sequence++
			message.Sequence = sequence
		}
		message.Error = cause
		frame, err := protocol.Encode(message)
		if err != nil {
			panic(err)
		}
		if err = protocol.WriteFrame(os.Stdout, frame); err != nil {
			os.Exit(10)
		}
	}
	core := func(id int32, raw string) {
		coreSequence++
		emit(protocol.Event, protocol.CoreEvent, 0, protocol.Callback{MessageID: id, CoreSequence: coreSequence, JSON: json.RawMessage(raw)}, nil)
	}
	coreJSON := func(id int32, value any) {
		raw, err := json.Marshal(value)
		if err != nil {
			panic(err)
		}
		core(id, string(raw))
	}
	emit(protocol.Hello, "", 0, protocol.Greeting{WorkerVersion: "cli-fixture", SessionID: "cli-fixture-session",
		OS: "fixture", Arch: "fixture", MaxFrameBytes: protocol.MaxFrameBytes, QueueMessages: protocol.QueueMessages, QueueBytes: protocol.QueueBytes}, nil)
	reader := protocol.NewReader(os.Stdin)
	var tasks []cliFixtureTask
	nextTaskID := int32(23)
	for {
		req, err := reader.Read()
		if err != nil {
			return 11
		}
		if path := os.Getenv("RHODECK_CLI_TRACE"); path != "" {
			f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
			if err != nil {
				return 12
			}
			fmt.Fprintln(f, req.Name)
			f.Close()
		}
		respond := func(body any) { emit(protocol.Response, req.Name, req.ID, body, nil) }
		switch req.Name {
		case protocol.Initialize:
			var init protocol.InitializeRequest
			if protocol.Decode(req.Payload, &init) != nil {
				return 13
			}
			if mode == "initslow" {
				// Give the parent test time to make the run directory unwritable
				// before the post-initialize record save.
				release := os.Getenv("RHODECK_CLI_INIT_RELEASE")
				deadline := time.Now().Add(10 * time.Second)
				for {
					if _, err := os.Stat(release); err == nil {
						break
					}
					if time.Now().After(deadline) {
						return 16
					}
					time.Sleep(5 * time.Millisecond)
				}
			}
			respond(protocol.RuntimeInfo{SnapshotID: init.SnapshotID, CoreVersion: "fixture-core", LibrarySHA256: strings.Repeat("a", 64),
				Resources: []protocol.Resource{{Root: init.ResourceRoots[0], VersionSHA256: strings.Repeat("b", 64)}}, Limits: &protocol.Limits{InputStringBytes: 1024,
					InputJSONBytes: 4096, ConfigEntries: 128, ConfigBytes: 4096, ImagePNG: protocol.BufferLimits{Initial: 1, Max: 1},
					ImageBGR: protocol.BufferLimits{Initial: 1, Max: 1}, UUID: protocol.BufferLimits{Initial: 1, Max: 1},
					Tasks: protocol.BufferLimits{Initial: 1, Max: 1}, CallbackBytes: 4096, InternalEvents: 64, InternalBytes: 4096,
					PublicEvents: 64, PublicBytes: 4096, PendingOperations: 8, EarlyResults: 8, EarlyBytes: 4096}, NullSize: math.MaxUint64})
		case protocol.Connect:
			respond(protocol.OperationAccepted{AsyncID: 9})
			emit(protocol.Event, protocol.OperationEvent, 0, protocol.OperationResult{RequestID: req.ID, AsyncID: 9, What: "Connect", OK: true}, nil)
		case protocol.AppendTask:
			if mode == "reject" {
				emit(protocol.Response, req.Name, req.ID, nil, &protocol.Error{Code: protocol.NativeRejected, Message: "fixture private-secret"})
				continue
			}
			var task protocol.TaskRequest
			if protocol.Decode(req.Payload, &task) != nil {
				return 14
			}
			trimmed := bytes.TrimSpace(task.Params)
			if len(trimmed) == 0 || trimmed[0] != '{' {
				return 14
			}
			// The raw FutureTask fixture still proves that its exact large
			// integer literal crossed the input path. Strict adapters send a
			// compiled object instead and are not inspected here.
			if task.Type == "FutureTask" && !bytes.Contains(task.Params, []byte("900719925474099312345")) {
				return 14
			}
			id := nextTaskID
			nextTaskID++
			tasks = append(tasks, cliFixtureTask{id: id, core: task.Type, params: task.Params})
			if path := os.Getenv("RHODECK_CLI_TASK_TRACE"); path != "" {
				f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
				if err != nil {
					return 12
				}
				fmt.Fprintf(f, "%s\t%s\n", task.Type, task.Params)
				f.Close()
			}
			respond(protocol.TaskAccepted{TaskID: id})
		case protocol.Start:
			if mode == "crash" {
				return 7
			}
			respond(struct{}{})
			for _, task := range tasks {
				coreJSON(10001, map[string]any{"taskid": task.id, "taskchain": task.core, "secret": "private-secret"})
			}
			if fixtureSendsTyped(mode) {
				for _, task := range tasks {
					fixtureTypedCallbacks(task, mode, coreJSON)
				}
			}
			finish := func(failing bool) {
				for _, task := range tasks {
					if failing && task.core != "OperProgress" && task.core != "SwitchTheme" {
						coreJSON(10000, map[string]any{"taskid": task.id, "taskchain": task.core, "secret": "private-secret"})
					} else {
						coreJSON(10002, map[string]any{"taskid": task.id, "taskchain": task.core, "secret": "private-secret"})
					}
				}
				finished := make([]int32, len(tasks))
				for i, task := range tasks {
					finished[i] = task.id
				}
				coreJSON(3, map[string]any{"finished_tasks": finished})
			}
			switch mode {
			case "wait", "stopcleanupfail", "stoptypedcleanupfail", "typedhold", "typedcrash":
				continue
			case "fault":
				emit(protocol.Event, protocol.FaultEvent, 0, protocol.FaultNotice{Error: protocol.Error{Code: "instance_fault", Message: "fixture fault"}}, nil)
				continue
			case "prooffault":
				// Establish the batch proof first, then report a fault before
				// cleanup so the established state and cleanup stay separate.
				finish(false)
				emit(protocol.Event, protocol.FaultEvent, 0, protocol.FaultNotice{Error: protocol.Error{Code: "instance_fault", Message: "fixture fault"}}, nil)
				continue
			}
			finish(mode == "failure")
		case protocol.Stop:
			if mode == "typedcrash" {
				// Crash while stopping: no Stop response is sent. The supervisor
				// must report the run unknown and keep confirmed facts.
				return 7
			}
			respond(protocol.StopResult{Stopped: true})
			switch mode {
			case "wait", "stopcleanupfail", "stoptypedcleanupfail", "typedhold":
				for _, task := range tasks {
					coreJSON(10004, map[string]any{"taskid": task.id})
				}
			}
		case protocol.Status:
			respond(protocol.ExecutionStatus{Running: false, Connected: true, Tasks: []int32{}})
		case protocol.Close:
			if mode == "stopcleanupfail" || mode == "stoptypedcleanupfail" {
				emit(protocol.Response, req.Name, req.ID, nil, &protocol.Error{Code: "close_failed", Message: "fixture close failure"})
				return 0
			}
			emit(protocol.Event, protocol.DiagnosticsEvent, 0, protocol.InstanceDiagnostics{}, nil)
			respond(struct{}{})
			return 0
		default:
			return 15
		}
	}
}

// fixtureSendsTyped reports whether a mode emits adapter callbacks.
func fixtureSendsTyped(mode string) bool {
	switch mode {
	case "success", "failure", "fault", "reported", "prooffault", "stoptypedcleanupfail",
		"partial", "typedhold", "typedcrash":
		return true
	default:
		return false
	}
}

// fixtureTypedCallbacks emits synthetic native callbacks for the two strict
// adapters. The extra rect/score/private fields prove that only filtered
// business facts reach default output.
func fixtureTypedCallbacks(task cliFixtureTask, mode string, coreJSON func(int32, any)) {
	if !fixtureSendsTyped(mode) {
		return
	}
	switch task.core {
	case "OperProgress":
		fixtureOperProgressCallbacks(task, mode, coreJSON)
	case "SwitchTheme":
		fixtureSwitchThemeCallbacks(task, mode, coreJSON)
	}
}

type fixtureOperProgressPlan struct {
	Role         string `json:"role"`
	Name         string `json:"name"`
	Elite        *int   `json:"elite"`
	SkillLevel   *int   `json:"skill_level"`
	SkillMastery []int  `json:"skill_mastery"`
}

func fixtureOperProgressCallbacks(task cliFixtureTask, mode string, coreJSON func(int32, any)) {
	var params struct {
		Plans []fixtureOperProgressPlan `json:"plans"`
	}
	if json.Unmarshal(task.params, &params) != nil {
		return
	}
	for i, plan := range params.Plans {
		if mode == "partial" && i > 0 {
			// A later plan receives no detail at all; the summary must not fill
			// in the missing target.
			continue
		}
		details := map[string]any{
			"role": plan.Role, "name": plan.Name,
			"rect": []int{1, 2, 3, 4}, "score": 0.99, "private": "private-secret",
		}
		switch mode {
		case "success", "partial", "typedhold", "typedcrash":
			details["result"], details["result_detail"] = "Success", "Completed"
			switch {
			case plan.Elite != nil:
				details["elite"] = *plan.Elite
			case plan.SkillLevel != nil:
				details["skill_level"] = *plan.SkillLevel
			case len(plan.SkillMastery) == 3:
				details["skill_mastery"] = plan.SkillMastery
			}
		case "failure", "fault", "prooffault", "stoptypedcleanupfail":
			details["result"], details["result_detail"] = "Failed", "ResourceInsufficient"
		case "reported":
			// A mastery plan reports the prerequisite level that actually
			// started, never the final target.
			details["result"], details["result_detail"] = "Success", "PrerequisiteTraining"
			mastery := append([]int(nil), plan.SkillMastery...)
			started := false
			for i, level := range mastery {
				if level > 1 {
					mastery[i] = level - 1
					started = true
				}
			}
			if !started {
				continue
			}
			details["skill_mastery"] = mastery
		}
		coreJSON(20003, map[string]any{"taskchain": "OperProgress", "taskid": task.id, "what": "OperProgressDetail", "details": details})
	}
	coreJSON(20003, map[string]any{"taskchain": "OperProgress", "taskid": task.id, "what": "OperProgressSummary",
		"details": map[string]any{"finished": []any{}, "success": 0, "failed": 0, "skipped": 0}})
}

func fixtureSwitchThemeCallbacks(task cliFixtureTask, mode string, coreJSON func(int32, any)) {
	var params struct {
		Themes []string `json:"themes"`
	}
	if json.Unmarshal(task.params, &params) != nil || len(params.Themes) == 0 {
		return
	}
	coreJSON(20002, map[string]any{"taskchain": "SwitchTheme", "taskid": task.id, "subtask": "ProcessTask",
		"details": map[string]any{"task": "SwitchThemeByNameSelectTheme",
			"result": map[string]any{"text": params.Themes[0], "score": 0.98, "rect": []int{1, 2, 3, 4}}}})
	node := ""
	switch mode {
	case "success", "typedhold", "typedcrash":
		node = "SwitchThemeByNameAlreadySet"
	case "failure", "fault", "prooffault", "stoptypedcleanupfail":
		node = "SwitchThemeByNameLockedTheme"
	case "reported":
		node = "SwitchThemeByNameConfirmTheme"
	}
	if node != "" {
		coreJSON(20002, map[string]any{"taskchain": "SwitchTheme", "taskid": task.id, "subtask": "ProcessTask",
			"details": map[string]any{"task": node}})
	}
}

func testPlan(t *testing.T) (control.Plan, string, string) {
	t.Helper()
	root := t.TempDir()
	library := filepath.Join(root, "library")
	require.NoError(t, os.WriteFile(library, []byte("fixture"), 0600))
	executable, err := os.Executable()
	require.NoError(t, err)
	return control.Plan{SchemaVersion: control.SchemaVersion, HostRunID: "host-run",
		Initialize: protocol.InitializeRequest{SnapshotID: "fixture", LibraryPath: library, UserDir: filepath.Join(root, "user"), ResourceRoots: []string{root}},
		Connection: protocol.ConnectRequest{ADBPath: executable, Address: "explicit-device", Config: "General"},
		Tasks:      []control.Task{{ID: "task-a", Type: "FutureTask", Params: json.RawMessage(`{"future":900719925474099312345}`)}}}, executable, filepath.Join(root, "data")
}

func cliArgs(command, executable, dir string) []string {
	return []string{command, "--input", "-", "--worker", executable, "--data-dir", dir,
		"--startup-timeout", "5s", "--request-timeout", "2s", "--stop-timeout", "2s", "--shutdown-timeout", "2s"}
}

func encodePlan(t *testing.T, p control.Plan) []byte {
	t.Helper()
	raw, err := json.Marshal(p)
	require.NoError(t, err)
	return raw
}

func decodeLines(t *testing.T, output []byte) []json.RawMessage {
	t.Helper()
	var lines []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var raw json.RawMessage
		err := decoder.Decode(&raw)
		if errors.Is(err, io.EOF) {
			return lines
		}
		require.NoError(t, err, "invalid machine output")
		var metadata struct {
			SchemaVersion int    `json:"schema_version"`
			Type          string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(raw, &metadata), "unversioned output: %s", raw)
		require.Equal(t, 1, metadata.SchemaVersion, "unversioned output: %s", raw)
		require.NotEmpty(t, metadata.Type, "unversioned output: %s", raw)
		lines = append(lines, raw)
	}
}

// TestVersionCommandNeedsNoInstallation keeps the version command independent
// of any installed artifact: it reports the build version without reading the
// input or starting a worker.
func TestVersionCommandNeedsNoInstallation(t *testing.T) {
	var out, diagnostics bytes.Buffer
	code := runCLI("dev", context.Background(), []string{"version"}, strings.NewReader("invalid"), &out, &diagnostics)
	require.Equal(t, 0, code, "diagnostics: %s", diagnostics.String())
	require.Len(t, decodeLines(t, out.Bytes()), 1)
	require.Empty(t, diagnostics.String(), "unexpected diagnostics")
}

// TestInputRejectionsHaveStablePrivateOutput keeps a rejected plan input out
// of both streams. The rejected arguments are covered by the command contract
// of the command surface tests.
func TestInputRejectionsHaveStablePrivateOutput(t *testing.T) {
	plan, executable, dir := testPlan(t)
	valid := encodePlan(t, plan)
	for _, input := range [][]byte{[]byte(`{"private-secret":true}`), append(bytes.Clone(valid), []byte(` {"secret":1}`)...)} {
		var out, diagnostics bytes.Buffer
		code := runCLI("dev", context.Background(), cliArgs("dry-run", executable, dir), bytes.NewReader(input), &out, &diagnostics)
		require.Equal(t, output.ExitRejected, code)
		decodeLines(t, out.Bytes())
		require.False(t, bytes.Contains(out.Bytes(), []byte("private-secret")), "input escaped into error diagnostics: %s", out.String())
		require.NotContains(t, diagnostics.String(), "secret", "input escaped into error diagnostics: %s", diagnostics.String())
	}
}

func TestRunMachineTerminalAndExitCodes(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		code  int
		state control.State
	}{
		{"success", output.ExitSuccess, control.Succeeded}, {"failure", output.ExitFailed, control.Failed},
		{"reject", output.ExitRejected, control.Rejected}, {"crash", output.ExitUnknown, control.Unknown},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("RHODECK_CLI_WORKER_MODE", tc.mode)
			plan, executable, dir := testPlan(t)
			var out, diagnostics bytes.Buffer
			code := runCLI("dev", context.Background(), cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics)
			require.Equal(t, tc.code, code, "out: %s diagnostics: %s", out.String(), diagnostics.String())
			lines := decodeLines(t, out.Bytes())
			var last output.Terminal
			require.NotEmpty(t, lines, "missing authoritative terminal: %s", out.String())
			require.NoError(t, json.Unmarshal(lines[len(lines)-1], &last), "missing authoritative terminal: %s", out.String())
			require.Equal(t, "terminal", last.Type, "missing authoritative terminal: %s", out.String())
			require.NotNil(t, last.Result, "missing authoritative terminal: %s", out.String())
			require.Equal(t, tc.state, last.Result.State, "missing authoritative terminal: %s", out.String())
			checkInstallationMetadata(t, last.Result.RuntimeInfo, plan)
			record, err := control.ReadRecord(dir, last.Result.RunID)
			require.NoError(t, err)
			checkInstallationMetadata(t, record.Result.RuntimeInfo, plan)
			require.False(t, bytes.Contains(out.Bytes(), []byte("private-secret")), "raw diagnostics leaked: %s (%s)", out.String(), diagnostics.String())
			require.NotContains(t, diagnostics.String(), "private-secret", "raw diagnostics leaked: %s (%s)", out.String(), diagnostics.String())
		})
	}
}

func TestDoctorDoesNotSubmitDeviceActions(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "doctor")
	trace := filepath.Join(t.TempDir(), "commands")
	t.Setenv("RHODECK_CLI_TRACE", trace)
	plan, executable, dir := testPlan(t)
	var out, diagnostics bytes.Buffer
	code := runCLI("dev", context.Background(), cliArgs("doctor", executable, dir), bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics)
	require.Equal(t, output.ExitSuccess, code, "out: %s diagnostics: %s", out.String(), diagnostics.String())
	commands, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.Equal(t, "initialize\nclose\n", string(commands), "doctor commands: %s", commands)
	decodeLines(t, out.Bytes())
	var report output.Doctor
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	checkInstallationMetadata(t, report.RuntimeInfo, plan)
}

// checkInstallationMetadata keeps host-manifest correlation fields intact
// across diagnosis, terminal output and durable records, without a fixed release.
func checkInstallationMetadata(t *testing.T, info *protocol.RuntimeInfo, plan control.Plan) {
	t.Helper()
	require.NotNil(t, info, "installation metadata was lost: %+v", info)
	require.Equal(t, plan.Initialize.SnapshotID, info.SnapshotID, "installation metadata was lost: %+v", info)
	require.Equal(t, "fixture-core", info.CoreVersion, "installation metadata was lost: %+v", info)
	require.Equal(t, strings.Repeat("a", 64), info.LibrarySHA256, "installation metadata was lost: %+v", info)
	require.Len(t, info.Resources, 1, "installation metadata was lost: %+v", info)
	require.Equal(t, plan.Initialize.ResourceRoots[0], info.Resources[0].Root, "installation metadata was lost: %+v", info)
	require.Equal(t, strings.Repeat("b", 64), info.Resources[0].VersionSHA256, "installation metadata was lost: %+v", info)
}

func TestDoctorMissingADBStillReportsCoreSnapshot(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "doctor")
	plan, executable, dir := testPlan(t)
	plan.Connection.ADBPath = filepath.Join(t.TempDir(), "missing-adb")
	var out, diagnostics bytes.Buffer
	code := runCLI("dev", context.Background(), cliArgs("doctor", executable, dir), bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics)
	require.Equal(t, output.ExitRejected, code, "out: %s", out.String())
	var report output.Doctor
	require.NoError(t, json.Unmarshal(out.Bytes(), &report), "missing native snapshot: %s", out.String())
	require.NotNil(t, report.RuntimeInfo, "missing native snapshot: %s", out.String())
	for _, check := range report.Checks {
		if check.Field == "connection.adb_path" && !check.OK && check.Code == "file_unavailable" {
			return
		}
	}
	require.FailNow(t, "missing field-level ADB rejection: %s", out.String())
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestBrokenOutputStillClosesRuntime(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "wait")
	plan, executable, dir := testPlan(t)
	var diagnostics bytes.Buffer
	code := runCLI("dev", context.Background(), cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), brokenWriter{}, &diagnostics)
	require.Equal(t, output.ExitUnknown, code)
	rt, err := control.New(control.Config{DataDir: dir, Worker: worker.Config{Executable: executable}})
	require.NoError(t, err, "data directory was not released after output failure")
	require.NoError(t, rt.Close(context.Background()))
}

func TestStartupTimeoutUsesTimeoutExitCode(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "nohello")
	plan, executable, dir := testPlan(t)
	var out, diagnostics bytes.Buffer
	args := append(cliArgs("run", executable, dir), "--startup-timeout", "50ms")
	code := runCLI("dev", context.Background(), args, bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics)
	require.Equal(t, output.ExitUnknown, code, "out: %s diagnostics: %s", out.String(), diagnostics.String())
	lines := decodeLines(t, out.Bytes())
	var terminal output.Terminal
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "startup timeout reason missing: %s", out.String())
	require.NotNil(t, terminal.Result, "startup timeout reason missing: %s", out.String())
	require.Equal(t, control.Rejected, terminal.Result.State, "startup timeout reason missing: %s", out.String())
	require.Equal(t, "startup_timeout", terminal.Result.Code, "startup timeout reason missing: %s", out.String())
}

type blockingWriter struct {
	started      chan struct{}
	release      chan struct{}
	returned     chan struct{}
	startOnce    sync.Once
	releaseOnce  sync.Once
	returnOnce   sync.Once
	writes       atomic.Int32
	terminalOnly bool
}

func newBlockingWriter() *blockingWriter {
	return &blockingWriter{started: make(chan struct{}), release: make(chan struct{}), returned: make(chan struct{})}
}

func (w *blockingWriter) Write(p []byte) (int, error) {
	w.writes.Add(1)
	if w.terminalOnly && !bytes.Contains(p, []byte(`"type":"terminal"`)) {
		return len(p), nil
	}
	w.startOnce.Do(func() { close(w.started) })
	<-w.release
	w.returnOnce.Do(func() { close(w.returned) })
	return len(p), nil
}

func (w *blockingWriter) unblock() { w.releaseOnce.Do(func() { close(w.release) }) }

func TestBlockedOutputReleasesRuntimeAfterCancellationOrCompletion(t *testing.T) {
	for _, tc := range []struct {
		name, mode, outputTimeout string
		cancel                    bool
	}{
		{"cancelled", "wait", "100ms", true},
		{"completed", "success", "2s", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RHODECK_CLI_WORKER_MODE", tc.mode)
			plan, executable, dir := testPlan(t)
			input := encodePlan(t, plan)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			out := newBlockingWriter()
			out.terminalOnly = !tc.cancel
			t.Cleanup(out.unblock)
			var diagnostics bytes.Buffer
			finished := make(chan int, 1)
			args := append(cliArgs("run", executable, dir), "--output-timeout", tc.outputTimeout)
			go func() { finished <- runCLI("dev", ctx, args, bytes.NewReader(input), out, &diagnostics) }()
			select {
			case <-out.started:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "CLI did not reach the blocked output writer")
			}
			if tc.cancel {
				cancel()
			}
			select {
			case code := <-finished:
				require.Equal(t, output.ExitUnknown, code, "blocked output exit")
			case <-time.After(10 * time.Second):
				require.FailNow(t, "blocked output retained the CLI beyond its cleanup deadlines")
			}
			require.Contains(t, diagnostics.String(), "output_timeout", "output deadline diagnostic missing: %s", diagnostics.String())
			rt, err := control.New(control.Config{DataDir: dir, Worker: worker.Config{Executable: executable}})
			require.NoError(t, err, "blocked output retained the directory lock")
			require.NoError(t, rt.Close(context.Background()))
			if !tc.cancel {
				entries, err := os.ReadDir(filepath.Join(dir, "runs"))
				require.NoError(t, err, "completed run record missing")
				require.Len(t, entries, 1, "completed run record missing")
				record, err := control.ReadRecord(dir, strings.TrimSuffix(entries[0].Name(), ".json"))
				require.NoError(t, err, "output failure changed confirmed native facts")
				require.Equal(t, control.Succeeded, record.Result.State, "output failure changed confirmed native facts: %+v", record.Result)
				require.Equal(t, "complete", record.Result.Cleanup, "output failure changed confirmed native facts: %+v", record.Result)
			}
			writesBeforeRelease := out.writes.Load()
			out.unblock()
			select {
			case <-out.returned:
			case <-time.After(5 * time.Second):
				require.FailNow(t, "test output writer did not terminate after release")
			}
			require.Equal(t, writesBeforeRelease, out.writes.Load(), "output was retried after abort: %d writes", out.writes.Load())
		})
	}
}
