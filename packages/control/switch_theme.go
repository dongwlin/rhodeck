package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	switchThemeTaskType      = "SwitchTheme"
	switchThemeAdapterName   = "switch_theme"
	switchThemeVersion       = 1
	switchThemeMaxThemes     = 32
	switchThemeMaxThemeBytes = 256

	// A theme task has one business target: the single theme that Core selects
	// from the ordered candidate set. Candidates are not execution targets.
	switchThemeTargetID = "theme-selection"
)

// NewSwitchThemeAdapter returns the local adapter for the SwitchTheme Core task,
// selected explicitly as AdapterRef{Name: "switch_theme", Version: 1}. A raw
// SwitchTheme task without that selection is never routed here.
//
// Prepare accepts only the strict themes object:
//
//	{"themes":["Theme One","Theme Two"]}
//
// themes is required and holds 1-32 exact strings. Each candidate must be valid
// UTF-8, 1-256 bytes, contain no NUL, have no leading or trailing whitespace and
// be non-empty. Names are never trimmed, translated or reordered. Exact
// duplicates, non-string or null elements, unknown top-level fields (including
// enable), duplicate keys at any depth, unpaired UTF-16 surrogate escapes and
// params over 64 KiB are rejected with an error wrapping ErrInvalidPlan.
// Compilation preserves array order.
//
// Multiple candidates mean Core selects exactly one of them uniformly at random
// once. The order is only the candidate set: it is not a retry sequence, a
// progressive switch list or a failure fallback. There is no static theme
// allowlist and no per-candidate target; the sole prepared target is
// theme-selection.
//
// Preparation is deterministic and side-effect free: it creates no directory,
// takes no lock, launches no worker, loads no native code and connects no
// device. It verifies local structure and budgets only; theme names, unlock
// state and device readiness remain unverified.
//
// During a run the reducer consumes only callbacks for this task's chain. A
// completed SwitchThemeByNameSelectTheme node records a selected theme only when
// details.result.text exactly and uniquely matches a candidate; a single
// candidate also requires that evidence. Only after that association may a
// completed SwitchThemeByNameAlreadySet, SwitchThemeByNameLockedTheme or
// SwitchThemeByNameConfirmTheme node become already_set, locked or
// confirmation_observed. A SwitchThemeNotFound extra-info event whose
// details.theme exactly matches a candidate becomes not_found. Skips, start-only
// evidence, missing OCR text and conflicting selections stay ambiguous or
// unobserved. Target details contain only the associated theme name; OCR
// geometry, scores, paths and raw node payloads are never retained. Confirmation
// does not re-read the final theme state, and the adapter never reports that the
// theme actually changed or that a final state was verified. A late task-chain
// success does not replace a lock or not-found fact.
func NewSwitchThemeAdapter() TaskAdapter { return switchThemeAdapter{} }

type switchThemeAdapter struct{}

func (switchThemeAdapter) Descriptor() AdapterRef {
	return AdapterRef{Name: switchThemeAdapterName, Version: switchThemeVersion}
}

func (switchThemeAdapter) Prepare(task Task) (PreparedTask, error) {
	if task.Type != switchThemeTaskType {
		return PreparedTask{}, switchThemeReject("adapter requires Core type %q", switchThemeTaskType)
	}
	themes, err := decodeSwitchThemeThemes(task.Params)
	if err != nil {
		return PreparedTask{}, err
	}
	params, err := json.Marshal(switchThemeCoreParams{Themes: themes})
	if err != nil {
		return PreparedTask{}, switchThemeReject("cannot encode compiled params")
	}
	if len(params) > MaxAdapterParamsBytes {
		return PreparedTask{}, switchThemeReject("compiled params exceed %d bytes", MaxAdapterParamsBytes)
	}
	return PreparedTask{
		Params:  params,
		Targets: []string{switchThemeTargetID},
		Reducer: newSwitchThemeReducer(themes),
	}, nil
}

// switchThemeReject wraps every input rejection with ErrInvalidPlan so callers
// classify failures with errors.Is instead of inspecting diagnostics.
func switchThemeReject(format string, args ...any) error {
	return fmt.Errorf("%w: switch_theme: %s", ErrInvalidPlan, fmt.Sprintf(format, args...))
}

