package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	maa "github.com/dongwlin/rhodeck/packages/maa-go"
)

type operation interface {
	ID() int32
	Wait(context.Context) (protocol.OperationResult, error)
}

// The command loop owns all native calls. Observe consumes the binding's one
// event stream independently; it never writes to IPC on a callback thread.
type engine interface {
	Connect(protocol.ConnectRequest) (operation, error)
	Click(protocol.ClickRequest) (operation, error)
	Screencap() (operation, error)
	AttachWindow(protocol.AttachWindowRequest) (operation, error)
	AppendTask(protocol.TaskRequest) (int32, error)
	SetTaskParams(protocol.TaskParamsRequest) error
	Start() error
	Stop() (bool, error)
	BackToHome() error
	Status() (protocol.ExecutionStatus, error)
	UUID() (protocol.UUIDResult, error)
	Observe(context.Context, func(protocol.Callback) error) error
	Close() error
}

type engineFactory func(protocol.InitializeRequest) (engine, protocol.RuntimeInfo, error)

// bindingDefaultLimits materializes the whole default limit set of the locked
// binding profile (frozen numbers of packages/maa-go's defaultLimits). When
// the caller omits limits this is the applied configuration, so the reported
// effective set is exactly the set handed to the binding; the binding
// re-validates it on every initialize, so a profile change cannot diverge
// silently.
var bindingDefaultLimits = protocol.Limits{
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

// effectiveLimits returns the limit set the worker applies: a detached copy of
// the explicit configuration, or the whole materialized default set when
// limits are omitted. Omission never selects an empty skeleton that fields
// would be backfilled into, and an explicit zero stays zero for the binding to
// reject — only a missing Limits selects the default set as a whole.
func effectiveLimits(l *protocol.Limits) protocol.Limits {
	if l == nil {
		return bindingDefaultLimits
	}
	return *l
}

// bindingLimits maps the wire limit set onto the binding configuration,
// including every BufferLimits probe strategy. nil maps to nil (the binding's
// own default set); the session materializes defaults before the factory, so
// production requests always carry the complete applied configuration.
func bindingLimits(l *protocol.Limits) *maa.Limits {
	if l == nil {
		return nil
	}
	return &maa.Limits{
		InputStringBytes:  l.InputStringBytes,
		InputJSONBytes:    l.InputJSONBytes,
		ConfigEntries:     l.ConfigEntries,
		ConfigBytes:       l.ConfigBytes,
		ImagePNG:          bindingBufferLimits(l.ImagePNG),
		ImageBGR:          bindingBufferLimits(l.ImageBGR),
		UUID:              bindingBufferLimits(l.UUID),
		Tasks:             bindingBufferLimits(l.Tasks),
		CallbackBytes:     l.CallbackBytes,
		InternalEvents:    l.InternalEvents,
		InternalBytes:     l.InternalBytes,
		PublicEvents:      l.PublicEvents,
		PublicBytes:       l.PublicBytes,
		PendingOperations: l.PendingOperations,
		EarlyResults:      l.EarlyResults,
		EarlyBytes:        l.EarlyBytes,
	}
}

func bindingBufferLimits(b protocol.BufferLimits) maa.BufferLimits {
	return maa.BufferLimits{Initial: b.Initial, Max: b.Max, GrowthRetries: b.GrowthRetries}
}

// bindingConfig maps the initialize request onto the binding runtime
// configuration: explicit paths, ordered resource roots, ordered static
// options, connection extras with their raw JSON preserved byte for byte, and
// the complete limit set. The binding deep-copies the configuration when it
// validates, so the mapped value is transient.
func bindingConfig(req protocol.InitializeRequest) maa.RuntimeConfig {
	cfg := maa.RuntimeConfig{
		LibraryPath:   req.LibraryPath,
		UserDir:       req.UserDir,
		ResourceRoots: req.ResourceRoots,
		Limits:        bindingLimits(req.Limits),
	}
	for _, opt := range req.StaticOptions {
		cfg.StaticOptions = append(cfg.StaticOptions, maa.StaticOption{Key: opt.Key, Value: opt.Value})
	}
	for _, extra := range req.ConnectionExtras {
		cfg.ConnectionExtras = append(cfg.ConnectionExtras, maa.ConnectionExtra{Name: extra.Name, JSON: extra.JSON})
	}
	return cfg
}

// bindingInstanceConfig maps the ordered instance options; instance
// configuration does not override the runtime's limit set.
func bindingInstanceConfig(req protocol.InitializeRequest) maa.InstanceConfig {
	var cfg maa.InstanceConfig
	for _, opt := range req.InstanceOptions {
		cfg.Options = append(cfg.Options, maa.InstanceOption{Key: opt.Key, Value: opt.Value})
	}
	return cfg
}

type nativeEngine struct {
	runtime  *maa.Runtime
	instance *maa.Instance
}

func openEngine(req protocol.InitializeRequest) (_ engine, info protocol.RuntimeInfo, err error) {
	if req.SnapshotID == "" || len(req.SnapshotID) > 256 || !utf8.ValidString(req.SnapshotID) || strings.ContainsRune(req.SnapshotID, 0) {
		return nil, info, wireError(protocol.InvalidInput, "snapshot_id must be a nonempty string of at most 256 bytes")
	}
	for _, path := range append([]string{req.LibraryPath, req.UserDir}, req.ResourceRoots...) {
		if !filepath.IsAbs(path) || !utf8.ValidString(path) || strings.ContainsRune(path, 0) {
			return nil, info, wireError(protocol.InvalidInput, "library, user and resource paths must be explicit absolute paths")
		}
	}
	if len(req.ResourceRoots) == 0 {
		return nil, info, wireError(protocol.InvalidInput, "resource_roots must not be empty")
	}
	info.SnapshotID = req.SnapshotID
	info.LibrarySHA256, err = hashFile(req.LibraryPath)
	if err != nil {
		return nil, info, wireError(protocol.LoadFailed, fmt.Sprintf("read library: %v", err))
	}
	if req.ExpectedLibrarySHA256 != "" && req.ExpectedLibrarySHA256 != info.LibrarySHA256 {
		return nil, info, wireError(protocol.SnapshotMismatch, "library SHA256 does not match expected_library_sha256")
	}
	for _, root := range req.ResourceRoots {
		digest, hashErr := hashFile(filepath.Join(root, "resource", "version.json"))
		if hashErr != nil && !errors.Is(hashErr, os.ErrNotExist) {
			return nil, info, wireError(protocol.InvalidInput, fmt.Sprintf("read resource version: %v", hashErr))
		}
		info.Resources = append(info.Resources, protocol.Resource{Root: root, VersionSHA256: digest})
	}
	cfg := bindingConfig(req)
	rt, err := maa.Open(cfg)
	if err != nil {
		return nil, info, err
	}
	defer func() {
		if err != nil {
			_ = rt.Close()
		}
	}()
	info.CoreVersion = rt.Version()
	if req.ExpectedCoreVersion != "" && req.ExpectedCoreVersion != info.CoreVersion {
		return nil, info, wireError(protocol.SnapshotMismatch, "Core version does not match expected_core_version")
	}
	caps := rt.Capabilities()
	info.Capabilities = protocol.Capabilities{ImageBGR: caps.ImageBgr(), AttachWindow: caps.AsyncAttachWindow(), MapLevelKey: caps.MapLevelKey(), ItemName: caps.ItemName()}
	info.NullSize = rt.NullSize()
	// The applied limit set is what the session passed in; report a detached
	// copy so the response never aliases the request.
	if req.Limits != nil {
		applied := *req.Limits
		info.Limits = &applied
	}
	in, err := rt.NewInstance(bindingInstanceConfig(req))
	if err != nil {
		return nil, info, err
	}
	return &nativeEngine{runtime: rt, instance: in}, info, nil
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	if _, err = io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (e *nativeEngine) Connect(req protocol.ConnectRequest) (operation, error) {
	op, err := e.instance.Connect(maa.ConnectConfig{ADBPath: req.ADBPath, Address: req.Address, Config: req.Config})
	if err != nil {
		return nil, err
	}
	return nativeOperation{op}, nil
}

func (e *nativeEngine) Click(req protocol.ClickRequest) (operation, error) {
	op, err := e.instance.Click(req.X, req.Y)
	if err != nil {
		return nil, err
	}
	return nativeOperation{op}, nil
}

func (e *nativeEngine) Screencap() (operation, error) {
	op, err := e.instance.Screencap()
	if err != nil {
		return nil, err
	}
	return nativeOperation{op}, nil
}

// hwndFromUint64 converts the wire window handle to platform pointer width.
// The check rejects any value the platform pointer cannot hold before the
// native call, so a wide handle is a rejection instead of a silent truncation.
func hwndFromUint64(v uint64) (uintptr, error) {
	hwnd, ok := protocol.PointerFromUint64(v)
	if !ok {
		return 0, wireError(protocol.InvalidInput, fmt.Sprintf("hwnd %d does not fit the platform pointer width", v))
	}
	return hwnd, nil
}

// AttachWindow submits the Win32 window binding. The handle is width-checked
// before the native call and the three method masks pass through bit for bit;
// a non-Windows platform or a missing optional symbol surfaces the binding's
// unsupported rejection and never falls back to an ADB submission.
func (e *nativeEngine) AttachWindow(req protocol.AttachWindowRequest) (operation, error) {
	hwnd, err := hwndFromUint64(req.Hwnd)
	if err != nil {
		return nil, err
	}
	op, err := e.instance.AttachWindow(hwnd, maa.Win32ScreencapMethod(req.ScreencapMethod), maa.Win32InputMethod(req.MouseMethod), maa.Win32InputMethod(req.KeyboardMethod))
	if err != nil {
		return nil, err
	}
	return nativeOperation{op}, nil
}

type nativeOperation struct{ op *maa.Operation }

func (o nativeOperation) ID() int32 { return int32(o.op.ID()) }
func (o nativeOperation) Wait(ctx context.Context) (protocol.OperationResult, error) {
	r, err := o.op.Wait(ctx)
	return protocol.OperationResult{AsyncID: int32(o.op.ID()), What: r.What, OK: r.OK, Details: r.Details}, err
}

func (e *nativeEngine) AppendTask(req protocol.TaskRequest) (int32, error) {
	id, err := e.instance.AppendTask(req.Type, req.Params)
	return int32(id), err
}
func (e *nativeEngine) SetTaskParams(req protocol.TaskParamsRequest) error {
	return e.instance.SetTaskParams(maa.TaskID(req.TaskID), req.Params)
}
func (e *nativeEngine) Start() error { return e.instance.Start() }
func (e *nativeEngine) Stop() (bool, error) {
	if err := e.instance.Stop(); err != nil {
		return false, err
	}
	running, err := e.instance.Running()
	return !running && err == nil, err
}

// BackToHome requests the instance to return to the home screen. The binding
// wraps a native false into ErrNativeRejected, which classifies as
// native_rejected; a nil return never implies the screen action completed.
func (e *nativeEngine) BackToHome() error { return e.instance.BackToHome() }
func (e *nativeEngine) Status() (protocol.ExecutionStatus, error) {
	var s protocol.ExecutionStatus
	var err error
	if s.Running, err = e.instance.Running(); err != nil {
		return s, err
	}
	if s.Connected, err = e.instance.Connected(); err != nil {
		return s, err
	}
	tasks, err := e.instance.Tasks()
	s.Tasks = make([]int32, len(tasks))
	for i, id := range tasks {
		s.Tasks[i] = int32(id)
	}
	return s, err
}

// UUID reads the device UUID from the Core buffer. The read never connects or
// performs any other device action, and the empty string is a normal result.
func (e *nativeEngine) UUID() (protocol.UUIDResult, error) {
	uuid, err := e.instance.UUID()
	return protocol.UUIDResult{UUID: uuid}, err
}
func (e *nativeEngine) Observe(ctx context.Context, emit func(protocol.Callback) error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.instance.Fault():
			return e.instance.Err()
		case event, ok := <-e.instance.Events():
			if !ok {
				// Close reports a Stop rejection through its response. Only an
				// actual binding fault interrupts the transport independently.
				return e.instance.Diagnostics().FirstFault
			}
			if err := emit(protocol.Callback{MessageID: int32(event.MessageID), CoreSequence: event.Sequence, JSON: event.JSON}); err != nil {
				return err
			}
		}
	}
}
func (e *nativeEngine) Close() error { return e.runtime.Close() }

