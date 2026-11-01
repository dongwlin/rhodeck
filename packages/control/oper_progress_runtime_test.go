package control_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/adapters/operprogress"
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
	return newPreparedTypedHarness(t, plan, coreIDs, true, operprogress.New())
}

func operProgressDrainEvents(r *Run) []Event {
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

func TestOperProgressRuntimeAssociatesSameTypeTasksByCoreID(t *testing.T) {
	plan := operProgressRuntimePlan(t,
		`{"plans":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}]}`,
		`{"plans":[{"role":"Tank","name":"Gamma","skill_level":7}]}`)
	r := newOperProgressRuntimeHarness(t, plan, []int32{101, 202})
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`)

	result := r.Snapshot()
	require.False(t, result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved || result.Tasks[0].Typed.Targets[1].Outcome != TargetUnobserved, "first task typed = %+v", result.Tasks[0].Typed)
	require.False(t, result.Tasks[1].Typed.Targets[0].Outcome != TargetUnobserved, "first task callback polluted the second task: %+v", result.Tasks[1].Typed)
	typedEvents := 0
	for _, e := range operProgressDrainEvents(r) {
		require.Empty(t, e.Details, "default event disclosed the raw callback")
		if e.Type != "typed" {
			continue
		}
		typedEvents++
		require.False(t, e.TaskID != "grow-0" || e.CoreTaskID != 101 || e.Typed == nil, "typed event association = %+v", e)
		e.Typed.Targets[0].Outcome = TargetAmbiguous
		e.Typed.Targets[0].Details[0] = '['
	}
	require.Equal(t, 1, typedEvents, "typed event count = %d", typedEvents)
	snapshot := r.Snapshot()
	got := snapshot.Tasks[0].Typed.Targets[0].Outcome
	require.Equal(t, TargetAchieved, got, "event aliases the snapshot: %s", got)
	details := snapshot.Tasks[0].Typed.Targets[0].Details
	require.Contains(t, string(details), `"reported":2`, "event aliases snapshot details: %s", details)

	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":202,"what":"OperProgressDetail","details":{"role":"Tank","name":"Gamma","result":"Success","result_detail":"Completed","skill_level":7}}`)
	result = r.Snapshot()
	require.False(t, result.Tasks[1].Typed.Targets[0].Outcome != TargetAchieved || result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved, "second task typed = %+v", result.Tasks)
	require.False(t, result.Tasks[1].Typed.Targets[0].Details == nil || !strings.Contains(string(result.Tasks[1].Typed.Targets[0].Details), `"reported":7`), "second task detail = %s", result.Tasks[1].Typed.Targets[0].Details)

	// Core's summary merges mastery startups and reports aggregate success. It
	// must not turn the unobserved mastery target into success.
	sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
		`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressSummary","details":{"finished":[{"role":"Warrior","name":"Alpha","elite":2},{"role":"Caster","name":"Beta","skill_mastery":[0,0,3]}],"success":2,"failed":0,"skipped":0}}`)
	result = r.Snapshot()
	require.False(t, !strings.Contains(string(result.Tasks[0].Typed.Summary), `"targets":2`) ||
		!strings.Contains(string(result.Tasks[0].Typed.Summary), `"observed":1`) ||
		!strings.Contains(string(result.Tasks[0].Typed.Summary), `"unobserved":1`), "summary = %s", result.Tasks[0].Typed.Summary)
	require.False(t, result.Tasks[0].Typed.Targets[1].Outcome != TargetUnobserved || result.Tasks[0].Typed.State != Unknown || result.Tasks[0].Typed.Code != "typed_incomplete", "summary fabricated success: %+v", result.Tasks[0].Typed)
	require.Empty(t, result.Tasks[1].Typed.Summary, "summary crossed task identity: %s", result.Tasks[1].Typed.Summary)

	record, err := ReadRecord(filepath.Dir(RunRecordDirForTest(r)), result.RunID)
	require.NoError(t, err)
	require.False(t, record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved ||
		record.Result.Tasks[1].Typed.Targets[0].Outcome != TargetAchieved ||
		!strings.Contains(string(record.Result.Tasks[0].Typed.Summary), `"observed":1`), "record lost typed facts: %+v", record.Result.Tasks)
	require.False(t, !strings.Contains(string(record.Plan.Tasks[0].Params), `"elite":2`) ||
		!strings.Contains(string(record.Plan.Tasks[0].Params), `"skill_mastery":[0,0,3]`) ||
		!strings.Contains(string(record.Plan.Tasks[1].Params), `"skill_level":7`), "record lost the original caller plan: %+v", record.Plan.Tasks)
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
	require.Equal(t, Succeeded, result.Tasks[0].State, "native chain fact = %s", result.Tasks[0].State)
	require.False(t, result.Tasks[0].Typed.State != Failed || result.Tasks[0].Typed.Code != "typed_failed", "typed aggregate = %+v", result.Tasks[0].Typed)
	got := result.Tasks[0].Typed.Targets[0]
	require.False(t, got.Outcome != TargetFailed || got.Code != "resource_insufficient", "late success replaced the failure: %+v", got)
	require.False(t, InspectRunForTest(r).Proof != Failed || InspectRunForTest(r).ProofCode != "typed_failed" || InspectRunForTest(r).Fatal != "", "batch proof = %s/%s fatal=%s", InspectRunForTest(r).Proof, InspectRunForTest(r).ProofCode, InspectRunForTest(r).Fatal)
	record, err := ReadRecord(filepath.Dir(RunRecordDirForTest(r)), result.RunID)
	require.NoError(t, err)
	require.False(t, record.Result.Tasks[0].State != Succeeded || record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetFailed, "record lost the independent chain/business facts: %+v", record.Result.Tasks[0])
}

