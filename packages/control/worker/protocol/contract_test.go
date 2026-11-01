package protocol

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// roundTrip encodes m as one frame, reads it back, and decodes its payload
// into target.
func roundTrip(t *testing.T, m Message, target any) {
	t.Helper()
	data, err := Encode(m)
	require.NoError(t, err)
	got, err := NewReader(bytes.NewReader(data)).Read()
	require.NoError(t, err)
	if target == nil {
		return
	}
	require.NoError(t, Decode(got.Payload, target))
}

// TestNumericVectors freezes the lossless integer encoding: every control
// number is a typed Go integer decoded straight from the JSON integer literal,
// never via float64. Values above 2^53 stay exact, MaxUint64 survives, and
// out-of-range or non-integer forms are rejected instead of wrapped.
func TestNumericVectors(t *testing.T) {
	for name, v := range map[string]uint64{
		"2^53+1": 9007199254740993,
		"2^60":   1152921504606846976,
		"MaxU64": math.MaxUint64,
		"mask":   (1 << 10) | (1 << 2) | 1,
		"small":  42,
	} {
		t.Run(name, func(t *testing.T) {
			m, err := NewMessage(Request, Ping, v, nil)
			require.NoError(t, err)
			data, err := Encode(m)
			require.NoError(t, err)
			// The literal must appear verbatim: 2^53+1 may not become
			// 9007199254740992 through any float rounding.
			require.True(t, bytes.Contains(data, []byte(uint64Text(v))), "literal %s not on the wire: %s", uint64Text(v), data)
			got, err := NewReader(bytes.NewReader(data)).Read()
			require.NoError(t, err)
			require.Equal(t, v, got.ID, "request ID %d came back as %d", v, got.ID)

			e := Message{Version: Version, Type: Event, Name: CoreEvent, Sequence: v}
			require.NoError(t, e.Validate())
			data, err = Encode(e)
			require.NoError(t, err)
			got, err = NewReader(bytes.NewReader(data)).Read()
			require.NoError(t, err)
			require.Equal(t, v, got.Sequence, "event sequence %d came back as %d", v, got.Sequence)
		})
	}

	for name, raw := range map[string]string{
		"above uint64": `{"version":1,"type":"request","id":18446744073709551616,"name":"ping"}` + "\n",
		"float form":   `{"version":1,"type":"request","id":1.5,"name":"ping"}` + "\n",
		"exponent":     `{"version":1,"type":"request","id":1e3,"name":"ping"}` + "\n",
		"negative":     `{"version":1,"type":"request","id":-1,"name":"ping"}` + "\n",
	} {
		t.Run("reject "+name, func(t *testing.T) {
			_, err := NewReader(strings.NewReader(raw)).Read()
			require.Error(t, err, "%s accepted", name)
		})
	}
}

func uint64Text(v uint64) string {
	data, _ := json.Marshal(v)
	return string(data)
}

// TestRawMessageBytePreservation freezes the opaque-payload policy: nested
// unknown fields, duplicate keys, and arbitrary precision inside RawMessage
// members are native schema, preserved byte for byte. Only the control object
// itself is strict.
func TestRawMessageBytePreservation(t *testing.T) {
	raw := json.RawMessage(`{"dup":1,"dup":2,"big":90071992547409931234567890,"nested":{"x":null},"neg":-18446744073709551615}`)
	req := InitializeRequest{
		SnapshotID: "s",
		ConnectionExtras: []ConnectionExtra{
			{Name: "extra", JSON: raw},
		},
	}
	m, err := NewMessage(Request, Initialize, 7, req)
	require.NoError(t, err)
	var got InitializeRequest
	roundTrip(t, m, &got)
	require.True(t, bytes.Equal(got.ConnectionExtras[0].JSON, raw), "raw extra changed:\n got %s\nwant %s", got.ConnectionExtras[0].JSON, raw)
}

