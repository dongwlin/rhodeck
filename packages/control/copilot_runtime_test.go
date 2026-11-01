package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

func copilotRuntimePlan(t *testing.T, params ...string) Plan {
	t.Helper()
	plan := runtimePlan(t)
	ref := NewCopilotAdapter().Descriptor()
	plan.Tasks = make([]Task, len(params))
	for i, raw := range params {
		plan.Tasks[i] = Task{ID: fmt.Sprintf("copilot-%d", i), Type: "Copilot", Adapter: &ref, Params: json.RawMessage(raw)}
	}
	return plan
}

func copilotRuntimeLoad(taskID, entryID int32, filename string) string {
	raw, _ := json.Marshal(struct {
		TaskID    int32  `json:"taskid"`
		TaskChain string `json:"taskchain"`
		Subtask   string `json:"subtask"`
		What      string `json:"what"`
		Details   any    `json:"details"`
	}{taskID, "Copilot", "MultiCopilotTask", "CopilotListLoadTaskFileSuccess", struct {
		ID       int32  `json:"id"`
		Filename string `json:"file_name"`
	}{entryID, filename}})
	return string(raw)
}

func copilotRuntimeNode(taskID int32, node string) string {
	return fmt.Sprintf(`{"taskid":%d,"taskchain":"Copilot","subtask":"ProcessTask","first":["Copilot@WaitUntilEndOfAction"],"details":{"task":%q}}`, taskID, node)
}

func copilotRuntimeFact(t *testing.T, run *Run, taskID, entryID int32, filename, node string) {
	t.Helper()
	sendTypedCallback(t, run, 20003, copilotRuntimeLoad(taskID, entryID, filename))
	sendTypedCallback(t, run, 20001, copilotRuntimeNode(taskID, node))
}

func TestCopilotRuntimeKeepsEntryAndCoreTaskIdentities(t *testing.T) {
	plan := copilotRuntimePlan(t,
		`{"copilot_list":[{"id":7,"filename":"same.json"},{"id":8,"filename":"same.json","is_raid":true}]}`,
		`{"copilot_list":[{"id":7,"filename":"same.json"}]}`)
	run := newPreparedTypedHarness(t, plan, []int32{101, 202}, true, NewCopilotAdapter())
	sendTypedCallback(t, run, 20003, copilotRuntimeLoad(101, 7, "same.json"))
	// Navigation can run ProcessTask nodes too; it is not the battle settlement.
	sendTypedCallback(t, run, 20001, `{"taskid":101,"taskchain":"Copilot","subtask":"ProcessTask","first":["StageNavigationBegin"],"details":{"task":"StageDrops-Stars-3"}}`)
	if got := run.Snapshot().Tasks[0].Typed.Targets[0].Outcome; got != TargetUnobserved {
		t.Fatalf("navigation created a battle fact: %s", got)
	}
	sendTypedCallback(t, run, 20001, copilotRuntimeNode(101, "StageDrops-Stars-3"))
	first := run.Snapshot()
	if first.Tasks[0].Typed.Targets[0].ID != "copilot:7" || first.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved ||
		first.Tasks[0].Typed.Targets[1].Outcome != TargetUnobserved || first.Tasks[1].Typed.Targets[0].Outcome != TargetUnobserved {
		t.Fatalf("entry or Core task identity leaked: %+v", first.Tasks)
	}
	copilotRuntimeFact(t, run, 202, 7, "same.json", "StageDrops-Stars-3")
	copilotRuntimeFact(t, run, 101, 8, "same.json", "StageDrops-Stars-Adverse")
	result := run.Snapshot()
	if result.Tasks[0].Typed.Policy != ObjectiveAll || result.Tasks[0].Typed.State != Succeeded || result.Tasks[1].Typed.State != Succeeded {
		t.Fatalf("independent entries were not achieved: %+v", result.Tasks)
	}
	if got := result.Tasks[0].Typed.Targets[1]; got.ID != "copilot:8" || got.Code != "adverse_clear" || got.Outcome != TargetAchieved {
		t.Fatalf("raid entry fact = %+v", got)
	}
	events := typedEventsIn(run)
	if len(events) != 3 || events[0].TaskID != "copilot-0" || events[0].CoreTaskID != 101 || events[1].TaskID != "copilot-1" || events[1].CoreTaskID != 202 {
		t.Fatalf("typed event associations = %+v", events)
	}
	record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(record.Result.Tasks, result.Tasks) || !reflect.DeepEqual(record.Plan, plan) {
		t.Fatalf("record changed target facts or original plan: %+v", record)
	}
	if !reflect.DeepEqual(events[2].Typed, result.Tasks[0].Typed) {
		t.Fatalf("last typed event differs from snapshot: %+v", events[2])
	}
	events[2].Typed.Targets[1].Outcome = TargetFailed
	events[2].Typed.Targets[1].Details[0] = '['
	if !reflect.DeepEqual(run.Snapshot().Tasks, result.Tasks) {
		t.Fatal("typed event aliases the snapshot")
	}
}

