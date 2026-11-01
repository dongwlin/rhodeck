package control_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	. "github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/adapters/roguelike"
	"github.com/stretchr/testify/require"
)

func roguelikeSettlement(taskid int32, node string) string {
	return fmt.Sprintf(`{"taskchain":"Roguelike","taskid":%d,"subtask":"ProcessTask","details":{"task":%q}}`, taskid, node)
}

func roguelikeExtra(taskid int32, what, details string) string {
	if details == "" {
		return fmt.Sprintf(`{"taskchain":"Roguelike","taskid":%d,"subtask":"ProcessTask","what":%q}`, taskid, what)
	}
	return fmt.Sprintf(`{"taskchain":"Roguelike","taskid":%d,"subtask":"ProcessTask","what":%q,"details":%s}`, taskid, what, details)
}

func roguelikeObserve(t *testing.T, reducer TaskReducer, messageID int32, sequence uint64, raw string) TypedUpdate {
	t.Helper()
	update, err := reducer.Observe(Observation{MessageID: messageID, CoreSequence: sequence, JSON: json.RawMessage(raw)})
	require.NoError(t, err)
	return update
}

func requireNoRoguelikeFacts(t *testing.T, update TypedUpdate) {
	t.Helper()
	require.True(t, len(update.Targets) == 0 && len(update.Summary) == 0, "callback manufactured business facts: %+v", update)
}

// roguelikeMutateField replaces or deletes one top-level field of a callback.
func roguelikeMutateField(t *testing.T, source, field, value string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(source), &fields))
	if value == "" {
		delete(fields, field)
	} else {
		fields[field] = json.RawMessage(value)
	}
	raw, err := json.Marshal(fields)
	require.NoError(t, err)
	return string(raw)
}

// roguelikePreparedPlan prepares one roguelike task and returns its preview
// with the unobserved typed skeleton, mirroring what a dry-run shows.
type roguelikePreparedPreview struct {
	PreparedPlan
	reducers map[int]TaskReducer
}

func roguelikePreparedPlan(t *testing.T, params string) roguelikePreparedPreview {
	t.Helper()
	plan := validTestPlan(t)
	ref := roguelike.New().Descriptor()
	plan.Tasks = []Task{{ID: "roguelike-task", Type: "Roguelike", Adapter: &ref, Params: json.RawMessage(params)}}
	preview, err := PreparePlan(plan, roguelike.New())
	prepared := roguelikePreparedPreview{PreparedPlan: preview, reducers: PreparedReducersForTest(preview)}
	require.NoError(t, err)
	require.True(t, len(prepared.reducers) == 1 && prepared.reducers[0] != nil, "interpreted preparation has no reducer: %+v", prepared.Tasks)
	return prepared
}

func roguelikeApplyUpdates(t *testing.T, typed *TypedResult, updates ...TypedUpdate) *TypedResult {
	t.Helper()
	for _, update := range updates {
		next, _, err := applyTypedUpdate(typed, update)
		require.NoError(t, err, "shared typed update rejected a roguelike fact: %v", err)
		typed = next
	}
	return typed
}

