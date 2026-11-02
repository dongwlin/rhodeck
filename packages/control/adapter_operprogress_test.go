// Covers: adapters/operprogress and the shared monitor.

package control_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	. "github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/adapters/operprogress"
)

// operProgressHarness applies reducer updates to a TypedResult so tests assert
// the same immutable-fact and aggregate behavior the runtime enforces.
type operProgressHarness struct {
	t       *testing.T
	reducer TaskReducer
	typed   *TypedResult
}

func newOperProgressHarness(t *testing.T, params string) *operProgressHarness {
	t.Helper()
	prepared, err := operprogress.New().Prepare(operProgressTask(params))
	require.NoError(t, err, "prepare %s", params)
	reducer := prepared.Reducer
	require.NotNil(t, reducer, "interpreted preparation has no reducer")
	typed := &TypedResult{SchemaVersion: SchemaVersion, Adapter: AdapterRef{Name: operProgressAdapterName, Version: operProgressVersion}, Policy: ObjectiveAll}
	for _, id := range prepared.Targets {
		typed.Targets = append(typed.Targets, TargetResult{ID: id, Outcome: TargetUnobserved})
	}
	typed.State, typed.Code = typedState(typed)
	return &operProgressHarness{t: t, reducer: reducer, typed: typed}
}

func (h *operProgressHarness) observe(messageID int32, raw string) TypedUpdate {
	h.t.Helper()
	update, err := h.reducer.Observe(Observation{MessageID: messageID, JSON: json.RawMessage(raw)})
	require.NoError(h.t, err, "Observe(%d, %s)", messageID, raw)
	return update
}

func (h *operProgressHarness) apply(messageID int32, raw string) bool {
	h.t.Helper()
	next, changed, err := applyTypedUpdate(h.typed, h.observe(messageID, raw))
	require.NoError(h.t, err, "apply %s", raw)
	h.typed = next
	return changed
}

func operProgressCallback(what, details string) string {
	return `{"taskchain":"OperProgress","taskid":41,"what":"` + what + `","details":` + details + `}`
}

func operProgressDetailCallback(details string) string {
	return operProgressCallback(operProgressWhatDetail, details)
}

func operProgressSummaryCallback(details string) string {
	return operProgressCallback(operProgressWhatSummary, details)
}

func checkOperProgressTarget(t *testing.T, typed *TypedResult, index int, want TargetResult) {
	t.Helper()
	got := typed.Targets[index]
	require.False(t, got.ID != want.ID || got.Outcome != want.Outcome || got.Code != want.Code || !bytes.Equal(got.Details, want.Details), "target %d = %+v, want %+v", index, got, want)
}

