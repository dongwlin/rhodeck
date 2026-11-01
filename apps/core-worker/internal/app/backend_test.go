package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	maa "github.com/dongwlin/rhodeck/packages/maa-go"
	"github.com/stretchr/testify/require"
)

func TestNativeErrorCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code string
	}{
		{maa.ErrInvalidInput, protocol.InvalidInput},
		{maa.ErrNativeRejected, protocol.NativeRejected},
		{maa.ErrUnsupported, protocol.Unsupported},
		{maa.ErrOperationLimit, protocol.LimitExceeded},
		{maa.ErrBufferLimit, protocol.LimitExceeded},
		{maa.ErrEventOverflow, protocol.EventOverflow},
		{maa.ErrProtocol, protocol.ProtocolError},
		{maa.ErrClosed, protocol.InvalidState},
		{maa.ErrRuntimeTerminated, protocol.InvalidState},
		{&maa.LibraryLoadError{Err: errors.New("fixture")}, protocol.LoadFailed},
		{&maa.SymbolLookupError{Err: errors.New("fixture")}, protocol.SymbolMissing},
		{&maa.FunctionRegistrationError{Err: errors.New("fixture")}, protocol.RegistrationFailed},
	} {
		t.Run(tc.code, func(t *testing.T) {
			got := classify(fmt.Errorf("context: %w", tc.err))
			require.Equal(t, tc.code, got.Code, "code = %s, want %s", got.Code, tc.code)
		})
	}
	got := classify(errors.New(strings.Repeat("界", 4096)))
	require.LessOrEqual(t, len(got.Message), 4096, "diagnostic exceeds bound: %d", len(got.Message))
}

// TestNativeErrorStructuredDetails proves the bounded structured reasons: the
// load/symbol/registration families carry the identifying field the binding's
// error types expose as data (never parsed from text), an aggregate maps to
// its first matching member, the two limit sentinels share limit_exceeded but
// stay distinguishable by kind without inventing numbers, and every produced
// error stays a valid wire frame — including a truncated over-long field.
func TestNativeErrorStructuredDetails(t *testing.T) {
	loadErr := &maa.LibraryLoadError{LibraryName: "libMaaCore.so", Path: "/opt/maa/libMaaCore.so", GOOS: "linux", GOARCH: "amd64", Err: errors.New("fixture loader failure")}
	symbolErr := &maa.SymbolLookupError{LibraryPath: "/opt/maa/libMaaCore.so", Symbol: "AsstCreateEx", Profile: "locked", Err: errors.New("fixture lookup failure")}
	registrationErr := &maa.FunctionRegistrationError{Symbol: "AsstDestroy", Signature: "func uintptr", Err: errors.New("fixture registration failure")}
	aggregate := maa.AggregateError{symbolErr, errors.New("unrelated member")}
	cases := []struct {
		name   string
		err    error
		code   string
		detail *protocol.ErrorDetail
	}{
		{"library load", loadErr, protocol.LoadFailed, &protocol.ErrorDetail{Kind: "library_load", Field: "libMaaCore.so"}},
		{"symbol lookup", symbolErr, protocol.SymbolMissing, &protocol.ErrorDetail{Kind: "symbol_lookup", Field: "AsstCreateEx"}},
		{"function registration", registrationErr, protocol.RegistrationFailed, &protocol.ErrorDetail{Kind: "function_registration", Field: "AsstDestroy"}},
		{"aggregate maps to its first matching member", fmt.Errorf("load transaction: %w", aggregate), protocol.SymbolMissing, &protocol.ErrorDetail{Kind: "symbol_lookup", Field: "AsstCreateEx"}},
		{"buffer limit", fmt.Errorf("probe exhausted: %w", maa.ErrBufferLimit), protocol.LimitExceeded, &protocol.ErrorDetail{Kind: "buffer_limit"}},
		{"operation limit", fmt.Errorf("limit reached: %w", maa.ErrOperationLimit), protocol.LimitExceeded, &protocol.ErrorDetail{Kind: "operation_limit"}},
		{"sentinels without structured data", maa.ErrInvalidInput, protocol.InvalidInput, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classify(fmt.Errorf("context: %w", tc.err))
			require.Equal(t, tc.code, got.Code, "code = %s, want %s", got.Code, tc.code)
			if tc.detail == nil {
				require.Nil(t, got.Detail, "detail = %+v, want none", got.Detail)
				return
			}
			require.NotNil(t, got.Detail, "detail = %+v, want %+v", got.Detail, tc.detail)
			require.Equal(t, *tc.detail, *got.Detail, "detail = %+v, want %+v", got.Detail, tc.detail)
			// Every mapped error must remain a valid frame payload.
			require.NoError(t, validateWireErrorForTest(got), "mapped error is not wire-valid")
		})
	}
	long := &maa.LibraryLoadError{LibraryName: strings.Repeat("长", 100), Err: errors.New("fixture")}
	got := classify(long)
	require.Equal(t, protocol.LoadFailed, got.Code, "over-long field not bounded")
	require.LessOrEqual(t, len(got.Detail.Field), 128, "over-long field not bounded: %d bytes", len(got.Detail.Field))
	require.NoError(t, validateWireErrorForTest(got), "truncated detail is not wire-valid")
}