func TestCopilotRuntimeDoesNotGuessMissingEntryScope(t *testing.T) {
	for _, raw := range []string{
		`{"taskid":101,"taskchain":"Copilot","subtask":"MultiCopilotTask","what":"CopilotListLoadTaskFileSuccess","details":{"file_name":"same.json"}}`,
		copilotRuntimeLoad(101, 99, "same.json"),
		copilotRuntimeLoad(101, 8, "different.json"),
	} {
		t.Run(raw, func(t *testing.T) {
			plan := copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"same.json"},{"id":8,"filename":"same.json","is_raid":true}]}`)
			run := newPreparedTypedHarness(t, plan, []int32{101}, true, NewCopilotAdapter())
			copilotRuntimeFact(t, run, 101, 7, "same.json", "StageDrops-Stars-3")
			sendTypedCallback(t, run, 20003, raw)
			sendTypedCallback(t, run, 20001, copilotRuntimeNode(101, "FightMissionFailed"))
			if run.fatal != "" {
				t.Fatalf("missing scope overwrote a confirmed fact: %s", run.fatal)
			}
			sendTypedCallback(t, run, 10002, `{"taskid":101}`)
			sendTypedCallback(t, run, 3, `{"finished_tasks":[101]}`)
			result := run.Snapshot()
			if run.proof != Unknown || run.proofCode != "typed_incomplete" || result.Tasks[0].State != Succeeded ||
				result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved || result.Tasks[0].Typed.Targets[1].Outcome == TargetAchieved || result.Tasks[0].Typed.Targets[1].Outcome == TargetFailed {
				t.Fatalf("unassociated evidence became a clear or failure: %+v, proof=%s/%s", result.Tasks[0], run.proof, run.proofCode)
			}
		})
	}
}

func TestCopilotRuntimeLoadStartInvalidatesPreviousEntryScope(t *testing.T) {
	plan := copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"same.json"},{"id":8,"filename":"same.json","is_raid":true}]}`)
	run := newPreparedTypedHarness(t, plan, []int32{101}, true, NewCopilotAdapter())
	copilotRuntimeFact(t, run, 101, 7, "same.json", "StageDrops-Stars-3")
	original := run.Snapshot().Tasks[0].Typed.Targets[0]
	// The next plugin has started, but its successful load observation is absent.
	// Its subsequent battle must not inherit the previous entry's identity.
	sendTypedCallback(t, run, 20001, `{"taskid":101,"taskchain":"Copilot","subtask":"MultiCopilotTask"}`)
	sendTypedCallback(t, run, 20001, copilotRuntimeNode(101, "FightMissionFailed"))
	if run.fatal != "" || !sameTarget(run.Snapshot().Tasks[0].Typed.Targets[0], original) || run.Snapshot().Tasks[0].Typed.Targets[1].Outcome != TargetUnobserved {
		t.Fatalf("missing load inherited the previous entry: fatal=%s, targets=%+v", run.fatal, run.Snapshot().Tasks[0].Typed.Targets)
	}
	sendTypedCallback(t, run, 10002, `{"taskid":101}`)
	sendTypedCallback(t, run, 3, `{"finished_tasks":[101]}`)
	if run.proof != Unknown || run.proofCode != "typed_incomplete" {
		t.Fatalf("missing load was hidden by chain completion: %s/%s", run.proof, run.proofCode)
	}
}

