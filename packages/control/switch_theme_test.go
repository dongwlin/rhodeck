package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func switchThemeTaskWithThemes(t *testing.T, themes ...string) Task {
	t.Helper()
	raw, err := json.Marshal(struct {
		Themes []string `json:"themes"`
	}{Themes: themes})
	if err != nil {
		t.Fatal(err)
	}
	return Task{ID: "theme", Type: switchThemeTaskType, Params: raw}
}

func switchThemeThemesJSON(t *testing.T, themes ...string) string {
	t.Helper()
	return string(switchThemeTaskWithThemes(t, themes...).Params)
}

func switchThemeNumberedThemes(n int) []string {
	themes := make([]string, n)
	for i := range themes {
		themes[i] = fmt.Sprintf("theme-%02d", i)
	}
	return themes
}

func switchThemeTestReducer(t *testing.T, themes ...string) *switchThemeReducer {
	t.Helper()
	prepared, err := NewSwitchThemeAdapter().Prepare(switchThemeTaskWithThemes(t, themes...))
	if err != nil {
		t.Fatal(err)
	}
	reducer, ok := prepared.Reducer.(*switchThemeReducer)
	if !ok {
		t.Fatalf("reducer type = %T, want a fresh *switchThemeReducer", prepared.Reducer)
	}
	return reducer
}

func switchThemeObserve(t *testing.T, reducer TaskReducer, messageID int32, raw string) TypedUpdate {
	t.Helper()
	update, err := reducer.Observe(Observation{MessageID: messageID, JSON: json.RawMessage(raw)})
	if err != nil {
		t.Fatalf("unexpected reducer error: %v", err)
	}
	return update
}

func switchThemeSelectCallback(t *testing.T, taskID int32, theme string) string {
	t.Helper()
	encoded, err := json.Marshal(theme)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"ProcessTask","details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":%s,"rect":[1,2,3,4],"score":0.98}}}`, taskID, encoded)
}

func switchThemeBranchCallback(t *testing.T, taskID int32, node string) string {
	t.Helper()
	encoded, err := json.Marshal(node)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"ProcessTask","details":{"task":%s,"result":{"text":"branch","rect":[5,6,7,8],"score":0.5}}}`, taskID, encoded)
}

func switchThemeNotFoundCallback(t *testing.T, taskID int32, theme string) string {
	t.Helper()
	encoded, err := json.Marshal(theme)
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"SwitchThemeTask","what":"SwitchThemeNotFound","details":{"theme":%s}}`, taskID, encoded)
}

func switchThemeSkippedCallback(taskID int32) string {
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"SwitchThemeTask","what":"SwitchThemeSkipped","details":{}}`, taskID)
}

func wantNoSwitchThemeUpdate(t *testing.T, update TypedUpdate) {
	t.Helper()
	if len(update.Targets) != 0 || len(update.Summary) != 0 {
		t.Fatalf("unexpected typed update: %+v", update)
	}
}

func wantSwitchThemeFact(t *testing.T, update TypedUpdate, outcome TargetOutcome, code, theme string) {
	t.Helper()
	if len(update.Summary) != 0 {
		t.Fatalf("unexpected summary: %s", update.Summary)
	}
	if len(update.Targets) != 1 {
		t.Fatalf("targets = %+v, want exactly one", update.Targets)
	}
	got := update.Targets[0]
	if got.ID != switchThemeTargetID || got.Outcome != outcome || got.Code != code {
		t.Fatalf("target = %+v, want id=%q outcome=%q code=%q", got, switchThemeTargetID, outcome, code)
	}
	var wantDetails json.RawMessage
	if theme != "" {
		raw, err := json.Marshal(switchThemeTargetDetails{Theme: theme})
		if err != nil {
			t.Fatal(err)
		}
		wantDetails = raw
	}
	if !bytes.Equal(got.Details, wantDetails) {
		t.Fatalf("details = %s, want %s", got.Details, wantDetails)
	}
}

func TestSwitchThemeAdapterDescriptor(t *testing.T) {
	adapter := NewSwitchThemeAdapter()
	want := AdapterRef{Name: "switch_theme", Version: 1}
	if got := adapter.Descriptor(); got != want {
		t.Fatalf("descriptor = %+v, want %+v", got, want)
	}
}

