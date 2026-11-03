package app

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
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

type eventEmitter func(name string, payload any) error

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
	Log(protocol.LogRequest) error
	MapLevelKey(protocol.MapLevelKeyRequest) (protocol.MapLevelKeyResult, error)
	ItemName(protocol.ItemNameRequest) (protocol.ItemNameResult, error)
	GetImagePNG() ([]byte, error)
	GetImageBGR() ([]byte, error)
	Observe(context.Context, eventEmitter) error
	Close() error
}

type engineFactory func(protocol.InitializeRequest) (engine, protocol.RuntimeInfo, error)

// bindingDefaultLimits materializes the whole default limit set of the
// binding (frozen numbers of packages/maa-go's defaultLimits). When
// the caller omits limits this is the applied configuration, so the reported
// effective set is exactly the set handed to the binding; the binding
// re-validates it on every initialize, so a default change cannot diverge
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

func effectiveLimits(l *protocol.Limits) protocol.Limits {
	if l == nil {
		return bindingDefaultLimits
	}
	return *l
}

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

func hwndFromUint64(v uint64) (uintptr, error) {
	hwnd, ok := protocol.PointerFromUint64(v)
	if !ok {
		return 0, wireError(protocol.InvalidInput, fmt.Sprintf("hwnd %d does not fit the platform pointer width", v))
	}
	return hwnd, nil
}

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

func (e *nativeEngine) UUID() (protocol.UUIDResult, error) {
	uuid, err := e.instance.UUID()
	return protocol.UUIDResult{UUID: uuid}, err
}

func (e *nativeEngine) Log(req protocol.LogRequest) error {
	return e.runtime.Log(req.Level, req.Message)
}

func (e *nativeEngine) MapLevelKey(req protocol.MapLevelKeyRequest) (protocol.MapLevelKeyResult, error) {
	mk, err := e.runtime.MapLevelKey(req.Key)
	if err != nil {
		return protocol.MapLevelKeyResult{}, err
	}
	return protocol.MapLevelKeyResult{StageID: mk.StageID, Code: mk.Code, LevelID: mk.LevelID, Name: mk.Name}, nil
}

func (e *nativeEngine) ItemName(req protocol.ItemNameRequest) (protocol.ItemNameResult, error) {
	name, err := e.runtime.ItemName(req.ID)
	if err != nil {
		return protocol.ItemNameResult{}, err
	}
	return protocol.ItemNameResult{Name: name}, nil
}

func (e *nativeEngine) GetImagePNG() ([]byte, error) { return e.instance.GetImagePNG() }

func (e *nativeEngine) GetImageBGR() ([]byte, error) { return e.instance.GetImageBGR() }

// instanceObservations is the observation surface of one native instance that
// Observe consumes. *maa.Instance satisfies it; the indirection keeps the
// observation pipeline testable without a native library.
type instanceObservations interface {
	Events() <-chan maa.Event
	Fault() <-chan struct{}
	Done() <-chan struct{}
	Diagnostics() maa.InstanceDiagnostics
}

// Observe carries the binding's observations across the process boundary
// through the session's ordered event stream: every Core callback as a core
// event, the first instance fault as a fault event, and the terminal
// diagnostics snapshot as a diagnostics event.
//
// The Events stream keeps its single reader. The one-shot fault notification
// and the terminal snapshot are watched outside that reader, so an event
// flood, a stalled consumer or big-data occupancy can never delay a fault
// report — the notification channel is independent of the event queue. The
// fault report precedes the diagnostics report; the snapshot is taken once
// after native Done, when Err and the diagnostics are stable. Observe returns
// when the event stream has ended and the terminal report was made (or became
// impossible), so the close path — which waits for Observe — always answers
// after the snapshot was enqueued. Every emission failure has already failed
// the session, so a report is either delivered or the session fails.
func (e *nativeEngine) Observe(ctx context.Context, emit eventEmitter) error {
	return observeInstance(ctx, e.instance, emit)
}

