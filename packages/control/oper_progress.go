package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

const (
	operProgressTaskType     = "OperProgress"
	operProgressAdapterName  = "oper_progress"
	operProgressVersion      = 1
	operProgressMaxNameBytes = 256
	// One strict plan yields one target, so the plan count shares the fixed
	// per-task target budget.
	operProgressMaxPlans = MaxTypedTargetsPerTask
)

// operProgressAction names the single training action of one strict plan entry.
type operProgressAction string

const (
	operProgressActionElite        operProgressAction = "elite"
	operProgressActionSkillLevel   operProgressAction = "skill_level"
	operProgressActionSkillMastery operProgressAction = "skill_mastery"
)

// operProgressPlan is one validated plan entry. Exactly one action is selected;
// Expected is the target elite level, target skill level or mastery level.
type operProgressPlan struct {
	Role        string
	Name        string
	Action      operProgressAction
	Expected    int
	MasterySlot int // 1-3 for skill mastery; 0 for other actions
}

// operProgressTarget maps one caller-visible target ID back to its source plan.
// SourceIndex is zero-based; IDs are one-based (op-0001..op-0128). MasterySlot
// is exposed as 1-3 and addresses the three-element mastery array as 0-2.
type operProgressTarget struct {
	ID          string
	SourceIndex int
	Role        string
	Name        string
	Action      operProgressAction
	Expected    int
	MasterySlot int
}

// NewOperProgressAdapter returns the local adapter for the OperProgress Core
// task, selected explicitly as AdapterRef{Name: "oper_progress", Version: 1}.
// A raw OperProgress task without that selection is never routed here.
//
// Prepare accepts only the strict plans object:
//
//	{"plans":[{"role":"Warrior","name":"Example","elite":2}]}
//
// plans keeps input order and holds 1-128 entries. Each entry requires role and
// name and exactly one of elite (1-2), skill_level (2-7) or skill_mastery (three
// integers with exactly one nonzero slot in 1-3). Field names are exact and
// case-sensitive: the only accepted top-level field is plans and the only
// accepted plan fields are role, name, elite, skill_level and skill_mastery.
// Names are valid UTF-8, 1-256 bytes, contain no NUL, are not all whitespace and
// are never trimmed; unpaired UTF-16 surrogate escapes are rejected instead of
// being replaced with U+FFFD. Roles are Pioneer, Warrior, Tank, Sniper, Caster,
// Medic, Support or Special. Unknown or duplicate (at any depth) fields, enable,
// duplicate (role, name) entries, multiple or missing actions and params over
// 64 KiB are rejected with an error wrapping ErrInvalidPlan.
//
// Preparation is deterministic and side-effect free: it creates no directory,
// takes no lock, launches no worker, loads no native code and connects no
// device. It compiles the final Core object and returns stable target IDs
// op-0001..op-0128 with a fresh reducer and target mapping. It verifies local
// structure and budgets only; operator identity, role agreement with installed
// battle data, skill existence, elite prerequisites, materials, inventory,
// training-room state and device readiness remain unverified.
//
// During a run the reducer consumes only Core message 20003 with
// what=OperProgressDetail or what=OperProgressSummary for this task's chain.
// Elite and skill-level Completed details become achieved only when the reported
// level matches the requested level; AlreadySatisfied details require at least
// the requested level. A mastery Completed detail reports that training at the
// target level started, not that it finished; PrerequisiteTraining reports only
// the lower level that actually started. A uniquely associated Failed detail
// reports a target failure with a stable lowercase reason code, and a
// Skipped TrainingRoomBusy detail contributes only to the summary skip count.
// Incomplete, malformed or incompatible details never become success, and a
// late task-chain success does not replace a target failure.
func NewOperProgressAdapter() TaskAdapter { return operProgressAdapter{} }

type operProgressAdapter struct{}

func (operProgressAdapter) Descriptor() AdapterRef {
	return AdapterRef{Name: operProgressAdapterName, Version: operProgressVersion}
}