func TestRoguelikeObjectivePolicyMatrix(t *testing.T) {
	runs := func(n int) []string {
		targets := make([]string, n)
		for i := range targets {
			targets[i] = roguelikeRunTargetID(i + 1)
		}
		return targets
	}
	for _, test := range []struct {
		name    string
		params  string
		policy  ObjectivePolicy
		targets []string
	}{
		{"mode 0 single run", `{"theme":"Phantom","mode":0,"starts_count":1}`, ObjectiveAll, runs(1)},
		{"mode 0 several runs", `{"theme":"Mizuki","mode":0,"starts_count":2}`, ObjectiveAny, runs(2)},
		{"mode 0 budget bound", `{"theme":"Sami","mode":0,"starts_count":128}`, ObjectiveAny, runs(128)},
		{"mode 0 omitted starts", `{"theme":"Phantom","mode":0}`, ObjectiveNone, nil},
		{"mode 0 zero starts", `{"theme":"Phantom","mode":0,"starts_count":0}`, ObjectiveNone, nil},
		{"mode 0 over budget", `{"theme":"Phantom","mode":0,"starts_count":129}`, ObjectiveNone, nil},
		{"mode 0 final boss stop", `{"theme":"Phantom","mode":0,"starts_count":2,"stop_at_final_boss":true}`, ObjectiveNone, nil},
		{"mode 0 max level stop", `{"theme":"Phantom","mode":0,"starts_count":2,"stop_at_max_level":true}`, ObjectiveNone, nil},
		{"mode 0 investment budget", `{"theme":"Phantom","mode":0,"starts_count":3,"investments_count":5}`, ObjectiveNone, nil},
		{"mode 0 stop when full", `{"theme":"Phantom","mode":0,"starts_count":3,"stop_when_investment_full":true}`, ObjectiveNone, nil},
		// A zero investments_count keeps Core's investment plugin verify false,
		// so no limit or full callback can stop the declared runs early.
		{"mode 0 zero investment count", `{"theme":"Phantom","mode":0,"starts_count":2,"investments_count":0}`, ObjectiveAny, runs(2)},
		{"mode 0 investment disabled", `{"theme":"Phantom","mode":0,"starts_count":2,"investment_enabled":false}`, ObjectiveAny, runs(2)},
		{"mode 0 blackflow", `{"theme":"BlackFlow","mode":0,"starts_count":2}`, ObjectiveNone, nil},
		{"mode 1 explicit count", `{"theme":"Phantom","mode":1,"investments_count":5}`, ObjectiveAll, []string{roguelikeTargetInvestment}},
		{"mode 1 stop when full", `{"theme":"Phantom","mode":1,"stop_when_investment_full":true}`, ObjectiveAll, []string{roguelikeTargetInvestment}},
		{"mode 1 investment disabled", `{"theme":"Phantom","mode":1,"investments_count":5,"investment_enabled":false}`, ObjectiveNone, nil},
		{"mode 1 both defaults", `{"theme":"Phantom","mode":1}`, ObjectiveNone, nil},
		{"mode 1 zero count", `{"theme":"Phantom","mode":1,"investments_count":0}`, ObjectiveNone, nil},
		{"mode 1 zero count full flag", `{"theme":"Phantom","mode":1,"investments_count":0,"stop_when_investment_full":true}`, ObjectiveNone, nil},
		{"mode 1 blackflow", `{"theme":"BlackFlow","mode":1,"blackflow_strategy":"investment"}`, ObjectiveNone, nil},
		{"mode 6 omitted iterate", `{"theme":"Phantom","mode":6}`, ObjectiveNone, nil},
		{"mode 6 iterate false", `{"theme":"Phantom","mode":6,"monthly_squad_auto_iterate":false}`, ObjectiveNone, nil},
		{"mode 6 iterate true", `{"theme":"Phantom","mode":6,"monthly_squad_auto_iterate":true}`, ObjectiveAll, []string{roguelikeTargetMonthlySquad}},
		{"mode 7 omitted iterate", `{"theme":"Phantom","mode":7}`, ObjectiveNone, nil},
		{"mode 7 iterate false", `{"theme":"Phantom","mode":7,"deep_exploration_auto_iterate":false}`, ObjectiveNone, nil},
		{"mode 7 iterate true", `{"theme":"Phantom","mode":7,"deep_exploration_auto_iterate":true}`, ObjectiveAll, []string{roguelikeTargetDeepExploration}},
		{"mode 20001", `{"theme":"JieGarden","mode":20001,"find_playTime_target":1}`, ObjectiveAll, []string{roguelikeTargetJieGarden}},
		{"mode 30003", `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1}`, ObjectiveAll, []string{roguelikeTargetStartRun}},
		{"mode 4", `{"theme":"Phantom","mode":4}`, ObjectiveNone, nil},
		{"mode 5", `{"theme":"Sami","mode":5}`, ObjectiveNone, nil},
		{"mode 10001", `{"theme":"Sarkaz","mode":10001,"start_with_seed":"S"}`, ObjectiveNone, nil},
		{"mode 30001", `{"theme":"BlackFlow","mode":30001,"blackflow_strategy":"baby_animal"}`, ObjectiveNone, nil},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := roguelike.New().Prepare(roguelikeTask(test.params))
			require.NoError(t, err)
			require.Equal(t, test.policy, prepared.Policy, "policy = %q, want %q", prepared.Policy, test.policy)
			if test.policy == ObjectiveNone {
				require.True(t, len(prepared.Targets) == 0 && prepared.Reducer == nil, "input-only preparation = %+v", prepared)
				return
			}
			require.True(t, reflect.DeepEqual(prepared.Targets, test.targets) && prepared.Reducer != nil,
				"targets = %v, want %v with a reducer", prepared.Targets, test.targets)
			again, err := roguelike.New().Prepare(roguelikeTask(test.params))
			require.True(t, err == nil && again.Policy == prepared.Policy && reflect.DeepEqual(again.Targets, prepared.Targets),
				"preparation was not deterministic: %+v, %v", again, err)
		})
	}
}