// validateWireErrorForTest runs the wire frame validation against an error as
// if it were embedded in a message.
func validateWireErrorForTest(e *protocol.Error) error {
	m := protocol.Message{Version: protocol.Version, Type: protocol.Response, ID: 1, Name: protocol.Ping, Error: e}
	return m.Validate()
}

// mappingLimitsFixture gives every Limits and BufferLimits field a distinct
// value so the mapping test catches cross-wired or dropped fields.
func mappingLimitsFixture() protocol.Limits {
	return protocol.Limits{
		InputStringBytes:  11,
		InputJSONBytes:    12,
		ConfigEntries:     13,
		ConfigBytes:       14,
		ImagePNG:          protocol.BufferLimits{Initial: 21, Max: 22, GrowthRetries: 23},
		ImageBGR:          protocol.BufferLimits{Initial: 31, Max: 32, GrowthRetries: 33},
		UUID:              protocol.BufferLimits{Initial: 41, Max: 42, GrowthRetries: 43},
		Tasks:             protocol.BufferLimits{Initial: 51, Max: 52, GrowthRetries: 53},
		CallbackBytes:     61,
		InternalEvents:    62,
		InternalBytes:     63,
		PublicEvents:      64,
		PublicBytes:       65,
		PendingOperations: 66,
		EarlyResults:      67,
		EarlyBytes:        68,
	}
}

// TestLimitsMappingTransfersEveryField proves the wire limit set maps onto the
// binding configuration field by field, including every BufferLimits strategy,
// and that omission maps to the binding's own default set instead of a partial
// skeleton.
func TestLimitsMappingTransfersEveryField(t *testing.T) {
	in := mappingLimitsFixture()
	got := bindingLimits(&in)
	want := maa.Limits{
		InputStringBytes:  11,
		InputJSONBytes:    12,
		ConfigEntries:     13,
		ConfigBytes:       14,
		ImagePNG:          maa.BufferLimits{Initial: 21, Max: 22, GrowthRetries: 23},
		ImageBGR:          maa.BufferLimits{Initial: 31, Max: 32, GrowthRetries: 33},
		UUID:              maa.BufferLimits{Initial: 41, Max: 42, GrowthRetries: 43},
		Tasks:             maa.BufferLimits{Initial: 51, Max: 52, GrowthRetries: 53},
		CallbackBytes:     61,
		InternalEvents:    62,
		InternalBytes:     63,
		PublicEvents:      64,
		PublicBytes:       65,
		PendingOperations: 66,
		EarlyResults:      67,
		EarlyBytes:        68,
	}
	require.Equal(t, want, *got, "binding limits = %+v, want %+v", *got, want)
	require.Nil(t, bindingLimits(nil), "omitted wire limits must map to the binding's own defaults")
}