// observeInstance implements the observation pipeline against one instance's
// channels. See nativeEngine.Observe for the frozen delivery contract.
func observeInstance(ctx context.Context, in instanceObservations, emit eventEmitter) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	readerDone := make(chan error, 1)
	go func() {
		for {
			select {
			case <-ctx.Done():
				readerDone <- ctx.Err()
				return
			case event, ok := <-in.Events():
				if !ok {
					readerDone <- nil
					return
				}
				if err := emit(protocol.CoreEvent, protocol.Callback{MessageID: int32(event.MessageID), CoreSequence: event.Sequence, JSON: event.JSON}); err != nil {
					readerDone <- err
					return
				}
			}
		}
	}()

	faultComplete := make(chan struct{})
	go func() {
		defer close(faultComplete)
		select {
		case <-ctx.Done():
			return
		case <-in.Fault():
			reportFault(in, emit)
		case <-in.Done():
			select {
			case <-in.Fault():
				reportFault(in, emit)
			default:
			}
		}
	}()

	terminal := make(chan error, 1)
	go func() {
		select {
		case <-ctx.Done():
			terminal <- ctx.Err()
		case <-in.Done():
			select {
			case <-faultComplete:
			case <-ctx.Done():
				terminal <- ctx.Err()
				return
			}
			terminal <- emit(protocol.DiagnosticsEvent, wireDiagnostics(in.Diagnostics()))
		}
	}()

	var readerErr, terminalErr error
	for readerDone != nil || terminal != nil {
		select {
		case err := <-readerDone:
			readerErr, readerDone = err, nil
			if err != nil {
				cancel()
			}
		case err := <-terminal:
			terminalErr, terminal = err, nil
			if err != nil {
				cancel()
			}
		}
	}
	return errors.Join(readerErr, terminalErr)
}

func reportFault(in instanceObservations, emit eventEmitter) {
	cause := in.Diagnostics().FirstFault
	if cause == nil {
		cause = errors.New("maa: fault notification fired without a recorded first fault")
	}
	_ = emit(protocol.FaultEvent, protocol.FaultNotice{Error: *classify(cause)})
}

func wireDiagnostics(snap maa.InstanceDiagnostics) protocol.InstanceDiagnostics {
	var out protocol.InstanceDiagnostics
	out.LostEvents = snap.LostEvents
	if snap.FirstFault != nil {
		out.FirstFault = classify(snap.FirstFault)
	}
	if len(snap.RawPrefix) > 0 {
		out.RawPrefix = base64.StdEncoding.EncodeToString(snap.RawPrefix)
	}
	if snap.StopDiagnostic != nil {
		out.StopDiagnostic = classify(snap.StopDiagnostic)
	}
	return out
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

func boundDetail(s string) string {
	if len(s) <= 128 {
		return s
	}
	s = s[:128]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
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
	var detail *protocol.ErrorDetail
	var load *maa.LibraryLoadError
	var symbol *maa.SymbolLookupError
	var registration *maa.FunctionRegistrationError
	switch {
	case errors.As(err, &load):
		code = protocol.LoadFailed
		detail = &protocol.ErrorDetail{Kind: "library_load", Field: boundDetail(load.LibraryName)}
	case errors.As(err, &symbol):
		code = protocol.SymbolMissing
		detail = &protocol.ErrorDetail{Kind: "symbol_lookup", Field: boundDetail(symbol.Symbol)}
	case errors.As(err, &registration):
		code = protocol.RegistrationFailed
		detail = &protocol.ErrorDetail{Kind: "function_registration", Field: boundDetail(registration.Symbol)}
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
	case errors.Is(err, maa.ErrBufferLimit):
		code, detail = protocol.LimitExceeded, &protocol.ErrorDetail{Kind: "buffer_limit"}
	case errors.Is(err, maa.ErrOperationLimit):
		code, detail = protocol.LimitExceeded, &protocol.ErrorDetail{Kind: "operation_limit"}
	case errors.Is(err, maa.ErrClosed), errors.Is(err, maa.ErrClosing), errors.Is(err, maa.ErrRuntimeInUse), errors.Is(err, maa.ErrRuntimeTerminated):
		code = protocol.InvalidState
	}
	out := wireError(code, err.Error())
	out.Detail = detail
	return out
}
