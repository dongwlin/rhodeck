package worker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

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
	// enforced even when transfers interleave on the wire. Every get_image
	// request owns a fresh ID, like the real worker's per-snapshot registry,
	// so concurrent transfers of the same kind keep independent state.
	type fixtureImage struct {
		payload  []byte
		chunkLen uint32
		next     uint64
		served   int
	}
	images := map[uint64]*fixtureImage{}
	var imageCoreSequence uint64
	var nextFixtureTransfer uint64
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
			salt := byte(0x50)
			if req.Name == protocol.GetImageBGR {
				salt = 0x47
			}
			nextFixtureTransfer++
			transferID := nextFixtureTransfer
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
	require.NoError(t, err)
	return Config{Executable: exe, Env: []string{"RHODECK_TEST_WORKER_MODE=" + mode}, StartupTimeout: 3 * time.Second, RequestTimeout: 3 * time.Second, ShutdownTimeout: 3 * time.Second}
}

func launchFixture(t *testing.T, mode string) *Client {
	t.Helper()
	c, err := Launch(context.Background(), fixtureConfig(t, mode))
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func requireFailure(t *testing.T, err error, unknown bool) *Failure {
	t.Helper()
	var failure *Failure
	require.ErrorAs(t, err, &failure, "failure = %v, want unknown=%t", err, unknown)
	require.Equal(t, unknown, failure.Unknown, "failure = %v, want unknown=%t", err, unknown)
	return failure
}

func requireReaped(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "worker was not reaped")
	}
	require.NotNil(t, c.cmd.ProcessState, "missing process exit state")
}

func TestClientFullConversation(t *testing.T) {
	c := launchFixture(t, "flow")
	require.Equal(t, "fixture", c.Greeting().WorkerVersion, "greeting = %+v", c.Greeting())
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	task, err := c.AppendTask(ctx, protocol.TaskRequest{Type: "FutureTask", Params: json.RawMessage(`{"unknown":900719925474099312345}`)})
	require.NoError(t, err, "append: %+v, %v", task, err)
	require.Equal(t, int32(23), task.TaskID, "append: %+v, %v", task, err)
	require.NoError(t, c.SetTaskParams(ctx, protocol.TaskParamsRequest{TaskID: 23, Params: json.RawMessage(`{"enable":false,"future":900719925474099312345}`)}))
	op, err := c.Connect(ctx, protocol.ConnectRequest{})
	require.NoError(t, err, "connect: %v, %v", op, err)
	require.Equal(t, int32(41), op.ID(), "connect: %v, %v", op, err)
	first := <-c.Events()
	require.Contains(t, string(first.Payload), "900719925474099312345", "raw event changed: %s", first.Payload)
	second := <-c.Events()
	var result protocol.OperationResult
	require.NoError(t, protocol.Decode(second.Payload, &result))
	require.False(t, result.OK, "completion: %+v", result)
	require.Equal(t, protocol.NativeRejected, result.Error.Code, "completion: %+v", result)
	require.Equal(t, uint64(1), first.Sequence, "completion: %+v", result)
	require.Equal(t, uint64(2), second.Sequence, "completion: %+v", result)
	// The same terminal state is reachable through the operation object without
	// depending on the event consumer.
	waited, waitErr := op.Wait(ctx)
	var rejected *protocol.Error
	require.ErrorAs(t, waitErr, &rejected, "operation wait: %+v, %v", waited, waitErr)
	require.Equal(t, protocol.NativeRejected, rejected.Code, "operation wait: %+v, %v", waited, waitErr)
	require.False(t, waited.OK, "operation wait: %+v, %v", waited, waitErr)
	require.Equal(t, int32(41), waited.AsyncID, "operation wait: %+v, %v", waited, waitErr)
	require.NoError(t, c.Start(ctx))
	status, err := c.Status(ctx)
	require.NoError(t, err, "status: %+v, %v", status, err)
	require.False(t, status.Running, "status: %+v, %v", status, err)
	require.Len(t, status.Tasks, 1, "status: %+v, %v", status, err)
	require.NoError(t, c.BackToHome(ctx))
	uuid, err := c.UUID(ctx)
	require.NoError(t, err, "uuid: %+v, %v", uuid, err)
	require.Equal(t, "emulator-5554", uuid.UUID, "uuid: %+v, %v", uuid, err)
	stopped, err := c.Stop(ctx)
	require.NoError(t, err, "stop: %+v, %v", stopped, err)
	require.True(t, stopped.Stopped, "stop: %+v, %v", stopped, err)
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
	_, open := <-c.Events()
	require.False(t, open, "event stream remains open")
	err = c.Ping(ctx)
	require.ErrorIs(t, err, ErrClosed, "post-close request: %v", err)
	err = c.Close(ctx)
	require.NoError(t, err, "repeated close: %v", err)
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
	require.NoError(t, err)
	require.Equal(t, uint64(math.MaxUint64), info.NullSize, "null_size = %d, want the locked sentinel", info.NullSize)
	wantCaps := protocol.Capabilities{ImageBGR: true, AttachWindow: false, MapLevelKey: true, ItemName: false}
	require.Equal(t, wantCaps, info.Capabilities, "capabilities = %+v, want %+v", info.Capabilities, wantCaps)
	limits := fixtureLimits()
	require.NotNil(t, info.Limits, "limits = %+v, want %+v", info.Limits, *limits)
	require.Equal(t, *limits, *info.Limits, "limits = %+v, want %+v", info.Limits, *limits)
	info.Limits.InputJSONBytes = 7
	info.Limits.ImagePNG.Max = 7
	info.Limits.Tasks.GrowthRetries = 7
	info.Resources[0].Root = "mutated"
	info.CoreVersion = "mutated"
	info.Capabilities.ImageBGR = false
	require.NoError(t, c.Ping(ctx), "mutated snapshot damaged the session")
	task, err := c.AppendTask(ctx, protocol.TaskRequest{Type: "FutureTask", Params: json.RawMessage(`{"unknown":900719925474099312345}`)})
	require.NoError(t, err, "append after mutation: %+v, %v", task, err)
	require.Equal(t, int32(23), task.TaskID, "append after mutation: %+v, %v", task, err)
	require.NoError(t, c.Close(ctx))
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
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	status, err := c.Status(ctx)
	require.NoError(t, err, "empty status: %+v, %v", status, err)
	require.False(t, status.Running, "empty status: %+v, %v", status, err)
	require.False(t, status.Connected, "empty status: %+v, %v", status, err)
	require.Empty(t, status.Tasks, "empty status: %+v, %v", status, err)
	uuid, err := c.UUID(ctx)
	require.NoError(t, err, "empty uuid: %+v, %v", uuid, err)
	require.Empty(t, uuid.UUID, "empty uuid: %+v, %v", uuid, err)
	require.NoError(t, c.BackToHome(ctx))
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
}