// TestInitializeConfigMappingTransfersEveryField proves the initialize request
// maps onto the binding runtime and instance configurations: explicit paths,
// ordered resource roots, static and instance options (empty values included),
// connection extras with raw JSON preserved, and the limit set.
func TestInitializeConfigMappingTransfersEveryField(t *testing.T) {
	limits := mappingLimitsFixture()
	req := protocol.InitializeRequest{
		SnapshotID:       "snapshot",
		LibraryPath:      "/opt/maa/libMaaCore.so",
		UserDir:          "/var/lib/rhodeck/core",
		ResourceRoots:    []string{"/opt/maa/base", "/opt/maa/overlay"},
		StaticOptions:    []protocol.Option{{Key: 1, Value: "cpu"}, {Key: 2, Value: ""}},
		InstanceOptions:  []protocol.Option{{Key: 7, Value: "instance"}},
		ConnectionExtras: []protocol.ConnectionExtra{{Name: "adb", JSON: []byte(`{"serial":"emulator-5554"}`)}},
		Limits:           &limits,
	}
	cfg := bindingConfig(req)
	require.Equal(t, req.LibraryPath, cfg.LibraryPath, "paths changed: %+v", cfg)
	require.Equal(t, req.UserDir, cfg.UserDir, "paths changed: %+v", cfg)
	require.Len(t, cfg.ResourceRoots, 2, "resource root order changed: %+v", cfg.ResourceRoots)
	require.Equal(t, "/opt/maa/base", cfg.ResourceRoots[0], "resource root order changed: %+v", cfg.ResourceRoots)
	require.Equal(t, "/opt/maa/overlay", cfg.ResourceRoots[1], "resource root order changed: %+v", cfg.ResourceRoots)
	require.Len(t, cfg.StaticOptions, 2, "static options changed: %+v", cfg.StaticOptions)
	require.Equal(t, maa.StaticOption{Key: 1, Value: "cpu"}, cfg.StaticOptions[0], "static options changed: %+v", cfg.StaticOptions)
	require.Equal(t, maa.StaticOption{Key: 2, Value: ""}, cfg.StaticOptions[1], "static options changed: %+v", cfg.StaticOptions)
	require.Len(t, cfg.ConnectionExtras, 1, "connection extras changed: %+v", cfg.ConnectionExtras)
	require.Equal(t, "adb", cfg.ConnectionExtras[0].Name, "connection extras changed: %+v", cfg.ConnectionExtras)
	require.Equal(t, `{"serial":"emulator-5554"}`, string(cfg.ConnectionExtras[0].JSON), "connection extras changed: %+v", cfg.ConnectionExtras)
	want := maa.Limits{
		InputStringBytes: 11, InputJSONBytes: 12, ConfigEntries: 13, ConfigBytes: 14,
		ImagePNG:      maa.BufferLimits{Initial: 21, Max: 22, GrowthRetries: 23},
		ImageBGR:      maa.BufferLimits{Initial: 31, Max: 32, GrowthRetries: 33},
		UUID:          maa.BufferLimits{Initial: 41, Max: 42, GrowthRetries: 43},
		Tasks:         maa.BufferLimits{Initial: 51, Max: 52, GrowthRetries: 53},
		CallbackBytes: 61, InternalEvents: 62, InternalBytes: 63, PublicEvents: 64,
		PublicBytes: 65, PendingOperations: 66, EarlyResults: 67, EarlyBytes: 68,
	}
	require.Equal(t, want, *cfg.Limits, "limits changed through the config mapping: %+v", *cfg.Limits)
	icfg := bindingInstanceConfig(req)
	require.Len(t, icfg.Options, 1, "instance options changed: %+v", icfg.Options)
	require.Equal(t, maa.InstanceOption{Key: 7, Value: "instance"}, icfg.Options[0], "instance options changed: %+v", icfg.Options)
}

