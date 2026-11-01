package control

import (
	"bytes"
	"encoding/json"
	"runtime"
	"strconv"
	"strings"
)

const (
	copilotMessageStart    int32 = 20001
	copilotMessageExtra    int32 = 20003
	copilotWhatLoaded            = "CopilotListLoadTaskFileSuccess"
	copilotSettlementFirst       = "Copilot@WaitUntilEndOfAction"
)

func copilotTargetID(id int32) string { return "copilot:" + strconv.FormatInt(int64(id), 10) }

// Core's Windows path-to-UTF-8 conversion replaces backslashes with slashes.
// Preserve the caller's params and all other path components and spelling.
func copilotCallbackFilename(filename string, windows bool) string {
	if windows {
		return strings.ReplaceAll(filename, `\`, "/")
	}
	return filename
}

// A successful list-file load explicitly names the entry about to execute.
// Core runs each entry's load, navigation, battle and settlement synchronously,
// with FIFO callbacks. The settlement ProcessTask retains its original first
// node even though AbstractTask strips namespaces from details.task. Neither a
// filename nor the arrival count can identify an entry on its own.
type copilotReducer struct {
	entries []copilotEntry
	byID    map[int32]int
	seen    []bool
	active  int
	coreID  int32
}

func newCopilotReducer(entries []copilotEntry) TaskReducer {
	r := &copilotReducer{
		entries: append([]copilotEntry(nil), entries...),
		byID:    make(map[int32]int, len(entries)),
		seen:    make([]bool, len(entries)),
		active:  -1,
	}
	for i, entry := range entries {
		r.byID[entry.ID] = i
	}
	return r
}

// preparationProgress recognizes only source-reviewed pre-admission diagnostics.
// Core emits these during set_params, before assigning a task ID. Its message
// thread can deliver them after Start, so arrival time cannot identify them.
// They neither establish an entry scope nor decide native AppendTask acceptance.
func (*copilotReducer) preparationProgress(o Observation) bool {
	if o.MessageID != copilotMessageExtra && o.MessageID != 20000 || !json.Valid(o.JSON) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(o.JSON))
	d.UseNumber()
	if checkTypedKeys(d) != nil {
		return false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(o.JSON, &top) != nil || !bytes.Equal(bytes.TrimSpace(top["taskid"]), []byte("0")) {
		return false
	}
	chain, _ := operProgressFieldString(top, "taskchain")
	if chain != copilotTaskType {
		return false
	}
	if _, ok := operProgressDetailFields(top); !ok {
		return false
	}
	subtask, _ := operProgressFieldString(top, "subtask")
	what, _ := operProgressFieldString(top, "what")
	if o.MessageID == copilotMessageExtra {
		return subtask == "CopilotTask" && what == "CopilotPrecheckFile" ||
			subtask == "BattleFormationTask" && (what == "BattleFormationOperboxMatched" || what == "BattleFormationOperbox1Unmatched") ||
			subtask == "BattleProcessTask" && what == "UnsupportedLevel"
	}
	if subtask == "BattleFormationTask" {
		why, _ := operProgressFieldString(top, "why")
		_, hasWhat := top["what"]
		return !hasWhat && why == "OperatorMissing"
	}
	if subtask == "CopilotTask" {
		switch what {
		case "OperboxDataParseFailed", "CopilotFileReadError", "UserAdditionalOperInvalid", "CopilotStageNotSupported":
			return true
		}
	}
	return false
}

func (r *copilotReducer) Observe(o Observation) (TypedUpdate, error) {
	if o.MessageID != copilotMessageStart && o.MessageID != copilotMessageExtra || len(o.JSON) == 0 {
		return TypedUpdate{}, nil
	}
	if !json.Valid(o.JSON) || len(bytes.TrimSpace(o.JSON)) == 0 || bytes.TrimSpace(o.JSON)[0] != '{' {
		return TypedUpdate{}, typedError("copilot: callback must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(o.JSON))
	dec.UseNumber()
	if err := checkTypedKeys(dec); err != nil {
		return TypedUpdate{}, typedError("copilot: ambiguous or malformed callback fields")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(o.JSON, &top); err != nil {
		return TypedUpdate{}, typedError("copilot: callback must be a JSON object")
	}
	chain, _ := operProgressFieldString(top, "taskchain")
	if chain != copilotTaskType {
		return TypedUpdate{}, nil
	}
	var coreID int32
	if json.Unmarshal(top["taskid"], &coreID) != nil || coreID <= 0 || r.coreID != 0 && r.coreID != coreID {
		return TypedUpdate{}, nil
	}
	r.coreID = coreID
	subtask, _ := operProgressFieldString(top, "subtask")
	if o.MessageID == copilotMessageStart && subtask == "MultiCopilotTask" {
		// A new entry has begun even if its later file-load callback is absent.
		// Its settlement must not inherit the preceding entry's association.
		r.active = -1
		return TypedUpdate{}, nil
	}
	if o.MessageID == copilotMessageExtra {
		what, _ := operProgressFieldString(top, "what")
		if what == copilotWhatLoaded {
			// Even an unusable load boundary invalidates the previous scope;
			// a missing id must never attribute the next stage to that entry.
			previous := r.active
			r.active = -1
			fields, ok := operProgressDetailFields(top)
			if !ok || subtask != "MultiCopilotTask" {
				return TypedUpdate{}, nil
			}
			var id int32
			if json.Unmarshal(fields["id"], &id) != nil || bytes.Equal(bytes.TrimSpace(fields["id"]), []byte("null")) {
				return TypedUpdate{}, nil
			}
			index, known := r.byID[id]
			filename, named := operProgressFieldString(fields, "file_name")
			if !known || !named || filename != copilotCallbackFilename(r.entries[index].Filename, runtime.GOOS == "windows") || r.seen[index] && previous != index {
				return TypedUpdate{}, nil
			}
			r.active = index
			r.seen[index] = true
		}
		return TypedUpdate{}, nil
	}
	if r.active < 0 || subtask != "ProcessTask" {
		return TypedUpdate{}, nil
	}
	var first []string
	if json.Unmarshal(top["first"], &first) != nil || len(first) != 1 || first[0] != copilotSettlementFirst {
		return TypedUpdate{}, nil
	}
	fields, ok := operProgressDetailFields(top)
	if !ok {
		return TypedUpdate{}, nil
	}
	node, _ := operProgressFieldString(fields, "task")
	outcome, code := TargetAchieved, ""
	switch node {
	case "StageDrops-Stars-3":
		code = "three_star_clear"
	case "StageDrops-Stars-Adverse":
		// Core's stage-drop analyzer maps this adverse-mode marker to three
		// stars. It is not evidence of a non-three-star clear.
		code = "adverse_clear"
	case "FightMissionFailed":
		outcome, code = TargetFailed, "mission_failed"
	default:
		return TypedUpdate{}, nil
	}
	entry := r.entries[r.active]
	details, err := json.Marshal(struct {
		ID    int32 `json:"entry_id"`
		Index int   `json:"entry_index"`
		Raid  bool  `json:"is_raid"`
	}{entry.ID, entry.SourceIndex, entry.IsRaid})
	if err != nil {
		return TypedUpdate{}, typedError("copilot: cannot encode entry identity")
	}
	return TypedUpdate{Targets: []TargetResult{{ID: copilotTargetID(entry.ID), Outcome: outcome, Code: code, Details: details}}}, nil
}
