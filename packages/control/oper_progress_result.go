package control

import (
	"bytes"
	"encoding/json"
)

// Core emits OperProgress training facts as AsstMsg::SubTaskExtraInfo (20003)
// with what=OperProgressDetail per training action and what=OperProgressSummary
// once the plan loop ends. Only those two shapes are interpreted here.
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

// operProgressFailureCodes maps the native ResultDetail enum names to stable
// lowercase reason codes. A failed detail remains failed when the reason is
// missing or unknown; its code then reports that no specific reason was
// understood.
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

// operProgressReducer interprets callbacks for one prepared OperProgress task.
// The runtime observes it serially on its single monitor goroutine, so it needs
// no internal locking. Scratch state is bounded by the prepared target count and
// never retains Observation.JSON.
type operProgressReducer struct {
	targets []operProgressTarget
	// exact indexes targets by exact (role, name). Strict preparation rejects
	// duplicate pairs, so every value holds exactly one index.
	exact map[string][]int
	// facts and factSet record the first non-unobserved fact emitted for each
	// target so the summary can be derived without retaining callback history.
	facts   []TargetOutcome
	factSet []bool
	// skipped marks targets whose mastery attempt was skipped because the
	// training room was busy. A skip is summary evidence, never a target fact.
	skipped          []bool
	trainingRoomBusy int
	summaryIssued    bool
}

// newOperProgressReducer binds a fresh reducer to a detached copy of the target
// mapping prepared for exactly one task.
func newOperProgressReducer(targets []operProgressTarget) TaskReducer {
	r := &operProgressReducer{
		targets: append([]operProgressTarget(nil), targets...),
		exact:   make(map[string][]int, len(targets)),
		facts:   make([]TargetOutcome, len(targets)),
		factSet: make([]bool, len(targets)),
		skipped: make([]bool, len(targets)),
	}
	for i, target := range r.targets {
		key := target.Role + "\x00" + target.Name
		r.exact[key] = append(r.exact[key], i)
	}
	return r
}

// Observe interprets one Core callback. Unrelated message kinds, unknown
// `what` values, foreign task chains, unknown future fields and unknown future
// result tokens are ignored so the targets stay unobserved instead of gaining a
// fabricated fact. Only an unusable callback object (malformed JSON or duplicate
// keys at any depth) returns an error, which the runtime maps to
// typed_invalid_event without admitting the update.
func (r *operProgressReducer) Observe(o Observation) (TypedUpdate, error) {
	if o.MessageID != operProgressMessageSubTaskExtraInfo || len(o.JSON) == 0 {
		return TypedUpdate{}, nil
	}
	trimmed := bytes.TrimSpace(o.JSON)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return TypedUpdate{}, typedError("oper_progress: callback must be a JSON object")
	}
	// Duplicate keys would make every consumed field ambiguous. The runtime
	// already rejects duplicate top-level fields; nested detail objects are
	// checked here before any field is interpreted.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := checkTypedKeys(dec); err != nil {
		return TypedUpdate{}, typedError("oper_progress: ambiguous or malformed callback fields")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return TypedUpdate{}, typedError("oper_progress: callback must be a JSON object")
	}
	if chain, ok := operProgressFieldString(top, "taskchain"); !ok || chain != operProgressTaskType {
		return TypedUpdate{}, nil
	}
	what, ok := operProgressFieldString(top, "what")
	if !ok {
		return TypedUpdate{}, nil
	}
	switch what {
	case operProgressWhatDetail:
		return r.observeDetail(top)
	case operProgressWhatSummary:
		return r.observeSummary(top)
	default:
		return TypedUpdate{}, nil
	}
}

// operProgressFieldString decodes one exact JSON string literal. A missing,
// null, non-string or malformed field reports false; callers treat that as an
// unusable association field rather than guessing a value.
func operProgressFieldString(fields map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := fields[key]
	if !ok {
		return "", false
	}
	value, err := operProgressDecodeString(raw)
	if err != nil {
		return "", false
	}
	return value, true
}

// operProgressDetailFields returns the decoded details object. Missing, null or
// non-object details cannot carry a business fact and are ignored.
func operProgressDetailFields(top map[string]json.RawMessage) (map[string]json.RawMessage, bool) {
	raw, ok := top["details"]
	if !ok {
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return nil, false
	}
	return fields, true
}

