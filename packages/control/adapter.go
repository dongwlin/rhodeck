package control

import (
	"encoding/json"
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

// AdapterRef explicitly selects one locally registered adapter version.
// Recognizing Task.Type never enables an adapter on the generic input path.
type AdapterRef struct {
	Name    string `json:"name"`
	Version int    `json:"version"`
}

// ObjectivePolicy selects how independent target facts establish a task's
// objective. It never establishes the native task-chain or cleanup result.
type ObjectivePolicy string

const (
	ObjectiveAll  ObjectivePolicy = "all"
	ObjectiveAny  ObjectivePolicy = "any"
	ObjectiveNone ObjectivePolicy = "none"
)

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
// An omitted Policy defaults to ObjectiveAll. ObjectiveNone is input-only and
// requires no targets or reducer; the runtime retains only native chain facts.
type PreparedTask struct {
	Params  json.RawMessage
	Policy  ObjectivePolicy
	Targets []string
	Reducer TaskReducer
}

// TaskReducer observes only callbacks associated with its submitted Core task.
// Returning an empty update ignores a callback. Reducers must bound their own
// scratch state; the runtime bounds every retained result and emitted update.
type TaskReducer interface {
	Observe(Observation) (TypedUpdate, error)
}

// UnassociatedObservationClassifier optionally recognizes exact non-factual
// progress callbacks emitted before Core assigns a task ID. The runtime calls
// it only for unambiguous callbacks with an explicit taskid of zero. Returning
// true permits that observation without creating target facts or changing the
// association requirements for execution callbacks. Implementations must be
// side-effect free and must not change their reducer's active execution scope.
type UnassociatedObservationClassifier interface {
	IsUnassociatedProgress(Observation) bool
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