func TestRoguelikeObjectiveSelectionIsVisibleInPreview(t *testing.T) {
	plan := validTestPlan(t)
	ref := roguelike.New().Descriptor()
	plan.Tasks = []Task{
		{ID: "runs", Type: "Roguelike", Adapter: &ref, Params: json.RawMessage(`{"theme":"Phantom","mode":0,"starts_count":2}`)},
		{ID: "plain", Type: "Roguelike", Adapter: &ref, Params: json.RawMessage(`{"theme":"Phantom","mode":4}`)},
	}
	prepared, err := PreparePlan(plan, roguelike.New())
	require.NoError(t, err)
	runs := prepared.Tasks[0].Typed.Targets
	require.True(t, prepared.Tasks[0].Policy == ObjectiveAny && len(runs) == 2 &&
		runs[0].ID == "roguelike:run:1" && runs[0].Outcome == TargetUnobserved &&
		runs[1].ID == "roguelike:run:2" && runs[1].Outcome == TargetUnobserved,
		"dry-run target preview = %+v", prepared.Tasks[0])
	require.True(t, prepared.Tasks[1].Policy == ObjectiveNone && prepared.Tasks[1].Typed == nil,
		"input-only preview = %+v", prepared.Tasks[1])
}

func TestRoguelikeReducerPairsSettlementsWithRuns(t *testing.T) {
	prepared := roguelikePreparedPlan(t, `{"theme":"Phantom","mode":0,"starts_count":3}`)
	reducer := prepared.reducers[0]
	var updates []TypedUpdate
	for i, node := range []string{"GamePass", "MissionFailedFlag2", "GamePass"} {
		updates = append(updates, roguelikeObserve(t, reducer, 20001, uint64(i+1), roguelikeSettlement(101, node)))
	}
	want := []TargetResult{
		{ID: "roguelike:run:1", Outcome: TargetAchieved, Code: roguelikeCodeGamePass},
		{ID: "roguelike:run:2", Outcome: TargetFailed, Code: roguelikeCodeMissionFailed},
		{ID: "roguelike:run:3", Outcome: TargetAchieved, Code: roguelikeCodeGamePass},
	}
	for i, update := range updates {
		require.True(t, len(update.Targets) == 1 && sameTarget(update.Targets[0], want[i]),
			"settlement %d = %+v, want %+v", i, update.Targets, want[i])
	}
	typed := roguelikeApplyUpdates(t, prepared.Tasks[0].Typed, updates...)
	require.True(t, typed.State == Succeeded && typed.Code == "", "two wins under any = %s/%s", typed.State, typed.Code)
}