func wireError(code, message string) *protocol.Error {
	if len(message) > 4096 {
		message = message[:4093]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
		message += "..."
	}
	return &protocol.Error{Code: code, Message: message}
}

func classify(err error) *protocol.Error {
	if err == nil {
		return nil
	}
	var existing *protocol.Error
	if errors.As(err, &existing) {
		return existing
	}
	code := protocol.InternalError
	var load *maa.LibraryLoadError
	var symbol *maa.SymbolLookupError
	var registration *maa.FunctionRegistrationError
	switch {
	case errors.As(err, &load):
		code = protocol.LoadFailed
	case errors.As(err, &symbol):
		code = protocol.SymbolMissing
	case errors.As(err, &registration):
		code = protocol.RegistrationFailed
	case errors.Is(err, maa.ErrInvalidInput):
		code = protocol.InvalidInput
	case errors.Is(err, maa.ErrEventOverflow), errors.Is(err, protocol.ErrQueueFull):
		code = protocol.EventOverflow
	case errors.Is(err, maa.ErrProtocol):
		code = protocol.ProtocolError
	case errors.Is(err, maa.ErrUnsupported):
		code = protocol.Unsupported
	case errors.Is(err, maa.ErrNativeRejected):
		code = protocol.NativeRejected
	case errors.Is(err, maa.ErrBufferLimit), errors.Is(err, maa.ErrOperationLimit):
		code = protocol.LimitExceeded
	case errors.Is(err, maa.ErrClosed), errors.Is(err, maa.ErrClosing), errors.Is(err, maa.ErrRuntimeInUse), errors.Is(err, maa.ErrRuntimeTerminated):
		code = protocol.InvalidState
	}
	return wireError(code, err.Error())
}