func (operProgressAdapter) Prepare(task Task) (PreparedTask, error) {
	if task.Type != operProgressTaskType {
		return PreparedTask{}, operProgressReject("adapter requires Core type %q", operProgressTaskType)
	}
	plans, err := decodeOperProgressPlans(task.Params)
	if err != nil {
		return PreparedTask{}, err
	}
	compiled := make([]operProgressCorePlan, len(plans))
	targets := make([]operProgressTarget, len(plans))
	ids := make([]string, len(plans))
	for i, plan := range plans {
		compiled[i] = plan.core()
		targets[i] = plan.target(i)
		ids[i] = targets[i].ID
	}
	params, err := json.Marshal(operProgressCoreParams{Plans: compiled})
	if err != nil {
		return PreparedTask{}, operProgressReject("cannot encode compiled params")
	}
	if len(params) > MaxAdapterParamsBytes {
		return PreparedTask{}, operProgressReject("compiled params exceed %d bytes", MaxAdapterParamsBytes)
	}
	return PreparedTask{Params: params, Targets: ids, Reducer: newOperProgressReducer(targets)}, nil
}

// operProgressReject wraps every input rejection with ErrInvalidPlan so callers
// classify failures with errors.Is instead of inspecting diagnostics.
func operProgressReject(format string, args ...any) error {
	return fmt.Errorf("%w: oper_progress: %s", ErrInvalidPlan, fmt.Sprintf(format, args...))
}

func decodeOperProgressPlans(raw json.RawMessage) ([]operProgressPlan, error) {
	if !validObject(raw, MaxAdapterParamsBytes) {
		return nil, operProgressReject("params must be a JSON object of at most %d bytes", MaxAdapterParamsBytes)
	}
	// Duplicate keys at any depth are rejected before names are interpreted.
	keys := json.NewDecoder(bytes.NewReader(raw))
	keys.UseNumber()
	if err := checkTypedKeys(keys); err != nil {
		return nil, operProgressReject("params contain duplicate or malformed fields")
	}
	// RawMessage values keep number literals exact; map keys are exact too, so
	// case variants such as PLANS or Role stay distinct and are rejected below.
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, operProgressReject("params must be a JSON object")
	}
	for key := range fields {
		if key != "plans" {
			return nil, operProgressReject("unknown params field %q", key)
		}
	}
	plansRaw, ok := fields["plans"]
	if !ok {
		return nil, operProgressReject("plans is required")
	}
	items, err := operProgressJSONArray(plansRaw)
	if err != nil {
		return nil, operProgressReject("plans must be an array")
	}
	if len(items) == 0 || len(items) > operProgressMaxPlans {
		return nil, operProgressReject("plans must contain 1-%d entries", operProgressMaxPlans)
	}
	plans := make([]operProgressPlan, len(items))
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		plan, err := decodeOperProgressPlan(item, i)
		if err != nil {
			return nil, err
		}
		key := plan.Role + "\x00" + plan.Name
		if seen[key] {
			return nil, operProgressReject("duplicate plan for role %q and name %q", plan.Role, plan.Name)
		}
		seen[key] = true
		plans[i] = plan
	}
	return plans, nil
}

