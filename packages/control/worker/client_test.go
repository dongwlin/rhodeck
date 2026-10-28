package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

// fixtureLimits gives every Limits and BufferLimits field a distinct value so
// the snapshot test catches any loss or mixing on the way through the client.
func fixtureLimits() *protocol.Limits {
	return &protocol.Limits{
		InputStringBytes:  12345,
		InputJSONBytes:    23456,
		ConfigEntries:     34567,
		ConfigBytes:       45678,
		ImagePNG:          protocol.BufferLimits{Initial: 111, Max: 222, GrowthRetries: 3},
		ImageBGR:          protocol.BufferLimits{Initial: 444, Max: 555, GrowthRetries: 6},
		UUID:              protocol.BufferLimits{Initial: 77, Max: 88, GrowthRetries: 9},
		Tasks:             protocol.BufferLimits{Initial: 10, Max: 20, GrowthRetries: 30},
		CallbackBytes:     11111,
		InternalEvents:    22222,
		InternalBytes:     33333,
		PublicEvents:      44444,
		PublicBytes:       55555,
		PendingOperations: 66666,
		EarlyResults:      1,
		EarlyBytes:        77777,
	}
}

func fixtureRuntimeInfo() protocol.RuntimeInfo {
	return protocol.RuntimeInfo{
		SnapshotID:    "fixture",
		CoreVersion:   "fixture-core",
		LibrarySHA256: strings.Repeat("a", 64),
		Resources:     []protocol.Resource{{Root: "fixture-root"}},
		Capabilities:  protocol.Capabilities{ImageBGR: true, AttachWindow: false, MapLevelKey: true, ItemName: false},
		Limits:        fixtureLimits(),
		NullSize:      math.MaxUint64,
	}
}

func TestMain(m *testing.M) {
	if mode := os.Getenv("RHODECK_TEST_WORKER_MODE"); mode != "" {
		os.Exit(helper(mode))
	}
	os.Exit(m.Run())
}