// TestEffectiveLimitsSelectsWholeDefaultOrExplicitCopy freezes the default
// versus explicit distinction: omission applies the complete materialized
// default set, an explicit configuration is applied as-is, and the returned
// snapshot is detached from the request. These numbers are the binding's
// frozen defaults, which must stay wire-compatible.
func TestEffectiveLimitsSelectsWholeDefaultOrExplicitCopy(t *testing.T) {
	defaults := protocol.Limits{
		InputStringBytes:  65_536,
		InputJSONBytes:    1 << 20,
		ConfigEntries:     256,
		ConfigBytes:       4 << 20,
		ImagePNG:          protocol.BufferLimits{Initial: 1 << 20, Max: 32 << 20, GrowthRetries: 5},
		ImageBGR:          protocol.BufferLimits{Initial: 4 << 20, Max: 64 << 20, GrowthRetries: 5},
		UUID:              protocol.BufferLimits{Initial: 64, Max: 4096, GrowthRetries: 6},
		Tasks:             protocol.BufferLimits{Initial: 64, Max: 1 << 16, GrowthRetries: 10},
		CallbackBytes:     1 << 20,
		InternalEvents:    256,
		InternalBytes:     16 << 20,
		PublicEvents:      256,
		PublicBytes:       16 << 20,
		PendingOperations: 128,
		EarlyResults:      1,
		EarlyBytes:        1 << 20,
	}
	require.Equal(t, defaults, bindingDefaultLimits, "materialized defaults drifted: %+v", bindingDefaultLimits)
	require.Equal(t, defaults, effectiveLimits(nil), "omitted limits = %+v, want the whole default set", effectiveLimits(nil))
	require.NoError(t, protocol.CheckWireLimits(&defaults), "defaults must be wire-compatible")

	explicit := mappingLimitsFixture()
	got := effectiveLimits(&explicit)
	require.Equal(t, explicit, got, "explicit limits = %+v, want %+v", got, explicit)
	explicit.InputJSONBytes = 999
	explicit.ImagePNG.Max = 1
	explicit.Tasks.GrowthRetries = 9
	require.NotEqual(t, uint64(999), got.InputJSONBytes, "effective snapshot is aliased to the request: %+v", got)
	require.NotEqual(t, uint64(1), got.ImagePNG.Max, "effective snapshot is aliased to the request: %+v", got)
	require.NotEqual(t, uint32(9), got.Tasks.GrowthRetries, "effective snapshot is aliased to the request: %+v", got)
}

// TestOpenEngineRejectsIllegalLimitsBeforeNativeLoad proves a zero capacity, a
// contradictory probe strategy, a cross-field violation and a non-fixed
// EarlyResults reach the binding's own validation and are rejected with
// invalid_input and the field path — never backfilled from defaults. These
// failures happen before the lifecycle gate, so they consume no process
// lifetime and never attempt to load the library.
func TestOpenEngineRejectsIllegalLimitsBeforeNativeLoad(t *testing.T) {
	dir := t.TempDir()
	library := filepath.Join(dir, "libMaaCore.so")
	require.NoError(t, os.WriteFile(library, []byte("placeholder bytes, never loaded"), 0o600))
	cases := []struct {
		name   string
		mutate func(*protocol.Limits)
		field  string
	}{
		{"zero capacity", func(l *protocol.Limits) { l.ImagePNG.Initial = 0 }, "Limits.ImagePNG.Initial"},
		{"initial exceeds max", func(l *protocol.Limits) { l.UUID = protocol.BufferLimits{Initial: 4097, Max: 4096, GrowthRetries: 6} }, "Limits.UUID.Initial"},
		{"cross field", func(l *protocol.Limits) { l.InputStringBytes = 65536; l.ConfigBytes = 32768 }, "Limits.InputStringBytes"},
		{"fixed early results", func(l *protocol.Limits) { l.EarlyResults = 2 }, "Limits.EarlyResults"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			limits := bindingDefaultLimits
			tc.mutate(&limits)
			req := protocol.InitializeRequest{SnapshotID: "limits", LibraryPath: library, UserDir: dir, ResourceRoots: []string{dir}, Limits: &limits}
			_, _, err := openEngine(req)
			require.ErrorIs(t, err, maa.ErrInvalidInput, "expected the binding's invalid-input rejection, got %v", err)
			require.True(t, strings.Contains(err.Error(), tc.field), "rejection must name %s, got %v", tc.field, err)
			got := classify(err)
			require.Equal(t, protocol.InvalidInput, got.Code, "code = %s, want invalid_input", got.Code)
		})
	}
}