func decodeSwitchThemeThemes(raw json.RawMessage) ([]string, error) {
	if !validObject(raw, MaxAdapterParamsBytes) {
		return nil, switchThemeReject("params must be a JSON object of at most %d bytes", MaxAdapterParamsBytes)
	}
	// Duplicate keys at any depth are rejected before a theme name is read.
	keys := json.NewDecoder(bytes.NewReader(raw))
	keys.UseNumber()
	if err := checkTypedKeys(keys); err != nil {
		return nil, switchThemeReject("params contain duplicate or malformed fields")
	}
	// RawMessage values keep strings exact; map keys are exact too, so case
	// variants such as THEMES or Themes stay distinct and are rejected below.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, switchThemeReject("params must be a JSON object")
	}
	for key := range fields {
		if key != "themes" {
			return nil, switchThemeReject("unknown params field %q", key)
		}
	}
	themesRaw, ok := fields["themes"]
	if !ok {
		return nil, switchThemeReject("themes is required")
	}
	// Reuse the package's strict JSON array and string decoders: null and other
	// non-array shapes are rejected, numbers never round-trip through float64 and
	// unpaired surrogate escapes cannot rename a candidate.
	items, err := operProgressJSONArray(themesRaw)
	if err != nil {
		return nil, switchThemeReject("themes must be an array")
	}
	if len(items) == 0 || len(items) > switchThemeMaxThemes {
		return nil, switchThemeReject("themes must contain 1-%d entries", switchThemeMaxThemes)
	}
	themes := make([]string, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		name, err := operProgressDecodeString(item)
		if err != nil {
			return nil, switchThemeReject("theme %d must be a string: %v", i, err)
		}
		if !validText(name, switchThemeMaxThemeBytes, false) {
			return nil, switchThemeReject("theme %d must be valid UTF-8 without NUL and 1-%d bytes", i, switchThemeMaxThemeBytes)
		}
		if strings.TrimSpace(name) != name {
			return nil, switchThemeReject("theme %d must not have leading or trailing whitespace", i)
		}
		if seen[name] {
			return nil, switchThemeReject("duplicate theme %q", name)
		}
		seen[name] = true
		themes[i] = name
	}
	return themes, nil
}

type switchThemeCoreParams struct {
	Themes []string `json:"themes"`
}

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
func newSwitchThemeReducer(themes []string) TaskReducer {
	return &switchThemeReducer{themes: append([]string(nil), themes...)}
}

// Observe interprets one Core callback. Unrelated message kinds, foreign task
// chains, unknown `what` values, unknown process nodes and unknown future fields
// are ignored so the target stays unobserved instead of gaining a fabricated
// fact. Only an unusable callback object (malformed JSON or duplicate keys at
// any depth) returns an error, which the runtime maps to typed_invalid_event
// without admitting the update.
func (r *switchThemeReducer) Observe(o Observation) (TypedUpdate, error) {
	if o.MessageID != switchThemeMessageSubTaskCompleted && o.MessageID != switchThemeMessageSubTaskExtraInfo {
		return TypedUpdate{}, nil
	}
	if len(o.JSON) == 0 {
		return TypedUpdate{}, nil
	}
	trimmed := bytes.TrimSpace(o.JSON)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return TypedUpdate{}, typedError("switch_theme: callback must be a JSON object")
	}
	// Duplicate keys would make every consumed field ambiguous. The runtime
	// rejects duplicate top-level fields; nested detail objects are checked here
	// before any field is interpreted.
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := checkTypedKeys(dec); err != nil {
		return TypedUpdate{}, typedError("switch_theme: ambiguous or malformed callback fields")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &top); err != nil {
		return TypedUpdate{}, typedError("switch_theme: callback must be a JSON object")
	}
	if chain, ok := switchThemeFieldString(top, "taskchain"); !ok || chain != switchThemeTaskType {
		return TypedUpdate{}, nil
	}
	if o.MessageID == switchThemeMessageSubTaskCompleted {
		return r.observeCompleted(top)
	}
	return r.observeExtra(top), nil
}