// TestEmptyValuesAreNormal freezes that legal empty observations never become
// errors at the codec layer.
func TestEmptyValuesAreNormal(t *testing.T) {
	cases := []struct {
		name   string
		m      Message
		target any
	}{
		{"status all empty", mustMessage(t, Response, Status, 1, ExecutionStatus{Tasks: []int32{}}), &ExecutionStatus{}},
		{"empty uuid", mustMessage(t, Response, UUID, 1, UUIDResult{UUID: ""}), &UUIDResult{}},
		{"empty level key", mustMessage(t, Response, MapLevelKey, 1, MapLevelKeyResult{}), &MapLevelKeyResult{}},
		{"empty item name", mustMessage(t, Response, ItemName, 1, ItemNameResult{Name: ""}), &ItemNameResult{}},
		{"empty params", mustMessage(t, Request, AppendTask, 1, TaskRequest{Type: "t", Params: json.RawMessage(`{}`)}), &TaskRequest{}},
		{"empty cached image", mustMessage(t, Response, GetImagePNG, 1, ImageTransfer{TransferID: 1, SHA256: sha256Hex(nil), ChunkBytes: ImageChunkBytes}), &ImageTransfer{}},
		{"empty log", mustMessage(t, Request, Log, 1, LogRequest{Level: "", Message: ""}), &LogRequest{}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.NoError(t, c.m.Validate())
			roundTrip(t, c.m, c.target)
		})
	}
}

// TestErrorDetailContract freezes the structured reason: it is optional,
// bounded, and carries machine-discriminable fields; the code remains the only
// control-plane discriminator.
func TestErrorDetailContract(t *testing.T) {
	m := Message{Version: Version, Type: Response, ID: 1, Name: Ping,
		Error: &Error{Code: LimitExceeded, Message: "boom", Detail: &ErrorDetail{Kind: "transport_budget", Field: "Limits.InputJSONBytes", Unit: "bytes", Capacity: 5 << 20, Maximum: MaxInputJSONBytes}}}
	data, err := Encode(m)
	require.NoError(t, err)
	require.True(t, bytes.Contains(data, []byte(`"detail"`)), "detail missing on the wire")
	gotFrame, err := NewReader(bytes.NewReader(data)).Read()
	require.NoError(t, err)
	require.True(t, gotFrame.Error != nil && gotFrame.Error.Detail != nil && gotFrame.Error.Detail.Maximum == MaxInputJSONBytes && gotFrame.Error.Detail.Kind == "transport_budget",
		"detail did not survive: %+v", gotFrame.Error)

	without := Message{Version: Version, Type: Response, ID: 1, Name: Ping, Error: &Error{Code: InvalidInput, Message: "plain"}}
	data, err = Encode(without)
	require.NoError(t, err)
	require.False(t, bytes.Contains(data, []byte(`"detail"`)), "nil detail must be omitted")

	oversized := Message{Version: Version, Type: Response, ID: 1, Name: Ping,
		Error: &Error{Code: InvalidInput, Message: "m", Detail: &ErrorDetail{Kind: strings.Repeat("x", 129)}}}
	_, err = Encode(oversized)
	require.Error(t, err, "oversized detail accepted")
}

// TestVersionCompatibilityRejection freezes the upgrade rule: v1 is the only
// accepted version and any other value fails before a peer can act on it.
func TestVersionCompatibilityRejection(t *testing.T) {
	for _, v := range []int{0, 2, Version + 1, math.MaxInt32} {
		m := Message{Version: v, Type: Request, ID: 1, Name: Ping}
		require.Error(t, m.Validate(), "version %d accepted", v)
		data, _ := json.Marshal(m)
		_, err := NewReader(bytes.NewReader(append(data, '\n'))).Read()
		require.Error(t, err, "frame with version %d accepted", v)
	}
	hello := Message{Version: 2, Type: Hello, Name: ""}
	require.Error(t, hello.Validate(), "greeting with wrong version accepted")
}

