package switchtheme

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/stretchr/testify/require"
)

func switchThemeTaskWithThemes(t *testing.T, themes ...string) control.Task {
	t.Helper()
	raw, err := json.Marshal(struct {
		Themes []string `json:"themes"`
	}{Themes: themes})
	require.NoError(t, err)
	return control.Task{ID: "theme", Type: switchThemeTaskType, Params: raw}
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
	prepared, err := New().Prepare(switchThemeTaskWithThemes(t, themes...))
	require.NoError(t, err)
	reducer, ok := prepared.Reducer.(*switchThemeReducer)
	require.True(t, ok, "reducer type = %T, want a fresh *switchThemeReducer", prepared.Reducer)
	return reducer
}

func switchThemeObserve(t *testing.T, reducer control.TaskReducer, messageID int32, raw string) control.TypedUpdate {
	t.Helper()
	update, err := reducer.Observe(control.Observation{MessageID: messageID, JSON: json.RawMessage(raw)})
	require.NoError(t, err, "unexpected reducer error: %v", err)
	return update
}

func switchThemeSelectCallback(t *testing.T, taskID int32, theme string) string {
	t.Helper()
	encoded, err := json.Marshal(theme)
	require.NoError(t, err)
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"ProcessTask","details":{"task":"SwitchThemeByNameSelectTheme","result":{"text":%s,"rect":[1,2,3,4],"score":0.98}}}`, taskID, encoded)
}

func switchThemeBranchCallback(t *testing.T, taskID int32, node string) string {
	t.Helper()
	encoded, err := json.Marshal(node)
	require.NoError(t, err)
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"ProcessTask","details":{"task":%s,"result":{"text":"branch","rect":[5,6,7,8],"score":0.5}}}`, taskID, encoded)
}

