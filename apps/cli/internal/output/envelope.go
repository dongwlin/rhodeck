// Package output owns the machine output contract of the CLI: the envelope
// types, the single stdout writer, the diagnostic line and the process exit
// codes. It depends on the control package for the domain facts it carries and
// on nothing else.
package output

import (
	"github.com/dongwlin/rhodeck/packages/control"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// SchemaVersion is the version of the machine envelopes. It follows the
// control data format.
const SchemaVersion = control.SchemaVersion

// ProtocolVersion is the version of the worker protocol the CLI talks to.
const ProtocolVersion = protocol.Version

// The envelope kinds. Every machine object carries exactly one of them in its
// type field.
const (
	KindError        = "error"
	KindHelp         = "help"
	KindVersion      = "version"
	KindCapabilities = "capabilities"
	KindPlan         = "plan"
	KindDoctor       = "doctor"
	KindTerminal     = "terminal"
)

// Header is the common prefix of every machine envelope.
type Header struct {
	SchemaVersion int    `json:"schema_version"`
	Type          string `json:"type"`
}

// NewHeader builds the header of one envelope kind.
func NewHeader(kind string) Header {
	return Header{SchemaVersion: SchemaVersion, Type: kind}
}

// Error reports a rejected or failed command.
type Error struct {
	Header
	Code string `json:"code"`
}

// NewError builds the error envelope of one stable diagnostic code.
func NewError(code string) *Error {
	return &Error{Header: NewHeader(KindError), Code: code}
}

// Help carries the help text of one command.
type Help struct {
	Header
	Usage string `json:"usage"`
}

// Version reports the CLI build version.
type Version struct {
	Header
	Version string `json:"version"`
}

// Capabilities reports the platform, the worker protocol version, the
// registered task adapters and the task input contract. Device scope stays in
// the adapter rows that observed it; the report carries no top-level summary
// or review basis that could outlive the evidence behind it.
type Capabilities struct {
	Header
	Version             string       `json:"cli_version"`
	ProtocolVersion     int          `json:"worker_protocol_version"`
	OS                  string       `json:"os"`
	Arch                string       `json:"arch"`
	Commands            []string     `json:"commands"`
	TaskInput           string       `json:"task_input"`
	TaskAdapters        []AdapterRow `json:"task_adapters"`
	CoreVersionPolicy   string       `json:"core_version_policy"`
	InstallationFacts   string       `json:"installation_facts"`
	NativeTaskDiscovery bool         `json:"native_task_discovery"`
	TaskAcceptance      string       `json:"task_acceptance"`
}

// AdapterRow describes one registered adapter in machine output. The name and
// version come from the adapter descriptor, the remaining fields document what
// the runner accepts, which objective policies the inputs can select and which
// evidence is still missing.
type AdapterRow struct {
	Name                 string                    `json:"name"`
	Version              int                       `json:"version"`
	CoreType             string                    `json:"core_type"`
	Input                string                    `json:"input"`
	ObjectivePolicies    []control.ObjectivePolicy `json:"objective_policies"`
	PolicySelection      string                    `json:"policy_selection"`
	ResultInterpretation string                    `json:"result_interpretation"`
	Acceptance           string                    `json:"acceptance"`
}

// Sources records where one plan came from.
type Sources struct {
	Input   string `json:"input"`
	Worker  string `json:"worker"`
	DataDir string `json:"data_dir"`
}

// Plan previews one prepared plan.
type Plan struct {
	Header
	Plan       *control.Plan        `json:"plan"`
	Tasks      []control.TaskResult `json:"tasks"`
	Sources    *Sources             `json:"sources"`
	Unverified []string             `json:"unverified"`
}

// Check is one local installation check.
type Check struct {
	Field string `json:"field"`
	OK    bool   `json:"ok"`
	Code  string `json:"code,omitempty"`
}

// Doctor reports the explicit installation.
type Doctor struct {
	Header
	Code        string                `json:"code,omitempty"`
	Unverified  []string              `json:"unverified,omitempty"`
	RuntimeInfo *protocol.RuntimeInfo `json:"runtime_info,omitempty"`
	Checks      []Check               `json:"checks,omitempty"`
}

// Terminal carries the final result of one run. It is written only after the
// run finished, its facts were persisted and the runtime released its
// directory lock.
type Terminal struct {
	Header
	Result *control.Result `json:"result"`
}
