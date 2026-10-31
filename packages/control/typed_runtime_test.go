package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

type runtimeAdapterFixture struct {
	prepare func(Task) (PreparedTask, error)
}

func (runtimeAdapterFixture) Descriptor() AdapterRef {
	return AdapterRef{Name: "runtime-fixture", Version: 1}
}
func (a runtimeAdapterFixture) Prepare(task Task) (PreparedTask, error) { return a.prepare(task) }

type runtimeReducerFunc func(Observation) (TypedUpdate, error)

func (f runtimeReducerFunc) Observe(o Observation) (TypedUpdate, error) { return f(o) }

func fixtureAdapter(reducer TaskReducer) TaskAdapter {
	return runtimeAdapterFixture{prepare: func(task Task) (PreparedTask, error) {
		return PreparedTask{Params: task.Params, Targets: []string{"goal"}, Reducer: reducer}, nil
	}}
}

func typedRuntimePlan(t *testing.T) Plan {
	t.Helper()
	plan := runtimePlan(t)
	plan.SchemaVersion = SchemaVersion
	ref := runtimeAdapterFixture{}.Descriptor()
	plan.Tasks[0].Adapter = &ref
	return plan
}

func fixtureOutcomeAdapter(outcome TargetOutcome) TaskAdapter {
	return runtimeAdapterFixture{prepare: func(task Task) (PreparedTask, error) {
		observed := false
		return PreparedTask{Params: task.Params, Targets: []string{"goal"}, Reducer: runtimeReducerFunc(func(o Observation) (TypedUpdate, error) {
			if observed || outcome == TargetUnobserved {
				return TypedUpdate{}, nil
			}
			observed = true
			return TypedUpdate{Targets: []TargetResult{{ID: "goal", Outcome: outcome, Details: json.RawMessage(`{"value":9007199254740993}`)}}, Summary: json.RawMessage(`{"observed":1}`)}, nil
		})}, nil
	}}
}

