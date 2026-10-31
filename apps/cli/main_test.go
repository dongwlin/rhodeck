package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

func TestMain(m *testing.M) {
	if os.Getenv("RHODECK_CLI_PROCESS") == "1" {
		_ = os.Unsetenv("RHODECK_CLI_PROCESS")
		main()
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
				Resources: []protocol.Resource{{Root: init.ResourceRoots[0]}}, Limits: &protocol.Limits{InputStringBytes: 1024,
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
			if fixtureHasTypedType(tasks) {
				// The supervisor marks Start as issued after it reads the
				// response. A typed callback that arrives earlier is rejected as
				// unassociated, so this real-child fixture allows that handoff.
				time.Sleep(50 * time.Millisecond)
			}
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
			case "wait", "stopcleanupfail", "stoptypedcleanupfail":
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
			respond(protocol.StopResult{Stopped: true})
			switch mode {
			case "wait", "stopcleanupfail", "stoptypedcleanupfail":
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

// fixtureHasTypedType reports whether any accepted task uses a Core type with a
// local adapter. Raw tasks of the same type are unaffected; the Reducer check in
// the runtime still keeps them on the raw path.
func fixtureHasTypedType(tasks []cliFixtureTask) bool {
	for _, task := range tasks {
		if task.core == "OperProgress" || task.core == "SwitchTheme" {
			return true
		}
	}
	return false
}

// fixtureSendsTyped reports whether a mode emits adapter callbacks.
func fixtureSendsTyped(mode string) bool {
	switch mode {
	case "success", "failure", "fault", "reported", "prooffault", "stoptypedcleanupfail":
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
	for _, plan := range params.Plans {
		details := map[string]any{
			"role": plan.Role, "name": plan.Name,
			"rect": []int{1, 2, 3, 4}, "score": 0.99, "private": "private-secret",
		}
		switch mode {
		case "success":
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
	case "success":
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
	if err := os.WriteFile(library, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func decodeLines(t *testing.T, output []byte) []json.RawMessage {
	t.Helper()
	var lines []json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(output))
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); errors.Is(err, io.EOF) {
			return lines
		} else if err != nil {
			t.Fatalf("invalid machine output: %v", err)
		}
		var metadata struct {
			SchemaVersion int    `json:"schema_version"`
			Type          string `json:"type"`
		}
		if err := json.Unmarshal(raw, &metadata); err != nil || metadata.SchemaVersion != 1 || metadata.Type == "" {
			t.Fatalf("unversioned output: %s", raw)
		}
		lines = append(lines, raw)
	}
}

func TestDiscoveryHasNoInstallationDependency(t *testing.T) {
	for _, command := range []string{"capabilities", "version", "help"} {
		t.Run(command, func(t *testing.T) {
			var out, diagnostics bytes.Buffer
			if code := runCLI(context.Background(), []string{command}, strings.NewReader("invalid"), &out, &diagnostics); code != 0 {
				t.Fatalf("exit = %d: %s", code, diagnostics.String())
			}
			if got := len(decodeLines(t, out.Bytes())); got != 1 {
				t.Fatalf("records = %d", got)
			}
			if diagnostics.Len() != 0 {
				t.Fatalf("unexpected diagnostics: %s", diagnostics.String())
			}
		})
	}
}

func TestDryRunPreservesOpenParametersWithoutEffects(t *testing.T) {
	plan, _, dir := testPlan(t)
	missingWorker := filepath.Join(t.TempDir(), "not-installed")
	var out, diagnostics bytes.Buffer
	if code := runCLI(context.Background(), cliArgs("dry-run", missingWorker, dir), bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics); code != 0 {
		t.Fatalf("exit = %d: %s", code, diagnostics.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("900719925474099312345")) || !bytes.Contains(out.Bytes(), []byte("FutureTask")) {
		t.Fatalf("raw parameters changed: %s", out.String())
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dry-run created data directory: %v", err)
	}
	lines := decodeLines(t, out.Bytes())
	var preview envelope
	if len(lines) != 1 || json.Unmarshal(lines[0], &preview) != nil || preview.Type != "plan" || len(preview.Unverified) == 0 {
		t.Fatalf("missing unverified plan: %s", out.String())
	}
}

func TestInputRejectionsHaveStablePrivateOutput(t *testing.T) {
	plan, executable, dir := testPlan(t)
	valid := encodePlan(t, plan)
	for _, input := range [][]byte{[]byte(`{"private-secret":true}`), append(bytes.Clone(valid), []byte(` {"secret":1}`)...)} {
		var out, diagnostics bytes.Buffer
		if code := runCLI(context.Background(), cliArgs("dry-run", executable, dir), bytes.NewReader(input), &out, &diagnostics); code != exitRejected {
			t.Fatalf("exit = %d", code)
		}
		decodeLines(t, out.Bytes())
		if bytes.Contains(out.Bytes(), []byte("private-secret")) || strings.Contains(diagnostics.String(), "secret") {
			t.Fatal("input escaped into error diagnostics")
		}
	}
	for _, args := range [][]string{nil, {"unknown"}, {"version", "secret"}, {"run", "--input", "secret"}, {"dry-run", "--timeout=-1s"}} {
		var out, diagnostics bytes.Buffer
		if code := runCLI(context.Background(), args, bytes.NewReader(valid), &out, &diagnostics); code != exitRejected {
			t.Fatalf("arguments %q: exit = %d", args, code)
		}
	}
}

func TestRunMachineTerminalAndExitCodes(t *testing.T) {
	for _, tc := range []struct {
		mode  string
		code  int
		state control.State
	}{
		{"success", exitSuccess, control.Succeeded}, {"failure", exitFailed, control.Failed},
		{"reject", exitRejected, control.Rejected}, {"crash", exitUnknown, control.Unknown},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			t.Setenv("RHODECK_CLI_WORKER_MODE", tc.mode)
			plan, executable, dir := testPlan(t)
			var out, diagnostics bytes.Buffer
			if code := runCLI(context.Background(), cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics); code != tc.code {
				t.Fatalf("exit = %d, want %d: %s (%s)", code, tc.code, out.String(), diagnostics.String())
			}
			lines := decodeLines(t, out.Bytes())
			var last envelope
			if len(lines) == 0 || json.Unmarshal(lines[len(lines)-1], &last) != nil || last.Type != "terminal" || last.Result == nil || last.Result.State != tc.state {
				t.Fatalf("missing authoritative terminal: %s", out.String())
			}
			if bytes.Contains(out.Bytes(), []byte("private-secret")) || strings.Contains(diagnostics.String(), "private-secret") {
				t.Fatalf("raw diagnostics leaked: %s (%s)", out.String(), diagnostics.String())
			}
		})
	}
}

func TestDoctorDoesNotSubmitDeviceActions(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "doctor")
	trace := filepath.Join(t.TempDir(), "commands")
	t.Setenv("RHODECK_CLI_TRACE", trace)
	plan, executable, dir := testPlan(t)
	var out, diagnostics bytes.Buffer
	if code := runCLI(context.Background(), cliArgs("doctor", executable, dir), bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics); code != exitSuccess {
		t.Fatalf("exit = %d: %s (%s)", code, out.String(), diagnostics.String())
	}
	commands, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if string(commands) != "initialize\nclose\n" {
		t.Fatalf("doctor commands: %s", commands)
	}
	decodeLines(t, out.Bytes())
}

func TestDoctorMissingADBStillReportsCoreSnapshot(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "doctor")
	plan, executable, dir := testPlan(t)
	plan.Connection.ADBPath = filepath.Join(t.TempDir(), "missing-adb")
	var out, diagnostics bytes.Buffer
	if code := runCLI(context.Background(), cliArgs("doctor", executable, dir), bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics); code != exitRejected {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	var report envelope
	if json.Unmarshal(out.Bytes(), &report) != nil || report.RuntimeInfo == nil {
		t.Fatalf("missing native snapshot: %s", out.String())
	}
	for _, check := range report.Checks {
		if check.Field == "connection.adb_path" && !check.OK && check.Code == "file_unavailable" {
			return
		}
	}
	t.Fatalf("missing field-level ADB rejection: %s", out.String())
}

func TestRawEventsRequireExplicitOptIn(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "success")
	plan, executable, dir := testPlan(t)
	var out, diagnostics bytes.Buffer
	args := append(cliArgs("run", executable, dir), "--include-raw-events")
	if code := runCLI(context.Background(), args, bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics); code != exitSuccess {
		t.Fatalf("exit = %d: %s", code, out.String())
	}
	if !bytes.Contains(out.Bytes(), []byte("private-secret")) {
		t.Fatalf("explicit raw callback missing: %s", out.String())
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestBrokenOutputStillClosesRuntime(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "wait")
	plan, executable, dir := testPlan(t)
	var diagnostics bytes.Buffer
	if code := runCLI(context.Background(), cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), brokenWriter{}, &diagnostics); code != exitUnknown {
		t.Fatalf("exit = %d", code)
	}
	rt, err := control.New(control.Config{DataDir: dir, Worker: worker.Config{Executable: executable}})
	if err != nil {
		t.Fatalf("data directory was not released after output failure: %v", err)
	}
	if err := rt.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}

type stopWriter struct {
	bytes.Buffer
	cancel context.CancelFunc
}

func (w *stopWriter) Write(p []byte) (int, error) {
	n, err := w.Buffer.Write(p)
	if bytes.Contains(p, []byte(`"state":"started"`)) {
		w.cancel()
	}
	return n, err
}

func TestCancellationUsesControlStopAndCleanup(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "wait")
	trace := filepath.Join(t.TempDir(), "commands")
	t.Setenv("RHODECK_CLI_TRACE", trace)
	plan, executable, dir := testPlan(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := &stopWriter{cancel: cancel}
	var diagnostics bytes.Buffer
	if code := runCLI(ctx, cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), out, &diagnostics); code != exitStopped {
		t.Fatalf("exit = %d: %s (%s)", code, out.String(), diagnostics.String())
	}
	commands, err := os.ReadFile(trace)
	if err != nil || !bytes.Contains(commands, []byte("stop\n")) || !bytes.HasSuffix(commands, []byte("close\n")) {
		t.Fatalf("stop/cleanup missing: %s (%v)", commands, err)
	}
}

func TestDeadlineRemainsDistinctFromSignalStop(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "wait")
	plan, executable, dir := testPlan(t)
	var out, diagnostics bytes.Buffer
	args := append(cliArgs("run", executable, dir), "--timeout", "1s")
	if code := runCLI(context.Background(), args, bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics); code != exitUnknown {
		t.Fatalf("exit = %d: %s (%s)", code, out.String(), diagnostics.String())
	}
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	if json.Unmarshal(lines[len(lines)-1], &terminal) != nil || terminal.Result == nil || terminal.Result.Code != "deadline_exceeded" {
		t.Fatalf("deadline reason missing: %s", out.String())
	}
}

func TestConfirmedTasksDoNotHideCleanupFailure(t *testing.T) {
	if code := resultExit(control.Result{State: control.Succeeded, Cleanup: "failed"}); code != exitUnknown {
		t.Fatalf("exit = %d", code)
	}
}

func TestStartupTimeoutUsesTimeoutExitCode(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "nohello")
	plan, executable, dir := testPlan(t)
	var out, diagnostics bytes.Buffer
	args := append(cliArgs("run", executable, dir), "--startup-timeout", "50ms")
	if code := runCLI(context.Background(), args, bytes.NewReader(encodePlan(t, plan)), &out, &diagnostics); code != exitUnknown {
		t.Fatalf("startup timeout exit = %d: %s (%s)", code, out.String(), diagnostics.String())
	}
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	if json.Unmarshal(lines[len(lines)-1], &terminal) != nil || terminal.Result == nil || terminal.Result.State != control.Rejected || terminal.Result.Code != "startup_timeout" {
		t.Fatalf("startup timeout reason missing: %s", out.String())
	}
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
			go func() { finished <- runCLI(ctx, args, bytes.NewReader(input), out, &diagnostics) }()
			select {
			case <-out.started:
			case <-time.After(5 * time.Second):
				t.Fatal("CLI did not reach the blocked output writer")
			}
			if tc.cancel {
				cancel()
			}
			select {
			case code := <-finished:
				if code != exitUnknown {
					t.Fatalf("blocked output exit = %d", code)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("blocked output retained the CLI beyond its cleanup deadlines")
			}
			if !strings.Contains(diagnostics.String(), "output_timeout") {
				t.Fatalf("output deadline diagnostic missing: %s", diagnostics.String())
			}
			rt, err := control.New(control.Config{DataDir: dir, Worker: worker.Config{Executable: executable}})
			if err != nil {
				t.Fatalf("blocked output retained the directory lock: %v", err)
			}
			if err := rt.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !tc.cancel {
				entries, err := os.ReadDir(filepath.Join(dir, "runs"))
				if err != nil || len(entries) != 1 {
					t.Fatalf("completed run record missing: %v", err)
				}
				record, err := control.ReadRecord(dir, strings.TrimSuffix(entries[0].Name(), ".json"))
				if err != nil || record.Result.State != control.Succeeded || record.Result.Cleanup != "complete" {
					t.Fatalf("output failure changed confirmed native facts: %+v (%v)", record.Result, err)
				}
			}
			writesBeforeRelease := out.writes.Load()
			out.unblock()
			select {
			case <-out.returned:
			case <-time.After(5 * time.Second):
				t.Fatal("test output writer did not terminate after release")
			}
			if out.writes.Load() != writesBeforeRelease {
				t.Fatalf("output was retried after abort: %d writes", out.writes.Load())
			}
		})
	}
}