func TestOperProgressResultEliteAndSkillFacts(t *testing.T) {
	elite := `{"plans":[{"role":"Warrior","name":"示例干员甲","elite":2}]}`
	skill := `{"plans":[{"role":"Tank","name":"技能干员","skill_level":7}]}`
	for _, tc := range []struct {
		name      string
		params    string
		details   string
		want      TargetResult
		wantState State
		wantCode  string
	}{
		{
			name: "elite completed", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"Completed","elite":2}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAchieved, Code: "completed", Details: json.RawMessage(`{"action":"elite","expected":2,"reported":2}`)},
			wantState: Succeeded,
		},
		{
			name: "elite completed below target", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"Completed","elite":1}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "elite already satisfied", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"AlreadySatisfied","elite":2}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAchieved, Code: "already_satisfied", Details: json.RawMessage(`{"action":"elite","expected":2,"reported":2}`)},
			wantState: Succeeded,
		},
		{
			name: "elite already satisfied below target", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"AlreadySatisfied","elite":1}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "elite reported value out of range", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"AlreadySatisfied","elite":3}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_malformed"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "skill completed", params: skill,
			details:   `{"role":"Tank","name":"技能干员","result":"Success","result_detail":"Completed","skill_level":7}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAchieved, Code: "completed", Details: json.RawMessage(`{"action":"skill_level","expected":7,"reported":7}`)},
			wantState: Succeeded,
		},
		{
			name: "skill completed below target", params: skill,
			details:   `{"role":"Tank","name":"技能干员","result":"Success","result_detail":"Completed","skill_level":6}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "skill already satisfied", params: skill,
			details:   `{"role":"Tank","name":"技能干员","result":"Success","result_detail":"AlreadySatisfied","skill_level":7}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAchieved, Code: "already_satisfied", Details: json.RawMessage(`{"action":"skill_level","expected":7,"reported":7}`)},
			wantState: Succeeded,
		},
		{
			name: "skill already satisfied below target", params: skill,
			details:   `{"role":"Tank","name":"技能干员","result":"Success","result_detail":"AlreadySatisfied","skill_level":5}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "success without action field", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"Completed"}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incomplete"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "success with conflicting actions", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"Completed","elite":2,"skill_level":7}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "success action does not match plan", params: skill,
			details:   `{"role":"Tank","name":"技能干员","result":"Success","result_detail":"Completed","elite":2}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "success reason is a failure reason", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"ResourceInsufficient"}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "success reason is training room busy", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"TrainingRoomBusy","elite":2}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
		{
			name: "unknown future success reason", params: elite,
			details:   `{"role":"Warrior","name":"示例干员甲","result":"Success","result_detail":"FutureReason","elite":2}`,
			want:      TargetResult{ID: "op-0001", Outcome: TargetUnobserved},
			wantState: Unknown, wantCode: "typed_incomplete",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperProgressHarness(t, tc.params)
			h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(tc.details))
			checkOperProgressTarget(t, h.typed, 0, tc.want)
			require.False(t, len(h.typed.Targets) != 1 || h.typed.State != tc.wantState || h.typed.Code != tc.wantCode, "aggregate = %s/%s, want %s/%s", h.typed.State, h.typed.Code, tc.wantState, tc.wantCode)
		})
	}
}

func TestOperProgressResultMasteryFacts(t *testing.T) {
	const plan3 = `{"plans":[{"role":"Caster","name":"专精干员","skill_mastery":[0,0,3]}]}`
	const plan2 = `{"plans":[{"role":"Caster","name":"专精干员","skill_mastery":[0,0,2]}]}`
	for _, tc := range []struct {
		name    string
		plan    string
		details string
		want    TargetResult
	}{
		{
			name:    "completed starts target level",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"Completed","skill_mastery":[0,0,3]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetReportedTrainingStarted, Code: "mastery_training_started", Details: json.RawMessage(`{"action":"skill_mastery","expected":3,"slot":3,"reported":3}`)},
		},
		{
			name:    "completed reports a lower level",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"Completed","skill_mastery":[0,0,2]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
		},
		{
			name:    "prerequisite training starts a lower level",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"PrerequisiteTraining","skill_mastery":[0,0,2]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetReportedPrerequisiteStarted, Code: "mastery_prerequisite_training_started", Details: json.RawMessage(`{"action":"skill_mastery","expected":3,"slot":3,"reported":2}`)},
		},
		{
			name:    "prerequisite training already at target",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"PrerequisiteTraining","skill_mastery":[0,0,3]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
		},
		{
			name:    "already satisfied at target",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"AlreadySatisfied","skill_mastery":[0,0,3]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAchieved, Code: "already_satisfied", Details: json.RawMessage(`{"action":"skill_mastery","expected":3,"slot":3,"reported":3}`)},
		},
		{
			name:    "already satisfied below target",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"AlreadySatisfied","skill_mastery":[0,0,2]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
		},
		{
			name:    "already satisfied above target",
			plan:    plan2,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"AlreadySatisfied","skill_mastery":[0,0,3]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAchieved, Code: "already_satisfied", Details: json.RawMessage(`{"action":"skill_mastery","expected":2,"slot":3,"reported":3}`)},
		},
		{
			name:    "another mastery slot is nonzero",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"Completed","skill_mastery":[0,2,3]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
		},
		{
			name:    "requested slot is zero",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"Completed","skill_mastery":[0,0,0]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
		},
		{
			name:    "mastery array has the wrong length",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"Completed","skill_mastery":[0,3]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_malformed"},
		},
		{
			name:    "mastery array has a non-integer",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"Completed","skill_mastery":[0,0,"3"]}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_malformed"},
		},
		{
			name:    "elite detail for a mastery target",
			plan:    plan3,
			details: `{"role":"Caster","name":"专精干员","result":"Success","result_detail":"Completed","elite":2}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperProgressHarness(t, tc.plan)
			h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(tc.details))
			checkOperProgressTarget(t, h.typed, 0, tc.want)
			switch tc.want.Outcome {
			case TargetReportedTrainingStarted, TargetReportedPrerequisiteStarted:
				// Reported startup is incomplete achievement evidence.
				require.False(t, h.typed.State != Unknown || h.typed.Code != "typed_incomplete", "startup report became %s/%s", h.typed.State, h.typed.Code)
			case TargetAchieved:
				require.Equal(t, Succeeded, h.typed.State, "achieved target aggregate = %s/%s", h.typed.State, h.typed.Code)
			}
		})
	}
}

