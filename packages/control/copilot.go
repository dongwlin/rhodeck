package control

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

const (
	copilotTaskType    = "Copilot"
	copilotAdapterName = "copilot"
	copilotVersion     = 1
	copilotMaxEntries  = MaxTypedTargetsPerTask
)

// copilotEntry keeps the submitted identity separate from the source file.
// SourceIndex is zero-based; the same filename may occur in several entries.
type copilotEntry struct {
	ID              int32
	SourceIndex     int
	Filename        string
	IsRaid          bool
	NavNameOverride *string
}

// NewCopilotAdapter returns the strict, explicitly selected copilot@1 adapter
// for the Copilot Core task. Raw Copilot tasks remain open Core inputs.
//
// Prepare requires exactly one of filename or a nonempty copilot_list. It
// validates formation, support, additional operators and loop options without
// reading any file or consulting a device. Paths are preserved exactly. List
// entries retain order and may repeat a filename; explicit signed 32-bit IDs
// must be unique, and omitted IDs are assigned from zero while avoiding every
// explicit ID. loop_times is an integer from 1 through 2147483647; a list only
// allows 1. Unknown fields, nulls, duplicate keys at any depth and input or
// compiled JSON over 64 KiB are rejected with ErrInvalidPlan.
//
// Preparation only compiles input and uses ObjectiveNone. It does not interpret
// task-chain completion as a clear, or establish any device-readiness claim.
func NewCopilotAdapter() TaskAdapter { return copilotAdapter{} }

type copilotAdapter struct{}

func (copilotAdapter) Descriptor() AdapterRef {
	return AdapterRef{Name: copilotAdapterName, Version: copilotVersion}
}

func (copilotAdapter) Prepare(task Task) (PreparedTask, error) {
	if task.Type != copilotTaskType {
		return PreparedTask{}, copilotReject("adapter requires Core type %q", copilotTaskType)
	}
	params, _, err := compileCopilot(task.Params)
	if err != nil {
		return PreparedTask{}, err
	}
	return PreparedTask{Params: params, Policy: ObjectiveNone}, nil
}

func copilotReject(format string, args ...any) error {
	return fmt.Errorf("%w: copilot: %s", ErrInvalidPlan, fmt.Sprintf(format, args...))
}

