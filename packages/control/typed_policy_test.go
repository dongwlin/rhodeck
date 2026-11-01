package control

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPreparePlanObjectivePolicies(t *testing.T) {
	for _, policy := range []ObjectivePolicy{"", ObjectiveAll, ObjectiveAny, ObjectiveNone} {
		t.Run(string(policy), func(t *testing.T) {
			plan, adapter := adaptedTestPlan(t)
			plan.Tasks = append(plan.Tasks, Task{ID: "raw", Type: "Future", Params: json.RawMessage(`{}`)})
			var params json.RawMessage
			adapter.prepare = func(task Task) (PreparedTask, error) {
				params = bytes.Clone(task.Params)
				prepared := PreparedTask{Params: params, Policy: policy}
				if policy != ObjectiveNone {
					prepared.Targets, prepared.Reducer = []string{"goal"}, testTaskReducer{}
				}
				return prepared, nil
			}
			prepared, err := PreparePlan(plan, adapter)
			require.NoError(t, err)
			wantPolicy := policy
			if wantPolicy == "" {
				wantPolicy = ObjectiveAll
			}
			require.True(t, prepared.Tasks[0].Policy == wantPolicy && prepared.Tasks[1].Policy == "" && prepared.Tasks[1].Typed == nil,
				"prepared task policy mismatch: %+v", prepared.Tasks)
			if policy == ObjectiveNone {
				require.True(t, prepared.Tasks[0].Typed == nil && len(prepared.reducers) == 0,
					"input-only task gained typed behavior: %+v", prepared)
			} else {
				got := prepared.Tasks[0].Typed
				require.True(t, got != nil && got.Policy == wantPolicy && len(prepared.reducers) == 1 && got.State == Unknown && got.Code == "typed_incomplete",
					"objective task missing explicit policy or invented a fact: %+v", got)
			}
			params[2] = 'X'
			require.True(t, bytes.Equal(prepared.Plan.Tasks[0].Params, plan.Tasks[0].Params), "compiled parameters alias adapter output")
			raw, err := json.Marshal(prepared.Tasks)
			require.True(t, err == nil && bytes.Contains(raw, []byte(`"objective_policy":"`+string(wantPolicy)+`"`)),
				"preview does not expose its selected policy: %s, %v", raw, err)
		})
	}
}

func TestPreparePlanRejectsIllegalObjectiveCombinations(t *testing.T) {
	for name, output := range map[string]PreparedTask{
		"unknown policy":      {Policy: "future", Targets: []string{"goal"}, Reducer: testTaskReducer{}},
		"default no targets":  {Reducer: testTaskReducer{}},
		"all no targets":      {Policy: ObjectiveAll, Reducer: testTaskReducer{}},
		"any no targets":      {Policy: ObjectiveAny, Reducer: testTaskReducer{}},
		"all no reducer":      {Policy: ObjectiveAll, Targets: []string{"goal"}},
		"any no reducer":      {Policy: ObjectiveAny, Targets: []string{"goal"}},
		"none with targets":   {Policy: ObjectiveNone, Targets: []string{"goal"}},
		"none with reducer":   {Policy: ObjectiveNone, Reducer: testTaskReducer{}},
		"none with both":      {Policy: ObjectiveNone, Targets: []string{"goal"}, Reducer: testTaskReducer{}},
		"any typed nil":       {Policy: ObjectiveAny, Targets: []string{"goal"}, Reducer: (*testTaskReducer)(nil)},
		"all duplicate goals": {Policy: ObjectiveAll, Targets: []string{"goal", "goal"}, Reducer: testTaskReducer{}},
		"any duplicate goals": {Policy: ObjectiveAny, Targets: []string{"goal", "goal"}, Reducer: testTaskReducer{}},
	} {
		t.Run(name, func(t *testing.T) {
			plan, adapter := adaptedTestPlan(t)
			adapter.prepare = func(task Task) (PreparedTask, error) {
				output.Params = task.Params
				return output, nil
			}
			_, err := PreparePlan(plan, adapter)
			require.ErrorIs(t, err, ErrInvalidPlan, "invalid objective combination accepted: %v", err)
		})
	}
	plan, adapter := adaptedTestPlan(t)
	adapter.prepare = func(task Task) (PreparedTask, error) {
		return PreparedTask{Params: task.Params, Policy: ObjectiveNone, Reducer: (*testTaskReducer)(nil)}, nil
	}
	_, err := PreparePlan(plan, adapter)
	require.NoError(t, err, "input-only typed-nil reducer rejected: %v", err)
}