func TestClientConcurrentRequestsAndClose(t *testing.T) {
	for _, mode := range []string{"flow", "reverse"} {
		t.Run(mode, func(t *testing.T) {
			c := launchFixture(t, mode)
			var wg sync.WaitGroup
			for range 32 {
				wg.Go(func() {
					assert.NoError(t, c.Ping(context.Background()))
				})
			}
			wg.Wait()
			for range 16 {
				wg.Go(func() {
					assert.NoError(t, c.Close(context.Background()))
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
			require.Error(t, c.Ping(context.Background()), "faulted session accepted another request")
		})
	}
}

func TestClientRequestAndShutdownDeadlines(t *testing.T) {
	for _, mode := range []string{"hang", "hangclose", "ackhang"} {
		t.Run(mode, func(t *testing.T) {
			cfg := fixtureConfig(t, mode)
			cfg.RequestTimeout, cfg.ShutdownTimeout = 150*time.Millisecond, 200*time.Millisecond
			c, err := Launch(context.Background(), cfg)
			require.NoError(t, err)
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
			c, err := Launch(context.Background(), cfg)
			if err == nil {
				c.Close(context.Background())
			}
			require.Error(t, err, "incompatible worker accepted")
		})
	}
	c := launchFixture(t, "flow")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, c.Ping(ctx), context.Canceled, "precancelled request")
	require.NoError(t, c.Ping(context.Background()), "precancelled request damaged worker")
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
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	require.NoError(t, c.Log(ctx, protocol.LogRequest{Level: "warn", Message: " hello 世界 "}))
	require.NoError(t, c.Log(ctx, protocol.LogRequest{}), "empty log input")
	mk, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "粉碎矿山"})
	require.NoError(t, err, "map_level_key: %+v, %v", mk, err)
	require.Equal(t, protocol.MapLevelKeyResult{StageID: "main-07", Code: "1-7", LevelID: "level_07", Name: "粉碎矿山"}, mk, "map_level_key: %+v, %v", mk, err)
	empty, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{})
	require.NoError(t, err, "not-found is a normal result: %+v, %v", empty, err)
	require.Equal(t, protocol.MapLevelKeyResult{}, empty, "not-found is a normal result: %+v, %v", empty, err)
	name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: ""})
	require.NoError(t, err, "empty id: %+v, %v", name, err)
	require.Equal(t, "Unknown", name.Name, "empty id: %+v, %v", name, err)
	name, err = c.ItemName(ctx, protocol.ItemNameRequest{ID: "known"})
	require.NoError(t, err, "known id: %+v, %v", name, err)
	require.Equal(t, "item-name", name.Name, "known id: %+v, %v", name, err)
	name, err = c.ItemName(ctx, protocol.ItemNameRequest{ID: "missing"})
	require.NoError(t, err, "empty name is a normal result: %+v, %v", name, err)
	require.Empty(t, name.Name, "empty name is a normal result: %+v, %v", name, err)
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
}