func TestSwitchThemePrepareCompilesOrderedCandidates(t *testing.T) {
	adapter := NewSwitchThemeAdapter()
	for _, tc := range []struct {
		name   string
		themes []string
	}{
		{"single candidate", []string{"Theme One"}},
		{"multiple candidates", []string{"Theme One", "Theme Two", "Theme Three"}},
		{"single unicode candidate", []string{"主题甲"}},
		{"maximum candidate count", switchThemeNumberedThemes(switchThemeMaxThemes)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			params := switchThemeThemesJSON(t, tc.themes...)
			prepared, err := adapter.Prepare(Task{ID: "theme-1", Type: switchThemeTaskType, Params: json.RawMessage(params)})
			if err != nil {
				t.Fatal(err)
			}
			if string(prepared.Params) != params {
				t.Fatalf("compiled params = %s, want ordered candidate array %s", prepared.Params, params)
			}
			// Multiple candidates still yield exactly one target. Preparation does
			// not model a sequential attempt per candidate.
			if !reflect.DeepEqual(prepared.Targets, []string{switchThemeTargetID}) {
				t.Fatalf("targets = %v, want [%s]", prepared.Targets, switchThemeTargetID)
			}
			reducer, ok := prepared.Reducer.(*switchThemeReducer)
			if !ok || reducer == nil {
				t.Fatalf("reducer has type %T, want fresh *switchThemeReducer", prepared.Reducer)
			}
			if !reflect.DeepEqual(reducer.themes, tc.themes) {
				t.Fatalf("reducer candidates = %v, want %v", reducer.themes, tc.themes)
			}
		})
	}
}

