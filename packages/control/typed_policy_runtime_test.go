package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

func policyRuntimeAdapter(policy ObjectivePolicy, targets []string, reducer TaskReducer) TaskAdapter {
	return runtimeAdapterFixture{prepare: func(task Task) (PreparedTask, error) {
		return PreparedTask{Params: task.Params, Policy: policy, Targets: targets, Reducer: reducer}, nil
	}}
}

func TestRuntimeObjectiveNoneUsesOnlyChainFacts(t *testing.T) {
	for _, test := range []struct {
		mode  string
		state State
	}{
		{"success", Succeeded},
		{"failure", Failed},
		{"stop", Stopped},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cfg := runtimeConfig(t, test.mode)
			cfg.Adapters = []TaskAdapter{policyRuntimeAdapter(ObjectiveNone, nil, nil)}
			rt := newRuntime(t, cfg)
			run, err := rt.Run(context.Background(), typedRuntimePlan(t))
			if err != nil {
				t.Fatal(err)
			}
			if test.mode == "stop" {
				waitStarted(t, run)
				if err := run.Stop(context.Background()); err != nil {
					t.Fatal(err)
				}
			}
			events, result := waitRuntimeRun(t, run)
			if result.State != test.state || result.Cleanup != "complete" {
				t.Fatalf("input-only result = %+v", result)
			}
			if result.Tasks[0].Policy != ObjectiveNone || result.Tasks[0].Typed != nil || len(run.reducers) != 0 {
				t.Fatalf("input-only task gained target interpretation: %+v", result.Tasks[0])
			}
			if result.Tasks[1].Policy != "" || result.Tasks[1].Typed != nil {
				t.Fatalf("raw task gained a policy or typed facts: %+v", result.Tasks[1])
			}
			for _, event := range events {
				if event.Type == "typed" || event.Typed != nil {
					t.Fatalf("input-only task emitted typed facts: %+v", event)
				}
			}
			record, err := ReadRecord(cfg.DataDir, result.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if record.Result.State != test.state || record.Result.Tasks[0].Policy != ObjectiveNone || record.Result.Tasks[0].Typed != nil {
				t.Fatalf("input-only record changed chain or target facts: %+v", record.Result)
			}
		})
	}
}

func objectivePolicyHarness(t *testing.T, targets []string, reducer TaskReducer) *Run {
	t.Helper()
	plan := typedRuntimePlan(t)
	plan.Tasks = plan.Tasks[:1]
	return newPreparedTypedHarness(t, plan, []int32{101}, true, policyRuntimeAdapter(ObjectiveAny, targets, reducer))
}