func TestClientRemoteRejectionAndBadResponseSchema(t *testing.T) {
	c := launchFixture(t, "reject")
	var rejected *protocol.Error
	err := c.Start(context.Background())
	require.ErrorAs(t, err, &rejected, "native rejection: %v", err)
	require.Equal(t, protocol.NativeRejected, rejected.Code, "native rejection: %v", err)
	require.NoError(t, c.Ping(context.Background()), "native rejection damaged worker")
	c = launchFixture(t, "badschema")
	_, err = c.Status(context.Background())
	requireFailure(t, err, true)
	requireReaped(t, c)
	c = launchFixture(t, "uuidschema")
	_, err = c.UUID(context.Background())
	require.Error(t, err, "bad uuid schema accepted")
	requireFailure(t, err, true)
	requireReaped(t, c)
	c = launchFixture(t, "queryschema")
	_, err = c.MapLevelKey(context.Background(), protocol.MapLevelKeyRequest{Key: "k"})
	require.Error(t, err, "bad map_level_key schema accepted")
	requireFailure(t, err, true)
	requireReaped(t, c)
}

func TestClientRejectsIncompleteClose(t *testing.T) {
	for _, mode := range []string{"badclose", "missingoperation"} {
		t.Run(mode, func(t *testing.T) {
			c := launchFixture(t, mode)
			if mode == "missingoperation" {
				_, err := c.Connect(context.Background(), protocol.ConnectRequest{})
				require.NoError(t, err)
			}
			requireFailure(t, c.Close(context.Background()), true)
			requireReaped(t, c)
		})
	}
}

func TestClientBoundedDiagnostics(t *testing.T) {
	c := launchFixture(t, "stderr")
	require.NoError(t, c.Close(context.Background()))
	logs := c.Diagnostics()
	require.Len(t, logs, diagnosticBytes, "diagnostics: %d bytes", len(logs))
	require.True(t, strings.HasSuffix(logs, "fixture log tail"), "diagnostics: %d bytes", len(logs))
}

// TestClientAsyncOperationsEndToEnd proves the four async submissions share one
// waitable contract: each acceptance yields an operation with a stable nonzero
// ID, the native rejection is a stable code through Wait, successful
// completions arrive per command, and the session closes cleanly afterwards.
func TestClientAsyncOperationsEndToEnd(t *testing.T) {
	c := launchFixture(t, "flow")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	connect, err := c.Connect(ctx, protocol.ConnectRequest{})
	require.NoError(t, err, "connect: %v, %v", connect, err)
	require.Equal(t, int32(41), connect.ID(), "connect: %v, %v", connect, err)
	click, err := c.Click(ctx, protocol.ClickRequest{X: -5, Y: 7})
	require.NoError(t, err, "click: %v, %v", click, err)
	require.Equal(t, int32(42), click.ID(), "click: %v, %v", click, err)
	cap, err := c.Screencap(ctx)
	require.NoError(t, err, "screencap: %v, %v", cap, err)
	require.Equal(t, int32(43), cap.ID(), "screencap: %v, %v", cap, err)
	window, err := c.AttachWindow(ctx, protocol.AttachWindowRequest{Hwnd: 9007199254740993, ScreencapMethod: 1 << 63, MouseMethod: math.MaxUint64, KeyboardMethod: 1 << 40})
	require.NoError(t, err, "attach_window: %v, %v", window, err)
	require.Equal(t, int32(44), window.ID(), "attach_window: %v, %v", window, err)
	res, waitErr := connect.Wait(ctx)
	var rejected *protocol.Error
	require.ErrorAs(t, waitErr, &rejected, "connect terminal state: %+v, %v", res, waitErr)
	require.Equal(t, protocol.NativeRejected, rejected.Code, "connect terminal state: %+v, %v", res, waitErr)
	require.False(t, res.OK, "connect terminal state: %+v, %v", res, waitErr)
	require.Equal(t, int32(41), res.AsyncID, "connect terminal state: %+v, %v", res, waitErr)
	for _, op := range []*Operation{click, cap, window} {
		res, waitErr := op.Wait(ctx)
		require.NoError(t, waitErr, "%d terminal state: %+v, %v", op.ID(), res, waitErr)
		require.True(t, res.OK, "%d terminal state: %+v, %v", op.ID(), res, waitErr)
		require.NotEmpty(t, res.What, "%d terminal state: %+v, %v", op.ID(), res, waitErr)
		require.Equal(t, op.ID(), res.AsyncID, "%d terminal state: %+v, %v", op.ID(), res, waitErr)
	}
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
	// IDs stay readable after settlement and after the session is gone.
	require.Equal(t, int32(41), connect.ID(), "IDs changed after settlement: %d %d", connect.ID(), window.ID())
	require.Equal(t, int32(44), window.ID(), "IDs changed after settlement: %d %d", connect.ID(), window.ID())
}