func TestSwitchThemePrepareRejectsInvalidInput(t *testing.T) {
	adapter := NewSwitchThemeAdapter()
	valid := switchThemeThemesJSON(t, "Theme One")
	cases := []struct {
		name   string
		params string
	}{
		{"params null", `null`},
		{"params array", `[]`},
		{"params scalar", `"themes"`},
		{"params empty object", `{}`},
		{"themes null", `{"themes":null}`},
		{"themes object", `{"themes":{}}`},
		{"themes string", `{"themes":"Theme One"}`},
		{"themes empty", `{"themes":[]}`},
		{"themes too many", switchThemeThemesJSON(t, switchThemeNumberedThemes(switchThemeMaxThemes+1)...)},
		{"unknown top-level field", `{"themes":["Theme One"],"enable":true}`},
		{"unknown future field", `{"themes":["Theme One"],"selected":"Theme One"}`},
		{"duplicate top-level key", `{"themes":["Theme One"],"themes":["Theme Two"]}`},
		{"duplicate nested key", `{"themes":["Theme One"],"future":{"x":1,"x":2}}`},
		{"trailing JSON value", valid + ` {}`},
		{"invalid UTF-8", valid + string([]byte{0xff})},
		{"theme null", `{"themes":[null]}`},
		{"theme number", `{"themes":[1]}`},
		{"theme bool", `{"themes":[true]}`},
		{"theme array", `{"themes":[[]]}`},
		{"theme object", `{"themes":[{}]}`},
		{"theme empty", `{"themes":[""]}`},
		{"theme spaces", `{"themes":["   "]}`},
		{"theme tab", `{"themes":["\t"]}`},
		{"theme unicode space", `{"themes":["\u3000"]}`},
		{"theme leading space", `{"themes":[" Theme One"]}`},
		{"theme trailing space", `{"themes":["Theme One "]}`},
		{"theme NUL", `{"themes":["Theme\u0000One"]}`},
		{"theme over 256 bytes", `{"themes":["` + strings.Repeat("a", switchThemeMaxThemeBytes+1) + `"]}`},
		{"duplicate theme", `{"themes":["Theme One","Theme One"]}`},
		{"duplicate escaped theme", `{"themes":["A","\u0041"]}`},
		{"unpaired high surrogate", `{"themes":["\ud800"]}`},
		{"unpaired low surrogate", `{"themes":["\udc00"]}`},
		{"uppercase themes", `{"THEMES":["Theme One"]}`},
		{"title-case themes", `{"Themes":["Theme One"]}`},
		{"themes and title-case themes", `{"themes":["Theme One"],"Themes":["Theme Two"]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := adapter.Prepare(Task{ID: "theme", Type: switchThemeTaskType, Params: json.RawMessage(tc.params)}); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("invalid input accepted: %v", err)
			}
		})
	}
}

func TestSwitchThemePrepareAcceptsCandidateBoundaries(t *testing.T) {
	adapter := NewSwitchThemeAdapter()
	longName := strings.Repeat("a", switchThemeMaxThemeBytes)
	themes := switchThemeNumberedThemes(switchThemeMaxThemes)
	cases := []struct {
		name   string
		params string
		check  func(t *testing.T, prepared PreparedTask)
	}{
		{"minimum count", switchThemeThemesJSON(t, "Theme One"), nil},
		{"maximum count", switchThemeThemesJSON(t, themes...), func(t *testing.T, prepared PreparedTask) {
			if len(prepared.Targets) != 1 {
				t.Fatalf("targets = %v", prepared.Targets)
			}
		}},
		{"maximum name bytes", `{"themes":["` + longName + `"]}`, nil},
		{"paired surrogate", `{"themes":["\ud83d\ude00"]}`, func(t *testing.T, prepared PreparedTask) {
			if string(prepared.Params) != `{"themes":["😀"]}` {
				t.Fatalf("compiled params = %s", prepared.Params)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := adapter.Prepare(Task{ID: "theme", Type: switchThemeTaskType, Params: json.RawMessage(tc.params)})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Reducer == nil {
				t.Fatal("adapter returned a nil reducer")
			}
			if tc.check != nil {
				tc.check(t, prepared)
			}
		})
	}
}

func TestSwitchThemePrepareParamsByteBoundary(t *testing.T) {
	adapter := NewSwitchThemeAdapter()
	base := switchThemeThemesJSON(t, "Theme One")
	for _, size := range []int{MaxAdapterParamsBytes, MaxAdapterParamsBytes + 1} {
		params := base + strings.Repeat(" ", size-len(base))
		_, err := adapter.Prepare(Task{ID: "theme", Type: switchThemeTaskType, Params: json.RawMessage(params)})
		if size == MaxAdapterParamsBytes && err != nil {
			t.Fatalf("params at the byte limit rejected: %v", err)
		}
		if size > MaxAdapterParamsBytes && !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("params over the byte limit accepted: %v", err)
		}
	}
}

func TestSwitchThemePrepareRejectsWrongCoreType(t *testing.T) {
	adapter := NewSwitchThemeAdapter()
	params := json.RawMessage(switchThemeThemesJSON(t, "Theme One"))
	for _, taskType := range []string{"OperProgress", "switch_theme", "SwitchTheme ", "switchtheme", ""} {
		if _, err := adapter.Prepare(Task{ID: "theme", Type: taskType, Params: params}); !errors.Is(err, ErrInvalidPlan) {
			t.Fatalf("Core type %q accepted: %v", taskType, err)
		}
	}
}

func TestSwitchThemePrepareIsDetachedAndFreshPerTask(t *testing.T) {
	adapter := NewSwitchThemeAdapter()
	const source = `{"themes":["Alpha","Beta"]}`
	input := []byte(source)
	prepared, err := adapter.Prepare(Task{ID: "theme-1", Type: switchThemeTaskType, Params: json.RawMessage(input)})
	if err != nil {
		t.Fatal(err)
	}
	if string(input) != source {
		t.Fatal("Prepare mutated the caller's params")
	}
	reducer := prepared.Reducer.(*switchThemeReducer)
	input[2] = 'X'
	if string(prepared.Params) != source || prepared.Targets[0] != switchThemeTargetID || reducer.themes[0] != "Alpha" {
		t.Fatal("prepared output aliases the caller's params")
	}

	second, err := adapter.Prepare(switchThemeTaskWithThemes(t, "Alpha", "Beta"))
	if err != nil {
		t.Fatal(err)
	}
	secondReducer := second.Reducer.(*switchThemeReducer)
	if secondReducer == reducer || &secondReducer.themes[0] == &reducer.themes[0] {
		t.Fatal("reducer was reused across preparations")
	}
	prepared.Targets[0] = "mutated"
	prepared.Params[2] = 'Y'
	reducer.themes[0] = "mutated"
	if second.Targets[0] != switchThemeTargetID || string(second.Params) != source || secondReducer.themes[0] != "Alpha" {
		t.Fatal("preparations share mutable slices or JSON buffers")
	}
}

func TestSwitchThemeRawPlanRemainsOpen(t *testing.T) {
	plan := validTestPlan(t)
	ref := AdapterRef{Name: switchThemeAdapterName, Version: switchThemeVersion}
	rawParams := json.RawMessage(`{"themes":["Raw Theme"],"enable":true,"future":9007199254740993}`)
	plan.Tasks = []Task{
		{ID: "raw", Type: switchThemeTaskType, Params: rawParams},
		{ID: "strict", Type: switchThemeTaskType, Adapter: &ref, Params: json.RawMessage(`{"themes":["Strict One","Strict Two"]}`)},
	}
	prepared, err := PreparePlan(plan, NewSwitchThemeAdapter())
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Tasks[0].Typed != nil {
		t.Fatal("raw task was routed through the adapter")
	}
	if !bytes.Contains(prepared.Plan.Tasks[0].Params, []byte(`"enable":true`)) ||
		!bytes.Contains(prepared.Plan.Tasks[0].Params, []byte("9007199254740993")) {
		t.Fatalf("raw params lost unknown fields or integer literals: %s", prepared.Plan.Tasks[0].Params)
	}
	typed := prepared.Tasks[1].Typed
	if typed == nil || len(typed.Targets) != 1 || typed.Targets[0].ID != switchThemeTargetID || typed.Targets[0].Outcome != TargetUnobserved {
		t.Fatalf("strict task was not prepared: %+v", typed)
	}
	if string(prepared.Plan.Tasks[1].Params) != `{"themes":["Strict One","Strict Two"]}` {
		t.Fatalf("strict task kept uncompiled params: %s", prepared.Plan.Tasks[1].Params)
	}
}

func TestSwitchThemeReducerRequiresExactSelection(t *testing.T) {
	t.Run("records the second candidate", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha", "Beta")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta")))
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta")))
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet))
		wantSwitchThemeFact(t, update, TargetAlreadySet, switchThemeCodeAlreadySet, "Beta")
	})
	t.Run("candidate order is not a fallback", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Beta", "Alpha")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
		wantSwitchThemeFact(t, update, TargetLocked, switchThemeCodeLocked, "Alpha")
	})
	t.Run("single candidate still requires selection evidence", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha")
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet))
		wantSwitchThemeFact(t, update, TargetAmbiguous, switchThemeCodeSelectionMissing, "")

		r = switchThemeTestReducer(t, "Alpha")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
		update = switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet))
		wantSwitchThemeFact(t, update, TargetAlreadySet, switchThemeCodeAlreadySet, "Alpha")
	})
	t.Run("non-candidate OCR text", func(t *testing.T) {
		for name, raw := range map[string]string{
			"other name":   switchThemeSelectCallback(t, 101, "Gamma"),
			"prefix":       switchThemeSelectCallback(t, 101, "Alph"),
			"case variant": switchThemeSelectCallback(t, 101, "alpha"),
		} {
			t.Run(name, func(t *testing.T) {
				r := switchThemeTestReducer(t, "Alpha", "Beta")
				update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, raw)
				wantSwitchThemeFact(t, update, TargetAmbiguous, switchThemeCodeSelectionUnmatched, "")
				wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeConfirm)))
			})
		}
	})
	t.Run("missing OCR text", func(t *testing.T) {
		for name, raw := range map[string]string{
			"missing result": `{"taskchain":"SwitchTheme","taskid":101,"details":{"task":"SwitchThemeByNameSelectTheme"}}`,
			"result scalar":  `{"taskchain":"SwitchTheme","taskid":101,"details":{"task":"SwitchThemeByNameSelectTheme","result":"Alpha"}}`,
			"result empty":   `{"taskchain":"SwitchTheme","taskid":101,"details":{"task":"SwitchThemeByNameSelectTheme","result":{}}}`,
			"text number":    `{"taskchain":"SwitchTheme","taskid":101,"details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":7}}}`,
		} {
			t.Run(name, func(t *testing.T) {
				r := switchThemeTestReducer(t, "Alpha")
				update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, raw)
				wantSwitchThemeFact(t, update, TargetAmbiguous, switchThemeCodeSelectionMissing, "")
			})
		}
	})
}

func TestSwitchThemeReducerBranches(t *testing.T) {
	for _, tc := range []struct {
		node    string
		outcome TargetOutcome
		code    string
	}{
		{switchThemeNodeAlreadySet, TargetAlreadySet, switchThemeCodeAlreadySet},
		{switchThemeNodeLocked, TargetLocked, switchThemeCodeLocked},
		{switchThemeNodeConfirm, TargetConfirmationObserved, switchThemeCodeConfirmationObserved},
	} {
		t.Run(tc.node, func(t *testing.T) {
			r := switchThemeTestReducer(t, "Alpha", "Beta")
			wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta")))
			update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, tc.node))
			wantSwitchThemeFact(t, update, tc.outcome, tc.code, "Beta")
			details := update.Targets[0].Details
			for _, forbidden := range []string{"rect", "score", "path", "actually_changed", "final_state_verified", "branch"} {
				if bytes.Contains(details, []byte(forbidden)) {
					t.Fatalf("details exposed %q: %s", forbidden, details)
				}
			}
		})
	}
}

func TestSwitchThemeReducerNotFound(t *testing.T) {
	t.Run("exact candidate match", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha", "Beta")
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskExtraInfo, switchThemeNotFoundCallback(t, 101, "Beta"))
		wantSwitchThemeFact(t, update, TargetNotFound, switchThemeCodeNotFound, "Beta")
	})
	t.Run("unmatched theme", func(t *testing.T) {
		for name, raw := range map[string]string{
			"other name":      switchThemeNotFoundCallback(t, 101, "Gamma"),
			"missing details": `{"taskchain":"SwitchTheme","taskid":101,"what":"SwitchThemeNotFound"}`,
			"missing theme":   `{"taskchain":"SwitchTheme","taskid":101,"what":"SwitchThemeNotFound","details":{}}`,
			"theme number":    `{"taskchain":"SwitchTheme","taskid":101,"what":"SwitchThemeNotFound","details":{"theme":3}}`,
		} {
			t.Run(name, func(t *testing.T) {
				r := switchThemeTestReducer(t, "Alpha")
				update := switchThemeObserve(t, r, switchThemeMessageSubTaskExtraInfo, raw)
				wantSwitchThemeFact(t, update, TargetAmbiguous, switchThemeCodeNotFoundUnassociated, "")
			})
		}
	})
	t.Run("selection conflict", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha", "Beta")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskExtraInfo, switchThemeNotFoundCallback(t, 101, "Beta"))
		wantSwitchThemeFact(t, update, TargetAmbiguous, switchThemeCodeSelectionConflict, "")
	})
	t.Run("same selected theme", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskExtraInfo, switchThemeNotFoundCallback(t, 101, "Alpha"))
		wantSwitchThemeFact(t, update, TargetNotFound, switchThemeCodeNotFound, "Alpha")
	})
}

func TestSwitchThemeReducerSkippedAndStartOnly(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha")
	// SubTaskStart only reports that an action was about to begin.
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, 20001, switchThemeSelectCallback(t, 101, "Alpha")))
	// A terminal chain callback carries no theme-selection evidence.
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`))
	update := switchThemeObserve(t, r, switchThemeMessageSubTaskExtraInfo, switchThemeSkippedCallback(101))
	wantSwitchThemeFact(t, update, TargetAmbiguous, switchThemeCodeSkipped, "")
}

