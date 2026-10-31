package control

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func operProgressRuntimePlan(t *testing.T, params ...string) Plan {
	t.Helper()
	ref := AdapterRef{Name: operProgressAdapterName, Version: operProgressVersion}
	plan := runtimePlan(t)
	plan.Tasks = make([]Task, len(params))
	for i, p := range params {
		plan.Tasks[i] = Task{ID: fmt.Sprintf("grow-%d", i), Type: operProgressTaskType, Adapter: &ref, Params: json.RawMessage(p)}
	}
	return plan
}

// newOperProgressRuntimeHarness drives the real monitor path (Run.observe)
// without launching a worker, so Core task ID association and persistence are
// exercised against the prepared OperProgress reducers.
func newOperProgressRuntimeHarness(t *testing.T, plan Plan, coreIDs []int32) *Run {
	t.Helper()
	prepared, err := PreparePlan(plan, NewOperProgressAdapter())
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
		startIssued:    true,
	}
}

func operProgressDrainEvents(r *Run) []Event {
	var events []Event
	for {
		select {
		case e := <-r.events:
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestOperProgressRuntimeAssociatesSameTypeTasksByCoreID(t *testing.T) {
	plan := operProgressRuntimePlan(t,
		`{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}]}`,
		`{"plans":[{"role":"Tank","name":"Gamma","skill_level":7}]}`)
	r := newOperProgressRuntimeHarness(t, plan, []int32{101, 202})
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`)

	result := r.Snapshot()
	if result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved || result.Tasks[0].Typed.Targets[1].Outcome != TargetUnobserved {
		t.Fatalf("first task typed = %+v", result.Tasks[0].Typed)
	}
	if result.Tasks[1].Typed.Targets[0].Outcome != TargetUnobserved {
		t.Fatalf("first task callback polluted the second task: %+v", result.Tasks[1].Typed)
	}
	typedEvents := 0
	for _, e := range operProgressDrainEvents(r) {
		if len(e.Details) != 0 {
			t.Fatal("default event disclosed the raw callback")
		}
		if e.Type != "typed" {
			continue
		}
		typedEvents++
		if e.TaskID != "grow-0" || e.CoreTaskID != 101 || e.Typed == nil {
			t.Fatalf("typed event association = %+v", e)
		}
		e.Typed.Targets[0].Outcome = TargetAmbiguous
		e.Typed.Targets[0].Details[0] = '['
	}
	if typedEvents != 1 {
		t.Fatalf("typed event count = %d", typedEvents)
	}
	snapshot := r.Snapshot()
	if got := snapshot.Tasks[0].Typed.Targets[0].Outcome; got != TargetAchieved {
		t.Fatalf("event aliases the snapshot: %s", got)
	}
	if details := snapshot.Tasks[0].Typed.Targets[0].Details; !strings.Contains(string(details), `"reported":2`) {
		t.Fatalf("event aliases snapshot details: %s", details)
	}

	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":202,"what":"OperProgressDetail","details":{"role":"Tank","name":"Gamma","result":"Success","result_detail":"Completed","skill_level":7}}`)
	result = r.Snapshot()
	if result.Tasks[1].Typed.Targets[0].Outcome != TargetAchieved || result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved {
		t.Fatalf("second task typed = %+v", result.Tasks)
	}
	if result.Tasks[1].Typed.Targets[0].Details == nil || !strings.Contains(string(result.Tasks[1].Typed.Targets[0].Details), `"reported":7`) {
		t.Fatalf("second task detail = %s", result.Tasks[1].Typed.Targets[0].Details)
	}

	// Core's summary merges mastery startups and reports aggregate success. It
	// must not turn the unobserved mastery target into success.
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressSummary","details":{"finished":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}],"success":2,"failed":0,"skipped":0}}`)
	result = r.Snapshot()
	if !strings.Contains(string(result.Tasks[0].Typed.Summary), `"targets":2`) ||
		!strings.Contains(string(result.Tasks[0].Typed.Summary), `"observed":1`) ||
		!strings.Contains(string(result.Tasks[0].Typed.Summary), `"unobserved":1`) {
		t.Fatalf("summary = %s", result.Tasks[0].Typed.Summary)
	}
	if result.Tasks[0].Typed.Targets[1].Outcome != TargetUnobserved || result.Tasks[0].Typed.State != Unknown || result.Tasks[0].Typed.Code != "typed_incomplete" {
		t.Fatalf("summary fabricated success: %+v", result.Tasks[0].Typed)
	}
	if len(result.Tasks[1].Typed.Summary) != 0 {
		t.Fatalf("summary crossed task identity: %s", result.Tasks[1].Typed.Summary)
	}

	record, err := ReadRecord(filepath.Dir(r.runtime.store.dir), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved ||
		record.Result.Tasks[1].Typed.Targets[0].Outcome != TargetAchieved ||
		!strings.Contains(string(record.Result.Tasks[0].Typed.Summary), `"observed":1`) {
		t.Fatalf("record lost typed facts: %+v", record.Result.Tasks)
	}
	if !strings.Contains(string(record.Plan.Tasks[0].Params), `"elite":2`) ||
		!strings.Contains(string(record.Plan.Tasks[0].Params), `"skill_mastery":[0,0,3]`) ||
		!strings.Contains(string(record.Plan.Tasks[1].Params), `"skill_level":7`) {
		t.Fatalf("record lost the original caller plan: %+v", record.Plan.Tasks)
	}
}

func TestOperProgressRuntimeKeepsTargetFailureAfterChainSuccess(t *testing.T) {
	plan := operProgressRuntimePlan(t, `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)
	r := newOperProgressRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Failed","result_detail":"ResourceInsufficient"}}`)
	sendTypedCallback(t, r, 10002, `{"taskchain":"OperProgress","taskid":101}`)
	// A late success for the same target arrives after the task chain settled.
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	if result.Tasks[0].State != Succeeded {
		t.Fatalf("native chain fact = %s", result.Tasks[0].State)
	}
	if result.Tasks[0].Typed.State != Failed || result.Tasks[0].Typed.Code != "typed_failed" {
		t.Fatalf("typed aggregate = %+v", result.Tasks[0].Typed)
	}
	got := result.Tasks[0].Typed.Targets[0]
	if got.Outcome != TargetFailed || got.Code != "resource_insufficient" {
		t.Fatalf("late success replaced the failure: %+v", got)
	}
	if r.proof != Failed || r.proofCode != "typed_failed" || r.fatal != "" {
		t.Fatalf("batch proof = %s/%s fatal=%s", r.proof, r.proofCode, r.fatal)
	}
	record, err := ReadRecord(filepath.Dir(r.runtime.store.dir), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Result.Tasks[0].State != Succeeded || record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetFailed {
		t.Fatalf("record lost the independent chain/business facts: %+v", record.Result.Tasks[0])
	}
}

func TestOperProgressRuntimeRejectsUnassociatedOrMalformedDetail(t *testing.T) {
	plan := operProgressRuntimePlan(t, `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)
	t.Run("unknown Core task ID", func(t *testing.T) {
		r := newOperProgressRuntimeHarness(t, plan, []int32{101})
		sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
			`{"taskchain":"OperProgress","taskid":999,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`)
		if r.fatal != "typed_invalid_event" {
			t.Fatalf("unassociated callback was accepted: %s", r.fatal)
		}
		for _, target := range r.Snapshot().Tasks[0].Typed.Targets {
			if target.Outcome != TargetUnobserved {
				t.Fatalf("unassociated callback created a fact: %+v", target)
			}
		}
	})
	t.Run("ambiguous detail preserves confirmed facts", func(t *testing.T) {
		r := newOperProgressRuntimeHarness(t, plan, []int32{101})
		sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
			`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`)
		sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
			`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","result_detail":"AlreadySatisfied","elite":2}}`)
		if r.fatal != "typed_invalid_event" {
			t.Fatalf("ambiguous detail was accepted: %s", r.fatal)
		}
		result := r.Snapshot()
		if result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved || result.Tasks[0].Typed.State != Succeeded {
			t.Fatalf("malformed callback destroyed a confirmed fact: %+v", result.Tasks[0].Typed)
		}
		for _, e := range operProgressDrainEvents(r) {
			if e.Type == "typed" && e.Typed.Targets[0].Outcome == TargetAmbiguous {
				t.Fatal("rejected update was published")
			}
		}
	})
}
