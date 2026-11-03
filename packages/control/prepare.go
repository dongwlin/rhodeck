package control

import (
	"bytes"
	"fmt"
	"reflect"

	"github.com/dongwlin/rhodeck/packages/control/internal/strictjson"
)

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
		input := clonePlan(Plan{Tasks: []Task{task}}).Tasks[0]
		prepared, err := adapter.Prepare(input)
		if err != nil {
			return PreparedPlan{}, fmt.Errorf("%w: adapter rejected task: %w", ErrInvalidPlan, err)
		}
		policy := prepared.Policy
		if policy == "" {
			policy = ObjectiveAll
		}
		switch policy {
		case ObjectiveNone:
			if len(prepared.Targets) != 0 || !nilInterface(prepared.Reducer) {
				return bad("input-only adapter returned targets or a reducer")
			}
		case ObjectiveAll, ObjectiveAny:
			if nilInterface(prepared.Reducer) {
				return bad("adapter returned a nil reducer")
			}
			if len(prepared.Targets) == 0 {
				return bad("adapter returned no targets")
			}
		default:
			return bad("adapter returned an unsupported objective policy")
		}
		if len(prepared.Targets) > MaxTypedTargetsPerTask {
			return PreparedPlan{}, fmt.Errorf("%w: %w", ErrInvalidPlan, typedBudget("too many prepared targets"))
		}
		if !strictjson.ValidObject(prepared.Params, MaxAdapterParamsBytes) {
			return bad("adapter returned invalid Core params")
		}
		out.Plan.Tasks[i].Params = bytes.Clone(prepared.Params)
		out.Tasks[i].Policy = policy
		if policy == ObjectiveNone {
			continue
		}
		typed := &TypedResult{SchemaVersion: SchemaVersion, Adapter: *task.Adapter, Policy: policy}
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
	return strictjson.ValidText(ref.Name, 128, false) && ref.Version > 0
}