// TestClientOperationWaitIsRepeatableAndConcurrent proves Wait is repeatable
// and concurrency-safe: repeated and concurrent waits observe the same cached
// terminal state, every call receives an independent copy whose mutation
// cannot rewrite the cache or other callers' results.
func TestClientOperationWaitIsRepeatableAndConcurrent(t *testing.T) {
	c := launchFixture(t, "flow")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	op, err := c.Connect(ctx, protocol.ConnectRequest{})
	require.NoError(t, err)
	first, waitErr := op.Wait(ctx)
	var rejected *protocol.Error
	require.ErrorAs(t, waitErr, &rejected, "first wait: %+v, %v", first, waitErr)
	require.Equal(t, protocol.NativeRejected, rejected.Code, "first wait: %+v, %v", first, waitErr)
	results := make([]protocol.OperationResult, 8)
	var wg sync.WaitGroup
	for i := range results {
		wg.Go(func() {
			r, err := op.Wait(ctx)
			var rejected *protocol.Error
			if assert.ErrorAs(t, err, &rejected, "concurrent wait: %+v, %v", r, err) {
				assert.Equal(t, protocol.NativeRejected, rejected.Code, "concurrent wait: %+v, %v", r, err)
			}
			results[i] = r
		})
	}
	wg.Wait()
	for i, r := range results {
		require.Equal(t, first.AsyncID, r.AsyncID, "wait %d diverged: %+v vs %+v", i, r, first)
		require.Equal(t, first.What, r.What, "wait %d diverged: %+v vs %+v", i, r, first)
		require.Equal(t, first.OK, r.OK, "wait %d diverged: %+v vs %+v", i, r, first)
		require.Equal(t, string(first.Details), string(r.Details), "wait %d diverged: %+v vs %+v", i, r, first)
	}
	// Mutating one returned copy leaves the cache untouched.
	first.Details[0] = 'X'
	again, waitErr := op.Wait(ctx)
	var stillRejected *protocol.Error
	require.ErrorAs(t, waitErr, &stillRejected, "cached result was rewritten through a returned copy: %s, %v", again.Details, waitErr)
	require.Equal(t, protocol.NativeRejected, stillRejected.Code, "cached result was rewritten through a returned copy: %s, %v", again.Details, waitErr)
	require.Equal(t, `{"ret":false}`, string(again.Details), "cached result was rewritten through a returned copy: %s, %v", again.Details, waitErr)
	require.Equal(t, int32(41), op.ID(), "ID changed after settlement: %d", op.ID())
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
}

