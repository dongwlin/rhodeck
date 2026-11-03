package switchtheme

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
)

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

type switchThemeReducer struct {
	themes             []string
	selected           string
	selectedSet        bool
	selectionAmbiguous bool
	factSet            bool
}

func newSwitchThemeReducer(themes []string) control.TaskReducer {
	return &switchThemeReducer{themes: append([]string(nil), themes...)}
}

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
		return control.TypedUpdate{}, nil
	}
}

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

func (r *switchThemeReducer) observeExtra(top map[string]json.RawMessage) control.TypedUpdate {
	what, ok := strictjson.FieldString(top, "what")
	if !ok {
		return control.TypedUpdate{}
	}
	switch what {
	case switchThemeWhatNotFound:
		return r.observeNotFound(top)
	case switchThemeWhatSkipped:
		return r.incompleteUpdate(switchThemeCodeSkipped)
	default:
		return control.TypedUpdate{}
	}
}

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

func (r *switchThemeReducer) selectionFailure(code string) control.TypedUpdate {
	r.selectionAmbiguous = true
	return r.incompleteUpdate(code)
}

func (r *switchThemeReducer) incompleteUpdate(code string) control.TypedUpdate {
	if r.factSet {
		return control.TypedUpdate{}
	}
	return r.factUpdate(control.TargetAmbiguous, code, nil)
}

func (r *switchThemeReducer) factUpdate(outcome control.TargetOutcome, code string, details json.RawMessage) control.TypedUpdate {
	r.factSet = true
	return control.TypedUpdate{Targets: []control.TargetResult{{
		ID:      switchThemeTargetID,
		Outcome: outcome,
		Code:    code,
		Details: details,
	}}}
}

type switchThemeTargetDetails struct {
	Theme string `json:"theme"`
}
