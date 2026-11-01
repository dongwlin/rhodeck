package switchtheme

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
)

// Core emits one SubTaskCompleted (20002) per executed process node. The theme
// nodes that matter are the selection OCR result and the three mutually
// exclusive branches after it. Core emits SwitchThemeNotFound and
// SwitchThemeSkipped as SubTaskExtraInfo (20003).
const (
	switchThemeMessageSubTaskCompleted int32 = 20002
	switchThemeMessageSubTaskExtraInfo int32 = 20003

	switchThemeNodeSelect     = "SwitchThemeByNameSelectTheme"
	switchThemeNodeAlreadySet = "SwitchThemeByNameAlreadySet"
	switchThemeNodeLocked     = "SwitchThemeByNameLockedTheme"
	switchThemeNodeConfirm    = "SwitchThemeByNameConfirmTheme"

	switchThemeWhatNotFound = "SwitchThemeNotFound"
	switchThemeWhatSkipped  = "SwitchThemeSkipped"
)

// Stable lowercase codes describe the observed fact. The outcome value carries
// the aggregate meaning; details carry only the associated theme name.
const (
	switchThemeCodeAlreadySet           = "already_set"
	switchThemeCodeLocked               = "locked"
	switchThemeCodeConfirmationObserved = "confirmation_observed"
	switchThemeCodeNotFound             = "not_found"
	switchThemeCodeSelectionMissing     = "selection_missing"
	switchThemeCodeSelectionUnmatched   = "selection_unmatched"
	switchThemeCodeSelectionConflict    = "selection_conflict"
	switchThemeCodeNotFoundUnassociated = "not_found_unassociated"
	switchThemeCodeSkipped              = "skipped"
)

// switchThemeReducer interprets callbacks for one prepared theme task. The
// runtime observes it serially on its single monitor goroutine, so it needs no
// internal locking. Scratch state is bounded by the candidate count and never
// retains Observation.JSON.
type switchThemeReducer struct {
	themes []string
	// selected is the theme proven by an exact SelectTheme OCR match or by a
	// NotFound event that names a candidate. A second, different selection is
	// never allowed to replace it.
	selected string
	// selectedSet records that selected is trustworthy.
	selectedSet bool
	// selectionAmbiguous records that the evidence named no unique candidate or
	// conflicted. Branch nodes then cannot be attributed to a theme.
	selectionAmbiguous bool
	// factSet records that the single target already received a fact. Facts are
	// immutable through the runtime; this only bounds further selection edits.
	factSet bool
}

// newSwitchThemeReducer binds a fresh reducer to a detached copy of the ordered
// candidate set prepared for exactly one task.
func newSwitchThemeReducer(themes []string) control.TaskReducer {
	return &switchThemeReducer{themes: append([]string(nil), themes...)}
}

// Observe interprets one Core callback. Unrelated message kinds, foreign task
// chains, unknown `what` values, unknown process nodes and unknown future fields
// are ignored so the target stays unobserved instead of gaining a fabricated
// fact. Only an unusable callback object (malformed JSON or duplicate keys at
// any depth) returns an error, which the runtime maps to typed_invalid_event
// without admitting the update.
func (r *switchThemeReducer) Observe(o control.Observation) (control.TypedUpdate, error) {
	if o.MessageID != switchThemeMessageSubTaskCompleted && o.MessageID != switchThemeMessageSubTaskExtraInfo {
		return control.TypedUpdate{}, nil
	}
	if len(o.JSON) == 0 {
		return control.TypedUpdate{}, nil
	}
	trimmed := bytes.TrimSpace(o.JSON)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "switch_theme: callback must be a JSON object")
	}
	// Duplicate keys would make every consumed field ambiguous. The runtime
	// rejects duplicate top-level fields; nested detail objects are checked here
	// before any field is interpreted.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := strictjson.CheckKeys(dec); err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "switch_theme: ambiguous or malformed callback fields")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "switch_theme: callback must be a JSON object")
	}
	if chain, ok := strictjson.FieldString(top, "taskchain"); !ok || chain != switchThemeTaskType {
		return control.TypedUpdate{}, nil
	}
	if o.MessageID == switchThemeMessageSubTaskCompleted {
		return r.observeCompleted(top)
	}
	return r.observeExtra(top), nil
}

func (r *switchThemeReducer) observeCompleted(top map[string]json.RawMessage) (control.TypedUpdate, error) {
	details, ok := strictjson.ObjectField(top, "details")
	if !ok {
		return control.TypedUpdate{}, nil
	}
	node, ok := strictjson.FieldString(details, "task")
	if !ok {
		return control.TypedUpdate{}, nil
	}
	switch node {
	case switchThemeNodeSelect:
		return r.observeSelection(details), nil
	case switchThemeNodeAlreadySet:
		return r.observeBranch(control.TargetAlreadySet, switchThemeCodeAlreadySet)
	case switchThemeNodeLocked:
		return r.observeBranch(control.TargetLocked, switchThemeCodeLocked)
	case switchThemeNodeConfirm:
		return r.observeBranch(control.TargetConfirmationObserved, switchThemeCodeConfirmationObserved)
	default:
		// Other process nodes (navigation, dragging, cancel, return) carry no
		// theme-selection evidence.
		return control.TypedUpdate{}, nil
	}
}

