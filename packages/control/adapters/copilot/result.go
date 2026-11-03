package copilot

import (
	"bytes"
	"encoding/json"
	"fmt"
	"runtime"
	"strconv"
	"strings"

	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
)

const (
	copilotMessageStart    int32 = 20001
	copilotMessageExtra    int32 = 20003
	copilotWhatLoaded            = "CopilotListLoadTaskFileSuccess"
	copilotSettlementFirst       = "Copilot@WaitUntilEndOfAction"
)

func copilotTargetID(id int32) string { return "copilot:" + strconv.FormatInt(int64(id), 10) }

func copilotCallbackFilename(filename string, windows bool) string {
	if windows {
		return strings.ReplaceAll(filename, `\`, "/")
	}
	return filename
}

type copilotReducer struct {
	entries []copilotEntry
	byID    map[int32]int
	seen    []bool
	active  int
	coreID  int32
}

func newCopilotReducer(entries []copilotEntry) control.TaskReducer {
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

func (*copilotReducer) IsUnassociatedProgress(o control.Observation) bool {
	if o.MessageID != copilotMessageExtra && o.MessageID != 20000 || !json.Valid(o.JSON) {
		return false
	}
	d := json.NewDecoder(bytes.NewReader(o.JSON))
	d.UseNumber()
	if strictjson.CheckKeys(d) != nil {
		return false
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(o.JSON, &top) != nil || !bytes.Equal(bytes.TrimSpace(top["taskid"]), []byte("0")) {
		return false
	}
	chain, _ := strictjson.FieldString(top, "taskchain")
	if chain != copilotTaskType {
		return false
	}
	if _, ok := strictjson.ObjectField(top, "details"); !ok {
		return false
	}
	subtask, _ := strictjson.FieldString(top, "subtask")
	what, _ := strictjson.FieldString(top, "what")
	if o.MessageID == copilotMessageExtra {
		return subtask == "CopilotTask" && what == "CopilotPrecheckFile" ||
			subtask == "BattleFormationTask" && (what == "BattleFormationOperboxMatched" || what == "BattleFormationOperbox1Unmatched") ||
			subtask == "BattleProcessTask" && what == "UnsupportedLevel"
	}
	if subtask == "BattleFormationTask" {
		why, _ := strictjson.FieldString(top, "why")
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

func (r *copilotReducer) Observe(o control.Observation) (control.TypedUpdate, error) {
	if o.MessageID != copilotMessageStart && o.MessageID != copilotMessageExtra || len(o.JSON) == 0 {
		return control.TypedUpdate{}, nil
	}
	if !json.Valid(o.JSON) || len(bytes.TrimSpace(o.JSON)) == 0 || bytes.TrimSpace(o.JSON)[0] != '{' {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "copilot: callback must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(o.JSON))
	dec.UseNumber()
	if err := strictjson.CheckKeys(dec); err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "copilot: ambiguous or malformed callback fields")
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(o.JSON, &top); err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "copilot: callback must be a JSON object")
	}
	chain, _ := strictjson.FieldString(top, "taskchain")
	if chain != copilotTaskType {
		return control.TypedUpdate{}, nil
	}
	var coreID int32
	if json.Unmarshal(top["taskid"], &coreID) != nil || coreID <= 0 || r.coreID != 0 && r.coreID != coreID {
		return control.TypedUpdate{}, nil
	}
	r.coreID = coreID
	subtask, _ := strictjson.FieldString(top, "subtask")
	if o.MessageID == copilotMessageStart && subtask == "MultiCopilotTask" {
		r.active = -1
		return control.TypedUpdate{}, nil
	}
	if o.MessageID == copilotMessageExtra {
		what, _ := strictjson.FieldString(top, "what")
		if what == copilotWhatLoaded {
			previous := r.active
			r.active = -1
			fields, ok := strictjson.ObjectField(top, "details")
			if !ok || subtask != "MultiCopilotTask" {
				return control.TypedUpdate{}, nil
			}
			var id int32
			if json.Unmarshal(fields["id"], &id) != nil || bytes.Equal(bytes.TrimSpace(fields["id"]), []byte("null")) {
				return control.TypedUpdate{}, nil
			}
			index, known := r.byID[id]
			filename, named := strictjson.FieldString(fields, "file_name")
			if !known || !named || filename != copilotCallbackFilename(r.entries[index].Filename, runtime.GOOS == "windows") || r.seen[index] && previous != index {
				return control.TypedUpdate{}, nil
			}
			r.active = index
			r.seen[index] = true
		}
		return control.TypedUpdate{}, nil
	}
	if r.active < 0 || subtask != "ProcessTask" {
		return control.TypedUpdate{}, nil
	}
	var first []string
	if json.Unmarshal(top["first"], &first) != nil || len(first) != 1 || first[0] != copilotSettlementFirst {
		return control.TypedUpdate{}, nil
	}
	fields, ok := strictjson.ObjectField(top, "details")
	if !ok {
		return control.TypedUpdate{}, nil
	}
	node, _ := strictjson.FieldString(fields, "task")
	outcome, code := control.TargetAchieved, ""
	switch node {
	case "StageDrops-Stars-3":
		code = "three_star_clear"
	case "StageDrops-Stars-Adverse":
		code = "adverse_clear"
	case "FightMissionFailed":
		outcome, code = control.TargetFailed, "mission_failed"
	default:
		return control.TypedUpdate{}, nil
	}
	entry := r.entries[r.active]
	details, err := json.Marshal(struct {
		ID    int32 `json:"entry_id"`
		Index int   `json:"entry_index"`
		Raid  bool  `json:"is_raid"`
	}{entry.ID, entry.SourceIndex, entry.IsRaid})
	if err != nil {
		return control.TypedUpdate{}, fmt.Errorf("%w: %s", control.ErrTypedResult, "copilot: cannot encode entry identity")
	}
	return control.TypedUpdate{Targets: []control.TargetResult{{ID: copilotTargetID(entry.ID), Outcome: outcome, Code: code, Details: details}}}, nil
}

var _ control.UnassociatedObservationClassifier = (*copilotReducer)(nil)