// TestHwndFromUint64ChecksPointerWidth proves the window-handle conversion
// never truncates: every value within the platform pointer width converts
// exactly, the 2^53+1 JSON boundary converts intact on 64-bit builds, and a
// value the platform pointer cannot hold (exercisable on 32-bit builds) is
// rejected with invalid_input instead of being wrapped.
func TestHwndFromUint64ChecksPointerWidth(t *testing.T) {
	width := uint64(^uintptr(0))
	for _, v := range []uint64{1, 1 << 16, width} {
		hwnd, err := hwndFromUint64(v)
		require.NoError(t, err, "hwndFromUint64(%d) = %d, %v", v, hwnd, err)
		require.Equal(t, uintptr(v), hwnd, "hwndFromUint64(%d) = %d, %v", v, hwnd, err)
	}
	if width >= 1<<53 {
		// 64-bit: the full uint64 range fits the pointer width, so the
		// >2^53 JSON boundary converts exactly and no uint64 can overflow.
		for _, v := range []uint64{1 << 53, (1 << 53) + 1, ^uint64(0)} {
			hwnd, err := hwndFromUint64(v)
			require.NoError(t, err, "hwndFromUint64(%d) = %d, %v", v, hwnd, err)
			require.Equal(t, uintptr(v), hwnd, "hwndFromUint64(%d) = %d, %v", v, hwnd, err)
		}
		return
	}
	// 32-bit: values beyond the pointer width are rejected, not truncated.
	_, err := hwndFromUint64(1 << 53)
	require.Error(t, err, "a value beyond the platform pointer width must be rejected")
	require.Equal(t, protocol.InvalidInput, classify(err).Code, "overflow code = %s, want invalid_input", classify(err).Code)
}

// stubObservations is a controllable stand-in for the native instance's
// observation surface, exercising observeInstance's pipeline without a native
// library: unbuffered channels give the tests a handle on the reader's
// blocking points, and the fault/done closures mimic the binding's one-shot
// notifications.
type stubObservations struct {
	events   chan maa.Event
	fault    chan struct{}
	done     chan struct{}
	faultErr error
	snapshot func() maa.InstanceDiagnostics
}

func newStubObservations() *stubObservations {
	return &stubObservations{events: make(chan maa.Event), fault: make(chan struct{}), done: make(chan struct{})}
}

func (s *stubObservations) Events() <-chan maa.Event { return s.events }
func (s *stubObservations) Fault() <-chan struct{}   { return s.fault }
func (s *stubObservations) Done() <-chan struct{}    { return s.done }
func (s *stubObservations) Diagnostics() maa.InstanceDiagnostics {
	if s.snapshot != nil {
		return s.snapshot()
	}
	// Like the binding, the snapshot carries whatever fault was recorded.
	return maa.InstanceDiagnostics{FirstFault: s.faultErr}
}

// armFault records the first fault and fires the notification in the binding's
// linearization order.
func (s *stubObservations) armFault(cause error) {
	s.faultErr = cause
	close(s.fault)
}

// finish terminates the instance: the event stream closes first, then the
// terminal notification.
func (s *stubObservations) finish() {
	close(s.events)
	close(s.done)
}

