package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/stretchr/testify/require"
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
	require.NotEmpty(t, lines, "missing terminal record: exit=%d %s (%s)", code, text, diagnostics)
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal record: exit=%d %s (%s)", code, text, diagnostics)
	require.Equal(t, "terminal", terminal.Type, "missing terminal record: exit=%d %s (%s)", code, text, diagnostics)
	require.NotNil(t, terminal.Result, "missing terminal record: exit=%d %s (%s)", code, text, diagnostics)
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
	require.Equal(t, exitSuccess, code, "%s (%s)", terminal.text, diagnostics)
	result := terminal.Result
	require.Equal(t, control.Succeeded, result.State, "result: %+v", result)
	require.Equal(t, "complete", result.Cleanup, "result: %+v", result)

	growA := taskResult(t, result.Tasks, "grow-a")
	require.Equal(t, control.Succeeded, growA.State, "grow-a: %+v", growA)
	require.NotNil(t, growA.Typed, "grow-a: %+v", growA)
	require.Equal(t, control.Succeeded, growA.Typed.State, "grow-a: %+v", growA)
	require.Equal(t, "op-0001", growA.Typed.Targets[0].ID, "grow-a target: %+v", growA.Typed.Targets[0])
	require.Equal(t, control.TargetAchieved, growA.Typed.Targets[0].Outcome, "grow-a target: %+v", growA.Typed.Targets[0])
	require.Equal(t, "completed", growA.Typed.Targets[0].Code, "grow-a target: %+v", growA.Typed.Targets[0])
	require.Contains(t, string(growA.Typed.Targets[0].Details), `"reported":2`, "grow-a target: %+v", growA.Typed.Targets[0])
	growB := taskResult(t, result.Tasks, "grow-b")
	require.Equal(t, control.Succeeded, growB.State, "grow-b: %+v", growB)
	require.NotNil(t, growB.Typed, "grow-b: %+v", growB)
	require.Equal(t, control.Succeeded, growB.Typed.State, "grow-b: %+v", growB)
	require.Equal(t, control.TargetAchieved, growB.Typed.Targets[0].Outcome, "grow-b target: %+v", growB.Typed.Targets[0])
	require.Equal(t, "completed", growB.Typed.Targets[0].Code, "grow-b target: %+v", growB.Typed.Targets[0])
	require.Contains(t, string(growB.Typed.Targets[0].Details), `"reported":7`, "grow-b target: %+v", growB.Typed.Targets[0])
	themeA := taskResult(t, result.Tasks, "theme-a")
	require.Equal(t, control.TargetAlreadySet, themeA.Typed.Targets[0].Outcome, "theme-a target: %+v", themeA.Typed.Targets[0])
	require.True(t, bytes.Equal(themeA.Typed.Targets[0].Details, json.RawMessage(`{"theme":"Theme One"}`)), "theme-a target: %+v", themeA.Typed.Targets[0])
	themeB := taskResult(t, result.Tasks, "theme-b")
	require.Equal(t, control.TargetAlreadySet, themeB.Typed.Targets[0].Outcome, "theme-b target: %+v", themeB.Typed.Targets[0])
	require.True(t, bytes.Equal(themeB.Typed.Targets[0].Details, json.RawMessage(`{"theme":"Theme Three"}`)), "theme-b target: %+v", themeB.Typed.Targets[0])
	for _, task := range result.Tasks {
		require.NotEqual(t, int32(0), task.CoreTaskID, "task %q lacks a Core task ID", task.ID)
	}
	require.NotEqual(t, growA.CoreTaskID, growB.CoreTaskID, "same-type tasks shared a Core task ID: %+v", result.Tasks)
	require.NotEqual(t, themeA.CoreTaskID, themeB.CoreTaskID, "same-type tasks shared a Core task ID: %+v", result.Tasks)

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
		require.NoError(t, json.Unmarshal(line, &event))
		if event.Type != "typed" {
			continue
		}
		require.NotEmpty(t, event.TaskID, "typed event lacks association: %s", line)
		require.NotEqual(t, int32(0), event.CoreTaskID, "typed event lacks association: %s", line)
		require.NotNil(t, event.Typed, "typed event lacks association: %s", line)
		counts[event.TaskID]++
		latest[event.TaskID] = event.Typed
	}
	wantCounts := map[string]int{"grow-a": 2, "grow-b": 2, "theme-a": 1, "theme-b": 1}
	for id, want := range wantCounts {
		require.Equal(t, want, counts[id], "typed events for %s (all: %v)", id, counts)
	}
	for id, want := range latest {
		require.Equal(t, want, taskResult(t, result.Tasks, id).Typed, "%s final event and terminal disagree", id)
	}

	record, err := control.ReadRecord(dir, result.RunID)
	require.NoError(t, err)
	require.Equal(t, result.Tasks, record.Result.Tasks, "record and terminal disagree: %+v vs %+v", record.Result.Tasks, result.Tasks)
}