// TestPointerFromUint64 freezes the window-handle conversion: the wire value
// is converted in the worker's own bit width, never through float64. On a
// 64-bit platform every uint64 fits; the overflow branch exists for 32-bit
// workers and is guarded by the same width comparison.
func TestPointerFromUint64(t *testing.T) {
	width := uint64(^uintptr(0))
	for _, v := range []uint64{0, 1, 1 << 32, width} {
		got, ok := PointerFromUint64(v)
		require.True(t, ok && got == uintptr(v), "PointerFromUint64(%d) = %d, %v", v, got, ok)
	}
	if width < math.MaxUint64 {
		_, ok := PointerFromUint64(width + 1)
		require.False(t, ok, "value beyond the %d-bit pointer width accepted", width)
	}
}

func mustMessage(t *testing.T, kind, name string, id uint64, payload any) Message {
	t.Helper()
	m, err := NewMessage(kind, name, id, payload)
	require.NoError(t, err)
	return m
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// TestWireLimitsRejectsUndeliverableConfigurations freezes the pre-commit
// rejection table: explicit limits above the wire budgets fail with
// limit_exceeded and the effective maximum, before the native library is
// touched. The binding's own range validation (including zero capacities) is
// separate and is not duplicated here.
func TestWireLimitsRejectsUndeliverableConfigurations(t *testing.T) {
	defaults := Limits{
		InputStringBytes:  65536,
		InputJSONBytes:    1 << 20,
		ConfigEntries:     256,
		ConfigBytes:       4 << 20,
		ImagePNG:          BufferLimits{Initial: 1 << 20, Max: 32 << 20, GrowthRetries: 5},
		ImageBGR:          BufferLimits{Initial: 4 << 20, Max: 64 << 20, GrowthRetries: 5},
		UUID:              BufferLimits{Initial: 64, Max: 4096, GrowthRetries: 6},
		Tasks:             BufferLimits{Initial: 64, Max: 1 << 16, GrowthRetries: 10},
		CallbackBytes:     1 << 20,
		InternalEvents:    256,
		InternalBytes:     16 << 20,
		PublicEvents:      256,
		PublicBytes:       16 << 20,
		PendingOperations: 128,
		EarlyResults:      1,
		EarlyBytes:        1 << 20,
	}
	err := CheckWireLimits(&defaults)
	require.NoError(t, err, "binding defaults must be wire-compatible: %v", err)

	legal := defaults
	legal.InputJSONBytes = MaxInputJSONBytes
	legal.ImageBGR = BufferLimits{Initial: 4 << 20, Max: ImageTransferMaxBytes, GrowthRetries: 5}
	err = CheckWireLimits(&legal)
	require.NoError(t, err, "binding-legal explicit maxima must be wire-compatible: %v", err)

	cases := []struct {
		name   string
		change func(*Limits)
		field  string
	}{
		{"input json", func(l *Limits) { l.InputJSONBytes = MaxInputJSONBytes + 1 }, "Limits.InputJSONBytes"},
		{"config bytes", func(l *Limits) { l.ConfigBytes = MaxConfigBytes + 1 }, "Limits.ConfigBytes"},
		{"callback", func(l *Limits) { l.CallbackBytes = MaxCallbackBytes + 1 }, "Limits.CallbackBytes"},
		{"png", func(l *Limits) { l.ImagePNG.Max = ImageTransferMaxBytes + 1 }, "Limits.ImagePNG.Max"},
		{"bgr", func(l *Limits) { l.ImageBGR.Max = ImageTransferMaxBytes + 1 }, "Limits.ImageBGR.Max"},
		{"tasks", func(l *Limits) { l.Tasks.Max = MaxTaskElements + 1 }, "Limits.Tasks.Max"},
	}
	for _, c := range cases {
		t.Run("reject "+c.name, func(t *testing.T) {
			l := defaults
			c.change(&l)
			err := CheckWireLimits(&l)
			var wire *Error
			require.True(t, errors.As(err, &wire) && wire.Code == LimitExceeded && wire.Detail != nil && wire.Detail.Maximum != 0 && wire.Detail.Field == c.field,
				"expected limit_exceeded with effective maximum for %s, got %v", c.field, err)
		})
	}
}

// TestImageTransferContract freezes the admission and per-chunk rules.
func TestImageTransferContract(t *testing.T) {
	t.Run("admission", func(t *testing.T) {
		require.NoError(t, AdmitImageTransfer(32<<20, 0))
		require.NoError(t, AdmitImageTransfer(64<<20, 1))
		require.NoError(t, AdmitImageTransfer(ImageTransferMaxBytes, ImageTransfersMax-1))
		err := AdmitImageTransfer(ImageTransferMaxBytes+1, 0)
		var wire *Error
		require.True(t, errors.As(err, &wire) && wire.Code == LimitExceeded && wire.Detail != nil && wire.Detail.Maximum == ImageTransferMaxBytes,
			"over-ceiling transfer must be rejected with the effective maximum, got %v", err)
		err = AdmitImageTransfer(1, ImageTransfersMax)
		require.True(t, errors.As(err, &wire) && wire.Code == LimitExceeded,
			"transfer beyond the concurrency budget must be rejected, got %v", err)
	})

	t.Run("transfer validation", func(t *testing.T) {
		bad := []ImageTransfer{
			{TotalBytes: 1, SHA256: sha256Hex([]byte("x")), ChunkBytes: ImageChunkBytes},
			{TransferID: 1, TotalBytes: 1, SHA256: sha256Hex([]byte("x"))},
			{TransferID: 1, TotalBytes: 1, SHA256: sha256Hex([]byte("x")), ChunkBytes: ImageChunkBytes + 1},
			{TransferID: 1, TotalBytes: 1, SHA256: strings.ToUpper(sha256Hex([]byte("x"))), ChunkBytes: ImageChunkBytes},
			{TransferID: 1, TotalBytes: 1, SHA256: "zz", ChunkBytes: ImageChunkBytes},
			{TransferID: 1, TotalBytes: ImageTransferMaxBytes + 1, SHA256: sha256Hex(nil), ChunkBytes: ImageChunkBytes},
		}
		for i, b := range bad {
			require.Error(t, b.Validate(), "invalid transfer %d accepted: %+v", i, b)
		}
		good := ImageTransfer{TransferID: math.MaxUint64, TotalBytes: 0, SHA256: sha256Hex(nil), ChunkBytes: 1}
		require.NoError(t, good.Validate())
	})

	t.Run("chunk validation", func(t *testing.T) {
		tr := ImageTransfer{TransferID: 5, TotalBytes: ImageChunkBytes + 10, SHA256: sha256Hex(nil), ChunkBytes: ImageChunkBytes}
		bad := []ImageChunkRequest{
			{TransferID: 4, Offset: 0, Length: 1},
			{TransferID: 5, Offset: 0, Length: 0},
			{TransferID: 5, Offset: 0, Length: ImageChunkBytes + 1},
			{TransferID: 5, Offset: ImageChunkBytes + 10, Length: 1},
			{TransferID: 5, Offset: ImageChunkBytes + 5, Length: 6},
		}
		for i, b := range bad {
			require.Error(t, b.Validate(&tr), "invalid chunk request %d accepted: %+v", i, b)
		}
		valid := ImageChunkRequest{TransferID: 5, Offset: ImageChunkBytes, Length: 10}
		require.NoError(t, valid.Validate(&tr))
	})
}

// TestDefaultImageBudgetCarriedEndToEnd is the budget proof: the default
// maximum PNG (32 MiB) and BGR (64 MiB) payloads are carried through the
// chunked contract with every encoded frame inside MaxFrameBytes, and the
// reassembled copy matches byte for byte including its declared SHA256.
func TestDefaultImageBudgetCarriedEndToEnd(t *testing.T) {
	for name, total := range map[string]int{"png32mib": 32 << 20, "bgr64mib": 64 << 20} {
		t.Run(name, func(t *testing.T) {
			payload := make([]byte, total)
			fillPseudoRandom(payload)
			transfer := ImageTransfer{TransferID: 9, TotalBytes: uint64(total), SHA256: sha256Hex(payload), ChunkBytes: ImageChunkBytes}
			require.NoError(t, transfer.Validate())
			reassembled := make([]byte, 0, total)
			chunks := 0
			for offset := uint64(0); offset < transfer.TotalBytes; {
				length := uint32(transfer.TotalBytes - offset)
				if length > transfer.ChunkBytes {
					length = transfer.ChunkBytes
				}
				req := ImageChunkRequest{TransferID: transfer.TransferID, Offset: offset, Length: length}
				require.NoError(t, req.Validate(&transfer))
				chunk := payload[offset : offset+uint64(length)]
				resp := ImageChunkResponse{TransferID: req.TransferID, Offset: req.Offset, Data: base64.StdEncoding.EncodeToString(chunk)}
				m := mustMessage(t, Response, ImageChunk, 100+uint64(chunks), resp)
				data, err := Encode(m)
				require.NoError(t, err)
				require.LessOrEqual(t, len(data), MaxFrameBytes, "chunk frame of %d bytes exceeds the %d byte frame budget", len(data), MaxFrameBytes)
				got, err := NewReader(bytes.NewReader(data)).Read()
				require.NoError(t, err)
				var back ImageChunkResponse
				require.NoError(t, Decode(got.Payload, &back))
				raw, err := base64.StdEncoding.DecodeString(back.Data)
				require.NoError(t, err)
				require.True(t, uint64(len(raw)) == uint64(length) && back.Offset == offset, "chunk mismatch: offset %d length %d", back.Offset, len(raw))
				reassembled = append(reassembled, raw...)
				offset += uint64(length)
				chunks++
			}
			wantChunks := (total + ImageChunkBytes - 1) / ImageChunkBytes
			require.Equal(t, wantChunks, chunks, "chunk count = %d, want %d", chunks, wantChunks)
			require.True(t, bytes.Equal(reassembled, payload), "reassembled image differs")
			require.Equal(t, transfer.SHA256, sha256Hex(reassembled), "sha256 of the reassembled image does not match the transfer")
		})
	}
}

// fillPseudoRandom fills b with a deterministic incompressible-ish pattern; an
// xorshift keeps the test reproducible without a random source.
func fillPseudoRandom(b []byte) {
	var state uint64 = 0x9e3779b97f4a7c15
	for i := range b {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		b[i] = byte(state >> 24)
	}
}

// TestFaultAndDiagnosticsPayloads freezes the independent observation
// contract: fault notice and diagnostics travel as session events with
// structured errors, saturating counts, and a byte-preserving raw prefix that
// may be neither valid UTF-8 nor valid JSON.
func TestFaultAndDiagnosticsPayloads(t *testing.T) {
	fault := mustMessage(t, Event, FaultEvent, 0, FaultNotice{Error: Error{Code: EventOverflow, Message: "events lost"}})
	fault.Sequence = 1
	require.NoError(t, fault.Validate())
	var notice FaultNotice
	roundTrip(t, fault, &notice)
	require.Equal(t, EventOverflow, notice.Error.Code, "fault notice: %+v", notice)

	rawPrefix := []byte{0x00, 0xFF, 0x80, 0xC3, 0x28, 0x00}
	report := InstanceDiagnostics{
		LostEvents:     math.MaxUint64,
		FirstFault:     &Error{Code: EventOverflow, Message: "first"},
		RawPrefix:      base64.StdEncoding.EncodeToString(rawPrefix),
		StopDiagnostic: &Error{Code: NativeRejected, Message: "stop returned 0"},
	}
	diag := mustMessage(t, Event, DiagnosticsEvent, 0, report)
	diag.Sequence = 2
	require.NoError(t, diag.Validate())
	var got InstanceDiagnostics
	roundTrip(t, diag, &got)
	require.True(t, got.LostEvents == math.MaxUint64 && got.FirstFault != nil && got.StopDiagnostic != nil, "diagnostics did not survive: %+v", got)
	decoded, err := base64.StdEncoding.DecodeString(got.RawPrefix)
	require.NoError(t, err)
	require.True(t, bytes.Equal(decoded, rawPrefix), "raw prefix changed: %x", decoded)

	// The payload validators reject bound breaches before a client records
	// the facts: unstable errors, an unbounded or undecodable raw prefix.
	require.Error(t, (&FaultNotice{}).Validate(), "a fault without a code must be rejected")
	require.Error(t, (&FaultNotice{Error: Error{Code: EventOverflow}}).Validate(), "a fault without a message must be rejected")
	require.Error(t, (&FaultNotice{Error: Error{Code: EventOverflow, Message: strings.Repeat("x", 4097)}}).Validate(), "an over-long fault message must be rejected")
	require.Error(t, (&FaultNotice{Error: Error{Code: EventOverflow, Message: "m", Detail: &ErrorDetail{Kind: strings.Repeat("x", 129)}}}).Validate(), "an over-long fault detail must be rejected")
	err = report.Validate()
	require.NoError(t, err, "the frozen report must validate: %v", err)
	require.Error(t, (&InstanceDiagnostics{RawPrefix: "!!!"}).Validate(), "a non-base64 raw prefix must be rejected")
	oversize := InstanceDiagnostics{RawPrefix: base64.StdEncoding.EncodeToString(make([]byte, MaxRawPrefixBytes+1))}
	require.Error(t, oversize.Validate(), "an unbounded raw prefix must be rejected")
	atBound := InstanceDiagnostics{RawPrefix: base64.StdEncoding.EncodeToString(make([]byte, MaxRawPrefixBytes))}
	err = atBound.Validate()
	require.NoError(t, err, "the frozen raw-prefix bound must validate: %v", err)
	require.Error(t, (&InstanceDiagnostics{FirstFault: &Error{Code: EventOverflow}}).Validate(), "a first fault without a message must be rejected")

	// A normal close carries neither fault nor prefix; the fields are absent.
	normal, err := json.Marshal(InstanceDiagnostics{LostEvents: 0})
	require.NoError(t, err)
	require.False(t, bytes.Contains(normal, []byte("first_fault")) || bytes.Contains(normal, []byte("raw_prefix")) || bytes.Contains(normal, []byte("stop_diagnostic")),
		"normal close must omit fault fields: %s", normal)
}

// TestFrozenCommandNamesAreValidFrames freezes the reserved v1 command names:
// every command in the contract is a syntactically valid frame, so peers can
// rely on the names even where a build rejects them with unsupported.
func TestFrozenCommandNamesAreValidFrames(t *testing.T) {
	commands := []string{Initialize, Connect, AppendTask, SetTaskParams, Start, Stop, Status, Ping, Close,
		BackToHome, UUID, Click, Screencap, AttachWindow, GetImagePNG, GetImageBGR, ImageChunk, Log, MapLevelKey, ItemName}
	for _, name := range commands {
		m := Message{Version: Version, Type: Request, ID: 1, Name: name}
		err := m.Validate()
		require.NoError(t, err, "command %q rejected by the wire rules: %v", name, err)
	}
	events := []string{CoreEvent, OperationEvent, FaultEvent, DiagnosticsEvent}
	for _, name := range events {
		m := Message{Version: Version, Type: Event, Name: name, Sequence: 1}
		err := m.Validate()
		require.NoError(t, err, "event %q rejected by the wire rules: %v", name, err)
	}
}
