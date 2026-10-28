package protocol

import (
	"errors"
	"fmt"
	"regexp"
)

// Wire capacity budgets frozen for protocol v1. Every binding default must be
// wire-compatible (see CheckWireLimits); larger explicit configurations are
// rejected before the native library is touched or a transfer starts, never
// silently lowered and never delivered as a surprise after execution.
const (
	// MaxInputJSONBytes bounds a single raw JSON input on the wire (task
	// params, one connection extra) so that it always fits in one frame with
	// envelope and re-encoding margin. The binding default (1 MiB) fits.
	MaxInputJSONBytes = 4 << 20
	// MaxConfigBytes bounds the total text of one initialize configuration on
	// the wire. It equals the binding default; larger explicit Limits are
	// rejected at initialize.
	MaxConfigBytes = 4 << 20
	// MaxCallbackBytes bounds a single Core callback JSON on the wire. The
	// binding default (1 MiB) fits.
	MaxCallbackBytes = 4 << 20
	// MaxTaskElements bounds the task-list element capacity the wire carries
	// in a status response. It equals the binding default.
	MaxTaskElements = 1 << 16
	// ImageChunkBytes is the maximum raw byte length of one image chunk.
	// Base64 expansion keeps every chunk response frame well below
	// MaxFrameBytes.
	ImageChunkBytes = 4 << 20
	// ImageTransferMaxBytes is the per-transfer ceiling. It carries the
	// binding default image maxima (PNG 32 MiB, BGR 64 MiB) and every legal
	// explicit maximum (PNG ≤ 64 MiB, BGR ≤ 128 MiB).
	ImageTransferMaxBytes = 128 << 20
	// ImageTransfersMax bounds concurrently retained image transfers; the
	// aggregate in-flight image memory is at most
	// ImageTransfersMax * ImageTransferMaxBytes on each side of the pipe.
	ImageTransfersMax = 2
)

// Limits is the wire form of one complete binding limit configuration. A nil
// *Limits in an InitializeRequest selects the whole default set; a non-nil
// Limits is explicit and complete — no field is individually backfilled from
// defaults, and zero capacities are rejected by the worker's validation.
type Limits struct {
	InputStringBytes  uint64       `json:"input_string_bytes"`
	InputJSONBytes    uint64       `json:"input_json_bytes"`
	ConfigEntries     uint64       `json:"config_entries"`
	ConfigBytes       uint64       `json:"config_bytes"`
	ImagePNG          BufferLimits `json:"image_png"`
	ImageBGR          BufferLimits `json:"image_bgr"`
	UUID              BufferLimits `json:"uuid"`
	Tasks             BufferLimits `json:"tasks"`
	CallbackBytes     uint64       `json:"callback_bytes"`
	InternalEvents    uint64       `json:"internal_events"`
	InternalBytes     uint64       `json:"internal_bytes"`
	PublicEvents      uint64       `json:"public_events"`
	PublicBytes       uint64       `json:"public_bytes"`
	PendingOperations uint64       `json:"pending_operations"`
	EarlyResults      uint64       `json:"early_results"`
	EarlyBytes        uint64       `json:"early_bytes"`
}

// BufferLimits is the wire form of one output-buffer capacity probe strategy.
// The unit depends on the using Limits field (bytes or elements).
type BufferLimits struct {
	Initial       uint64 `json:"initial"`
	Max           uint64 `json:"max"`
	GrowthRetries uint32 `json:"growth_retries"`
}

