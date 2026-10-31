package control

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// newPreparedTypedHarness drives the real monitor path (Run.observe and
// Run.observeTyped) against prepared adapters without launching a worker. It is
// the same harness shape used by the adapter runtime tests, with the Start flag
// left adjustable so early-callback rejection can be exercised.
func newPreparedTypedHarness(t *testing.T, plan Plan, coreIDs []int32, startIssued bool, adapters ...TaskAdapter) *Run {
	t.Helper()
	prepared, err := PreparePlan(plan, adapters...)
	if err != nil {
		t.Fatal(err)
	}
	if len(coreIDs) != len(prepared.Tasks) {
		t.Fatalf("got %d Core task IDs for %d tasks", len(coreIDs), len(prepared.Tasks))
	}
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.close()
	})
	index := make(map[int32]int, len(coreIDs))
	for i, id := range coreIDs {
		prepared.Tasks[i].CoreTaskID = id
		prepared.Tasks[i].State = Started
		index[id] = i
	}
	return &Run{
		runtime:        &Runtime{store: s},
		plan:           prepared.Plan,
		sourcePlan:     clonePlan(plan),
		result:         Result{SchemaVersion: SchemaVersion, RunID: newRunID(), State: Started, Cleanup: "pending", Tasks: prepared.Tasks},
		events:         make(chan Event, runEventMessages),
		done:           make(chan struct{}),
		changed:        make(chan struct{}, 1),
		commands:       ctx,
		cancelCommands: cancel,
		taskIndex:      index,
		reducers:       prepared.reducers,
		startIssued:    startIssued,
	}
}

func typedEventsIn(r *Run) []Event {
	var events []Event
	for {
		select {
		case event := <-r.events:
			if event.Type == "typed" {
				events = append(events, event)
			}
		default:
			return events
		}
	}
}

// A typed callback that reaches the monitor before Start was issued cannot be
// associated with an accepted submission. It must not be queued, and it must
// not create a fact; the run reports the stable typed_invalid_event code.
func TestTypedMonitorRejectsCallbackBeforeStart(t *testing.T) {
	plan := operProgressRuntimePlan(t, `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)
	r := newPreparedTypedHarness(t, plan, []int32{101}, false, NewOperProgressAdapter())
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`)
	if r.fatal != "typed_invalid_event" {
		t.Fatalf("early callback fatal = %q, want typed_invalid_event", r.fatal)
	}
	result := r.Snapshot()
	if result.Tasks[0].Typed.State != Unknown || result.Tasks[0].Typed.Code != "typed_incomplete" {
		t.Fatalf("early callback changed the aggregate: %+v", result.Tasks[0].Typed)
	}
	if target := result.Tasks[0].Typed.Targets[0]; target.Outcome != TargetUnobserved {
		t.Fatalf("early callback created a fact: %+v", target)
	}
	if events := typedEventsIn(r); len(events) != 0 {
		t.Fatalf("early callback published %d typed events", len(events))
	}
}