// TestClientWaitCancellationIsLocal proves a cancelled or timed-out wait ends
// only that wait: the worker stays alive, the submission keeps its native slot,
// a later wait still works, and the eventual close settlement arrives as a
// distinguishable lifecycle error rather than unknown.
func TestClientWaitCancellationIsLocal(t *testing.T) {
	c := launchFixture(t, "pendingops")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	op, err := c.Connect(ctx, protocol.ConnectRequest{})
	require.NoError(t, err)
	for range 2 {
		waitCtx, cancel := context.WithTimeout(ctx, 100*time.Millisecond)
		_, waitErr := op.Wait(waitCtx)
		cancel()
		require.ErrorIs(t, waitErr, context.DeadlineExceeded, "timed-out wait: %v", waitErr)
	}
	require.NoError(t, c.Ping(ctx), "cancelled wait damaged the session")
	type settled struct {
		result protocol.OperationResult
		err    error
	}
	done := make(chan settled, 1)
	go func() {
		r, err := op.Wait(ctx)
		done <- settled{r, err}
	}()
	require.NoError(t, c.Close(ctx))
	got := <-done
	var lifecycle *protocol.Error
	require.ErrorAs(t, got.err, &lifecycle, "close settlement: %+v, %v", got.result, got.err)
	require.Equal(t, protocol.InvalidState, lifecycle.Code, "close settlement: %+v, %v", got.result, got.err)
	require.False(t, got.result.OK, "close settlement: %+v, %v", got.result, got.err)
	require.Empty(t, got.result.What, "close settlement: %+v, %v", got.result, got.err)
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
			_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
			require.NoError(t, err)
			_, _ = c.Connect(ctx, protocol.ConnectRequest{})
			select {
			case <-c.Done():
			case <-time.After(5 * time.Second):
				require.FailNow(t, "session survived a wrong operation result")
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
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	a, err := c.Connect(ctx, protocol.ConnectRequest{})
	require.NoError(t, err)
	b, err := c.Connect(ctx, protocol.ConnectRequest{})
	require.NoError(t, err)
	require.Equal(t, int32(41), a.ID(), "async ids: %d %d", a.ID(), b.ID())
	require.Equal(t, int32(42), b.ID(), "async ids: %d %d", a.ID(), b.ID())
	rb, err := b.Wait(ctx)
	require.NoError(t, err, "second completion: %+v, %v", rb, err)
	require.True(t, rb.OK, "second completion: %+v, %v", rb, err)
	require.Equal(t, int32(42), rb.AsyncID, "second completion: %+v, %v", rb, err)
	ra, err := a.Wait(ctx)
	require.NoError(t, err, "first completion: %+v, %v", ra, err)
	require.True(t, ra.OK, "first completion: %+v, %v", ra, err)
	require.Equal(t, int32(41), ra.AsyncID, "first completion: %+v, %v", ra, err)
	require.NotEqual(t, ra.RequestID, rb.RequestID, "results crossed their request IDs")
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
}

// TestClientAttachWindowWireIsLossless proves the window handle and method
// masks cross the client without numeric loss: the fixture exits with an error
// if the decoded values differ from the >2^53 handle and full-width masks the
// test submitted, so reaching the happy path is the precision proof.
func TestClientAttachWindowWireIsLossless(t *testing.T) {
	c := launchFixture(t, "winmask")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	op, err := c.AttachWindow(ctx, protocol.AttachWindowRequest{Hwnd: 9007199254740993, ScreencapMethod: math.MaxUint64, MouseMethod: math.MaxUint64, KeyboardMethod: 1 << 40})
	require.NoError(t, err)
	res, waitErr := op.Wait(ctx)
	require.NoError(t, waitErr, "attach_window terminal state: %+v, %v", res, waitErr)
	require.True(t, res.OK, "attach_window terminal state: %+v, %v", res, waitErr)
	require.Equal(t, "AttachWindow", res.What, "attach_window terminal state: %+v, %v", res, waitErr)
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
}

// TestClientUnconfirmedOperationAfterFaultIsUnknown proves a session fault
// leaves an operation without any result genuinely unknown: the waiter returns
// the sticky *Failure with Unknown=true — never a fabricated terminal state —
// and no wait hangs against the dead session.
func TestClientUnconfirmedOperationAfterFaultIsUnknown(t *testing.T) {
	c := launchFixture(t, "pendingcrash")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	op, err := c.Connect(ctx, protocol.ConnectRequest{})
	require.NoError(t, err)
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
	require.Error(t, c.Start(ctx), "crashing start succeeded")
	select {
	case got := <-done:
		requireFailure(t, got.err, true)
		require.Empty(t, got.result.What, "a fault must not fabricate a terminal state: %+v", got.result)
		require.False(t, got.result.OK, "a fault must not fabricate a terminal state: %+v", got.result)
		require.Nil(t, got.result.Error, "a fault must not fabricate a terminal state: %+v", got.result)
	case <-time.After(5 * time.Second):
		require.FailNow(t, "wait hung against a faulted session")
	}
	requireReaped(t, c)
	_, err = op.Wait(ctx)
	require.ErrorIs(t, err, c.Err(), "later wait diverged from the sticky failure: %v", err)
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
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	png, err := c.GetImagePNG(ctx)
	require.NoError(t, err)
	wantPNG := fixtureImagePattern(3<<20+4567, 0x50)
	require.True(t, bytes.Equal(png, wantPNG), "png snapshot diverged: %d bytes", len(png))
	bgr, err := c.GetImageBGR(ctx)
	require.NoError(t, err)
	require.True(t, bytes.Equal(bgr, fixtureImagePattern(3<<20+4567, 0x47)), "bgr snapshot diverged: %d bytes", len(bgr))
	// One core event interleaved with each transfer's chunk traffic; both
	// kept their own contiguous sequence domain.
	var sequences []uint64
	for range 2 {
		select {
		case event := <-c.Events():
			require.Equal(t, protocol.CoreEvent, event.Name, "unexpected event: %+v", event)
			sequences = append(sequences, event.Sequence)
		case <-time.After(5 * time.Second):
			require.FailNow(t, "interleaved core event missing")
		}
	}
	require.Len(t, sequences, 2, "event sequences: %v", sequences)
	require.Equal(t, uint64(1), sequences[0], "event sequences: %v", sequences)
	require.Equal(t, uint64(2), sequences[1], "event sequences: %v", sequences)
	select {
	case event := <-c.Events():
		require.FailNow(t, fmt.Sprintf("chunk traffic produced extra events: %+v", event))
	default:
	}
	// The returned snapshots are independently owned: mutating one does not
	// damage the session or any other returned buffer.
	png[0] ^= 0xFF
	require.NoError(t, c.Ping(ctx), "session damaged by caller mutation")
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)

	// Concurrent transfers interleave on the wire while each stays a
	// sequential, backpressured pull of its own snapshot.
	c = launchFixture(t, "images")
	_, err = c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	var wg sync.WaitGroup
	results := make([][]byte, 2)
	errs := make([]error, 2)
	wg.Add(2)
	go func() { defer wg.Done(); results[0], errs[0] = c.GetImagePNG(ctx) }()
	go func() { defer wg.Done(); results[1], errs[1] = c.GetImageBGR(ctx) }()
	wg.Wait()
	for i, err := range errs {
		require.NoError(t, err, "concurrent transfer %d", i)
	}
	require.True(t, bytes.Equal(results[0], wantPNG), "concurrent snapshots diverged")
	require.True(t, bytes.Equal(results[1], fixtureImagePattern(3<<20+4567, 0x47)), "concurrent snapshots diverged")
	require.NoError(t, c.Close(ctx))
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
				require.FailNow(t, "second chunk request never arrived")
			}
			time.Sleep(10 * time.Millisecond)
		}
		cancel()
		err := <-errCh
		require.Error(t, err, "cancelled transfer succeeded")
		requireFailure(t, err, true)
		requireReaped(t, c)
	})
	t.Run("worker rejections stay remote errors", func(t *testing.T) {
		c := launchFixture(t, "chunkerror")
		var rejected *protocol.Error
		_, err := c.GetImagePNG(context.Background())
		require.ErrorAs(t, err, &rejected, "chunk rejection: %v", err)
		require.Equal(t, protocol.LimitExceeded, rejected.Code, "chunk rejection: %v", err)
		require.NoError(t, c.Ping(context.Background()), "session unusable after the rejection")
		require.NoError(t, c.Close(context.Background()))
	})
	t.Run("missing optional BGR symbol is unsupported", func(t *testing.T) {
		c := launchFixture(t, "unsupportedbgr")
		var rejected *protocol.Error
		_, err := c.GetImageBGR(context.Background())
		require.ErrorAs(t, err, &rejected, "bgr rejection: %v", err)
		require.Equal(t, protocol.Unsupported, rejected.Code, "bgr rejection: %v", err)
		png, err := c.GetImagePNG(context.Background())
		require.NoError(t, err, "png after bgr rejection: %d bytes, %v", len(png), err)
		require.True(t, bytes.Equal(png, fixtureImagePattern(3<<20+4567, 0x50)), "png after bgr rejection: %d bytes, %v", len(png), err)
		require.NoError(t, c.Close(context.Background()))
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
		require.NoError(t, err)
		require.NotNil(t, image, "empty image must be an empty non-nil snapshot: %#v", image)
		require.Empty(t, image, "empty image must be an empty non-nil snapshot: %#v", image)
	}
	require.NoError(t, c.Ping(ctx))
	require.NoError(t, c.Close(ctx))
}

