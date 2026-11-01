package copilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control"
)

func copilotResultEntries() []copilotEntry {
	return []copilotEntry{
		{ID: 0, SourceIndex: 0, Filename: "same.json"},
		{ID: 7, SourceIndex: 1, Filename: "same.json", IsRaid: true},
		{ID: -3, SourceIndex: 2, Filename: "last.json"},
	}
}

func copilotResultLoad(id int32, filename string) string {
	return fmt.Sprintf(`{"taskchain":"Copilot","taskid":101,"subtask":"MultiCopilotTask","what":"CopilotListLoadTaskFileSuccess","details":{"id":%d,"file_name":%q}}`, id, filename)
}

func copilotResultNode(node string) string {
	return fmt.Sprintf(`{"taskchain":"Copilot","taskid":101,"subtask":"ProcessTask","first":["Copilot@WaitUntilEndOfAction"],"details":{"task":%q}}`, node)
}

func observeCopilotResult(t *testing.T, reducer control.TaskReducer, messageID int32, raw string) control.TypedUpdate {
	t.Helper()
	update, err := reducer.Observe(control.Observation{MessageID: messageID, JSON: json.RawMessage(raw)})
	if err != nil {
		t.Fatal(err)
	}
	return update
}

func requireNoCopilotFacts(t *testing.T, update control.TypedUpdate) {
	t.Helper()
	if len(update.Targets) != 0 || len(update.Summary) != 0 {
		t.Fatalf("callback manufactured business facts: %+v", update)
	}
}

func mutateCopilotResultField(t *testing.T, source, field, value string) string {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(source), &fields); err != nil {
		t.Fatal(err)
	}
	if value == "" {
		delete(fields, field)
	} else {
		fields[field] = json.RawMessage(value)
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestCopilotReducerInterpretsOnlyAssociatedStageFacts(t *testing.T) {
	for _, test := range []struct {
		node    string
		outcome control.TargetOutcome
		code    string
	}{
		{"StageDrops-Stars-3", control.TargetAchieved, "three_star_clear"},
		{"StageDrops-Stars-Adverse", control.TargetAchieved, "adverse_clear"},
		{"FightMissionFailed", control.TargetFailed, "mission_failed"},
	} {
		t.Run(test.node, func(t *testing.T) {
			reducer := newCopilotReducer(copilotResultEntries())
			requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20003, copilotResultLoad(7, "same.json")))
			raw := copilotResultNode(test.node)
			update := observeCopilotResult(t, reducer, 20001, raw)
			want := control.TypedUpdate{Targets: []control.TargetResult{{ID: "copilot:7", Outcome: test.outcome, Code: test.code, Details: json.RawMessage(`{"entry_id":7,"entry_index":1,"is_raid":true}`)}}}
			if !reflect.DeepEqual(update, want) {
				t.Fatalf("associated stage fact = %+v, want %+v", update, want)
			}
			if duplicate := observeCopilotResult(t, reducer, 20001, raw); !reflect.DeepEqual(duplicate, update) {
				t.Fatalf("duplicate changed business fact: %+v, want %+v", duplicate, update)
			}
			if bytes.Contains(update.Targets[0].Details, []byte("same.json")) {
				t.Fatal("public target details contain a private source path")
			}
		})
	}
}

func TestCopilotReducerSeparatesRepeatedFilesAndRaids(t *testing.T) {
	reducer := newCopilotReducer(copilotResultEntries())
	for _, test := range []struct {
		id       int32
		filename string
		target   string
	}{
		{0, "same.json", "copilot:0"},
		{7, "same.json", "copilot:7"},
		{-3, "last.json", "copilot:-3"},
	} {
		requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20003, copilotResultLoad(test.id, test.filename)))
		update := observeCopilotResult(t, reducer, 20001, copilotResultNode("StageDrops-Stars-3"))
		if len(update.Targets) != 1 || update.Targets[0].ID != test.target {
			t.Fatalf("entry %d changed another target: %+v", test.id, update)
		}
	}
	// A replayed older entry cannot reopen its scope after another entry began.
	requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20003, copilotResultLoad(0, "same.json")))
	for _, node := range []string{"StageDrops-Stars-3", "FightMissionFailed"} {
		requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20001, copilotResultNode(node)))
	}
}