func TestOperProgressResultFailureFacts(t *testing.T) {
	const plan = `{"plans":[{"role":"Warrior","name":"失败干员","elite":2}]}`
	for _, tc := range []struct {
		name    string
		details string
		want    TargetResult
	}{
		{
			name:    "resource insufficient",
			details: `{"role":"Warrior","name":"失败干员","result":"Failed","result_detail":"ResourceInsufficient"}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetFailed, Code: "resource_insufficient"},
		},
		{
			name:    "operator not found",
			details: `{"role":"Warrior","name":"失败干员","result":"Failed","result_detail":"OperatorNotFound"}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetFailed, Code: "operator_not_found"},
		},
		{
			name:    "recognition failed",
			details: `{"role":"Warrior","name":"失败干员","result":"Failed","result_detail":"RecognitionFailed"}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetFailed, Code: "recognition_failed"},
		},
		{
			name:    "interrupt",
			details: `{"role":"Warrior","name":"失败干员","result":"Failed","result_detail":"Interrupt"}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetFailed, Code: "interrupt"},
		},
		{
			name:    "failure without an action or reason",
			details: `{"role":"Warrior","name":"失败干员","result":"Failed"}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetFailed, Code: "unspecified_failure"},
		},
		{
			name:    "failure with an unknown reason",
			details: `{"role":"Warrior","name":"失败干员","result":"Failed","result_detail":"FutureReason"}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetFailed, Code: "unspecified_failure"},
		},
		{
			name:    "failure carrying an unrelated action field",
			details: `{"role":"Warrior","name":"失败干员","result":"Failed","result_detail":"ResourceInsufficient","elite":2}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetFailed, Code: "resource_insufficient"},
		},
		{
			name:    "failed result with a completion reason",
			details: `{"role":"Warrior","name":"失败干员","result":"Failed","result_detail":"Completed"}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
		},
		{
			name:    "failed result with a training room skip",
			details: `{"role":"Warrior","name":"失败干员","result":"Failed","result_detail":"TrainingRoomBusy"}`,
			want:    TargetResult{ID: "op-0001", Outcome: TargetAmbiguous, Code: "detail_incompatible"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newOperProgressHarness(t, plan)
			h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(tc.details))
			checkOperProgressTarget(t, h.typed, 0, tc.want)
			switch tc.want.Outcome {
			case TargetFailed:
				require.False(t, h.typed.State != Failed || h.typed.Code != "typed_failed", "failure aggregate = %s/%s", h.typed.State, h.typed.Code)
			default:
				require.False(t, h.typed.State != Unknown || h.typed.Code != "typed_incomplete", "ambiguous aggregate = %s/%s", h.typed.State, h.typed.Code)
			}
		})
	}
}

func TestOperProgressResultSkippedOnlyEntersSummary(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Caster","name":"静默干员","skill_mastery":[0,0,3]}]}`)
	skip := operProgressDetailCallback(`{"role":"Caster","name":"静默干员","result":"Skipped","result_detail":"TrainingRoomBusy"}`)
	require.False(t, h.apply(operProgressMessageSubTaskExtraInfo, skip), "a skipped training-room attempt created a target fact")
	h.apply(operProgressMessageSubTaskExtraInfo, skip)
	checkOperProgressTarget(t, h.typed, 0, TargetResult{ID: "op-0001", Outcome: TargetUnobserved})
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressSummaryCallback(`{"finished":[],"success":0,"failed":0,"skipped":1}`))
	const want = `{"targets":1,"observed":0,"unobserved":1,"achieved":0,"reported_target_training_started":0,"reported_prerequisite_training_started":0,"failed":0,"ambiguous":0,"skipped_training_room_busy":1}`
	require.Equal(t, want, string(h.typed.Summary), "summary = %s, want %s", h.typed.Summary, want)
	require.False(t, h.typed.State != Unknown || h.typed.Code != "typed_incomplete", "skip became success: %s/%s", h.typed.State, h.typed.Code)
}