func (r *operProgressReducer) observeDetail(top map[string]json.RawMessage) (TypedUpdate, error) {
	fields, ok := operProgressDetailFields(top)
	if !ok {
		return TypedUpdate{}, nil
	}
	role, roleOK := operProgressFieldString(fields, "role")
	name, nameOK := operProgressFieldString(fields, "name")
	result, resultOK := operProgressFieldString(fields, "result")
	detail, detailOK := operProgressFieldString(fields, "result_detail")

	// Exact (role, name) identity is the only association that may carry a
	// success or failure fact. Arrival order and input order are never used.
	if roleOK && nameOK {
		if !operProgressValidRole(role) || name == "" {
			return TypedUpdate{}, nil
		}
		matches := r.exact[role+"\x00"+name]
		switch len(matches) {
		case 0:
			return TypedUpdate{}, nil
		case 1:
			return r.associatedDetail(matches[0], result, resultOK, detail, detailOK, fields)
		default:
			// Strict preparation rejects duplicate pairs; never pick one.
			return r.ambiguousUpdate(matches), nil
		}
	}
	if roleOK && !operProgressValidRole(role) {
		// A concrete role that a strict plan cannot carry names no target.
		return TypedUpdate{}, nil
	}
	if nameOK && name == "" {
		return TypedUpdate{}, nil
	}
	if resultOK && result == operProgressResultSkipped {
		// A skip is summary evidence only. Without a unique target it cannot
		// even be counted, and it never creates a target fact.
		return TypedUpdate{}, nil
	}
	// A missing or malformed role or name cannot be repaired by guessing. Mark
	// every still-unobserved candidate named by the remaining field ambiguous
	// rather than selecting the first match.
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

// associatedDetail interprets one detail event whose (role, name) identifies
// exactly one prepared target.
func (r *operProgressReducer) associatedDetail(index int, result string, resultOK bool, detail string, detailOK bool, fields map[string]json.RawMessage) (TypedUpdate, error) {
	if !resultOK {
		return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncomplete, nil), nil
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
		return TypedUpdate{}, nil
	default:
		// Future result tokens cannot establish either success or failure.
		return TypedUpdate{}, nil
	}
}

// successDetail requires a recognized completion reason and a reported value
// compatible with the planned action before it admits any success evidence.
func (r *operProgressReducer) successDetail(index int, detail string, detailOK bool, fields map[string]json.RawMessage) (TypedUpdate, error) {
	if !detailOK {
		return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncomplete, nil), nil
	}
	switch detail {
	case operProgressDetailCompleted, operProgressDetailAlreadySatisfied, operProgressDetailPrerequisiteTraining:
		return r.successFact(index, detail, fields)
	case operProgressDetailTrainingRoomBusy:
		return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
	default:
		if _, known := operProgressFailureCodes[detail]; known {
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		// Unknown future success reason: derive nothing.
		return TypedUpdate{}, nil
	}
}

func (r *operProgressReducer) successFact(index int, detail string, fields map[string]json.RawMessage) (TypedUpdate, error) {
	target := r.targets[index]
	present := 0
	for _, key := range []string{"elite", "skill_level", "skill_mastery"} {
		if _, ok := fields[key]; ok {
			present++
		}
	}
	if present == 0 {
		return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncomplete, nil), nil
	}
	if present > 1 {
		return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
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
			// The reported action does not match the planned action.
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		n, err := operProgressInteger(raw)
		if err != nil || n < 1 || n > limit {
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailMalformed, nil), nil
		}
		level = n
	case operProgressActionSkillMastery:
		raw, ok := fields["skill_mastery"]
		if !ok {
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		values, ok := operProgressMasteryValues(raw)
		if !ok || target.MasterySlot < 1 || target.MasterySlot > 3 {
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailMalformed, nil), nil
		}
		for i, value := range values {
			if i != target.MasterySlot-1 && value != 0 {
				return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
			}
		}
		level = values[target.MasterySlot-1]
		if level == 0 {
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
	default:
		return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailMalformed, nil), nil
	}

	outcome, code := TargetAchieved, operProgressCodeCompleted
	switch detail {
	case operProgressDetailCompleted:
		if level != target.Expected {
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		if target.Action == operProgressActionSkillMastery {
			// Core reports that training at the target mastery level started.
			// It does not prove training finished or was independently checked.
			outcome, code = TargetReportedTrainingStarted, operProgressCodeMasteryTrainingStarted
		}
	case operProgressDetailAlreadySatisfied:
		if level < target.Expected {
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		code = operProgressCodeAlreadySatisfied
	case operProgressDetailPrerequisiteTraining:
		if target.Action != operProgressActionSkillMastery || level >= target.Expected {
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil), nil
		}
		// The array reports the started prerequisite level, not the final target.
		outcome, code = TargetReportedPrerequisiteStarted, operProgressCodeMasteryPrerequisiteStarted
	}
	details, err := json.Marshal(operProgressTargetFact{
		Action:   string(target.Action),
		Expected: target.Expected,
		Slot:     target.MasterySlot,
		Reported: level,
	})
	if err != nil {
		return TypedUpdate{}, typedError("oper_progress: cannot encode target fact")
	}
	return r.factUpdate(index, outcome, code, details), nil
}

