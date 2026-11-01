package control_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"

	. "github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/adapters/switchtheme"
	"github.com/stretchr/testify/require"
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
	require.True(t, first != nil && second != nil, "typed results missing: %+v", result.Tasks)
	require.True(t, first.Targets[0].Outcome == TargetAlreadySet && first.Targets[0].Code == switchThemeCodeAlreadySet &&
		bytes.Equal(first.Targets[0].Details, json.RawMessage(`{"theme":"Beta"}`)), "first task typed = %+v", first)
	require.True(t, second.Targets[0].Outcome == TargetLocked && second.Targets[0].Code == switchThemeCodeLocked &&
		bytes.Equal(second.Targets[0].Details, json.RawMessage(`{"theme":"Gamma"}`)), "second task typed = %+v", second)
	// The second task's selection must not leak into the first reducer.
	require.True(t, bytes.Contains(first.Targets[0].Details, []byte("Beta")) && !bytes.Contains(first.Targets[0].Details, []byte("Gamma")),
		"cross-task selection pollution: %s", first.Targets[0].Details)

	typedEvents := 0
	for _, e := range switchThemeDrainEvents(r) {
		require.Empty(t, e.Details, "default event disclosed the raw callback: %+v", e)
		if e.Type != "typed" {
			continue
		}
		typedEvents++
		require.True(t, e.Typed != nil && len(e.Typed.Targets) == 1, "typed event = %+v", e)
		wantTask, wantCore := "theme-0", int32(101)
		if typedEvents == 2 {
			wantTask, wantCore = "theme-1", 202
		}
		require.True(t, e.TaskID == wantTask && e.CoreTaskID == wantCore, "typed event association = %+v", e)
		details := e.Typed.Targets[0].Details
		require.True(t, !bytes.Contains(details, []byte("rect")) && !bytes.Contains(details, []byte("score")),
			"typed event retained OCR geometry: %s", details)
		e.Typed.Targets[0].Outcome = TargetAmbiguous
		e.Typed.Targets[0].Details[0] = '['
	}
	require.Equal(t, 2, typedEvents, "typed event count = %d, want 2", typedEvents)
	snapshot := r.Snapshot()
	require.True(t, snapshot.Tasks[0].Typed.Targets[0].Outcome == TargetAlreadySet && snapshot.Tasks[1].Typed.Targets[0].Outcome == TargetLocked,
		"typed events alias snapshot facts")

	record, err := ReadRecord(filepath.Dir(RunRecordDirForTest(r)), result.RunID)
	require.NoError(t, err)
	require.True(t, record.Result.Tasks[0].Typed.Targets[0].Outcome == TargetAlreadySet &&
		record.Result.Tasks[1].Typed.Targets[0].Outcome == TargetLocked, "record lost typed facts: %+v", record.Result.Tasks)
	require.True(t, bytes.Contains(record.Plan.Tasks[0].Params, []byte(`"themes":["Alpha","Beta"]`)) &&
		bytes.Contains(record.Plan.Tasks[1].Params, []byte(`"themes":["Gamma"]`)),
		"record lost the original caller plan: %+v", record.Plan.Tasks)

	// Chain success cannot turn the locked task into a successful batch.
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":202}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101,202]}`)
	result = r.Snapshot()
	require.True(t, result.Tasks[0].State == Succeeded && result.Tasks[1].State == Succeeded, "native chain facts = %+v", result.Tasks)
	require.True(t, InspectRunForTest(r).Proof == Failed && InspectRunForTest(r).ProofCode == "typed_failed" && InspectRunForTest(r).Fatal == "",
		"batch proof = %s/%s fatal=%s", InspectRunForTest(r).Proof, InspectRunForTest(r).ProofCode, InspectRunForTest(r).Fatal)
}

