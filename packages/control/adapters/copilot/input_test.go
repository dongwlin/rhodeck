package copilot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func copilotTask(params string) control.Task {
	return control.Task{ID: "copilot-task", Type: copilotTaskType, Params: json.RawMessage(params)}
}

func TestCopilotPrepareAcceptsStrictInputs(t *testing.T) {
	for _, params := range []string{
		`{"filename":"relative/missing.json"}`,
		`{"filename":"/missing/a/../job.json","loop_times":2}`,
		`{"filename":"作业.json","loop_times":2147483647}`,
		`{"filename":"x","formation":false,"formation_index":0,"add_trust":true,"ignore_requirements":false,"use_sanity_potion":true,"support_unit_usage":0}`,
		`{"filename":"x","formation":true,"formation_index":4,"operbox_data_path":"../missing/box.json","user_additional":[{"name":"Alpha","skill":0},{"name":"Beta","skill":3}],"support_unit_usage":2,"support_unit_name":"Support"}`,
		`{"filename":"x","formation":true,"user_additional":[],"support_unit_usage":1,"support_unit_name":"Support"}`,
		`{"filename":"x","support_unit_usage":3}`,
		`{"copilot_list":[{"filename":"same.json"},{"filename":"same.json","is_raid":true},{"filename":"same.json","is_raid":false,"nav_name_override":""}],"loop_times":1}`,
		`{"copilot_list":[{"filename":"x","id":-2147483648},{"filename":"x","id":2147483647}],"formation":true}`,
		`{"filename":"\ud83d\ude00.json"}`,
	} {
		t.Run(params, func(t *testing.T) {
			prepared, err := New().Prepare(copilotTask(params))
			require.NoError(t, err)
			require.True(t, strictjson.ValidObject(prepared.Params, control.MaxAdapterParamsBytes), "compiled params are invalid: %s", prepared.Params)
			if strings.Contains(params, `"copilot_list"`) {
				require.False(t, prepared.Policy != control.ObjectiveAll || len(prepared.Targets) == 0 || prepared.Reducer == nil, "list preparation = %+v", prepared)
			} else {
				require.False(t, prepared.Policy != control.ObjectiveNone || len(prepared.Targets) != 0 || prepared.Reducer != nil, "input-only preparation = %+v", prepared)
			}
		})
	}
}