func TestSwitchThemeReducerIgnoresUnrelatedCallbacks(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha")
	for name, raw := range map[string]string{
		"other chain":       `{"taskchain":"OperProgress","taskid":101,"details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":"Alpha"}}}`,
		"missing chain":     `{"taskid":101,"details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":"Alpha"}}}`,
		"missing details":   `{"taskchain":"SwitchTheme","taskid":101}`,
		"null details":      `{"taskchain":"SwitchTheme","taskid":101,"details":null}`,
		"numeric task node": `{"taskchain":"SwitchTheme","taskid":101,"details":{"task":3}}`,
		"other node":        switchThemeBranchCallback(t, 101, "SwitchThemeByNameBegin"),
		"unknown what":      `{"taskchain":"SwitchTheme","taskid":101,"what":"SwitchThemeFuture","details":{}}`,
		"missing what":      `{"taskchain":"SwitchTheme","taskid":101,"details":{}}`,
		"numeric what":      `{"taskchain":"SwitchTheme","taskid":101,"what":3,"details":{}}`,
	} {
		t.Run(name, func(t *testing.T) {
			wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, raw))
		})
	}
}

func TestSwitchThemeReducerRejectsMalformedCallbacks(t *testing.T) {
	for name, raw := range map[string]string{
		"truncated":        `{`,
		"array":            `[]`,
		"scalar":           `"SwitchTheme"`,
		"top duplicate":    `{"taskchain":"SwitchTheme","taskchain":"Other"}`,
		"nested duplicate": `{"taskchain":"SwitchTheme","details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":"Alpha","text":"Beta"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			r := switchThemeTestReducer(t, "Alpha")
			if _, err := r.Observe(Observation{MessageID: switchThemeMessageSubTaskCompleted, JSON: json.RawMessage(raw)}); !errors.Is(err, ErrTypedResult) {
				t.Fatalf("malformed callback accepted: %v", err)
			}
		})
	}
}

func TestSwitchThemeReducerIgnoresUnknownCallbackFields(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha")
	// Unknown fields at the callback, details and result levels are ignored, not
	// treated as business facts.
	selectRaw := `{"taskchain":"SwitchTheme","taskid":101,"future":1,"details":{"task":"SwitchThemeByNameSelectTheme","future":{"n":9007199254740993},"result":{"text":"Alpha","future":true}}}`
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, selectRaw))
	branchRaw := `{"taskchain":"SwitchTheme","taskid":101,"future":"x","details":{"task":"SwitchThemeByNameAlreadySet","future":[1],"result":{"text":"已设置","future":2}}}`
	update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, branchRaw)
	wantSwitchThemeFact(t, update, TargetAlreadySet, switchThemeCodeAlreadySet, "Alpha")
}

func TestSwitchThemeReducerRepeatingIdenticalNodeIsStable(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha")
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
	first := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
	second := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
	wantSwitchThemeFact(t, first, TargetLocked, switchThemeCodeLocked, "Alpha")
	wantSwitchThemeFact(t, second, TargetLocked, switchThemeCodeLocked, "Alpha")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("repeated node produced a different update: %+v vs %+v", first, second)
	}
}

func TestSwitchThemeReducerSelectionConflictKeepsFirstSelection(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha", "Beta")
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
	update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta"))
	wantSwitchThemeFact(t, update, TargetAmbiguous, switchThemeCodeSelectionConflict, "")
	// The target fact is now fixed as ambiguous; a later branch cannot promote it.
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet)))
	if r.selected != "Alpha" || !r.selectedSet {
		t.Fatalf("conflicting selection rewrote the recorded theme: %q %v", r.selected, r.selectedSet)
	}
}

func TestSwitchThemeReducerKeepsFirstFactImmutably(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha")
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
	locked := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
	wantSwitchThemeFact(t, locked, TargetLocked, switchThemeCodeLocked, "Alpha")
	// A contradictory late confirm is still returned so the runtime can report
	// typed_conflict and preserve the original locked fact.
	confirm := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeConfirm))
	wantSwitchThemeFact(t, confirm, TargetConfirmationObserved, switchThemeCodeConfirmationObserved, "Alpha")
	if r.selected != "Alpha" {
		t.Fatalf("conflicting branch rewrote the selected theme: %q", r.selected)
	}
}

func switchThemeRuntimePlan(t *testing.T, params ...string) Plan {
	t.Helper()
	ref := AdapterRef{Name: switchThemeAdapterName, Version: switchThemeVersion}
	plan := runtimePlan(t)
	plan.Tasks = make([]Task, len(params))
	for i, p := range params {
		plan.Tasks[i] = Task{ID: fmt.Sprintf("theme-%d", i), Type: switchThemeTaskType, Adapter: &ref, Params: json.RawMessage(p)}
	}
	return plan
}

// newSwitchThemeRuntimeHarness drives the real monitor path (Run.observe)
// without launching a worker, so Core task ID association, persistence and
// typed event publication are exercised against the prepared theme reducers.
func newSwitchThemeRuntimeHarness(t *testing.T, plan Plan, coreIDs []int32) *Run {
	t.Helper()
	prepared, err := PreparePlan(plan, NewSwitchThemeAdapter())
	if err != nil {
		t.Fatal(err)
	}
	if len(coreIDs) != len(prepared.Tasks) {
		t.Fatalf("got %d Core task IDs for %d tasks", len(coreIDs), len(prepared.Tasks))
	}
	s, err := openStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = s.close()
	})
	index := make(map[int32]int, len(coreIDs))
	for i, id := range coreIDs {
		prepared.Tasks[i].CoreTaskID = id
		prepared.Tasks[i].State = Started
		index[id] = i
	}
	return &Run{
		runtime:        &Runtime{store: s},
		plan:           prepared.Plan,
		sourcePlan:     clonePlan(plan),
		result:         Result{SchemaVersion: SchemaVersion, RunID: newRunID(), State: Started, Cleanup: "pending", Tasks: prepared.Tasks},
		events:         make(chan Event, runEventMessages),
		done:           make(chan struct{}),
		changed:        make(chan struct{}, 1),
		commands:       ctx,
		cancelCommands: cancel,
		taskIndex:      index,
		reducers:       prepared.reducers,
		startIssued:    true,
	}
}

func switchThemeDrainEvents(r *Run) []Event {
	var events []Event
	for {
		select {
		case e := <-r.events:
			events = append(events, e)
		default:
			return events
		}
	}
}

func TestSwitchThemeRuntimeSameTypeTasksStayIsolated(t *testing.T) {
	plan := switchThemeRuntimePlan(t,
		switchThemeThemesJSON(t, "Alpha", "Beta"),
		switchThemeThemesJSON(t, "Gamma"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101, 202})

	// The selection OCR result is the only association for the chosen theme.
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 202, "Gamma"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 202, switchThemeNodeLocked))
	// Repeating the identical node is harmless: the stored fact stays and no
	// second typed event is published.
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 202, switchThemeNodeLocked))

	result := r.Snapshot()
	first, second := result.Tasks[0].Typed, result.Tasks[1].Typed
	if first == nil || second == nil {
		t.Fatalf("typed results missing: %+v", result.Tasks)
	}
	if first.Targets[0].Outcome != TargetAlreadySet || first.Targets[0].Code != switchThemeCodeAlreadySet ||
		!bytes.Equal(first.Targets[0].Details, json.RawMessage(`{"theme":"Beta"}`)) {
		t.Fatalf("first task typed = %+v", first)
	}
	if second.Targets[0].Outcome != TargetLocked || second.Targets[0].Code != switchThemeCodeLocked ||
		!bytes.Equal(second.Targets[0].Details, json.RawMessage(`{"theme":"Gamma"}`)) {
		t.Fatalf("second task typed = %+v", second)
	}
	// The second task's selection must not leak into the first reducer.
	if !bytes.Contains(first.Targets[0].Details, []byte("Beta")) || bytes.Contains(first.Targets[0].Details, []byte("Gamma")) {
		t.Fatalf("cross-task selection pollution: %s", first.Targets[0].Details)
	}

	typedEvents := 0
	for _, e := range switchThemeDrainEvents(r) {
		if len(e.Details) != 0 {
			t.Fatalf("default event disclosed the raw callback: %+v", e)
		}
		if e.Type != "typed" {
			continue
		}
		typedEvents++
		if e.Typed == nil || len(e.Typed.Targets) != 1 {
			t.Fatalf("typed event = %+v", e)
		}
		wantTask, wantCore := "theme-0", int32(101)
		if typedEvents == 2 {
			wantTask, wantCore = "theme-1", 202
		}
		if e.TaskID != wantTask || e.CoreTaskID != wantCore {
			t.Fatalf("typed event association = %+v", e)
		}
		details := e.Typed.Targets[0].Details
		if bytes.Contains(details, []byte("rect")) || bytes.Contains(details, []byte("score")) {
			t.Fatalf("typed event retained OCR geometry: %s", details)
		}
		e.Typed.Targets[0].Outcome = TargetAmbiguous
		e.Typed.Targets[0].Details[0] = '['
	}
	if typedEvents != 2 {
		t.Fatalf("typed event count = %d, want 2", typedEvents)
	}
	snapshot := r.Snapshot()
	if snapshot.Tasks[0].Typed.Targets[0].Outcome != TargetAlreadySet || snapshot.Tasks[1].Typed.Targets[0].Outcome != TargetLocked {
		t.Fatal("typed events alias snapshot facts")
	}

	record, err := ReadRecord(filepath.Dir(r.runtime.store.dir), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetAlreadySet ||
		record.Result.Tasks[1].Typed.Targets[0].Outcome != TargetLocked {
		t.Fatalf("record lost typed facts: %+v", record.Result.Tasks)
	}
	if !bytes.Contains(record.Plan.Tasks[0].Params, []byte(`"themes":["Alpha","Beta"]`)) ||
		!bytes.Contains(record.Plan.Tasks[1].Params, []byte(`"themes":["Gamma"]`)) {
		t.Fatalf("record lost the original caller plan: %+v", record.Plan.Tasks)
	}

	// Chain success cannot turn the locked task into a successful batch.
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":202}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101,202]}`)
	result = r.Snapshot()
	if result.Tasks[0].State != Succeeded || result.Tasks[1].State != Succeeded {
		t.Fatalf("native chain facts = %+v", result.Tasks)
	}
	if r.proof != Failed || r.proofCode != "typed_failed" || r.fatal != "" {
		t.Fatalf("batch proof = %s/%s fatal=%s", r.proof, r.proofCode, r.fatal)
	}
}