func TestSwitchThemeRuntimeLockedSurvivesChainCompletion(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	require.Equal(t, Succeeded, result.Tasks[0].State, "native chain fact = %s", result.Tasks[0].State)
	require.True(t, result.Tasks[0].Typed.State == Failed && result.Tasks[0].Typed.Code == "typed_failed",
		"typed aggregate = %+v", result.Tasks[0].Typed)
	got := result.Tasks[0].Typed.Targets[0]
	require.True(t, got.Outcome == TargetLocked && got.Code == switchThemeCodeLocked && bytes.Equal(got.Details, json.RawMessage(`{"theme":"Alpha"}`)),
		"locked fact = %+v", got)
	require.True(t, InspectRunForTest(r).Proof == Failed && InspectRunForTest(r).ProofCode == "typed_failed" && InspectRunForTest(r).Fatal == "",
		"batch proof = %s/%s fatal=%s", InspectRunForTest(r).Proof, InspectRunForTest(r).ProofCode, InspectRunForTest(r).Fatal)

	// A late confirm after the settled chain must not replace the lock.
	switchThemeDrainEvents(r)
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeConfirm))
	require.True(t, InspectRunForTest(r).Fatal == "" && len(switchThemeDrainEvents(r)) == 0,
		"late confirm changed the settled run: fatal=%s", InspectRunForTest(r).Fatal)
	got = r.Snapshot().Tasks[0].Typed.Targets[0]
	require.Equal(t, TargetLocked, got.Outcome, "late confirm replaced the lock: %+v", got)
}

func TestSwitchThemeRuntimeConfirmationRemainsIncomplete(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeConfirm))
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	require.Equal(t, Succeeded, result.Tasks[0].State, "native chain fact = %s", result.Tasks[0].State)
	typed := result.Tasks[0].Typed
	require.True(t, typed.State == Unknown && typed.Code == "typed_incomplete", "confirmation aggregate = %+v", typed)
	got := typed.Targets[0]
	require.True(t, got.Outcome == TargetConfirmationObserved && got.Code == switchThemeCodeConfirmationObserved,
		"confirmation fact = %+v", got)
	require.True(t, bytes.Equal(got.Details, json.RawMessage(`{"theme":"Alpha"}`)), "confirmation details = %s", got.Details)
	require.True(t, InspectRunForTest(r).Proof == Unknown && InspectRunForTest(r).ProofCode == "typed_incomplete" && InspectRunForTest(r).Fatal == "",
		"batch proof = %s/%s fatal=%s", InspectRunForTest(r).Proof, InspectRunForTest(r).ProofCode, InspectRunForTest(r).Fatal)
	record, err := ReadRecord(filepath.Dir(RunRecordDirForTest(r)), result.RunID)
	require.NoError(t, err)
	require.True(t, record.Result.Tasks[0].Typed.Targets[0].Outcome == TargetConfirmationObserved && record.Result.State == Started,
		"record lost confirmation evidence: %+v", record.Result)
}

func TestSwitchThemeRuntimeChainCompletionIsNotThemeSuccess(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	// Start-only evidence and a terminal chain callback carry no selection.
	sendTypedCallback(t, r, 20001, switchThemeSelectCallback(t, 101, "Alpha"))
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	require.Equal(t, Succeeded, result.Tasks[0].State, "native chain fact = %s", result.Tasks[0].State)
	typed := result.Tasks[0].Typed
	require.True(t, typed.State == Unknown && typed.Code == "typed_incomplete", "chain completion fabricated typed success: %+v", typed)
	got := typed.Targets[0]
	require.True(t, got.Outcome == TargetUnobserved && got.Code == "" && len(got.Details) == 0, "unobserved target = %+v", got)
	require.True(t, InspectRunForTest(r).Proof == Unknown && InspectRunForTest(r).ProofCode == "typed_incomplete" && InspectRunForTest(r).Fatal == "",
		"batch proof = %s/%s fatal=%s", InspectRunForTest(r).Proof, InspectRunForTest(r).ProofCode, InspectRunForTest(r).Fatal)
}

func TestSwitchThemeRuntimeMalformedCallbackStopsWithoutFact(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted,
		`{"taskchain":"SwitchTheme","taskid":101,"details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":"Alpha","text":"Beta"}}}`)
	require.Equal(t, "typed_invalid_event", InspectRunForTest(r).Fatal, "malformed callback = %s", InspectRunForTest(r).Fatal)
	for _, e := range switchThemeDrainEvents(r) {
		require.NotEqual(t, "typed", e.Type, "rejected callback published a typed event: %+v", e)
	}
	for _, target := range r.Snapshot().Tasks[0].Typed.Targets {
		require.Equal(t, TargetUnobserved, target.Outcome, "malformed callback created a fact: %+v", target)
	}
}