func decodeOperProgressPlan(raw json.RawMessage, index int) (operProgressPlan, error) {
	var out operProgressPlan
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return out, operProgressReject("plan %d must be a JSON object", index)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &fields); err != nil {
		return out, operProgressReject("plan %d must be a JSON object", index)
	}
	for key := range fields {
		switch key {
		case "role", "name", "elite", "skill_level", "skill_mastery":
		default:
			return out, operProgressReject("plan %d has unknown field %q", index, key)
		}
	}

	roleRaw, ok := fields["role"]
	if !ok {
		return out, operProgressReject("plan %d requires role", index)
	}
	role, err := operProgressDecodeString(roleRaw)
	if err != nil {
		return out, operProgressReject("plan %d role must be a string: %v", index, err)
	}
	if !operProgressValidRole(role) {
		return out, operProgressReject("plan %d has unsupported role %q", index, role)
	}
	out.Role = role

	nameRaw, ok := fields["name"]
	if !ok {
		return out, operProgressReject("plan %d requires name", index)
	}
	name, err := operProgressDecodeString(nameRaw)
	if err != nil {
		return out, operProgressReject("plan %d name must be a string: %v", index, err)
	}
	if !validText(name, operProgressMaxNameBytes, false) {
		return out, operProgressReject("plan %d name must be valid UTF-8 without NUL and 1-%d bytes", index, operProgressMaxNameBytes)
	}
	if strings.TrimSpace(name) == "" {
		return out, operProgressReject("plan %d name must not be blank", index)
	}
	out.Name = name

	actions := 0
	if eliteRaw, ok := fields["elite"]; ok {
		actions++
		level, err := operProgressInteger(eliteRaw)
		if err != nil || level < 1 || level > 2 {
			return out, operProgressReject("plan %d elite must be an integer 1-2", index)
		}
		out.Action, out.Expected = operProgressActionElite, level
	}
	if skillRaw, ok := fields["skill_level"]; ok {
		actions++
		level, err := operProgressInteger(skillRaw)
		if err != nil || level < 2 || level > 7 {
			return out, operProgressReject("plan %d skill_level must be an integer 2-7", index)
		}
		out.Action, out.Expected = operProgressActionSkillLevel, level
	}
	if masteryRaw, ok := fields["skill_mastery"]; ok {
		actions++
		slot, level, err := decodeOperProgressMastery(masteryRaw)
		if err != nil {
			return out, operProgressReject("plan %d skill_mastery %v", index, err)
		}
		out.Action, out.MasterySlot, out.Expected = operProgressActionSkillMastery, slot, level
	}
	if actions != 1 {
		return out, operProgressReject("plan %d requires exactly one of elite, skill_level or skill_mastery", index)
	}
	return out, nil
}

// decodeOperProgressMastery requires exactly three integers where exactly one
// slot is 1-3 and the remaining slots are 0.
func decodeOperProgressMastery(raw json.RawMessage) (slot, level int, err error) {
	values, err := operProgressJSONArray(raw)
	if err != nil || len(values) != 3 {
		return 0, 0, errors.New("must be an array of exactly 3 integers")
	}
	for i, value := range values {
		n, err := operProgressInteger(value)
		if err != nil || n < 0 || n > 3 {
			return 0, 0, errors.New("levels must be integers 0-3")
		}
		if n == 0 {
			continue
		}
		if slot != 0 {
			return 0, 0, errors.New("must have exactly one nonzero slot")
		}
		slot, level = i+1, n
	}
	if slot == 0 {
		return 0, 0, errors.New("must have exactly one nonzero slot")
	}
	return slot, level, nil
}

// operProgressJSONArray accepts only a JSON array. Null and other values are
// rejected instead of being silently decoded as an empty array.
func operProgressJSONArray(raw json.RawMessage) ([]json.RawMessage, error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || b[0] != '[' {
		return nil, errors.New("not a JSON array")
	}
	var items []json.RawMessage
	if err := json.Unmarshal(b, &items); err != nil {
		return nil, err
	}
	return items, nil
}

