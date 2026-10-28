package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	maa "github.com/dongwlin/rhodeck/packages/maa-go"
)

type fakeEngine struct {
	events     chan protocol.Callback
	closed     chan struct{}
	closes     atomic.Int32
	task       protocol.TaskRequest
	params     protocol.TaskParamsRequest
	connect    protocol.ConnectRequest
	click      protocol.ClickRequest
	attach     protocol.AttachWindowRequest
	start      func() error
	backToHome func() error
	stop       func() (bool, error)
	status     func() (protocol.ExecutionStatus, error)
	uuid       func() (protocol.UUIDResult, error)
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
	return &fakeEngine{events: make(chan protocol.Callback, 256), closed: make(chan struct{})}
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
func (f *fakeEngine) Observe(ctx context.Context, emit func(protocol.Callback) error) error {
	for {
		select {
		case c := <-f.events:
			if err := emit(c); err != nil {
				return err
			}
		case <-f.closed:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (f *fakeEngine) Close() error {
	f.closes.Add(1)
	if f.closeHook != nil {
		f.closeHook()
	}
	close(f.closed)
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
	go func() { done <- serve(ctx, input, output, factory, 30*time.Millisecond); output.Close() }()
	h := &sessionHarness{t: t, in: toWorker, out: fromWorker, reader: protocol.NewReader(fromWorker), done: done}
	t.Cleanup(func() { cancel(); toWorker.Close(); fromWorker.Close() })
	if m := h.read(); m.Type != protocol.Hello {
		t.Fatalf("first frame: %+v", m)
	}
	return h
}

func (h *sessionHarness) read() protocol.Message {
	h.t.Helper()
	m, err := h.reader.Read()
	if err != nil {
		h.t.Fatal(err)
	}
	return m
}
func (h *sessionHarness) send(name string, body any) uint64 {
	h.t.Helper()
	h.id++
	m, err := protocol.NewMessage(protocol.Request, name, h.id, body)
	if err != nil {
		h.t.Fatal(err)
	}
	data, err := protocol.Encode(m)
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := h.in.Write(data); err != nil {
		h.t.Fatal(err)
	}
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
		if m.ID != id {
			h.t.Fatalf("response ID = %d, want %d", m.ID, id)
		}
		return m
	}
}
func (h *sessionHarness) call(name string, body any) protocol.Message {
	return h.response(h.send(name, body))
}
func (h *sessionHarness) initialize() {
	h.t.Helper()
	if m := h.call(protocol.Initialize, protocol.InitializeRequest{SnapshotID: "test"}); m.Error != nil {
		h.t.Fatal(m.Error)
	}
}
func fakeFactory(f *fakeEngine) engineFactory {
	return func(protocol.InitializeRequest) (engine, protocol.RuntimeInfo, error) {
		return f, protocol.RuntimeInfo{SnapshotID: "test", CoreVersion: "test-core"}, nil
	}
}

func TestSessionRawTasksAndUnknownCallbacks(t *testing.T) {
	f := newFakeEngine()
	h := harness(t, fakeFactory(f))
	if m := h.call(protocol.Start, nil); m.Error == nil || m.Error.Code != protocol.InvalidState {
		t.Fatalf("pre-init start: %+v", m)
	}
	h.initialize()
	if m := h.call(protocol.Initialize, protocol.InitializeRequest{}); m.Error == nil || m.Error.Code != protocol.InvalidState {
		t.Fatalf("second init: %+v", m)
	}
	raw := json.RawMessage(`{"future":900719925474099312345}`)
	if m := h.call(protocol.AppendTask, protocol.TaskRequest{Type: "FutureTask", Params: raw}); m.Error != nil {
		t.Fatal(m.Error)
	}
	if f.task.Type != "FutureTask" || string(f.task.Params) != string(raw) {
		t.Fatalf("task changed: %+v", f.task)
	}
	params := json.RawMessage(`{"enable":false,"future":9007199254740993,"nested":{"deep":900719925474099312345}}`)
	if m := h.call(protocol.SetTaskParams, protocol.TaskParamsRequest{TaskID: -23, Params: params}); m.Error != nil {
		t.Fatal(m.Error)
	}
	if f.params.TaskID != -23 || string(f.params.Params) != string(params) {
		t.Fatalf("task params changed: %+v", f.params)
	}
	f.events <- protocol.Callback{MessageID: 7654321, CoreSequence: 1, JSON: raw}
	m := h.read()
	var callback protocol.Callback
	if err := protocol.Decode(m.Payload, &callback); err != nil {
		t.Fatal(err)
	}
	if m.Sequence != 1 || callback.MessageID != 7654321 || string(callback.JSON) != string(raw) {
		t.Fatalf("callback changed: %+v", m)
	}
	if m := h.call(protocol.Close, nil); m.Error != nil {
		t.Fatal(m.Error)
	}
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
	if f.closes.Load() != 1 {
		t.Fatalf("close count: %d", f.closes.Load())
	}
}

// TestSessionReservedCommandsStayUnsupported proves the frozen v1 command
// names are answerable before they are implemented: a reserved command is
// rejected with the stable unsupported code and the session survives.
func TestSessionReservedCommandsStayUnsupported(t *testing.T) {
	f := newFakeEngine()
	h := harness(t, fakeFactory(f))
	h.initialize()
	for _, name := range []string{protocol.GetImagePNG, protocol.GetImageBGR, protocol.ImageChunk, protocol.Log, protocol.MapLevelKey, protocol.ItemName} {
		m := h.call(name, nil)
		if m.Error == nil || m.Error.Code != protocol.Unsupported {
			t.Fatalf("%s must be rejected with unsupported, got %+v", name, m)
		}
	}
	if m := h.call(protocol.Ping, nil); m.Error != nil {
		t.Fatalf("session did not survive reserved rejections: %+v", m)
	}
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
		if m.Error != nil {
			t.Fatal(m.Error)
		}
		if rec.calls.Load() != 1 || rec.last.Limits == nil || *rec.last.Limits != bindingDefaultLimits {
			t.Fatalf("factory limits = %+v, want the materialized defaults", rec.last.Limits)
		}
		var info protocol.RuntimeInfo
		if err := protocol.Decode(m.Payload, &info); err != nil {
			t.Fatal(err)
		}
		if info.NullSize != math.MaxUint64 {
			t.Fatalf("null_size = %d, want the locked sentinel", info.NullSize)
		}
		if info.Limits == nil || *info.Limits != bindingDefaultLimits {
			t.Fatalf("reported limits = %+v, want the applied defaults", info.Limits)
		}
		if !info.Capabilities.ImageBGR || !info.Capabilities.AttachWindow || info.Capabilities.MapLevelKey || !info.Capabilities.ItemName {
			t.Fatalf("capabilities changed: %+v", info.Capabilities)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
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
		if m.Error != nil {
			t.Fatal(m.Error)
		}
		if rec.calls.Load() != 1 || rec.last.Limits == nil || *rec.last.Limits != explicit {
			t.Fatalf("factory limits = %+v, want the explicit configuration", rec.last.Limits)
		}
		var info protocol.RuntimeInfo
		if err := protocol.Decode(m.Payload, &info); err != nil {
			t.Fatal(err)
		}
		if info.Limits == nil || *info.Limits != explicit {
			t.Fatalf("reported limits = %+v, want the explicit configuration", info.Limits)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("wire budget rejection precedes the factory", func(t *testing.T) {
		rec := &limitsRecorder{}
		h := harness(t, limitsFactory(rec))
		oversized := bindingDefaultLimits
		oversized.ConfigBytes = protocol.MaxConfigBytes + 1
		m := h.call(protocol.Initialize, protocol.InitializeRequest{SnapshotID: "test", Limits: &oversized})
		if m.Error == nil || m.Error.Code != protocol.LimitExceeded {
			t.Fatalf("oversized limits: %+v", m)
		}
		if m.Error.Detail == nil || m.Error.Detail.Field != "Limits.ConfigBytes" || m.Error.Detail.Maximum != protocol.MaxConfigBytes {
			t.Fatalf("effective maximum not reported: %+v", m.Error.Detail)
		}
		if rec.calls.Load() != 0 {
			t.Fatal("wire rejection must not reach the native factory")
		}
		if m := h.call(protocol.Ping, nil); m.Error != nil {
			t.Fatalf("session unusable after wire rejection: %+v", m)
		}
		if m := h.call(protocol.Initialize, protocol.InitializeRequest{SnapshotID: "test"}); m.Error != nil {
			t.Fatalf("legal initialize after wire rejection: %+v", m)
		}
		if rec.calls.Load() != 1 {
			t.Fatalf("factory calls: %d", rec.calls.Load())
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestSessionAsyncCompletionDoesNotBlockStop(t *testing.T) {
	f := newFakeEngine()
	f.op = newFakeOperation(41)
	h := harness(t, fakeFactory(f))
	h.initialize()
	accepted := h.call(protocol.Connect, protocol.ConnectRequest{})
	var op protocol.OperationAccepted
	if err := protocol.Decode(accepted.Payload, &op); err != nil || op.AsyncID != 41 || accepted.Error != nil {
		t.Fatalf("connect acceptance: %+v, %v", accepted, err)
	}
	if m := h.call(protocol.Stop, nil); m.Error != nil {
		t.Fatal(m.Error)
	}
	f.op.finish()
	m := h.read()
	var result protocol.OperationResult
	if err := protocol.Decode(m.Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result.RequestID != accepted.ID || result.OK || result.Error.Code != protocol.NativeRejected || !strings.Contains(string(result.Details), "9007199254740993") {
		t.Fatalf("async result: %+v", result)
	}
	h.call(protocol.Close, nil)
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
}

// TestSessionSyncControlLifecycleAdmission proves the lifecycle admission rules
// of the synchronous control commands: before initialize every native command
// is rejected with invalid_state while the session stays usable; a native
// rejection of BackToHome is native_rejected and never an empty success; and
// closing/terminated instance rejections keep their stable invalid_state code.
func TestSessionSyncControlLifecycleAdmission(t *testing.T) {
	t.Run("rejected before initialize", func(t *testing.T) {
		h := harness(t, fakeFactory(newFakeEngine()))
		for _, name := range []string{protocol.BackToHome, protocol.UUID, protocol.Status, protocol.Stop} {
			if m := h.call(name, nil); m.Error == nil || m.Error.Code != protocol.InvalidState {
				t.Fatalf("pre-init %s: %+v", name, m)
			}
		}
		if m := h.call(protocol.Ping, nil); m.Error != nil {
			t.Fatalf("session unusable after pre-init rejections: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("native rejection is not an empty success", func(t *testing.T) {
		f := newFakeEngine()
		f.backToHome = func() error { return maa.ErrNativeRejected }
		h := harness(t, fakeFactory(f))
		h.initialize()
		if m := h.call(protocol.BackToHome, nil); m.Error == nil || m.Error.Code != protocol.NativeRejected {
			t.Fatalf("back_to_home native rejection: %+v", m)
		}
		if m := h.call(protocol.Ping, nil); m.Error != nil {
			t.Fatalf("session damaged by native rejection: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("closing and terminated rejections keep invalid_state", func(t *testing.T) {
		f := newFakeEngine()
		f.backToHome = func() error { return maa.ErrClosing }
		f.uuid = func() (protocol.UUIDResult, error) { return protocol.UUIDResult{}, maa.ErrRuntimeTerminated }
		h := harness(t, fakeFactory(f))
		h.initialize()
		if m := h.call(protocol.BackToHome, nil); m.Error == nil || m.Error.Code != protocol.InvalidState {
			t.Fatalf("closing rejection: %+v", m)
		}
		if m := h.call(protocol.UUID, nil); m.Error == nil || m.Error.Code != protocol.InvalidState {
			t.Fatalf("terminated rejection: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
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
		if m := h.call(protocol.UUID, nil); m.Error != nil || string(m.Payload) != `{"uuid":"emulator-5554"}` {
			t.Fatalf("uuid: %+v", m)
		}
		if m := h.call(protocol.UUID, nil); m.Error != nil || string(m.Payload) != `{"uuid":""}` {
			t.Fatalf("empty uuid is a normal result: %+v", m)
		}
		if m := h.call(protocol.Status, nil); m.Error != nil || string(m.Payload) != `{"running":false,"connected":false,"tasks":[]}` {
			t.Fatalf("empty status: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("buffer exhaustion and bad native output are distinguishable", func(t *testing.T) {
		f := newFakeEngine()
		f.uuid = func() (protocol.UUIDResult, error) { return protocol.UUIDResult{}, maa.ErrBufferLimit }
		h := harness(t, fakeFactory(f))
		h.initialize()
		if m := h.call(protocol.UUID, nil); m.Error == nil || m.Error.Code != protocol.LimitExceeded {
			t.Fatalf("buffer exhaustion: %+v", m)
		}
		f.uuid = func() (protocol.UUIDResult, error) { return protocol.UUIDResult{}, maa.ErrProtocol }
		if m := h.call(protocol.UUID, nil); m.Error == nil || m.Error.Code != protocol.ProtocolError {
			t.Fatalf("bad native output: %+v", m)
		}
		if m := h.call(protocol.Ping, nil); m.Error != nil {
			t.Fatalf("session damaged by read failures: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
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
	if m.Error != nil {
		t.Fatal(m.Error)
	}
	var result protocol.StopResult
	if err := protocol.Decode(m.Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result.Stopped {
		t.Fatalf("accepted stop must not fabricate a stopped confirmation: %+v", result)
	}
	// A task terminal state is an independent core observation; acceptance
	// never synthesizes one.
	f.events <- protocol.Callback{MessageID: 4, CoreSequence: 1, JSON: json.RawMessage(`{"what":"TaskChainCompleted","details":{}}`)}
	event := h.read()
	if event.Name != protocol.CoreEvent || !strings.Contains(string(event.Payload), "TaskChainCompleted") {
		t.Fatalf("terminal state must arrive as a core event: %+v", event)
	}
	if m := h.call(protocol.Close, nil); m.Error != nil {
		t.Fatal(m.Error)
	}
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
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
	if m := h.response(pingID); m.Error != nil {
		t.Fatal(m.Error)
	}
	close(release)
	h.response(startID)
	h.call(protocol.Close, nil)
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
}

func TestSessionInitializationFailureIsTerminal(t *testing.T) {
	var calls atomic.Int32
	h := harness(t, func(protocol.InitializeRequest) (engine, protocol.RuntimeInfo, error) {
		calls.Add(1)
		return nil, protocol.RuntimeInfo{}, maa.ErrNativeRejected
	})
	if m := h.call(protocol.Initialize, protocol.InitializeRequest{}); m.Error == nil || m.Error.Code != protocol.NativeRejected {
		t.Fatalf("init error: %+v", m)
	}
	if err := <-h.done; !errors.Is(err, maa.ErrNativeRejected) {
		t.Fatalf("exit: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("factory called %d times", calls.Load())
	}
}

func TestSessionCloseErrorIsReportedBeforeExit(t *testing.T) {
	f := newFakeEngine()
	f.closeError = maa.ErrNativeRejected
	h := harness(t, fakeFactory(f))
	h.initialize()
	if m := h.call(protocol.Close, nil); m.Error == nil || m.Error.Code != protocol.NativeRejected {
		t.Fatalf("close diagnostic: %+v", m)
	}
	if err := <-h.done; !errors.Is(err, maa.ErrNativeRejected) {
		t.Fatalf("close exit: %v", err)
	}
	if f.closes.Load() != 1 {
		t.Fatalf("close count: %d", f.closes.Load())
	}
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
			if err := <-h.done; err == nil {
				t.Fatal("damaged session succeeded")
			}
			if f.closes.Load() != 1 {
				t.Fatalf("close count: %d", f.closes.Load())
			}
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
		if err := <-h.done; err == nil || !strings.Contains(err.Error(), "shutdown deadline") {
			t.Fatalf("hung shutdown: %v", err)
		}
		close(release)
		<-returned
	})
	t.Run("slow consumer", func(t *testing.T) {
		f := newFakeEngine()
		h := harness(t, fakeFactory(f))
		h.initialize()
		for i := range 128 {
			f.events <- protocol.Callback{MessageID: 1, CoreSequence: uint64(i + 1), JSON: json.RawMessage(`{}`)}
		}
		if err := <-h.done; !errors.Is(err, protocol.ErrQueueFull) {
			t.Fatalf("output overflow: %v", err)
		}
		if f.closes.Load() != 1 {
			t.Fatalf("close count: %d", f.closes.Load())
		}
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
		if m.Name != protocol.OperationEvent {
			t.Fatalf("expected an operation event, got %+v", m)
		}
		var r protocol.OperationResult
		if err := protocol.Decode(m.Payload, &r); err != nil {
			t.Fatal(err)
		}
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
		if m := h.call(protocol.Click, protocol.ClickRequest{X: -5, Y: 7}); m.Error != nil {
			t.Fatal(m.Error)
		} else {
			var accepted protocol.OperationAccepted
			if err := protocol.Decode(m.Payload, &accepted); err != nil || accepted.AsyncID != 1 {
				t.Fatalf("click acceptance: %+v, %v", accepted, err)
			}
		}
		if f.click.X != -5 || f.click.Y != 7 {
			t.Fatalf("click payload changed: %+v", f.click)
		}
		if m := h.call(protocol.Screencap, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		attach := protocol.AttachWindowRequest{
			Hwnd:            9007199254740993, // 2^53 + 1
			ScreencapMethod: 1 << 63,
			MouseMethod:     math.MaxUint64,
			KeyboardMethod:  1 << 40,
		}
		if m := h.call(protocol.AttachWindow, attach); m.Error != nil {
			t.Fatal(m.Error)
		}
		if f.attach != attach {
			t.Fatalf("attach_window payload changed: %+v, want %+v", f.attach, attach)
		}
		if m := h.call(protocol.Ping, nil); m.Error != nil {
			t.Fatalf("session unusable after four submissions: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("schema violations are invalid_input", func(t *testing.T) {
		f := newFakeEngine()
		h := harness(t, fakeFactory(f))
		h.initialize()
		if m := h.call(protocol.Click, nil); m.Error == nil || m.Error.Code != protocol.InvalidInput {
			t.Fatalf("click without payload: %+v", m)
		}
		if m := h.call(protocol.AttachWindow, map[string]any{"hwnd": 1.5, "screencap_method": 1, "mouse_method": 0, "keyboard_method": 0}); m.Error == nil || m.Error.Code != protocol.InvalidInput {
			t.Fatalf("fractional hwnd: %+v", m)
		}
		if m := h.call(protocol.AttachWindow, map[string]any{"hwnd": "9e18", "screencap_method": 1, "mouse_method": 0, "keyboard_method": 0}); m.Error == nil || m.Error.Code != protocol.InvalidInput {
			t.Fatalf("exponent hwnd: %+v", m)
		}
		if m := h.call(protocol.Ping, nil); m.Error != nil {
			t.Fatalf("session damaged by schema rejections: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("submission rejections keep stable codes", func(t *testing.T) {
		f := newFakeEngine()
		f.clickHook = func(protocol.ClickRequest) (operation, error) { return nil, maa.ErrNativeRejected }
		f.screencapHook = func() (operation, error) { return nil, maa.ErrOperationLimit }
		f.attachHook = func(protocol.AttachWindowRequest) (operation, error) { return nil, maa.ErrUnsupported }
		h := harness(t, fakeFactory(f))
		h.initialize()
		if m := h.call(protocol.Click, protocol.ClickRequest{X: 1, Y: 2}); m.Error == nil || m.Error.Code != protocol.NativeRejected {
			t.Fatalf("click rejection: %+v", m)
		}
		if m := h.call(protocol.Screencap, nil); m.Error == nil || m.Error.Code != protocol.LimitExceeded {
			t.Fatalf("screencap operation limit: %+v", m)
		}
		if m := h.call(protocol.AttachWindow, protocol.AttachWindowRequest{}); m.Error == nil || m.Error.Code != protocol.Unsupported {
			t.Fatalf("attach_window without the native symbol must stay unsupported, not fall back: %+v", m)
		}
		if len(f.created) != 0 {
			t.Fatalf("rejected submissions must not hold pending slots: %d", len(f.created))
		}
		if m := h.call(protocol.Ping, nil); m.Error != nil {
			t.Fatalf("session damaged by submission rejections: %+v", m)
		}
		if m := h.call(protocol.Close, nil); m.Error != nil {
			t.Fatal(m.Error)
		}
		if err := <-h.done; err != nil {
			t.Fatal(err)
		}
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
		if err := protocol.Decode(m.Payload, &accepted); err != nil || accepted.AsyncID != tc.want {
			t.Fatalf("acceptance %d: %+v, %v", tc.id, accepted, err)
		}
	}
	if m := h.call(protocol.Stop, nil); m.Error != nil {
		t.Fatalf("command loop blocked by in-flight operations: %+v", m)
	}
	// The second operation completes out of submission order while the first
	// is still in flight.
	second.finish()
	completion := operationEvents(t, h, 1)[0]
	if completion.AsyncID != 42 || completion.RequestID != secondID || completion.OK {
		t.Fatalf("second completion: %+v (request %d)", completion, secondID)
	}
	if completion.Error == nil || completion.Error.Code != protocol.NativeRejected || !strings.Contains(string(completion.Details), "9007199254740993") {
		t.Fatalf("native failure facts: %+v", completion)
	}
	first.finish()
	completion = operationEvents(t, h, 1)[0]
	if completion.AsyncID != 41 || completion.RequestID != firstID || !completion.OK || completion.Error != nil {
		t.Fatalf("first completion: %+v (request %d)", completion, firstID)
	}
	if !strings.Contains(string(completion.Details), "9007199254740993") {
		t.Fatalf("success details changed: %s", completion.Details)
	}
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
	m := h.response(thirdID)
	var accepted protocol.OperationAccepted
	if err := protocol.Decode(m.Payload, &accepted); err != nil || accepted.AsyncID != 43 {
		t.Fatalf("third acceptance: %+v, %v", accepted, err)
	}
	completion = operationEvents(t, h, 1)[0]
	if completion.AsyncID != 43 || completion.RequestID != thirdID || !completion.OK {
		t.Fatalf("early completion: %+v (request %d)", completion, thirdID)
	}
	if m := h.call(protocol.Close, nil); m.Error != nil {
		t.Fatal(m.Error)
	}
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
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
	if accepted.Error != nil {
		t.Fatal(accepted.Error)
	}
	if m := h.call(protocol.Close, nil); m.Error != nil {
		t.Fatal(m.Error)
	}
	settlement := operationEvents(t, h, 1)[0]
	if settlement.RequestID != accepted.ID || settlement.AsyncID != 41 || settlement.OK || settlement.What != "" {
		t.Fatalf("close settlement: %+v", settlement)
	}
	if settlement.Error == nil || settlement.Error.Code != protocol.InvalidState {
		t.Fatalf("lifecycle settlement must be invalid_state: %+v", settlement.Error)
	}
	if err := <-h.done; err != nil {
		t.Fatal(err)
	}
}