// A real OS process exercises pipe ownership, deadlines, process waiting, and
// stderr independently of the native binding and worker implementation.
func helper(mode string) int {
	if mode == "nohello" {
		time.Sleep(time.Hour)
		return 9
	}
	if mode == "badhello" {
		fmt.Fprintln(os.Stdout, `{"version":2,"type":"hello","payload":{}}`)
		return 8
	}
	emit := func(kind, name string, id, seq uint64, body any, cause *protocol.Error) {
		m, err := protocol.NewMessage(kind, name, id, body)
		if err != nil {
			panic(err)
		}
		m.Sequence, m.Error = seq, cause
		data, err := protocol.Encode(m)
		if err != nil {
			panic(err)
		}
		if err := protocol.WriteFrame(os.Stdout, data); err != nil {
			os.Exit(10)
		}
	}
	g := protocol.Greeting{WorkerVersion: "fixture", SessionID: "fixture-session", OS: "fixture", Arch: "fixture", MaxFrameBytes: protocol.MaxFrameBytes, QueueMessages: protocol.QueueMessages, QueueBytes: protocol.QueueBytes}
	emit(protocol.Hello, "", 0, 0, g, nil)
	reader := protocol.NewReader(os.Stdin)
	var last uint64
	var held *protocol.Message
	var sequence uint64
	for {
		req, err := reader.Read()
		if err != nil {
			return 11
		}
		if req.ID <= last {
			return 12
		}
		last = req.ID
		respond := func(body any) { emit(protocol.Response, req.Name, req.ID, 0, body, nil) }
		if mode == "wrongid" {
			emit(protocol.Response, req.Name, req.ID+1, 0, struct{}{}, nil)
			continue
		}
		if mode == "wrongmethod" {
			emit(protocol.Response, "wrong", req.ID, 0, struct{}{}, nil)
			continue
		}
		if mode == "truncated" {
			io.WriteString(os.Stdout, `{"version":1`)
			return 6
		}
		if mode == "disconnect" {
			os.Stdout.Close()
			time.Sleep(time.Hour)
			return 6
		}
		if mode == "reverse" && req.Name == protocol.Ping {
			if held == nil {
				copy := req
				held = &copy
				continue
			}
			respond(struct{}{})
			emit(protocol.Response, held.Name, held.ID, 0, struct{}{}, nil)
			held = nil
			continue
		}
		switch req.Name {
		case protocol.Initialize:
			if mode == "thininfo" {
				respond(protocol.RuntimeInfo{SnapshotID: "fixture", CoreVersion: "fixture-core", LibrarySHA256: strings.Repeat("a", 64), Resources: []protocol.Resource{{Root: "fixture-root"}}})
				continue
			}
			if mode == "fatlimits" {
				info := fixtureRuntimeInfo()
				info.Limits = fixtureLimits()
				info.Limits.ConfigBytes = protocol.MaxConfigBytes + 1
				respond(info)
				continue
			}
			respond(fixtureRuntimeInfo())
		case protocol.Connect:
			respond(protocol.OperationAccepted{AsyncID: 41})
			if mode == "missingoperation" {
				continue
			}
			sequence++
			emit(protocol.Event, protocol.CoreEvent, 0, sequence, protocol.Callback{MessageID: 7654321, CoreSequence: 1, JSON: json.RawMessage(`{"unknown":900719925474099312345}`)}, nil)
			sequence++
			emit(protocol.Event, protocol.OperationEvent, 0, sequence, protocol.OperationResult{RequestID: req.ID, AsyncID: 41, What: "Connect", OK: false, Details: json.RawMessage(`{"ret":false}`), Error: &protocol.Error{Code: protocol.NativeRejected, Message: "fixture rejected connection"}}, nil)
		case protocol.AppendTask:
			var task protocol.TaskRequest
			if protocol.Decode(req.Payload, &task) != nil || task.Type != "FutureTask" || !strings.Contains(string(task.Params), "900719925474099312345") {
				return 13
			}
			respond(protocol.TaskAccepted{TaskID: 23})
		case protocol.SetTaskParams:
			var task protocol.TaskParamsRequest
			if protocol.Decode(req.Payload, &task) != nil || task.TaskID != 23 || string(task.Params) != `{"enable":false,"future":900719925474099312345}` {
				return 13
			}
			respond(struct{}{})
		case protocol.Start:
			switch mode {
			case "crash":
				fmt.Fprintln(os.Stderr, "fixture native crash")
				return 7
			case "hang":
				time.Sleep(time.Hour)
				return 7
			case "reject":
				emit(protocol.Response, req.Name, req.ID, 0, nil, &protocol.Error{Code: protocol.NativeRejected, Message: "fixture rejected start"})
				continue
			case "badsequence":
				emit(protocol.Event, protocol.CoreEvent, 0, 2, protocol.Callback{CoreSequence: 1, JSON: json.RawMessage(`{}`)}, nil)
				continue
			case "unknownoperation":
				emit(protocol.Event, protocol.OperationEvent, 0, 1, protocol.OperationResult{RequestID: 123, AsyncID: 41}, nil)
				continue
			case "overflow":
				for i := range protocol.QueueMessages + 2 {
					emit(protocol.Event, protocol.CoreEvent, 0, uint64(i+1), protocol.Callback{CoreSequence: uint64(i + 1), JSON: json.RawMessage(`{}`)}, nil)
				}
			case "byteoverflow":
				raw, _ := json.Marshal(map[string]string{"data": strings.Repeat("x", 5<<20)})
				for i := range 4 {
					emit(protocol.Event, protocol.CoreEvent, 0, uint64(i+1), protocol.Callback{CoreSequence: uint64(i + 1), JSON: raw}, nil)
				}
			}
			respond(struct{}{})
		case protocol.Status:
			if mode == "badschema" {
				respond(map[string]string{"running": "invalid"})
				continue
			}
			if mode == "empty" {
				respond(protocol.ExecutionStatus{Tasks: []int32{}})
				continue
			}
			respond(protocol.ExecutionStatus{Tasks: []int32{23}})
		case protocol.BackToHome:
			respond(struct{}{})
		case protocol.UUID:
			switch mode {
			case "empty":
				respond(protocol.UUIDResult{UUID: ""})
			case "uuidschema":
				respond(map[string]int{"uuid": 42})
			default:
				respond(protocol.UUIDResult{UUID: "emulator-5554"})
			}
			continue
		case protocol.Stop:
			respond(protocol.StopResult{Stopped: true})
		case protocol.Close:
			if mode == "badclose" {
				respond(map[string]bool{"invalid": true})
				return 0
			}
			if mode == "hangclose" {
				time.Sleep(time.Hour)
				return 5
			}
			if mode == "stderr" {
				fmt.Fprint(os.Stderr, strings.Repeat("x", 100000)+"fixture log tail")
			}
			respond(struct{}{})
			if mode == "ackhang" {
				time.Sleep(time.Hour)
				return 5
			}
			return 0
		default:
			respond(struct{}{})
		}
	}
}

func fixtureConfig(t *testing.T, mode string) Config {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return Config{Executable: exe, Env: []string{"RHODECK_TEST_WORKER_MODE=" + mode}, StartupTimeout: 3 * time.Second, RequestTimeout: 3 * time.Second, ShutdownTimeout: 3 * time.Second}
}

func launchFixture(t *testing.T, mode string) *Client {
	t.Helper()
	c, err := Launch(context.Background(), fixtureConfig(t, mode))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func requireFailure(t *testing.T, err error, unknown bool) *Failure {
	t.Helper()
	var failure *Failure
	if !errors.As(err, &failure) || failure.Unknown != unknown {
		t.Fatalf("failure = %v, want unknown=%t", err, unknown)
	}
	return failure
}

func requireReaped(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("worker was not reaped")
	}
	if c.cmd.ProcessState == nil {
		t.Fatal("missing process exit state")
	}
}