// waitFault waits for the client's native fault observation.
func waitFault(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.Fault():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the native fault observation never fired")
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
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	// The fault is armed by Start; the event channel is left undrained on
	// purpose — the fault observation must not depend on the consumer.
	require.NoError(t, c.Start(ctx))
	waitFault(t, c)
	instErr := c.InstanceErr()
	require.NotNil(t, instErr, "instance error = %v, want event_overflow", instErr)
	require.Equal(t, protocol.EventOverflow, instErr.Code, "instance error = %v, want event_overflow", instErr)

	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
	select {
	case <-c.InstanceDone():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the native terminal observation never fired")
	}
	snap, ok := c.InstanceDiagnostics()
	require.True(t, ok, "the terminal snapshot was reported missing after delivery")
	require.Equal(t, uint64(math.MaxUint64), snap.LostEvents, "lost_events = %d, want the saturated count", snap.LostEvents)
	require.NotNil(t, snap.FirstFault, "first fault = %+v, want event_overflow", snap.FirstFault)
	require.Equal(t, protocol.EventOverflow, snap.FirstFault.Code, "first fault = %+v, want event_overflow", snap.FirstFault)
	require.Nil(t, snap.StopDiagnostic, "unexpected stop diagnostic: %+v", snap.StopDiagnostic)
	want := []byte{0xDE, 0x00, 0xFF, 0xFE, 0x80}
	require.Equal(t, string(want), string(snap.RawPrefix), "raw prefix changed: %q, want %q", snap.RawPrefix, want)
	// The final snapshot is stable and every access is an independent copy.
	snap.RawPrefix[0] = 'X'
	if snap.FirstFault.Detail != nil {
		snap.FirstFault.Detail.Kind = "mutated"
	}
	again, ok := c.InstanceDiagnostics()
	require.True(t, ok, "snapshot was rewritten through a returned copy: %+v, %t", again, ok)
	require.Equal(t, string(want), string(again.RawPrefix), "snapshot was rewritten through a returned copy: %+v, %t", again, ok)
	require.NotNil(t, again.FirstFault, "snapshot was rewritten through a returned copy: %+v, %t", again, ok)
	require.Equal(t, protocol.EventOverflow, again.FirstFault.Code, "snapshot was rewritten through a returned copy: %+v, %t", again, ok)
	// The process done channel is a separate observation from the native one;
	// after a normal close both are closed, but neither substitutes the other.
	select {
	case <-c.Done():
	default:
		require.FailNow(t, "process done must be closed after a reaped close")
	}
}

// TestClientConfirmedOperationSurvivesLaterFault proves a confirmed result is
// never rewritten: an operation that settled successfully before the fault
// keeps its cached success after the fault event and the terminal snapshot.
func TestClientConfirmedOperationSurvivesLaterFault(t *testing.T) {
	c := launchFixture(t, "faultdiag")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	op, err := c.Click(ctx, protocol.ClickRequest{X: -5, Y: 7})
	require.NoError(t, err)
	first, waitErr := op.Wait(ctx)
	require.NoError(t, waitErr, "click terminal state: %+v, %v", first, waitErr)
	require.True(t, first.OK, "click terminal state: %+v, %v", first, waitErr)
	require.NoError(t, c.Start(ctx))
	waitFault(t, c)
	again, waitErr := op.Wait(ctx)
	require.NoError(t, waitErr, "confirmed result was rewritten by the fault: %+v, %v", again, waitErr)
	require.True(t, again.OK, "confirmed result was rewritten by the fault: %+v, %v", again, waitErr)
	require.Equal(t, string(first.Details), string(again.Details), "confirmed result was rewritten by the fault: %+v, %v", again, waitErr)
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
	final, waitErr := op.Wait(ctx)
	require.NoError(t, waitErr, "confirmed result changed after close: %+v, %v", final, waitErr)
	require.True(t, final.OK, "confirmed result changed after close: %+v, %v", final, waitErr)
}