func TestCopilotReducerRequiresCompleteLoadIdentity(t *testing.T) {
	const validDetails = `{"id":7,"file_name":"same.json"}`
	for name, details := range map[string]string{
		"missing":            "",
		"null":               `null`,
		"array":              `[]`,
		"string":             `"x"`,
		"empty":              `{}`,
		"missing id":         `{"file_name":"same.json"}`,
		"null id":            `{"id":null,"file_name":"same.json"}`,
		"string id":          `{"id":"7","file_name":"same.json"}`,
		"fraction id":        `{"id":7.0,"file_name":"same.json"}`,
		"exponent id":        `{"id":7e0,"file_name":"same.json"}`,
		"overflow id":        `{"id":2147483648,"file_name":"same.json"}`,
		"unknown id":         `{"id":8,"file_name":"same.json"}`,
		"missing filename":   `{"id":7}`,
		"null filename":      `{"id":7,"file_name":null}`,
		"number filename":    `{"id":7,"file_name":7}`,
		"empty filename":     `{"id":7,"file_name":""}`,
		"different filename": `{"id":7,"file_name":"different.json"}`,
		"incorrect key":      `{"id":7,"filename":"same.json"}`,
	} {
		t.Run(name, func(t *testing.T) {
			reducer := newCopilotReducer(copilotResultEntries())
			observeCopilotResult(t, reducer, 20003, copilotResultLoad(0, "same.json"))
			raw := mutateCopilotResultField(t, copilotResultLoad(7, "same.json"), "details", details)
			requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20003, raw))
			requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20001, copilotResultNode("StageDrops-Stars-3")))
		})
	}
	for _, details := range []string{validDetails, `{"id":7,"file_name":"same.json","future":{"n":9007199254740993}}`} {
		reducer := newCopilotReducer(copilotResultEntries())
		observeCopilotResult(t, reducer, 20003, mutateCopilotResultField(t, copilotResultLoad(7, "same.json"), "details", details))
		if update := observeCopilotResult(t, reducer, 20001, copilotResultNode("StageDrops-Stars-3")); len(update.Targets) != 1 || update.Targets[0].ID != "copilot:7" {
			t.Fatalf("valid loaded entry was not associated: %+v", update)
		}
	}
}

func TestCopilotReducerRequiresExactSettlementScope(t *testing.T) {
	base := copilotResultNode("StageDrops-Stars-3")
	for _, test := range []struct {
		field string
		value string
	}{
		{"subtask", ""}, {"subtask", `null`}, {"subtask", `3`}, {"subtask", `"BattleProcessTask"`}, {"subtask", `"MultiCopilotTask"`},
		{"first", ""}, {"first", `null`}, {"first", `"Copilot@WaitUntilEndOfAction"`}, {"first", `[]`},
		{"first", `[null]`}, {"first", `[3]`}, {"first", `["WaitUntilEndOfAction"]`},
		{"first", `["Other@WaitUntilEndOfAction"]`}, {"first", `["Copilot@WaitUntilEndOfAction","Stop"]`},
		{"details", ""}, {"details", `null`}, {"details", `[]`}, {"details", `{}`},
		{"details", `{"task":null}`}, {"details", `{"task":7}`},
		{"details", `{"task":"Copilot@StageDrops-Stars-3"}`},
		{"details", `{"task":"Other@StageDrops-Stars-3"}`},
		{"details", `{"task":"StageDrops-Stars-2"}`}, {"details", `{"task":"FutureNode"}`},
	} {
		t.Run(test.field+"="+test.value, func(t *testing.T) {
			reducer := newCopilotReducer(copilotResultEntries())
			observeCopilotResult(t, reducer, 20003, copilotResultLoad(7, "same.json"))
			raw := mutateCopilotResultField(t, base, test.field, test.value)
			requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20001, raw))
		})
	}
	// A recognizable node still needs a prior uniquely associated load callback.
	for _, node := range []string{"StageDrops-Stars-3", "StageDrops-Stars-Adverse", "FightMissionFailed"} {
		requireNoCopilotFacts(t, observeCopilotResult(t, newCopilotReducer(copilotResultEntries()), 20001, copilotResultNode(node)))
	}
}

func TestCopilotReducerClearsScopeOnNextEntryStart(t *testing.T) {
	reducer := newCopilotReducer(copilotResultEntries())
	observeCopilotResult(t, reducer, 20003, copilotResultLoad(0, "same.json"))
	start := `{"taskchain":"Copilot","taskid":101,"subtask":"MultiCopilotTask"}`
	requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20001, start))
	requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20001, copilotResultNode("StageDrops-Stars-3")))
	// A duplicated old load cannot reopen an entry after its boundary ended.
	requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20003, copilotResultLoad(0, "same.json")))
	requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20001, copilotResultNode("StageDrops-Stars-3")))
	observeCopilotResult(t, reducer, 20003, copilotResultLoad(7, "same.json"))
	update := observeCopilotResult(t, reducer, 20001, copilotResultNode("StageDrops-Stars-3"))
	if len(update.Targets) != 1 || update.Targets[0].ID != "copilot:7" {
		t.Fatalf("next distinct entry did not establish scope: %+v", update)
	}
}

