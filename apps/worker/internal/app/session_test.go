package app

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	maa "github.com/dongwlin/rhodeck/packages/maa-go"
	"github.com/stretchr/testify/require"
)

// fakeInstance mimics the native instance's observation surface: an event
// stream that closes as the instance reaches its terminal state, a one-shot
// fault notification independent of that stream, and a bounded diagnostics
// snapshot that is read after the terminal notification.
type fakeInstance struct {
	events    chan maa.Event
	fault     chan struct{}
	done      chan struct{}
	faultOnce sync.Once
	doneOnce  sync.Once
	// sendMu serializes producers against the terminal close: the binding's
	// dispatcher stops before the stream closes, and the fake gives test
	// producers the same guarantee.
	sendMu   sync.Mutex
	faultErr error
	snapshot func() maa.InstanceDiagnostics
}

func newFakeInstance() *fakeInstance {
	return &fakeInstance{events: make(chan maa.Event, 256), fault: make(chan struct{}), done: make(chan struct{})}
}

func (fi *fakeInstance) Events() <-chan maa.Event { return fi.events }
func (fi *fakeInstance) Fault() <-chan struct{}   { return fi.fault }
func (fi *fakeInstance) Done() <-chan struct{}    { return fi.done }

// Diagnostics returns the configured snapshot; like the binding, an unset
// configuration is the zero snapshot — or the recorded first fault — not an
// error.
func (fi *fakeInstance) Diagnostics() maa.InstanceDiagnostics {
	if fi.snapshot != nil {
		return fi.snapshot()
	}
	return maa.InstanceDiagnostics{FirstFault: fi.faultErr}
}

// faultNow records the first fault and fires the one-shot notification, the
// binding's linearization order.
func (fi *fakeInstance) faultNow(cause error) {
	fi.faultOnce.Do(func() {
		fi.faultErr = cause
		close(fi.fault)
	})
}

// sendEvent delivers one event the way the binding's dispatcher does: it
// stops at the terminal state instead of sending on a closed stream, so test
// producers can never race the close. A producer blocked on a full stream
// holds the gate, so the close waits for it — tests keep their floods within
// the buffer capacity, like real dispatchers bounded by their budgets.
func (fi *fakeInstance) sendEvent(ev maa.Event) {
	fi.sendMu.Lock()
	defer fi.sendMu.Unlock()
	select {
	case <-fi.done:
		return
	default:
	}
	select {
	case fi.events <- ev:
	case <-fi.done:
	}
}

// finish terminates the fake instance: the event stream closes first, then
// the terminal notification, like the binding's close completion. The send
// gate guarantees no producer is mid-send when the stream closes.
func (fi *fakeInstance) finish() {
	fi.doneOnce.Do(func() {
		fi.sendMu.Lock()
		defer fi.sendMu.Unlock()
		close(fi.events)
		close(fi.done)
	})
}

type fakeEngine struct {
	inst       *fakeInstance
	closes     atomic.Int32
	task       protocol.TaskRequest
	params     protocol.TaskParamsRequest
	connect    protocol.ConnectRequest
	click      protocol.ClickRequest
	attach     protocol.AttachWindowRequest
	logInput   protocol.LogRequest
	queryKey   string
	queryID    string
	start      func() error
	backToHome func() error
	stop       func() (bool, error)
	status     func() (protocol.ExecutionStatus, error)
	uuid       func() (protocol.UUIDResult, error)
	log        func(protocol.LogRequest) error
	mapLevel   func(protocol.MapLevelKeyRequest) (protocol.MapLevelKeyResult, error)
	itemName   func(protocol.ItemNameRequest) (protocol.ItemNameResult, error)
	imagePNG   func() ([]byte, error)
	imageBGR   func() ([]byte, error)
	closeHook  func()
	closeError error
	// hooks replace the default submission behavior; the default returns the
	// engine's shared operation without touching the native side.
	connectHook   func(protocol.ConnectRequest) (operation, error)
	clickHook     func(protocol.ClickRequest) (operation, error)
	screencapHook func() (operation, error)
	attachHook    func(protocol.AttachWindowRequest) (operation, error)
	op            *fakeOperation
	created       []*fakeOperation
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{inst: newFakeInstance()}
}
func (f *fakeEngine) Connect(req protocol.ConnectRequest) (operation, error) {
	f.connect = req
	if f.connectHook != nil {
		return f.connectHook(req)
	}
	return f.op, nil
}
func (f *fakeEngine) Click(req protocol.ClickRequest) (operation, error) {
	f.click = req
	if f.clickHook != nil {
		return f.clickHook(req)
	}
	return f.op, nil
}
func (f *fakeEngine) Screencap() (operation, error) {
	if f.screencapHook != nil {
		return f.screencapHook()
	}
	return f.op, nil
}
func (f *fakeEngine) AttachWindow(req protocol.AttachWindowRequest) (operation, error) {
	f.attach = req
	if f.attachHook != nil {
		return f.attachHook(req)
	}
	return f.op, nil
}
func (f *fakeEngine) AppendTask(req protocol.TaskRequest) (int32, error) {
	f.task = req
	return 23, nil
}
func (f *fakeEngine) SetTaskParams(req protocol.TaskParamsRequest) error {
	f.params = req
	return nil
}
func (f *fakeEngine) Start() error {
	if f.start != nil {
		return f.start()
	}
	return nil
}
func (f *fakeEngine) Stop() (bool, error) {
	if f.stop != nil {
		return f.stop()
	}
	return true, nil
}
func (f *fakeEngine) BackToHome() error {
	if f.backToHome != nil {
		return f.backToHome()
	}
	return nil
}
func (f *fakeEngine) Status() (protocol.ExecutionStatus, error) {
	if f.status != nil {
		return f.status()
	}
	return protocol.ExecutionStatus{Tasks: []int32{23}}, nil
}
func (f *fakeEngine) UUID() (protocol.UUIDResult, error) {
	if f.uuid != nil {
		return f.uuid()
	}
	return protocol.UUIDResult{}, nil
}
func (f *fakeEngine) Log(input protocol.LogRequest) error {
	f.logInput = input
	if f.log != nil {
		return f.log(input)
	}
	return nil
}
func (f *fakeEngine) MapLevelKey(query protocol.MapLevelKeyRequest) (protocol.MapLevelKeyResult, error) {
	f.queryKey = query.Key
	if f.mapLevel != nil {
		return f.mapLevel(query)
	}
	return protocol.MapLevelKeyResult{}, nil
}
func (f *fakeEngine) ItemName(query protocol.ItemNameRequest) (protocol.ItemNameResult, error) {
	f.queryID = query.ID
	if f.itemName != nil {
		return f.itemName(query)
	}
	return protocol.ItemNameResult{}, nil
}
func (f *fakeEngine) GetImagePNG() ([]byte, error) {
	if f.imagePNG != nil {
		return f.imagePNG()
	}
	return []byte{}, nil
}
func (f *fakeEngine) GetImageBGR() ([]byte, error) {
	if f.imageBGR != nil {
		return f.imageBGR()
	}
	return []byte{}, nil
}
func (f *fakeEngine) Observe(ctx context.Context, emit eventEmitter) error {
	return observeInstance(ctx, f.inst, emit)
}
func (f *fakeEngine) Close() error {
	f.closes.Add(1)
	if f.closeHook != nil {
		f.closeHook()
	}
	f.inst.finish()
	for _, op := range f.created {
		op.finish()
	}
	if f.op != nil {
		f.op.finish()
	}
	return f.closeError
}

