package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
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
	// fixtureImagePattern is the deterministic image the fixture subprocess
	// serves: byte[i] = i*131 + salt (mod 256). The tests replicate it
	// independently and compare byte for byte.
	fixtureImagePattern := func(size int, salt byte) []byte {
		out := make([]byte, size)
		for i := range out {
			out[i] = byte(uint64(i)*131 + uint64(salt))
		}
		return out
	}
	fixtureImageSHA256 := func(payload []byte) string {
		sum := sha256.Sum256(payload)
		return hex.EncodeToString(sum[:])
	}
	// imageTransfers tracks one served snapshot per transfer ID with the next
	// expected chunk offset, so the frozen per-transfer sequential pull is
	// enforced even when two transfers interleave on the wire.
	type fixtureImage struct {
		payload  []byte
		chunkLen uint32
		next     uint64
		served   int
	}
	images := map[uint64]*fixtureImage{}
	var imageCoreSequence uint64
	const (
		imagePNGTransfer = uint64(77)
		imageBGRTransfer = uint64(78)
	)
	g := protocol.Greeting{WorkerVersion: "fixture", SessionID: "fixture-session", OS: "fixture", Arch: "fixture", MaxFrameBytes: protocol.MaxFrameBytes, QueueMessages: protocol.QueueMessages, QueueBytes: protocol.QueueBytes}
	emit(protocol.Hello, "", 0, 0, g, nil)
	reader := protocol.NewReader(os.Stdin)
	var last uint64
	var held *protocol.Message
	var sequence uint64
	var nextAsync int32 = 41
	var firstConnectID uint64
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
		case protocol.Connect, protocol.Click, protocol.Screencap, protocol.AttachWindow:
			what := map[string]string{protocol.Connect: "Connect", protocol.Click: "Click", protocol.Screencap: "Screencap", protocol.AttachWindow: "AttachWindow"}[req.Name]
			if req.Name == protocol.Click {
				var click protocol.ClickRequest
				if protocol.Decode(req.Payload, &click) != nil || click.X != -5 || click.Y != 7 {
					return 13
				}
			}
			if req.Name == protocol.AttachWindow && mode == "winmask" {
				var w protocol.AttachWindowRequest
				if protocol.Decode(req.Payload, &w) != nil || w.Hwnd != 9007199254740993 || w.ScreencapMethod != math.MaxUint64 || w.MouseMethod != math.MaxUint64 || w.KeyboardMethod != 1<<40 {
					return 13
				}
			}
			if mode == "pendingops" || mode == "pendingcrash" {
				firstConnectID = req.ID
				respond(protocol.OperationAccepted{AsyncID: 41})
				continue
			}
			if mode == "wrongwhat" || mode == "wrongasync" || mode == "duplicateoperation" {
				respond(protocol.OperationAccepted{AsyncID: 41})
				id := int32(41)
				if mode == "wrongasync" {
					id = 99
				}
				if mode == "wrongwhat" {
					what = "Screencap"
				}
				sequence++
				emit(protocol.Event, protocol.OperationEvent, 0, sequence, protocol.OperationResult{RequestID: req.ID, AsyncID: id, What: what, OK: true, Details: json.RawMessage(`{"ret":true}`)}, nil)
				if mode == "duplicateoperation" {
					sequence++
					emit(protocol.Event, protocol.OperationEvent, 0, sequence, protocol.OperationResult{RequestID: req.ID, AsyncID: 41, What: what, OK: true, Details: json.RawMessage(`{"ret":true}`)}, nil)
				}
				continue
			}
			if mode == "reversedops" && req.Name == protocol.Connect {
				if firstConnectID == 0 {
					firstConnectID = req.ID
					respond(protocol.OperationAccepted{AsyncID: 41})
					continue
				}
				respond(protocol.OperationAccepted{AsyncID: 42})
				// Complete the second submission first; the client must keep
				// every result keyed to its own request and async ID.
				sequence++
				emit(protocol.Event, protocol.OperationEvent, 0, sequence, protocol.OperationResult{RequestID: req.ID, AsyncID: 42, What: "Connect", OK: true, Details: json.RawMessage(`{"ret":true}`)}, nil)
				sequence++
				emit(protocol.Event, protocol.OperationEvent, 0, sequence, protocol.OperationResult{RequestID: firstConnectID, AsyncID: 41, What: "Connect", OK: true, Details: json.RawMessage(`{"ret":true}`)}, nil)
				continue
			}
			if req.Name != protocol.Connect {
				nextAsync++
				respond(protocol.OperationAccepted{AsyncID: nextAsync})
				sequence++
				emit(protocol.Event, protocol.OperationEvent, 0, sequence, protocol.OperationResult{RequestID: req.ID, AsyncID: nextAsync, What: what, OK: true, Details: json.RawMessage(`{"ret":true}`)}, nil)
				continue
			}
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
			case "crash", "pendingcrash":
				fmt.Fprintln(os.Stderr, "fixture native crash")
				return 7
			case "hang":
				time.Sleep(time.Hour)
				return 7
			case "reject":
				emit(protocol.Response, req.Name, req.ID, 0, nil, &protocol.Error{Code: protocol.NativeRejected, Message: "fixture rejected start"})
				continue
			case "faultdiag", "dupfault", "badfault":
				respond(struct{}{})
				notice := protocol.FaultNotice{Error: protocol.Error{Code: protocol.EventOverflow, Message: "fixture instance fault"}}
				if mode == "badfault" {
					notice.Error.Message = "" // invalid: a fault without a reason
				}
				sequence++
				emit(protocol.Event, protocol.FaultEvent, 0, sequence, notice, nil)
				if mode == "dupfault" {
					sequence++
					emit(protocol.Event, protocol.FaultEvent, 0, sequence, notice, nil)
				}
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
		case protocol.Log:
			var input protocol.LogRequest
			if protocol.Decode(req.Payload, &input) != nil {
				return 13
			}
			// "flow" sends one nonempty and one empty log input; both must
			// arrive verbatim and be answered by the empty object.
			verbatim := input.Level == "warn" && input.Message == " hello 世界 "
			if !verbatim && (input.Level != "" || input.Message != "") {
				return 13
			}
			respond(struct{}{})
			continue
		case protocol.MapLevelKey:
			var query protocol.MapLevelKeyRequest
			if protocol.Decode(req.Payload, &query) != nil {
				return 13
			}
			if mode == "queryschema" {
				respond(map[string]int{"stage_id": 42})
				continue
			}
			// The empty key means "not found": the zero value is the answer.
			if query.Key == "" {
				respond(protocol.MapLevelKeyResult{})
				continue
			}
			respond(protocol.MapLevelKeyResult{StageID: "main-07", Code: "1-7", LevelID: "level_07", Name: "粉碎矿山"})
			continue
		case protocol.ItemName:
			var query protocol.ItemNameRequest
			if protocol.Decode(req.Payload, &query) != nil {
				return 13
			}
			switch {
			case query.ID == "":
				respond(protocol.ItemNameResult{Name: "Unknown"})
			case query.ID == "known":
				respond(protocol.ItemNameResult{Name: "item-name"})
			default:
				respond(protocol.ItemNameResult{})
			}
			continue
		case protocol.Stop:
			respond(protocol.StopResult{Stopped: true})
		case protocol.GetImagePNG, protocol.GetImageBGR:
			if mode == "unsupportedbgr" && req.Name == protocol.GetImageBGR {
				emit(protocol.Response, req.Name, req.ID, 0, nil, &protocol.Error{Code: protocol.Unsupported, Message: "fixture lacks the optional BGR symbol"})
				continue
			}
			transferID := imagePNGTransfer
			salt := byte(0x50)
			if req.Name == protocol.GetImageBGR {
				transferID = imageBGRTransfer
				salt = 0x47
			}
			payload := fixtureImagePattern(3<<20+4567, salt)
			if mode == "emptyimage" {
				payload = nil
			}
			declared := fixtureImageSHA256(payload)
			if mode == "badsha" {
				declared = fixtureImageSHA256(fixtureImagePattern(len(payload), salt+1))
			}
			chunkBytes := uint32(1 << 20)
			total := uint64(0)
			if mode != "emptyimage" {
				total = uint64(len(payload))
			}
			if mode == "fatimage" {
				total = protocol.ImageTransferMaxBytes + 1
			}
			respond(protocol.ImageTransfer{TransferID: transferID, TotalBytes: total, SHA256: declared, ChunkBytes: chunkBytes})
			if mode != "emptyimage" && mode != "fatimage" {
				images[transferID] = &fixtureImage{payload: payload, chunkLen: chunkBytes}
			}
			continue
		case protocol.ImageChunk:
			var chunk protocol.ImageChunkRequest
			if protocol.Decode(req.Payload, &chunk) != nil {
				return 13
			}
			image, ok := images[chunk.TransferID]
			if !ok || chunk.Offset != image.next {
				return 13 // not the frozen backpressured sequential pull
			}
			length := uint64(image.chunkLen)
			if remaining := uint64(len(image.payload)) - chunk.Offset; remaining < length {
				length = remaining
			}
			if chunk.Length != uint32(length) {
				return 13
			}
			if mode == "chunkerror" {
				emit(protocol.Response, req.Name, req.ID, 0, nil, &protocol.Error{Code: protocol.LimitExceeded, Message: "fixture exhausted the snapshot"})
				continue
			}
			if mode == "slowchunk" && image.served == 1 {
				// Announce mid-flight on stderr, then hold the second chunk
				// response until the supervisor's cancellation kills us.
				fmt.Fprintln(os.Stderr, "slowchunk: second chunk request received")
				time.Sleep(30 * time.Second)
			}
			data := image.payload[chunk.Offset : chunk.Offset+length]
			offset := chunk.Offset
			if mode == "wrongoffset" {
				offset++
			}
			if mode == "shortchunk" && chunk.Offset == 0 {
				data = data[:1]
			}
			image.next, image.served = chunk.Offset+length, image.served+1
			if mode == "images" && image.served == 2 {
				// A core event interleaves with chunk traffic: the frozen
				// contract keeps the two streams independent.
				sequence++
				imageCoreSequence++
				emit(protocol.Event, protocol.CoreEvent, 0, sequence, protocol.Callback{MessageID: 3, CoreSequence: imageCoreSequence, JSON: json.RawMessage(`{"between":"chunks"}`)}, nil)
			}
			respond(protocol.ImageChunkResponse{TransferID: chunk.TransferID, Offset: offset, Data: base64.StdEncoding.EncodeToString(data)})
			continue
		case protocol.Close:
			if mode == "pendingops" && firstConnectID != 0 {
				// Settle the still-pending operation as a lifecycle result
				// before acknowledging close, like the real worker does.
				sequence++
				emit(protocol.Event, protocol.OperationEvent, 0, sequence, protocol.OperationResult{RequestID: firstConnectID, AsyncID: 41, OK: false, Error: &protocol.Error{Code: protocol.InvalidState, Message: "fixture close settlement"}}, nil)
			}
			// faultdiag/stopdiag/ghostfault/baddiag/bigdiag/latefault send the
			// terminal diagnostics snapshot before the close response, like
			// the real worker's close ordering; latefault follows the close
			// response with an unreported fault, a contract breach.
			fixtureDiagnostics := map[string]func() protocol.InstanceDiagnostics{
				"faultdiag": func() protocol.InstanceDiagnostics {
					return protocol.InstanceDiagnostics{
						LostEvents: math.MaxUint64,
						FirstFault: &protocol.Error{Code: protocol.EventOverflow, Message: "fixture instance fault"},
						RawPrefix:  base64.StdEncoding.EncodeToString([]byte{0xDE, 0x00, 0xFF, 0xFE, 0x80}),
					}
				},
				"stopdiag": func() protocol.InstanceDiagnostics {
					return protocol.InstanceDiagnostics{StopDiagnostic: &protocol.Error{Code: protocol.NativeRejected, Message: "fixture stop returned 0"}}
				},
				"ghostfault": func() protocol.InstanceDiagnostics {
					return protocol.InstanceDiagnostics{FirstFault: &protocol.Error{Code: protocol.EventOverflow, Message: "fixture instance fault"}}
				},
				"baddiag": func() protocol.InstanceDiagnostics {
					return protocol.InstanceDiagnostics{RawPrefix: "!!!not-base64!!!"}
				},
				"bigdiag": func() protocol.InstanceDiagnostics {
					return protocol.InstanceDiagnostics{RawPrefix: base64.StdEncoding.EncodeToString(make([]byte, protocol.MaxRawPrefixBytes+1))}
				},
				"latefault": func() protocol.InstanceDiagnostics {
					return protocol.InstanceDiagnostics{LostEvents: 1}
				},
			}
			if build, ok := fixtureDiagnostics[mode]; ok {
				sequence++
				emit(protocol.Event, protocol.DiagnosticsEvent, 0, sequence, build(), nil)
			}
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
			if mode == "latefault" {
				sequence++
				emit(protocol.Event, protocol.FaultEvent, 0, sequence, protocol.FaultNotice{Error: protocol.Error{Code: protocol.EventOverflow, Message: "late fixture fault"}}, nil)
			}
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
	if err != nil || op.ID() != 41 {
		t.Fatalf("connect: %v, %v", op, err)
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
	// The same terminal state is reachable through the operation object without
	// depending on the event consumer.
	waited, waitErr := op.Wait(ctx)
	var rejected *protocol.Error
	if !errors.As(waitErr, &rejected) || rejected.Code != protocol.NativeRejected || waited.OK || waited.AsyncID != 41 {
		t.Fatalf("operation wait: %+v, %v", waited, waitErr)
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

// TestClientLogAndExperimentQueries proves the client surfaces for the
// process-level log and the two experiment queries: the log input crosses
// verbatim (empty strings included) and acceptance is delivered without
// inventing a completion fact; the four-field level result transfers without
// loss and the zero value answers an empty key; "Unknown", the empty name and
// the not-found zero value arrive as normal observations.
func TestClientLogAndExperimentQueries(t *testing.T) {
	c := launchFixture(t, "flow")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Log(ctx, protocol.LogRequest{Level: "warn", Message: " hello 世界 "}); err != nil {
		t.Fatal(err)
	}
	if err := c.Log(ctx, protocol.LogRequest{}); err != nil {
		t.Fatalf("empty log input: %v", err)
	}
	mk, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "粉碎矿山"})
	if err != nil || mk != (protocol.MapLevelKeyResult{StageID: "main-07", Code: "1-7", LevelID: "level_07", Name: "粉碎矿山"}) {
		t.Fatalf("map_level_key: %+v, %v", mk, err)
	}
	if empty, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{}); err != nil || empty != (protocol.MapLevelKeyResult{}) {
		t.Fatalf("not-found is a normal result: %+v, %v", empty, err)
	}
	if name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: ""}); err != nil || name.Name != "Unknown" {
		t.Fatalf("empty id: %+v, %v", name, err)
	}
	if name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "known"}); err != nil || name.Name != "item-name" {
		t.Fatalf("known id: %+v, %v", name, err)
	}
	if name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "missing"}); err != nil || name.Name != "" {
		t.Fatalf("empty name is a normal result: %+v, %v", name, err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
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
	c = launchFixture(t, "queryschema")
	if _, err := c.MapLevelKey(context.Background(), protocol.MapLevelKeyRequest{Key: "k"}); err == nil {
		t.Fatal("bad map_level_key schema accepted")
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

// TestClientAsyncOperationsEndToEnd proves the four async submissions share one
// waitable contract: each acceptance yields an operation with a stable nonzero
// ID, the native rejection is a stable code through Wait, successful
// completions arrive per command, and the session closes cleanly afterwards.
func TestClientAsyncOperationsEndToEnd(t *testing.T) {
	c := launchFixture(t, "flow")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	connect, err := c.Connect(ctx, protocol.ConnectRequest{})
	if err != nil || connect.ID() != 41 {
		t.Fatalf("connect: %v, %v", connect, err)
	}
	click, err := c.Click(ctx, protocol.ClickRequest{X: -5, Y: 7})
	if err != nil || click.ID() != 42 {
		t.Fatalf("click: %v, %v", click, err)
	}
	cap, err := c.Screencap(ctx)
	if err != nil || cap.ID() != 43 {
		t.Fatalf("screencap: %v, %v", cap, err)
	}
	window, err := c.AttachWindow(ctx, protocol.AttachWindowRequest{Hwnd: 9007199254740993, ScreencapMethod: 1 << 63, MouseMethod: math.MaxUint64, KeyboardMethod: 1 << 40})
	if err != nil || window.ID() != 44 {
		t.Fatalf("attach_window: %v, %v", window, err)
	}
	res, waitErr := connect.Wait(ctx)
	var rejected *protocol.Error
	if !errors.As(waitErr, &rejected) || rejected.Code != protocol.NativeRejected || res.OK || res.AsyncID != 41 {
		t.Fatalf("connect terminal state: %+v, %v", res, waitErr)
	}
	for _, op := range []*Operation{click, cap, window} {
		res, waitErr := op.Wait(ctx)
		if waitErr != nil || !res.OK || res.What == "" || res.AsyncID != op.ID() {
			t.Fatalf("%d terminal state: %+v, %v", op.ID(), res, waitErr)
		}
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
	// IDs stay readable after settlement and after the session is gone.
	if connect.ID() != 41 || window.ID() != 44 {
		t.Fatalf("IDs changed after settlement: %d %d", connect.ID(), window.ID())
	}
}

// TestClientOperationWaitIsRepeatableAndConcurrent proves Wait is repeatable
// and concurrency-safe: repeated and concurrent waits observe the same cached
// terminal state, every call receives an independent copy whose mutation
// cannot rewrite the cache or other callers' results.
func TestClientOperationWaitIsRepeatableAndConcurrent(t *testing.T) {
	c := launchFixture(t, "flow")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	op, err := c.Connect(ctx, protocol.ConnectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	first, waitErr := op.Wait(ctx)
	var rejected *protocol.Error
	if !errors.As(waitErr, &rejected) || rejected.Code != protocol.NativeRejected {
		t.Fatalf("first wait: %+v, %v", first, waitErr)
	}
	results := make([]protocol.OperationResult, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			r, err := op.Wait(ctx)
			var rejected *protocol.Error
			if !errors.As(err, &rejected) || rejected.Code != protocol.NativeRejected {
				t.Errorf("concurrent wait: %+v, %v", r, err)
			}
			results[i] = r
		})
	}
	wg.Wait()
	for i, r := range results {
		if r.AsyncID != first.AsyncID || r.What != first.What || r.OK != first.OK || string(r.Details) != string(first.Details) {
			t.Fatalf("wait %d diverged: %+v vs %+v", i, r, first)
		}
	}
	// Mutating one returned copy leaves the cache untouched.
	first.Details[0] = 'X'
	again, waitErr := op.Wait(ctx)
	var stillRejected *protocol.Error
	if !errors.As(waitErr, &stillRejected) || stillRejected.Code != protocol.NativeRejected || string(again.Details) != `{"ret":false}` {
		t.Fatalf("cached result was rewritten through a returned copy: %s, %v", again.Details, waitErr)
	}
	if op.ID() != 41 {
		t.Fatalf("ID changed after settlement: %d", op.ID())
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
}

// TestClientWaitCancellationIsLocal proves a cancelled or timed-out wait ends
// only that wait: the worker stays alive, the submission keeps its native slot,
// a later wait still works, and the eventual close settlement arrives as a
// distinguishable lifecycle error rather than unknown.
func TestClientWaitCancellationIsLocal(t *testing.T) {
	c := launchFixture(t, "pendingops")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	op, err := c.Connect(ctx, protocol.ConnectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		if _, err := op.Wait(waitCtx); !errors.Is(err, context.DeadlineExceeded) {
			cancel()
			t.Fatalf("timed-out wait: %v", err)
		}
		cancel()
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("cancelled wait damaged the session: %v", err)
	}
	type settled struct {
		result protocol.OperationResult
		err    error
	}
	done := make(chan settled, 1)
	go func() {
		r, err := op.Wait(ctx)
		done <- settled{r, err}
	}()
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	got := <-done
	var lifecycle *protocol.Error
	if !errors.As(got.err, &lifecycle) || lifecycle.Code != protocol.InvalidState || got.result.OK || got.result.What != "" {
		t.Fatalf("close settlement: %+v, %v", got.result, got.err)
	}
	requireReaped(t, c)
}

// TestClientRejectsWrongOperationAttributes proves the client validates every
// operation result against its acceptance: a wrong what, an unknown async ID,
// and a duplicate result each abort the session as a protocol failure instead
// of being matched loosely.
func TestClientRejectsWrongOperationAttributes(t *testing.T) {
	for _, mode := range []string{"wrongwhat", "wrongasync", "duplicateoperation"} {
		t.Run(mode, func(t *testing.T) {
			c := launchFixture(t, mode)
			ctx := context.Background()
			if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
				t.Fatal(err)
			}
			_, _ = c.Connect(ctx, protocol.ConnectRequest{})
			select {
			case <-c.Done():
			case <-time.After(5 * time.Second):
				t.Fatal("session survived a wrong operation result")
			}
			requireFailure(t, c.Err(), true)
			requireReaped(t, c)
		})
	}
}

// TestClientAsyncCompletionOutOfOrder proves completions may arrive in any
// order: each operation object is matched to its own request and async ID even
// when the later submission settles first.
func TestClientAsyncCompletionOutOfOrder(t *testing.T) {
	c := launchFixture(t, "reversedops")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	a, err := c.Connect(ctx, protocol.ConnectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	b, err := c.Connect(ctx, protocol.ConnectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if a.ID() != 41 || b.ID() != 42 {
		t.Fatalf("async ids: %d %d", a.ID(), b.ID())
	}
	rb, err := b.Wait(ctx)
	if err != nil || !rb.OK || rb.AsyncID != 42 {
		t.Fatalf("second completion: %+v, %v", rb, err)
	}
	ra, err := a.Wait(ctx)
	if err != nil || !ra.OK || ra.AsyncID != 41 {
		t.Fatalf("first completion: %+v, %v", ra, err)
	}
	if ra.RequestID == rb.RequestID {
		t.Fatal("results crossed their request IDs")
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
}

// TestClientAttachWindowWireIsLossless proves the window handle and method
// masks cross the client without numeric loss: the fixture exits with an error
// if the decoded values differ from the >2^53 handle and full-width masks the
// test submitted, so reaching the happy path is the precision proof.
func TestClientAttachWindowWireIsLossless(t *testing.T) {
	c := launchFixture(t, "winmask")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	op, err := c.AttachWindow(ctx, protocol.AttachWindowRequest{Hwnd: 9007199254740993, ScreencapMethod: math.MaxUint64, MouseMethod: math.MaxUint64, KeyboardMethod: 1 << 40})
	if err != nil {
		t.Fatal(err)
	}
	res, waitErr := op.Wait(ctx)
	if waitErr != nil || !res.OK || res.What != "AttachWindow" {
		t.Fatalf("attach_window terminal state: %+v, %v", res, waitErr)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
}

// TestClientUnconfirmedOperationAfterFaultIsUnknown proves a session fault
// leaves an operation without any result genuinely unknown: the waiter returns
// the sticky *Failure with Unknown=true — never a fabricated terminal state —
// and no wait hangs against the dead session.
func TestClientUnconfirmedOperationAfterFaultIsUnknown(t *testing.T) {
	c := launchFixture(t, "pendingcrash")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	op, err := c.Connect(ctx, protocol.ConnectRequest{})
	if err != nil {
		t.Fatal(err)
	}
	type settled struct {
		result protocol.OperationResult
		err    error
	}
	done := make(chan settled, 1)
	go func() {
		r, err := op.Wait(ctx)
		done <- settled{r, err}
	}()
	// The crash arrives through the same session; the waiting goroutine must
	// return without any further input.
	if err := c.Start(ctx); err == nil {
		t.Fatal("crashing start succeeded")
	}
	select {
	case got := <-done:
		requireFailure(t, got.err, true)
		if got.result.What != "" || got.result.OK || got.result.Error != nil {
			t.Fatalf("a fault must not fabricate a terminal state: %+v", got.result)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wait hung against a faulted session")
	}
	requireReaped(t, c)
	if _, err := op.Wait(ctx); !errors.Is(err, c.Err()) {
		t.Fatalf("later wait diverged from the sticky failure: %v", err)
	}
}

// fixtureImagePattern is the deterministic image the fixture subprocess serves
// (byte[i] = i*131 + salt): the tests replicate it independently and compare
// byte for byte, so a divergence anywhere in the chunk pipeline is visible.
func fixtureImagePattern(size int, salt byte) []byte {
	out := make([]byte, size)
	for i := range out {
		out[i] = byte(uint64(i)*131 + uint64(salt))
	}
	return out
}

// TestClientImageTransfersAssembleVerifiedSnapshots proves the client side of
// the frozen image contract: transfers assemble byte for byte from a
// backpressured sequential chunk pull, chunk traffic never enters or disturbs
// the event stream, and concurrent transfers stay keyed to their own transfer
// IDs and offsets.
func TestClientImageTransfersAssembleVerifiedSnapshots(t *testing.T) {
	c := launchFixture(t, "images")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	png, err := c.GetImagePNG(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantPNG := fixtureImagePattern(3<<20+4567, 0x50)
	if !bytes.Equal(png, wantPNG) {
		t.Fatalf("png snapshot diverged: %d bytes", len(png))
	}
	bgr, err := c.GetImageBGR(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bgr, fixtureImagePattern(3<<20+4567, 0x47)) {
		t.Fatalf("bgr snapshot diverged: %d bytes", len(bgr))
	}
	// One core event interleaved with each transfer's chunk traffic; both
	// kept their own contiguous sequence domain.
	var sequences []uint64
	for range 2 {
		select {
		case event := <-c.Events():
			if event.Name != protocol.CoreEvent {
				t.Fatalf("unexpected event: %+v", event)
			}
			sequences = append(sequences, event.Sequence)
		case <-time.After(5 * time.Second):
			t.Fatal("interleaved core event missing")
		}
	}
	if len(sequences) != 2 || sequences[0] != 1 || sequences[1] != 2 {
		t.Fatalf("event sequences: %v", sequences)
	}
	select {
	case event := <-c.Events():
		t.Fatalf("chunk traffic produced extra events: %+v", event)
	default:
	}
	// The returned snapshots are independently owned: mutating one does not
	// damage the session or any other returned buffer.
	png[0] ^= 0xFF
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("session damaged by caller mutation: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)

	// Concurrent transfers interleave on the wire while each stays a
	// sequential, backpressured pull of its own snapshot.
	c = launchFixture(t, "images")
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	results := make([][]byte, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); results[0], errs[0] = c.GetImagePNG(ctx) }()
	go func() { defer wg.Done(); results[1], errs[1] = c.GetImageBGR(ctx) }()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent transfer %d: %v", i, err)
		}
	}
	if !bytes.Equal(results[0], wantPNG) || !bytes.Equal(results[1], fixtureImagePattern(3<<20+4567, 0x47)) {
		t.Fatal("concurrent snapshots diverged")
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestClientRejectsBrokenImageTransfers proves that broken transfers never
// publish a partial success: a declared digest that the assembled bytes do
// not match, a short chunk, a wrong offset, and an oversized total length all
// terminate the session with a protocol failure, while worker-side
// rejections (a rejected chunk, the missing optional BGR symbol) surface as
// stable remote errors and leave the session usable.
func TestClientRejectsBrokenImageTransfers(t *testing.T) {
	t.Run("mismatched digest aborts the session", func(t *testing.T) {
		c := launchFixture(t, "badsha")
		_, err := c.GetImagePNG(context.Background())
		requireFailure(t, err, true)
		requireReaped(t, c)
	})
	t.Run("short chunk aborts the session", func(t *testing.T) {
		c := launchFixture(t, "shortchunk")
		_, err := c.GetImagePNG(context.Background())
		requireFailure(t, err, true)
		requireReaped(t, c)
	})
	t.Run("wrong offset aborts the session", func(t *testing.T) {
		c := launchFixture(t, "wrongoffset")
		_, err := c.GetImagePNG(context.Background())
		requireFailure(t, err, true)
		requireReaped(t, c)
	})
	t.Run("total beyond the frozen budget aborts the session", func(t *testing.T) {
		c := launchFixture(t, "fatimage")
		_, err := c.GetImagePNG(context.Background())
		requireFailure(t, err, true)
		requireReaped(t, c)
	})
	t.Run("mid-transfer cancellation aborts without partial success", func(t *testing.T) {
		c := launchFixture(t, "slowchunk")
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		errCh := make(chan error, 1)
		go func() { _, err := c.GetImagePNG(ctx); errCh <- err }()
		// Wait until the second chunk request is provably in flight (the
		// fixture announces it on stderr), so the cancellation is a
		// post-enqueue one under the frozen failure discipline.
		deadline := time.Now().Add(10 * time.Second)
		for !strings.Contains(c.Diagnostics(), "slowchunk: second chunk request received") {
			if time.Now().After(deadline) {
				t.Fatal("second chunk request never arrived")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		err := <-errCh
		if err == nil {
			t.Fatal("cancelled transfer succeeded")
		}
		requireFailure(t, err, true)
		requireReaped(t, c)
	})
	t.Run("worker rejections stay remote errors", func(t *testing.T) {
		c := launchFixture(t, "chunkerror")
		var rejected *protocol.Error
		if _, err := c.GetImagePNG(context.Background()); !errors.As(err, &rejected) || rejected.Code != protocol.LimitExceeded {
			t.Fatalf("chunk rejection: %v", err)
		}
		if err := c.Ping(context.Background()); err != nil {
			t.Fatalf("session unusable after the rejection: %v", err)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("missing optional BGR symbol is unsupported", func(t *testing.T) {
		c := launchFixture(t, "unsupportedbgr")
		var rejected *protocol.Error
		if _, err := c.GetImageBGR(context.Background()); !errors.As(err, &rejected) || rejected.Code != protocol.Unsupported {
			t.Fatalf("bgr rejection: %v", err)
		}
		png, err := c.GetImagePNG(context.Background())
		if err != nil || !bytes.Equal(png, fixtureImagePattern(3<<20+4567, 0x50)) {
			t.Fatalf("png after bgr rejection: %d bytes, %v", len(png), err)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestClientEmptyImageIsNormalResult proves a zero-length cached image is a
// complete, verified transfer that needs no chunks and returns an empty,
// non-nil snapshot.
func TestClientEmptyImageIsNormalResult(t *testing.T) {
	c := launchFixture(t, "emptyimage")
	ctx := context.Background()
	for _, fetch := range []func(context.Context) ([]byte, error){c.GetImagePNG, c.GetImageBGR} {
		image, err := fetch(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if image == nil || len(image) != 0 {
			t.Fatalf("empty image must be an empty non-nil snapshot: %#v", image)
		}
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
}

// waitFault waits for the client's native fault observation.
func waitFault(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.Fault():
	case <-time.After(5 * time.Second):
		t.Fatal("the native fault observation never fired")
	}
}

// TestClientFaultAndDiagnosticsAreNativeObservations proves the client-side
// equivalents of the binding's five public observations: the fault notice
// arrives as a channel observation with its stable code, the terminal
// diagnostics snapshot follows it before close completes, the raw prefix
// decodes back to the original bytes (which are neither valid UTF-8 nor valid
// JSON), the saturating lost count survives exactly, and repeated accesses
// return independent copies of the stable final snapshot.
func TestClientFaultAndDiagnosticsAreNativeObservations(t *testing.T) {
	c := launchFixture(t, "faultdiag")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	// The fault is armed by Start; the event channel is left undrained on
	// purpose — the fault observation must not depend on the consumer.
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFault(t, c)
	if err := c.InstanceErr(); err == nil || err.Code != protocol.EventOverflow {
		t.Fatalf("instance error = %v, want event_overflow", err)
	}

	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
	select {
	case <-c.InstanceDone():
	case <-time.After(5 * time.Second):
		t.Fatal("the native terminal observation never fired")
	}
	snap, ok := c.InstanceDiagnostics()
	if !ok {
		t.Fatal("the terminal snapshot was reported missing after delivery")
	}
	if snap.LostEvents != math.MaxUint64 {
		t.Fatalf("lost_events = %d, want the saturated count", snap.LostEvents)
	}
	if snap.FirstFault == nil || snap.FirstFault.Code != protocol.EventOverflow {
		t.Fatalf("first fault = %+v, want event_overflow", snap.FirstFault)
	}
	if snap.StopDiagnostic != nil {
		t.Fatalf("unexpected stop diagnostic: %+v", snap.StopDiagnostic)
	}
	want := []byte{0xDE, 0x00, 0xFF, 0xFE, 0x80}
	if string(snap.RawPrefix) != string(want) {
		t.Fatalf("raw prefix changed: %q, want %q", snap.RawPrefix, want)
	}
	// The final snapshot is stable and every access is an independent copy.
	snap.RawPrefix[0] = 'X'
	if snap.FirstFault.Detail != nil {
		snap.FirstFault.Detail.Kind = "mutated"
	}
	again, ok := c.InstanceDiagnostics()
	if !ok || string(again.RawPrefix) != string(want) || again.FirstFault == nil || again.FirstFault.Code != protocol.EventOverflow {
		t.Fatalf("snapshot was rewritten through a returned copy: %+v, %t", again, ok)
	}
	// The process done channel is a separate observation from the native one;
	// after a normal close both are closed, but neither substitutes the other.
	select {
	case <-c.Done():
	default:
		t.Fatal("process done must be closed after a reaped close")
	}
}

// TestClientConfirmedOperationSurvivesLaterFault proves a confirmed result is
// never rewritten: an operation that settled successfully before the fault
// keeps its cached success after the fault event and the terminal snapshot.
func TestClientConfirmedOperationSurvivesLaterFault(t *testing.T) {
	c := launchFixture(t, "faultdiag")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	op, err := c.Click(ctx, protocol.ClickRequest{X: -5, Y: 7})
	if err != nil {
		t.Fatal(err)
	}
	first, waitErr := op.Wait(ctx)
	if waitErr != nil || !first.OK {
		t.Fatalf("click terminal state: %+v, %v", first, waitErr)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	waitFault(t, c)
	again, waitErr := op.Wait(ctx)
	if waitErr != nil || !again.OK || string(again.Details) != string(first.Details) {
		t.Fatalf("confirmed result was rewritten by the fault: %+v, %v", again, waitErr)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
	final, waitErr := op.Wait(ctx)
	if waitErr != nil || !final.OK {
		t.Fatalf("confirmed result changed after close: %+v, %v", final, waitErr)
	}
}

// TestClientFaultObservationDoesNotDependOnEventConsumption proves the
// supervisor-level fault report never waits for the business consumer: with
// nobody draining Events, the fault and the terminal snapshot are still
// observed within a bounded time and the close completes.
func TestClientFaultObservationDoesNotDependOnEventConsumption(t *testing.T) {
	c := launchFixture(t, "faultdiag")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// No goroutine reads c.Events() in this test.
	waitFault(t, c)
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
	select {
	case <-c.InstanceDone():
	case <-time.After(5 * time.Second):
		t.Fatal("the terminal snapshot never fired without a consumer")
	}
	if _, ok := c.InstanceDiagnostics(); !ok {
		t.Fatal("the terminal snapshot was reported missing without a consumer")
	}
}

// TestClientStopDiagnosticWithoutFault proves a stop-only diagnostic is a
// terminal fact of its own: it is delivered in the snapshot, the fault
// observation never fires, and the instance error stays nil.
func TestClientStopDiagnosticWithoutFault(t *testing.T) {
	c := launchFixture(t, "stopdiag")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireReaped(t, c)
	select {
	case <-c.Fault():
		t.Fatal("a stop-only diagnostic must not close the fault observation")
	default:
	}
	if err := c.InstanceErr(); err != nil {
		t.Fatalf("instance error = %v, want nil", err)
	}
	snap, ok := c.InstanceDiagnostics()
	if !ok || snap.StopDiagnostic == nil || snap.StopDiagnostic.Code != protocol.NativeRejected || snap.FirstFault != nil || snap.LostEvents != 0 {
		t.Fatalf("snapshot = %+v, %t", snap, ok)
	}
}

// TestClientKilledProcessReportsDiagnosticsUnavailable proves the process and
// native observations stay separate: a crash closes the process Done but
// never the native Done or the fault observation, and the missing snapshot is
// reported as unavailable instead of being fabricated.
func TestClientKilledProcessReportsDiagnosticsUnavailable(t *testing.T) {
	c := launchFixture(t, "crash")
	ctx := context.Background()
	if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
		t.Fatal(err)
	}
	err := c.Start(ctx)
	// The exit races the EOF on the pipes, so only the unknown-discipline is
	// pinned here: the process is gone and no native fact survived it.
	requireFailure(t, err, true)
	requireReaped(t, c)
	select {
	case <-c.InstanceDone():
		t.Fatal("a killed process must not fabricate the native terminal state")
	default:
	}
	select {
	case <-c.Fault():
		t.Fatal("a killed process must not fabricate a native fault")
	default:
	}
	if err := c.InstanceErr(); err != nil {
		t.Fatalf("instance error = %v, want nil", err)
	}
	if _, ok := c.InstanceDiagnostics(); ok {
		t.Fatal("a killed process must not fabricate a diagnostics snapshot")
	}
}

// TestClientRejectsBrokenFaultAndDiagnosticsReports proves the client does
// not trust fault traffic: a duplicated fault, a fault after the terminal
// snapshot, a snapshot naming an unreported fault, an unbounded raw prefix
// and a non-base64 prefix all abort the session as protocol errors.
func TestClientRejectsBrokenFaultAndDiagnosticsReports(t *testing.T) {
	for _, mode := range []string{"dupfault", "badfault", "ghostfault", "latefault", "baddiag", "bigdiag"} {
		t.Run(mode, func(t *testing.T) {
			c := launchFixture(t, mode)
			ctx := context.Background()
			if _, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"}); err != nil {
				t.Fatal(err)
			}
			if err := c.Start(ctx); err != nil {
				if mode == "badfault" {
					requireFailure(t, err, true)
					requireReaped(t, c)
					return
				}
				t.Fatal(err)
			}
			err := c.Close(ctx)
			failure := requireFailure(t, err, true)
			if failure.Code != "protocol_error" {
				t.Fatalf("failure code = %s, want protocol_error", failure.Code)
			}
			requireReaped(t, c)
		})
	}
}