// compileCopilot returns detached Core params and the stable list mapping for
// task-local result interpretation. It has no file, native or device effects.
func compileCopilot(raw json.RawMessage) (json.RawMessage, []copilotEntry, error) {
	if !validObject(raw, MaxAdapterParamsBytes) {
		return nil, nil, copilotReject("params must be a JSON object of at most %d bytes", MaxAdapterParamsBytes)
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := checkTypedKeys(d); err != nil {
		return nil, nil, copilotReject("params contain duplicate or malformed fields")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, nil, copilotReject("params must be a JSON object")
	}
	for name, value := range fields {
		switch name {
		case "filename", "copilot_list", "formation", "formation_index", "add_trust", "ignore_requirements", "use_sanity_potion", "support_unit_usage", "support_unit_name", "loop_times", "operbox_data_path", "user_additional":
		default:
			return nil, nil, copilotReject("unknown params field %q", name)
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, nil, copilotReject("%s must not be null", name)
		}
	}
	_, hasFilename := fields["filename"]
	_, hasList := fields["copilot_list"]
	if hasFilename == hasList {
		return nil, nil, copilotReject("exactly one of filename or copilot_list is required")
	}
	if hasFilename {
		if _, err := copilotString(fields["filename"], false); err != nil {
			return nil, nil, copilotReject("filename must be a nonempty string without NUL")
		}
	}
	formation := false
	for _, name := range []string{"formation", "add_trust", "ignore_requirements", "use_sanity_potion"} {
		if value, ok := fields[name]; ok {
			flag, err := copilotBool(value)
			if err != nil {
				return nil, nil, copilotReject("%s must be a boolean", name)
			}
			if name == "formation" {
				formation = flag
			}
		}
	}
	if _, ok := fields["formation"]; !ok {
		fields["formation"] = json.RawMessage("false")
	}
	for _, option := range []struct {
		name string
		max  int
	}{{"formation_index", 4}, {"support_unit_usage", 3}} {
		if value, ok := fields[option.name]; ok {
			n, err := operProgressInteger(value)
			if err != nil || n < 0 || n > option.max {
				return nil, nil, copilotReject("%s must be an integer 0-%d", option.name, option.max)
			}
		}
	}
	for _, name := range []string{"support_unit_name", "operbox_data_path"} {
		if value, ok := fields[name]; ok {
			if _, err := copilotString(value, false); err != nil {
				return nil, nil, copilotReject("%s must be a nonempty string without NUL", name)
			}
		}
	}
	if value, ok := fields["support_unit_usage"]; ok && bytes.Equal(bytes.TrimSpace(value), []byte("2")) {
		if _, ok := fields["support_unit_name"]; !ok {
			return nil, nil, copilotReject("support_unit_usage 2 requires support_unit_name")
		}
	}
	if _, ok := fields["operbox_data_path"]; ok && !formation {
		return nil, nil, copilotReject("operbox_data_path requires formation=true")
	}
	if value, ok := fields["user_additional"]; ok {
		if !formation {
			return nil, nil, copilotReject("user_additional requires formation=true")
		}
		if err := validateCopilotAdditional(value); err != nil {
			return nil, nil, err
		}
	}
	if value, ok := fields["loop_times"]; ok {
		n, err := strconv.ParseUint(string(bytes.TrimSpace(value)), 10, 64)
		if err != nil || n == 0 || n > 1<<31-1 {
			return nil, nil, copilotReject("loop_times must be an integer 1-2147483647")
		}
		if hasList && n > 1 {
			return nil, nil, copilotReject("copilot_list does not support loop_times greater than 1")
		}
	}
	var entries []copilotEntry
	if hasList {
		var err error
		entries, err = decodeCopilotEntries(fields["copilot_list"])
		if err != nil {
			return nil, nil, err
		}
		compiled := make([]copilotCoreEntry, len(entries))
		for i, entry := range entries {
			compiled[i] = copilotCoreEntry{ID: entry.ID, Filename: entry.Filename, NavNameOverride: entry.NavNameOverride, IsRaid: entry.IsRaid}
		}
		fields["copilot_list"], err = json.Marshal(compiled)
		if err != nil {
			return nil, nil, copilotReject("cannot encode copilot_list")
		}
	}
	params, err := json.Marshal(fields)
	if err != nil || len(params) > MaxAdapterParamsBytes {
		return nil, nil, copilotReject("compiled params exceed %d bytes", MaxAdapterParamsBytes)
	}
	return params, entries, nil
}

type copilotCoreEntry struct {
	ID              int32   `json:"id"`
	Filename        string  `json:"filename"`
	NavNameOverride *string `json:"nav_name_override,omitempty"`
	IsRaid          bool    `json:"is_raid"`
}

func decodeCopilotEntries(raw json.RawMessage) ([]copilotEntry, error) {
	items, err := operProgressJSONArray(raw)
	if err != nil || len(items) == 0 || len(items) > copilotMaxEntries {
		return nil, copilotReject("copilot_list must contain 1-%d objects", copilotMaxEntries)
	}
	entries := make([]copilotEntry, len(items))
	explicit := make([]bool, len(items))
	used := make(map[int32]bool, len(items))
	for i, raw := range items {
		var fields map[string]json.RawMessage
		if !validObject(raw, MaxAdapterParamsBytes) || json.Unmarshal(raw, &fields) != nil {
			return nil, copilotReject("copilot_list entry %d must be an object", i)
		}
		entry := copilotEntry{SourceIndex: i}
		for name, value := range fields {
			switch name {
			case "id", "filename", "nav_name_override", "is_raid":
			default:
				return nil, copilotReject("copilot_list entry %d has unknown field %q", i, name)
			}
			if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, copilotReject("copilot_list entry %d field %s must not be null", i, name)
			}
		}
		filename, err := copilotString(fields["filename"], false)
		if err != nil {
			return nil, copilotReject("copilot_list entry %d requires a nonempty filename without NUL", i)
		}
		entry.Filename = filename
		if value, ok := fields["id"]; ok {
			id, err := operProgressInteger(value)
			if err != nil || int64(id) < -1<<31 || int64(id) > 1<<31-1 {
				return nil, copilotReject("copilot_list entry %d id must be a signed 32-bit integer", i)
			}
			entry.ID = int32(id)
			if used[entry.ID] {
				return nil, copilotReject("duplicate explicit copilot_list id %d", entry.ID)
			}
			used[entry.ID], explicit[i] = true, true
		}
		if value, ok := fields["is_raid"]; ok {
			flag, err := copilotBool(value)
			if err != nil {
				return nil, copilotReject("copilot_list entry %d is_raid must be a boolean", i)
			}
			entry.IsRaid = flag
		}
		if value, ok := fields["nav_name_override"]; ok {
			name, err := copilotString(value, true)
			if err != nil {
				return nil, copilotReject("copilot_list entry %d nav_name_override must be a string without NUL", i)
			}
			entry.NavNameOverride = &name
		}
		entries[i] = entry
	}
	var next int32
	for i := range entries {
		if explicit[i] {
			continue
		}
		for used[next] {
			next++
		}
		entries[i].ID = next
		used[next] = true
		next++
	}
	return entries, nil
}

func validateCopilotAdditional(raw json.RawMessage) error {
	items, err := operProgressJSONArray(raw)
	if err != nil {
		return copilotReject("user_additional must be an array")
	}
	seen := make(map[string]bool, len(items))
	for i, raw := range items {
		var fields map[string]json.RawMessage
		if !validObject(raw, MaxAdapterParamsBytes) || json.Unmarshal(raw, &fields) != nil {
			return copilotReject("user_additional entry %d must be an object", i)
		}
		for name := range fields {
			if name != "name" && name != "skill" {
				return copilotReject("user_additional entry %d has unknown field %q", i, name)
			}
		}
		name, err := copilotString(fields["name"], false)
		if err != nil || seen[name] {
			return copilotReject("user_additional entry %d requires a unique nonempty name without NUL", i)
		}
		seen[name] = true
		skill, err := operProgressInteger(fields["skill"])
		if err != nil || skill < 0 || skill > 3 {
			return copilotReject("user_additional entry %d skill must be an integer 0-3", i)
		}
	}
	return nil
}

func copilotString(raw json.RawMessage, empty bool) (string, error) {
	s, err := operProgressDecodeString(raw)
	if err != nil || !validText(s, MaxAdapterParamsBytes, empty) {
		return "", copilotReject("invalid string")
	}
	return s, nil
}

func copilotBool(raw json.RawMessage) (bool, error) {
	switch string(bytes.TrimSpace(raw)) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	default:
		return false, copilotReject("invalid boolean")
	}
}