func TestOperProgressResultAssociationAmbiguity(t *testing.T) {
	const sameName = `{"plans":[{"role":"Warrior","name":"同名干员","elite":2},{"role":"Tank","name":"同名干员","elite":1}]}`
	t.Run("missing role marks every name candidate ambiguous", func(t *testing.T) {
		h := newOperProgressHarness(t, sameName)
		h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"name":"同名干员","result":"Failed","result_detail":"RecognitionFailed"}`))
		for i := range h.typed.Targets {
			checkOperProgressTarget(t, h.typed, i, TargetResult{ID: fmt.Sprintf("op-%04d", i+1), Outcome: TargetAmbiguous, Code: "association_ambiguous"})
		}
	})
	t.Run("missing name marks every role candidate ambiguous", func(t *testing.T) {
		h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2},{"role":"Warrior","name":"乙","elite":2}]}`)
		h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","result":"Failed","result_detail":"RecognitionFailed"}`))
		for i := range h.typed.Targets {
			checkOperProgressTarget(t, h.typed, i, TargetResult{ID: fmt.Sprintf("op-%04d", i+1), Outcome: TargetAmbiguous, Code: "association_ambiguous"})
		}
	})
	t.Run("a concrete role that names no target is ignored", func(t *testing.T) {
		h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2}]}`)
		h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Caster","name":"甲","result":"Failed","result_detail":"RecognitionFailed"}`))
		checkOperProgressTarget(t, h.typed, 0, TargetResult{ID: "op-0001", Outcome: TargetUnobserved})
	})
	t.Run("exact identity wins over a shared name", func(t *testing.T) {
		h := newOperProgressHarness(t, sameName)
		h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","name":"同名干员","result":"Failed","result_detail":"ResourceInsufficient"}`))
		checkOperProgressTarget(t, h.typed, 0, TargetResult{ID: "op-0001", Outcome: TargetFailed, Code: "resource_insufficient"})
		checkOperProgressTarget(t, h.typed, 1, TargetResult{ID: "op-0002", Outcome: TargetUnobserved})
	})
	t.Run("a missing name does not replace confirmed facts", func(t *testing.T) {
		h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2},{"role":"Warrior","name":"乙","elite":2}]}`)
		h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}`))
		h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","result":"Failed","result_detail":"RecognitionFailed"}`))
		checkOperProgressTarget(t, h.typed, 0, TargetResult{ID: "op-0001", Outcome: TargetAchieved, Code: "completed", Details: json.RawMessage(`{"action":"elite","expected":2,"reported":2}`)})
		checkOperProgressTarget(t, h.typed, 1, TargetResult{ID: "op-0002", Outcome: TargetAmbiguous, Code: "association_ambiguous"})
	})
	t.Run("empty role or name is never associated", func(t *testing.T) {
		h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2}]}`)
		for _, details := range []string{
			`{"role":"","name":"甲","result":"Failed","result_detail":"ResourceInsufficient"}`,
			`{"role":"Warrior","name":"","result":"Failed","result_detail":"ResourceInsufficient"}`,
			`{"role":"warrior","name":"甲","result":"Failed","result_detail":"ResourceInsufficient"}`,
		} {
			require.False(t, h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(details)), "malformed identity %s changed a target", details)
		}
	})
}

func TestOperProgressResultIgnoresUnrelatedCallbacks(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2}]}`)
	callbacks := []struct {
		messageID int32
		raw       string
	}{
		{20002, operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}`)},
		{10002, `{"taskchain":"OperProgress","taskid":41}`},
		{20003, `{"taskchain":"OperProgress","taskid":41,"what":"FutureWhat","details":{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}}`},
		{20003, `{"taskchain":"SwitchTheme","taskid":41,"what":"OperProgressDetail","details":{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}}`},
		{20003, `{"taskid":41,"what":"OperProgressDetail","details":{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}}`},
		{20003, `{"taskchain":7,"taskid":41,"what":"OperProgressDetail","details":{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}}`},
		{20003, `{"taskchain":"OperProgress","taskid":41,"what":7,"details":{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}}`},
		{20003, `{"taskchain":"OperProgress","taskid":41,"what":"OperProgressDetail"}`},
		{20003, `{"taskchain":"OperProgress","taskid":41,"what":"OperProgressDetail","details":[]}`},
		{20003, `{"taskchain":"OperProgress","taskid":41,"what":"OperProgressDetail","details":null}`},
		{20003, `{"taskchain":"OperProgress","taskid":41,"what":"OperProgressDetail","details":{"role":7,"name":true,"result":"Success","result_detail":"Completed","elite":2}}`},
	}
	for _, tc := range callbacks {
		require.False(t, h.apply(tc.messageID, tc.raw), "unrelated callback %s created a target fact", tc.raw)
	}
	checkOperProgressTarget(t, h.typed, 0, TargetResult{ID: "op-0001", Outcome: TargetUnobserved})
	// Unknown future fields do not prevent a well-formed detail from being
	// interpreted through its known fields.
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2,"future_field":{"n":9007199254740993}}`))
	checkOperProgressTarget(t, h.typed, 0, TargetResult{ID: "op-0001", Outcome: TargetAchieved, Code: "completed", Details: json.RawMessage(`{"action":"elite","expected":2,"reported":2}`)})
}

