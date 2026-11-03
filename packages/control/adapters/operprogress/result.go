package operprogress

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
)

const (
	operProgressMessageSubTaskExtraInfo int32 = 20003

	operProgressWhatDetail  = "OperProgressDetail"
	operProgressWhatSummary = "OperProgressSummary"

	operProgressResultSuccess = "Success"
	operProgressResultFailed  = "Failed"
	operProgressResultSkipped = "Skipped"

	operProgressDetailCompleted            = "Completed"
	operProgressDetailAlreadySatisfied     = "AlreadySatisfied"
	operProgressDetailPrerequisiteTraining = "PrerequisiteTraining"
	operProgressDetailTrainingRoomBusy     = "TrainingRoomBusy"
)

var operProgressFailureCodes = map[string]string{
	"ResourceInsufficient": "resource_insufficient",
	"OperatorNotFound":     "operator_not_found",
	"PrerequisiteNotMet":   "prerequisite_not_met",
	"ChipNotCraftable":     "chip_not_craftable",
	"FormulaLocked":        "formula_locked",
	"Unsupported":          "unsupported",
	"RecognitionFailed":    "recognition_failed",
	"Interrupt":            "interrupt",
}

const (
	operProgressCodeCompleted                  = "completed"
	operProgressCodeAlreadySatisfied           = "already_satisfied"
	operProgressCodeMasteryTrainingStarted     = "mastery_training_started"
	operProgressCodeMasteryPrerequisiteStarted = "mastery_prerequisite_training_started"
	operProgressCodeUnspecifiedFailure         = "unspecified_failure"
	operProgressCodeAssociationAmbiguous       = "association_ambiguous"
	operProgressCodeDetailIncomplete           = "detail_incomplete"
	operProgressCodeDetailMalformed            = "detail_malformed"
	operProgressCodeDetailIncompatible         = "detail_incompatible"
)

type operProgressReducer struct {
	targets          []operProgressTarget
	exact            map[string][]int
	facts            []control.TargetOutcome
	factSet          []bool
	skipped          []bool
	trainingRoomBusy int
	summaryIssued    bool
}

func newOperProgressReducer(targets []operProgressTarget) control.TaskReducer {
	r := &operProgressReducer{
		targets: append([]operProgressTarget(nil), targets...),
		exact:   make(map[string][]int, len(targets)),
		facts:   make([]control.TargetOutcome, len(targets)),
		factSet: make([]bool, len(targets)),
		skipped: make([]bool, len(targets)),
	}
	for i, target := range r.targets {
		key := target.Role + "\x00" + target.Name
		r.exact[key] = append(r.exact[key], i)
	}
	return r
}

func (r *operProgressReducer) Observe(o control.Observation) (control.TypedUpdate, error) {
	if o.MessageID != operProgressMessageSubTaskExtraInfo || len(o.JSON) == 0 {
		return control.TypedUpdate{}, nil
	}
	trimmed := bytes.TrimSpace(o.JSON)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "oper_progress: callback must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := strictjson.CheckKeys(dec); err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "oper_progress: ambiguous or malformed callback fields")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "oper_progress: callback must be a JSON object")
	}
	if chain, ok := strictjson.FieldString(top, "taskchain"); !ok || chain != operProgressTaskType {
		return control.TypedUpdate{}, nil
	}
	what, ok := strictjson.FieldString(top, "what")
	if !ok {
		return control.TypedUpdate{}, nil
	}
	switch what {
	case operProgressWhatDetail:
		return r.observeDetail(top)
	case operProgressWhatSummary:
		return r.observeSummary(top)
	default:
		return control.TypedUpdate{}, nil
	}
}

func (r *operProgressReducer) observeDetail(top map[string]json.RawMessage) (control.TypedUpdate, error) {
	fields, ok := strictjson.ObjectField(top, "details")
	if !ok {
		return control.TypedUpdate{}, nil
	}
	role, roleOK := strictjson.FieldString(fields, "role")
	name, nameOK := strictjson.FieldString(fields, "name")
	result, resultOK := strictjson.FieldString(fields, "result")
	detail, detailOK := strictjson.FieldString(fields, "result_detail")

	if roleOK && nameOK {
		if !operProgressValidRole(role) || name == "" {
			return control.TypedUpdate{}, nil
		}
		matches := r.exact[role+"\x00"+name]
		switch len(matches) {
		case 0:
			return control.TypedUpdate{}, nil
		case 1:
			return r.associatedDetail(matches[0], result, resultOK, detail, detailOK, fields)
		default:
			return r.ambiguousUpdate(matches), nil
		}
	}
	if roleOK && !operProgressValidRole(role) {
		return control.TypedUpdate{}, nil
	}
	if nameOK && name == "" {
		return control.TypedUpdate{}, nil
	}
	if resultOK && result == operProgressResultSkipped {
		return control.TypedUpdate{}, nil
	}
	var candidates []int
	switch {
	case nameOK:
		for i, target := range r.targets {
			if target.Name == name {
				candidates = append(candidates, i)
			}
		}
	case roleOK:
		for i, target := range r.targets {
			if target.Role == role {
				candidates = append(candidates, i)
			}
		}
	}
	return r.ambiguousUpdate(candidates), nil
}

