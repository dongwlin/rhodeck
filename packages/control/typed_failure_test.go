package control

import (
	"context"
	"path/filepath"
	"testing"
	"time"
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