// observationRecorder collects the events observeInstance emits, in order.
type observationRecorder struct {
	mu       sync.Mutex
	messages []protocol.Message
}

func (r *observationRecorder) emit(name string, payload any) error {
	m, err := protocol.NewMessage(protocol.Event, name, 0, payload)
	if err != nil {
		return err
	}
	r.mu.Lock()
	r.messages = append(r.messages, m)
	r.mu.Unlock()
	return nil
}

func (r *observationRecorder) count(name string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, m := range r.messages {
		if m.Name == name {
			n++
		}
	}
	return n
}

func (r *observationRecorder) named(name string) []protocol.Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []protocol.Message
	for _, m := range r.messages {
		if m.Name == name {
			out = append(out, m)
		}
	}
	return out
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	require.FailNow(t, "condition never became true")
}

// TestObserveInstanceReportsFaultWhileTheReaderIsBlocked proves the fault
// notification is observed outside the single Events reader: with the reader
// blocked on an empty stream, the fault is still reported in bounded time,
// with its classified stable reason, and without fabricating extra events.
func TestObserveInstanceReportsFaultWhileTheReaderIsBlocked(t *testing.T) {
	in := newStubObservations()
	var rec observationRecorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	obsErr := make(chan error, 1)
	go func() { obsErr <- observeInstance(ctx, in, rec.emit) }()

	// One core event: the unbuffered send completes when the reader receives
	// it; waiting for its emission leaves the reader blocked on the stream.
	in.events <- maa.Event{MessageID: 1, Sequence: 1, JSON: json.RawMessage(`{}`)}
	waitFor(t, func() bool { return rec.count(protocol.CoreEvent) == 1 })

	in.armFault(fmt.Errorf("probe exhausted: %w", maa.ErrEventOverflow))
	waitFor(t, func() bool { return rec.count(protocol.FaultEvent) == 1 })
	notices := rec.named(protocol.FaultEvent)
	var notice protocol.FaultNotice
	require.NoError(t, protocol.Decode(notices[0].Payload, &notice))
	require.Equal(t, protocol.EventOverflow, notice.Error.Code, "fault code = %s, want event_overflow", notice.Error.Code)
	require.Equal(t, 1, rec.count(protocol.CoreEvent), "the blocked reader produced extra events: %+v", rec.named(protocol.CoreEvent))

	cancel()
	require.Error(t, <-obsErr, "a cancelled observation must report why")
}