func TestRuntimeObjectiveAnyKeepsIndependentTargetFacts(t *testing.T) {
	for _, test := range []struct {
		name     string
		updates  []TargetResult
		state    State
		code     string
		outcomes []TargetOutcome
	}{
		{"failure then success", []TargetResult{{ID: "run-1", Outcome: TargetFailed}, {ID: "run-2", Outcome: TargetAchieved}}, Succeeded, "", []TargetOutcome{TargetFailed, TargetAchieved}},
		{"success then failure", []TargetResult{{ID: "run-1", Outcome: TargetAchieved}, {ID: "run-2", Outcome: TargetFailed}}, Succeeded, "", []TargetOutcome{TargetAchieved, TargetFailed}},
		{"all failed", []TargetResult{{ID: "run-1", Outcome: TargetLocked}, {ID: "run-2", Outcome: TargetNotFound}}, Failed, "typed_failed", []TargetOutcome{TargetLocked, TargetNotFound}},
		{"success with unknown", []TargetResult{{ID: "run-1", Outcome: TargetAlreadySet}}, Succeeded, "", []TargetOutcome{TargetAlreadySet, TargetUnobserved}},
		{"failure with unknown", []TargetResult{{ID: "run-1", Outcome: TargetFailed}}, Unknown, "typed_incomplete", []TargetOutcome{TargetFailed, TargetUnobserved}},
		{"all unobserved", nil, Unknown, "typed_incomplete", []TargetOutcome{TargetUnobserved, TargetUnobserved}},
	} {
		t.Run(test.name, func(t *testing.T) {
			reducer := runtimeReducerFunc(func(o Observation) (TypedUpdate, error) {
				if o.MessageID != 20003 {
					return TypedUpdate{}, nil
				}
				var fields struct {
					Target TargetResult `json:"target"`
				}
				if err := json.Unmarshal(o.JSON, &fields); err != nil {
					return TypedUpdate{}, err
				}
				return TypedUpdate{Targets: []TargetResult{fields.Target}}, nil
			})
			run := objectivePolicyHarness(t, []string{"run-1", "run-2"}, reducer)
			for _, update := range test.updates {
				raw, err := json.Marshal(struct {
					TaskID int32        `json:"taskid"`
					Target TargetResult `json:"target"`
				}{101, update})
				if err != nil {
					t.Fatal(err)
				}
				sendTypedCallback(t, run, 20003, string(raw))
			}
			sendTypedCallback(t, run, 10002, `{"taskid":101}`)
			sendTypedCallback(t, run, 3, `{"finished_tasks":[101]}`)
			result := run.Snapshot()
			if run.proof != test.state || run.proofCode != test.code || run.fatal != "" {
				t.Fatalf("run aggregate = %s/%s, fatal = %q", run.proof, run.proofCode, run.fatal)
			}
			if result.Tasks[0].State != Succeeded || result.Tasks[0].Policy != ObjectiveAny || result.Tasks[0].Typed.State != test.state {
				t.Fatalf("chain and objective aggregate = %+v", result.Tasks[0])
			}
			for i, outcome := range test.outcomes {
				if got := result.Tasks[0].Typed.Targets[i].Outcome; got != outcome {
					t.Fatalf("target %d = %s, want %s", i, got, outcome)
				}
			}
			if events := typedEventsIn(run); len(events) != len(test.updates) {
				t.Fatalf("typed event count = %d, want %d; chain completion must not create targets", len(events), len(test.updates))
			}
			record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), result.RunID)
			if err != nil || record.Result.Tasks[0].Typed.Policy != ObjectiveAny {
				t.Fatalf("policy was not persisted with target facts: %+v, %v", record, err)
			}
		})
	}
}

func achievedAnyRuntimeAdapter() TaskAdapter {
	return runtimeAdapterFixture{prepare: func(task Task) (PreparedTask, error) {
		observed := false
		return PreparedTask{Params: task.Params, Policy: ObjectiveAny, Targets: []string{"run-1", "run-2"}, Reducer: runtimeReducerFunc(func(o Observation) (TypedUpdate, error) {
			if observed || o.MessageID != 10001 {
				return TypedUpdate{}, nil
			}
			observed = true
			return TypedUpdate{Targets: []TargetResult{{ID: "run-1", Outcome: TargetAchieved, Details: json.RawMessage(`{"value":9007199254740993}`)}, {ID: "run-2", Outcome: TargetFailed}}}, nil
		})}, nil
	}}
}

func waitAnyPolicyFact(t *testing.T, run *Run) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-run.Events():
			if !ok {
				t.Fatalf("run ended before target achievement: %+v", run.Snapshot())
			}
			if event.Type == "typed" {
				if event.Typed == nil || event.Typed.State != Succeeded || event.Typed.Policy != ObjectiveAny {
					t.Fatalf("unexpected objective fact: %+v", event)
				}
				return
			}
		case <-timer.C:
			t.Fatal("target achievement was not observed")
		}
	}
}