func (r *operProgressReducer) associatedDetail(index int, result string, resultOK bool, detail string, detailOK bool, fields map[string]json.RawMessage) (control.TypedUpdate, error) {
	if !resultOK {
		return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncomplete, nil), nil
	}
	switch result {
	case operProgressResultSuccess:
		return r.successDetail(index, detail, detailOK, fields)
	case operProgressResultFailed:
		return r.failedFact(index, detail, detailOK), nil
	case operProgressResultSkipped:
		if detailOK && detail == operProgressDetailTrainingRoomBusy {
			r.countTrainingRoomBusy(index)
		}
		return control.TypedUpdate{}, nil
	default:
		return control.TypedUpdate{}, nil
	}
}

func (r *operProgressReducer) successDetail(index int, detail string, detailOK bool, fields map[string]json.RawMessage) (control.TypedUpdate, error) {
	if !detailOK {
		return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncomplete, nil), nil
	}
	switch detail {
	case operProgressDetailCompleted, operProgressDetailAlreadySatisfied, operProgressDetailPrerequisiteTraining:
		return r.successFact(index, detail, fields)
	case operProgressDetailTrainingRoomBusy:
		return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
	default:
		if _, known := operProgressFailureCodes[detail]; known {
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		return control.TypedUpdate{}, nil
	}
}

func (r *operProgressReducer) successFact(index int, detail string, fields map[string]json.RawMessage) (control.TypedUpdate, error) {
	target := r.targets[index]
	present := 0
	for _, key := range []string{"elite", "skill_level", "skill_mastery"} {
		if _, ok := fields[key]; ok {
			present++
		}
	}
	if present == 0 {
		return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncomplete, nil), nil
	}
	if present > 1 {
		return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
	}

	var level int
	switch target.Action {
	case operProgressActionElite, operProgressActionSkillLevel:
		key, limit := "elite", 2
		if target.Action == operProgressActionSkillLevel {
			key, limit = "skill_level", 7
		}
		raw, ok := fields[key]
		if !ok {
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		n, err := strictjson.Integer(raw)
		if err != nil || n < 1 || n > limit {
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailMalformed, nil), nil
		}
		level = n
	case operProgressActionSkillMastery:
		raw, ok := fields["skill_mastery"]
		if !ok {
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		values, ok := operProgressMasteryValues(raw)
		if !ok || target.MasterySlot < 1 || target.MasterySlot > 3 {
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailMalformed, nil), nil
		}
		for i, value := range values {
			if i != target.MasterySlot-1 && value != 0 {
				return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
			}
		}
		level = values[target.MasterySlot-1]
		if level == 0 {
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
	default:
		return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailMalformed, nil), nil
	}

	outcome, code := control.TargetAchieved, operProgressCodeCompleted
	switch detail {
	case operProgressDetailCompleted:
		if level != target.Expected {
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		if target.Action == operProgressActionSkillMastery {
			outcome, code = control.TargetReportedTrainingStarted, operProgressCodeMasteryTrainingStarted
		}
	case operProgressDetailAlreadySatisfied:
		if level < target.Expected {
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		code = operProgressCodeAlreadySatisfied
	case operProgressDetailPrerequisiteTraining:
		if target.Action != operProgressActionSkillMastery || level >= target.Expected {
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		outcome, code = control.TargetReportedPrerequisiteStarted, operProgressCodeMasteryPrerequisiteStarted
	}
	details, err := json.Marshal(operProgressTargetFact{
		Action:   string(target.Action),
		Expected: target.Expected,
		Slot:     target.MasterySlot,
		Reported: level,
	})
	if err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "oper_progress: cannot encode target fact")
	}
	return r.factUpdate(index, outcome, code, details), nil
}