func TestSwitchThemeRuntimeLockedSurvivesChainCompletion(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	if result.Tasks[0].State != Succeeded {
		t.Fatalf("native chain fact = %s", result.Tasks[0].State)
	}
	if result.Tasks[0].Typed.State != Failed || result.Tasks[0].Typed.Code != "typed_failed" {
		t.Fatalf("typed aggregate = %+v", result.Tasks[0].Typed)
	}
	got := result.Tasks[0].Typed.Targets[0]
	if got.Outcome != TargetLocked || got.Code != switchThemeCodeLocked || !bytes.Equal(got.Details, json.RawMessage(`{"theme":"Alpha"}`)) {
		t.Fatalf("locked fact = %+v", got)
	}
	if r.proof != Failed || r.proofCode != "typed_failed" || r.fatal != "" {
		t.Fatalf("batch proof = %s/%s fatal=%s", r.proof, r.proofCode, r.fatal)
	}

	// A late confirm after the settled chain must not replace the lock.
	switchThemeDrainEvents(r)
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeConfirm))
	if r.fatal != "" || len(switchThemeDrainEvents(r)) != 0 {
		t.Fatalf("late confirm changed the settled run: fatal=%s", r.fatal)
	}
	if got := r.Snapshot().Tasks[0].Typed.Targets[0]; got.Outcome != TargetLocked {
		t.Fatalf("late confirm replaced the lock: %+v", got)
	}
}