func TestOperProgressResultDetailFiltersNativePayload(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2}]}`)
	raw := operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2,` +
		`"rect":[1,2,3,4],"score":0.99,"path":"/private/work","raw":{"full":"payload"},"future":{"n":9007199254740993}}`)
	h.apply(operProgressMessageSubTaskExtraInfo, raw)
	const want = `{"action":"elite","expected":2,"reported":2}`
	require.Equal(t, want, string(h.typed.Targets[0].Details), "retained detail = %s, want %s", h.typed.Targets[0].Details, want)
	for _, forbidden := range []string{"rect", "score", "path", "raw", "future", "9007199254740993"} {
		require.False(t, strings.Contains(string(h.typed.Targets[0].Details), forbidden), "retained detail disclosed %q", forbidden)
	}
}

func TestOperProgressResultSummaryCountsConfirmedFactsOnly(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2},{"role":"Caster","name":"乙","skill_mastery":[0,0,3]}]}`)
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}`))
	// Core's own counters and finished list cannot turn the unobserved mastery
	// target into success.
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressSummaryCallback(`{"finished":[{"role":"Warrior","name":"甲","elite":2},{"role":"Caster","name":"乙","skill_mastery":[0,0,3]}],"success":2,"failed":0,"skipped":0}`))
	const want = `{"targets":2,"observed":1,"unobserved":1,"achieved":1,"reported_target_training_started":0,"reported_prerequisite_training_started":0,"failed":0,"ambiguous":0,"skipped_training_room_busy":0}`
	require.Equal(t, want, string(h.typed.Summary), "summary = %s, want %s", h.typed.Summary, want)
	checkOperProgressTarget(t, h.typed, 1, TargetResult{ID: "op-0002", Outcome: TargetUnobserved})
	require.False(t, h.typed.State != Unknown || h.typed.Code != "typed_incomplete", "summary fabricated success: %s/%s", h.typed.State, h.typed.Code)
}

func TestOperProgressResultSummaryCountsFailuresAndSkips(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2},{"role":"Warrior","name":"乙","elite":2},{"role":"Caster","name":"丙","skill_mastery":[0,0,3]}]}`)
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}`))
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","name":"乙","result":"Failed","result_detail":"OperatorNotFound"}`))
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Caster","name":"丙","result":"Skipped","result_detail":"TrainingRoomBusy"}`))
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressSummaryCallback(`{"finished":[{"role":"Warrior","name":"甲","elite":2}],"success":1,"failed":1,"skipped":1}`))
	const want = `{"targets":3,"observed":2,"unobserved":1,"achieved":1,"reported_target_training_started":0,"reported_prerequisite_training_started":0,"failed":1,"ambiguous":0,"skipped_training_room_busy":1}`
	require.Equal(t, want, string(h.typed.Summary), "summary = %s, want %s", h.typed.Summary, want)
}

func TestOperProgressResultSummaryIsGeneratedOnce(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2},{"role":"Warrior","name":"乙","elite":2}]}`)
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}`))
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressSummaryCallback(`{}`))
	first := string(h.typed.Summary)
	require.Contains(t, first, `"observed":1`, "first summary = %s", first)
	// A later fact and a second summary must not replace the first summary.
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","name":"乙","result":"Success","result_detail":"Completed","elite":2}`))
	require.False(t, h.apply(operProgressMessageSubTaskExtraInfo, operProgressSummaryCallback(`{}`)), "second summary replaced the first")
	require.Equal(t, first, string(h.typed.Summary), "summary changed to %s", h.typed.Summary)
	require.Equal(t, Succeeded, h.typed.State, "all achieved after summary = %s", h.typed.State)
}

