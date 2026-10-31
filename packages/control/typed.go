package control

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

const (
	MaxAdapterParamsBytes      = 64 << 10
	MaxTypedTargetsPerTask     = 128
	MaxTypedTargetsPerRun      = 512
	MaxTypedTaskBytes          = 64 << 10
	MaxTypedResultBytes        = 512 << 10
	MaxTypedDetailBytes        = 4096
	MaxTypedSummaryBytes       = 8192
	MaxTypedEventBytes         = 64 << 10
	MaxTypedEventMessages      = 32
	MaxTypedEventRetainedBytes = 256 << 10
)

var (
	ErrTypedResult   = errors.New("control: invalid typed result")
	ErrTypedConflict = errors.New("control: conflicting typed fact")
	ErrTypedBudget   = errors.New("control: typed budget exceeded")
)

// AdapterRef explicitly selects one locally registered adapter version.
// Recognizing Task.Type never enables an adapter on the generic input path.
type AdapterRef struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// TaskAdapter validates a task and creates a fresh, task-local reducer. Prepare
// must be deterministic and have no filesystem, native or device side effects.
// Implementations must reject task types they do not understand and export only
// selected business facts, never an unfiltered native callback as typed details.
// An adapter may be reused concurrently by separate PreparePlan calls.
type TaskAdapter interface {
	Descriptor() AdapterRef
	Prepare(Task) (PreparedTask, error)
}

// PreparedTask bridges adapter input to the open Core task contract. Params is
// the final Core JSON object. Targets are stable caller-visible correlation IDs.
// Reducer is fresh per preparation and receives callbacks serially for one task.
type PreparedTask struct {
	Params  json.RawMessage
	Targets []string
	Reducer TaskReducer
}

// TaskReducer observes only callbacks associated with its submitted Core task.
// Returning an empty update ignores a callback. Reducers must bound their own
// scratch state; the runtime bounds every retained result and emitted update.
type TaskReducer interface {
	Observe(Observation) (TypedUpdate, error)
}

type Observation struct {
	MessageID    int32
	CoreSequence uint64
	JSON         json.RawMessage
}

// TypedUpdate adds immutable target facts and an optional immutable summary.
// Repeating the identical fact is harmless; a different fact for the same
// target is a conflict. Targets absent from the update remain unchanged.
type TypedUpdate struct {
	Targets []TargetResult
	Summary json.RawMessage
}

type TargetOutcome string

const (
	TargetUnobserved                  TargetOutcome = "unobserved"
	TargetAchieved                    TargetOutcome = "achieved"
	TargetReportedTrainingStarted     TargetOutcome = "reported_target_training_started"
	TargetReportedPrerequisiteStarted TargetOutcome = "reported_prerequisite_training_started"
	TargetFailed                      TargetOutcome = "failed"
	TargetAmbiguous                   TargetOutcome = "ambiguous"
	TargetAlreadySet                  TargetOutcome = "already_set"
	TargetConfirmationObserved        TargetOutcome = "confirmation_observed"
	TargetLocked                      TargetOutcome = "locked"
	TargetNotFound                    TargetOutcome = "not_found"
)

// TargetResult contains a selected fact, not the task-chain completion state.
// Reported training and confirmation observations do not establish achievement.
type TargetResult struct {
	ID      string          `json:"id"`
	Outcome TargetOutcome   `json:"outcome"`
	Code    string          `json:"code,omitempty"`
	Details json.RawMessage `json:"details,omitempty"`
}

type TypedResult struct {
	SchemaVersion int             `json:"schema_version"`
	Adapter       AdapterRef      `json:"adapter"`
	State         State           `json:"state"`
	Code          string          `json:"code,omitempty"`
	Targets       []TargetResult  `json:"targets"`
	Summary       json.RawMessage `json:"summary,omitempty"`
}

// PreparedPlan is a detached, side-effect-free preview. Runtime.Run prepares
// again so mutating this preview can never change an admitted run or reducer.
type PreparedPlan struct {
	Plan     Plan         `json:"plan"`
	Tasks    []TaskResult `json:"tasks"`
	reducers map[int]TaskReducer
}