func TestSwitchThemeRuntimeConfirmationRemainsIncomplete(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha"))
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeConfirm))
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	if result.Tasks[0].State != Succeeded {
		t.Fatalf("native chain fact = %s", result.Tasks[0].State)
	}
	typed := result.Tasks[0].Typed
	if typed.State != Unknown || typed.Code != "typed_incomplete" {
		t.Fatalf("confirmation aggregate = %+v", typed)
	}
	got := typed.Targets[0]
	if got.Outcome != TargetConfirmationObserved || got.Code != switchThemeCodeConfirmationObserved {
		t.Fatalf("confirmation fact = %+v", got)
	}
	if !bytes.Equal(got.Details, json.RawMessage(`{"theme":"Alpha"}`)) {
		t.Fatalf("confirmation details = %s", got.Details)
	}
	if r.proof != Unknown || r.proofCode != "typed_incomplete" || r.fatal != "" {
		t.Fatalf("batch proof = %s/%s fatal=%s", r.proof, r.proofCode, r.fatal)
	}
	record, err := ReadRecord(filepath.Dir(r.runtime.store.dir), result.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Result.Tasks[0].Typed.Targets[0].Outcome != TargetConfirmationObserved || record.Result.State != Started {
		t.Fatalf("record lost confirmation evidence: %+v", record.Result)
	}
}