func TestRuntimeTypedChainAndBusinessResults(t *testing.T) {
	for _, test := range []struct {
		outcome TargetOutcome
		state   State
		code    string
	}{
		{TargetAchieved, Succeeded, ""},
		{TargetFailed, Failed, "typed_failed"},
		{TargetReportedTrainingStarted, Unknown, "typed_incomplete"},
		{TargetUnobserved, Unknown, "typed_incomplete"},
	} {
		t.Run(string(test.outcome), func(t *testing.T) {
			cfg := runtimeConfig(t, "success")
			cfg.Adapters = []TaskAdapter{fixtureOutcomeAdapter(test.outcome)}
			rt := newRuntime(t, cfg)
			cfg.Adapters[0] = nil // Runtime owns its registry slice.
			plan := typedRuntimePlan(t)
			r, err := rt.Run(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			plan.Tasks[0].Adapter.Name = "mutated"
			events, result := waitRuntimeRun(t, r)
			if result.SchemaVersion != SchemaVersion || result.State != test.state || result.Code != test.code || result.Cleanup != "complete" {
				t.Fatalf("business result = %+v", result)
			}
			if result.Tasks[0].State != Succeeded || result.Tasks[1].Typed != nil || result.Tasks[0].Typed.Targets[0].Outcome != test.outcome {
				t.Fatalf("chain/business isolation = %+v", result.Tasks)
			}
			typedEvents := 0
			for _, e := range events {
				if len(e.Details) != 0 {
					t.Fatal("default event disclosed raw callback")
				}
				if e.Type != "typed" {
					continue
				}
				typedEvents++
				if e.SchemaVersion != SchemaVersion || e.TaskID != "first" || e.CoreTaskID != 101 || e.Typed == nil {
					t.Fatalf("typed association = %+v", e)
				}
				e.Typed.Targets[0].Outcome = TargetAmbiguous
				e.Typed.Targets[0].Details[0] = '['
				e.Typed.Summary[0] = '['
			}
			wantEvents := 1
			if test.outcome == TargetUnobserved {
				wantEvents = 0
			}
			if typedEvents != wantEvents {
				t.Fatalf("typed event count = %d, want %d", typedEvents, wantEvents)
			}
			result.Tasks[0].Typed.Targets[0].Outcome = TargetAmbiguous
			next := r.Snapshot()
			if next.Tasks[0].Typed.Targets[0].Outcome != test.outcome {
				t.Fatal("event or result aliases runtime facts")
			}
			record, err := ReadRecord(cfg.DataDir, result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Result.State != test.state || record.Result.Tasks[0].Typed.Targets[0].Outcome != test.outcome || !strings.Contains(string(record.Plan.Tasks[0].Params), "9007199254740993") {
				t.Fatalf("record lost facts or raw precision: %+v", record)
			}
		})
	}
}

func TestRuntimeRejectsUnregisteredAdapterBeforeLaunch(t *testing.T) {
	rt := newRuntime(t, runtimeConfig(t, "nohello"))
	if _, err := rt.Run(context.Background(), typedRuntimePlan(t)); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("unsupported adapter = %v", err)
	}
	entries, err := os.ReadDir(rt.store.dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("rejection persisted an admission: %v %v", entries, err)
	}
}

func TestRuntimeTypedCleanupFailurePreservesProvenFacts(t *testing.T) {
	for _, test := range []struct {
		outcome TargetOutcome
		state   State
		code    string
	}{
		{TargetAchieved, Succeeded, ""},
		{TargetFailed, Failed, "typed_failed"},
	} {
		t.Run(string(test.outcome), func(t *testing.T) {
			cfg := runtimeConfig(t, "closehang")
			cfg.Adapters = []TaskAdapter{fixtureOutcomeAdapter(test.outcome)}
			rt := newRuntime(t, cfg)
			r, err := rt.Run(context.Background(), typedRuntimePlan(t))
			if err != nil {
				t.Fatal(err)
			}
			_, result := waitRuntimeRun(t, r)
			if result.State != test.state || result.Code != test.code || result.Cleanup != "failed" || result.CleanupCode == "" {
				t.Fatalf("cleanup failure changed proven outcome: %+v", result)
			}
			if result.Tasks[0].State != Succeeded || result.Tasks[0].Typed.State != test.state || result.Tasks[0].Typed.Targets[0].Outcome != test.outcome {
				t.Fatalf("cleanup failure changed task facts: %+v", result.Tasks[0])
			}
			record, err := ReadRecord(cfg.DataDir, result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Result.State != test.state || record.Result.Cleanup != "failed" || record.Result.Tasks[0].Typed.Targets[0].Outcome != test.outcome {
				t.Fatalf("record lost facts after cleanup failure: %+v", record.Result)
			}
			result.Tasks[0].Typed.Targets[0].Outcome = TargetAmbiguous
			if r.Snapshot().Tasks[0].Typed.Targets[0].Outcome != test.outcome {
				t.Fatal("returned result aliases confirmed facts after cleanup failure")
			}
		})
	}
}

func TestRuntimePreservesSourceWhenAdapterCompilesParams(t *testing.T) {
	original := json.RawMessage(`{"value":9007199254740993,"original":{"future":9007199254740993}}`)
	compiled := json.RawMessage(`{"value":9007199254740993,"compiled":true}`)
	cfg := runtimeConfig(t, "success")
	cfg.Adapters = []TaskAdapter{runtimeAdapterFixture{prepare: func(task Task) (PreparedTask, error) {
		return PreparedTask{Params: compiled, Targets: []string{"goal"}, Reducer: runtimeReducerFunc(func(Observation) (TypedUpdate, error) {
			return TypedUpdate{Targets: []TargetResult{{ID: "goal", Outcome: TargetAchieved}}}, nil
		})}, nil
	}}}
	rt := newRuntime(t, cfg)
	plan := typedRuntimePlan(t)
	plan.Tasks[0].Params = append(json.RawMessage(nil), original...)
	r, err := rt.Run(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	plan.Tasks[0].Params[0] = '['
	_, result := waitRuntimeRun(t, r)
	if result.State != Succeeded || string(r.plan.Tasks[0].Params) != string(compiled) {
		t.Fatalf("compiled Core input was not used: %+v %s", result, r.plan.Tasks[0].Params)
	}
	record, err := ReadRecord(cfg.DataDir, result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if string(record.Plan.Tasks[0].Params) != string(original) {
		t.Fatalf("record lost original adapter input: %s", record.Plan.Tasks[0].Params)
	}
}

func TestRuntimeTypedFactPersistsBeforeChainAndRecovers(t *testing.T) {
	cfg := runtimeConfig(t, "stop")
	cfg.Adapters = []TaskAdapter{fixtureOutcomeAdapter(TargetAchieved)}
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), typedRuntimePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-r.Events():
			if event.Type == "typed" {
				goto persisted
			}
		case <-r.Done():
			t.Fatalf("terminated before fact: %+v", r.Snapshot())
		case <-timer.C:
			t.Fatal("typed fact was not published")
		}
	}

persisted:
	record, err := ReadRecord(cfg.DataDir, r.Snapshot().RunID)
	if err != nil {
		t.Fatal(err)
	}
	if terminal(record.Result.State) || terminal(record.Result.Tasks[0].State) || record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
		t.Fatalf("in-flight fact was not durable: %+v", record.Result)
	}
	recoveryCfg := runtimeConfig(t, "crash")
	s, err := openStore(recoveryCfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(record); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	_ = newRuntime(t, recoveryCfg)
	recovered, err := ReadRecord(recoveryCfg.DataDir, record.Result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Result.State != Unknown || recovered.Result.Code != "host_interrupted" || recovered.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
		t.Fatalf("recovery lost confirmed fact: %+v", recovered.Result)
	}
	if err := r.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func typedObservationHarness(t *testing.T, reducer TaskReducer) *Run {
	t.Helper()
	plan := typedRuntimePlan(t)
	plan.Tasks = plan.Tasks[:1]
	prepared, err := PreparePlan(plan, fixtureAdapter(reducer))
	if err != nil {
		t.Fatal(err)
	}
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() { cancel(); _ = s.close() })
	prepared.Tasks[0].CoreTaskID, prepared.Tasks[0].State = 101, Started
	return &Run{runtime: &Runtime{store: s}, plan: prepared.Plan, sourcePlan: clonePlan(plan),
		result: Result{SchemaVersion: SchemaVersion, RunID: newRunID(), State: Started, Cleanup: "pending", Tasks: prepared.Tasks},
		events: make(chan Event, runEventMessages), done: make(chan struct{}), changed: make(chan struct{}, 1),
		commands: ctx, cancelCommands: cancel, taskIndex: map[int32]int{101: 0}, reducers: prepared.reducers, startIssued: true}
}

func sendTypedCallback(t *testing.T, r *Run, messageID int32, raw string) {
	t.Helper()
	m, err := protocol.NewMessage(protocol.Event, protocol.CoreEvent, 0, protocol.Callback{MessageID: messageID, CoreSequence: 1, JSON: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	r.observe(m)
}

func TestRuntimeTypedInvalidAndConflictingUpdatesPreserveFacts(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		upd  TypedUpdate
		code string
	}{
		{"conflict", `{"taskid":101}`, TypedUpdate{Targets: []TargetResult{{ID: "goal", Outcome: TargetFailed}}}, "typed_conflict"},
		{"oversized", `{"taskid":101}`, TypedUpdate{Summary: json.RawMessage(`{"value":"` + strings.Repeat("x", MaxTypedSummaryBytes) + `"}`)}, "typed_result_overflow"},
		{"malformed", `{"taskid":101,"taskid":101}`, TypedUpdate{}, "typed_invalid_event"},
		{"duplicatechain", `{"taskid":101,"taskchain":"FutureTask","taskchain":"Other"}`, TypedUpdate{}, "typed_invalid_event"},
		{"unassociated", `{"taskid":999,"taskchain":"FutureTask"}`, TypedUpdate{}, "typed_invalid_event"},
		{"badformat", `{"taskid":101}`, TypedUpdate{Summary: json.RawMessage(`{"nested":{"duplicate":1,"duplicate":2}}`)}, "typed_invalid_event"},
	} {
		t.Run(test.name, func(t *testing.T) {
			update := TypedUpdate{Targets: []TargetResult{{ID: "goal", Outcome: TargetAchieved, Details: json.RawMessage(`{"confirmed":true}`)}}}
			r := typedObservationHarness(t, runtimeReducerFunc(func(Observation) (TypedUpdate, error) { return update, nil }))
			sendTypedCallback(t, r, 20003, `{"taskid":101}`)
			update = test.upd
			sendTypedCallback(t, r, 20003, test.raw)
			if r.fatal != test.code || r.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
				t.Fatalf("rejected update altered facts: fatal=%s result=%+v", r.fatal, r.Snapshot())
			}
			record, err := ReadRecord(filepath.Dir(r.runtime.store.dir), r.result.RunID)
			if err != nil || record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
				t.Fatalf("durable fact changed: %+v %v", record, err)
			}
		})
	}
}