func TestClientFullConversation(t *testing.T) {
	c := launchFixture(t, "flow")
	if c.Greeting().WorkerVersion != "fixture" {
		t.Fatal(c.Greeting())
	}
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	task, err := c.AppendTask(ctx, protocol.TaskRequest{Type: "FutureTask", Params: json.RawMessage(`{"unknown":900719925474099312345}`)})
	if err != nil || task.TaskID != 23 {
		t.Fatalf("append: %+v, %v", task, err)
	}
	if err := c.SetTaskParams(ctx, protocol.TaskParamsRequest{TaskID: 23, Params: json.RawMessage(`{"enable":false,"future":900719925474099312345}`)}); err != nil {
		t.Fatal(err)
	}
	op, err := c.Connect(ctx, protocol.ConnectRequest{})
	if err != nil || op.AsyncID != 41 {
		t.Fatalf("connect: %+v, %v", op, err)
	}
	first := <-c.Events()
	if !strings.Contains(string(first.Payload), "900719925474099312345") {
		t.Fatalf("raw event changed: %s", first.Payload)
	}
	second := <-c.Events()
	var result protocol.OperationResult
	if err := protocol.Decode(second.Payload, &result); err != nil {
		t.Fatal(err)
	}
	if result.OK || result.Error.Code != protocol.NativeRejected || first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("completion: %+v", result)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if status, err := c.Status(ctx); err != nil || status.Running || len(status.Tasks) != 1 {
		t.Fatalf("status: %+v, %v", status, err)
	}
	if err := c.BackToHome(ctx); err != nil {
		t.Fatal(err)
	}
	if uuid, err := c.UUID(ctx); err != nil || uuid.UUID != "emulator-5554" {
		t.Fatalf("uuid: %+v, %v", uuid, err)
	}
	if stopped, err := c.Stop(ctx); err != nil || !stopped.Stopped {
		t.Fatalf("stop: %+v, %v", stopped, err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
	if _, open := <-c.Events(); open {
		t.Fatal("event stream remains open")
	}
	if err := c.Ping(ctx); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-close request: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("repeated close: %v", err)
	}
}

// TestClientInitializeSnapshotIsLosslessAndDetached proves the initialize
// snapshot crosses the client without loss — the MaxUint64 null size, every
// Limits/BufferLimits field, and the capability booleans — and that the
// caller's copy is independent: deep mutation neither damages the session nor
// reaches the worker.
func TestClientInitializeSnapshotIsLosslessAndDetached(t *testing.T) {
	c := launchFixture(t, "flow")
	ctx := context.Background()
	info, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	if err != nil {
		t.Fatal(err)
	}
	if info.NullSize != math.MaxUint64 {
		t.Fatalf("null_size = %d, want the locked sentinel", info.NullSize)
	}
	wantCaps := protocol.Capabilities{ImageBGR: true, AttachWindow: false, MapLevelKey: true, ItemName: false}
	if info.Capabilities != wantCaps {
		t.Fatalf("capabilities = %+v, want %+v", info.Capabilities, wantCaps)
	}
	if limits := fixtureLimits(); info.Limits == nil || *info.Limits != *limits {
		t.Fatalf("limits = %+v, want %+v", info.Limits, *limits)
	}
	info.Limits.InputJSONBytes = 7
	info.Limits.ImagePNG.Max = 7
	info.Limits.Tasks.GrowthRetries = 7
	info.Resources[0].Root = "mutated"
	info.CoreVersion = "mutated"
	info.Capabilities.ImageBGR = false
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("mutated snapshot damaged the session: %v", err)
	}
	task, err := c.AppendTask(ctx, protocol.TaskRequest{Type: "FutureTask", Params: json.RawMessage(`{"unknown":900719925474099312345}`)})
	if err != nil || task.TaskID != 23 {
		t.Fatalf("append after mutation: %+v, %v", task, err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
}

// TestClientRejectsIncompleteRuntimeSnapshot proves the client enforces the
// frozen initialize contract: a snapshot without the applied limit set and
// null size, or reporting limits above the wire budgets, is a protocol
// failure and terminates the session.
func TestClientRejectsIncompleteRuntimeSnapshot(t *testing.T) {
	for _, mode := range []string{"thininfo", "fatlimits"} {
		t.Run(mode, func(t *testing.T) {
			c := launchFixture(t, mode)
			_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "fixture"})
			requireFailure(t, err, true)
			requireReaped(t, c)
		})
	}
}

