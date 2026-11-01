package control_test

import (
	"encoding/json"
	"testing"

	. "github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/adapters/roguelike"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	"github.com/stretchr/testify/require"
)

// sendRoguelikeCallback drives the real monitor path with an explicit binding
// sequence so settlement duplicates can be exercised end to end.
func sendRoguelikeCallback(t *testing.T, r *Run, messageID int32, sequence uint64, raw string) {
	t.Helper()
	m, err := protocol.NewMessage(protocol.Event, protocol.CoreEvent, 0, protocol.Callback{MessageID: messageID, CoreSequence: sequence, JSON: json.RawMessage(raw)})
	require.NoError(t, err)
	ObserveRunForTest(r, m)
}

func roguelikeRuntimePlan(t *testing.T, params string) Plan {
	t.Helper()
	plan := runtimePlan(t)
	ref := roguelike.New().Descriptor()
	plan.Tasks = []Task{{ID: "roguelike-0", Type: "Roguelike", Adapter: &ref, Params: json.RawMessage(params)}}
	return plan
}

// The BlackFlow port's internal context never receives a task id, so its
// subtask callbacks arrive with taskid 0 on the Roguelike chain. They are
// environmental: they must neither fault the run nor create facts, while the
// accepted task's own start-run facts still land.
func TestRoguelikeRuntimeAbsorbsBlackFlowEnvironment(t *testing.T) {
	run := newPreparedTypedHarness(t,
		roguelikeRuntimePlan(t, `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1}`),
		[]int32{101}, true, roguelike.New())
	sendRoguelikeCallback(t, run, 20001, 1, `{"taskchain":"Roguelike","taskid":0,"subtask":"ProcessTask","details":{"task":"RolesDefault"}}`)
	sendRoguelikeCallback(t, run, 20003, 2, `{"taskchain":"Roguelike","taskid":0,"subtask":"BlackFlowTask","what":"BlackFlowInventoryCleanup","details":{"status":"cleaned"}}`)
	require.Empty(t, InspectRunForTest(run).Fatal, "environmental callback faulted the run: %s", InspectRunForTest(run).Fatal)
	// Strategy results and running statuses are progress, not the objective.
	sendRoguelikeCallback(t, run, 20003, 3, `{"taskchain":"Roguelike","taskid":101,"subtask":"BlackFlowTask","what":"BlackFlowStrategyResult","details":{"outcome":"start_run_prepared","succeeded":true}}`)
	sendRoguelikeCallback(t, run, 20003, 4, `{"taskchain":"Roguelike","taskid":101,"subtask":"BlackFlowTask","what":"BlackFlowStartRun","details":{"status":"target_run"}}`)
	typed := run.Snapshot().Tasks[0].Typed
	require.True(t, typed.State == Unknown && typed.Code == "typed_incomplete" && len(typed.Targets) == 1 && typed.Targets[0].Outcome == TargetUnobserved,
		"progress changed the unobserved target: %+v", typed)
	sendRoguelikeCallback(t, run, 20003, 5, `{"taskchain":"Roguelike","taskid":101,"subtask":"BlackFlowTask","what":"BlackFlowStartRun","details":{"status":"completed","rewards":["unnumbered_item"]}}`)
	result := run.Snapshot()
	target := result.Tasks[0].Typed.Targets[0]
	require.True(t, target.ID == "roguelike:start_run" && target.Outcome == TargetAchieved && target.Code == "start_run_completed" && target.Details == nil,
		"start-run fact = %+v", target)
	require.Equal(t, Succeeded, result.Tasks[0].Typed.State, "achieved start run = %s/%s", result.Tasks[0].Typed.State, result.Tasks[0].Typed.Code)
	events := typedEventsIn(run)
	require.True(t, len(events) == 1 && events[0].TaskID == "roguelike-0" && events[0].CoreTaskID == 101 && events[0].MessageID == 20003,
		"typed events = %+v", events)
	sendRoguelikeCallback(t, run, 10002, 6, `{"taskchain":"Roguelike","taskid":101}`)
	sendRoguelikeCallback(t, run, 3, 7, `{"finished_tasks":[101]}`)
	require.True(t, InspectRunForTest(run).Proof == Succeeded && InspectRunForTest(run).ProofCode == "",
		"proof = %s/%s, want succeeded", InspectRunForTest(run).Proof, InspectRunForTest(run).ProofCode)
}

// Mode 0 settlements pair with runs in arrival order; a chain completion adds
// no fact and the any-aggregated objective stays separate from the chain.
func TestRoguelikeRuntimeCountsRunsUnderAny(t *testing.T) {
	run := newPreparedTypedHarness(t,
		roguelikeRuntimePlan(t, `{"theme":"Phantom","mode":0,"starts_count":2}`),
		[]int32{101}, true, roguelike.New())
	sendRoguelikeCallback(t, run, 20001, 1, roguelikeSettlement(101, "MissionFailedFlag2"))
	sendRoguelikeCallback(t, run, 20001, 2, roguelikeSettlement(101, "GamePass"))
	// A duplicate delivery of the same callback is one settlement.
	sendRoguelikeCallback(t, run, 20001, 2, roguelikeSettlement(101, "GamePass"))
	result := run.Snapshot()
	targets := result.Tasks[0].Typed.Targets
	require.True(t, targets[0].ID == "roguelike:run:1" && targets[0].Outcome == TargetFailed && targets[0].Code == "mission_failed" &&
		targets[1].ID == "roguelike:run:2" && targets[1].Outcome == TargetAchieved && targets[1].Code == "game_pass",
		"run targets = %+v", targets)
	require.Equal(t, Succeeded, result.Tasks[0].Typed.State, "any aggregation = %s/%s", result.Tasks[0].Typed.State, result.Tasks[0].Typed.Code)
	// The chain fact arrives separately and stays separate from the objective.
	sendRoguelikeCallback(t, run, 10002, 3, `{"taskchain":"Roguelike","taskid":101}`)
	sendRoguelikeCallback(t, run, 3, 4, `{"finished_tasks":[101]}`)
	require.True(t, InspectRunForTest(run).Proof == Succeeded && InspectRunForTest(run).Fatal == "",
		"proof = %s/%s, fatal = %s", InspectRunForTest(run).Proof, InspectRunForTest(run).ProofCode, InspectRunForTest(run).Fatal)
}
