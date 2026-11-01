package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
)

var (
	ErrTypedResult   = errors.New("control: invalid typed result")
	ErrTypedConflict = errors.New("control: conflicting typed fact")
	ErrTypedBudget   = errors.New("control: typed budget exceeded")
)

type TargetOutcome string

const (
	TargetUnobserved                  TargetOutcome = "unobserved"
	TargetAchieved                    TargetOutcome = "achieved"
	TargetReportedTrainingStarted     TargetOutcome = "reported_target_training_started"
	TargetReportedPrerequisiteStarted TargetOutcome = "reported_prerequisite_training_started"
	TargetFailed                      TargetOutcome = "failed"
	TargetAmbiguous                   TargetOutcome = "ambiguous"
	TargetAlreadySet                  TargetOutcome = "already_set"
	TargetConfirmationObserved        TargetOutcome = "confirmation_observed"
	TargetLocked                      TargetOutcome = "locked"
	TargetNotFound                    TargetOutcome = "not_found"
)

// TargetResult contains a selected fact, not the task-chain completion state.
// Reported training and confirmation observations do not establish achievement.
type TargetResult struct {
	ID      string          `json:"id"`
	Outcome TargetOutcome   `json:"outcome"`
	Code    string          `json:"code,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

type TypedResult struct {
	SchemaVersion int             `json:"schema_version"`
	Adapter       AdapterRef      `json:"adapter"`
	Policy        ObjectivePolicy `json:"objective_policy"`
	State         State           `json:"state"`
	Code          string          `json:"code,omitempty"`
	Targets       []TargetResult  `json:"targets"`
	Summary       json.RawMessage `json:"summary,omitempty"`
}

func cloneTypedResult(in *TypedResult) *TypedResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Targets = append([]TargetResult(nil), in.Targets...)
	for i := range out.Targets {
		out.Targets[i].Details = bytes.Clone(out.Targets[i].Details)
	}
	out.Summary = bytes.Clone(in.Summary)
	return &out
}

func typedState(in *TypedResult) (State, string) {
	successes, failures := 0, 0
	for _, target := range in.Targets {
		switch target.Outcome {
		case TargetFailed, TargetLocked, TargetNotFound:
			failures++
		case TargetAchieved, TargetAlreadySet:
			successes++
		}
	}
	switch in.Policy {
	case ObjectiveAll:
		if failures > 0 {
			return Failed, "typed_failed"
		}
		if len(in.Targets) > 0 && successes == len(in.Targets) {
			return Succeeded, ""
		}
	case ObjectiveAny:
		if successes > 0 {
			return Succeeded, ""
		}
		if len(in.Targets) > 0 && failures == len(in.Targets) {
			return Failed, "typed_failed"
		}
	}
	return Unknown, "typed_incomplete"
}

func validOutcome(outcome TargetOutcome) bool {
	switch outcome {
	case TargetUnobserved, TargetAchieved, TargetReportedTrainingStarted,
		TargetReportedPrerequisiteStarted, TargetFailed, TargetAmbiguous,
		TargetAlreadySet, TargetConfirmationObserved, TargetLocked, TargetNotFound:
		return true
	default:
		return false
	}
}

func typedError(message string) error {
	return fmt.Errorf("%w: %s", ErrTypedResult, message)
}

func typedBudget(message string) error {
	return fmt.Errorf("%w: %w: %s", ErrTypedResult, ErrTypedBudget, message)
}

func validateTarget(target TargetResult) error {
	if !strictjson.ValidText(target.ID, 128, false) || !validOutcome(target.Outcome) || !strictjson.ValidText(target.Code, 128, true) {
		return typedError("invalid target identity, outcome or code")
	}
	if target.Outcome == TargetUnobserved && (target.Code != "" || len(target.Details) != 0) {
		return typedError("unobserved target contains a fact")
	}
	return validateTypedObject(target.Details, MaxTypedDetailBytes)
}

func validateTypedResult(in *TypedResult) error {
	if in == nil {
		return nil
	}
	if in.SchemaVersion != SchemaVersion || !validAdapterRef(in.Adapter) {
		return typedError("unsupported typed schema or adapter identity")
	}
	if in.Policy != ObjectiveAll && in.Policy != ObjectiveAny {
		return typedError("unsupported typed objective policy")
	}
	if len(in.Targets) == 0 {
		return typedError("typed result requires targets")
	}
	if len(in.Targets) > MaxTypedTargetsPerTask {
		return typedBudget("too many task targets")
	}
	seen := make(map[string]bool, len(in.Targets))
	for _, target := range in.Targets {
		if seen[target.ID] {
			return typedError("duplicate target ID")
		}
		seen[target.ID] = true
		if err := validateTarget(target); err != nil {
			return err
		}
	}
	if err := validateTypedObject(in.Summary, MaxTypedSummaryBytes); err != nil {
		return err
	}
	state, code := typedState(in)
	if in.State != state || in.Code != code {
		return typedError("inconsistent typed aggregate")
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return typedError("cannot encode typed result")
	}
	if len(raw) > MaxTypedTaskBytes {
		return typedBudget("task result exceeds byte limit")
	}
	return nil
}

func validateTypedResults(tasks []TaskResult) error {
	count, size := 0, 0
	for _, task := range tasks {
		if err := validateTypedResult(task.Typed); err != nil {
			return err
		}
		if task.Typed == nil {
			continue
		}
		count += len(task.Typed.Targets)
		raw, _ := json.Marshal(task.Typed) // Validated above.
		size += len(raw)
		if count > MaxTypedTargetsPerRun || size > MaxTypedResultBytes {
			return typedBudget("run result exceeds count or byte limit")
		}
	}
	return nil
}

func validateTypedObject(raw json.RawMessage, limit int) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > limit {
		return typedBudget("typed object exceeds byte limit")
	}
	if !strictjson.ValidObject(raw, uint64(limit)) {
		return typedError("typed detail or summary must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := strictjson.CheckKeys(d); err != nil {
		return typedError("duplicate or malformed typed object field")
	}
	return nil
}

func sameTarget(a, b TargetResult) bool {
	return a.ID == b.ID && a.Outcome == b.Outcome && a.Code == b.Code && bytes.Equal(a.Details, b.Details)
}

func applyTypedUpdate(current *TypedResult, update TypedUpdate) (*TypedResult, bool, error) {
	if current == nil {
		return nil, false, typedError("missing typed result")
	}
	if err := validateTypedResult(current); err != nil {
		return nil, false, err
	}
	if len(update.Targets) > MaxTypedTargetsPerTask {
		return nil, false, typedBudget("too many update targets")
	}
	if err := validateTypedObject(update.Summary, MaxTypedSummaryBytes); err != nil {
		return nil, false, err
	}
	out := cloneTypedResult(current)
	index := make(map[string]int, len(out.Targets))
	for i, target := range out.Targets {
		index[target.ID] = i
	}
	changed := false
	seen := make(map[string]bool, len(update.Targets))
	for _, target := range update.Targets {
		if err := validateTarget(target); err != nil {
			return nil, false, err
		}
		i, ok := index[target.ID]
		if !ok || seen[target.ID] {
			return nil, false, typedError("unknown or duplicate update target")
		}
		seen[target.ID] = true
		previous := out.Targets[i]
		if sameTarget(previous, target) {
			continue
		}
		if previous.Outcome != TargetUnobserved {
			return nil, false, fmt.Errorf("%w: %w", ErrTypedResult, ErrTypedConflict)
		}
		target.Details = bytes.Clone(target.Details)
		out.Targets[i] = target
		changed = true
	}
	if len(update.Summary) != 0 && !bytes.Equal(out.Summary, update.Summary) {
		if len(out.Summary) != 0 {
			return nil, false, fmt.Errorf("%w: %w", ErrTypedResult, ErrTypedConflict)
		}
		out.Summary = bytes.Clone(update.Summary)
		changed = true
	}
	out.State, out.Code = typedState(out)
	if err := validateTypedResult(out); err != nil {
		return nil, false, err
	}
	return out, changed, nil
}