// TestClientEmptyObservationsAreNormalResults proves the read-side contract on
// the client: running=false, connected=false, an empty task list and the empty
// UUID arrive as normal results — no error, no synthetic failure — and
// BackToHome acceptance is delivered without inventing a completion fact.
func TestClientEmptyObservationsAreNormalResults(t *testing.T) {
	c := launchFixture(t, "empty")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	status, err := c.Status(ctx)
	if err != nil || status.Running || status.Connected || len(status.Tasks) != 0 {
		t.Fatalf("empty status: %+v, %v", status, err)
	}
	if uuid, err := c.UUID(ctx); err != nil || uuid.UUID != "" {
		t.Fatalf("empty uuid: %+v, %v", uuid, err)
	}
	if err := c.BackToHome(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
}

func TestClientConcurrentRequestsAndClose(t *testing.T) {
	for _, mode := range []string{"flow", "reverse"} {
		t.Run(mode, func(t *testing.T) {
			c := launchFixture(t, mode)
			var wg sync.WaitGroup
			for range 32 {
				wg.Go(func() {
					if err := c.Ping(context.Background()); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			for range 16 {
				wg.Go(func() {
					if err := c.Close(context.Background()); err != nil {
						t.Error(err)
					}
				})
			}
			wg.Wait()
			requireReaped(t, c)
		})
	}
}

func TestClientFailureIsolationAndNoReplay(t *testing.T) {
	for _, mode := range []string{"crash", "wrongid", "wrongmethod", "truncated", "disconnect", "badsequence", "unknownoperation", "overflow", "byteoverflow"} {
		t.Run(mode, func(t *testing.T) {
			c := launchFixture(t, mode)
			err := c.Start(context.Background())
			if err == nil {
				requireReaped(t, c)
				err = c.Err()
			}
			requireFailure(t, err, true)
			requireReaped(t, c)
			if err := c.Ping(context.Background()); err == nil {
				t.Fatal("faulted session accepted another request")
			}
		})
	}
}

func TestClientRequestAndShutdownDeadlines(t *testing.T) {
	for _, mode := range []string{"hang", "hangclose", "ackhang"} {
		t.Run(mode, func(t *testing.T) {
			cfg := fixtureConfig(t, mode)
			cfg.RequestTimeout, cfg.ShutdownTimeout = 150*time.Millisecond, 200*time.Millisecond
			c, err := Launch(context.Background(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { c.Close(context.Background()) })
			if mode == "hang" {
				err = c.Start(context.Background())
			} else {
				err = c.Close(context.Background())
			}
			requireFailure(t, err, true)
			requireReaped(t, c)
		})
	}
}

func TestClientHandshakeAndPrecancelledRequests(t *testing.T) {
	for _, mode := range []string{"nohello", "badhello", "flow"} {
		t.Run(mode, func(t *testing.T) {
			cfg := fixtureConfig(t, mode)
			cfg.StartupTimeout = 200 * time.Millisecond
			if mode == "flow" {
				cfg.ExpectedWorkerVersion = "different-build"
			}
			if c, err := Launch(context.Background(), cfg); err == nil {
				c.Close(context.Background())
				t.Fatal("incompatible worker accepted")
			}
		})
	}
	c := launchFixture(t, "flow")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("precancelled request: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("precancelled request damaged worker: %v", err)
	}
}

func TestClientRemoteRejectionAndBadResponseSchema(t *testing.T) {
	c := launchFixture(t, "reject")
	var rejected *protocol.Error
	if err := c.Start(context.Background()); !errors.As(err, &rejected) || rejected.Code != protocol.NativeRejected {
		t.Fatalf("native rejection: %v", err)
	}
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("native rejection damaged worker: %v", err)
	}
	c = launchFixture(t, "badschema")
	_, err := c.Status(context.Background())
	requireFailure(t, err, true)
	requireReaped(t, c)
	c = launchFixture(t, "uuidschema")
	if _, err := c.UUID(context.Background()); err == nil {
		t.Fatal("bad uuid schema accepted")
	} else {
		requireFailure(t, err, true)
	}
	requireReaped(t, c)
}

func TestClientRejectsIncompleteClose(t *testing.T) {
	for _, mode := range []string{"badclose", "missingoperation"} {
		t.Run(mode, func(t *testing.T) {
			c := launchFixture(t, mode)
			if mode == "missingoperation" {
				if _, err := c.Connect(context.Background(), protocol.ConnectRequest{}); err != nil {
					t.Fatal(err)
				}
			}
			requireFailure(t, c.Close(context.Background()), true)
			requireReaped(t, c)
		})
	}
}

func TestClientBoundedDiagnostics(t *testing.T) {
	c := launchFixture(t, "stderr")
	if err := c.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	logs := c.Diagnostics()
	if len(logs) != diagnosticBytes || !strings.HasSuffix(logs, "fixture log tail") {
		t.Fatalf("diagnostics: %d bytes", len(logs))
	}
}
