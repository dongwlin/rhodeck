// Package protocol defines the versioned Core worker wire contract. Protocol
// v1 is frozen as a whole: the command/event tables, DTO shapes, numeric
// encoding, error codes, and transport budgets in this package are the
// contract. Any later change that a v1 peer could not parse — added, removed,
// renamed, or retyped fields, commands, events, or codes; framing or sequence
// changes — requires a protocol version bump, because decoding is strict and
// peers reject unknown control fields instead of ignoring them. It has no
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
	Initialize       = "initialize"
	Connect          = "connect"
	AppendTask       = "append_task"
	SetTaskParams    = "set_task_params"
	Start            = "start"
	Stop             = "stop"
	Status           = "status"
	Ping             = "ping"
	Close            = "close"
	BackToHome       = "back_to_home"
	UUID             = "uuid"
	Click            = "click"
	Screencap        = "screencap"
	AttachWindow     = "attach_window"
	GetImagePNG      = "get_image_png"
	GetImageBGR      = "get_image_bgr"
	ImageChunk       = "image_chunk"
	Log              = "log"
	MapLevelKey      = "map_level_key"
	ItemName         = "item_name"
	CoreEvent        = "core"
	OperationEvent   = "operation"
	FaultEvent       = "fault"
	DiagnosticsEvent = "diagnostics"
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
// Detail optionally carries a bounded, machine-discriminable structured reason
// (for example which buffer limit was hit and its effective maximum); it never
// replaces the Code.
type Error struct {
	Code    string       `json:"code"`
	Message string       `json:"message"`
	Detail  *ErrorDetail `json:"detail,omitempty"`
}

func (e *Error) Error() string { return fmt.Sprintf("worker: %s: %s", e.Code, e.Message) }

// ErrorDetail bounds the structured reason carried by an Error. Kind selects
// the reason family (for example "buffer_limit", "operation_limit",
// "transport_budget"); the remaining fields qualify it. Omitted numeric fields
// are absent, not zero-valued facts.
type ErrorDetail struct {
	Kind     string `json:"kind,omitempty"`
	Buffer   string `json:"buffer,omitempty"`
	Unit     string `json:"unit,omitempty"`
	Field    string `json:"field,omitempty"`
	Attempts uint64 `json:"attempts,omitempty"`
	Capacity uint64 `json:"capacity,omitempty"`
	Maximum  uint64 `json:"maximum,omitempty"`
}

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
// Limits == nil selects the binding's whole default limit set; a non-nil Limits
// is one complete explicit configuration — fields are never individually filled
// from defaults, and configurations that exceed the wire budgets frozen in this
// package are rejected before the native library is touched.
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
	Limits                *Limits           `json:"limits,omitempty"`
}

type Resource struct {
	Root          string `json:"root"`
	VersionSHA256 string `json:"version_sha256,omitempty"`
}

// Capabilities reports which optional symbols the loaded native library
// exports. It describes native availability only: it never claims that a wire
// command is implemented, that a task type is accepted, or that an operation
// will succeed on the current installation.
type Capabilities struct {
	ImageBGR     bool `json:"image_bgr"`
	AttachWindow bool `json:"attach_window"`
	MapLevelKey  bool `json:"map_level_key"`
	ItemName     bool `json:"item_name"`
}

// RuntimeInfo is the immutable metadata copied at initialization. Limits is the
// effective (post-validation) limit set the worker applies, and NullSize is the
// native output-buffer capacity sentinel (locked profile: math.MaxUint64).
type RuntimeInfo struct {
	SnapshotID    string       `json:"snapshot_id"`
	CoreVersion   string       `json:"core_version"`
	LibrarySHA256 string       `json:"library_sha256"`
	Resources     []Resource   `json:"resources"`
	Capabilities  Capabilities `json:"capabilities"`
	Limits        *Limits      `json:"limits,omitempty"`
	NullSize      uint64       `json:"null_size,omitempty"`
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
