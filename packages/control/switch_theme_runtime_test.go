package control_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	. "github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/adapters/switchtheme"
)

func switchThemeRuntimePlan(t *testing.T, params ...string) Plan {
	t.Helper()
	ref := AdapterRef{Name: switchThemeAdapterName, Version: switchThemeVersion}
	plan := runtimePlan(t)
	plan.Tasks = make([]Task, len(params))
	for i, p := range params {
		plan.Tasks[i] = Task{ID: fmt.Sprintf("theme-%d", i), Type: switchThemeTaskType, Adapter: &ref, Params: json.RawMessage(p)}
	}
	return plan
}

// newSwitchThemeRuntimeHarness drives the real monitor path (Run.observe)
// without launching a worker, so Core task ID association, persistence and
// typed event publication are exercised against the prepared theme reducers.
func newSwitchThemeRuntimeHarness(t *testing.T, plan Plan, coreIDs []int32) *Run {
	t.Helper()
	return newPreparedTypedHarness(t, plan, coreIDs, true, switchtheme.New())
}

func switchThemeDrainEvents(r *Run) []Event {
	var events []Event
	for {
		select {
		case e := <-r.Events():
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestSwitchThemeRuntimeSameTypeTasksStayIsolated(t *testing.T) {
	plan := switchThemeRuntimePlan(t,
		switchThemeThemesJSON(t, "Alpha", "Beta"),
		switchThemeThemesJSON(t, "Gamma"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101, 202})

	// The selection OCR result is the only association for the chosen theme.
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 202, "Gamma"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 202, switchThemeNodeLocked))
	// Repeating the identical node is harmless: the stored fact stays and no
	// second typed event is published.
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 202, switchThemeNodeLocked))

	result := r.Snapshot()
	first, second := result.Tasks[0].Typed, result.Tasks[1].Typed
	if first == nil || second == nil {
		t.Fatalf("typed results missing: %+v", result.Tasks)
	}
	if first.Targets[0].Outcome != TargetAlreadySet || first.Targets[0].Code != switchThemeCodeAlreadySet ||
		!bytes.Equal(first.Targets[0].Details, json.RawMessage(`{"theme":"Beta"}`)) {
		t.Fatalf("first task typed = %+v", first)
	}
	if second.Targets[0].Outcome != TargetLocked || second.Targets[0].Code != switchThemeCodeLocked ||
		!bytes.Equal(second.Targets[0].Details, json.RawMessage(`{"theme":"Gamma"}`)) {
		t.Fatalf("second task typed = %+v", second)
	}
	// The second task's selection must not leak into the first reducer.
	if !bytes.Contains(first.Targets[0].Details, []byte("Beta")) || bytes.Contains(first.Targets[0].Details, []byte("Gamma")) {
		t.Fatalf("cross-task selection pollution: %s", first.Targets[0].Details)
	}

	typedEvents := 0
	for _, e := range switchThemeDrainEvents(r) {
		if len(e.Details) != 0 {
			t.Fatalf("default event disclosed the raw callback: %+v", e)
		}
		if e.Type != "typed" {
			continue
		}
		typedEvents++
		if e.Typed == nil || len(e.Typed.Targets) != 1 {
			t.Fatalf("typed event = %+v", e)
		}
		wantTask, wantCore := "theme-0", int32(101)
		if typedEvents == 2 {
			wantTask, wantCore = "theme-1", 202
		}
		if e.TaskID != wantTask || e.CoreTaskID != wantCore {
			t.Fatalf("typed event association = %+v", e)
		}
		details := e.Typed.Targets[0].Details
		if bytes.Contains(details, []byte("rect")) || bytes.Contains(details, []byte("score")) {
			t.Fatalf("typed event retained OCR geometry: %s", details)
		}
		e.Typed.Targets[0].Outcome = TargetAmbiguous
		e.Typed.Targets[0].Details[0] = '['
	}
	if typedEvents != 2 {
		t.Fatalf("typed event count = %d, want 2", typedEvents)
	}
	snapshot := r.Snapshot()
	if snapshot.Tasks[0].Typed.Targets[0].Outcome != TargetAlreadySet || snapshot.Tasks[1].Typed.Targets[0].Outcome != TargetLocked {
		t.Fatal("typed events alias snapshot facts")
	}

	record, err := ReadRecord(filepath.Dir(RunRecordDirForTest(r)), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAlreadySet ||
		record.Result.Tasks[1].Typed.Targets[0].Outcome != TargetLocked {
		t.Fatalf("record lost typed facts: %+v", record.Result.Tasks)
	}
	if !bytes.Contains(record.Plan.Tasks[0].Params, []byte(`"themes":["Alpha","Beta"]`)) ||
		!bytes.Contains(record.Plan.Tasks[1].Params, []byte(`"themes":["Gamma"]`)) {
		t.Fatalf("record lost the original caller plan: %+v", record.Plan.Tasks)
	}

	// Chain success cannot turn the locked task into a successful batch.
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":202}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101,202]}`)
	result = r.Snapshot()
	if result.Tasks[0].State != Succeeded || result.Tasks[1].State != Succeeded {
		t.Fatalf("native chain facts = %+v", result.Tasks)
	}
	if InspectRunForTest(r).Proof != Failed || InspectRunForTest(r).ProofCode != "typed_failed" || InspectRunForTest(r).Fatal != "" {
		t.Fatalf("batch proof = %s/%s fatal=%s", InspectRunForTest(r).Proof, InspectRunForTest(r).ProofCode, InspectRunForTest(r).Fatal)
	}
}