func TestRoguelikeRunFactsAggregateUnderAny(t *testing.T) {
	for _, test := range []struct {
		name       string
		settlement []string
		wantState  State
		wantCode   string
	}{
		{"failure then success", []string{"MissionFailedFlag2", "GamePass"}, Succeeded, ""},
		{"success then failure", []string{"GamePass", "MissionFailedFlag2"}, Succeeded, ""},
		{"all failures", []string{"MissionFailedFlag2", "MissionFailedFlag2"}, Failed, "typed_failed"},
		{"unobserved remainder", []string{"MissionFailedFlag2"}, Unknown, "typed_incomplete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := roguelikePreparedPlan(t, `{"theme":"Phantom","mode":0,"starts_count":2}`)
			reducer := prepared.reducers[0]
			var updates []TypedUpdate
			for i, node := range test.settlement {
				updates = append(updates, roguelikeObserve(t, reducer, 20001, uint64(i+1), roguelikeSettlement(101, node)))
			}
			typed := roguelikeApplyUpdates(t, prepared.Tasks[0].Typed, updates...)
			require.True(t, typed.State == test.wantState && typed.Code == test.wantCode,
				"aggregate = %s/%s, want %s/%s; targets %+v", typed.State, typed.Code, test.wantState, test.wantCode, typed.Targets)
		})
	}
}

func TestRoguelikeReducerIgnoresExtraAndRepeatedSettlements(t *testing.T) {
	prepared := roguelikePreparedPlan(t, `{"theme":"Phantom","mode":0,"starts_count":2}`)
	reducer := prepared.reducers[0]
	update := roguelikeObserve(t, reducer, 20001, 7, roguelikeSettlement(101, "GamePass"))
	require.Equal(t, "roguelike:run:1", update.Targets[0].ID, "first settlement = %+v", update)
	// The same callback delivered again is one settlement, not a second run.
	requireNoRoguelikeFacts(t, roguelikeObserve(t, reducer, 20001, 7, roguelikeSettlement(101, "GamePass")))
	update = roguelikeObserve(t, reducer, 20001, 8, roguelikeSettlement(101, "GamePass"))
	require.Equal(t, "roguelike:run:2", update.Targets[0].ID, "second settlement = %+v", update)
	// Core kept going past the declared run count; that cannot name a target.
	requireNoRoguelikeFacts(t, roguelikeObserve(t, reducer, 20001, 9, roguelikeSettlement(101, "GamePass")))
}

func TestRoguelikeReducerRequiresSameAcceptedTaskID(t *testing.T) {
	prepared := roguelikePreparedPlan(t, `{"theme":"Phantom","mode":0,"starts_count":2}`)
	reducer := prepared.reducers[0]
	update := roguelikeObserve(t, reducer, 20001, 1, roguelikeSettlement(101, "GamePass"))
	require.Equal(t, "roguelike:run:1", update.Targets[0].ID, "accepted task settlement = %+v", update)
	for _, taskid := range []int32{102, 0, -1} {
		requireNoRoguelikeFacts(t, roguelikeObserve(t, reducer, 20001, 2, roguelikeSettlement(taskid, "GamePass")))
	}
	// The fixed association still accepts its own task.
	update = roguelikeObserve(t, reducer, 20001, 3, roguelikeSettlement(101, "MissionFailedFlag2"))
	require.Equal(t, "roguelike:run:2", update.Targets[0].ID, "own task settlement after foreign events = %+v", update)
	startRun := roguelikePreparedPlan(t, `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1}`)
	for _, taskid := range []int32{0, -5} {
		requireNoRoguelikeFacts(t, roguelikeObserve(t, startRun.reducers[0], 20003, 4, roguelikeExtra(taskid, roguelikeWhatStartRun, `{"status":"completed"}`)))
	}
}