// TestClientFaultObservationDoesNotDependOnEventConsumption proves the
// supervisor-level fault report never waits for the business consumer: with
// nobody draining Events, the fault and the terminal snapshot are still
// observed within a bounded time and the close completes.
func TestClientFaultObservationDoesNotDependOnEventConsumption(t *testing.T) {
	c := launchFixture(t, "faultdiag")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	// No goroutine reads c.Events() in this test.
	require.NoError(t, c.Start(ctx))
	waitFault(t, c)
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
	select {
	case <-c.InstanceDone():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "the terminal snapshot never fired without a consumer")
	}
	_, ok := c.InstanceDiagnostics()
	require.True(t, ok, "the terminal snapshot was reported missing without a consumer")
}

// TestClientStopDiagnosticWithoutFault proves a stop-only diagnostic is a
// terminal fact of its own: it is delivered in the snapshot, the fault
// observation never fires, and the instance error stays nil.
func TestClientStopDiagnosticWithoutFault(t *testing.T) {
	c := launchFixture(t, "stopdiag")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	require.NoError(t, c.Close(ctx))
	requireReaped(t, c)
	select {
	case <-c.Fault():
		require.FailNow(t, "a stop-only diagnostic must not close the fault observation")
	default:
	}
	instErr := c.InstanceErr()
	require.Nil(t, instErr, "instance error = %v, want nil", instErr)
	snap, ok := c.InstanceDiagnostics()
	require.True(t, ok, "snapshot = %+v, %t", snap, ok)
	require.NotNil(t, snap.StopDiagnostic, "snapshot = %+v, %t", snap, ok)
	require.Equal(t, protocol.NativeRejected, snap.StopDiagnostic.Code, "snapshot = %+v, %t", snap, ok)
	require.Nil(t, snap.FirstFault, "snapshot = %+v, %t", snap, ok)
	require.Equal(t, uint64(0), snap.LostEvents, "snapshot = %+v, %t", snap, ok)
}

// TestClientKilledProcessReportsDiagnosticsUnavailable proves the process and
// native observations stay separate: a crash closes the process Done but
// never the native Done or the fault observation, and the missing snapshot is
// reported as unavailable instead of being fabricated.
func TestClientKilledProcessReportsDiagnosticsUnavailable(t *testing.T) {
	c := launchFixture(t, "crash")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	err = c.Start(ctx)
	// The exit races the EOF on the pipes, so only the unknown-discipline is
	// pinned here: the process is gone and no native fact survived it.
	requireFailure(t, err, true)
	requireReaped(t, c)
	select {
	case <-c.InstanceDone():
		require.FailNow(t, "a killed process must not fabricate the native terminal state")
	default:
	}
	select {
	case <-c.Fault():
		require.FailNow(t, "a killed process must not fabricate a native fault")
	default:
	}
	instErr := c.InstanceErr()
	require.Nil(t, instErr, "instance error = %v, want nil", instErr)
	_, ok := c.InstanceDiagnostics()
	require.False(t, ok, "a killed process must not fabricate a diagnostics snapshot")
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
			_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
			require.NoError(t, err)
			err = c.Start(ctx)
			if err != nil {
				if mode == "badfault" {
					requireFailure(t, err, true)
					requireReaped(t, c)
					return
				}
				require.NoError(t, err)
			}
			err = c.Close(ctx)
			failure := requireFailure(t, err, true)
			require.Equal(t, "protocol_error", failure.Code, "failure code = %s, want protocol_error", failure.Code)
			requireReaped(t, c)
		})
	}
}

// requireIPCStopped asserts the session left no IPC goroutine behind: after
// Done, both the read and the write loop must have returned.
func requireIPCStopped(t *testing.T, c *Client) {
	t.Helper()
	select {
	case <-c.readDone:
	default:
		require.FailNow(t, "the read loop outlived Done")
	}
	select {
	case <-c.writeDone:
	default:
		require.FailNow(t, "the write loop outlived Done")
	}
}

