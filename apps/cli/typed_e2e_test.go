package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
)

type typedTerminal struct {
	envelope
	lines []json.RawMessage
	text  string
}

func runTypedTerminal(t *testing.T, args []string, input []byte) (int, typedTerminal, string) {
	t.Helper()
	code, text, lines, diagnostics := runMachine(t, args, input)
	var terminal envelope
	if len(lines) == 0 || json.Unmarshal(lines[len(lines)-1], &terminal) != nil || terminal.Type != "terminal" || terminal.Result == nil {
		t.Fatalf("missing terminal record: exit=%d %s (%s)", code, text, diagnostics)
	}
	return code, typedTerminal{envelope: terminal, lines: lines, text: text}, diagnostics
}

// Two tasks of the same Core type in one batch must stay separated by their
// accepted Core task IDs. Each task's typed facts, typed events and durable
// record carry only its own targets.
func TestTypedSameTypeTasksStayIsolatedAcrossWorker(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "success")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-a", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
		operProgressTask("grow-b", `{"plans":[{"role":"Caster","name":"Beta","skill_level":7}]}`),
		switchThemeTask("theme-a", `{"themes":["Theme One","Theme Two"]}`),
		switchThemeTask("theme-b", `{"themes":["Theme Three"]}`),
	)
	code, terminal, diagnostics := runTypedTerminal(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	if code != exitSuccess {
		t.Fatalf("exit = %d: %s (%s)", code, terminal.text, diagnostics)
	}
	result := terminal.Result
	if result.State != control.Succeeded || result.Cleanup != "complete" {
		t.Fatalf("result = %+v", result)
	}

	growA := taskResult(t, result.Tasks, "grow-a")
	if growA.State != control.Succeeded || growA.Typed == nil || growA.Typed.State != control.Succeeded {
		t.Fatalf("grow-a = %+v", growA)
	}
	if target := growA.Typed.Targets[0]; target.ID != "op-0001" || target.Outcome != control.TargetAchieved ||
		target.Code != "completed" || !strings.Contains(string(target.Details), `"reported":2`) {
		t.Fatalf("grow-a target = %+v", target)
	}
	growB := taskResult(t, result.Tasks, "grow-b")
	if growB.State != control.Succeeded || growB.Typed == nil || growB.Typed.State != control.Succeeded {
		t.Fatalf("grow-b = %+v", growB)
	}
	if target := growB.Typed.Targets[0]; target.Outcome != control.TargetAchieved ||
		target.Code != "completed" || !strings.Contains(string(target.Details), `"reported":7`) {
		t.Fatalf("grow-b target = %+v", target)
	}
	themeA := taskResult(t, result.Tasks, "theme-a")
	if target := themeA.Typed.Targets[0]; target.Outcome != control.TargetAlreadySet || !bytes.Equal(target.Details, json.RawMessage(`{"theme":"Theme One"}`)) {
		t.Fatalf("theme-a target = %+v", target)
	}
	themeB := taskResult(t, result.Tasks, "theme-b")
	if target := themeB.Typed.Targets[0]; target.Outcome != control.TargetAlreadySet || !bytes.Equal(target.Details, json.RawMessage(`{"theme":"Theme Three"}`)) {
		t.Fatalf("theme-b target = %+v", target)
	}
	for _, task := range result.Tasks {
		if task.CoreTaskID == 0 {
			t.Fatalf("task %q lacks a Core task ID", task.ID)
		}
	}
	if growA.CoreTaskID == growB.CoreTaskID || themeA.CoreTaskID == themeB.CoreTaskID {
		t.Fatalf("same-type tasks shared a Core task ID: %+v", result.Tasks)
	}

	// Every typed event names one accepted task and the latest event agrees with
	// the terminal result; no event mixes facts between tasks.
	counts := make(map[string]int)
	latest := make(map[string]*control.TypedResult)
	for _, line := range terminal.lines {
		var event struct {
			Type       string               `json:"type"`
			TaskID     string               `json:"task_id"`
			CoreTaskID int32                `json:"core_task_id"`
			Typed      *control.TypedResult `json:"typed"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			t.Fatal(err)
		}
		if event.Type != "typed" {
			continue
		}
		if event.TaskID == "" || event.CoreTaskID == 0 || event.Typed == nil {
			t.Fatalf("typed event lacks association: %s", line)
		}
		counts[event.TaskID]++
		latest[event.TaskID] = event.Typed
	}
	wantCounts := map[string]int{"grow-a": 2, "grow-b": 2, "theme-a": 1, "theme-b": 1}
	for id, want := range wantCounts {
		if counts[id] != want {
			t.Fatalf("typed events for %s = %d, want %d (all: %v)", id, counts[id], want, counts)
		}
	}
	for id, want := range latest {
		if got := taskResult(t, result.Tasks, id).Typed; !reflect.DeepEqual(got, want) {
			t.Fatalf("%s final event and terminal disagree: %+v vs %+v", id, want, got)
		}
	}

	record, err := control.ReadRecord(dir, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record.Result.Tasks, result.Tasks) {
		t.Fatalf("record and terminal disagree: %+v vs %+v", record.Result.Tasks, result.Tasks)
	}
}

// A summary cannot fill in a target that never received its own detail, and a
// task-chain success cannot become a typed success.
func TestTypedPartialObservationRemainsIncomplete(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "partial")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_level":7}]}`),
	)
	code, terminal, diagnostics := runTypedTerminal(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	if code != exitUnknown {
		t.Fatalf("exit = %d: %s (%s)", code, terminal.text, diagnostics)
	}
	result := terminal.Result
	if result.State != control.Unknown || result.Code != "typed_incomplete" || result.Cleanup != "complete" {
		t.Fatalf("result = %+v", result)
	}
	grow := taskResult(t, result.Tasks, "grow-1")
	if grow.State != control.Succeeded {
		t.Fatalf("partial typed payload rewrote the chain fact: %+v", grow)
	}
	if grow.Typed.State != control.Unknown || grow.Typed.Code != "typed_incomplete" {
		t.Fatalf("typed aggregate = %+v", grow.Typed)
	}
	if target := grow.Typed.Targets[0]; target.Outcome != control.TargetAchieved {
		t.Fatalf("observed target = %+v", target)
	}
	if target := grow.Typed.Targets[1]; target.Outcome != control.TargetUnobserved || target.Code != "" || len(target.Details) != 0 {
		t.Fatalf("missing detail was filled in: %+v", target)
	}
	summary := string(grow.Typed.Summary)
	if !strings.Contains(summary, `"observed":1`) || !strings.Contains(summary, `"unobserved":1`) {
		t.Fatalf("summary = %s", summary)
	}
	record, err := control.ReadRecord(dir, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record.Result.Tasks, result.Tasks) {
		t.Fatalf("record lost the partial facts: %+v", record.Result.Tasks)
	}
}

// A mastery Completed report means training at the target level started. It is
// reported startup evidence, not achievement, so the batch stays incomplete even
// when the native chain succeeds.
func TestTypedMasteryCompletionReportsStartupOnly(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "success")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}]}`),
	)
	code, terminal, diagnostics := runTypedTerminal(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	if code != exitUnknown {
		t.Fatalf("exit = %d: %s (%s)", code, terminal.text, diagnostics)
	}
	result := terminal.Result
	if result.State != control.Unknown || result.Code != "typed_incomplete" || result.Cleanup != "complete" {
		t.Fatalf("result = %+v", result)
	}
	grow := taskResult(t, result.Tasks, "grow-1")
	if grow.State != control.Succeeded || grow.Typed.State != control.Unknown || grow.Typed.Code != "typed_incomplete" {
		t.Fatalf("task = %+v", grow)
	}
	if target := grow.Typed.Targets[0]; target.Outcome != control.TargetAchieved {
		t.Fatalf("elite target = %+v", target)
	}
	mastery := grow.Typed.Targets[1]
	if mastery.Outcome != control.TargetReportedTrainingStarted || mastery.Code != "mastery_training_started" {
		t.Fatalf("mastery target = %+v", mastery)
	}
	if !strings.Contains(string(mastery.Details), `"reported":3`) {
		t.Fatalf("mastery detail = %s", mastery.Details)
	}
	record, err := control.ReadRecord(dir, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := record.Result.Tasks[0].Typed.Targets[1]; got.Outcome != control.TargetReportedTrainingStarted {
		t.Fatalf("record = %+v", got)
	}
}

// A Core append rejection rejects the run before Start. The typed targets stay
// unobserved; chain rejection is not a business success or failure.
func TestTypedCoreRejectionDoesNotBecomeTypedSuccess(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "reject")
	trace := filepath.Join(t.TempDir(), "commands")
	t.Setenv("RHODECK_CLI_TRACE", trace)
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
	)
	code, terminal, diagnostics := runTypedTerminal(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	if code != exitRejected {
		t.Fatalf("exit = %d: %s (%s)", code, terminal.text, diagnostics)
	}
	result := terminal.Result
	if result.State != control.Rejected || result.Code != "native_rejected" || result.Cleanup != "complete" {
		t.Fatalf("rejection result = %+v", result)
	}
	grow := taskResult(t, result.Tasks, "grow-1")
	if grow.State != control.Rejected {
		t.Fatalf("chain fact = %+v", grow)
	}
	if grow.Typed == nil || grow.Typed.State != control.Unknown || grow.Typed.Code != "typed_incomplete" {
		t.Fatalf("typed aggregate = %+v", grow.Typed)
	}
	if target := grow.Typed.Targets[0]; target.Outcome != control.TargetUnobserved {
		t.Fatalf("rejection created a fact: %+v", target)
	}
	commands, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"start\n"} {
		if bytes.Contains(commands, []byte(forbidden)) {
			t.Fatalf("rejection issued %q: %s", forbidden, commands)
		}
	}
	if !bytes.Contains(commands, []byte("append_task\n")) {
		t.Fatalf("fixture did not receive the rejected append: %s", commands)
	}
}

// A deadline is not a successful stop. Confirmed target facts survive and the
// run remains stopped with its timeout code.
func TestTypedDeadlineKeepsConfirmedFacts(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "typedhold")
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	t.Setenv("RHODECK_CLI_WORKER_PID", pidFile)
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
	)
	args := append(cliArgs("run", executable, dir), "--timeout", "3s")
	code, terminal, diagnostics := runTypedTerminal(t, args, encodePlan(t, plan))
	if code != exitUnknown {
		t.Fatalf("exit = %d: %s (%s)", code, terminal.text, diagnostics)
	}
	result := terminal.Result
	if result.State != control.Stopped || result.Code != "deadline_exceeded" || result.Cleanup != "complete" {
		t.Fatalf("deadline result = %+v", result)
	}
	grow := taskResult(t, result.Tasks, "grow-1")
	if grow.Typed == nil || grow.Typed.State != control.Succeeded || grow.Typed.Targets[0].Outcome != control.TargetAchieved {
		t.Fatalf("deadline lost the confirmed fact: %+v", grow)
	}
	record, err := control.ReadRecord(dir, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Result.State != control.Stopped || record.Result.Tasks[0].Typed.Targets[0].Outcome != control.TargetAchieved {
		t.Fatalf("record = %+v", record.Result)
	}
	requireWorkerExited(t, pidFile)
}

// A worker crash keeps the already confirmed typed fact but leaves the run
// unknown and its cleanup failed. Nothing restarts or replays the batch. The
// crash is triggered by cancelling only after the published typed event, so the
// fixture cannot crash before the fact was persisted.
func TestTypedWorkerCrashKeepsConfirmedFacts(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "typedcrash")
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	t.Setenv("RHODECK_CLI_WORKER_PID", pidFile)
	trace := filepath.Join(t.TempDir(), "commands")
	t.Setenv("RHODECK_CLI_TRACE", trace)
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &markerCancelWriter{cancel: cancel, marker: []byte(`"type":"typed"`)}
	var diagnostics bytes.Buffer
	args := append(cliArgs("run", executable, dir), "--timeout", "20s")
	code := runCLI(ctx, args, bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	if code != exitUnknown {
		t.Fatalf("exit = %d: %s", code, diagnostics.String())
	}
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
		t.Fatalf("missing terminal record: %v", err)
	}
	result := terminal.Result
	// The worker process disappearing is reported as a transport failure. Both
	// stable codes mean the same thing here: the run is unknown, never success.
	if result.State != control.Unknown || (result.Code != "worker_exited" && result.Code != "disconnected") ||
		result.Cleanup != "failed" || result.CleanupCode == "" {
		t.Fatalf("crash result = %+v", result)
	}
	grow := taskResult(t, result.Tasks, "grow-1")
	if grow.Typed == nil || grow.Typed.State != control.Succeeded || grow.Typed.Targets[0].Outcome != control.TargetAchieved {
		t.Fatalf("crash lost the confirmed fact: %+v", grow)
	}
	record, err := control.ReadRecord(dir, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Result.Tasks[0].Typed.Targets[0].Outcome != control.TargetAchieved {
		t.Fatalf("record lost the confirmed fact: %+v", record.Result.Tasks[0])
	}
	requireWorkerExited(t, pidFile)
	commands, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(commands), "initialize\n"); got != 1 {
		t.Fatalf("worker lifecycle restarted after crash: %d initializes\n%s", got, commands)
	}
	if !bytes.Contains(commands, []byte("stop\n")) {
		t.Fatalf("crash was not triggered by a stop request: %s", commands)
	}
}

// Context cancellation is a confirmed stop: the typed fact survives, cleanup is
// complete and the exit code is the stopped code, not a failure or success.
func TestTypedCancellationKeepsConfirmedFacts(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "typedhold")
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	t.Setenv("RHODECK_CLI_WORKER_PID", pidFile)
	trace := filepath.Join(t.TempDir(), "commands")
	t.Setenv("RHODECK_CLI_TRACE", trace)
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
	)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &markerCancelWriter{cancel: cancel, marker: []byte(`"type":"typed"`)}
	var diagnostics bytes.Buffer
	code := runCLI(ctx, cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	if code != exitStopped {
		t.Fatalf("exit = %d: %s", code, diagnostics.String())
	}
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	if err := json.Unmarshal(lines[len(lines)-1], &terminal); err != nil || terminal.Result == nil {
		t.Fatalf("missing terminal record: %v", err)
	}
	result := terminal.Result
	if result.State != control.Stopped || result.Code != "stop_confirmed" || result.Cleanup != "complete" {
		t.Fatalf("cancellation result = %+v", result)
	}
	if got := taskResult(t, result.Tasks, "grow-1").Typed.Targets[0].Outcome; got != control.TargetAchieved {
		t.Fatalf("cancellation lost the confirmed fact: %q", got)
	}
	commands, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasSuffix(commands, []byte("stop\nclose\n")) {
		t.Fatalf("worker was not stopped and closed: %s", commands)
	}
	requireWorkerExited(t, pidFile)
}

// A blocked terminal write aborts output without erasing the confirmed record or
// retaining the directory lock.
func TestTypedBlockedTerminalOutputKeepsRecordFacts(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "success")
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	t.Setenv("RHODECK_CLI_WORKER_PID", pidFile)
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`),
	)
	out := newBlockingWriter()
	out.terminalOnly = true
	t.Cleanup(out.unblock)
	var diagnostics bytes.Buffer
	finished := make(chan int, 1)
	args := append(cliArgs("run", executable, dir), "--output-timeout", "200ms")
	go func() {
		finished <- runCLI(context.Background(), args, bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	}()
	select {
	case <-out.started:
	case <-time.After(10 * time.Second):
		t.Fatal("CLI did not reach the blocked terminal write")
	}
	select {
	case code := <-finished:
		if code != exitUnknown {
			t.Fatalf("blocked output exit = %d", code)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("blocked output retained the CLI beyond its cleanup deadline")
	}
	if !strings.Contains(diagnostics.String(), "output_timeout") {
		t.Fatalf("output deadline diagnostic missing: %s", diagnostics.String())
	}
	rt, err := control.New(control.Config{DataDir: dir, Worker: worker.Config{Executable: executable}})
	if err != nil {
		t.Fatalf("data directory was not released: %v", err)
	}
	if err := rt.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "runs"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("run record missing: %v %v", entries, err)
	}
	record, err := control.ReadRecord(dir, strings.TrimSuffix(entries[0].Name(), ".json"))
	if err != nil {
		t.Fatal(err)
	}
	if record.Result.State != control.Succeeded || record.Result.Cleanup != "complete" ||
		record.Result.Tasks[0].Typed.Targets[0].Outcome != control.TargetAchieved {
		t.Fatalf("output failure changed confirmed facts: %+v", record.Result)
	}
	requireWorkerExited(t, pidFile)
	out.unblock()
	select {
	case <-out.returned:
	case <-time.After(5 * time.Second):
		t.Fatal("test output writer did not terminate after release")
	}
}