func TestRoguelikeReducerRequiresExactSettlementShape(t *testing.T) {
	base := roguelikeSettlement(101, "GamePass")
	for _, test := range []struct {
		field string
		value string
	}{
		{"taskid", ""}, {"taskid", `0`}, {"taskid", `"101"`}, {"taskid", `101.0`}, {"taskid", `101e0`}, {"taskid", `2147483648`},
		{"taskchain", ""}, {"taskchain", `null`}, {"taskchain", `1`}, {"taskchain", `"roguelike"`}, {"taskchain", `"Copilot"`},
		{"subtask", ""}, {"subtask", `null`}, {"subtask", `3`}, {"subtask", `"BattleProcessTask"`},
		{"details", ""}, {"details", `null`}, {"details", `[]`}, {"details", `{}`},
		{"details", `{"task":null}`}, {"details", `{"task":7}`},
		{"details", `{"task":"Phantom@Roguelike@GamePass"}`},
		{"details", `{"task":"Roguelike@GamePass"}`},
		{"details", `{"task":"gamepass"}`}, {"details", `{"task":"GamePassFlag"}`}, {"details", `{"task":"FutureNode"}`},
	} {
		t.Run(test.field+"="+test.value, func(t *testing.T) {
			prepared := roguelikePreparedPlan(t, `{"theme":"Phantom","mode":0,"starts_count":1}`)
			raw := roguelikeMutateField(t, base, test.field, test.value)
			requireNoRoguelikeFacts(t, roguelikeObserve(t, prepared.reducers[0], 20001, 1, raw))
		})
	}
}

func TestRoguelikeReducerIgnoresNonEvidenceMessages(t *testing.T) {
	startRun := `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1}`
	for _, test := range []struct {
		name      string
		params    string
		messageID int32
		raw       string
	}{
		{"chain completed", `{"theme":"Phantom","mode":0,"starts_count":1}`, 10002, `{"taskchain":"Roguelike","taskid":101}`},
		{"batch finished", `{"theme":"Phantom","mode":0,"starts_count":1}`, 3, `{"finished_tasks":[101]}`},
		{"settlement completion", `{"theme":"Phantom","mode":0,"starts_count":1}`, 20002, roguelikeSettlement(101, "GamePass")},
		{"settlement extra", `{"theme":"Phantom","mode":0,"starts_count":1}`, 20003, roguelikeExtra(101, "RoguelikeSettlement", `{"game_pass":true}`)},
		{"wrong extra for mode 0", `{"theme":"Phantom","mode":0,"starts_count":1}`, 20003, roguelikeExtra(101, roguelikeWhatStartRun, `{"status":"completed"}`)},
		{"investment progress", `{"theme":"Phantom","mode":1,"investments_count":5}`, 20003, roguelikeExtra(101, "RoguelikeInvestment", `{"count":1,"total":1,"deposit":100}`)},
		{"strategy result prepared", startRun, 20003,
			roguelikeExtra(101, "BlackFlowStrategyResult", `{"profile":"start_run_rewards","outcome":"start_run_prepared","succeeded":true,"next_action":"restart_current_run"}`)},
		{"strategy result burn", startRun, 20003,
			roguelikeExtra(101, "BlackFlowStrategyResult", `{"outcome":"burn_completed","succeeded":true}`)},
		{"unknown what", `{"theme":"Phantom","mode":6,"monthly_squad_auto_iterate":true}`, 20003, roguelikeExtra(101, "FutureCallback", `{}`)},
		{"node start for what mode", `{"theme":"Phantom","mode":6,"monthly_squad_auto_iterate":true}`, 20001, roguelikeSettlement(101, "GamePass")},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := roguelikePreparedPlan(t, test.params)
			requireNoRoguelikeFacts(t, roguelikeObserve(t, prepared.reducers[0], test.messageID, 1, test.raw))
		})
	}
}