func TestOperProgressRuntimeRejectsUnassociatedOrMalformedDetail(t *testing.T) {
	plan := operProgressRuntimePlan(t, `{"plans":[{"role":"Warrior","name":"Alpha","elite":2}]}`)
	t.Run("unknown Core task ID", func(t *testing.T) {
		r := newOperProgressRuntimeHarness(t, plan, []int32{101})
		sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
			`{"taskchain":"OperProgress","taskid":999,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`)
		require.Equal(t, "typed_invalid_event", InspectRunForTest(r).Fatal, "unassociated callback was accepted: %s", InspectRunForTest(r).Fatal)
		for _, target := range r.Snapshot().Tasks[0].Typed.Targets {
			require.False(t, target.Outcome != TargetUnobserved, "unassociated callback created a fact: %+v", target)
		}
	})
	t.Run("ambiguous detail preserves confirmed facts", func(t *testing.T) {
		r := newOperProgressRuntimeHarness(t, plan, []int32{101})
		sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
			`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","elite":2}}`)
		sendTypedCallback(t, r, operProgressMessageSubTaskExtraInfo,
			`{"taskchain":"OperProgress","taskid":101,"what":"OperProgressDetail","details":{"role":"Warrior","name":"Alpha","result":"Success","result_detail":"Completed","result_detail":"AlreadySatisfied","elite":2}}`)
		require.Equal(t, "typed_invalid_event", InspectRunForTest(r).Fatal, "ambiguous detail was accepted: %s", InspectRunForTest(r).Fatal)
		result := r.Snapshot()
		require.False(t, result.Tasks[0].Typed.Targets[0].Outcome != TargetAchieved || result.Tasks[0].Typed.State != Succeeded, "malformed callback destroyed a confirmed fact: %+v", result.Tasks[0].Typed)
		for _, e := range operProgressDrainEvents(r) {
			require.False(t, e.Type == "typed" && e.Typed.Targets[0].Outcome == TargetAmbiguous, "rejected update was published")
		}
	})
}