func TestSwitchThemeRuntimeLockedSurvivesChainCompletion(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	if result.Tasks[0].State != Succeeded {
		t.Fatalf("native chain fact = %s", result.Tasks[0].State)
	}
	if result.Tasks[0].Typed.State != Failed || result.Tasks[0].Typed.Code != "typed_failed" {
		t.Fatalf("typed aggregate = %+v", result.Tasks[0].Typed)
	}
	got := result.Tasks[0].Typed.Targets[0]
	if got.Outcome != TargetLocked || got.Code != switchThemeCodeLocked || !bytes.Equal(got.Details, json.RawMessage(`{"theme":"Alpha"}`)) {
		t.Fatalf("locked fact = %+v", got)
	}
	if InspectRunForTest(r).Proof != Failed || InspectRunForTest(r).ProofCode != "typed_failed" || InspectRunForTest(r).Fatal != "" {
		t.Fatalf("batch proof = %s/%s fatal=%s", InspectRunForTest(r).Proof, InspectRunForTest(r).ProofCode, InspectRunForTest(r).Fatal)
	}

	// A late confirm after the settled chain must not replace the lock.
	switchThemeDrainEvents(r)
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeConfirm))
	if InspectRunForTest(r).Fatal != "" || len(switchThemeDrainEvents(r)) != 0 {
		t.Fatalf("late confirm changed the settled run: fatal=%s", InspectRunForTest(r).Fatal)
	}
	if got := r.Snapshot().Tasks[0].Typed.Targets[0]; got.Outcome != TargetLocked {
		t.Fatalf("late confirm replaced the lock: %+v", got)
	}
}

func TestSwitchThemeRuntimeConfirmationRemainsIncomplete(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeConfirm))
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	if result.Tasks[0].State != Succeeded {
		t.Fatalf("native chain fact = %s", result.Tasks[0].State)
	}
	typed := result.Tasks[0].Typed
	if typed.State != Unknown || typed.Code != "typed_incomplete" {
		t.Fatalf("confirmation aggregate = %+v", typed)
	}
	got := typed.Targets[0]
	if got.Outcome != TargetConfirmationObserved || got.Code != switchThemeCodeConfirmationObserved {
		t.Fatalf("confirmation fact = %+v", got)
	}
	if !bytes.Equal(got.Details, json.RawMessage(`{"theme":"Alpha"}`)) {
		t.Fatalf("confirmation details = %s", got.Details)
	}
	if InspectRunForTest(r).Proof != Unknown || InspectRunForTest(r).ProofCode != "typed_incomplete" || InspectRunForTest(r).Fatal != "" {
		t.Fatalf("batch proof = %s/%s fatal=%s", InspectRunForTest(r).Proof, InspectRunForTest(r).ProofCode, InspectRunForTest(r).Fatal)
	}
	record, err := ReadRecord(filepath.Dir(RunRecordDirForTest(r)), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetConfirmationObserved || record.Result.State != Started {
		t.Fatalf("record lost confirmation evidence: %+v", record.Result)
	}
}

func TestSwitchThemeRuntimeChainCompletionIsNotThemeSuccess(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	// Start-only evidence and a terminal chain callback carry no selection.
	sendTypedCallback(t, r, 20001, switchThemeSelectCallback(t, 101, "Alpha"))
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	if result.Tasks[0].State != Succeeded {
		t.Fatalf("native chain fact = %s", result.Tasks[0].State)
	}
	typed := result.Tasks[0].Typed
	if typed.State != Unknown || typed.Code != "typed_incomplete" {
		t.Fatalf("chain completion fabricated typed success: %+v", typed)
	}
	if got := typed.Targets[0]; got.Outcome != TargetUnobserved || got.Code != "" || len(got.Details) != 0 {
		t.Fatalf("unobserved target = %+v", got)
	}
	if InspectRunForTest(r).Proof != Unknown || InspectRunForTest(r).ProofCode != "typed_incomplete" || InspectRunForTest(r).Fatal != "" {
		t.Fatalf("batch proof = %s/%s fatal=%s", InspectRunForTest(r).Proof, InspectRunForTest(r).ProofCode, InspectRunForTest(r).Fatal)
	}
}

func TestSwitchThemeRuntimeMalformedCallbackStopsWithoutFact(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted,
		`{"taskchain":"SwitchTheme","taskid":101,"details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":"Alpha","text":"Beta"}}}`)
	if InspectRunForTest(r).Fatal != "typed_invalid_event" {
		t.Fatalf("malformed callback = %s", InspectRunForTest(r).Fatal)
	}
	for _, e := range switchThemeDrainEvents(r) {
		if e.Type == "typed" {
			t.Fatalf("rejected callback published a typed event: %+v", e)
		}
	}
	for _, target := range r.Snapshot().Tasks[0].Typed.Targets {
		if target.Outcome != TargetUnobserved {
			t.Fatalf("malformed callback created a fact: %+v", target)
		}
	}
}