// TestObserveInstanceOrdersFaultBeforeDiagnostics proves the terminal
// snapshot follows the fault report, is taken after native Done, and carries
// the binding's bounded facts: the saturating lost count exactly, the raw
// prefix base64-encoded byte for byte, and the classified stop diagnostic.
func TestObserveInstanceOrdersFaultBeforeDiagnostics(t *testing.T) {
	in := newStubObservations()
	var rec observationRecorder
	rawPrefix := []byte{0xDE, 0x00, 0xFF, 0xFE, 0x80} // NUL and invalid UTF-8 survive byte for byte
	fault := fmt.Errorf("event budget exhausted: %w", maa.ErrEventOverflow)
	in.snapshot = func() maa.InstanceDiagnostics {
		return maa.InstanceDiagnostics{
			LostEvents:     math.MaxUint64,
			FirstFault:     fault,
			RawPrefix:      rawPrefix,
			StopDiagnostic: fmt.Errorf("stop returned 0: %w", maa.ErrNativeRejected),
		}
	}
	obsErr := make(chan error, 1)
	go func() { obsErr <- observeInstance(context.Background(), in, rec.emit) }()
	in.armFault(fault)
	in.finish()
	require.NoError(t, <-obsErr, "observe after a faulted close")

	require.Equal(t, 1, rec.count(protocol.FaultEvent), "fault events = %d, want exactly one", rec.count(protocol.FaultEvent))
	reports := rec.named(protocol.DiagnosticsEvent)
	require.Len(t, reports, 1, "diagnostics events = %d, want exactly one", len(reports))
	var order []string
	for _, m := range rec.messages {
		order = append(order, m.Name)
	}
	faultAt, diagnosticsAt := -1, -1
	for i, name := range order {
		switch name {
		case protocol.FaultEvent:
			faultAt = i
		case protocol.DiagnosticsEvent:
			diagnosticsAt = i
		}
	}
	require.NotEqual(t, -1, faultAt, "fault report must precede the terminal snapshot: %v", order)
	require.NotEqual(t, -1, diagnosticsAt, "fault report must precede the terminal snapshot: %v", order)
	require.LessOrEqual(t, faultAt, diagnosticsAt, "fault report must precede the terminal snapshot: %v", order)

	var report protocol.InstanceDiagnostics
	require.NoError(t, protocol.Decode(reports[0].Payload, &report))
	require.Equal(t, uint64(math.MaxUint64), report.LostEvents, "lost_events = %d, want the saturated count", report.LostEvents)
	require.NotNil(t, report.FirstFault, "first fault = %+v, want event_overflow", report.FirstFault)
	require.Equal(t, protocol.EventOverflow, report.FirstFault.Code, "first fault = %+v, want event_overflow", report.FirstFault)
	require.NotNil(t, report.StopDiagnostic, "stop diagnostic = %+v, want native_rejected", report.StopDiagnostic)
	require.Equal(t, protocol.NativeRejected, report.StopDiagnostic.Code, "stop diagnostic = %+v, want native_rejected", report.StopDiagnostic)
	decoded, err := base64.StdEncoding.DecodeString(report.RawPrefix)
	require.NoError(t, err, "raw prefix changed: %q -> %q", rawPrefix, decoded)
	require.Equal(t, string(rawPrefix), string(decoded), "raw prefix changed")
	require.NoError(t, report.Validate(), "report is not wire-valid")
}

// TestObserveInstanceTerminalSnapshotWithoutFault proves a close without any
// instance fault produces the terminal snapshot only — once — and that a
// stop-only diagnostic never fabricates a fault event.
func TestObserveInstanceTerminalSnapshotWithoutFault(t *testing.T) {
	t.Run("clean close keeps every optional field absent", func(t *testing.T) {
		in := newStubObservations()
		var rec observationRecorder
		obsErr := make(chan error, 1)
		go func() { obsErr <- observeInstance(context.Background(), in, rec.emit) }()
		in.finish()
		require.NoError(t, <-obsErr, "clean close")
		require.Equal(t, 0, rec.count(protocol.FaultEvent), "a clean close must not report a fault: %+v", rec.named(protocol.FaultEvent))
		reports := rec.named(protocol.DiagnosticsEvent)
		require.Len(t, reports, 1, "diagnostics events = %d, want exactly one", len(reports))
		var report protocol.InstanceDiagnostics
		require.NoError(t, protocol.Decode(reports[0].Payload, &report))
		require.Zero(t, report.LostEvents, "clean close snapshot = %+v, want the zero form", report)
		require.Nil(t, report.FirstFault, "clean close snapshot = %+v, want the zero form", report)
		require.Empty(t, report.RawPrefix, "clean close snapshot = %+v, want the zero form", report)
		require.Nil(t, report.StopDiagnostic, "clean close snapshot = %+v, want the zero form", report)
	})
	t.Run("a stop-only diagnostic is not a fault", func(t *testing.T) {
		in := newStubObservations()
		var rec observationRecorder
		in.snapshot = func() maa.InstanceDiagnostics {
			return maa.InstanceDiagnostics{StopDiagnostic: fmt.Errorf("stop returned 0: %w", maa.ErrNativeRejected)}
		}
		obsErr := make(chan error, 1)
		go func() { obsErr <- observeInstance(context.Background(), in, rec.emit) }()
		in.finish()
		require.NoError(t, <-obsErr, "stop-only close")
		require.Equal(t, 0, rec.count(protocol.FaultEvent), "a stop diagnostic must not fabricate a fault: %+v", rec.named(protocol.FaultEvent))
		var report protocol.InstanceDiagnostics
		require.NoError(t, protocol.Decode(rec.named(protocol.DiagnosticsEvent)[0].Payload, &report))
		require.NotNil(t, report.StopDiagnostic, "stop-only snapshot = %+v", report)
		require.Equal(t, protocol.NativeRejected, report.StopDiagnostic.Code, "stop-only snapshot = %+v", report)
		require.Nil(t, report.FirstFault, "stop-only snapshot = %+v", report)
	})
}