type fakeOperation struct {
	id     int32
	done   chan struct{}
	once   sync.Once
	result protocol.OperationResult
	err    error
}

// newFakeOperation builds a pending operation that, once finished, reports a
// native rejection whose details keep a >2^53 integer verbatim.
func newFakeOperation(id int32) *fakeOperation {
	return &fakeOperation{
		id:   id,
		done: make(chan struct{}),
		result: protocol.OperationResult{
			AsyncID: id, What: "Connect", OK: false,
			Details: json.RawMessage(`{"ret":false,"large":9007199254740993}`),
		},
		err: maa.ErrNativeRejected,
	}
}

func (o *fakeOperation) ID() int32 { return o.id }
func (o *fakeOperation) finish()   { o.once.Do(func() { close(o.done) }) }
func (o *fakeOperation) Wait(ctx context.Context) (protocol.OperationResult, error) {
	select {
	case <-o.done:
		return o.result, o.err
	case <-ctx.Done():
		return protocol.OperationResult{AsyncID: o.id}, ctx.Err()
	}
}

// submit registers a created operation so the engine's close settles it, then
// returns it as the submission result.
func (f *fakeEngine) submit(op *fakeOperation) (operation, error) {
	f.created = append(f.created, op)
	return op, nil
}

type sessionHarness struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *io.PipeReader
	reader *protocol.Reader
	done   <-chan error
	id     uint64
	events []protocol.Message
}

func harness(t *testing.T, factory engineFactory) *sessionHarness {
	t.Helper()
	input, toWorker := io.Pipe()
	fromWorker, output := io.Pipe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	done := make(chan error, 1)
	go func() { done <- serve(ctx, input, output, factory, 30*time.Millisecond, "dev"); output.Close() }()
	h := &sessionHarness{t: t, in: toWorker, out: fromWorker, reader: protocol.NewReader(fromWorker), done: done}
	t.Cleanup(func() { cancel(); toWorker.Close(); fromWorker.Close() })
	m := h.read()
	require.Equal(t, protocol.Hello, m.Type, "first frame: %+v", m)
	return h
}

func (h *sessionHarness) read() protocol.Message {
	h.t.Helper()
	m, err := h.reader.Read()
	require.NoError(h.t, err)
	return m
}
func (h *sessionHarness) send(name string, body any) uint64 {
	h.t.Helper()
	h.id++
	m, err := protocol.NewMessage(protocol.Request, name, h.id, body)
	require.NoError(h.t, err)
	data, err := protocol.Encode(m)
	require.NoError(h.t, err)
	_, err = h.in.Write(data)
	require.NoError(h.t, err)
	return h.id
}
func (h *sessionHarness) response(id uint64) protocol.Message {
	h.t.Helper()
	for {
		m := h.read()
		if m.Type == protocol.Event {
			h.events = append(h.events, m)
			continue
		}
		require.Equal(h.t, id, m.ID, "response ID = %d, want %d", m.ID, id)
		return m
	}
}
func (h *sessionHarness) call(name string, body any) protocol.Message {
	return h.response(h.send(name, body))
}
func (h *sessionHarness) initialize() {
	h.t.Helper()
	m := h.call(protocol.Initialize, protocol.InitializeRequest{SnapshotID: "test"})
	require.Nil(h.t, m.Error, "unexpected error: %v", m.Error)
}
func fakeFactory(f *fakeEngine) engineFactory {
	return func(protocol.InitializeRequest) (engine, protocol.RuntimeInfo, error) {
		return f, protocol.RuntimeInfo{SnapshotID: "test", CoreVersion: "test-core"}, nil
	}
}