func switchThemeNotFoundCallback(t *testing.T, taskID int32, theme string) string {
	t.Helper()
	encoded, err := json.Marshal(theme)
	require.NoError(t, err)
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"SwitchThemeTask","what":"SwitchThemeNotFound","details":{"theme":%s}}`, taskID, encoded)
}

func switchThemeSkippedCallback(taskID int32) string {
	return fmt.Sprintf(`{"taskchain":"SwitchTheme","taskid":%d,"subtask":"SwitchThemeTask","what":"SwitchThemeSkipped","details":{}}`, taskID)
}

func wantNoSwitchThemeUpdate(t *testing.T, update control.TypedUpdate) {
	t.Helper()
	require.False(t, len(update.Targets) != 0 || len(update.Summary) != 0, "unexpected typed update: %+v", update)
}

func wantSwitchThemeFact(t *testing.T, update control.TypedUpdate, outcome control.TargetOutcome, code, theme string) {
	t.Helper()
	require.Empty(t, update.Summary, "unexpected summary: %s", update.Summary)
	require.Len(t, update.Targets, 1, "targets = %+v, want exactly one", update.Targets)
	got := update.Targets[0]
	require.True(t, got.ID == switchThemeTargetID && got.Outcome == outcome && got.Code == code,
		"target = %+v, want id=%q outcome=%q code=%q", got, switchThemeTargetID, outcome, code)
	var wantDetails json.RawMessage
	if theme != "" {
		raw, err := json.Marshal(switchThemeTargetDetails{Theme: theme})
		require.NoError(t, err)
		wantDetails = raw
	}
	require.True(t, bytes.Equal(got.Details, wantDetails), "details = %s, want %s", got.Details, wantDetails)
}

func TestSwitchThemeAdapterDescriptor(t *testing.T) {
	adapter := New()
	want := control.AdapterRef{Name: "switch_theme", Version: 1}
	got := adapter.Descriptor()
	require.Equal(t, want, got, "descriptor = %+v, want %+v", got, want)
}

func TestSwitchThemePrepareCompilesOrderedCandidates(t *testing.T) {
	adapter := New()
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
			prepared, err := adapter.Prepare(control.Task{ID: "theme-1", Type: switchThemeTaskType, Params: json.RawMessage(params)})
			require.NoError(t, err)
			require.Equal(t, params, string(prepared.Params), "compiled params = %s, want ordered candidate array %s", prepared.Params, params)
			// Multiple candidates still yield exactly one target. Preparation does
			// not model a sequential attempt per candidate.
			require.Equal(t, []string{switchThemeTargetID}, prepared.Targets, "targets = %v, want [%s]", prepared.Targets, switchThemeTargetID)
			reducer, ok := prepared.Reducer.(*switchThemeReducer)
			require.True(t, ok && reducer != nil, "reducer has type %T, want fresh *switchThemeReducer", prepared.Reducer)
			require.Equal(t, tc.themes, reducer.themes, "reducer candidates = %v, want %v", reducer.themes, tc.themes)
		})
	}
}

func TestSwitchThemePrepareRejectsInvalidInput(t *testing.T) {
	adapter := New()
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
			_, err := adapter.Prepare(control.Task{ID: "theme", Type: switchThemeTaskType, Params: json.RawMessage(tc.params)})
			require.ErrorIs(t, err, control.ErrInvalidPlan, "invalid input accepted: %v", err)
		})
	}
}

func TestSwitchThemePrepareAcceptsCandidateBoundaries(t *testing.T) {
	adapter := New()
	longName := strings.Repeat("a", switchThemeMaxThemeBytes)
	themes := switchThemeNumberedThemes(switchThemeMaxThemes)
	cases := []struct {
		name   string
		params string
		check  func(t *testing.T, prepared control.PreparedTask)
	}{
		{"minimum count", switchThemeThemesJSON(t, "Theme One"), nil},
		{"maximum count", switchThemeThemesJSON(t, themes...), func(t *testing.T, prepared control.PreparedTask) {
			require.Len(t, prepared.Targets, 1, "targets = %v", prepared.Targets)
		}},
		{"maximum name bytes", `{"themes":["` + longName + `"]}`, nil},
		{"paired surrogate", `{"themes":["\ud83d\ude00"]}`, func(t *testing.T, prepared control.PreparedTask) {
			require.Equal(t, `{"themes":["😀"]}`, string(prepared.Params), "compiled params = %s", prepared.Params)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prepared, err := adapter.Prepare(control.Task{ID: "theme", Type: switchThemeTaskType, Params: json.RawMessage(tc.params)})
			require.NoError(t, err)
			require.True(t, prepared.Reducer != nil, "adapter returned a nil reducer")
			if tc.check != nil {
				tc.check(t, prepared)
			}
		})
	}
}

func TestSwitchThemePrepareParamsByteBoundary(t *testing.T) {
	adapter := New()
	base := switchThemeThemesJSON(t, "Theme One")
	for _, size := range []int{control.MaxAdapterParamsBytes, control.MaxAdapterParamsBytes + 1} {
		params := base + strings.Repeat(" ", size-len(base))
		_, err := adapter.Prepare(control.Task{ID: "theme", Type: switchThemeTaskType, Params: json.RawMessage(params)})
		if size == control.MaxAdapterParamsBytes {
			require.NoError(t, err, "params at the byte limit rejected: %v", err)
		}
		if size > control.MaxAdapterParamsBytes {
			require.ErrorIs(t, err, control.ErrInvalidPlan, "params over the byte limit accepted: %v", err)
		}
	}
}

func TestSwitchThemePrepareRejectsWrongCoreType(t *testing.T) {
	adapter := New()
	params := json.RawMessage(switchThemeThemesJSON(t, "Theme One"))
	for _, taskType := range []string{"OperProgress", "switch_theme", "SwitchTheme ", "switchtheme", ""} {
		_, err := adapter.Prepare(control.Task{ID: "theme", Type: taskType, Params: params})
		require.ErrorIs(t, err, control.ErrInvalidPlan, "Core type %q accepted: %v", taskType, err)
	}
}

func TestSwitchThemePrepareIsDetachedAndFreshPerTask(t *testing.T) {
	adapter := New()
	const source = `{"themes":["Alpha","Beta"]}`
	input := []byte(source)
	prepared, err := adapter.Prepare(control.Task{ID: "theme-1", Type: switchThemeTaskType, Params: json.RawMessage(input)})
	require.NoError(t, err)
	require.Equal(t, source, string(input), "Prepare mutated the caller's params")
	reducer := prepared.Reducer.(*switchThemeReducer)
	input[2] = 'X'
	require.True(t, string(prepared.Params) == source && prepared.Targets[0] == switchThemeTargetID && reducer.themes[0] == "Alpha",
		"prepared output aliases the caller's params")

	second, err := adapter.Prepare(switchThemeTaskWithThemes(t, "Alpha", "Beta"))
	require.NoError(t, err)
	secondReducer := second.Reducer.(*switchThemeReducer)
	require.True(t, secondReducer != reducer && &secondReducer.themes[0] != &reducer.themes[0],
		"reducer was reused across preparations")
	prepared.Targets[0] = "mutated"
	prepared.Params[2] = 'Y'
	reducer.themes[0] = "mutated"
	require.True(t, second.Targets[0] == switchThemeTargetID && string(second.Params) == source && secondReducer.themes[0] == "Alpha",
		"preparations share mutable slices or JSON buffers")
}

func TestSwitchThemeRawPlanRemainsOpen(t *testing.T) {
	plan := validTestPlan(t)
	ref := control.AdapterRef{Name: switchThemeAdapterName, Version: switchThemeVersion}
	rawParams := json.RawMessage(`{"themes":["Raw Theme"],"enable":true,"future":9007199254740993}`)
	plan.Tasks = []control.Task{
		{ID: "raw", Type: switchThemeTaskType, Params: rawParams},
		{ID: "strict", Type: switchThemeTaskType, Adapter: &ref, Params: json.RawMessage(`{"themes":["Strict One","Strict Two"]}`)},
	}
	prepared, err := control.PreparePlan(plan, New())
	require.NoError(t, err)
	require.Nil(t, prepared.Tasks[0].Typed, "raw task was routed through the adapter")
	// The 9007199254740993 literal is asserted byte-wise on purpose; it must
	// survive as an exact integer, not a float64 round-trip.
	require.True(t, bytes.Contains(prepared.Plan.Tasks[0].Params, []byte(`"enable":true`)) &&
		bytes.Contains(prepared.Plan.Tasks[0].Params, []byte("9007199254740993")),
		"raw params lost unknown fields or integer literals: %s", prepared.Plan.Tasks[0].Params)
	typed := prepared.Tasks[1].Typed
	require.True(t, typed != nil && len(typed.Targets) == 1 && typed.Targets[0].ID == switchThemeTargetID && typed.Targets[0].Outcome == control.TargetUnobserved,
		"strict task was not prepared: %+v", typed)
	require.Equal(t, `{"themes":["Strict One","Strict Two"]}`, string(prepared.Plan.Tasks[1].Params), "strict task kept uncompiled params: %s", prepared.Plan.Tasks[1].Params)
}

func TestSwitchThemeReducerRequiresExactSelection(t *testing.T) {
	t.Run("records the second candidate", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha", "Beta")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta")))
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta")))
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet))
		wantSwitchThemeFact(t, update, control.TargetAlreadySet, switchThemeCodeAlreadySet, "Beta")
	})
	t.Run("candidate order is not a fallback", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Beta", "Alpha")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
		wantSwitchThemeFact(t, update, control.TargetLocked, switchThemeCodeLocked, "Alpha")
	})
	t.Run("single candidate still requires selection evidence", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha")
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet))
		wantSwitchThemeFact(t, update, control.TargetAmbiguous, switchThemeCodeSelectionMissing, "")

		r = switchThemeTestReducer(t, "Alpha")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
		update = switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet))
		wantSwitchThemeFact(t, update, control.TargetAlreadySet, switchThemeCodeAlreadySet, "Alpha")
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
				wantSwitchThemeFact(t, update, control.TargetAmbiguous, switchThemeCodeSelectionUnmatched, "")
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
				wantSwitchThemeFact(t, update, control.TargetAmbiguous, switchThemeCodeSelectionMissing, "")
			})
		}
	})
}

func TestSwitchThemeReducerBranches(t *testing.T) {
	for _, tc := range []struct {
		node    string
		outcome control.TargetOutcome
		code    string
	}{
		{switchThemeNodeAlreadySet, control.TargetAlreadySet, switchThemeCodeAlreadySet},
		{switchThemeNodeLocked, control.TargetLocked, switchThemeCodeLocked},
		{switchThemeNodeConfirm, control.TargetConfirmationObserved, switchThemeCodeConfirmationObserved},
	} {
		t.Run(tc.node, func(t *testing.T) {
			r := switchThemeTestReducer(t, "Alpha", "Beta")
			wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta")))
			update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, tc.node))
			wantSwitchThemeFact(t, update, tc.outcome, tc.code, "Beta")
			details := update.Targets[0].Details
			for _, forbidden := range []string{"rect", "score", "path", "actually_changed", "final_state_verified", "branch"} {
				require.False(t, bytes.Contains(details, []byte(forbidden)), "details exposed %q: %s", forbidden, details)
			}
		})
	}
}

func TestSwitchThemeReducerNotFound(t *testing.T) {
	t.Run("exact candidate match", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha", "Beta")
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskExtraInfo, switchThemeNotFoundCallback(t, 101, "Beta"))
		wantSwitchThemeFact(t, update, control.TargetNotFound, switchThemeCodeNotFound, "Beta")
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
				wantSwitchThemeFact(t, update, control.TargetAmbiguous, switchThemeCodeNotFoundUnassociated, "")
			})
		}
	})
	t.Run("selection conflict", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha", "Beta")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskExtraInfo, switchThemeNotFoundCallback(t, 101, "Beta"))
		wantSwitchThemeFact(t, update, control.TargetAmbiguous, switchThemeCodeSelectionConflict, "")
	})
	t.Run("same selected theme", func(t *testing.T) {
		r := switchThemeTestReducer(t, "Alpha")
		wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
		update := switchThemeObserve(t, r, switchThemeMessageSubTaskExtraInfo, switchThemeNotFoundCallback(t, 101, "Alpha"))
		wantSwitchThemeFact(t, update, control.TargetNotFound, switchThemeCodeNotFound, "Alpha")
	})
}

func TestSwitchThemeReducerSkippedAndStartOnly(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha")
	// SubTaskStart only reports that an action was about to begin.
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, 20001, switchThemeSelectCallback(t, 101, "Alpha")))
	// A terminal chain callback carries no theme-selection evidence.
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, 10002, `{"taskchain":"SwitchTheme","taskid":101}`))
	update := switchThemeObserve(t, r, switchThemeMessageSubTaskExtraInfo, switchThemeSkippedCallback(101))
	wantSwitchThemeFact(t, update, control.TargetAmbiguous, switchThemeCodeSkipped, "")
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
			_, err := r.Observe(control.Observation{MessageID: switchThemeMessageSubTaskCompleted, JSON: json.RawMessage(raw)})
			require.ErrorIs(t, err, control.ErrTypedResult, "malformed callback accepted: %v", err)
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
	wantSwitchThemeFact(t, update, control.TargetAlreadySet, switchThemeCodeAlreadySet, "Alpha")
}

func TestSwitchThemeReducerRepeatingIdenticalNodeIsStable(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha")
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
	first := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
	second := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
	wantSwitchThemeFact(t, first, control.TargetLocked, switchThemeCodeLocked, "Alpha")
	wantSwitchThemeFact(t, second, control.TargetLocked, switchThemeCodeLocked, "Alpha")
	require.Equal(t, first, second, "repeated node produced a different update: %+v vs %+v", first, second)
}

func TestSwitchThemeReducerSelectionConflictKeepsFirstSelection(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha", "Beta")
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
	update := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Beta"))
	wantSwitchThemeFact(t, update, control.TargetAmbiguous, switchThemeCodeSelectionConflict, "")
	// The target fact is now fixed as ambiguous; a later branch cannot promote it.
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeAlreadySet)))
	require.True(t, r.selected == "Alpha" && r.selectedSet,
		"conflicting selection rewrote the recorded theme: %q %v", r.selected, r.selectedSet)
}

func TestSwitchThemeReducerKeepsFirstFactImmutably(t *testing.T) {
	r := switchThemeTestReducer(t, "Alpha")
	wantNoSwitchThemeUpdate(t, switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeSelectCallback(t, 101, "Alpha")))
	locked := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeLocked))
	wantSwitchThemeFact(t, locked, control.TargetLocked, switchThemeCodeLocked, "Alpha")
	// A contradictory late confirm is still returned so the runtime can report
	// typed_conflict and preserve the original locked fact.
	confirm := switchThemeObserve(t, r, switchThemeMessageSubTaskCompleted, switchThemeBranchCallback(t, 101, switchThemeNodeConfirm))
	wantSwitchThemeFact(t, confirm, control.TargetConfirmationObserved, switchThemeCodeConfirmationObserved, "Alpha")
	require.Equal(t, "Alpha", r.selected, "conflicting branch rewrote the selected theme: %q", r.selected)
}

func ExampleNew() {
	adapter := New()
	prepared, err := adapter.Prepare(control.Task{
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