func (r *operProgressReducer) failedFact(index int, detail string, detailOK bool) control.TypedUpdate {
	code := operProgressCodeUnspecifiedFailure
	if detailOK {
		switch detail {
		case operProgressDetailCompleted, operProgressDetailAlreadySatisfied,
			operProgressDetailPrerequisiteTraining, operProgressDetailTrainingRoomBusy:
			return r.factUpdate(index, control.TargetAmbiguous, operProgressCodeDetailIncompatible, nil)
		default:
			if mapped, ok := operProgressFailureCodes[detail]; ok {
				code = mapped
			}
		}
	}
	return r.factUpdate(index, control.TargetFailed, code, nil)
}

func operProgressMasteryValues(raw json.RawMessage) ([3]int, bool) {
	var values [3]int
	items, err := strictjson.Array(raw)
	if err != nil || len(items) != 3 {
		return values, false
	}
	for i, item := range items {
		n, err := strictjson.Integer(item)
		if err != nil || n < 0 || n > 3 {
			return values, false
		}
		values[i] = n
	}
	return values, true
}

func (r *operProgressReducer) factUpdate(index int, outcome control.TargetOutcome, code string, details json.RawMessage) control.TypedUpdate {
	if !r.factSet[index] {
		r.factSet[index], r.facts[index] = true, outcome
		if r.skipped[index] {
			r.skipped[index] = false
			r.trainingRoomBusy--
		}
	}
	return control.TypedUpdate{Targets: []control.TargetResult{{ID: r.targets[index].ID, Outcome: outcome, Code: code, Details: details}}}
}

func (r *operProgressReducer) ambiguousUpdate(indexes []int) control.TypedUpdate {
	var targets []control.TargetResult
	for _, index := range indexes {
		if r.factSet[index] {
			continue
		}
		r.factSet[index], r.facts[index] = true, control.TargetAmbiguous
		if r.skipped[index] {
			r.skipped[index] = false
			r.trainingRoomBusy--
		}
		targets = append(targets, control.TargetResult{ID: r.targets[index].ID, Outcome: control.TargetAmbiguous, Code: operProgressCodeAssociationAmbiguous})
	}
	if len(targets) == 0 {
		return control.TypedUpdate{}
	}
	return control.TypedUpdate{Targets: targets}
}

func (r *operProgressReducer) countTrainingRoomBusy(index int) {
	if r.factSet[index] || r.skipped[index] {
		return
	}
	r.skipped[index] = true
	r.trainingRoomBusy++
}

type operProgressTargetFact struct {
	Action   string `json:"action"`
	Expected int    `json:"expected"`
	Slot     int    `json:"slot,omitempty"`
	Reported int    `json:"reported"`
}

type operProgressSummary struct {
	Targets                             int `json:"targets"`
	Observed                            int `json:"observed"`
	Unobserved                          int `json:"unobserved"`
	Achieved                            int `json:"achieved"`
	ReportedTargetTrainingStarted       int `json:"reported_target_training_started"`
	ReportedPrerequisiteTrainingStarted int `json:"reported_prerequisite_training_started"`
	Failed                              int `json:"failed"`
	Ambiguous                           int `json:"ambiguous"`
	SkippedTrainingRoomBusy             int `json:"skipped_training_room_busy"`
}

func (r *operProgressReducer) observeSummary(top map[string]json.RawMessage) (control.TypedUpdate, error) {
	if r.summaryIssued {
		return control.TypedUpdate{}, nil
	}
	if raw, ok := top["details"]; ok {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return control.TypedUpdate{}, nil
		}
	}
	summary := operProgressSummary{Targets: len(r.targets), SkippedTrainingRoomBusy: r.trainingRoomBusy}
	for i := range r.targets {
		if !r.factSet[i] {
			continue
		}
		summary.Observed++
		switch r.facts[i] {
		case control.TargetAchieved:
			summary.Achieved++
		case control.TargetReportedTrainingStarted:
			summary.ReportedTargetTrainingStarted++
		case control.TargetReportedPrerequisiteStarted:
			summary.ReportedPrerequisiteTrainingStarted++
		case control.TargetFailed:
			summary.Failed++
		case control.TargetAmbiguous:
			summary.Ambiguous++
		}
	}
	summary.Unobserved = summary.Targets - summary.Observed
	raw, err := json.Marshal(summary)
	if err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "oper_progress: cannot encode summary")
	}
	r.summaryIssued = true
	return control.TypedUpdate{Summary: raw}, nil
}