// observeSelection records the selected theme only after an exact, unique OCR
// match. Selection alone is not a business fact, so it emits no update. A
// different second selection is a conflict: the first selection is preserved
// and no branch may be attributed through the ambiguous choice.
func (r *switchThemeReducer) observeSelection(details map[string]json.RawMessage) control.TypedUpdate {
	if r.factSet {
		return control.TypedUpdate{}
	}
	result, ok := strictjson.ObjectField(details, "result")
	if !ok {
		return r.selectionFailure(switchThemeCodeSelectionMissing)
	}
	text, ok := strictjson.FieldString(result, "text")
	if !ok {
		return r.selectionFailure(switchThemeCodeSelectionMissing)
	}
	theme, ok := r.matchCandidate(text)
	if !ok {
		return r.selectionFailure(switchThemeCodeSelectionUnmatched)
	}
	if r.selectedSet {
		if r.selected == theme {
			return control.TypedUpdate{}
		}
		return r.selectionFailure(switchThemeCodeSelectionConflict)
	}
	r.selected, r.selectedSet = theme, true
	return control.TypedUpdate{}
}

// matchCandidate requires exactly one exact candidate for text. Strict
// preparation rejects duplicate candidates, but counting keeps the uniqueness
// rule explicit instead of relying on input order.
func (r *switchThemeReducer) matchCandidate(text string) (string, bool) {
	match := ""
	count := 0
	for _, theme := range r.themes {
		if theme == text {
			match = theme
			count++
		}
	}
	return match, count == 1
}

// observeBranch maps an AlreadySet, LockedTheme or ConfirmTheme node to a fact
// for the proven selection. Without a proven selection the branch cannot name a
// theme, so no outcome is guessed from the candidate order.
func (r *switchThemeReducer) observeBranch(outcome control.TargetOutcome, code string) (control.TypedUpdate, error) {
	if !r.selectedSet || r.selectionAmbiguous {
		return r.incompleteUpdate(switchThemeCodeSelectionMissing), nil
	}
	details, err := json.Marshal(switchThemeTargetDetails{Theme: r.selected})
	if err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "switch_theme: cannot encode target fact")
	}
	return r.factUpdate(outcome, code, details), nil
}

// observeExtra interprets SwitchThemeNotFound and SwitchThemeSkipped extra-info
// events. Unknown future `what` values are ignored.
func (r *switchThemeReducer) observeExtra(top map[string]json.RawMessage) control.TypedUpdate {
	what, ok := strictjson.FieldString(top, "what")
	if !ok {
		return control.TypedUpdate{}
	}
	switch what {
	case switchThemeWhatNotFound:
		return r.observeNotFound(top)
	case switchThemeWhatSkipped:
		// A skip means Core never attempted a selection. It is not success and
		// not evidence for any candidate.
		return r.incompleteUpdate(switchThemeCodeSkipped)
	default:
		return control.TypedUpdate{}
	}
}

// observeNotFound accepts the event's own theme value as independent
// association, but only when it exactly matches a candidate. A conflicting
// selection is never overwritten.
func (r *switchThemeReducer) observeNotFound(top map[string]json.RawMessage) control.TypedUpdate {
	details, ok := strictjson.ObjectField(top, "details")
	if !ok {
		return r.incompleteUpdate(switchThemeCodeNotFoundUnassociated)
	}
	theme, ok := strictjson.FieldString(details, "theme")
	if !ok {
		return r.incompleteUpdate(switchThemeCodeNotFoundUnassociated)
	}
	matched, ok := r.matchCandidate(theme)
	if !ok {
		return r.incompleteUpdate(switchThemeCodeNotFoundUnassociated)
	}
	if r.selectedSet {
		if r.selected != matched {
			return r.selectionFailure(switchThemeCodeSelectionConflict)
		}
	} else {
		r.selected, r.selectedSet = matched, true
	}
	raw, err := json.Marshal(switchThemeTargetDetails{Theme: matched})
	if err != nil {
		return r.incompleteUpdate(switchThemeCodeNotFoundUnassociated)
	}
	return r.factUpdate(control.TargetNotFound, switchThemeCodeNotFound, raw)
}

// selectionFailure records that a selection observation could not identify one
// theme and marks branch attribution unavailable.
func (r *switchThemeReducer) selectionFailure(code string) control.TypedUpdate {
	r.selectionAmbiguous = true
	return r.incompleteUpdate(code)
}

// incompleteUpdate marks the target association-ambiguous once. It is a no-op
// if the immutable target fact is already established.
func (r *switchThemeReducer) incompleteUpdate(code string) control.TypedUpdate {
	if r.factSet {
		return control.TypedUpdate{}
	}
	return r.factUpdate(control.TargetAmbiguous, code, nil)
}

// factUpdate records the target fact and returns it. A later different fact is
// still returned so the runtime can report typed_conflict and preserve the
// original; the reducer never rewrites its own selection.
func (r *switchThemeReducer) factUpdate(outcome control.TargetOutcome, code string, details json.RawMessage) control.TypedUpdate {
	r.factSet = true
	return control.TypedUpdate{Targets: []control.TargetResult{{
		ID:      switchThemeTargetID,
		Outcome: outcome,
		Code:    code,
		Details: details,
	}}}
}

// switchThemeTargetDetails is the only business detail retained for the target:
// the associated theme name. OCR geometry, score, paths, branch text and the raw
// callback are never copied.
type switchThemeTargetDetails struct {
	Theme string `json:"theme"`
}