func TestSwitchThemeRuntimeChainCompletionIsNotThemeSuccess(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	// Start-only evidence and a terminal chain callback carry no selection.
	sendTypedCallback(t, r, 20001, switchThemeSelectCallback(t, 101, "Alpha"))
	sendTypedCallback(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`)
	sendTypedCallback(t, r, 3, `{"finished_tasks":[101]}`)

	result := r.Snapshot()
	if result.Tasks[0].State != Succeeded {
		t.Fatalf("native chain fact = %s", result.Tasks[0].State)
	}
	typed := result.Tasks[0].Typed
	if typed.State != Unknown || typed.Code != "typed_incomplete" {
		t.Fatalf("chain completion fabricated typed success: %+v", typed)
	}
	if got := typed.Targets[0]; got.Outcome != TargetUnobserved || got.Code != "" || len(got.Details) != 0 {
		t.Fatalf("unobserved target = %+v", got)
	}
	if r.proof != Unknown || r.proofCode != "typed_incomplete" || r.fatal != "" {
		t.Fatalf("batch proof = %s/%s fatal=%s", r.proof, r.proofCode, r.fatal)
	}
}

func TestSwitchThemeRuntimeMalformedCallbackStopsWithoutFact(t *testing.T) {
	plan := switchThemeRuntimePlan(t, switchThemeThemesJSON(t, "Alpha"))
	r := newSwitchThemeRuntimeHarness(t, plan, []int32{101})
	sendTypedCallback(t, r, switchThemeMessageSubTaskCompleted,
		`{"taskchain":"SwitchTheme","taskid":101,"details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":"Alpha","text":"Beta"}}}`)
	if r.fatal != "typed_invalid_event" {
		t.Fatalf("malformed callback = %s", r.fatal)
	}
	for _, e := range switchThemeDrainEvents(r) {
		if e.Type == "typed" {
			t.Fatalf("rejected callback published a typed event: %+v", e)
		}
	}
	for _, target := range r.Snapshot().Tasks[0].Typed.Targets {
		if target.Outcome != TargetUnobserved {
			t.Fatalf("malformed callback created a fact: %+v", target)
		}
	}
}

func ExampleNewSwitchThemeAdapter() {
	adapter := NewSwitchThemeAdapter()
	prepared, err := adapter.Prepare(Task{
		ID:     "theme-1",
		Type:   "SwitchTheme",
		Params: json.RawMessage(`{"themes":["Theme One","Theme Two"]}`),
	})
	if err != nil {
		fmt.Println(err)
		return
	}
	ref := adapter.Descriptor()
	fmt.Printf("%s v%d\n", ref.Name, ref.Version)
	fmt.Println(string(prepared.Params))
	fmt.Println(prepared.Targets)
	// Output:
	// switch_theme v1
	// {"themes":["Theme One","Theme Two"]}
	// [theme-selection]
}