// CheckWireLimits rejects explicit limit configurations whose single-unit
// maxima could produce a result that the frozen transport could not deliver.
// It is called by the worker before the native library is touched, so an
// incompatible configuration fails with limit_exceeded and the effective
// maximum instead of a mid-session delivery failure. Binding's own range and
// cross-field validation happens in the worker's native layer and is not
// duplicated here.
func CheckWireLimits(l *Limits) error {
	tooLarge := func(field string, value, maximum uint64) error {
		return &Error{
			Code:    LimitExceeded,
			Message: fmt.Sprintf("%s (%d) exceeds the wire budget of %d bytes", field, value, maximum),
			Detail:  &ErrorDetail{Kind: "transport_budget", Field: field, Unit: "bytes", Capacity: value, Maximum: maximum},
		}
	}
	switch {
	case l.InputJSONBytes > MaxInputJSONBytes:
		return tooLarge("Limits.InputJSONBytes", l.InputJSONBytes, MaxInputJSONBytes)
	case l.ConfigBytes > MaxConfigBytes:
		return tooLarge("Limits.ConfigBytes", l.ConfigBytes, MaxConfigBytes)
	case l.CallbackBytes > MaxCallbackBytes:
		return tooLarge("Limits.CallbackBytes", l.CallbackBytes, MaxCallbackBytes)
	case l.ImagePNG.Max > ImageTransferMaxBytes:
		return tooLarge("Limits.ImagePNG.Max", l.ImagePNG.Max, ImageTransferMaxBytes)
	case l.ImageBGR.Max > ImageTransferMaxBytes:
		return tooLarge("Limits.ImageBGR.Max", l.ImageBGR.Max, ImageTransferMaxBytes)
	}
	if l.Tasks.Max > MaxTaskElements {
		return &Error{
			Code:    LimitExceeded,
			Message: fmt.Sprintf("Limits.Tasks.Max (%d elements) exceeds the wire budget of %d elements", l.Tasks.Max, uint64(MaxTaskElements)),
			Detail:  &ErrorDetail{Kind: "transport_budget", Field: "Limits.Tasks.Max", Unit: "elements", Capacity: l.Tasks.Max, Maximum: MaxTaskElements},
		}
	}
	return nil
}

// PointerFromUint64 converts a wire integer to platform pointer width. The
// worker must use it (never a float64 round trip) before handing a value such
// as a window handle to the native layer; ok is false when v exceeds the
// platform pointer range.
func PointerFromUint64(v uint64) (uintptr, bool) {
	if width := uint64(^uintptr(0)); v > width {
		return 0, false
	}
	return uintptr(v), true
}

// UUIDResult carries the device UUID. The empty string is a normal result, not
// an error.
type UUIDResult struct {
	UUID string `json:"uuid"`
}

// ClickRequest submits a click at device coordinates.
type ClickRequest struct {
	X int32 `json:"x"`
	Y int32 `json:"y"`
}

// AttachWindowRequest binds a window. Hwnd travels as an unsigned 64-bit
// integer and is converted to platform pointer width by the worker after
// validation. The three method fields are uint64 bit masks passed through
// verbatim; the binding decides which combinations are valid.
type AttachWindowRequest struct {
	Hwnd            uint64 `json:"hwnd"`
	ScreencapMethod uint64 `json:"screencap_method"`
	MouseMethod     uint64 `json:"mouse_method"`
	KeyboardMethod  uint64 `json:"keyboard_method"`
}

// ImageTransfer is the response of get_image_png/get_image_bgr. It describes
// one owned snapshot of the cached image: TotalBytes and SHA256 describe the
// snapshot exactly, and later cache updates never change an in-flight
// transfer. TotalBytes may be zero (an empty cached image is a normal
// result); such a transfer is complete at acceptance and needs no chunks.
type ImageTransfer struct {
	TransferID uint64 `json:"transfer_id"`
	TotalBytes uint64 `json:"total_bytes"`
	SHA256     string `json:"sha256"`
	ChunkBytes uint32 `json:"chunk_bytes"`
}

var sha256Pattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate checks an ImageTransfer the client received before it allocates or
// trusts anything derived from it. The total length is bounded by the frozen
// per-transfer budget: a peer claiming more could never have accepted the
// transfer, so allocating for it would be trusting unbounded input.
func (t *ImageTransfer) Validate() error {
	if t.TransferID == 0 {
		return errors.New("worker: transfer_id must be nonzero")
	}
	if t.TotalBytes > ImageTransferMaxBytes {
		return fmt.Errorf("worker: total_bytes must be within [0, %d]", ImageTransferMaxBytes)
	}
	if t.ChunkBytes == 0 || t.ChunkBytes > ImageChunkBytes {
		return fmt.Errorf("worker: chunk_bytes must be within (0, %d]", ImageChunkBytes)
	}
	if !sha256Pattern.MatchString(t.SHA256) {
		return errors.New("worker: sha256 must be 64 lowercase hex digits")
	}
	return nil
}

// ImageChunkRequest fetches one chunk of a transfer. Length must be within
// [1, ChunkBytes] of the transfer and Offset+Length must not exceed
// TotalBytes. Requests may use any offset order; the supervisor assembles and
// verifies the whole payload before delivering it. Chunk traffic never enters
// the event stream or its sequence domain.
type ImageChunkRequest struct {
	TransferID uint64 `json:"transfer_id"`
	Offset     uint64 `json:"offset"`
	Length     uint32 `json:"length"`
}