func TestAnyObjectiveAggregatesIndependentFacts(t *testing.T) {
	for _, test := range []struct {
		name     string
		outcomes []TargetOutcome
		state    State
		code     string
	}{
		{"success then failure", []TargetOutcome{TargetAchieved, TargetFailed}, Succeeded, ""},
		{"failure then success", []TargetOutcome{TargetFailed, TargetAchieved}, Succeeded, ""},
		{"already set then failure", []TargetOutcome{TargetAlreadySet, TargetFailed}, Succeeded, ""},
		{"success with unknown", []TargetOutcome{TargetAchieved, TargetUnobserved}, Succeeded, ""},
		{"success with ambiguous", []TargetOutcome{TargetAchieved, TargetAmbiguous}, Succeeded, ""},
		{"all failures", []TargetOutcome{TargetFailed, TargetLocked, TargetNotFound}, Failed, "typed_failed"},
		{"failure with unknown", []TargetOutcome{TargetFailed, TargetUnobserved}, Unknown, "typed_incomplete"},
		{"failure with ambiguous", []TargetOutcome{TargetFailed, TargetAmbiguous}, Unknown, "typed_incomplete"},
		{"only unknown", []TargetOutcome{TargetUnobserved, TargetUnobserved}, Unknown, "typed_incomplete"},
		{"reported training", []TargetOutcome{TargetFailed, TargetReportedTrainingStarted}, Unknown, "typed_incomplete"},
		{"reported prerequisite", []TargetOutcome{TargetFailed, TargetReportedPrerequisiteStarted}, Unknown, "typed_incomplete"},
		{"observed confirmation", []TargetOutcome{TargetFailed, TargetConfirmationObserved}, Unknown, "typed_incomplete"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ids := []string{"run-1", "run-2", "run-3"}[:len(test.outcomes)]
			current := testTypedResult(ids...)
			current.Policy = ObjectiveAny
			for i, outcome := range test.outcomes {
				var err error
				current, _, err = applyTypedUpdate(current, TypedUpdate{Targets: []TargetResult{{ID: ids[i], Outcome: outcome}}})
				require.NoError(t, err)
				if i == 0 && outcome == TargetFailed {
					require.Equal(t, Unknown, current.State, "one failed run decided an unfinished any objective: %+v", current)
				}
			}
			require.True(t, current.State == test.state && current.Code == test.code,
				"aggregate = %s/%s, want %s/%s", current.State, current.Code, test.state, test.code)
			for i, target := range current.Targets {
				require.Equal(t, test.outcomes[i], target.Outcome, "aggregation replaced an independent fact: %+v", current.Targets)
			}
		})
	}
}

func TestObjectivePolicyDoesNotPermitReusingATarget(t *testing.T) {
	for _, policy := range []ObjectivePolicy{ObjectiveAll, ObjectiveAny} {
		for _, first := range []TargetOutcome{TargetAchieved, TargetFailed} {
			current := testTypedResult("run-1")
			current.Policy = policy
			current, _, err := applyTypedUpdate(current, TypedUpdate{Targets: []TargetResult{{ID: "run-1", Outcome: first}}})
			require.NoError(t, err)
			second := TargetFailed
			if first == TargetFailed {
				second = TargetAchieved
			}
			_, _, err = applyTypedUpdate(current, TypedUpdate{Targets: []TargetResult{{ID: "run-1", Outcome: second}}})
			require.ErrorIs(t, err, ErrTypedConflict, "%s replaced a same-target fact: %v", policy, err)
			require.True(t, current.Targets[0].Outcome == first && (first != TargetFailed || current.State == Failed),
				"%s lost original failure precedence: %+v", policy, current)
		}
	}
}

func TestTypedPayloadRequiresExplicitObjectivePolicy(t *testing.T) {
	for _, policy := range []ObjectivePolicy{"", ObjectiveNone, "future"} {
		current := testTypedResult("goal")
		current.Policy = policy
		err := validateTypedResult(current)
		require.ErrorIs(t, err, ErrTypedResult, "invalid typed payload policy %q accepted: %v", policy, err)
	}
}

func TestAnyObjectiveSuccessStillEnforcesBudgets(t *testing.T) {
	current := testTypedResult("run-1", "run-2")
	current.Policy = ObjectiveAny
	current, _, err := applyTypedUpdate(current, TypedUpdate{Targets: []TargetResult{{ID: "run-1", Outcome: TargetAchieved}}})
	require.True(t, err == nil && current.State == Succeeded, "initial achievement = %+v, %v", current, err)
	_, _, err = applyTypedUpdate(current, TypedUpdate{Targets: []TargetResult{{ID: "run-2", Outcome: TargetFailed, Details: sizedObject(MaxTypedDetailBytes + 1)}}})
	require.ErrorIs(t, err, ErrTypedBudget, "success bypassed the detail budget: %v", err)
	require.True(t, current.Targets[1].Outcome == TargetUnobserved && current.State == Succeeded, "budget rejection changed confirmed facts: %+v", current)
	_, _, err = applyTypedUpdate(current, TypedUpdate{Summary: sizedObject(MaxTypedSummaryBytes + 1)})
	require.ErrorIs(t, err, ErrTypedBudget, "success bypassed the summary budget: %v", err)
}