// operProgressInteger accepts only an unquoted JSON integer literal. Fractional,
// exponent, quoted and non-number forms are rejected without a float64 roundtrip.
func operProgressInteger(raw json.RawMessage) (int, error) {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || (b[0] != '-' && (b[0] < '0' || b[0] > '9')) {
		return 0, errors.New("not a JSON number")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var n json.Number
	if err := dec.Decode(&n); err != nil {
		return 0, err
	}
	v, err := n.Int64()
	if err != nil {
		return 0, err
	}
	if int64(int(v)) != v {
		return 0, errors.New("integer out of range")
	}
	return int(v), nil
}

// operProgressDecodeString decodes one JSON string literal and rejects unpaired
// UTF-16 surrogate escapes. encoding/json would silently replace them with
// U+FFFD, which would rename the caller's target; a literal U+FFFD escape is
// accepted because it names that character exactly.
func operProgressDecodeString(raw json.RawMessage) (string, error) {
	b := bytes.TrimSpace(raw)
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return "", errors.New("not a JSON string")
	}
	end := len(b) - 1
	var out strings.Builder
	out.Grow(end - 1)
	for i := 1; i < end; {
		c := b[i]
		if c != '\\' {
			out.WriteByte(c)
			i++
			continue
		}
		if i+1 >= end {
			return "", errors.New("truncated escape")
		}
		switch b[i+1] {
		case '"', '\\', '/':
			out.WriteByte(b[i+1])
			i += 2
		case 'b':
			out.WriteByte('\b')
			i += 2
		case 'f':
			out.WriteByte('\f')
			i += 2
		case 'n':
			out.WriteByte('\n')
			i += 2
		case 'r':
			out.WriteByte('\r')
			i += 2
		case 't':
			out.WriteByte('\t')
			i += 2
		case 'u':
			unit, next, err := operProgressHexUnit(b, i, end)
			if err != nil {
				return "", err
			}
			i = next
			if unit >= 0xD800 && unit <= 0xDBFF {
				if i+1 >= end || b[i] != '\\' || b[i+1] != 'u' {
					return "", errors.New("unpaired UTF-16 surrogate")
				}
				low, next, err := operProgressHexUnit(b, i, end)
				if err != nil || low < 0xDC00 || low > 0xDFFF {
					return "", errors.New("unpaired UTF-16 surrogate")
				}
				i = next
				out.WriteRune(0x10000 + (rune(unit)-0xD800)<<10 + (rune(low) - 0xDC00))
			} else if unit >= 0xDC00 && unit <= 0xDFFF {
				return "", errors.New("unpaired UTF-16 surrogate")
			} else {
				out.WriteRune(rune(unit))
			}
		default:
			return "", errors.New("invalid escape")
		}
	}
	return out.String(), nil
}

// operProgressHexUnit decodes the \uXXXX escape starting at b[start] and returns
// the UTF-16 code unit plus the index just past the escape.
func operProgressHexUnit(b []byte, start, end int) (uint64, int, error) {
	if start+6 > end {
		return 0, 0, errors.New("truncated unicode escape")
	}
	unit, err := strconv.ParseUint(string(b[start+2:start+6]), 16, 16)
	if err != nil {
		return 0, 0, errors.New("invalid unicode escape")
	}
	return unit, start + 6, nil
}

func operProgressValidRole(role string) bool {
	switch role {
	case "Pioneer", "Warrior", "Tank", "Sniper", "Caster", "Medic", "Support", "Special":
		return true
	default:
		return false
	}
}

type operProgressCoreParams struct {
	Plans []operProgressCorePlan `json:"plans"`
}

// operProgressCorePlan is the final Core shape. Field order keeps the compiled
// preview deterministic.
type operProgressCorePlan struct {
	Role         string `json:"role"`
	Name         string `json:"name"`
	Elite        *int   `json:"elite,omitempty"`
	SkillLevel   *int   `json:"skill_level,omitempty"`
	SkillMastery []int  `json:"skill_mastery,omitempty"`
}

func (p operProgressPlan) core() operProgressCorePlan {
	out := operProgressCorePlan{Role: p.Role, Name: p.Name}
	switch p.Action {
	case operProgressActionElite:
		level := p.Expected
		out.Elite = &level
	case operProgressActionSkillLevel:
		level := p.Expected
		out.SkillLevel = &level
	case operProgressActionSkillMastery:
		mastery := make([]int, 3)
		mastery[p.MasterySlot-1] = p.Expected
		out.SkillMastery = mastery
	}
	return out
}

func (p operProgressPlan) target(index int) operProgressTarget {
	return operProgressTarget{
		ID:          fmt.Sprintf("op-%04d", index+1),
		SourceIndex: index,
		Role:        p.Role,
		Name:        p.Name,
		Action:      p.Action,
		Expected:    p.Expected,
		MasterySlot: p.MasterySlot,
	}
}