// A summary cannot fill in a target that never received its own detail, and a
// task-chain success cannot become a typed success.
func TestTypedPartialObservationRemainsIncomplete(t *testing.T) {
	t.Setenv("RHODECK_CLI_WORKER_MODE", "partial")
	plan, executable, dir := typedPlan(t,
		operProgressTask("grow-1", `{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_level":7}]}`),
	)
	code, terminal, diagnostics := runTypedTerminal(t, cliArgs("run", executable, dir), encodePlan(t, plan))
	require.Equal(t, exitUnknown, code, "%s (%s)", terminal.text, diagnostics)
	result := terminal.Result
	require.Equal(t, control.Unknown, result.State, "result: %+v", result)
	require.Equal(t, "typed_incomplete", result.Code, "result: %+v", result)
	require.Equal(t, "complete", result.Cleanup, "result: %+v", result)
	grow := taskResult(t, result.Tasks, "grow-1")
	require.Equal(t, control.Succeeded, grow.State, "partial typed payload rewrote the chain fact: %+v", grow)
	require.Equal(t, control.Unknown, grow.Typed.State, "typed aggregate: %+v", grow.Typed)
	require.Equal(t, "typed_incomplete", grow.Typed.Code, "typed aggregate: %+v", grow.Typed)
	require.Equal(t, control.TargetAchieved, grow.Typed.Targets[0].Outcome, "observed target: %+v", grow.Typed.Targets[0])
	require.Equal(t, control.TargetUnobserved, grow.Typed.Targets[1].Outcome, "missing detail was filled in: %+v", grow.Typed.Targets[1])
	require.Empty(t, grow.Typed.Targets[1].Code, "missing detail was filled in: %+v", grow.Typed.Targets[1])
	require.Empty(t, grow.Typed.Targets[1].Details, "missing detail was filled in: %+v", grow.Typed.Targets[1])
	summary := string(grow.Typed.Summary)
	require.Contains(t, summary, `"observed":1`, "summary: %s", summary)
	require.Contains(t, summary, `"unobserved":1`, "summary: %s", summary)
	record, err := control.ReadRecord(dir, result.RunID)
	require.NoError(t, err)
	require.Equal(t, result.Tasks, record.Result.Tasks, "record lost the partial facts: %+v", record.Result.Tasks)
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
	require.Equal(t, exitUnknown, code, "%s (%s)", terminal.text, diagnostics)
	result := terminal.Result
	require.Equal(t, control.Unknown, result.State, "result: %+v", result)
	require.Equal(t, "typed_incomplete", result.Code, "result: %+v", result)
	require.Equal(t, "complete", result.Cleanup, "result: %+v", result)
	grow := taskResult(t, result.Tasks, "grow-1")
	require.Equal(t, control.Succeeded, grow.State, "task: %+v", grow)
	require.Equal(t, control.Unknown, grow.Typed.State, "task: %+v", grow)
	require.Equal(t, "typed_incomplete", grow.Typed.Code, "task: %+v", grow)
	require.Equal(t, control.TargetAchieved, grow.Typed.Targets[0].Outcome, "elite target: %+v", grow.Typed.Targets[0])
	mastery := grow.Typed.Targets[1]
	require.Equal(t, control.TargetReportedTrainingStarted, mastery.Outcome, "mastery target: %+v", mastery)
	require.Equal(t, "mastery_training_started", mastery.Code, "mastery target: %+v", mastery)
	require.Contains(t, string(mastery.Details), `"reported":3`, "mastery detail: %s", mastery.Details)
	record, err := control.ReadRecord(dir, result.RunID)
	require.NoError(t, err)
	require.Equal(t, control.TargetReportedTrainingStarted, record.Result.Tasks[0].Typed.Targets[1].Outcome, "record: %+v", record.Result.Tasks[0].Typed.Targets[1])
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
	require.Equal(t, exitRejected, code, "%s (%s)", terminal.text, diagnostics)
	result := terminal.Result
	require.Equal(t, control.Rejected, result.State, "rejection result: %+v", result)
	require.Equal(t, "native_rejected", result.Code, "rejection result: %+v", result)
	require.Equal(t, "complete", result.Cleanup, "rejection result: %+v", result)
	grow := taskResult(t, result.Tasks, "grow-1")
	require.Equal(t, control.Rejected, grow.State, "chain fact: %+v", grow)
	require.NotNil(t, grow.Typed, "typed aggregate: %+v", grow.Typed)
	require.Equal(t, control.Unknown, grow.Typed.State, "typed aggregate: %+v", grow.Typed)
	require.Equal(t, "typed_incomplete", grow.Typed.Code, "typed aggregate: %+v", grow.Typed)
	require.Equal(t, control.TargetUnobserved, grow.Typed.Targets[0].Outcome, "rejection created a fact: %+v", grow.Typed.Targets[0])
	commands, err := os.ReadFile(trace)
	require.NoError(t, err)
	for _, forbidden := range []string{"start\n"} {
		require.False(t, bytes.Contains(commands, []byte(forbidden)), "rejection issued %q: %s", forbidden, commands)
	}
	require.True(t, bytes.Contains(commands, []byte("append_task\n")), "fixture did not receive the rejected append: %s", commands)
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
	require.Equal(t, exitUnknown, code, "%s (%s)", terminal.text, diagnostics)
	result := terminal.Result
	require.Equal(t, control.Stopped, result.State, "deadline result: %+v", result)
	require.Equal(t, "deadline_exceeded", result.Code, "deadline result: %+v", result)
	require.Equal(t, "complete", result.Cleanup, "deadline result: %+v", result)
	grow := taskResult(t, result.Tasks, "grow-1")
	require.NotNil(t, grow.Typed, "deadline lost the confirmed fact: %+v", grow)
	require.Equal(t, control.Succeeded, grow.Typed.State, "deadline lost the confirmed fact: %+v", grow)
	require.Equal(t, control.TargetAchieved, grow.Typed.Targets[0].Outcome, "deadline lost the confirmed fact: %+v", grow)
	record, err := control.ReadRecord(dir, result.RunID)
	require.NoError(t, err)
	require.Equal(t, control.Stopped, record.Result.State, "record: %+v", record.Result)
	require.Equal(t, control.TargetAchieved, record.Result.Tasks[0].Typed.Targets[0].Outcome, "record: %+v", record.Result)
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
	code := runCLI("dev", ctx, args, bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	require.Equal(t, exitUnknown, code, "diagnostics: %s", diagnostics.String())
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal record")
	require.NotNil(t, terminal.Result, "missing terminal record")
	result := terminal.Result
	// The worker process disappearing is reported as a transport failure. Both
	// stable codes mean the same thing here: the run is unknown, never success.
	require.Equal(t, control.Unknown, result.State, "crash result: %+v", result)
	require.True(t, result.Code == "worker_exited" || result.Code == "disconnected", "crash result: %+v", result)
	require.Equal(t, "failed", result.Cleanup, "crash result: %+v", result)
	require.NotEmpty(t, result.CleanupCode, "crash result: %+v", result)
	grow := taskResult(t, result.Tasks, "grow-1")
	require.NotNil(t, grow.Typed, "crash lost the confirmed fact: %+v", grow)
	require.Equal(t, control.Succeeded, grow.Typed.State, "crash lost the confirmed fact: %+v", grow)
	require.Equal(t, control.TargetAchieved, grow.Typed.Targets[0].Outcome, "crash lost the confirmed fact: %+v", grow)
	record, err := control.ReadRecord(dir, result.RunID)
	require.NoError(t, err)
	require.Equal(t, control.TargetAchieved, record.Result.Tasks[0].Typed.Targets[0].Outcome, "record lost the confirmed fact: %+v", record.Result.Tasks[0])
	requireWorkerExited(t, pidFile)
	commands, err := os.ReadFile(trace)
	require.NoError(t, err)
	initializes := strings.Count(string(commands), "initialize\n")
	require.Equal(t, 1, initializes, "worker lifecycle restarted after crash: %d initializes\n%s", initializes, commands)
	require.True(t, bytes.Contains(commands, []byte("stop\n")), "crash was not triggered by a stop request: %s", commands)
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
	code := runCLI("dev", ctx, cliArgs("run", executable, dir), bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	require.Equal(t, exitStopped, code, "diagnostics: %s", diagnostics.String())
	lines := decodeLines(t, out.Bytes())
	var terminal envelope
	require.NoError(t, json.Unmarshal(lines[len(lines)-1], &terminal), "missing terminal record")
	require.NotNil(t, terminal.Result, "missing terminal record")
	result := terminal.Result
	require.Equal(t, control.Stopped, result.State, "cancellation result: %+v", result)
	require.Equal(t, "stop_confirmed", result.Code, "cancellation result: %+v", result)
	require.Equal(t, "complete", result.Cleanup, "cancellation result: %+v", result)
	require.Equal(t, control.TargetAchieved, taskResult(t, result.Tasks, "grow-1").Typed.Targets[0].Outcome,
		"cancellation lost the confirmed fact")
	commands, err := os.ReadFile(trace)
	require.NoError(t, err)
	require.True(t, bytes.HasSuffix(commands, []byte("stop\nclose\n")), "worker was not stopped and closed: %s", commands)
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
		finished <- runCLI("dev", context.Background(), args, bytes.NewReader(encodePlan(t, plan)), out, &diagnostics)
	}()
	select {
	case <-out.started:
	case <-time.After(10 * time.Second):
		require.FailNow(t, "CLI did not reach the blocked terminal write")
	}
	select {
	case code := <-finished:
		require.Equal(t, exitUnknown, code, "blocked output exit")
	case <-time.After(10 * time.Second):
		require.FailNow(t, "blocked output retained the CLI beyond its cleanup deadline")
	}
	require.Contains(t, diagnostics.String(), "output_timeout", "output deadline diagnostic missing: %s", diagnostics.String())
	rt, err := control.New(control.Config{DataDir: dir, Worker: worker.Config{Executable: executable}})
	require.NoError(t, err, "data directory was not released")
	require.NoError(t, rt.Close(context.Background()))
	entries, err := os.ReadDir(filepath.Join(dir, "runs"))
	require.NoError(t, err, "run record missing")
	require.Len(t, entries, 1, "run record missing: %v", entries)
	record, err := control.ReadRecord(dir, strings.TrimSuffix(entries[0].Name(), ".json"))
	require.NoError(t, err)
	require.Equal(t, control.Succeeded, record.Result.State, "output failure changed confirmed facts: %+v", record.Result)
	require.Equal(t, "complete", record.Result.Cleanup, "output failure changed confirmed facts: %+v", record.Result)
	require.Equal(t, control.TargetAchieved, record.Result.Tasks[0].Typed.Targets[0].Outcome, "output failure changed confirmed facts: %+v", record.Result)
	requireWorkerExited(t, pidFile)
	out.unblock()
	select {
	case <-out.returned:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test output writer did not terminate after release")
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
	require.Equal(t, exitSuccess, code, "default run: %s (%s)", plain.text, diagnostics)
	for _, forbidden := range []string{"private-secret", `"rect"`, `"score"`} {
		require.NotContains(t, plain.text, forbidden, "default output disclosed %q", forbidden)
	}
	grow := taskResult(t, plain.Result.Tasks, "grow-1")
	require.NotNil(t, grow.Typed, "default typed result: %+v", grow)
	require.Equal(t, control.Succeeded, grow.Typed.State, "default typed result: %+v", grow)
	for _, id := range []string{"raw-1", "raw-2"} {
		task := taskResult(t, plain.Result.Tasks, id)
		require.Nil(t, task.Typed, "%s gained typed facts without an adapter: %+v", id, task.Typed)
	}

	rawArgs := append(cliArgs("run", executable, filepath.Join(dir, "raw")), "--include-raw-events")
	code, withRaw, diagnostics := runTypedTerminal(t, rawArgs, input)
	require.Equal(t, exitSuccess, code, "raw run: %s (%s)", withRaw.text, diagnostics)
	require.Contains(t, withRaw.text, "private-secret", "explicit raw callback missing: %s", withRaw.text)
	require.Equal(t, plain.Result.Tasks, withRaw.Result.Tasks, "raw diagnostics changed task results: %+v vs %+v", withRaw.Result.Tasks, plain.Result.Tasks)

	record, err := control.ReadRecord(dir, plain.Result.RunID)
	require.NoError(t, err)
	require.Contains(t, string(record.Plan.Tasks[1].Params), "900719925474099312345", "record raw parameters changed: %+v", record.Plan.Tasks)
	require.Contains(t, string(record.Plan.Tasks[2].Params), "900719925474099312345", "record raw parameters changed: %+v", record.Plan.Tasks)
	require.True(t, bytes.Contains(record.Plan.Tasks[0].Params, []byte(`"name":"Alpha","role":"Warrior"`)), "record lost the original strict input: %s", record.Plan.Tasks[0].Params)
}