// PreparePlan validates explicit adapter selections and compiles final Core
// parameters. A raw task never invokes an adapter, even for a familiar type.
func PreparePlan(p Plan, adapters ...TaskAdapter) (PreparedPlan, error) {
	bad := func(s string) (PreparedPlan, error) {
		return PreparedPlan{}, fmt.Errorf("%w: %s", ErrInvalidPlan, s)
	}
	if err := ValidatePlan(p); err != nil {
		return PreparedPlan{}, err
	}
	registry := make(map[AdapterRef]TaskAdapter, len(adapters))
	for _, adapter := range adapters {
		if nilInterface(adapter) {
			return bad("nil adapter")
		}
		ref := adapter.Descriptor()
		if !validAdapterRef(ref) || registry[ref] != nil {
			return bad("invalid or duplicate registered adapter")
		}
		registry[ref] = adapter
	}
	out := PreparedPlan{Plan: clonePlan(p), Tasks: make([]TaskResult, len(p.Tasks)), reducers: make(map[int]TaskReducer)}
	for i, task := range out.Plan.Tasks {
		out.Tasks[i] = TaskResult{ID: task.ID, State: Admitted}
		if task.Adapter == nil {
			continue
		}
		adapter, ok := registry[*task.Adapter]
		if !ok {
			return bad("unsupported adapter or version")
		}
		// Prepare receives an independent copy of every mutable input field.
		input := clonePlan(Plan{Tasks: []Task{task}}).Tasks[0]
		prepared, err := adapter.Prepare(input)
		if err != nil {
			return PreparedPlan{}, fmt.Errorf("%w: adapter rejected task: %w", ErrInvalidPlan, err)
		}
		if nilInterface(prepared.Reducer) {
			return bad("adapter returned a nil reducer")
		}
		if len(prepared.Targets) == 0 {
			return bad("adapter returned no targets")
		}
		if len(prepared.Targets) > MaxTypedTargetsPerTask {
			return PreparedPlan{}, fmt.Errorf("%w: %w", ErrInvalidPlan, typedBudget("too many prepared targets"))
		}
		if !validObject(prepared.Params, MaxAdapterParamsBytes) {
			return bad("adapter returned invalid Core params")
		}
		out.Plan.Tasks[i].Params = bytes.Clone(prepared.Params)
		typed := &TypedResult{SchemaVersion: TypedSchemaVersion, Adapter: *task.Adapter}
		for _, id := range prepared.Targets {
			typed.Targets = append(typed.Targets, TargetResult{ID: id, Outcome: TargetUnobserved})
		}
		typed.State, typed.Code = typedState(typed)
		out.Tasks[i].Typed = typed
		out.reducers[i] = prepared.Reducer
	}
	if err := ValidatePlan(out.Plan); err != nil {
		return PreparedPlan{}, err
	}
	if err := validateTypedResults(out.Tasks); err != nil {
		return PreparedPlan{}, fmt.Errorf("%w: %w", ErrInvalidPlan, err)
	}
	return out, nil
}

func nilInterface(v any) bool {
	if v == nil {
		return true
	}
	r := reflect.ValueOf(v)
	switch r.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return r.IsNil()
	default:
		return false
	}
}

func validAdapterRef(ref AdapterRef) bool {
	return validText(ref.Name, 128, false) && ref.Version > 0
}

func cloneTypedResult(in *TypedResult) *TypedResult {
	if in == nil {
		return nil
	}
	out := *in
	out.Targets = append([]TargetResult(nil), in.Targets...)
	for i := range out.Targets {
		out.Targets[i].Details = bytes.Clone(out.Targets[i].Details)
	}
	out.Summary = bytes.Clone(in.Summary)
	return &out
}

func typedState(in *TypedResult) (State, string) {
	complete := len(in.Targets) > 0
	for _, target := range in.Targets {
		switch target.Outcome {
		case TargetFailed, TargetLocked, TargetNotFound:
			return Failed, "typed_failed"
		case TargetAchieved, TargetAlreadySet:
		default:
			complete = false
		}
	}
	if complete {
		return Succeeded, ""
	}
	return Unknown, "typed_incomplete"
}

func validOutcome(outcome TargetOutcome) bool {
	switch outcome {
	case TargetUnobserved, TargetAchieved, TargetReportedTrainingStarted,
		TargetReportedPrerequisiteStarted, TargetFailed, TargetAmbiguous,
		TargetAlreadySet, TargetConfirmationObserved, TargetLocked, TargetNotFound:
		return true
	default:
		return false
	}
}

func typedError(message string) error {
	return fmt.Errorf("%w: %s", ErrTypedResult, message)
}

func typedBudget(message string) error {
	return fmt.Errorf("%w: %w: %s", ErrTypedResult, ErrTypedBudget, message)
}

func validateTarget(target TargetResult) error {
	if !validText(target.ID, 128, false) || !validOutcome(target.Outcome) || !validText(target.Code, 128, true) {
		return typedError("invalid target identity, outcome or code")
	}
	if target.Outcome == TargetUnobserved && (target.Code != "" || len(target.Details) != 0) {
		return typedError("unobserved target contains a fact")
	}
	return validateTypedObject(target.Details, MaxTypedDetailBytes)
}