// switchThemeFieldString decodes one exact JSON string literal with the
// package's strict decoder, which rejects unpaired UTF-16 surrogate escapes
// instead of silently renaming the caller's theme. A missing, null, non-string
// or malformed field reports false.
func switchThemeFieldString(fields map[string]json.RawMessage, key string) (string, bool) {
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

// switchThemeObjectField returns one decoded JSON object field. Missing, null
// or non-object values cannot carry a business fact and report false.
func switchThemeObjectField(fields map[string]json.RawMessage, key string) (map[string]json.RawMessage, bool) {
	raw, ok := fields[key]
	if !ok {
		return nil, false
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, false
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return nil, false
	}
	return out, true
}

func (r *switchThemeReducer) observeCompleted(top map[string]json.RawMessage) (TypedUpdate, error) {
	details, ok := switchThemeObjectField(top, "details")
	if !ok {
		return TypedUpdate{}, nil
	}
	node, ok := switchThemeFieldString(details, "task")
	if !ok {
		return TypedUpdate{}, nil
	}
	switch node {
	case switchThemeNodeSelect:
		return r.observeSelection(details), nil
	case switchThemeNodeAlreadySet:
		return r.observeBranch(TargetAlreadySet, switchThemeCodeAlreadySet)
	case switchThemeNodeLocked:
		return r.observeBranch(TargetLocked, switchThemeCodeLocked)
	case switchThemeNodeConfirm:
		return r.observeBranch(TargetConfirmationObserved, switchThemeCodeConfirmationObserved)
	default:
		// Other process nodes (navigation, dragging, cancel, return) carry no
		// theme-selection evidence.
		return TypedUpdate{}, nil
	}
}

// observeSelection records the selected theme only after an exact, unique OCR
// match. Selection alone is not a business fact, so it emits no update. A
// different second selection is a conflict: the first selection is preserved
// and no branch may be attributed through the ambiguous choice.
func (r *switchThemeReducer) observeSelection(details map[string]json.RawMessage) TypedUpdate {
	if r.factSet {
		return TypedUpdate{}
	}
	result, ok := switchThemeObjectField(details, "result")
	if !ok {
		return r.selectionFailure(switchThemeCodeSelectionMissing)
	}
	text, ok := switchThemeFieldString(result, "text")
	if !ok {
		return r.selectionFailure(switchThemeCodeSelectionMissing)
	}
	theme, ok := r.matchCandidate(text)
	if !ok {
		return r.selectionFailure(switchThemeCodeSelectionUnmatched)
	}
	if r.selectedSet {
		if r.selected == theme {
			return TypedUpdate{}
		}
		return r.selectionFailure(switchThemeCodeSelectionConflict)
	}
	r.selected, r.selectedSet = theme, true
	return TypedUpdate{}
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
func (r *switchThemeReducer) observeBranch(outcome TargetOutcome, code string) (TypedUpdate, error) {
	if !r.selectedSet || r.selectionAmbiguous {
		return r.incompleteUpdate(switchThemeCodeSelectionMissing), nil
	}
	details, err := json.Marshal(switchThemeTargetDetails{Theme: r.selected})
	if err != nil {
		return TypedUpdate{}, typedError("switch_theme: cannot encode target fact")
	}
	return r.factUpdate(outcome, code, details), nil
}

// observeExtra interprets SwitchThemeNotFound and SwitchThemeSkipped extra-info
// events. Unknown future `what` values are ignored.
func (r *switchThemeReducer) observeExtra(top map[string]json.RawMessage) TypedUpdate {
	what, ok := switchThemeFieldString(top, "what")
	if !ok {
		return TypedUpdate{}
	}
	switch what {
	case switchThemeWhatNotFound:
		return r.observeNotFound(top)
	case switchThemeWhatSkipped:
		// A skip means Core never attempted a selection. It is not success and
		// not evidence for any candidate.
		return r.incompleteUpdate(switchThemeCodeSkipped)
	default:
		return TypedUpdate{}
	}
}

// observeNotFound accepts the event's own theme value as independent
// association, but only when it exactly matches a candidate. A conflicting
// selection is never overwritten.
func (r *switchThemeReducer) observeNotFound(top map[string]json.RawMessage) TypedUpdate {
	details, ok := switchThemeObjectField(top, "details")
	if !ok {
		return r.incompleteUpdate(switchThemeCodeNotFoundUnassociated)
	}
	theme, ok := switchThemeFieldString(details, "theme")
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
	return r.factUpdate(TargetNotFound, switchThemeCodeNotFound, raw)
}

// selectionFailure records that a selection observation could not identify one
// theme and marks branch attribution unavailable.
func (r *switchThemeReducer) selectionFailure(code string) TypedUpdate {
	r.selectionAmbiguous = true
	return r.incompleteUpdate(code)
}

// incompleteUpdate marks the target association-ambiguous once. It is a no-op
// if the immutable target fact is already established.
func (r *switchThemeReducer) incompleteUpdate(code string) TypedUpdate {
	if r.factSet {
		return TypedUpdate{}
	}
	return r.factUpdate(TargetAmbiguous, code, nil)
}

// factUpdate records the target fact and returns it. A later different fact is
// still returned so the runtime can report typed_conflict and preserve the
// original; the reducer never rewrites its own selection.
func (r *switchThemeReducer) factUpdate(outcome TargetOutcome, code string, details json.RawMessage) TypedUpdate {
	r.factSet = true
	return TypedUpdate{Targets: []TargetResult{{
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