// TestClientConcurrentTransfersOperationsFaultAndClose proves the frozen
// shutdown sequence under full load: large image transfers, settled async
// operations and a native fault all share the session while a concurrent
// Close races them. Every participant exits with a terminal fact — a
// completed image is the full verified snapshot, never a prefix; a fetch that
// overlaps the closing admission either delivers that snapshot or fails;
// confirmed operation results survive the fault and the close; the final
// diagnostics stay readable — and the process is reaped with no IPC goroutine
// outliving Done.
func TestClientConcurrentTransfersOperationsFaultAndClose(t *testing.T) {
	// The fixture subprocess re-generates every snapshot and encodes every
	// chunk in-process, which the race instrumentation slows down enough to
	// outrun the short fixture deadlines; this test pins concurrency and
	// shutdown invariants, not latency, so it runs with the production
	// default budgets.
	cfg := fixtureConfig(t, "faultdiag")
	cfg.RequestTimeout, cfg.ShutdownTimeout = 30*time.Second, 5*time.Second
	c, err := Launch(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	ctx := context.Background()
	_, err = c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	wantPNG := fixtureImagePattern(3<<20+4567, 0x50)
	wantBGR := fixtureImagePattern(3<<20+4567, 0x47)

	var wg sync.WaitGroup
	for i := range 6 {
		wg.Go(func() {
			fetch, want := c.GetImagePNG, wantPNG
			if i%2 == 1 {
				fetch, want = c.GetImageBGR, wantBGR
			}
			got, err := fetch(ctx)
			if !assert.NoError(t, err, "transfer %d", i) {
				return
			}
			assert.True(t, bytes.Equal(got, want), "transfer %d delivered %d bytes instead of the verified snapshot", i, len(got))
		})
	}
	for range 4 {
		wg.Go(func() {
			op, err := c.Click(ctx, protocol.ClickRequest{X: -5, Y: 7})
			if !assert.NoError(t, err) {
				return
			}
			result, waitErr := op.Wait(ctx)
			if !assert.NoError(t, waitErr, "click terminal state: %+v, %v", result, waitErr) {
				return
			}
			assert.True(t, result.OK, "click terminal state: %+v, %v", result, waitErr)
		})
	}
	wg.Wait()

	// The native fault is armed by Start. From here the closers, a last
	// transfer and a repeat wait race each other on the shared terminal facts.
	require.NoError(t, c.Start(ctx))
	waitFault(t, c)
	wg = sync.WaitGroup{}
	for range 8 {
		wg.Go(func() {
			assert.NoError(t, c.Close(ctx), "concurrent close")
		})
	}
	wg.Go(func() {
		// Overlapping the closing admission is allowed to fail, never to
		// succeed partially: whatever comes back must be the whole verified
		// snapshot or an error.
		got, err := c.GetImagePNG(ctx)
		if err == nil {
			assert.True(t, bytes.Equal(got, wantPNG), "racing transfer delivered %d bytes instead of the verified snapshot", len(got))
		}
	})
	wg.Wait()
	requireReaped(t, c)
	requireIPCStopped(t, c)

	// A native instance fault does not fail the process close, and the final
	// observations stay readable after Done: the buffered events drain, then
	// the closed stream terminates the range.
	snap, ok := c.InstanceDiagnostics()
	require.True(t, ok, "final diagnostics after the concurrent close: %+v, %t", snap, ok)
	require.NotNil(t, snap.FirstFault, "final diagnostics after the concurrent close: %+v, %t", snap, ok)
	require.Equal(t, protocol.EventOverflow, snap.FirstFault.Code, "final diagnostics after the concurrent close: %+v, %t", snap, ok)
	closed := time.After(5 * time.Second)
	for {
		select {
		case _, open := <-c.Events():
			if !open {
				return
			}
		case <-closed:
			require.FailNow(t, "the event stream never closed after Done")
		}
	}
}

// TestClientKillReleasesEveryWaiter proves a forced termination never invents
// a terminal state and never strands a waiter: an accepted operation the
// process took to its grave reports unknown to every concurrent waiter, the
// process is reaped, and no IPC goroutine outlives Done.
func TestClientKillReleasesEveryWaiter(t *testing.T) {
	c := launchFixture(t, "pendingcrash")
	ctx := context.Background()
	_, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	op, err := c.Connect(ctx, protocol.ConnectRequest{})
	require.NoError(t, err)
	var wg sync.WaitGroup
	waitErrs := make([]error, 8)
	for i := range 8 {
		wg.Go(func() {
			_, waitErrs[i] = op.Wait(ctx)
		})
	}
	// Let every waiter block on the unsettled operation, then crash the
	// process under them: no wait may invent a result, a cancellation or a
	// rollback, and none may hang.
	time.Sleep(100 * time.Millisecond)
	require.Error(t, c.Start(ctx), "the crashed session reported a successful start")
	wg.Wait()
	for _, waitErr := range waitErrs {
		requireFailure(t, waitErr, true)
	}
	requireReaped(t, c)
	requireIPCStopped(t, c)
}

// TestClientCloseDuringStuckTransferStaysBounded proves an unconfirmed image
// cannot buy a clean close: with a chunk response withheld mid-transfer,
// Close stops admission, fails within its shutdown deadline, kills and reaps
// the worker, and the interrupted fetch reports failure instead of a partial
// success.
func TestClientCloseDuringStuckTransferStaysBounded(t *testing.T) {
	cfg := fixtureConfig(t, "slowchunk")
	cfg.ShutdownTimeout = 500 * time.Millisecond
	c, err := Launch(context.Background(), cfg)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	ctx := context.Background()
	_, err = c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "fixture"})
	require.NoError(t, err)
	fetchErr := make(chan error, 1)
	go func() { _, err := c.GetImagePNG(ctx); fetchErr <- err }()
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(c.Diagnostics(), "slowchunk: second chunk request received") {
		if time.Now().After(deadline) {
			require.FailNow(t, "the second chunk request never arrived")
		}
		time.Sleep(10 * time.Millisecond)
	}
	closing := make(chan error, 1)
	go func() { closing <- c.Close(context.Background()) }()
	select {
	case err := <-fetchErr:
		require.Error(t, err, "the interrupted transfer reported success")
		requireFailure(t, err, true)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "the interrupted fetch never returned")
	}
	select {
	case err := <-closing:
		requireFailure(t, err, true)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "close did not finish within its bounded deadline")
	}
	requireReaped(t, c)
	requireIPCStopped(t, c)
}