// Validate checks one chunk request against its transfer.
func (r *ImageChunkRequest) Validate(t *ImageTransfer) error {
	if r.TransferID == 0 || r.TransferID != t.TransferID {
		return errors.New("worker: chunk request does not reference the transfer")
	}
	if r.Length == 0 || uint64(r.Length) > uint64(t.ChunkBytes) {
		return fmt.Errorf("worker: chunk length must be within (0, %d]", t.ChunkBytes)
	}
	if r.Offset > t.TotalBytes || uint64(r.Length) > t.TotalBytes-r.Offset {
		return errors.New("worker: chunk request exceeds the transfer range")
	}
	return nil
}

// ImageChunkResponse is the response of image_chunk. Data is the standard base64
// encoding of exactly the requested length; the worker serves bytes from its
// owned snapshot.
type ImageChunkResponse struct {
	TransferID uint64 `json:"transfer_id"`
	Offset     uint64 `json:"offset"`
	Data       string `json:"data"`
}

// AdmitImageTransfer is the pre-submission capacity gate for a new transfer of
// totalBytes with active transfers already retained. It returns the wire error
// to answer with; the caller rejects the command without reading or holding
// any image bytes. The structured reason carries the effective maximum.
func AdmitImageTransfer(totalBytes uint64, active int) error {
	if active >= ImageTransfersMax {
		return &Error{
			Code:    LimitExceeded,
			Message: fmt.Sprintf("%d image transfers are already in flight, the limit is %d", active, ImageTransfersMax),
			Detail:  &ErrorDetail{Kind: "transport_budget", Buffer: "image_transfer", Unit: "transfers", Capacity: uint64(active), Maximum: ImageTransfersMax},
		}
	}
	if totalBytes > ImageTransferMaxBytes {
		return &Error{
			Code:    LimitExceeded,
			Message: fmt.Sprintf("cached image of %d bytes exceeds the transfer budget of %d bytes", totalBytes, ImageTransferMaxBytes),
			Detail:  &ErrorDetail{Kind: "transport_budget", Buffer: "image_transfer", Unit: "bytes", Capacity: totalBytes, Maximum: ImageTransferMaxBytes},
		}
	}
	return nil
}

// LogRequest feeds the process-level native log. Empty strings are valid
// inputs; the empty-object response only means the call returned.
type LogRequest struct {
	Level   string `json:"level"`
	Message string `json:"message"`
}

// MapLevelKeyRequest queries the experiment mapping by level key. The key is
// passed through verbatim, including the empty string.
type MapLevelKeyRequest struct {
	Key string `json:"key"`
}

// MapLevelKeyResult is the four-field level lookup result. Zero values mean
// "not found" and are normal results, not errors.
type MapLevelKeyResult struct {
	StageID string `json:"stage_id"`
	Code    string `json:"code"`
	LevelID string `json:"level_id"`
	Name    string `json:"name"`
}

// ItemNameRequest queries the experiment item name by item id.
type ItemNameRequest struct {
	ID string `json:"id"`
}

// ItemNameResult carries the item name. The empty string means "not found"
// and is a normal result, not an error.
type ItemNameResult struct {
	Name string `json:"name"`
}

// FaultNotice is the payload of the fault event: the first-instance-fault
// reason with its stable code. Delivery is ordered by the session event
// sequence; it is either delivered or the session fails — it is never
// silently dropped to make room for other traffic.
type FaultNotice struct {
	Error Error `json:"error"`
}

// InstanceDiagnostics is the payload of the diagnostics event: the bounded
// diagnostics snapshot of the native instance, sent once when the instance
// reaches its terminal state. It is the wire form of the binding's snapshot:
// LostEvents saturates at math.MaxUint64, FirstFault is absent on a normal
// close, StopDiagnostic may exist without a fault, and RawPrefix is the
// standard base64 encoding of up to 4096 raw bytes that may be neither valid
// UTF-8 nor valid JSON (absent when no raw prefix was kept).
type InstanceDiagnostics struct {
	LostEvents     uint64 `json:"lost_events"`
	FirstFault     *Error `json:"first_fault,omitempty"`
	RawPrefix      string `json:"raw_prefix,omitempty"`
	StopDiagnostic *Error `json:"stop_diagnostic,omitempty"`
}