func TestCopilotRuntimeDuplicateConflictAndUnknownTaskPreserveFacts(t *testing.T) {
	for _, fault := range []string{"conflict", "foreign task", "lost events"} {
		t.Run(fault, func(t *testing.T) {
			plan := copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"same.json"}]}`)
			run := newPreparedTypedHarness(t, plan, []int32{101}, true, NewCopilotAdapter())
			copilotRuntimeFact(t, run, 101, 7, "same.json", "StageDrops-Stars-3")
			original := run.Snapshot().Tasks[0].Typed.Targets[0]
			copilotRuntimeFact(t, run, 101, 7, "same.json", "StageDrops-Stars-3")
			if got := len(typedEventsIn(run)); got != 1 {
				t.Fatalf("duplicate fact emitted %d events", got)
			}
			code := "typed_conflict"
			switch fault {
			case "conflict":
				sendTypedCallback(t, run, 20001, copilotRuntimeNode(101, "FightMissionFailed"))
			case "foreign task":
				code = "typed_invalid_event"
				sendTypedCallback(t, run, 20003, copilotRuntimeLoad(999, 7, "same.json"))
			case "lost events":
				code = "event_overflow"
				message, err := protocol.NewMessage(protocol.Event, protocol.DiagnosticsEvent, 0, protocol.InstanceDiagnostics{LostEvents: 1})
				if err != nil {
					t.Fatal(err)
				}
				run.observe(message)
			}
			sendTypedCallback(t, run, 10002, `{"taskid":101}`)
			sendTypedCallback(t, run, 3, `{"finished_tasks":[101]}`)
			if run.fatal != code || run.proof != "" || !sameTarget(run.Snapshot().Tasks[0].Typed.Targets[0], original) {
				t.Fatalf("fault changed a confirmed fact: fatal=%s, proof=%s, target=%+v", run.fatal, run.proof, run.Snapshot().Tasks[0].Typed.Targets[0])
			}
			record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), run.Snapshot().RunID)
			if err != nil || !sameTarget(record.Result.Tasks[0].Typed.Targets[0], original) {
				t.Fatalf("fault changed durable facts: %+v, %v", record, err)
			}
		})
	}
}

func TestCopilotRuntimeSeparatesChainAndStageResults(t *testing.T) {
	for _, test := range []struct {
		name  string
		node  string
		state State
		code  string
	}{
		{"no settlement", "", Unknown, "typed_incomplete"},
		{"failed stage", "FightMissionFailed", Failed, "typed_failed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"same.json"},{"id":8,"filename":"same.json"}]}`)
			run := newPreparedTypedHarness(t, plan, []int32{101}, true, NewCopilotAdapter())
			copilotRuntimeFact(t, run, 101, 7, "same.json", "StageDrops-Stars-3")
			if test.node != "" {
				copilotRuntimeFact(t, run, 101, 8, "same.json", test.node)
			}
			sendTypedCallback(t, run, 10002, `{"taskid":101}`)
			sendTypedCallback(t, run, 3, `{"finished_tasks":[101]}`)
			result := run.Snapshot()
			if run.proof != test.state || run.proofCode != test.code || result.Tasks[0].State != Succeeded || result.Tasks[0].Typed.State != test.state {
				t.Fatalf("chain completion replaced stage evidence: %+v, proof=%s/%s", result.Tasks[0], run.proof, run.proofCode)
			}
		})
	}
}

// The process helper supplies lifecycle faults. This wrapper feeds known native
// callback shapes through the real reducer on its single observation consumer;
// it does not add a second event consumer or change the shared worker fixture.
type copilotLifecycleAdapter struct{}

func (copilotLifecycleAdapter) Descriptor() AdapterRef { return NewCopilotAdapter().Descriptor() }

func (copilotLifecycleAdapter) Prepare(task Task) (PreparedTask, error) {
	prepared, err := NewCopilotAdapter().Prepare(task)
	if err != nil {
		return PreparedTask{}, err
	}
	reducer := prepared.Reducer
	injected := false
	prepared.Reducer = runtimeReducerFunc(func(o Observation) (TypedUpdate, error) {
		if o.MessageID != 10001 || injected {
			return reducer.Observe(o)
		}
		injected = true
		if _, err := reducer.Observe(Observation{MessageID: 20003, CoreSequence: 2, JSON: json.RawMessage(copilotRuntimeLoad(101, 7, "9007199254740993.json"))}); err != nil {
			return TypedUpdate{}, err
		}
		return reducer.Observe(Observation{MessageID: 20001, CoreSequence: 3, JSON: json.RawMessage(copilotRuntimeNode(101, "StageDrops-Stars-3"))})
	})
	return prepared, nil
}

func waitCopilotRuntimeFact(t *testing.T, run *Run) {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event, ok := <-run.Events():
			if !ok {
				t.Fatalf("run ended before its stage fact: %+v", run.Snapshot())
			}
			if event.Type == "typed" {
				if event.Typed == nil || event.Typed.Targets[0].Outcome != TargetAchieved {
					t.Fatalf("unexpected stage fact: %+v", event)
				}
				return
			}
		case <-timer.C:
			t.Fatal("stage fact was not observed")
		}
	}
}