func TestCopilotPrepareRejectsInvalidInputs(t *testing.T) {
	for name, params := range map[string]string{
		"not object":              `[]`,
		"null root":               `null`,
		"missing source":          `{}`,
		"both sources":            `{"filename":"x","copilot_list":[{"filename":"x"}]}`,
		"empty filename":          `{"filename":""}`,
		"filename number":         `{"filename":3}`,
		"empty list":              `{"copilot_list":[]}`,
		"list object":             `{"copilot_list":{}}`,
		"string list entry":       `{"copilot_list":["x"]}`,
		"null list entry":         `{"copilot_list":[null]}`,
		"missing list filename":   `{"copilot_list":[{"id":0}]}`,
		"unknown top":             `{"filename":"x","future":true}`,
		"case variant":            `{"filename":"x","Formation":true}`,
		"deprecated paradox":      `{"copilot_list":[{"filename":"x","is_paradox":false}]}`,
		"wrong stage field":       `{"copilot_list":[{"filename":"x","stage_name":"1-7"}]}`,
		"top raid":                `{"filename":"x","is_raid":true}`,
		"duplicate top":           `{"filename":"x","filename":"y"}`,
		"duplicate list field":    `{"copilot_list":[{"filename":"x","is_raid":false,"is_raid":true}]}`,
		"duplicate deep key":      `{"filename":"x","future":{"params":{"json":{"n":1,"n":2}}}}`,
		"duplicate extra field":   `{"filename":"x","formation":true,"user_additional":[{"name":"A","skill":0,"skill":1}]}`,
		"duplicate explicit id":   `{"copilot_list":[{"filename":"x","id":1},{"filename":"y","id":1}]}`,
		"same numeric id":         `{"copilot_list":[{"filename":"x","id":0},{"filename":"y","id":-0}]}`,
		"id fraction":             `{"copilot_list":[{"filename":"x","id":1.0}]}`,
		"id exponent":             `{"copilot_list":[{"filename":"x","id":1e0}]}`,
		"id quoted":               `{"copilot_list":[{"filename":"x","id":"1"}]}`,
		"id overflow":             `{"copilot_list":[{"filename":"x","id":2147483648}]}`,
		"id underflow":            `{"copilot_list":[{"filename":"x","id":-2147483649}]}`,
		"list raid number":        `{"copilot_list":[{"filename":"x","is_raid":1}]}`,
		"list nav number":         `{"copilot_list":[{"filename":"x","nav_name_override":1}]}`,
		"formation number":        `{"filename":"x","formation":1}`,
		"index low":               `{"filename":"x","formation_index":-1}`,
		"index high":              `{"filename":"x","formation_index":5}`,
		"support low":             `{"filename":"x","support_unit_usage":-1}`,
		"support high":            `{"filename":"x","support_unit_usage":4}`,
		"support missing name":    `{"filename":"x","support_unit_usage":2}`,
		"support empty name":      `{"filename":"x","support_unit_name":""}`,
		"loop zero":               `{"filename":"x","loop_times":0}`,
		"loop negative":           `{"filename":"x","loop_times":-1}`,
		"loop fraction":           `{"filename":"x","loop_times":1.0}`,
		"loop exponent":           `{"filename":"x","loop_times":1e0}`,
		"loop overflow":           `{"filename":"x","loop_times":18446744073709551616}`,
		"loop Core range":         `{"filename":"x","loop_times":2147483648}`,
		"list loop ignored":       `{"copilot_list":[{"filename":"x"}],"loop_times":2}`,
		"box without formation":   `{"filename":"x","operbox_data_path":"box.json"}`,
		"box formation false":     `{"filename":"x","formation":false,"operbox_data_path":"box.json"}`,
		"empty box":               `{"filename":"x","formation":true,"operbox_data_path":""}`,
		"extra without formation": `{"filename":"x","user_additional":[]}`,
		"extra object":            `{"filename":"x","formation":true,"user_additional":{}}`,
		"extra null entry":        `{"filename":"x","formation":true,"user_additional":[null]}`,
		"extra missing name":      `{"filename":"x","formation":true,"user_additional":[{"skill":0}]}`,
		"extra empty name":        `{"filename":"x","formation":true,"user_additional":[{"name":"","skill":0}]}`,
		"extra missing skill":     `{"filename":"x","formation":true,"user_additional":[{"name":"A"}]}`,
		"extra duplicate name":    `{"filename":"x","formation":true,"user_additional":[{"name":"A","skill":0},{"name":"A","skill":2}]}`,
		"extra skill low":         `{"filename":"x","formation":true,"user_additional":[{"name":"A","skill":-1}]}`,
		"extra skill high":        `{"filename":"x","formation":true,"user_additional":[{"name":"A","skill":4}]}`,
		"extra unknown field":     `{"filename":"x","formation":true,"user_additional":[{"name":"A","skill":0,"future":1}]}`,
		"nul filename":            `{"filename":"x\u0000y"}`,
		"unpaired high surrogate": `{"filename":"\ud800"}`,
		"unpaired low surrogate":  `{"copilot_list":[{"filename":"x","nav_name_override":"\udc00"}]}`,
		"invalid utf8":            "{\"filename\":\"\xff\"}",
		"trailing input":          `{"filename":"x"}{}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New().Prepare(copilotTask(params))
			require.ErrorIs(t, err, control.ErrInvalidPlan, "invalid input accepted: %v", err)
		})
	}
	for _, field := range []string{"filename", "copilot_list", "formation", "formation_index", "add_trust", "ignore_requirements", "use_sanity_potion", "support_unit_usage", "support_unit_name", "loop_times", "operbox_data_path", "user_additional"} {
		t.Run("null "+field, func(t *testing.T) {
			params := `{"filename":"x","` + field + `":null}`
			if field == "filename" {
				params = `{"filename":null}`
			}
			_, err := New().Prepare(copilotTask(params))
			require.ErrorIs(t, err, control.ErrInvalidPlan, "null input accepted: %v", err)
		})
	}
	for _, field := range []string{"id", "filename", "nav_name_override", "is_raid"} {
		t.Run("null list "+field, func(t *testing.T) {
			params := `{"copilot_list":[{"filename":"x","` + field + `":null}]}`
			if field == "filename" {
				params = `{"copilot_list":[{"filename":null}]}`
			}
			_, err := New().Prepare(copilotTask(params))
			require.ErrorIs(t, err, control.ErrInvalidPlan, "null list input accepted: %v", err)
		})
	}
}

func TestCopilotCompilePreservesFieldsAndEntryIdentity(t *testing.T) {
	const input = `{"copilot_list":[{"filename":"same/../job.json","is_raid":false},{"filename":"same/../job.json","id":0,"is_raid":true},{"filename":"same/../job.json"},{"filename":"same/../job.json","id":2,"nav_name_override":"1-7"}],"formation":true,"formation_index":4,"add_trust":true,"ignore_requirements":true,"use_sanity_potion":true,"support_unit_usage":2,"support_unit_name":"Support","operbox_data_path":"../box.json","user_additional":[{"name":"Alpha","skill":3}],"loop_times":1}`
	params, entries, err := compileCopilot(json.RawMessage(input))
	require.NoError(t, err)
	got := []int32{entries[0].ID, entries[1].ID, entries[2].ID, entries[3].ID}
	require.Equal(t, []int32{1, 0, 3, 2}, got, "assigned IDs = %v", got)
	for i, entry := range entries {
		require.False(t, entry.SourceIndex != i || entry.Filename != "same/../job.json" || entry.IsRaid != (i == 1), "entry mapping lost identity: %+v", entry)
	}
	var source, compiled map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(input), &source))
	require.NoError(t, json.Unmarshal(params, &compiled))
	for field, want := range source {
		if field != "copilot_list" {
			require.True(t, bytes.Equal(compiled[field], want), "field %s changed: %s, want %s", field, compiled[field], want)
		}
	}
	var list []copilotCoreEntry
	require.NoError(t, json.Unmarshal(compiled["copilot_list"], &list))
	require.False(t, list[3].NavNameOverride == nil || *list[3].NavNameOverride != "1-7" || !list[1].IsRaid || list[0].ID == list[2].ID, "list fields lost: %+v", list)
	for range 5 {
		again, mapping, err := compileCopilot(json.RawMessage(input))
		require.False(t, err != nil || !bytes.Equal(again, params) || !reflect.DeepEqual(mapping, entries), "compilation was not deterministic: %s, %+v, %v", again, mapping, err)
	}
}

func TestCopilotPrepareBudgets(t *testing.T) {
	for name, params := range map[string]string{
		"input bytes":    `{"filename":"` + strings.Repeat("x", control.MaxAdapterParamsBytes) + `"}`,
		"compiled bytes": `{"filename":"` + strings.Repeat("<", control.MaxAdapterParamsBytes/6) + `"}`,
		"entry count":    `{"copilot_list":[` + strings.TrimSuffix(strings.Repeat(`{"filename":"x"},`, copilotMaxEntries+1), ",") + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := New().Prepare(copilotTask(params))
			require.ErrorIs(t, err, control.ErrInvalidPlan, "over-budget input accepted: %v", err)
		})
	}
	params := `{"copilot_list":[` + strings.TrimSuffix(strings.Repeat(`{"filename":"x"},`, copilotMaxEntries), ",") + `]}`
	_, err := New().Prepare(copilotTask(params))
	require.NoError(t, err, "entry limit rejected: %v", err)
}

func TestCopilotPrepareIsDetachedAndConcurrent(t *testing.T) {
	const input = `{"copilot_list":[{"filename":"not-present/../job.json","is_raid":true}]}`
	adapter := New()
	got := adapter.Descriptor()
	require.Equal(t, control.AdapterRef{Name: "copilot", Version: 1}, got, "descriptor = %+v", got)
	task := copilotTask(input)
	first, err := adapter.Prepare(task)
	require.NoError(t, err)
	task.Params[2] = 'X'
	require.False(t, bytes.Contains(first.Params, []byte("X")), "compiled params alias input")
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prepared, err := adapter.Prepare(copilotTask(input))
			assert.False(t, err != nil || !bytes.Equal(prepared.Params, first.Params), "concurrent preparation changed output: %s, %v", prepared.Params, err)
		}()
	}
	wg.Wait()
	_, err = adapter.Prepare(control.Task{Type: "ParadoxCopilot", Params: json.RawMessage(input)})
	require.ErrorIs(t, err, control.ErrInvalidPlan, "wrong Core task type accepted: %v", err)
}

func ExampleNew() {
	adapter := New()
	prepared, err := adapter.Prepare(control.Task{ID: "battle", Type: "Copilot", Params: json.RawMessage(`{"filename":"job.json","loop_times":2}`)})
	if err != nil {
		fmt.Println(err)
		return
	}
	fmt.Println(string(prepared.Params))
	fmt.Println(prepared.Policy)
	// Output:
	// {"filename":"job.json","formation":false,"loop_times":2}
	// none
}
