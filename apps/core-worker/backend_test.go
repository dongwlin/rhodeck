package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	maa "github.com/dongwlin/rhodeck/packages/maa-go"
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
			if got := classify(fmt.Errorf("context: %w", tc.err)); got.Code != tc.code {
				t.Fatalf("code = %s, want %s", got.Code, tc.code)
			}
		})
	}
	if got := classify(errors.New(strings.Repeat("界", 4096))); len(got.Message) > 4096 {
		t.Fatalf("diagnostic exceeds bound: %d", len(got.Message))
	}
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
	if *got != want {
		t.Fatalf("binding limits = %+v, want %+v", *got, want)
	}
	if bindingLimits(nil) != nil {
		t.Fatal("omitted wire limits must map to the binding's own defaults")
	}
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
	if cfg.LibraryPath != req.LibraryPath || cfg.UserDir != req.UserDir {
		t.Fatalf("paths changed: %+v", cfg)
	}
	if len(cfg.ResourceRoots) != 2 || cfg.ResourceRoots[0] != "/opt/maa/base" || cfg.ResourceRoots[1] != "/opt/maa/overlay" {
		t.Fatalf("resource root order changed: %+v", cfg.ResourceRoots)
	}
	if len(cfg.StaticOptions) != 2 || cfg.StaticOptions[0] != (maa.StaticOption{Key: 1, Value: "cpu"}) || cfg.StaticOptions[1] != (maa.StaticOption{Key: 2, Value: ""}) {
		t.Fatalf("static options changed: %+v", cfg.StaticOptions)
	}
	if len(cfg.ConnectionExtras) != 1 || cfg.ConnectionExtras[0].Name != "adb" || string(cfg.ConnectionExtras[0].JSON) != `{"serial":"emulator-5554"}` {
		t.Fatalf("connection extras changed: %+v", cfg.ConnectionExtras)
	}
	want := maa.Limits{
		InputStringBytes: 11, InputJSONBytes: 12, ConfigEntries: 13, ConfigBytes: 14,
		ImagePNG:      maa.BufferLimits{Initial: 21, Max: 22, GrowthRetries: 23},
		ImageBGR:      maa.BufferLimits{Initial: 31, Max: 32, GrowthRetries: 33},
		UUID:          maa.BufferLimits{Initial: 41, Max: 42, GrowthRetries: 43},
		Tasks:         maa.BufferLimits{Initial: 51, Max: 52, GrowthRetries: 53},
		CallbackBytes: 61, InternalEvents: 62, InternalBytes: 63, PublicEvents: 64,
		PublicBytes: 65, PendingOperations: 66, EarlyResults: 67, EarlyBytes: 68,
	}
	if *cfg.Limits != want {
		t.Fatalf("limits changed through the config mapping: %+v", *cfg.Limits)
	}
	icfg := bindingInstanceConfig(req)
	if len(icfg.Options) != 1 || icfg.Options[0] != (maa.InstanceOption{Key: 7, Value: "instance"}) {
		t.Fatalf("instance options changed: %+v", icfg.Options)
	}
}

// TestEffectiveLimitsSelectsWholeDefaultOrExplicitCopy freezes the default
// versus explicit distinction: omission applies the complete materialized
// default set, an explicit configuration is applied as-is, and the returned
// snapshot is detached from the request. The pinned numbers are the locked
// binding profile's frozen defaults, which must stay wire-compatible.
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
	if bindingDefaultLimits != defaults {
		t.Fatalf("materialized defaults drifted: %+v", bindingDefaultLimits)
	}
	if got := effectiveLimits(nil); got != defaults {
		t.Fatalf("omitted limits = %+v, want the whole default set", got)
	}
	if err := protocol.CheckWireLimits(&defaults); err != nil {
		t.Fatalf("defaults must be wire-compatible: %v", err)
	}

	explicit := mappingLimitsFixture()
	got := effectiveLimits(&explicit)
	if got != explicit {
		t.Fatalf("explicit limits = %+v, want %+v", got, explicit)
	}
	explicit.InputJSONBytes = 999
	explicit.ImagePNG.Max = 1
	explicit.Tasks.GrowthRetries = 9
	if got.InputJSONBytes == 999 || got.ImagePNG.Max == 1 || got.Tasks.GrowthRetries == 9 {
		t.Fatalf("effective snapshot is aliased to the request: %+v", got)
	}
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
	if err := os.WriteFile(library, []byte("placeholder bytes, never loaded"), 0o600); err != nil {
		t.Fatal(err)
	}
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
			if !errors.Is(err, maa.ErrInvalidInput) {
				t.Fatalf("expected the binding's invalid-input rejection, got %v", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("rejection must name %s, got %v", tc.field, err)
			}
			if got := classify(err); got.Code != protocol.InvalidInput {
				t.Fatalf("code = %s, want invalid_input", got.Code)
			}
		})
	}
}

// TestHwndFromUint64ChecksPointerWidth proves the window-handle conversion
// never truncates: values within the platform pointer width convert exactly
// (including the 2^53+1 JSON boundary and the full uint64 range on 64-bit
// builds), and a value the platform pointer cannot hold is rejected with
// invalid_input instead of being wrapped.
func TestHwndFromUint64ChecksPointerWidth(t *testing.T) {
	width := uint64(^uintptr(0))
	for _, v := range []uint64{1, 1 << 53, (1 << 53) + 1, width} {
		hwnd, err := hwndFromUint64(v)
		if err != nil || hwnd != uintptr(v) {
			t.Fatalf("hwndFromUint64(%d) = %d, %v", v, hwnd, err)
		}
	}
	if width < ^uint64(0) {
		if _, err := hwndFromUint64(width + 1); err == nil {
			t.Fatal("a value beyond the platform pointer width must be rejected")
		} else if got := classify(err).Code; got != protocol.InvalidInput {
			t.Fatalf("overflow code = %s, want invalid_input", got)
		}
	}
}