func validateTypedResult(in *TypedResult) error {
	if in == nil {
		return nil
	}
	if in.SchemaVersion != TypedSchemaVersion || !validAdapterRef(in.Adapter) {
		return typedError("unsupported typed schema or adapter identity")
	}
	if len(in.Targets) == 0 {
		return typedError("typed result requires targets")
	}
	if len(in.Targets) > MaxTypedTargetsPerTask {
		return typedBudget("too many task targets")
	}
	seen := make(map[string]bool, len(in.Targets))
	for _, target := range in.Targets {
		if seen[target.ID] {
			return typedError("duplicate target ID")
		}
		seen[target.ID] = true
		if err := validateTarget(target); err != nil {
			return err
		}
	}
	if err := validateTypedObject(in.Summary, MaxTypedSummaryBytes); err != nil {
		return err
	}
	state, code := typedState(in)
	if in.State != state || in.Code != code {
		return typedError("inconsistent typed aggregate")
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return typedError("cannot encode typed result")
	}
	if len(raw) > MaxTypedTaskBytes {
		return typedBudget("task result exceeds byte limit")
	}
	return nil
}

func validateTypedResults(tasks []TaskResult) error {
	count, size := 0, 0
	for _, task := range tasks {
		if err := validateTypedResult(task.Typed); err != nil {
			return err
		}
		if task.Typed == nil {
			continue
		}
		count += len(task.Typed.Targets)
		raw, _ := json.Marshal(task.Typed) // Validated above.
		size += len(raw)
		if count > MaxTypedTargetsPerRun || size > MaxTypedResultBytes {
			return typedBudget("run result exceeds count or byte limit")
		}
	}
	return nil
}

func validateTypedObject(raw json.RawMessage, limit int) error {
	if len(raw) == 0 {
		return nil
	}
	if len(raw) > limit {
		return typedBudget("typed object exceeds byte limit")
	}
	if !validObject(raw, uint64(limit)) {
		return typedError("typed detail or summary must be a JSON object")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	if err := checkTypedKeys(d); err != nil {
		return typedError("duplicate or malformed typed object field")
	}
	return nil
}

// Unlike control-key validation, every nested object is strict here, including
// fields named params or json. Numbers are tokenized without a float64 roundtrip.
func checkTypedKeys(d *json.Decoder) error {
	tok, err := d.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]bool)
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return err
			}
			name, ok := key.(string)
			if !ok || seen[name] {
				return ErrTypedResult
			}
			seen[name] = true
			if err := checkTypedKeys(d); err != nil {
				return err
			}
		}
	case '[':
		for d.More() {
			if err := checkTypedKeys(d); err != nil {
				return err
			}
		}
	default:
		return ErrTypedResult
	}
	_, err = d.Token()
	return err
}

func sameTarget(a, b TargetResult) bool {
	return a.ID == b.ID && a.Outcome == b.Outcome && a.Code == b.Code && bytes.Equal(a.Details, b.Details)
}

func applyTypedUpdate(current *TypedResult, update TypedUpdate) (*TypedResult, bool, error) {
	if current == nil {
		return nil, false, typedError("missing typed result")
	}
	if err := validateTypedResult(current); err != nil {
		return nil, false, err
	}
	if len(update.Targets) > MaxTypedTargetsPerTask {
		return nil, false, typedBudget("too many update targets")
	}
	if err := validateTypedObject(update.Summary, MaxTypedSummaryBytes); err != nil {
		return nil, false, err
	}
	out := cloneTypedResult(current)
	index := make(map[string]int, len(out.Targets))
	for i, target := range out.Targets {
		index[target.ID] = i
	}
	changed := false
	seen := make(map[string]bool, len(update.Targets))
	for _, target := range update.Targets {
		if err := validateTarget(target); err != nil {
			return nil, false, err
		}
		i, ok := index[target.ID]
		if !ok || seen[target.ID] {
			return nil, false, typedError("unknown or duplicate update target")
		}
		seen[target.ID] = true
		previous := out.Targets[i]
		if sameTarget(previous, target) {
			continue
		}
		if previous.Outcome != TargetUnobserved {
			return nil, false, fmt.Errorf("%w: %w", ErrTypedResult, ErrTypedConflict)
		}
		target.Details = bytes.Clone(target.Details)
		out.Targets[i] = target
		changed = true
	}
	if len(update.Summary) != 0 && !bytes.Equal(out.Summary, update.Summary) {
		if len(out.Summary) != 0 {
			return nil, false, fmt.Errorf("%w: %w", ErrTypedResult, ErrTypedConflict)
		}
		out.Summary = bytes.Clone(update.Summary)
		changed = true
	}
	out.State, out.Code = typedState(out)
	if err := validateTypedResult(out); err != nil {
		return nil, false, err
	}
	return out, changed, nil
}