func TestSessionRawTasksAndUnknownCallbacks(t *testing.T) {
	f := newFakeEngine()
	h := harness(t, fakeFactory(f))
	m := h.call(protocol.Start, nil)
	require.NotNil(t, m.Error, "pre-init start: %+v", m)
	require.Equal(t, protocol.InvalidState, m.Error.Code, "pre-init start: %+v", m)
	h.initialize()
	m = h.call(protocol.Initialize, protocol.InitializeRequest{})
	require.NotNil(t, m.Error, "second init: %+v", m)
	require.Equal(t, protocol.InvalidState, m.Error.Code, "second init: %+v", m)
	raw := json.RawMessage(`{"future":900719925474099312345}`)
	m = h.call(protocol.AppendTask, protocol.TaskRequest{Type: "FutureTask", Params: raw})
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	require.Equal(t, "FutureTask", f.task.Type, "task changed: %+v", f.task)
	require.Equal(t, string(raw), string(f.task.Params), "task changed: %+v", f.task)
	params := json.RawMessage(`{"enable":false,"future":9007199254740993,"nested":{"deep":900719925474099312345}}`)
	m = h.call(protocol.SetTaskParams, protocol.TaskParamsRequest{TaskID: -23, Params: params})
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	require.Equal(t, int32(-23), f.params.TaskID, "task params changed: %+v", f.params)
	require.Equal(t, string(params), string(f.params.Params), "task params changed: %+v", f.params)
	f.inst.sendEvent(maa.Event{MessageID: 7654321, Sequence: 1, JSON: raw})
	m = h.read()
	var callback protocol.Callback
	require.NoError(t, protocol.Decode(m.Payload, &callback))
	require.Equal(t, uint64(1), m.Sequence, "callback changed: %+v", m)
	require.Equal(t, int32(7654321), callback.MessageID, "callback changed: %+v", m)
	require.Equal(t, string(raw), string(callback.JSON), "callback changed: %+v", m)
	m = h.call(protocol.Close, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	require.NoError(t, <-h.done)
	require.Equal(t, int32(1), f.closes.Load(), "close count: %d", f.closes.Load())
}

// TestSessionRuntimeLogAndExperimentQueries proves the wire contract of the
// process-level log and the two experiment queries: the log input reaches the
// engine verbatim (empty strings included) and the C void return is answered
// as an empty object that states only "the call returned"; the four-field
// level query transfers every field without loss; zero-valued query results,
// empty names and the native "Unknown" stay normal results; and capability
// absence, input rejections, native contract breaches and lifecycle states
// keep their stable codes with the session intact.
func TestSessionRuntimeLogAndExperimentQueries(t *testing.T) {
	t.Run("log input is verbatim and the void return is no completion fact", func(t *testing.T) {
		f := newFakeEngine()
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.Log, protocol.LogRequest{Level: "warn", Message: " hello 世界 "})
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.Equal(t, "warn", f.logInput.Level, "log input changed: %+v", f.logInput)
		require.Equal(t, " hello 世界 ", f.logInput.Message, "log input changed: %+v", f.logInput)
		// Empty strings are valid inputs; the empty-object response only means
		// the call was issued and returned.
		m = h.call(protocol.Log, protocol.LogRequest{})
		require.Nil(t, m.Error, "empty log input: %+v payload %s", m, m.Payload)
		require.Equal(t, "{}", string(m.Payload), "empty log input: %+v payload %s", m, m.Payload)
		require.Empty(t, f.logInput.Level, "empty log input changed: %+v", f.logInput)
		require.Empty(t, f.logInput.Message, "empty log input changed: %+v", f.logInput)
		// A hook error is classified, not swallowed into an empty success.
		f.log = func(protocol.LogRequest) error { return maa.ErrRuntimeTerminated }
		m = h.call(protocol.Log, protocol.LogRequest{Level: "x", Message: "y"})
		require.NotNil(t, m.Error, "terminated log: %+v", m)
		require.Equal(t, protocol.InvalidState, m.Error.Code, "terminated log: %+v", m)
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session unusable after log calls: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("four-field query transfers without loss", func(t *testing.T) {
		f := newFakeEngine()
		want := protocol.MapLevelKeyResult{StageID: "main-07", Code: "1-7", LevelID: "level_07", Name: "粉碎矿山"}
		f.mapLevel = func(protocol.MapLevelKeyRequest) (protocol.MapLevelKeyResult, error) { return want, nil }
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.MapLevelKey, protocol.MapLevelKeyRequest{Key: "粉碎矿山"})
		require.Nil(t, m.Error, "map_level_key: %+v payload %s", m, m.Payload)
		require.Equal(t, `{"stage_id":"main-07","code":"1-7","level_id":"level_07","name":"粉碎矿山"}`, string(m.Payload), "map_level_key: %+v payload %s", m, m.Payload)
		require.Equal(t, "粉碎矿山", f.queryKey, "query key changed: %q", f.queryKey)
		// The empty key passes through verbatim.
		m = h.call(protocol.MapLevelKey, protocol.MapLevelKeyRequest{})
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.Empty(t, f.queryKey, "empty key changed: %q", f.queryKey)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("zero values, empty names and Unknown are normal results", func(t *testing.T) {
		f := newFakeEngine()
		f.itemName = func(query protocol.ItemNameRequest) (protocol.ItemNameResult, error) {
			if query.ID == "" {
				return protocol.ItemNameResult{Name: "Unknown"}, nil
			}
			return protocol.ItemNameResult{}, nil
		}
		h := harness(t, fakeFactory(f))
		h.initialize()
		// Not found is the zero value: a normal result, not an error.
		m := h.call(protocol.MapLevelKey, protocol.MapLevelKeyRequest{Key: "missing"})
		require.Nil(t, m.Error, "not-found result: %+v payload %s", m, m.Payload)
		require.Equal(t, `{"stage_id":"","code":"","level_id":"","name":""}`, string(m.Payload), "not-found result: %+v payload %s", m, m.Payload)
		m = h.call(protocol.ItemName, protocol.ItemNameRequest{ID: ""})
		require.Nil(t, m.Error, "empty id result: %+v payload %s", m, m.Payload)
		require.Equal(t, `{"name":"Unknown"}`, string(m.Payload), "empty id result: %+v payload %s", m, m.Payload)
		m = h.call(protocol.ItemName, protocol.ItemNameRequest{ID: "unknown-id"})
		require.Nil(t, m.Error, "empty name result: %+v payload %s", m, m.Payload)
		require.Equal(t, `{"name":""}`, string(m.Payload), "empty name result: %+v payload %s", m, m.Payload)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("capability absence and contract breaches keep stable codes", func(t *testing.T) {
		f := newFakeEngine()
		f.log = func(protocol.LogRequest) error { return maa.ErrInvalidInput }
		f.mapLevel = func(protocol.MapLevelKeyRequest) (protocol.MapLevelKeyResult, error) {
			return protocol.MapLevelKeyResult{}, maa.ErrUnsupported
		}
		f.itemName = func(protocol.ItemNameRequest) (protocol.ItemNameResult, error) {
			return protocol.ItemNameResult{}, maa.ErrProtocol
		}
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.Log, protocol.LogRequest{Level: "l", Message: "m"})
		require.NotNil(t, m.Error, "invalid log input: %+v", m)
		require.Equal(t, protocol.InvalidInput, m.Error.Code, "invalid log input: %+v", m)
		m = h.call(protocol.MapLevelKey, protocol.MapLevelKeyRequest{Key: "k"})
		require.NotNil(t, m.Error, "absent experiment symbol: %+v", m)
		require.Equal(t, protocol.Unsupported, m.Error.Code, "absent experiment symbol: %+v", m)
		m = h.call(protocol.ItemName, protocol.ItemNameRequest{ID: "i"})
		require.NotNil(t, m.Error, "native contract breach: %+v", m)
		require.Equal(t, protocol.ProtocolError, m.Error.Code, "native contract breach: %+v", m)
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session unusable after classified rejections: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})
}

// TestSessionUnknownCommandsStayUnsupported proves the stable rejection floor
// of the frozen v1 table: a command name the build does not implement is
// answered with the unsupported code and the session survives. The last
// reserved names (log, map_level_key, item_name) became implemented commands
// in WKR-07, so this now guards the default branch itself.
func TestSessionUnknownCommandsStayUnsupported(t *testing.T) {
	f := newFakeEngine()
	h := harness(t, fakeFactory(f))
	h.initialize()
	for _, name := range []string{"frobnicate", "log2", "MAP_LEVEL_KEY"} {
		m := h.call(name, nil)
		require.NotNil(t, m.Error, "%s must be rejected with unsupported, got %+v", name, m)
		require.Equal(t, protocol.Unsupported, m.Error.Code, "%s must be rejected with unsupported, got %+v", name, m)
	}
	m := h.call(protocol.Ping, nil)
	require.Nil(t, m.Error, "session did not survive unknown rejections: %+v", m)
}

// limitsRecorder records the initialize requests one factory received and
// echoes the applied limit set back in the runtime info.
type limitsRecorder struct {
	calls atomic.Int32
	last  protocol.InitializeRequest
}

func limitsFactory(rec *limitsRecorder) engineFactory {
	return func(req protocol.InitializeRequest) (engine, protocol.RuntimeInfo, error) {
		rec.calls.Add(1)
		rec.last = req
		info := protocol.RuntimeInfo{
			SnapshotID:   "test",
			CoreVersion:  "test-core",
			Capabilities: protocol.Capabilities{ImageBGR: true, AttachWindow: true, MapLevelKey: false, ItemName: true},
			NullSize:     math.MaxUint64,
		}
		if req.Limits != nil {
			echoed := *req.Limits
			info.Limits = &echoed
		}
		return newFakeEngine(), info, nil
	}
}

// TestSessionAppliesLimitsContract proves the worker applies the caller's
// limit configuration instead of ignoring it: omitted limits select the whole
// materialized default set, explicit limits are applied verbatim and echoed in
// the initialize response, and a configuration above the frozen wire budgets
// is rejected with limit_exceeded and the effective maximum before the native
// factory is touched, leaving the session alive.
func TestSessionAppliesLimitsContract(t *testing.T) {
	t.Run("omitted limits select the whole default set", func(t *testing.T) {
		rec := &limitsRecorder{}
		h := harness(t, limitsFactory(rec))
		m := h.call(protocol.Initialize, protocol.InitializeRequest{SnapshotID: "test"})
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.Equal(t, int32(1), rec.calls.Load(), "factory limits = %+v, want the materialized defaults", rec.last.Limits)
		require.NotNil(t, rec.last.Limits, "factory limits = %+v, want the materialized defaults", rec.last.Limits)
		require.Equal(t, bindingDefaultLimits, *rec.last.Limits, "factory limits = %+v, want the materialized defaults", rec.last.Limits)
		var info protocol.RuntimeInfo
		require.NoError(t, protocol.Decode(m.Payload, &info))
		require.Equal(t, uint64(math.MaxUint64), info.NullSize, "null_size = %d, want the locked sentinel", info.NullSize)
		require.NotNil(t, info.Limits, "reported limits = %+v, want the applied defaults", info.Limits)
		require.Equal(t, bindingDefaultLimits, *info.Limits, "reported limits = %+v, want the applied defaults", info.Limits)
		require.True(t, info.Capabilities.ImageBGR, "capabilities changed: %+v", info.Capabilities)
		require.True(t, info.Capabilities.AttachWindow, "capabilities changed: %+v", info.Capabilities)
		require.False(t, info.Capabilities.MapLevelKey, "capabilities changed: %+v", info.Capabilities)
		require.True(t, info.Capabilities.ItemName, "capabilities changed: %+v", info.Capabilities)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("explicit limits are applied verbatim", func(t *testing.T) {
		rec := &limitsRecorder{}
		h := harness(t, limitsFactory(rec))
		explicit := protocol.Limits{
			InputStringBytes:  65_536,
			InputJSONBytes:    1 << 20,
			ConfigEntries:     128,
			ConfigBytes:       4 << 20,
			ImagePNG:          protocol.BufferLimits{Initial: 1 << 20, Max: 32 << 20, GrowthRetries: 5},
			ImageBGR:          protocol.BufferLimits{Initial: 4 << 20, Max: 128 << 20, GrowthRetries: 5},
			UUID:              protocol.BufferLimits{Initial: 64, Max: 4096, GrowthRetries: 6},
			Tasks:             protocol.BufferLimits{Initial: 64, Max: 1 << 16, GrowthRetries: 3},
			CallbackBytes:     1 << 20,
			InternalEvents:    512,
			InternalBytes:     16 << 20,
			PublicEvents:      512,
			PublicBytes:       16 << 20,
			PendingOperations: 64,
			EarlyResults:      1,
			EarlyBytes:        1 << 20,
		}
		m := h.call(protocol.Initialize, protocol.InitializeRequest{SnapshotID: "test", Limits: &explicit})
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.Equal(t, int32(1), rec.calls.Load(), "factory limits = %+v, want the explicit configuration", rec.last.Limits)
		require.NotNil(t, rec.last.Limits, "factory limits = %+v, want the explicit configuration", rec.last.Limits)
		require.Equal(t, explicit, *rec.last.Limits, "factory limits = %+v, want the explicit configuration", rec.last.Limits)
		var info protocol.RuntimeInfo
		require.NoError(t, protocol.Decode(m.Payload, &info))
		require.NotNil(t, info.Limits, "reported limits = %+v, want the explicit configuration", info.Limits)
		require.Equal(t, explicit, *info.Limits, "reported limits = %+v, want the explicit configuration", info.Limits)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("wire budget rejection precedes the factory", func(t *testing.T) {
		rec := &limitsRecorder{}
		h := harness(t, limitsFactory(rec))
		oversized := bindingDefaultLimits
		oversized.ConfigBytes = protocol.MaxConfigBytes + 1
		m := h.call(protocol.Initialize, protocol.InitializeRequest{SnapshotID: "test", Limits: &oversized})
		require.NotNil(t, m.Error, "oversized limits: %+v", m)
		require.Equal(t, protocol.LimitExceeded, m.Error.Code, "oversized limits: %+v", m)
		require.NotNil(t, m.Error.Detail, "effective maximum not reported: %+v", m.Error.Detail)
		require.Equal(t, "Limits.ConfigBytes", m.Error.Detail.Field, "effective maximum not reported: %+v", m.Error.Detail)
		require.Equal(t, uint64(protocol.MaxConfigBytes), m.Error.Detail.Maximum, "effective maximum not reported: %+v", m.Error.Detail)
		require.Equal(t, int32(0), rec.calls.Load(), "wire rejection must not reach the native factory")
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session unusable after wire rejection: %+v", m)
		m = h.call(protocol.Initialize, protocol.InitializeRequest{SnapshotID: "test"})
		require.Nil(t, m.Error, "legal initialize after wire rejection: %+v", m)
		require.Equal(t, int32(1), rec.calls.Load(), "factory calls: %d", rec.calls.Load())
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})
}

func TestSessionAsyncCompletionDoesNotBlockStop(t *testing.T) {
	f := newFakeEngine()
	f.op = newFakeOperation(41)
	h := harness(t, fakeFactory(f))
	h.initialize()
	accepted := h.call(protocol.Connect, protocol.ConnectRequest{})
	var op protocol.OperationAccepted
	require.NoError(t, protocol.Decode(accepted.Payload, &op), "connect acceptance: %+v", accepted)
	require.Equal(t, int32(41), op.AsyncID, "connect acceptance: %+v", accepted)
	require.Nil(t, accepted.Error, "connect acceptance: %+v", accepted)
	m := h.call(protocol.Stop, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	f.op.finish()
	m = h.read()
	var result protocol.OperationResult
	require.NoError(t, protocol.Decode(m.Payload, &result))
	require.Equal(t, accepted.ID, result.RequestID, "async result: %+v", result)
	require.False(t, result.OK, "async result: %+v", result)
	require.NotNil(t, result.Error, "async result: %+v", result)
	require.Equal(t, protocol.NativeRejected, result.Error.Code, "async result: %+v", result)
	require.True(t, strings.Contains(string(result.Details), "9007199254740993"), "async result: %+v", result)
	h.call(protocol.Close, nil)
	require.NoError(t, <-h.done)
}

// TestSessionSyncControlLifecycleAdmission proves the lifecycle admission rules
// of the synchronous control commands: before initialize every native command
// is rejected with invalid_state while the session stays usable; a native
// rejection of BackToHome is native_rejected and never an empty success; and
// closing/terminated instance rejections keep their stable invalid_state code.
func TestSessionSyncControlLifecycleAdmission(t *testing.T) {
	t.Run("rejected before initialize", func(t *testing.T) {
		h := harness(t, fakeFactory(newFakeEngine()))
		for _, name := range []string{protocol.BackToHome, protocol.UUID, protocol.Status, protocol.Stop, protocol.GetImagePNG, protocol.GetImageBGR, protocol.ImageChunk, protocol.Log, protocol.MapLevelKey, protocol.ItemName} {
			m := h.call(name, nil)
			require.NotNil(t, m.Error, "pre-init %s: %+v", name, m)
			require.Equal(t, protocol.InvalidState, m.Error.Code, "pre-init %s: %+v", name, m)
		}
		m := h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session unusable after pre-init rejections: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("native rejection is not an empty success", func(t *testing.T) {
		f := newFakeEngine()
		f.backToHome = func() error { return maa.ErrNativeRejected }
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.BackToHome, nil)
		require.NotNil(t, m.Error, "back_to_home native rejection: %+v", m)
		require.Equal(t, protocol.NativeRejected, m.Error.Code, "back_to_home native rejection: %+v", m)
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session damaged by native rejection: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("closing and terminated rejections keep invalid_state", func(t *testing.T) {
		f := newFakeEngine()
		f.backToHome = func() error { return maa.ErrClosing }
		f.uuid = func() (protocol.UUIDResult, error) { return protocol.UUIDResult{}, maa.ErrRuntimeTerminated }
		f.log = func(protocol.LogRequest) error { return maa.ErrClosing }
		f.mapLevel = func(protocol.MapLevelKeyRequest) (protocol.MapLevelKeyResult, error) {
			return protocol.MapLevelKeyResult{}, maa.ErrRuntimeTerminated
		}
		f.itemName = func(protocol.ItemNameRequest) (protocol.ItemNameResult, error) {
			return protocol.ItemNameResult{}, maa.ErrClosing
		}
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.BackToHome, nil)
		require.NotNil(t, m.Error, "closing rejection: %+v", m)
		require.Equal(t, protocol.InvalidState, m.Error.Code, "closing rejection: %+v", m)
		m = h.call(protocol.UUID, nil)
		require.NotNil(t, m.Error, "terminated rejection: %+v", m)
		require.Equal(t, protocol.InvalidState, m.Error.Code, "terminated rejection: %+v", m)
		m = h.call(protocol.Log, protocol.LogRequest{Level: "l", Message: "m"})
		require.NotNil(t, m.Error, "closing log rejection: %+v", m)
		require.Equal(t, protocol.InvalidState, m.Error.Code, "closing log rejection: %+v", m)
		m = h.call(protocol.MapLevelKey, protocol.MapLevelKeyRequest{Key: "k"})
		require.NotNil(t, m.Error, "terminated query rejection: %+v", m)
		require.Equal(t, protocol.InvalidState, m.Error.Code, "terminated query rejection: %+v", m)
		m = h.call(protocol.ItemName, protocol.ItemNameRequest{ID: "i"})
		require.NotNil(t, m.Error, "closing query rejection: %+v", m)
		require.Equal(t, protocol.InvalidState, m.Error.Code, "closing query rejection: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})
}

// TestSessionUUIDAndStatusObservations proves the read-side contract: the UUID
// string is delivered verbatim, the empty UUID, empty task list and false
// running/connected observations are normal results, and a buffer-exhausted
// read (limit_exceeded) stays distinguishable from bad native output
// (protocol_error).
func TestSessionUUIDAndStatusObservations(t *testing.T) {
	t.Run("verbatim uuid and empty observations", func(t *testing.T) {
		f := newFakeEngine()
		uuids := []string{"emulator-5554", ""}
		f.uuid = func() (protocol.UUIDResult, error) {
			v := uuids[0]
			uuids = uuids[1:]
			return protocol.UUIDResult{UUID: v}, nil
		}
		f.status = func() (protocol.ExecutionStatus, error) {
			return protocol.ExecutionStatus{Tasks: []int32{}}, nil
		}
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.UUID, nil)
		require.Nil(t, m.Error, "uuid: %+v", m)
		require.Equal(t, `{"uuid":"emulator-5554"}`, string(m.Payload), "uuid: %+v", m)
		m = h.call(protocol.UUID, nil)
		require.Nil(t, m.Error, "empty uuid is a normal result: %+v", m)
		require.Equal(t, `{"uuid":""}`, string(m.Payload), "empty uuid is a normal result: %+v", m)
		m = h.call(protocol.Status, nil)
		require.Nil(t, m.Error, "empty status: %+v", m)
		require.Equal(t, `{"running":false,"connected":false,"tasks":[]}`, string(m.Payload), "empty status: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("buffer exhaustion and bad native output are distinguishable", func(t *testing.T) {
		f := newFakeEngine()
		f.uuid = func() (protocol.UUIDResult, error) { return protocol.UUIDResult{}, maa.ErrBufferLimit }
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.UUID, nil)
		require.NotNil(t, m.Error, "buffer exhaustion: %+v", m)
		require.Equal(t, protocol.LimitExceeded, m.Error.Code, "buffer exhaustion: %+v", m)
		f.uuid = func() (protocol.UUIDResult, error) { return protocol.UUIDResult{}, maa.ErrProtocol }
		m = h.call(protocol.UUID, nil)
		require.NotNil(t, m.Error, "bad native output: %+v", m)
		require.Equal(t, protocol.ProtocolError, m.Error.Code, "bad native output: %+v", m)
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session damaged by read failures: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})
}

// TestSessionStopAcceptanceAndTerminalStates keeps the three stop facts
// distinguishable: acceptance (a response without error), the stopped
// confirmation queried from the native running state, and task terminal states
// that only arrive as core events. An accepted stop with the queue still
// running reports stopped=false and invents no completion fact.
func TestSessionStopAcceptanceAndTerminalStates(t *testing.T) {
	f := newFakeEngine()
	f.stop = func() (bool, error) { return false, nil }
	h := harness(t, fakeFactory(f))
	h.initialize()
	m := h.call(protocol.Stop, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	var result protocol.StopResult
	require.NoError(t, protocol.Decode(m.Payload, &result))
	require.False(t, result.Stopped, "accepted stop must not fabricate a stopped confirmation: %+v", result)
	// A task terminal state is an independent core observation; acceptance
	// never synthesizes one.
	f.inst.sendEvent(maa.Event{MessageID: 4, Sequence: 1, JSON: json.RawMessage(`{"what":"TaskChainCompleted","details":{}}`)})
	event := h.read()
	require.Equal(t, protocol.CoreEvent, event.Name, "terminal state must arrive as a core event: %+v", event)
	require.True(t, strings.Contains(string(event.Payload), "TaskChainCompleted"), "terminal state must arrive as a core event: %+v", event)
	m = h.call(protocol.Close, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	require.NoError(t, <-h.done)
}

func TestPingRemainsResponsiveDuringNativeCall(t *testing.T) {
	f := newFakeEngine()
	entered, release := make(chan struct{}), make(chan struct{})
	f.start = func() error { close(entered); <-release; return nil }
	h := harness(t, fakeFactory(f))
	h.initialize()
	startID := h.send(protocol.Start, nil)
	<-entered
	pingID := h.send(protocol.Ping, nil)
	m := h.response(pingID)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	close(release)
	h.response(startID)
	h.call(protocol.Close, nil)
	require.NoError(t, <-h.done)
}

func TestSessionInitializationFailureIsTerminal(t *testing.T) {
	var calls atomic.Int32
	h := harness(t, func(protocol.InitializeRequest) (engine, protocol.RuntimeInfo, error) {
		calls.Add(1)
		return nil, protocol.RuntimeInfo{}, maa.ErrNativeRejected
	})
	m := h.call(protocol.Initialize, protocol.InitializeRequest{})
	require.NotNil(t, m.Error, "init error: %+v", m)
	require.Equal(t, protocol.NativeRejected, m.Error.Code, "init error: %+v", m)
	require.ErrorIs(t, <-h.done, maa.ErrNativeRejected, "exit")
	require.Equal(t, int32(1), calls.Load(), "factory called %d times", calls.Load())
}

func TestSessionCloseErrorIsReportedBeforeExit(t *testing.T) {
	f := newFakeEngine()
	f.closeError = maa.ErrNativeRejected
	h := harness(t, fakeFactory(f))
	h.initialize()
	m := h.call(protocol.Close, nil)
	require.NotNil(t, m.Error, "close diagnostic: %+v", m)
	require.Equal(t, protocol.NativeRejected, m.Error.Code, "close diagnostic: %+v", m)
	require.ErrorIs(t, <-h.done, maa.ErrNativeRejected, "close exit")
	require.Equal(t, int32(1), f.closes.Load(), "close count: %d", f.closes.Load())
}

func TestSessionProtocolDamageAndEOFRunCleanup(t *testing.T) {
	for _, frame := range []string{"", `{"version":2,"type":"request","id":2,"name":"ping"}` + "\n", `{"version":1,"type":"request","id":1,"name":"ping"}` + "\n"} {
		t.Run(frame, func(t *testing.T) {
			f := newFakeEngine()
			h := harness(t, fakeFactory(f))
			h.initialize()
			if frame == "" {
				h.in.Close()
			} else {
				_, _ = io.WriteString(h.in, frame)
			}
			require.Error(t, <-h.done, "damaged session succeeded")
			require.Equal(t, int32(1), f.closes.Load(), "close count: %d", f.closes.Load())
		})
	}
}

func TestSessionShutdownDeadlineAndOutputOverflow(t *testing.T) {
	t.Run("hung close", func(t *testing.T) {
		f := newFakeEngine()
		release, returned := make(chan struct{}), make(chan struct{})
		f.closeHook = func() { <-release; close(returned) }
		h := harness(t, fakeFactory(f))
		h.initialize()
		h.in.Close()
		err := <-h.done
		require.Error(t, err, "hung shutdown")
		require.True(t, strings.Contains(err.Error(), "shutdown deadline"), "hung shutdown: %v", err)
		close(release)
		<-returned
	})
	t.Run("slow consumer", func(t *testing.T) {
		f := newFakeEngine()
		h := harness(t, fakeFactory(f))
		h.initialize()
		for i := range 128 {
			f.inst.sendEvent(maa.Event{MessageID: 1, Sequence: uint64(i + 1), JSON: json.RawMessage(`{}`)})
		}
		require.ErrorIs(t, <-h.done, protocol.ErrQueueFull, "output overflow")
		require.Equal(t, int32(1), f.closes.Load(), "close count: %d", f.closes.Load())
	})
}

// operationEvents returns the next n operation results, draining messages the
// harness already collected plus anything still in flight.
func operationEvents(t *testing.T, h *sessionHarness, n int) []protocol.OperationResult {
	t.Helper()
	var out []protocol.OperationResult
	for len(out) < n {
		var m protocol.Message
		if len(h.events) > 0 {
			m, h.events = h.events[0], h.events[1:]
		} else {
			m = h.read()
		}
		require.Equal(t, protocol.OperationEvent, m.Name, "expected an operation event, got %+v", m)
		var r protocol.OperationResult
		require.NoError(t, protocol.Decode(m.Payload, &r))
		out = append(out, r)
	}
	return out
}

// TestSessionAsyncSubmissionFamily proves the four async submissions share one
// acceptance contract: each accepted command answers with a nonzero async_id,
// request payloads travel verbatim (including a >2^53 window handle and
// full-width masks), schema violations are invalid_input, and submission
// rejections keep their stable codes while the session survives.
func TestSessionAsyncSubmissionFamily(t *testing.T) {
	t.Run("four submissions are accepted with verbatim payloads", func(t *testing.T) {
		f := newFakeEngine()
		var next int32
		f.connectHook = func(protocol.ConnectRequest) (operation, error) { next++; return f.submit(newFakeOperation(next)) }
		f.clickHook = func(protocol.ClickRequest) (operation, error) { next++; return f.submit(newFakeOperation(next)) }
		f.screencapHook = func() (operation, error) { next++; return f.submit(newFakeOperation(next)) }
		f.attachHook = func(protocol.AttachWindowRequest) (operation, error) { next++; return f.submit(newFakeOperation(next)) }
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.Click, protocol.ClickRequest{X: -5, Y: 7})
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		var accepted protocol.OperationAccepted
		require.NoError(t, protocol.Decode(m.Payload, &accepted), "click acceptance: %+v", accepted)
		require.Equal(t, int32(1), accepted.AsyncID, "click acceptance: %+v", accepted)
		require.Equal(t, int32(-5), f.click.X, "click payload changed: %+v", f.click)
		require.Equal(t, int32(7), f.click.Y, "click payload changed: %+v", f.click)
		m = h.call(protocol.Screencap, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		attach := protocol.AttachWindowRequest{
			Hwnd:            9007199254740993, // 2^53 + 1
			ScreencapMethod: 1 << 63,
			MouseMethod:     math.MaxUint64,
			KeyboardMethod:  1 << 40,
		}
		m = h.call(protocol.AttachWindow, attach)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.Equal(t, attach, f.attach, "attach_window payload changed: %+v, want %+v", f.attach, attach)
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session unusable after four submissions: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("schema violations are invalid_input", func(t *testing.T) {
		f := newFakeEngine()
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.Click, nil)
		require.NotNil(t, m.Error, "click without payload: %+v", m)
		require.Equal(t, protocol.InvalidInput, m.Error.Code, "click without payload: %+v", m)
		m = h.call(protocol.AttachWindow, map[string]any{"hwnd": 1.5, "screencap_method": 1, "mouse_method": 0, "keyboard_method": 0})
		require.NotNil(t, m.Error, "fractional hwnd: %+v", m)
		require.Equal(t, protocol.InvalidInput, m.Error.Code, "fractional hwnd: %+v", m)
		m = h.call(protocol.AttachWindow, map[string]any{"hwnd": "9e18", "screencap_method": 1, "mouse_method": 0, "keyboard_method": 0})
		require.NotNil(t, m.Error, "exponent hwnd: %+v", m)
		require.Equal(t, protocol.InvalidInput, m.Error.Code, "exponent hwnd: %+v", m)
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session damaged by schema rejections: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})

	t.Run("submission rejections keep stable codes", func(t *testing.T) {
		f := newFakeEngine()
		f.clickHook = func(protocol.ClickRequest) (operation, error) { return nil, maa.ErrNativeRejected }
		f.screencapHook = func() (operation, error) { return nil, maa.ErrOperationLimit }
		f.attachHook = func(protocol.AttachWindowRequest) (operation, error) { return nil, maa.ErrUnsupported }
		h := harness(t, fakeFactory(f))
		h.initialize()
		m := h.call(protocol.Click, protocol.ClickRequest{X: 1, Y: 2})
		require.NotNil(t, m.Error, "click rejection: %+v", m)
		require.Equal(t, protocol.NativeRejected, m.Error.Code, "click rejection: %+v", m)
		m = h.call(protocol.Screencap, nil)
		require.NotNil(t, m.Error, "screencap operation limit: %+v", m)
		require.Equal(t, protocol.LimitExceeded, m.Error.Code, "screencap operation limit: %+v", m)
		m = h.call(protocol.AttachWindow, protocol.AttachWindowRequest{})
		require.NotNil(t, m.Error, "attach_window without the native symbol must stay unsupported, not fall back: %+v", m)
		require.Equal(t, protocol.Unsupported, m.Error.Code, "attach_window without the native symbol must stay unsupported, not fall back: %+v", m)
		require.Empty(t, f.created, "rejected submissions must not hold pending slots: %d", len(f.created))
		m = h.call(protocol.Ping, nil)
		require.Nil(t, m.Error, "session damaged by submission rejections: %+v", m)
		m = h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
	})
}

// TestSessionAsyncCompletionOrdering proves the completion path is decoupled
// from the command loop and from submission order: several operations stay in
// flight while the command loop keeps answering, one completes before the
// harness reads its acceptance, completions arrive out of submission order,
// and each event is keyed to its own request ID with an intact result.
func TestSessionAsyncCompletionOrdering(t *testing.T) {
	f := newFakeEngine()
	first, second := newFakeOperation(41), newFakeOperation(42)
	first.result.What, first.result.OK, first.err = "Connect", true, nil
	first.result.Details = json.RawMessage(`{"ret":true,"large":9007199254740993}`)
	var calls atomic.Int32
	f.connectHook = func(protocol.ConnectRequest) (operation, error) {
		if calls.Add(1) == 1 {
			op, _ := f.submit(first)
			return op, nil
		}
		op, _ := f.submit(second)
		return op, nil
	}
	h := harness(t, fakeFactory(f))
	h.initialize()
	firstID := h.send(protocol.Connect, protocol.ConnectRequest{})
	secondID := h.send(protocol.Connect, protocol.ConnectRequest{})
	for _, tc := range []struct {
		id   uint64
		want int32
	}{{firstID, 41}, {secondID, 42}} {
		m := h.response(tc.id)
		var accepted protocol.OperationAccepted
		require.NoError(t, protocol.Decode(m.Payload, &accepted), "acceptance %d: %+v", tc.id, accepted)
		require.Equal(t, tc.want, accepted.AsyncID, "acceptance %d: %+v", tc.id, accepted)
	}
	m := h.call(protocol.Stop, nil)
	require.Nil(t, m.Error, "command loop blocked by in-flight operations: %+v", m)
	// The second operation completes out of submission order while the first
	// is still in flight.
	second.finish()
	completion := operationEvents(t, h, 1)[0]
	require.Equal(t, int32(42), completion.AsyncID, "second completion: %+v (request %d)", completion, secondID)
	require.Equal(t, secondID, completion.RequestID, "second completion: %+v (request %d)", completion, secondID)
	require.False(t, completion.OK, "second completion: %+v (request %d)", completion, secondID)
	require.NotNil(t, completion.Error, "native failure facts: %+v", completion)
	require.Equal(t, protocol.NativeRejected, completion.Error.Code, "native failure facts: %+v", completion)
	require.True(t, strings.Contains(string(completion.Details), "9007199254740993"), "native failure facts: %+v", completion)
	first.finish()
	completion = operationEvents(t, h, 1)[0]
	require.Equal(t, int32(41), completion.AsyncID, "first completion: %+v (request %d)", completion, firstID)
	require.Equal(t, firstID, completion.RequestID, "first completion: %+v (request %d)", completion, firstID)
	require.True(t, completion.OK, "first completion: %+v (request %d)", completion, firstID)
	require.Nil(t, completion.Error, "first completion: %+v (request %d)", completion, firstID)
	require.True(t, strings.Contains(string(completion.Details), "9007199254740993"), "success details changed: %s", completion.Details)
	// A third submission completes before the session can wait for it at all:
	// the early completion event still follows its acceptance response.
	third := newFakeOperation(43)
	third.result.What, third.result.OK, third.err = "Connect", true, nil
	f.connectHook = func(protocol.ConnectRequest) (operation, error) {
		third.finish()
		op, _ := f.submit(third)
		return op, nil
	}
	thirdID := h.send(protocol.Connect, protocol.ConnectRequest{})
	m = h.response(thirdID)
	var accepted protocol.OperationAccepted
	require.NoError(t, protocol.Decode(m.Payload, &accepted), "third acceptance: %+v", accepted)
	require.Equal(t, int32(43), accepted.AsyncID, "third acceptance: %+v", accepted)
	completion = operationEvents(t, h, 1)[0]
	require.Equal(t, int32(43), completion.AsyncID, "early completion: %+v (request %d)", completion, thirdID)
	require.Equal(t, thirdID, completion.RequestID, "early completion: %+v (request %d)", completion, thirdID)
	require.True(t, completion.OK, "early completion: %+v (request %d)", completion, thirdID)
	m = h.call(protocol.Close, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	require.NoError(t, <-h.done)
}

// TestSessionCloseSettlesPendingOperations proves a close settles every
// pending operation as a lifecycle result (invalid_state, no fabricated What)
// before the close response, and the worker still exits cleanly.
func TestSessionCloseSettlesPendingOperations(t *testing.T) {
	f := newFakeEngine()
	op := newFakeOperation(41)
	op.result = protocol.OperationResult{AsyncID: 41}
	op.err = maa.ErrClosed
	f.op = op
	h := harness(t, fakeFactory(f))
	h.initialize()
	accepted := h.call(protocol.Connect, protocol.ConnectRequest{})
	require.Nil(t, accepted.Error, "unexpected error: %v", accepted.Error)
	m := h.call(protocol.Close, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	// The terminal diagnostics and the operation settlement race each other on
	// the way out (neither ordering is frozen), so filter by name instead of
	// assuming one precedes the other.
	settlements := namedEvents(h, protocol.OperationEvent)
	require.Len(t, settlements, 1, "expected exactly one settlement event, got %d", len(settlements))
	var settlement protocol.OperationResult
	require.NoError(t, protocol.Decode(settlements[0].Payload, &settlement))
	require.Equal(t, accepted.ID, settlement.RequestID, "close settlement: %+v", settlement)
	require.Equal(t, int32(41), settlement.AsyncID, "close settlement: %+v", settlement)
	require.False(t, settlement.OK, "close settlement: %+v", settlement)
	require.Empty(t, settlement.What, "close settlement: %+v", settlement)
	require.NotNil(t, settlement.Error, "lifecycle settlement must be invalid_state: %+v", settlement.Error)
	require.Equal(t, protocol.InvalidState, settlement.Error.Code, "lifecycle settlement must be invalid_state: %+v", settlement.Error)
	require.NoError(t, <-h.done)
}

// TestSessionCloseReleasesRetainedTransfersAndSettlesOperations proves close
// drains the session's own state in one pass: a partially served snapshot
// stays owned until the close, the pending operation is settled as a
// lifecycle result before the close response, and the close completes without
// a second cleanup error so the process can exit.
func TestSessionCloseReleasesRetainedTransfersAndSettlesOperations(t *testing.T) {
	f := newFakeEngine()
	f.imagePNG = func() ([]byte, error) { return []byte("snapshot"), nil }
	op := newFakeOperation(41)
	op.result = protocol.OperationResult{AsyncID: 41}
	op.err = maa.ErrClosed
	f.connectHook = func(protocol.ConnectRequest) (operation, error) { return f.submit(op) }
	h := harness(t, fakeFactory(f))
	h.initialize()
	var accepted protocol.ImageTransfer
	m := h.call(protocol.GetImagePNG, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	require.NoError(t, protocol.Decode(m.Payload, &accepted))
	require.Equal(t, uint64(8), accepted.TotalBytes, "transfer: %+v", accepted)
	// Serve only the first half, so the snapshot is still retained when the
	// close arrives.
	chunk := h.call(protocol.ImageChunk, protocol.ImageChunkRequest{TransferID: accepted.TransferID, Offset: 0, Length: 3})
	require.Nil(t, chunk.Error, "unexpected error: %v", chunk.Error)
	connectID := h.send(protocol.Connect, protocol.ConnectRequest{})
	m = h.response(connectID)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	m = h.call(protocol.Close, nil)
	require.Nil(t, m.Error, "close: %+v", m)
	// The terminal diagnostics and the operation settlement race each other on
	// the way out (neither ordering is frozen), so filter by name instead of
	// assuming one precedes the other.
	settlements := namedEvents(h, protocol.OperationEvent)
	require.Len(t, settlements, 1, "expected exactly one settlement event, got %d", len(settlements))
	var settlement protocol.OperationResult
	require.NoError(t, protocol.Decode(settlements[0].Payload, &settlement))
	require.Equal(t, connectID, settlement.RequestID, "close settlement: %+v", settlement)
	require.Equal(t, int32(41), settlement.AsyncID, "close settlement: %+v", settlement)
	require.False(t, settlement.OK, "close settlement: %+v", settlement)
	require.Empty(t, settlement.What, "close settlement: %+v", settlement)
	require.NotNil(t, settlement.Error, "lifecycle settlement must be invalid_state: %+v", settlement.Error)
	require.Equal(t, protocol.InvalidState, settlement.Error.Code, "lifecycle settlement must be invalid_state: %+v", settlement.Error)
	// The close response is answered exactly once and the session exits
	// cleanly: the release path added no second close diagnostic.
	require.NoError(t, <-h.done)
	require.Equal(t, int32(1), f.closes.Load(), "close count: %d", f.closes.Load())
}

// namedEvents returns the collected session events with the given name, in
// arrival order.
func namedEvents(h *sessionHarness, name string) []protocol.Message {
	var out []protocol.Message
	for _, m := range h.events {
		if m.Name == name {
			out = append(out, m)
		}
	}
	return out
}

// TestSessionFaultReportIsIndependentOfTheEventsReader proves the frozen
// fault contract end to end at the session layer: the fault event is
// delivered on the ordered session sequence while the single Events reader is
// blocked, the first fault travels with its stable classified code, and the
// terminal snapshot follows it with the saturated lost count and a raw prefix
// that is neither valid UTF-8 nor valid JSON transported byte for byte.
func TestSessionFaultReportIsIndependentOfTheEventsReader(t *testing.T) {
	f := newFakeEngine()
	f.inst.events = make(chan maa.Event) // unbuffered: every send synchronizes with the reader
	h := harness(t, fakeFactory(f))
	h.initialize()

	// One core event: the send completes when the reader receives it, and
	// waiting for its frame leaves the reader blocked on the empty stream.
	f.inst.sendEvent(maa.Event{MessageID: 7654321, Sequence: 1, JSON: json.RawMessage(`{}`)})
	m := h.read()
	require.Equal(t, protocol.CoreEvent, m.Name, "core event: %+v", m)
	require.Equal(t, uint64(1), m.Sequence, "core event: %+v", m)

	// Arm the fault while the reader is blocked: the report must not wait
	// for any further event consumption.
	f.inst.faultNow(fmt.Errorf("probe exhausted: %w", maa.ErrEventOverflow))
	m = h.read()
	require.Equal(t, protocol.FaultEvent, m.Name, "fault event: %+v", m)
	require.Equal(t, uint64(2), m.Sequence, "fault event: %+v", m)
	var notice protocol.FaultNotice
	require.NoError(t, protocol.Decode(m.Payload, &notice))
	require.Equal(t, protocol.EventOverflow, notice.Error.Code, "fault code = %s, want event_overflow", notice.Error.Code)

	// The reader is still blocked: no further core event was consumed. The
	// close completes cleanly and delivers the terminal snapshot afterwards.
	f.inst.snapshot = func() maa.InstanceDiagnostics {
		return maa.InstanceDiagnostics{
			LostEvents: math.MaxUint64,
			FirstFault: f.inst.faultErr,
			RawPrefix:  []byte{0xDE, 0x00, 0xFF, 0xFE},
		}
	}
	m = h.call(protocol.Close, nil)
	require.Nil(t, m.Error, "unexpected error: %v", m.Error)
	require.NoError(t, <-h.done)
	reports := namedEvents(h, protocol.DiagnosticsEvent)
	require.Len(t, reports, 1, "diagnostics events = %d, want exactly one", len(reports))
	// The fault event was read above at sequence 2; the terminal snapshot
	// must follow it on the same ordered sequence.
	require.Equal(t, uint64(3), reports[0].Sequence, "the terminal snapshot must follow the fault report: sequence %d", reports[0].Sequence)
	var report protocol.InstanceDiagnostics
	require.NoError(t, protocol.Decode(reports[0].Payload, &report))
	require.Equal(t, uint64(math.MaxUint64), report.LostEvents, "lost_events = %d, want the saturated count", report.LostEvents)
	require.NotNil(t, report.FirstFault, "snapshot = %+v", report)
	require.Equal(t, protocol.EventOverflow, report.FirstFault.Code, "snapshot = %+v", report)
	require.Nil(t, report.StopDiagnostic, "snapshot = %+v", report)
	decoded, err := base64.StdEncoding.DecodeString(report.RawPrefix)
	require.NoError(t, err, "raw prefix changed")
	require.Equal(t, string([]byte{0xDE, 0x00, 0xFF, 0xFE}), string(decoded), "raw prefix changed")
}

// TestSessionDiagnosticsWithoutFault proves the terminal snapshot is a normal
// close fact: a clean close omits every optional field, and a stop-only
// diagnostic is delivered as a stop diagnostic — never fabricated into a
// fault event.
func TestSessionDiagnosticsWithoutFault(t *testing.T) {
	t.Run("clean close keeps every optional field absent", func(t *testing.T) {
		f := newFakeEngine()
		h := harness(t, fakeFactory(f))
		h.initialize()
		f.inst.snapshot = func() maa.InstanceDiagnostics { return maa.InstanceDiagnostics{LostEvents: 3} }
		m := h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
		got := namedEvents(h, protocol.FaultEvent)
		require.Empty(t, got, "a clean close must not report a fault: %+v", got)
		reports := namedEvents(h, protocol.DiagnosticsEvent)
		require.Len(t, reports, 1, "diagnostics events = %d, want exactly one", len(reports))
		var report protocol.InstanceDiagnostics
		require.NoError(t, protocol.Decode(reports[0].Payload, &report))
		require.Equal(t, uint64(3), report.LostEvents, "snapshot = %+v", report)
		require.Nil(t, report.FirstFault, "snapshot = %+v", report)
		require.Empty(t, report.RawPrefix, "snapshot = %+v", report)
		require.Nil(t, report.StopDiagnostic, "snapshot = %+v", report)
	})

	t.Run("a stop-only diagnostic is not a fault", func(t *testing.T) {
		f := newFakeEngine()
		h := harness(t, fakeFactory(f))
		h.initialize()
		f.inst.snapshot = func() maa.InstanceDiagnostics {
			return maa.InstanceDiagnostics{StopDiagnostic: fmt.Errorf("stop returned 0: %w", maa.ErrNativeRejected)}
		}
		m := h.call(protocol.Close, nil)
		require.Nil(t, m.Error, "unexpected error: %v", m.Error)
		require.NoError(t, <-h.done)
		got := namedEvents(h, protocol.FaultEvent)
		require.Empty(t, got, "a stop diagnostic must not fabricate a fault: %+v", got)
		var report protocol.InstanceDiagnostics
		require.NoError(t, protocol.Decode(namedEvents(h, protocol.DiagnosticsEvent)[0].Payload, &report))
		require.NotNil(t, report.StopDiagnostic, "snapshot = %+v", report)
		require.Equal(t, protocol.NativeRejected, report.StopDiagnostic.Code, "snapshot = %+v", report)
		require.Nil(t, report.FirstFault, "snapshot = %+v", report)
	})
}
