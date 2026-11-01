package switchtheme

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
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

// New returns the local adapter for the SwitchTheme Core task,
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
func New() control.TaskAdapter { return switchThemeAdapter{} }

type switchThemeAdapter struct{}

func (switchThemeAdapter) Descriptor() control.AdapterRef {
	return control.AdapterRef{Name: switchThemeAdapterName, Version: switchThemeVersion}
}

func (switchThemeAdapter) Prepare(task control.Task) (control.PreparedTask, error) {
	if task.Type != switchThemeTaskType {
		return control.PreparedTask{}, switchThemeReject("adapter requires Core type %q", switchThemeTaskType)
	}
	themes, err := decodeSwitchThemeThemes(task.Params)
	if err != nil {
		return control.PreparedTask{}, err
	}
	params, err := json.Marshal(switchThemeCoreParams{Themes: themes})
	if err != nil {
		return control.PreparedTask{}, switchThemeReject("cannot encode compiled params")
	}
	if len(params) > control.MaxAdapterParamsBytes {
		return control.PreparedTask{}, switchThemeReject("compiled params exceed %d bytes", control.MaxAdapterParamsBytes)
	}
	return control.PreparedTask{
		Params:  params,
		Targets: []string{switchThemeTargetID},
		Reducer: newSwitchThemeReducer(themes),
	}, nil
}

// switchThemeReject wraps every input rejection with ErrInvalidPlan so callers
// classify failures with errors.Is instead of inspecting diagnostics.
func switchThemeReject(format string, args ...any) error {
	return fmt.Errorf("%w: switch_theme: %s", control.ErrInvalidPlan, fmt.Sprintf(format, args...))
}

func decodeSwitchThemeThemes(raw json.RawMessage) ([]string, error) {
	if !strictjson.ValidObject(raw, control.MaxAdapterParamsBytes) {
		return nil, switchThemeReject("params must be a JSON object of at most %d bytes", control.MaxAdapterParamsBytes)
	}
	// Duplicate keys at any depth are rejected before a theme name is read.
	keys := json.NewDecoder(bytes.NewReader(raw))
	keys.UseNumber()
	if err := strictjson.CheckKeys(keys); err != nil {
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
	items, err := strictjson.Array(themesRaw)
	if err != nil {
		return nil, switchThemeReject("themes must be an array")
	}
	if len(items) == 0 || len(items) > switchThemeMaxThemes {
		return nil, switchThemeReject("themes must contain 1-%d entries", switchThemeMaxThemes)
	}
	themes := make([]string, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		name, err := strictjson.String(item)
		if err != nil {
			return nil, switchThemeReject("theme %d must be a string: %v", i, err)
		}
		if !strictjson.ValidText(name, switchThemeMaxThemeBytes, false) {
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
