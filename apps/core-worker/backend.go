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
	AppendTask(protocol.TaskRequest) (int32, error)
	SetTaskParams(protocol.TaskParamsRequest) error
	Start() error
	Stop() (bool, error)
	Status() (protocol.ExecutionStatus, error)
	Observe(context.Context, func(protocol.Callback) error) error
	Close() error
}

type engineFactory func(protocol.InitializeRequest) (engine, protocol.RuntimeInfo, error)

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
	cfg := maa.RuntimeConfig{LibraryPath: req.LibraryPath, UserDir: req.UserDir, ResourceRoots: req.ResourceRoots}
	for _, opt := range req.StaticOptions {
		cfg.StaticOptions = append(cfg.StaticOptions, maa.StaticOption{Key: opt.Key, Value: opt.Value})
	}
	for _, extra := range req.ConnectionExtras {
		cfg.ConnectionExtras = append(cfg.ConnectionExtras, maa.ConnectionExtra{Name: extra.Name, JSON: extra.JSON})
	}
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
	var icfg maa.InstanceConfig
	for _, opt := range req.InstanceOptions {
		icfg.Options = append(icfg.Options, maa.InstanceOption{Key: opt.Key, Value: opt.Value})
	}
	in, err := rt.NewInstance(icfg)
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