func TestCopilotReducerRequiresMatchingCoreIdentity(t *testing.T) {
	for _, test := range []struct {
		field string
		value string
	}{
		{"taskid", ""}, {"taskid", `null`}, {"taskid", `0`}, {"taskid", `-1`}, {"taskid", `102`},
		{"taskid", `"101"`}, {"taskid", `101.0`}, {"taskid", `101e0`}, {"taskid", `2147483648`},
		{"taskchain", ""}, {"taskchain", `null`}, {"taskchain", `1`}, {"taskchain", `"Fight"`}, {"taskchain", `"copilot"`},
	} {
		t.Run(test.field+"="+test.value, func(t *testing.T) {
			reducer := newCopilotReducer(copilotResultEntries())
			observeCopilotResult(t, reducer, 20003, copilotResultLoad(7, "same.json"))
			raw := mutateCopilotResultField(t, copilotResultNode("StageDrops-Stars-3"), test.field, test.value)
			requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20001, raw))
		})
	}
	reducer := newCopilotReducer(copilotResultEntries())
	observeCopilotResult(t, reducer, 20003, copilotResultLoad(7, "same.json"))
	foreign := mutateCopilotResultField(t, copilotResultLoad(0, "same.json"), "taskid", "102")
	requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20003, foreign))
	update := observeCopilotResult(t, reducer, 20001, copilotResultNode("StageDrops-Stars-3"))
	if len(update.Targets) != 1 || update.Targets[0].ID != "copilot:7" {
		t.Fatalf("foreign task changed active entry: %+v", update)
	}
}

func TestCopilotReducerIgnoresNonEvidence(t *testing.T) {
	for _, test := range []struct {
		messageID int32
		raw       string
	}{
		{10002, `{"taskchain":"Copilot","taskid":101}`},
		{3, `{"finished_tasks":[101]}`},
		{20002, copilotResultNode("StageDrops-Stars-3")},
		{20003, copilotResultNode("StageDrops-Stars-3")},
		{20003, `{"taskchain":"Copilot","taskid":101,"what":"CopilotAction","details":{"task":"StageDrops-Stars-3"}}`},
		{20003, `{"taskchain":"Copilot","taskid":101,"what":"FutureCallback","details":{"task":"StageDrops-Stars-3"}}`},
	} {
		t.Run(fmt.Sprint(test.messageID)+test.raw, func(t *testing.T) {
			reducer := newCopilotReducer(copilotResultEntries())
			observeCopilotResult(t, reducer, 20003, copilotResultLoad(7, "same.json"))
			requireNoCopilotFacts(t, observeCopilotResult(t, reducer, test.messageID, test.raw))
		})
	}
	for _, subtask := range []string{"", `null`, `7`, `"ProcessTask"`} {
		reducer := newCopilotReducer(copilotResultEntries())
		observeCopilotResult(t, reducer, 20003, copilotResultLoad(0, "same.json"))
		load := mutateCopilotResultField(t, copilotResultLoad(7, "same.json"), "subtask", subtask)
		requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20003, load))
		requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20001, copilotResultNode("StageDrops-Stars-3")))
	}
}

func TestCopilotReducerRejectsMalformedAndDuplicateCallbacks(t *testing.T) {
	for _, raw := range []string{
		`{`, `[]`, `null`, `"x"`, `{"taskid":101}{}`,
		`{"taskchain":"Copilot","taskchain":"Copilot","taskid":101}`,
		`{"taskchain":"Copilot","taskid":101,"details":{"id":0,"id":7}}`,
		`{"taskchain":"Copilot","taskid":101,"future":{"params":{"json":{"n":1,"n":2}}}}`,
	} {
		for _, messageID := range []int32{20001, 20003} {
			t.Run(fmt.Sprint(messageID)+raw, func(t *testing.T) {
				reducer := newCopilotReducer(copilotResultEntries())
				update, err := reducer.Observe(control.Observation{MessageID: messageID, JSON: json.RawMessage(raw)})
				if !errors.Is(err, control.ErrTypedResult) {
					t.Fatalf("malformed callback error = %v", err)
				}
				requireNoCopilotFacts(t, update)
			})
		}
	}
}

func TestCopilotReducerMappingIsDetached(t *testing.T) {
	entries := copilotResultEntries()
	reducer := newCopilotReducer(entries)
	entries[0].ID = 99
	entries[0].Filename = "changed.json"
	entries[0].IsRaid = true
	raw := []byte(copilotResultLoad(0, "same.json"))
	requireNoCopilotFacts(t, observeCopilotResult(t, reducer, 20003, string(raw)))
	for i := range raw {
		raw[i] = 'x'
	}
	update := observeCopilotResult(t, reducer, 20001, copilotResultNode("StageDrops-Stars-3"))
	if len(update.Targets) != 1 || update.Targets[0].ID != "copilot:0" || string(update.Targets[0].Details) != `{"entry_id":0,"entry_index":0,"is_raid":false}` {
		t.Fatalf("caller mutation changed entry identity: %+v", update)
	}
}

func TestCopilotCallbackFilenameUsesPlatformRendering(t *testing.T) {
	for _, test := range []struct {
		filename string
		windows  bool
		want     string
	}{
		{`C:\jobs\..\job.json`, true, "C:/jobs/../job.json"},
		{`C:/jobs\job.json`, true, "C:/jobs/job.json"},
		{`\\host\share\job.json`, true, "//host/share/job.json"},
		{`relative\job.json`, false, `relative\job.json`},
		{"./jobs/../job.json", false, "./jobs/../job.json"},
	} {
		if got := copilotCallbackFilename(test.filename, test.windows); got != test.want {
			t.Fatalf("callback filename for %q, windows=%v = %q, want %q", test.filename, test.windows, got, test.want)
		}
	}
}
