package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
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
	start      func() error
	closeHook  func()
	closeError error
	op         *fakeOperation
}

func newFakeEngine() *fakeEngine {
	return &fakeEngine{events: make(chan protocol.Callback, 256), closed: make(chan struct{})}
}
func (f *fakeEngine) Connect(protocol.ConnectRequest) (operation, error) { return f.op, nil }
func (f *fakeEngine) AppendTask(req protocol.TaskRequest) (int32, error) {
	f.task = req
	return 23, nil
}
func (f *fakeEngine) SetTaskParams(protocol.TaskParamsRequest) error { return nil }
func (f *fakeEngine) Start() error {
	if f.start != nil {
		return f.start()
	}
	return nil
}
func (f *fakeEngine) Stop() (bool, error) { return true, nil }
func (f *fakeEngine) Status() (protocol.ExecutionStatus, error) {
	return protocol.ExecutionStatus{Tasks: []int32{23}}, nil
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
	if f.op != nil {
		f.op.finish()
	}
	return f.closeError
}

type fakeOperation struct {
	done chan struct{}
	once sync.Once
}

func (o *fakeOperation) ID() int32 { return 41 }
func (o *fakeOperation) finish()   { o.once.Do(func() { close(o.done) }) }
func (o *fakeOperation) Wait(ctx context.Context) (protocol.OperationResult, error) {
	select {
	case <-o.done:
		return protocol.OperationResult{AsyncID: 41, What: "Connect", OK: false, Details: json.RawMessage(`{"ret":false,"large":9007199254740993}`)}, maa.ErrNativeRejected
	case <-ctx.Done():
		return protocol.OperationResult{AsyncID: 41}, ctx.Err()
	}
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

func TestSessionAsyncCompletionDoesNotBlockStop(t *testing.T) {
	f := newFakeEngine()
	f.op = &fakeOperation{done: make(chan struct{})}
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
