// Package protocol defines the versioned Core worker wire contract. It has no
// dependencies on the native binding, application services, or process manager.
package protocol

import (
	"encoding/json"
	"fmt"
)

const (
	Version       = 1
	MaxFrameBytes = 8 << 20 // Excludes the terminating newline.
	QueueMessages = 64
	QueueBytes    = 16 << 20
	RequestQueue  = 16
)

const (
	Hello    = "hello"
	Request  = "request"
	Response = "response"
	Event    = "event"
)

const (
	Initialize     = "initialize"
	Connect        = "connect"
	AppendTask     = "append_task"
	SetTaskParams  = "set_task_params"
	Start          = "start"
	Stop           = "stop"
	Status         = "status"
	Ping           = "ping"
	Close          = "close"
	CoreEvent      = "core"
	OperationEvent = "operation"
)

const (
	InvalidInput       = "invalid_input"
	InvalidState       = "invalid_state"
	NativeRejected     = "native_rejected"
	Unsupported        = "unsupported"
	LoadFailed         = "load_failed"
	SymbolMissing      = "symbol_missing"
	RegistrationFailed = "registration_failed"
	SnapshotMismatch   = "snapshot_mismatch"
	LimitExceeded      = "limit_exceeded"
	EventOverflow      = "event_overflow"
	ProtocolError      = "protocol_error"
	InternalError      = "internal_error"
)

// Error is a stable code plus bounded diagnostic text. Consumers switch on Code,
// never parse Message. It describes a command or operation, not a task outcome.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("worker: %s: %s", e.Code, e.Message) }

// Message is one JSONL frame. Request IDs increase strictly within a session.
// Events use their own contiguous sequence; Core callback sequences are retained
// separately in Callback. Payload stays raw to preserve unknown fields/numbers.
type Message struct {
	Version  int             `json:"version"`
	Type     string          `json:"type"`
	ID       uint64          `json:"id,omitempty"`
	Name     string          `json:"name,omitempty"`
	Sequence uint64          `json:"sequence,omitempty"`
	Payload  json.RawMessage `json:"payload,omitempty"`
	Error    *Error          `json:"error,omitempty"`
}

type Greeting struct {
	WorkerVersion string `json:"worker_version"`
	SessionID     string `json:"session_id"`
	OS            string `json:"os"`
	Arch          string `json:"arch"`
	MaxFrameBytes int    `json:"max_frame_bytes"`
	QueueMessages int    `json:"queue_messages"`
	QueueBytes    int    `json:"queue_bytes"`
}

type Option struct {
	Key   int32  `json:"key"`
	Value string `json:"value"`
}

type ConnectionExtra struct {
	Name string          `json:"name"`
	JSON json.RawMessage `json:"json"`
}

// InitializeRequest selects a caller-owned, immutable installation. SnapshotID
// identifies that installation; it is not a claim that its whole tree was hashed.
// The library and available resource/version.json files are hashed by the worker.
type InitializeRequest struct {
	SnapshotID            string            `json:"snapshot_id"`
	LibraryPath           string            `json:"library_path"`
	UserDir               string            `json:"user_dir"`
	ResourceRoots         []string          `json:"resource_roots"`
	ExpectedCoreVersion   string            `json:"expected_core_version,omitempty"`
	ExpectedLibrarySHA256 string            `json:"expected_library_sha256,omitempty"`
	StaticOptions         []Option          `json:"static_options,omitempty"`
	InstanceOptions       []Option          `json:"instance_options,omitempty"`
	ConnectionExtras      []ConnectionExtra `json:"connection_extras,omitempty"`
}

type Resource struct {
	Root          string `json:"root"`
	VersionSHA256 string `json:"version_sha256,omitempty"`
}

type Capabilities struct {
	ImageBGR     bool `json:"image_bgr"`
	AttachWindow bool `json:"attach_window"`
	MapLevelKey  bool `json:"map_level_key"`
	ItemName     bool `json:"item_name"`
}

type RuntimeInfo struct {
	SnapshotID    string       `json:"snapshot_id"`
	CoreVersion   string       `json:"core_version"`
	LibrarySHA256 string       `json:"library_sha256"`
	Resources     []Resource   `json:"resources"`
	Capabilities  Capabilities `json:"capabilities"`
}

type ConnectRequest struct {
	ADBPath string `json:"adb_path"`
	Address string `json:"address"`
	Config  string `json:"config"`
}

type TaskRequest struct {
	Type   string          `json:"type"`
	Params json.RawMessage `json:"params"`
}

type TaskParamsRequest struct {
	TaskID int32           `json:"task_id"`
	Params json.RawMessage `json:"params"`
}

type TaskAccepted struct {
	TaskID int32 `json:"task_id"`
}
type OperationAccepted struct {
	AsyncID int32 `json:"async_id"`
}

// ExecutionStatus is a native observation. Running=false does not imply success.
type ExecutionStatus struct {
	Running   bool    `json:"running"`
	Connected bool    `json:"connected"`
	Tasks     []int32 `json:"tasks"`
}

type StopResult struct {
	Stopped bool `json:"stopped"`
}

// Callback preserves all valid native messages, including unknown message IDs.
// Task and async IDs, when present, remain in the raw JSON with original numbers.
type Callback struct {
	MessageID    int32           `json:"message_id"`
	CoreSequence uint64          `json:"core_sequence"`
	JSON         json.RawMessage `json:"json"`
}

type OperationResult struct {
	RequestID uint64          `json:"request_id"`
	AsyncID   int32           `json:"async_id"`
	What      string          `json:"what"`
	OK        bool            `json:"ok"`
	Details   json.RawMessage `json:"details,omitempty"`
	Error     *Error          `json:"error,omitempty"`
}