func TestOperProgressResultSummaryRequiresAnObjectWhenPresent(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2}]}`)
	for _, raw := range []string{
		operProgressCallback(operProgressWhatSummary, `[]`),
		operProgressCallback(operProgressWhatSummary, `null`),
		operProgressCallback(operProgressWhatSummary, `"future"`),
	} {
		require.False(t, h.apply(operProgressMessageSubTaskExtraInfo, raw), "malformed summary %s was admitted", raw)
	}
	require.Empty(t, h.typed.Summary, "malformed summary was retained: %s", h.typed.Summary)
	// A summary without a details object still derives counts from confirmed
	// facts only.
	h.apply(operProgressMessageSubTaskExtraInfo, `{"taskchain":"OperProgress","taskid":41,"what":"OperProgressSummary"}`)
	require.Contains(t, string(h.typed.Summary), `"unobserved":1`, "summary without details = %s", h.typed.Summary)
}

func TestOperProgressResultMalformedCallback(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2}]}`)
	for _, raw := range []string{
		`{`,
		`[]`,
		`null`,
		`"OperProgressDetail"`,
		`{"taskchain":"OperProgress","taskchain":"Other","what":"OperProgressDetail"}`,
		`{"taskchain":"OperProgress","what":"OperProgressDetail","details":{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","result_detail":"AlreadySatisfied","elite":2}}`,
	} {
		_, err := h.reducer.Observe(Observation{MessageID: operProgressMessageSubTaskExtraInfo, JSON: json.RawMessage(raw)})
		require.ErrorIs(t, err, ErrTypedResult, "malformed callback %s", raw)
	}
	checkOperProgressTarget(t, h.typed, 0, TargetResult{ID: "op-0001", Outcome: TargetUnobserved})
	// An empty observation is ignored so callers can probe the reducer.
	h.observe(operProgressMessageSubTaskExtraInfo, ``)
}

func TestOperProgressResultDuplicateAndLateFacts(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2}]}`)
	raw := operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2}`)
	require.True(t, h.apply(operProgressMessageSubTaskExtraInfo, raw), "first fact was not admitted")
	require.False(t, h.apply(operProgressMessageSubTaskExtraInfo, raw), "identical repeat changed the result")
	// The runtime reports typed_conflict for a different later fact; the
	// reducer must surface it rather than silently rewriting the original.
	update := h.observe(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Failed","result_detail":"RecognitionFailed"}`))
	next, _, err := applyTypedUpdate(h.typed, update)
	require.ErrorIs(t, err, ErrTypedConflict, "conflicting fact = %+v", next)
	require.Nil(t, next, "conflicting fact = %+v, %v", next, err)
	checkOperProgressTarget(t, h.typed, 0, TargetResult{ID: "op-0001", Outcome: TargetAchieved, Code: "completed", Details: json.RawMessage(`{"action":"elite","expected":2,"reported":2}`)})
}

func TestOperProgressResultBudgetAtTargetLimit(t *testing.T) {
	h := newOperProgressHarness(t, operProgressPlansJSON(operProgressMaxPlans))
	for i := 0; i < operProgressMaxPlans; i++ {
		details := fmt.Sprintf(`{"role":"Warrior","name":"operator-%03d","result":"Success","result_detail":"Completed","elite":1}`, i)
		require.True(t, h.apply(operProgressMessageSubTaskExtraInfo, operProgressDetailCallback(details)), "target %d was not admitted", i)
	}
	err := validateTypedResult(h.typed)
	require.NoError(t, err, "bounded task result rejected")
	h.apply(operProgressMessageSubTaskExtraInfo, operProgressSummaryCallback(`{"success":128}`))
	require.LessOrEqual(t, len(h.typed.Summary), MaxTypedSummaryBytes, "summary is %d bytes", len(h.typed.Summary))
	err = validateTypedResult(h.typed)
	require.NoError(t, err, "result with summary rejected")
	require.False(t, h.typed.State != Succeeded || !strings.Contains(string(h.typed.Summary), `"achieved":128`), "aggregate = %s, summary = %s", h.typed.State, h.typed.Summary)
}

func TestOperProgressResultIgnoresOversizedFutureFields(t *testing.T) {
	h := newOperProgressHarness(t, `{"plans":[{"role":"Warrior","name":"甲","elite":2}]}`)
	future := strings.Repeat("x", 64<<10)
	raw := operProgressDetailCallback(`{"role":"Warrior","name":"甲","result":"Success","result_detail":"Completed","elite":2,"future":"` + future + `"}`)
	h.apply(operProgressMessageSubTaskExtraInfo, raw)
	const want = `{"action":"elite","expected":2,"reported":2}`
	require.Equal(t, want, string(h.typed.Targets[0].Details), "future field leaked into details: %s", h.typed.Targets[0].Details)
	require.NoError(t, validateTypedResult(h.typed))
}

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
