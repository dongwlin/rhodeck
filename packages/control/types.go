// Package control owns one business run at a time, its worker, observations,
// durable record and cleanup. It never loads native code or replays device work.
package control

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// SchemaVersion is the unchanged generic plan and machine envelope version.
const SchemaVersion = 1

// AdaptedSchemaVersion identifies explicit adapter plans and their containers.
const AdaptedSchemaVersion = 2

// TypedSchemaVersion versions the nested typed result independently.
const TypedSchemaVersion = 1

const (
	MaxPlanBytes = 16 << 20
	MaxTasks     = 128
)

var (
	ErrBusy        = errors.New("control: runtime or data directory is busy")
	ErrClosed      = errors.New("control: runtime is closed")
	ErrInvalidPlan = errors.New("control: invalid plan")
)

type State string

const (
	Admitted   State = "admitted"
	Connecting State = "connecting"
	Submitted  State = "submitted"
	Started    State = "started"
	Stopping   State = "stopping"
	Succeeded  State = "succeeded"
	Failed     State = "failed"
	Stopped    State = "stopped"
	Rejected   State = "rejected"
	Unknown    State = "unknown"
)

// Task.ID is caller-owned and unique within a plan; it is never a Core task ID.
type Task struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"`
	Params  json.RawMessage `json:"params"`
	Adapter *AdapterRef     `json:"adapter,omitempty"`
}

// Plan is a complete explicit installation, connection and ordered task batch.
// HostRunID is opaque correlation metadata, never used as a filesystem path.
type Plan struct {
	SchemaVersion int                        `json:"schema_version"`
	HostRunID     string                     `json:"host_run_id,omitempty"`
	Initialize    protocol.InitializeRequest `json:"initialize"`
	Connection    protocol.ConnectRequest    `json:"connection"`
	Tasks         []Task                     `json:"tasks"`
}

type Config struct {
	DataDir          string
	Worker           worker.Config
	StopTimeout      time.Duration // Zero selects five seconds; negative is rejected.
	IncludeRawEvents bool          // Explicit local diagnostic opt-in; may contain private data.
	Adapters         []TaskAdapter // Explicit, runtime-local adapters; none are selected by task name.
}

// TaskResult.State is the native task-chain fact. Typed carries the independent
// business target facts used when deciding an adapted run's overall result.
type TaskResult struct {
	ID         string       `json:"id"`
	CoreTaskID int32        `json:"core_task_id,omitempty"`
	State      State        `json:"state"`
	Typed      *TypedResult `json:"typed,omitempty"`
}

// Result separates observed task facts from cleanup. Succeeded with a failed
// cleanup is not a successful CLI exit. Codes are stable; diagnostic text is not.
type Result struct {
	SchemaVersion int                   `json:"schema_version"`
	RunID         string                `json:"run_id"`
	HostRunID     string                `json:"host_run_id,omitempty"`
	State         State                 `json:"state"`
	Code          string                `json:"code,omitempty"`
	Cleanup       string                `json:"cleanup"`
	CleanupCode   string                `json:"cleanup_code,omitempty"`
	StartedAt     time.Time             `json:"started_at"`
	FinishedAt    time.Time             `json:"finished_at,omitempty"`
	Tasks         []TaskResult          `json:"tasks"`
	RuntimeInfo   *protocol.RuntimeInfo `json:"runtime_info,omitempty"`
}

// Event is a bounded observation stream. A caller must consume Events and Done
// together; Snapshot remains authoritative if its consumer falls behind.
type Event struct {
	SchemaVersion int             `json:"schema_version"`
	RunID         string          `json:"run_id"`
	HostRunID     string          `json:"host_run_id,omitempty"`
	Sequence      uint64          `json:"sequence"`
	Time          time.Time       `json:"time"`
	Type          string          `json:"type"`
	State         State           `json:"state,omitempty"`
	TaskID        string          `json:"task_id,omitempty"`
	CoreTaskID    int32           `json:"core_task_id,omitempty"`
	MessageID     int32           `json:"message_id"`
	CoreSequence  uint64          `json:"core_sequence,omitempty"`
	Details       json.RawMessage `json:"details,omitempty"`
	Code          string          `json:"code,omitempty"`
	Typed         *TypedResult    `json:"typed,omitempty"`
}

// Record is private local state. Plans may contain credentials or device data;
// neither records nor raw callbacks are automatically forwarded to diagnostics.
type Record struct {
	SchemaVersion int    `json:"schema_version"`
	Plan          Plan   `json:"plan"`
	Result        Result `json:"result"`
}