func TestRoguelikeReducerSingleTargetFacts(t *testing.T) {
	for _, test := range []struct {
		name    string
		params  string
		what    string
		details string
		want    TargetResult
	}{
		{"investment limit", `{"theme":"Phantom","mode":1,"investments_count":5}`, roguelikeWhatInvestmentLimit, `{"limit":5}`,
			TargetResult{ID: roguelikeTargetInvestment, Outcome: TargetAchieved, Code: roguelikeCodeInvestmentLimit}},
		{"investment full", `{"theme":"Phantom","mode":1,"stop_when_investment_full":true}`, roguelikeWhatInvestmentFull, `{}`,
			TargetResult{ID: roguelikeTargetInvestment, Outcome: TargetAchieved, Code: roguelikeCodeInvestmentFull}},
		{"monthly squad", `{"theme":"Phantom","mode":6,"monthly_squad_auto_iterate":true}`, roguelikeWhatMonthlySquad, `{}`,
			TargetResult{ID: roguelikeTargetMonthlySquad, Outcome: TargetAchieved, Code: roguelikeCodeRotationDone}},
		{"deep exploration", `{"theme":"Phantom","mode":7,"deep_exploration_auto_iterate":true}`, roguelikeWhatDeepExploration, `{}`,
			TargetResult{ID: roguelikeTargetDeepExploration, Outcome: TargetAchieved, Code: roguelikeCodeRotationDone}},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := roguelikePreparedPlan(t, test.params)
			reducer := prepared.reducers[0]
			first := roguelikeObserve(t, reducer, 20003, 1, roguelikeExtra(101, test.what, test.details))
			require.True(t, len(first.Targets) == 1 && sameTarget(first.Targets[0], test.want),
				"fact = %+v, want %+v", first.Targets, test.want)
			// The identical fact is idempotent through the shared update.
			repeat := roguelikeObserve(t, reducer, 20003, 2, roguelikeExtra(101, test.what, test.details))
			require.True(t, reflect.DeepEqual(repeat, first), "repeated fact changed: %+v, want %+v", repeat, first)
			typed := roguelikeApplyUpdates(t, prepared.Tasks[0].Typed, first, repeat)
			require.Equal(t, Succeeded, typed.State, "repeated identical fact did not stay achieved: %+v", typed)
		})
	}
}

