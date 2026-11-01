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
	"github.com/stretchr/testify/require"
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
			require.NoError(t, err)
			if test.mode == "stop" {
				waitStarted(t, run)
				require.NoError(t, run.Stop(context.Background()))
			}
			events, result := waitRuntimeRun(t, run)
			require.True(t, result.State == test.state && result.Cleanup == "complete", "input-only result = %+v", result)
			require.True(t, result.Tasks[0].Policy == ObjectiveNone && result.Tasks[0].Typed == nil && len(run.reducers) == 0,
				"input-only task gained target interpretation: %+v", result.Tasks[0])
			require.True(t, result.Tasks[1].Policy == "" && result.Tasks[1].Typed == nil,
				"raw task gained a policy or typed facts: %+v", result.Tasks[1])
			for _, event := range events {
				require.True(t, event.Type != "typed" && event.Typed == nil, "input-only task emitted typed facts: %+v", event)
			}
			record, err := ReadRecord(cfg.DataDir, result.RunID)
			require.NoError(t, err)
			require.True(t, record.Result.State == test.state && record.Result.Tasks[0].Policy == ObjectiveNone && record.Result.Tasks[0].Typed == nil,
				"input-only record changed chain or target facts: %+v", record.Result)
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
				require.NoError(t, err)
				sendTypedCallback(t, run, 20003, string(raw))
			}
			sendTypedCallback(t, run, 10002, `{"taskid":101}`)
			sendTypedCallback(t, run, 3, `{"finished_tasks":[101]}`)
			result := run.Snapshot()
			require.True(t, run.proof == test.state && run.proofCode == test.code && run.fatal == "",
				"run aggregate = %s/%s, fatal = %q", run.proof, run.proofCode, run.fatal)
			require.True(t, result.Tasks[0].State == Succeeded && result.Tasks[0].Policy == ObjectiveAny && result.Tasks[0].Typed.State == test.state,
				"chain and objective aggregate = %+v", result.Tasks[0])
			for i, outcome := range test.outcomes {
				got := result.Tasks[0].Typed.Targets[i].Outcome
				require.Equal(t, outcome, got, "target %d = %s, want %s", i, got, outcome)
			}
			events := typedEventsIn(run)
			require.Len(t, events, len(test.updates), "typed event count = %d, want %d; chain completion must not create targets", len(events), len(test.updates))
			record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), result.RunID)
			require.True(t, err == nil && record.Result.Tasks[0].Typed.Policy == ObjectiveAny,
				"policy was not persisted with target facts: %+v, %v", record, err)
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
				require.FailNow(t, fmt.Sprintf("run ended before target achievement: %+v", run.Snapshot()))
			}
			if event.Type == "typed" {
				require.True(t, event.Typed != nil && event.Typed.State == Succeeded && event.Typed.Policy == ObjectiveAny,
					"unexpected objective fact: %+v", event)
				return
			}
		case <-timer.C:
			require.FailNow(t, "target achievement was not observed")
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
			require.NoError(t, err)
			if test.mode == "stop" || test.mode == "stopstuck" {
				waitAnyPolicyFact(t, run)
				stopErr := run.Stop(context.Background())
				if test.mode == "stopstuck" {
					var failure *protocol.Error
					require.ErrorAs(t, stopErr, &failure, "unconfirmed stop error = %v", stopErr)
					require.Equal(t, "stop_unconfirmed", failure.Code, "unconfirmed stop error = %v", stopErr)
				} else {
					require.NoError(t, stopErr)
				}
			}
			_, result := waitRuntimeRun(t, run)
			require.True(t, result.State == test.state && (test.cleanup == "" || result.Cleanup == test.cleanup),
				"target achievement overrode run termination: %+v", result)
			if test.mode == "stopstuck" {
				require.True(t, result.Code == "stop_timeout" && (result.Cleanup == "complete" || result.Cleanup == "failed"),
					"unconfirmed stop lost its deadline or cleanup result: %+v", result)
			}
			typed := result.Tasks[0].Typed
			require.True(t, typed != nil && typed.State == Succeeded && typed.Policy == ObjectiveAny &&
				typed.Targets[0].Outcome == TargetAchieved && typed.Targets[1].Outcome == TargetFailed,
				"run termination erased independent target facts: %+v", typed)
			record, err := ReadRecord(cfg.DataDir, result.RunID)
			require.True(t, err == nil && record.Result.State == test.state && record.Result.Tasks[0].Typed.State == Succeeded,
				"record lost target/run separation: %+v, %v", record, err)
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
				require.NoError(t, err)
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
			require.True(t, run.fatal == code && run.proof == "", "achieved target hid a fault: fatal=%s, proof=%s", run.fatal, run.proof)
			record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), run.Snapshot().RunID)
			require.True(t, err == nil && record.Result.Tasks[0].Typed.State == Succeeded && record.Result.Tasks[0].Typed.Targets[0].Outcome == TargetAchieved,
				"fault destroyed durable target fact: %+v, %v", record, err)
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
			events := typedEventsIn(run)
			require.Len(t, events, 1, "first fact published %d typed events, want 1", len(events))
			update = TypedUpdate{Targets: []TargetResult{{ID: "run-1", Outcome: test.second, Code: "conflicting_fact"}}}
			sendTypedCallback(t, run, 20003, `{"taskid":101}`)
			sendTypedCallback(t, run, 10002, `{"taskid":101}`)
			sendTypedCallback(t, run, 3, `{"finished_tasks":[101]}`)
			require.True(t, run.fatal == "typed_conflict" && run.proof == "",
				"same-target conflict established a run result: fatal=%s, proof=%s", run.fatal, run.proof)
			typed := run.Snapshot().Tasks[0].Typed
			require.True(t, typed.State == test.state && sameTarget(typed.Targets[0], original) && typed.Targets[1].Outcome == TargetUnobserved,
				"conflict replaced or invented a target fact: %+v", typed)
			events = typedEventsIn(run)
			require.Empty(t, events, "conflict published %d further typed events", len(events))
			record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), run.Snapshot().RunID)
			require.NoError(t, err)
			durable := record.Result.Tasks[0].Typed
			require.True(t, durable.Policy == ObjectiveAny && durable.State == test.state && sameTarget(durable.Targets[0], original) && durable.Targets[1].Outcome == TargetUnobserved,
				"conflict changed durable target facts: %+v", durable)
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
	require.True(t, run.fatal == "typed_event_overflow" && run.typedEventCount == MaxTypedEventMessages && run.proof == "",
		"achieved target bypassed event budget: fatal=%s, count=%d, proof=%s", run.fatal, run.typedEventCount, run.proof)
	result := run.Snapshot()
	require.True(t, result.Tasks[0].Typed.State == Succeeded && result.Tasks[0].Typed.Targets[count-1].Outcome == TargetUnobserved,
		"overflow changed or invented facts: %+v", result.Tasks[0].Typed)
	record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), result.RunID)
	require.True(t, err == nil && record.Result.Tasks[0].Typed.State == Succeeded && record.Result.Tasks[0].Typed.Targets[count-1].Outcome == TargetUnobserved,
		"overflow changed durable facts: %+v, %v", record, err)
}