// TestObserveInstanceReportsFaultWhenDoneAndFaultAreSimultaneous proves the
// watcher's selection cannot lose the fault report: whichever ready
// notification the select picks, the first fault is reported exactly once and
// before the terminal snapshot.
func TestObserveInstanceReportsFaultWhenDoneAndFaultAreSimultaneous(t *testing.T) {
	in := newStubObservations()
	var rec observationRecorder
	fault := fmt.Errorf("first fault: %w", maa.ErrProtocol)
	in.snapshot = func() maa.InstanceDiagnostics {
		return maa.InstanceDiagnostics{FirstFault: in.faultErr}
	}
	obsErr := make(chan error, 1)
	go func() { obsErr <- observeInstance(context.Background(), in, rec.emit) }()
	// Both notifications closed before the watcher runs: the maximum
	// simultaneity the select can face. The event stream closes first, like
	// the binding's close completion.
	in.armFault(fault)
	close(in.events)
	close(in.done)
	require.NoError(t, <-obsErr, "observe")
	require.Equal(t, 1, rec.count(protocol.FaultEvent), "fault events = %d, want exactly one", rec.count(protocol.FaultEvent))
	reports := rec.named(protocol.DiagnosticsEvent)
	require.Len(t, reports, 1, "diagnostics events = %d, want exactly one", len(reports))
	var order []string
	for _, m := range rec.messages {
		order = append(order, m.Name)
	}
	faultAt, diagnosticsAt := -1, -1
	for i, name := range order {
		switch name {
		case protocol.FaultEvent:
			faultAt = i
		case protocol.DiagnosticsEvent:
			diagnosticsAt = i
		}
	}
	require.LessOrEqual(t, faultAt, diagnosticsAt, "fault report must precede the terminal snapshot: %v", order)
}

// TestObserveInstancePropagatesEmissionFailure proves a failed emission ends
// the observation with the emission error instead of continuing silently —
// the session-failure path for an undeliverable report.
func TestObserveInstancePropagatesEmissionFailure(t *testing.T) {
	t.Run("reader emission failure", func(t *testing.T) {
		in := newStubObservations()
		boom := errors.New("outbound queue failed")
		emit := func(name string, payload any) error {
			if name == protocol.CoreEvent {
				return boom
			}
			return nil
		}
		obsErr := make(chan error, 1)
		go func() { obsErr <- observeInstance(context.Background(), in, emit) }()
		in.events <- maa.Event{MessageID: 1, Sequence: 1, JSON: json.RawMessage(`{}`)}
		require.ErrorIs(t, <-obsErr, boom, "observe error must be the emission failure")
	})
	t.Run("terminal emission failure", func(t *testing.T) {
		in := newStubObservations()
		in.snapshot = func() maa.InstanceDiagnostics { return maa.InstanceDiagnostics{LostEvents: 1} }
		boom := errors.New("outbound queue failed")
		emit := func(name string, payload any) error {
			if name == protocol.DiagnosticsEvent {
				return boom
			}
			return nil
		}
		obsErr := make(chan error, 1)
		go func() { obsErr <- observeInstance(context.Background(), in, emit) }()
		in.finish()
		require.ErrorIs(t, <-obsErr, boom, "observe error must be the emission failure")
	})
}