func TestRoguelikeReducerStartRunStatuses(t *testing.T) {
	params := `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1}`
	for _, test := range []struct {
		status string
		want   TargetResult
	}{
		{"completed", TargetResult{ID: roguelikeTargetStartRun, Outcome: TargetAchieved, Code: roguelikeCodeStartRunDone}},
		{"reward_page_missing", TargetResult{ID: roguelikeTargetStartRun, Outcome: TargetFailed, Code: "reward_page_missing"}},
		{"reward_recognition_failed", TargetResult{ID: roguelikeTargetStartRun, Outcome: TargetFailed, Code: "reward_recognition_failed"}},
		{"reward_unrecognized", TargetResult{ID: roguelikeTargetStartRun, Outcome: TargetFailed, Code: "reward_unrecognized"}},
	} {
		t.Run(test.status, func(t *testing.T) {
			prepared := roguelikePreparedPlan(t, params)
			details := `{"status":"` + test.status + `"}`
			if test.status == "completed" {
				// Recognized rewards stay native data and are not published.
				details = `{"status":"completed","rewards":["unnumbered_item"]}`
			}
			update := roguelikeObserve(t, prepared.reducers[0], 20003, 1, roguelikeExtra(101, roguelikeWhatStartRun, details))
			require.True(t, len(update.Targets) == 1 && sameTarget(update.Targets[0], test.want),
				"status %s = %+v, want %+v", test.status, update.Targets, test.want)
		})
	}
	for _, status := range []string{"preparing", "target_run", "reward_missed", "reward_picked", "future_status"} {
		t.Run("progress "+status, func(t *testing.T) {
			prepared := roguelikePreparedPlan(t, params)
			requireNoRoguelikeFacts(t, roguelikeObserve(t, prepared.reducers[0], 20003, 1,
				roguelikeExtra(101, roguelikeWhatStartRun, `{"status":"`+status+`"}`)))
		})
	}
	for name, details := range map[string]string{
		"missing details": "",
		"array details":   `[]`,
		"empty details":   `{}`,
		"null status":     `{"status":null}`,
		"number status":   `{"status":7}`,
		"missing status":  `{"rewards":["unnumbered_item"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			prepared := roguelikePreparedPlan(t, params)
			requireNoRoguelikeFacts(t, roguelikeObserve(t, prepared.reducers[0], 20003, 1, roguelikeExtra(101, roguelikeWhatStartRun, details)))
		})
	}
}

func TestRoguelikeReducerJieGardenTargetSubtype(t *testing.T) {
	params := `{"theme":"JieGarden","mode":20001,"find_playTime_target":2}`
	prepared := roguelikePreparedPlan(t, params)
	update := roguelikeObserve(t, prepared.reducers[0], 20003, 1, roguelikeExtra(101, roguelikeWhatJieGardenFound, `{"target_subtype":"Nian"}`))
	want := TargetResult{ID: roguelikeTargetJieGarden, Outcome: TargetAchieved, Code: roguelikeCodeTargetFound, Details: json.RawMessage(`{"target_subtype":"Nian"}`)}
	require.True(t, len(update.Targets) == 1 && sameTarget(update.Targets[0], want), "found fact = %+v, want %+v", update.Targets, want)
	typed := roguelikeApplyUpdates(t, prepared.Tasks[0].Typed, update)
	require.Equal(t, Succeeded, typed.State, "found fact did not achieve the target: %+v", typed)
	// The what alone proves the fact; unusable subtypes are simply not published.
	for name, details := range map[string]string{
		"no subtype":         `{}`,
		"null subtype":       `{"target_subtype":null}`,
		"number subtype":     `{"target_subtype":3}`,
		"bool subtype":       `{"target_subtype":true}`,
		"object subtype":     `{"target_subtype":{}}`,
		"unpaired surrogate": `{"target_subtype":"\ud800"}`,
		"nul subtype":        `{"target_subtype":"a\u0000b"}`,
		"oversized subtype":  `{"target_subtype":"` + strings.Repeat("长", 200) + `"}`,
	} {
		t.Run(name, func(t *testing.T) {
			fresh := roguelikePreparedPlan(t, params)
			update := roguelikeObserve(t, fresh.reducers[0], 20003, 1, roguelikeExtra(101, roguelikeWhatJieGardenFound, details))
			require.True(t, len(update.Targets) == 1 && update.Targets[0].Outcome == TargetAchieved && update.Targets[0].Details == nil,
				"unusable subtype changed the fact: %+v", update.Targets)
		})
	}
}

func TestRoguelikeReducerConflictsStayWithSharedUpdate(t *testing.T) {
	for _, test := range []struct {
		name   string
		params string
		first  string
		second string
	}{
		{"investment limit then full", `{"theme":"Phantom","mode":1,"investments_count":5}`,
			roguelikeExtra(101, roguelikeWhatInvestmentLimit, `{"limit":5}`),
			roguelikeExtra(101, roguelikeWhatInvestmentFull, `{}`)},
		{"start run failed then completed", `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1}`,
			roguelikeExtra(101, roguelikeWhatStartRun, `{"status":"reward_page_missing"}`),
			roguelikeExtra(101, roguelikeWhatStartRun, `{"status":"completed"}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := roguelikePreparedPlan(t, test.params)
			reducer := prepared.reducers[0]
			first := roguelikeObserve(t, reducer, 20003, 1, test.first)
			typed, _, err := applyTypedUpdate(prepared.Tasks[0].Typed, first)
			require.NoError(t, err)
			second := roguelikeObserve(t, reducer, 20003, 2, test.second)
			_, _, err = applyTypedUpdate(typed, second)
			require.ErrorIs(t, err, ErrTypedConflict, "contradictory fact = %v, want ErrTypedConflict", err)
			// The first fact survives the rejected update.
			require.True(t, typed.Targets[0].Outcome == first.Targets[0].Outcome && typed.Targets[0].Code == first.Targets[0].Code,
				"rejected update changed the recorded fact: %+v", typed.Targets)
		})
	}
}