func TestCopilotRuntimeLifecyclePreservesClearFacts(t *testing.T) {
	for _, test := range []struct {
		mode    string
		state   State
		cleanup string
	}{
		{"success", Succeeded, "complete"},
		{"failure", Failed, "complete"},
		{"stop", Stopped, "complete"},
		{"stopstuck", Unknown, ""},
		{"crash", Unknown, "failed"},
		{"closehang", Succeeded, "failed"},
	} {
		t.Run(test.mode, func(t *testing.T) {
			cfg := runtimeConfig(t, test.mode)
			cfg.Adapters = []TaskAdapter{copilotLifecycleAdapter{}}
			rt := newRuntime(t, cfg)
			plan := copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"9007199254740993.json"}]}`)
			run, err := rt.Run(context.Background(), plan)
			if err != nil {
				t.Fatal(err)
			}
			if test.mode == "stop" || test.mode == "stopstuck" {
				waitCopilotRuntimeFact(t, run)
				err := run.Stop(context.Background())
				if test.mode == "stopstuck" {
					var failure *protocol.Error
					if !errors.As(err, &failure) || failure.Code != "stop_unconfirmed" {
						t.Fatalf("unconfirmed stop error = %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
			}
			_, result := waitRuntimeRun(t, run)
			if result.State != test.state || test.cleanup != "" && result.Cleanup != test.cleanup {
				t.Fatalf("clear fact changed lifecycle outcome: %+v", result)
			}
			if test.mode == "stopstuck" && result.Code != "stop_timeout" {
				t.Fatalf("unconfirmed stop lost its deadline: %+v", result)
			}
			record, err := ReadRecord(cfg.DataDir, result.RunID)
			if err != nil || record.Result.State != test.state || !reflect.DeepEqual(record.Result.Tasks, result.Tasks) || result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
				t.Fatalf("lifecycle fault erased clear facts: %+v, %v", record, err)
			}
		})
	}
}

func TestCopilotRuntimeRecoversFactsWithoutAdapterOrReplay(t *testing.T) {
	plan := copilotRuntimePlan(t, `{"copilot_list":[{"id":7,"filename":"same.json"},{"id":8,"filename":"same.json"}]}`)
	run := newPreparedTypedHarness(t, plan, []int32{101}, true, NewCopilotAdapter())
	copilotRuntimeFact(t, run, 101, 7, "same.json", "StageDrops-Stars-3")
	record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), run.Snapshot().RunID)
	if err != nil || terminal(record.Result.State) || record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
		t.Fatalf("in-flight clear was not persisted: %+v, %v", record, err)
	}
	recoveryDir := t.TempDir()
	s, err := openStore(recoveryDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.save(record); err != nil {
		t.Fatal(err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	cfg := runtimeConfig(t, "crash")
	cfg.DataDir = recoveryDir
	cfg.Worker.Executable = filepath.Join(t.TempDir(), "missing-worker")
	_ = newRuntime(t, cfg)
	recovered, err := ReadRecord(recoveryDir, record.Result.RunID)
	if err != nil || recovered.Result.State != Unknown || recovered.Result.Code != "host_interrupted" || recovered.Result.Cleanup != "unconfirmed" ||
		!reflect.DeepEqual(recovered.Result.Tasks[0].Typed, record.Result.Tasks[0].Typed) || !reflect.DeepEqual(recovered.Plan, plan) {
		t.Fatalf("recovery reinterpreted or lost clear evidence: %+v, %v", recovered, err)
	}
}

func TestCopilotRuntimeEventBudgetPreservesAcceptedFacts(t *testing.T) {
	entries := make([]struct {
		ID       int    `json:"id"`
		Filename string `json:"filename"`
	}, MaxTypedEventMessages+3)
	for i := range entries {
		entries[i].ID, entries[i].Filename = i, "same.json"
	}
	params, err := json.Marshal(struct {
		Entries any `json:"copilot_list"`
	}{entries})
	if err != nil {
		t.Fatal(err)
	}
	plan := copilotRuntimePlan(t, string(params))
	run := newPreparedTypedHarness(t, plan, []int32{101}, true, NewCopilotAdapter())
	id := int32(101)
	for i := range entries {
		run.observeTyped(protocol.Callback{MessageID: 20003, CoreSequence: uint64(2*i + 1), JSON: json.RawMessage(copilotRuntimeLoad(id, int32(i), "same.json"))}, &id, nil)
		run.observeTyped(protocol.Callback{MessageID: 20001, CoreSequence: uint64(2*i + 2), JSON: json.RawMessage(copilotRuntimeNode(id, "StageDrops-Stars-3"))}, &id, nil)
	}
	result := run.Snapshot()
	if run.fatal != "typed_event_overflow" || run.proof != "" || result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved || result.Tasks[0].Typed.Targets[len(entries)-1].Outcome != TargetUnobserved {
		t.Fatalf("event budget erased or manufactured stage facts: fatal=%s, proof=%s, targets=%+v", run.fatal, run.proof, result.Tasks[0].Typed.Targets)
	}
	record, err := ReadRecord(filepath.Dir(run.runtime.store.dir), result.RunID)
	if err != nil || !reflect.DeepEqual(record.Result.Tasks, result.Tasks) {
		t.Fatalf("event overflow changed durable stage facts: %+v, %v", record, err)
	}
}