func TestRuntimeObjectiveAnyDoesNotOverrideRunTermination(t *testing.T) {
	for _, test := range []struct {
		mode    string
		state   State
		cleanup string
	}{
		{"failure", Failed, "complete"},
		{"errorstopped", Failed, "complete"},
		{"stop", Stopped, "complete"},
		{"stopstuck", Unknown, ""},
		{"crash", Unknown, "failed"},
		{"closehang", Succeeded, "failed"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cfg := runtimeConfig(t, test.mode)
			cfg.Adapters = []TaskAdapter{achievedAnyRuntimeAdapter()}
			rt := newRuntime(t, cfg)
			run, err := rt.Run(context.Background(), typedRuntimePlan(t))
			if err != nil {
				t.Fatal(err)
			}
			if test.mode == "stop" || test.mode == "stopstuck" {
				waitAnyPolicyFact(t, run)
				stopErr := run.Stop(context.Background())
				if test.mode == "stopstuck" {
					var failure *protocol.Error
					if !errors.As(stopErr, &failure) || failure.Code != "stop_unconfirmed" {
						t.Fatalf("unconfirmed stop error = %v", stopErr)
					}
				} else if stopErr != nil {
					t.Fatal(stopErr)
				}
			}
			_, result := waitRuntimeRun(t, run)
			if result.State != test.state || test.cleanup != "" && result.Cleanup != test.cleanup {
				t.Fatalf("target achievement overrode run termination: %+v", result)
			}
			if test.mode == "stopstuck" && (result.Code != "stop_timeout" || (result.Cleanup != "complete" && result.Cleanup != "failed")) {
				t.Fatalf("unconfirmed stop lost its deadline or cleanup result: %+v", result)
			}
			typed := result.Tasks[0].Typed
			if typed == nil || typed.State != Succeeded || typed.Policy != ObjectiveAny || typed.Targets[0].Outcome != TargetAchieved || typed.Targets[1].Outcome != TargetFailed {
				t.Fatalf("run termination erased independent target facts: %+v", typed)
			}
			record, err := ReadRecord(cfg.DataDir, result.RunID)
			if err != nil || record.Result.State != test.state || record.Result.Tasks[0].Typed.State != Succeeded {
				t.Fatalf("record lost target/run separation: %+v, %v", record, err)
			}
		})
	}
}

func TestRuntimeObjectiveAnyPreservesFactsOnLostOrInvalidEvents(t *testing.T) {
	for _, fault := range []string{"lost events", "conflicting target", "result budget"} {
		t.Run(fault, func(t *testing.T) {
			update := TypedUpdate{Targets: []TargetResult{{ID: "run-1", Outcome: TargetAchieved}}}
			run := objectivePolicyHarness(t, []string{"run-1", "run-2"}, runtimeReducerFunc(func(o Observation) (TypedUpdate, error) {
				if o.MessageID == 20003 {
					return update, nil
				}
				return TypedUpdate{}, nil
			}))
			sendTypedCallback(t, run, 20003, `{"taskid":101}`)
			code := "event_overflow"
			switch fault {
			case "lost events":
				m, err := protocol.NewMessage(protocol.Event, protocol.DiagnosticsEvent, 0, protocol.InstanceDiagnostics{LostEvents: 1})
				if err != nil {
					t.Fatal(err)
				}
				run.observe(m)
			case "conflicting target":
				code = "typed_conflict"
				update = TypedUpdate{Targets: []TargetResult{{ID: "run-1", Outcome: TargetFailed}}}
				sendTypedCallback(t, run, 20003, `{"taskid":101}`)
			case "result budget":
				code = "typed_result_overflow"
				update = TypedUpdate{Summary: json.RawMessage(`{"value":"` + strings.Repeat("x", MaxTypedSummaryBytes) + `"}`)}
				sendTypedCallback(t, run, 20003, `{"taskid":101}`)
			}
			sendTypedCallback(t, run, 10002, `{"taskid":101}`)
			sendTypedCallback(t, run, 3, `{"finished_tasks":[101]}`)
			if run.fatal != code || run.proof != "" {
				t.Fatalf("achieved target hid a fault: fatal=%s, proof=%s", run.fatal, run.proof)
			}
			record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), run.Snapshot().RunID)
			if err != nil || record.Result.Tasks[0].Typed.State != Succeeded || record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
				t.Fatalf("fault destroyed durable target fact: %+v, %v", record, err)
			}
		})
	}
}

