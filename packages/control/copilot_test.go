package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func copilotTask(params string) Task {
	return Task{ID: "copilot-task", Type: copilotTaskType, Params: json.RawMessage(params)}
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
			prepared, err := NewCopilotAdapter().Prepare(copilotTask(params))
			if err != nil {
				t.Fatal(err)
			}
			if !validObject(prepared.Params, MaxAdapterParamsBytes) || prepared.Policy != ObjectiveNone || len(prepared.Targets) != 0 || prepared.Reducer != nil {
				t.Fatalf("input-only preparation = %+v", prepared)
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
			if _, err := NewCopilotAdapter().Prepare(copilotTask(params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("invalid input accepted: %v", err)
			}
		})
	}
	for _, field := range []string{"filename", "copilot_list", "formation", "formation_index", "add_trust", "ignore_requirements", "use_sanity_potion", "support_unit_usage", "support_unit_name", "loop_times", "operbox_data_path", "user_additional"} {
		t.Run("null "+field, func(t *testing.T) {
			params := `{"filename":"x","` + field + `":null}`
			if field == "filename" {
				params = `{"filename":null}`
			}
			if _, err := NewCopilotAdapter().Prepare(copilotTask(params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("null input accepted: %v", err)
			}
		})
	}
	for _, field := range []string{"id", "filename", "nav_name_override", "is_raid"} {
		t.Run("null list "+field, func(t *testing.T) {
			params := `{"copilot_list":[{"filename":"x","` + field + `":null}]}`
			if field == "filename" {
				params = `{"copilot_list":[{"filename":null}]}`
			}
			if _, err := NewCopilotAdapter().Prepare(copilotTask(params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("null list input accepted: %v", err)
			}
		})
	}
}

func TestCopilotCompilePreservesFieldsAndEntryIdentity(t *testing.T) {
	const input = `{"copilot_list":[{"filename":"same/../job.json","is_raid":false},{"filename":"same/../job.json","id":0,"is_raid":true},{"filename":"same/../job.json"},{"filename":"same/../job.json","id":2,"nav_name_override":"1-7"}],"formation":true,"formation_index":4,"add_trust":true,"ignore_requirements":true,"use_sanity_potion":true,"support_unit_usage":2,"support_unit_name":"Support","operbox_data_path":"../box.json","user_additional":[{"name":"Alpha","skill":3}],"loop_times":1}`
	params, entries, err := compileCopilot(json.RawMessage(input))
	if err != nil {
		t.Fatal(err)
	}
	if got := []int32{entries[0].ID, entries[1].ID, entries[2].ID, entries[3].ID}; !reflect.DeepEqual(got, []int32{1, 0, 3, 2}) {
		t.Fatalf("assigned IDs = %v", got)
	}
	for i, entry := range entries {
		if entry.SourceIndex != i || entry.Filename != "same/../job.json" || entry.IsRaid != (i == 1) {
			t.Fatalf("entry mapping lost identity: %+v", entry)
		}
	}
	var source, compiled map[string]json.RawMessage
	if err := json.Unmarshal([]byte(input), &source); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(params, &compiled); err != nil {
		t.Fatal(err)
	}
	for field, want := range source {
		if field != "copilot_list" && !bytes.Equal(compiled[field], want) {
			t.Fatalf("field %s changed: %s, want %s", field, compiled[field], want)
		}
	}
	var list []copilotCoreEntry
	if err := json.Unmarshal(compiled["copilot_list"], &list); err != nil {
		t.Fatal(err)
	}
	if list[3].NavNameOverride == nil || *list[3].NavNameOverride != "1-7" || !list[1].IsRaid || list[0].ID == list[2].ID {
		t.Fatalf("list fields lost: %+v", list)
	}
	for range 5 {
		again, mapping, err := compileCopilot(json.RawMessage(input))
		if err != nil || !bytes.Equal(again, params) || !reflect.DeepEqual(mapping, entries) {
			t.Fatalf("compilation was not deterministic: %s, %+v, %v", again, mapping, err)
		}
	}
}

func TestCopilotPrepareBudgets(t *testing.T) {
	for name, params := range map[string]string{
		"input bytes":    `{"filename":"` + strings.Repeat("x", MaxAdapterParamsBytes) + `"}`,
		"compiled bytes": `{"filename":"` + strings.Repeat("<", MaxAdapterParamsBytes/6) + `"}`,
		"entry count":    `{"copilot_list":[` + strings.TrimSuffix(strings.Repeat(`{"filename":"x"},`, copilotMaxEntries+1), ",") + `]}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCopilotAdapter().Prepare(copilotTask(params)); !errors.Is(err, ErrInvalidPlan) {
				t.Fatalf("over-budget input accepted: %v", err)
			}
		})
	}
	params := `{"copilot_list":[` + strings.TrimSuffix(strings.Repeat(`{"filename":"x"},`, copilotMaxEntries), ",") + `]}`
	if _, err := NewCopilotAdapter().Prepare(copilotTask(params)); err != nil {
		t.Fatalf("entry limit rejected: %v", err)
	}
}

func TestCopilotPrepareIsDetachedAndConcurrent(t *testing.T) {
	const input = `{"copilot_list":[{"filename":"not-present/../job.json","is_raid":true}]}`
	adapter := NewCopilotAdapter()
	if got := adapter.Descriptor(); got != (AdapterRef{Name: "copilot", Version: 1}) {
		t.Fatalf("descriptor = %+v", got)
	}
	task := copilotTask(input)
	first, err := adapter.Prepare(task)
	if err != nil {
		t.Fatal(err)
	}
	task.Params[2] = 'X'
	if bytes.Contains(first.Params, []byte("X")) {
		t.Fatal("compiled params alias input")
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prepared, err := adapter.Prepare(copilotTask(input))
			if err != nil || !bytes.Equal(prepared.Params, first.Params) {
				t.Errorf("concurrent preparation changed output: %s, %v", prepared.Params, err)
			}
		}()
	}
	wg.Wait()
	if _, err := adapter.Prepare(Task{Type: "ParadoxCopilot", Params: json.RawMessage(input)}); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("wrong Core task type accepted: %v", err)
	}
}

func TestCopilotRawPlanRemainsOpen(t *testing.T) {
	plan := validTestPlan(t)
	ref := NewCopilotAdapter().Descriptor()
	const raw = `{"future":{"n":9007199254740993},"is_paradox":true}`
	plan.Tasks = []Task{
		{ID: "raw", Type: "Copilot", Params: json.RawMessage(raw)},
		{ID: "strict", Type: "Copilot", Adapter: &ref, Params: json.RawMessage(`{"filename":"unavailable/../job.json"}`)},
	}
	prepared, err := PreparePlan(plan, NewCopilotAdapter())
	if err != nil {
		t.Fatal(err)
	}
	if string(prepared.Plan.Tasks[0].Params) != raw || prepared.Tasks[0].Policy != "" || prepared.Tasks[0].Typed != nil {
		t.Fatalf("raw task changed: %+v", prepared.Tasks[0])
	}
	if prepared.Tasks[1].Policy != ObjectiveNone || prepared.Tasks[1].Typed != nil || len(prepared.reducers) != 0 {
		t.Fatalf("input-only preview = %+v", prepared.Tasks[1])
	}
	if !bytes.Contains(prepared.Plan.Tasks[1].Params, []byte(`"filename":"unavailable/../job.json"`)) {
		t.Fatalf("path was changed: %s", prepared.Plan.Tasks[1].Params)
	}
}

func ExampleNewCopilotAdapter() {
	adapter := NewCopilotAdapter()
	prepared, err := adapter.Prepare(Task{ID: "battle", Type: "Copilot", Params: json.RawMessage(`{"filename":"job.json","loop_times":2}`)})
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