func TestRoguelikeReducerRejectsMalformedAndDuplicateCallbacks(t *testing.T) {
	for _, raw := range []string{
		`{`, `[]`, `null`, `"x"`, `{"taskchain":"Roguelike","taskid":101}{}`,
		`{"taskchain":"Roguelike","taskchain":"Roguelike","taskid":101}`,
		`{"taskchain":"Roguelike","taskid":101,"details":{"task":"GamePass","task":"MissionFailedFlag2"}}`,
		`{"taskchain":"Roguelike","taskid":101,"what":"FutureCallback","future":{"params":{"json":{"n":1,"n":2}}}}`,
	} {
		for _, messageID := range []int32{20001, 20003} {
			t.Run(fmt.Sprint(messageID)+raw, func(t *testing.T) {
				prepared := roguelikePreparedPlan(t, `{"theme":"Phantom","mode":0,"starts_count":1}`)
				update, err := prepared.reducers[0].Observe(Observation{MessageID: messageID, JSON: json.RawMessage(raw)})
				require.ErrorIs(t, err, ErrTypedResult, "malformed callback error = %v", err)
				requireNoRoguelikeFacts(t, update)
			})
		}
	}
}

func TestRoguelikeReducerClassifiesBlackFlowEnvironment(t *testing.T) {
	prepared := roguelikePreparedPlan(t, `{"theme":"BlackFlow","mode":30003,"blackflow_start_animals":["swaddled_cat"],"blackflow_start_rewards":1}`)
	reducer := prepared.reducers[0]
	classifier := reducer.(UnassociatedObservationClassifier)
	for _, test := range []struct {
		messageID int32
		raw       string
	}{
		{20000, `{"taskchain":"Roguelike","taskid":0,"subtask":"ProcessTask","why":"Timeout"}`},
		{20001, `{"taskchain":"Roguelike","taskid":0,"subtask":"ProcessTask","details":{"task":"RolesDefault"}}`},
		{20002, `{"taskchain":"Roguelike","taskid":0,"subtask":"ProcessTask","details":{"task":"RolesDefault"}}`},
		{20003, `{"taskchain":"Roguelike","taskid":0,"subtask":"BlackFlowTask","what":"BlackFlowInventoryCleanup","details":{"status":"cleaned","name":"Cell"}}`},
	} {
		require.True(t, classifier.IsUnassociatedProgress(Observation{MessageID: test.messageID, JSON: json.RawMessage(test.raw)}),
			"internal callback was not classified as environment: %d %s", test.messageID, test.raw)
	}
	for _, test := range []struct {
		messageID int32
		raw       string
	}{
		{20001, roguelikeSettlement(101, "GamePass")},
		{20001, `{"taskchain":"Copilot","taskid":0,"subtask":"ProcessTask","details":{"task":"x"}}`},
		{20003, roguelikeExtra(101, roguelikeWhatStartRun, `{"status":"completed"}`)},
		{20001, `{"taskchain":"Roguelike","taskid":0,"subtask":"ProcessTask","details":{"task":"a","task":"b"}}`},
		{20001, `{"taskchain":"Roguelike"}`},
	} {
		require.False(t, classifier.IsUnassociatedProgress(Observation{MessageID: test.messageID, JSON: json.RawMessage(test.raw)}),
			"associated or malformed callback was absorbed: %d %s", test.messageID, test.raw)
	}
	// Environmental events carry no facts even if they reached Observe.
	requireNoRoguelikeFacts(t, roguelikeObserve(t, reducer, 20001, 1, `{"taskchain":"Roguelike","taskid":0,"subtask":"ProcessTask","details":{"task":"GamePass"}}`))
}