func TestRuntimeObjectiveAnyRejectsSameTargetConflictInEitherOrder(t *testing.T) {
	for _, test := range []struct {
		name   string
		first  TargetOutcome
		second TargetOutcome
		state  State
	}{
		{"success then failure", TargetAchieved, TargetFailed, Succeeded},
		{"failure then success", TargetFailed, TargetAchieved, Unknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			original := TargetResult{ID: "run-1", Outcome: test.first, Code: "first_fact", Details: json.RawMessage(`{"observed":1}`)}
			update := TypedUpdate{Targets: []TargetResult{original}}
			run := objectivePolicyHarness(t, []string{"run-1", "run-2"}, runtimeReducerFunc(func(o Observation) (TypedUpdate, error) {
				if o.MessageID == 20003 {
					return update, nil
				}
				return TypedUpdate{}, nil
			}))
			sendTypedCallback(t, run, 20003, `{"taskid":101}`)
			if events := typedEventsIn(run); len(events) != 1 {
				t.Fatalf("first fact published %d typed events, want 1", len(events))
			}
			update = TypedUpdate{Targets: []TargetResult{{ID: "run-1", Outcome: test.second, Code: "conflicting_fact"}}}
			sendTypedCallback(t, run, 20003, `{"taskid":101}`)
			sendTypedCallback(t, run, 10002, `{"taskid":101}`)
			sendTypedCallback(t, run, 3, `{"finished_tasks":[101]}`)
			if run.fatal != "typed_conflict" || run.proof != "" {
				t.Fatalf("same-target conflict established a run result: fatal=%s, proof=%s", run.fatal, run.proof)
			}
			typed := run.Snapshot().Tasks[0].Typed
			if typed.State != test.state || !sameTarget(typed.Targets[0], original) || typed.Targets[1].Outcome != TargetUnobserved {
				t.Fatalf("conflict replaced or invented a target fact: %+v", typed)
			}
			if events := typedEventsIn(run); len(events) != 0 {
				t.Fatalf("conflict published %d further typed events", len(events))
			}
			record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), run.Snapshot().RunID)
			if err != nil {
				t.Fatal(err)
			}
			durable := record.Result.Tasks[0].Typed
			if durable.Policy != ObjectiveAny || durable.State != test.state || !sameTarget(durable.Targets[0], original) || durable.Targets[1].Outcome != TargetUnobserved {
				t.Fatalf("conflict changed durable target facts: %+v", durable)
			}
		})
	}
}

func TestRuntimeObjectiveAnyEventBudgetStillStopsAchievedRun(t *testing.T) {
	const count = MaxTypedEventMessages + 3
	targets := make([]string, count)
	for i := range targets {
		targets[i] = fmt.Sprintf("run-%02d", i)
	}
	next := 0
	run := objectivePolicyHarness(t, targets, runtimeReducerFunc(func(Observation) (TypedUpdate, error) {
		target := targets[next]
		next++
		return TypedUpdate{Targets: []TargetResult{{ID: target, Outcome: TargetAchieved}}}, nil
	}))
	id := int32(101)
	for i := range targets {
		run.observeTyped(protocol.Callback{MessageID: 20003, CoreSequence: uint64(i + 1), JSON: json.RawMessage(`{"taskid":101}`)}, &id, nil)
	}
	if run.fatal != "typed_event_overflow" || run.typedEventCount != MaxTypedEventMessages || run.proof != "" {
		t.Fatalf("achieved target bypassed event budget: fatal=%s, count=%d, proof=%s", run.fatal, run.typedEventCount, run.proof)
	}
	result := run.Snapshot()
	if result.Tasks[0].Typed.State != Succeeded || result.Tasks[0].Typed.Targets[count-1].Outcome != TargetUnobserved {
		t.Fatalf("overflow changed or invented facts: %+v", result.Tasks[0].Typed)
	}
	record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), result.RunID)
	if err != nil || record.Result.Tasks[0].Typed.State != Succeeded || record.Result.Tasks[0].Typed.Targets[count-1].Outcome != TargetUnobserved {
		t.Fatalf("overflow changed durable facts: %+v, %v", record, err)
	}
}