func TestRuntimeTypedLateCallbacksCannotChangeSettledFacts(t *testing.T) {
	calls := 0
	r := typedObservationHarness(t, runtimeReducerFunc(func(Observation) (TypedUpdate, error) {
		calls++
		return TypedUpdate{Targets: []TargetResult{{ID: "goal", Outcome: TargetAchieved}}}, nil
	}))
	sendTypedCallback(t, r, 20003, `{"taskid":101}`)
	sendTypedCallback(t, r, 10002, `{"taskid":101}`)
	settledCalls := calls
	sendTypedCallback(t, r, 20003, `{"taskid":101,"future":true}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)
	sendTypedCallback(t, r, 10000, `{"taskid":101}`)
	if calls != settledCalls || r.proof != Succeeded || r.fatal != "" || r.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
		t.Fatalf("late callback changed result: calls=%d proof=%s fatal=%s", calls, r.proof, r.fatal)
	}
}

func TestRuntimeTypedWriteFailureDoesNotPublishUndurableFact(t *testing.T) {
	r := typedObservationHarness(t, runtimeReducerFunc(func(Observation) (TypedUpdate, error) {
		return TypedUpdate{Targets: []TargetResult{{ID: "goal", Outcome: TargetAchieved}}}, nil
	}))
	if err := os.RemoveAll(r.runtime.store.dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.runtime.store.dir, []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	sendTypedCallback(t, r, 20003, `{"taskid":101}`)
	if r.fatal != "record_write_failed" || r.proof != "" || r.Snapshot().Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
		t.Fatalf("write failure promoted or erased fact: fatal=%s result=%+v", r.fatal, r.Snapshot())
	}
	for len(r.events) > 0 {
		if e := <-r.events; e.Type == "typed" {
			t.Fatal("undurable fact was published as typed event")
		}
	}
}

func TestRuntimeTypedQueueHasIndependentCountAndByteBounds(t *testing.T) {
	for _, large := range []bool{false, true} {
		t.Run(fmt.Sprint(large), func(t *testing.T) {
			r := typedObservationHarness(t, runtimeReducerFunc(func(Observation) (TypedUpdate, error) { return TypedUpdate{}, nil }))
			typed := cloneTypedResult(r.result.Tasks[0].Typed)
			if large {
				typed.Targets = nil
				for i := 0; i < 20; i++ {
					typed.Targets = append(typed.Targets, TargetResult{ID: fmt.Sprint(i), Outcome: TargetAchieved, Details: json.RawMessage(`{"value":"` + strings.Repeat("x", 3000) + `"}`)})
				}
				typed.State, typed.Code = typedState(typed)
				if err := validateTypedResult(typed); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i <= MaxTypedEventMessages && r.fatal == ""; i++ {
				r.emit(Event{Type: "typed", Typed: typed})
			}
			if r.fatal != "typed_event_overflow" || r.typedEventCount > MaxTypedEventMessages || r.typedEventBytes > MaxTypedEventRetainedBytes {
				t.Fatalf("typed queue exceeded independent bounds: fatal=%s count=%d bytes=%d", r.fatal, r.typedEventCount, r.typedEventBytes)
			}
			if large && r.typedEventCount >= MaxTypedEventMessages {
				t.Fatal("byte budget did not bound queue independently")
			}
			for len(r.events) > 0 {
				<-r.events
			}
			r.emit(Event{Type: "typed", Typed: typed})
			if r.typedEventCount != 1 {
				t.Fatalf("consumed typed entries still counted: %d", r.typedEventCount)
			}
		})
	}
}

func TestRuntimeTypedReducerAndConcurrentCloseKeepCleanupIndependent(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	cfg := runtimeConfig(t, "stop")
	cfg.Adapters = []TaskAdapter{fixtureAdapter(runtimeReducerFunc(func(Observation) (TypedUpdate, error) {
		close(entered)
		<-release
		return TypedUpdate{Targets: []TargetResult{{ID: "goal", Outcome: TargetAchieved}}}, nil
	}))}
	rt := newRuntime(t, cfg)
	r, err := rt.Run(context.Background(), typedRuntimePlan(t))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("reducer did not enter")
	}
	snapshotDone := make(chan struct{})
	go func() { _ = r.Snapshot(); close(snapshotDone) }()
	select {
	case <-snapshotDone:
	case <-time.After(time.Second):
		t.Fatal("reducer held the result lock")
	}
	var wg sync.WaitGroup
	wg.Go(func() { _ = r.Stop(context.Background()) })
	wg.Go(func() { _ = rt.Close(context.Background()) })
	close(release)
	wg.Wait()
	// No consumer has drained Events. Closing and reaping must still finish.
	result := r.Snapshot()
	if result.State != Stopped || result.Cleanup != "complete" || result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
		t.Fatalf("concurrent close lost typed fact: %+v", result)
	}
}