// The raw diagnostic opt-in changes only the event detail stream. Typed business
// facts, raw parameters and large integer literals are identical with and
// without it.
func TestTypedDiagnosticsOptInIsBusinessNeutral(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "success")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"name":"Alpha","role":"Warrior","elite":2}]}`),
		rawTask("raw-1", "FutureTask", `{"future":900719925474099312345,"unknown":{"a":1}}`),
		rawTask("raw-2", "FutureExploration", `{"future":900719925474099312345}`),
	)
	input := encodePlan(t, plan)
	code, plain, diagnostics := runTypedTerminal(t, cliArgs("run", executable, dir), input)
	if code != exitSuccess {
		t.Fatalf("default run exit = %d: %s (%s)", code, plain.text, diagnostics)
	}
	for _, forbidden := range []string{"private-secret", `"rect"`, `"score"`} {
		if strings.Contains(plain.text, forbidden) {
			t.Fatalf("default output disclosed %q", forbidden)
		}
	}
	if grow := taskResult(t, plain.Result.Tasks, "grow-1"); grow.Typed == nil || grow.Typed.State != control.Succeeded {
		t.Fatalf("default typed result = %+v", grow)
	}
	for _, id := range []string{"raw-1", "raw-2"} {
		if task := taskResult(t, plain.Result.Tasks, id); task.Typed != nil {
			t.Fatalf("%s gained typed facts without an adapter: %+v", id, task.Typed)
		}
	}

	rawArgs := append(cliArgs("run", executable, filepath.Join(dir, "raw")), "--include-raw-events")
	code, withRaw, diagnostics := runTypedTerminal(t, rawArgs, input)
	if code != exitSuccess {
		t.Fatalf("raw run exit = %d: %s (%s)", code, withRaw.text, diagnostics)
	}
	if !strings.Contains(withRaw.text, "private-secret") {
		t.Fatalf("explicit raw callback missing: %s", withRaw.text)
	}
	if !reflect.DeepEqual(withRaw.Result.Tasks, plain.Result.Tasks) {
		t.Fatalf("raw diagnostics changed task results: %+v vs %+v", withRaw.Result.Tasks, plain.Result.Tasks)
	}

	record, err := control.ReadRecord(dir, plain.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(record.Plan.Tasks[1].Params), "900719925474099312345") ||
		!strings.Contains(string(record.Plan.Tasks[2].Params), "900719925474099312345") {
		t.Fatalf("record raw parameters changed: %+v", record.Plan.Tasks)
	}
	if !bytes.Contains(record.Plan.Tasks[0].Params, []byte(`"name":"Alpha","role":"Warrior"`)) {
		t.Fatalf("record lost the original strict input: %s", record.Plan.Tasks[0].Params)
	}
}