// failedFact records a uniquely associated failure. Failure details carry no
// action or value, so the exact (role, name) identity is sufficient. A known
// reason becomes a stable lowercase code; an unknown or missing reason keeps the
// failure but reports that no specific reason was understood.
func (r *operProgressReducer) failedFact(index int, detail string, detailOK bool) TypedUpdate {
	code := operProgressCodeUnspecifiedFailure
	if detailOK {
		switch detail {
		case operProgressDetailCompleted, operProgressDetailAlreadySatisfied,
			operProgressDetailPrerequisiteTraining, operProgressDetailTrainingRoomBusy:
			// A success or skip reason cannot describe a failed result.
			return r.factUpdate(index, TargetAmbiguous, operProgressCodeDetailIncompatible, nil)
		default:
			if mapped, ok := operProgressFailureCodes[detail]; ok {
				code = mapped
			}
		}
	}
	return r.factUpdate(index, TargetFailed, code, nil)
}

// operProgressMasteryValues accepts exactly three integer levels in 0-3 without
// a float64 roundtrip.
func operProgressMasteryValues(raw json.RawMessage) ([3]int, bool) {
	var values [3]int
	items, err := operProgressJSONArray(raw)
	if err != nil || len(items) != 3 {
		return values, false
	}
	for i, item := range items {
		n, err := operProgressInteger(item)
		if err != nil || n < 0 || n > 3 {
			return values, false
		}
		values[i] = n
	}
	return values, true
}

// factUpdate records the first fact for one target and returns it as a single
// target update. A later different fact is still returned so the runtime can
// report typed_conflict and preserve the original; the reducer keeps counting
// the original fact because the runtime rejects the conflicting update.
func (r *operProgressReducer) factUpdate(index int, outcome TargetOutcome, code string, details json.RawMessage) TypedUpdate {
	if !r.factSet[index] {
		r.factSet[index], r.facts[index] = true, outcome
		if r.skipped[index] {
			r.skipped[index] = false
			r.trainingRoomBusy--
		}
	}
	return TypedUpdate{Targets: []TargetResult{{ID: r.targets[index].ID, Outcome: outcome, Code: code, Details: details}}}
}

// ambiguousUpdate marks still-unobserved candidates as association-ambiguous.
// Targets that already hold a fact keep it; an unconfirmed event must not
// replace confirmed evidence.
func (r *operProgressReducer) ambiguousUpdate(indexes []int) TypedUpdate {
	var targets []TargetResult
	for _, index := range indexes {
		if r.factSet[index] {
			continue
		}
		r.factSet[index], r.facts[index] = true, TargetAmbiguous
		if r.skipped[index] {
			r.skipped[index] = false
			r.trainingRoomBusy--
		}
		targets = append(targets, TargetResult{ID: r.targets[index].ID, Outcome: TargetAmbiguous, Code: operProgressCodeAssociationAmbiguous})
	}
	if len(targets) == 0 {
		return TypedUpdate{}
	}
	return TypedUpdate{Targets: targets}
}

// countTrainingRoomBusy records a skipped mastery attempt once per target. A
// target that already carries a fact or was already skipped is not recounted.
func (r *operProgressReducer) countTrainingRoomBusy(index int) {
	if r.factSet[index] || r.skipped[index] {
		return
	}
	r.skipped[index] = true
	r.trainingRoomBusy++
}

// operProgressTargetFact is the filtered business detail retained for one
// target. It contains the planned action and expected level plus the level Core
// reported, never OCR geometry, scores, paths or the raw callback.
type operProgressTargetFact struct {
	Action   string `json:"action"`
	Expected int    `json:"expected"`
	Slot     int    `json:"slot,omitempty"`
	Reported int    `json:"reported"`
}

// operProgressSummary is the aggregate fact emitted once per task when Core
// sends OperProgressSummary. It counts only facts confirmed by this reducer and
// the training-room-busy skips that produced no target fact. Core's own
// success/failed/skipped counters and finished list are deliberately not copied:
// they merge mastery startups and cannot describe each planned target.
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

func (r *operProgressReducer) observeSummary(top map[string]json.RawMessage) (TypedUpdate, error) {
	if r.summaryIssued {
		// The first summary is immutable. A duplicate or late summary cannot
		// replace it and must not be reported as a conflicting update.
		return TypedUpdate{}, nil
	}
	if raw, ok := top["details"]; ok {
		trimmed := bytes.TrimSpace(raw)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			return TypedUpdate{}, nil
		}
	}
	summary := operProgressSummary{Targets: len(r.targets), SkippedTrainingRoomBusy: r.trainingRoomBusy}
	for i := range r.targets {
		if !r.factSet[i] {
			continue
		}
		summary.Observed++
		switch r.facts[i] {
		case TargetAchieved:
			summary.Achieved++
		case TargetReportedTrainingStarted:
			summary.ReportedTargetTrainingStarted++
		case TargetReportedPrerequisiteStarted:
			summary.ReportedPrerequisiteTrainingStarted++
		case TargetFailed:
			summary.Failed++
		case TargetAmbiguous:
			summary.Ambiguous++
		}
	}
	summary.Unobserved = summary.Targets - summary.Observed
	raw, err := json.Marshal(summary)
	if err != nil {
		return TypedUpdate{}, typedError("oper_progress: cannot encode summary")
	}
	r.summaryIssued = true
	return TypedUpdate{Summary: raw}, nil
}