// A repeated identical fact is a no-op, while a different fact for an already
// confirmed target is a conflict: the runtime stops with typed_conflict and the
// durable record still carries the original fact.
func TestTypedMonitorDuplicateAndConflictingFacts(t *testing.T) {
	plan := operProgressRuntimePlan(t, `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)
	r := newPreparedTypedHarness(t, plan, []int32{101}, true, NewOperProgressAdapter())
	achieved := `{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo, achieved)
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo, achieved)
	if r.fatal != "" {
		t.Fatalf("repeating a fact failed the run: %s", r.fatal)
	}
	if events := typedEventsIn(r); len(events) != 1 {
		t.Fatalf("duplicate fact published %d typed events, want 1", len(events))
	}
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Failed","result_detail":"ResourceInsufficient"}}`)
	if r.fatal != "typed_conflict" {
		t.Fatalf("conflicting fact fatal = %q, want typed_conflict", r.fatal)
	}
	result := r.Snapshot()
	target := result.Tasks[0].Typed.Targets[0]
	if target.Outcome != TargetAchieved || target.Code != "completed" || !strings.Contains(string(target.Details), `"reported":2`) {
		t.Fatalf("conflict replaced the confirmed fact: %+v", target)
	}
	record, err := ReadRecord(filepath.Dir(r.runtime.store.dir), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got := record.Result.Tasks[0].Typed.Targets[0]; got.Outcome != TargetAchieved || string(got.Details) != string(target.Details) {
		t.Fatalf("durable fact changed: %+v", got)
	}
}

// Retained typed events have their own count and byte bounds inside the total
// event queue. Without a consumer, a real adapter cannot grow history without
// bound: the run reports typed_event_overflow, keeps the facts that were already
// confirmed and persisted, and stops admitting further updates.
func TestTypedMonitorBoundsRetainedEventsWithoutConsumer(t *testing.T) {
	const plans = 40
	entries := make([]string, plans)
	for i := range entries {
		entries[i] = fmt.Sprintf(`{"role":"Warrior","name":"Operator-%02d","elite":2}`, i)
	}
	plan := operProgressRuntimePlan(t, `{"plans":[`+strings.Join(entries, ",")+`]}`)
	r := newPreparedTypedHarness(t, plan, []int32{101}, true, NewOperProgressAdapter())
	coreID := int32(101)
	for i := 0; i < plans; i++ {
		raw := fmt.Sprintf(`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Operator-%02d","result":"Success","result_detail":"Completed","elite":2}}`, i)
		r.observeTyped(protocol.Callback{MessageID: operProgressMessageSubTaskExtraInfo, CoreSequence: uint64(i + 1), JSON: json.RawMessage(raw)}, &coreID, nil)
	}
	if r.fatal != "typed_event_overflow" {
		t.Fatalf("fatal = %q, want typed_event_overflow", r.fatal)
	}
	if r.typedEventCount != MaxTypedEventMessages || r.typedEventBytes > MaxTypedEventRetainedBytes {
		t.Fatalf("retained typed events = %d/%d bytes, want the count bound", r.typedEventCount, r.typedEventBytes)
	}
	if len(r.events) != MaxTypedEventMessages {
		t.Fatalf("queued typed events = %d, want %d", len(r.events), MaxTypedEventMessages)
	}
	// The update that could not publish its event was applied and persisted
	// before publication, so at most one extra confirmed fact may exist.
	achieved := 0
	snapshot := r.Snapshot()
	for _, target := range snapshot.Tasks[0].Typed.Targets {
		if target.Outcome == TargetAchieved {
			achieved++
		}
	}
	if achieved < MaxTypedEventMessages || achieved > MaxTypedEventMessages+1 {
		t.Fatalf("confirmed facts = %d, want %d or %d", achieved, MaxTypedEventMessages, MaxTypedEventMessages+1)
	}
	record, err := ReadRecord(filepath.Dir(r.runtime.store.dir), snapshot.RunID)
	if err != nil {
		t.Fatal(err)
	}
	durable := 0
	for _, target := range record.Result.Tasks[0].Typed.Targets {
		if target.Outcome == TargetAchieved {
			durable++
		}
	}
	if durable != achieved {
		t.Fatalf("durable facts = %d, snapshot = %d", durable, achieved)
	}
}

// A consumer that stops draining must not keep the worker or the data-directory
// lock alive after an event overflow. The run stops unknown and a new runtime
// can take ownership immediately after cleanup.
func TestRuntimeEventOverflowReleasesCleanupAndDirectory(t *testing.T) {
	cfg := runtimeConfig(t, "overflow")
	cfg.IncludeRawEvents = true
	pidFile := filepath.Join(t.TempDir(), "worker.pid")
	cfg.Worker.Env = append(cfg.Worker.Env, "RHODECK_TEST_CONTROL_WORKER_PID="+pidFile)
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), runtimePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Done():
	case <-time.After(60 * time.Second):
		t.Fatal("cleanup depended on the event consumer")
	}
	if result := r.Snapshot(); result.State != Unknown || result.Code != "event_overflow" {
		t.Fatalf("overflow result = %+v", result)
	}
	if err := rt.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireWorkerProcessExited(t, pidFile)
	next, err := New(cfg)
	if err != nil {
		t.Fatalf("directory lock was retained after event overflow: %v", err)
	}
	if err := next.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
}
