package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	"github.com/stretchr/testify/require"
)

var processBinary struct {
	once      sync.Once
	dir, path string
	err       error
}

func TestMain(m *testing.M) {
	code := m.Run()
	if processBinary.dir != "" {
		_ = os.RemoveAll(processBinary.dir)
	}
	os.Exit(code)
}

func workerBinary(t *testing.T) string {
	t.Helper()
	processBinary.once.Do(func() {
		processBinary.dir, processBinary.err = os.MkdirTemp("", "rhodeck-worker-test-")
		if processBinary.err != nil {
			return
		}
		processBinary.path = filepath.Join(processBinary.dir, "core-worker")
		if runtime.GOOS == "windows" {
			processBinary.path += ".exe"
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		cmd := exec.CommandContext(ctx, "go", "build", "-o", processBinary.path, "github.com/dongwlin/rhodeck/apps/core-worker")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			processBinary.err = errors.New(string(out) + err.Error())
		}
	})
	require.NoError(t, processBinary.err)
	return processBinary.path
}

func launchWorker(t *testing.T) *worker.Client {
	t.Helper()
	c, err := worker.Launch(context.Background(), worker.Config{Executable: workerBinary(t), StartupTimeout: 5 * time.Second, RequestTimeout: 30 * time.Second, ShutdownTimeout: 5 * time.Second})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestWorkerProcess(t *testing.T) {
	t.Run("handshake and uninitialized close", func(t *testing.T) {
		c := launchWorker(t)
		g := c.Greeting()
		require.Equal(t, "dev", g.WorkerVersion, "greeting: %+v", g)
		require.Len(t, g.SessionID, 32, "greeting: %+v", g)
		require.Equal(t, runtime.GOOS, g.OS, "greeting: %+v", g)
		require.Equal(t, runtime.GOARCH, g.Arch, "greeting: %+v", g)
		err := c.Ping(context.Background())
		require.NoError(t, err)
		err = c.Close(context.Background())
		require.NoError(t, err)
	})
	t.Run("load failure is typed and terminal", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "missing-library", LibraryPath: filepath.Join(root, "missing-library"), UserDir: root, ResourceRoots: []string{root}})
		var remote *protocol.Error
		require.ErrorAs(t, err, &remote, "load failure: %v", err)
		require.Equal(t, protocol.LoadFailed, remote.Code, "load failure: %v", err)
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "failed initializer did not exit")
		}
	})
	t.Run("snapshot hash mismatch precedes native loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		require.NoError(t, os.WriteFile(library, []byte("not a dynamic library"), 0600))
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "mismatch", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}, ExpectedLibrarySHA256: strings.Repeat("0", 64)})
		var remote *protocol.Error
		require.ErrorAs(t, err, &remote, "snapshot check: %v", err)
		require.Equal(t, protocol.SnapshotMismatch, remote.Code, "snapshot check: %v", err)
	})
	t.Run("binding rejects zero-capacity limits before loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		require.NoError(t, os.WriteFile(library, []byte("not a dynamic library"), 0600))
		limits := bindingDefaultLimits
		limits.ImagePNG.Initial = 0
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "zero-capacity", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}, Limits: &limits})
		var remote *protocol.Error
		require.ErrorAs(t, err, &remote, "zero capacity must be rejected by the binding with its field path: %v", err)
		require.Equal(t, protocol.InvalidInput, remote.Code, "zero capacity must be rejected by the binding with its field path: %v", err)
		require.True(t, strings.Contains(remote.Message, "Limits.ImagePNG.Initial"), "zero capacity must be rejected by the binding with its field path: %v", err)
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "rejected initializer did not exit")
		}
	})
	t.Run("binding rejects contradictory limits before loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		require.NoError(t, os.WriteFile(library, []byte("not a dynamic library"), 0600))
		limits := bindingDefaultLimits
		limits.UUID = protocol.BufferLimits{Initial: 4097, Max: 4096, GrowthRetries: 6}
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "contradiction", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}, Limits: &limits})
		var remote *protocol.Error
		require.ErrorAs(t, err, &remote, "contradictory limits must be rejected by the binding with its field path: %v", err)
		require.Equal(t, protocol.InvalidInput, remote.Code, "contradictory limits must be rejected by the binding with its field path: %v", err)
		require.True(t, strings.Contains(remote.Message, "Limits.UUID.Initial"), "contradictory limits must be rejected by the binding with its field path: %v", err)
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "rejected initializer did not exit")
		}
	})
	// A complete, binding-legal limit configuration must reach the native
	// factory; with a placeholder library the attempt fails as load_failed,
	// which proves validation passed and the mapping was applied.
	t.Run("complete explicit limits reach native loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		require.NoError(t, os.WriteFile(library, []byte("not a dynamic library"), 0600))
		limits := bindingDefaultLimits
		limits.ImageBGR = protocol.BufferLimits{Initial: 4 << 20, Max: 128 << 20, GrowthRetries: 5}
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "explicit-limits", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}, Limits: &limits})
		var remote *protocol.Error
		require.ErrorAs(t, err, &remote, "legal explicit limits must reach native loading: %v", err)
		require.Equal(t, protocol.LoadFailed, remote.Code, "legal explicit limits must reach native loading: %v", err)
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "failed initializer did not exit")
		}
	})
	t.Run("omitted limits reach native loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		require.NoError(t, os.WriteFile(library, []byte("not a dynamic library"), 0600))
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "default-limits", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}})
		var remote *protocol.Error
		require.ErrorAs(t, err, &remote, "omitted limits must select the whole default set and load: %v", err)
		require.Equal(t, protocol.LoadFailed, remote.Code, "omitted limits must select the whole default set and load: %v", err)
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "failed initializer did not exit")
		}
	})
}

// Opt in with explicit, caller-owned paths. This test never connects, starts a
// task, discovers devices, or uses an ADB executable. The only asynchronous
// probe is an unconnected Screencap: Core rejects it locally with ret=false
// without contacting any device, the same failure probe the real-Core
// binding acceptance uses. CI excludes this test explicitly.
func TestRealCoreWorkerNoDevice(t *testing.T) {
	library := os.Getenv("RHODECK_WORKER_CORE_LIBRARY")
	root := os.Getenv("RHODECK_WORKER_RESOURCE_ROOT")
	digest := os.Getenv("RHODECK_WORKER_LIBRARY_SHA256")
	expectedVersion := os.Getenv("RHODECK_WORKER_CORE_VERSION")
	if library == "" && root == "" && digest == "" && expectedVersion == "" {
		t.Skip("real Core worker unverified: explicit library, resource root, SHA256 and version were not provided")
	}
	require.True(t, filepath.IsAbs(library), "all real Core inputs must be explicit, including SHA256 and version")
	require.True(t, filepath.IsAbs(root), "all real Core inputs must be explicit, including SHA256 and version")
	require.Len(t, digest, 64, "all real Core inputs must be explicit, including SHA256 and version")
	require.NotEmpty(t, expectedVersion, "all real Core inputs must be explicit, including SHA256 and version")
	ctx := context.Background()

	t.Run("default limits, metadata, queries and rejections", func(t *testing.T) {
		c := launchWorker(t)
		info, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "explicit-no-device-test", LibraryPath: library, UserDir: t.TempDir(), ResourceRoots: []string{root}, ExpectedLibrarySHA256: digest, ExpectedCoreVersion: expectedVersion})
		require.NoError(t, err, "initialize; diagnostics: %s", c.Diagnostics())
		// Metadata: the actual library and resource digests, the native
		// NullSize sentinel and the whole materialized default limit set must
		// come back in the immutable snapshot.
		require.Equal(t, digest, info.LibrarySHA256, "runtime snapshot: %+v", info)
		require.Equal(t, expectedVersion, info.CoreVersion, "runtime snapshot: %+v", info)
		require.Len(t, info.Resources, 1, "runtime snapshot: %+v", info)
		require.Len(t, info.Resources[0].VersionSHA256, 64, "runtime snapshot: %+v", info)
		require.Equal(t, uint64(math.MaxUint64), info.NullSize, "null size sentinel: got %d, want %d", info.NullSize, uint64(math.MaxUint64))
		require.NotNil(t, info.Limits, "omitted limits must report the materialized default set: %+v", info.Limits)
		require.Equal(t, bindingDefaultLimits, *info.Limits, "omitted limits must report the materialized default set: %+v", info.Limits)
		if runtime.GOOS != "windows" {
			require.False(t, info.Capabilities.AttachWindow, "async attach window is Windows-only and must be reported absent on %s", runtime.GOOS)
		}
		assertQueryCapabilityMatchesBehavior(t, c, info.Capabilities)
		// Queries: a log line and the device UUID are plain native calls with
		// no device on the other end; UUID without a connection must not
		// imply one (empty string is a normal result).
		err = c.Log(ctx, protocol.LogRequest{Level: "Trace", Message: "worker no-device probe 9007199254740993"})
		require.NoError(t, err, "log; diagnostics: %s", c.Diagnostics())
		uuid, err := c.UUID(ctx)
		require.NoError(t, err, "uuid; diagnostics: %s", c.Diagnostics())
		status, err := c.Status(ctx)
		require.NoError(t, err, "uuid read must not open an implicit connection: %+v", status)
		require.False(t, status.Connected, "uuid read must not open an implicit connection: %+v, %v", status, err)
		// Rejections: Core decides task-type acceptance, and a task id that
		// was never appended is a native false.
		_, err = c.AppendTask(ctx, protocol.TaskRequest{Type: "RhodeckUnknownTaskForProtocolTest", Params: json.RawMessage(`{"future":9007199254740993}`)})
		require.True(t, isNativeRejected(err), "Core must decide unknown task acceptance: %v", err)
		var ids []int32
		for range 2 {
			r, err := c.AppendTask(ctx, protocol.TaskRequest{Type: "Award", Params: json.RawMessage(`{"enable":false,"future":9007199254740993}`)})
			require.NoError(t, err, "append only: %+v", r)
			require.NotZero(t, r.TaskID, "append only: %+v, %v", r, err)
			ids = append(ids, r.TaskID)
		}
		err = c.SetTaskParams(ctx, protocol.TaskParamsRequest{TaskID: ids[0], Params: json.RawMessage(`{"enable":false}`)})
		require.NoError(t, err)
		err = c.SetTaskParams(ctx, protocol.TaskParamsRequest{TaskID: 1 << 30, Params: json.RawMessage(`{"enable":false}`)})
		require.True(t, isNativeRejected(err), "params for a task id that was never appended must be a native rejection: %v", err)
		status, err = c.Status(ctx)
		require.NoError(t, err, "unconnected status: %+v, %v", status, err)
		require.False(t, status.Running, "unconnected status: %+v, %v", status, err)
		require.False(t, status.Connected, "unconnected status: %+v, %v", status, err)
		require.Len(t, status.Tasks, 2, "unconnected status: %+v, %v", status, err)
		for _, id := range ids {
			found := false
			for _, actual := range status.Tasks {
				found = found || id == actual
			}
			require.True(t, found, "missing submitted task %d: %+v", id, status)
		}
		// Stop on a never-started instance is a plain native observation, not
		// a device action; Core clears its pending task list and caches.
		_, err = c.Stop(ctx)
		require.NoError(t, err, "stop on a never-started instance; diagnostics: %s", c.Diagnostics())
		// Unconnected Screencap failure probe: the submission is accepted, the
		// native completion arrives with ok=false and is reported as the
		// stable native_rejected code, never as a success.
		op, err := c.Screencap(ctx)
		require.NoError(t, err, "screencap submission")
		require.NotZero(t, op.ID(), "screencap submission must return a nonzero async id")
		res, err := op.Wait(ctx)
		if err == nil {
			require.FailNow(t, fmt.Sprintf("unconnected screencap must not report success: %+v", res))
		}
		var remote *protocol.Error
		require.ErrorAs(t, err, &remote, "unconnected screencap must settle as native_rejected: %v", err)
		require.Equal(t, protocol.NativeRejected, remote.Code, "unconnected screencap must settle as native_rejected: %v", err)
		require.Equal(t, "Screencap", res.What, "screencap completion must keep its identity: %+v", res)
		require.False(t, res.OK, "screencap completion must keep its identity: %+v", res)
		require.Equal(t, op.ID(), res.AsyncID, "screencap completion must keep its identity: %+v", res)
		// Cached image reads without any capture: an empty image is a legal,
		// complete transfer, not an error and not a fake success.
		png, err := c.GetImagePNG(ctx)
		require.NoError(t, err, "empty PNG cache must be an empty transfer: %d bytes, %v", len(png), err)
		require.Empty(t, png, "empty PNG cache must be an empty transfer: %d bytes, %v", len(png), err)
		bgr, err := c.GetImageBGR(ctx)
		require.NoError(t, err, "empty BGR cache must be an empty transfer: %d bytes, %v", len(bgr), err)
		require.Empty(t, bgr, "empty BGR cache must be an empty transfer: %d bytes, %v", len(bgr), err)
		err = c.Ping(ctx)
		require.NoError(t, err)
		err = c.Close(ctx)
		require.NoError(t, err, "close; diagnostics: %s", c.Diagnostics())
		diagnostics, callbacks, operations := drainClosedEvents(t, c)
		require.Zero(t, diagnostics.LostEvents, "clean close must deliver a fault-free diagnostics snapshot: %+v", diagnostics)
		require.Nil(t, diagnostics.FirstFault, "clean close must deliver a fault-free diagnostics snapshot: %+v", diagnostics)
		// The unconnected Screencap completion (message 4) precedes Destroyed
		// (message 5) on the same FIFO observation stream; the buffered events
		// stay readable after the close response, and the worker's own
		// operation event carries the same terminal state.
		require.Len(t, callbacks, 2, "expected the screencap completion then Destroyed, got %+v", callbacks)
		require.Equal(t, int32(4), callbacks[0].MessageID, "expected the screencap completion then Destroyed, got %+v", callbacks)
		require.Equal(t, int32(5), callbacks[1].MessageID, "expected the screencap completion then Destroyed, got %+v", callbacks)
		require.Equal(t, uint64(1), callbacks[0].CoreSequence, "expected the screencap completion then Destroyed, got %+v", callbacks)
		require.Equal(t, uint64(2), callbacks[1].CoreSequence, "expected the screencap completion then Destroyed, got %+v", callbacks)
		require.Len(t, operations, 1, "operation event must carry the screencap terminal state: %+v", operations)
		require.Equal(t, "Screencap", operations[0].What, "operation event must carry the screencap terminal state: %+v", operations)
		require.False(t, operations[0].OK, "operation event must carry the screencap terminal state: %+v", operations)
		require.Equal(t, op.ID(), operations[0].AsyncID, "operation event must carry the screencap terminal state: %+v", operations)
		require.NotNil(t, operations[0].Error, "operation event must carry the screencap terminal state: %+v", operations)
		require.True(t, isNativeRejected(operations[0].Error), "operation event must carry the screencap terminal state: %+v", operations)
		t.Logf("platform=%s/%s Core=%s library_sha256=%s resource_version_sha256=%s null_size=%d uuid=%q capabilities=%+v; metadata/config/query/rejection/close passed without device actions",
			runtime.GOOS, runtime.GOARCH, info.CoreVersion, info.LibrarySHA256, info.Resources[0].VersionSHA256, info.NullSize, uuid.UUID, info.Capabilities)
	})

	// A complete, binding-legal explicit limit set must reach the real native
	// factory and come back as the effective configuration. This is the only
	// place where the explicit mapping meets a real Core, so it uses a second
	// worker process (one Runtime per process).
	t.Run("explicit limits reach the real factory", func(t *testing.T) {
		c := launchWorker(t)
		limits := bindingDefaultLimits
		limits.ImageBGR = protocol.BufferLimits{Initial: 4 << 20, Max: 128 << 20, GrowthRetries: 5}
		info, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "explicit-no-device-limits", LibraryPath: library, UserDir: t.TempDir(), ResourceRoots: []string{root}, ExpectedLibrarySHA256: digest, ExpectedCoreVersion: expectedVersion, Limits: &limits})
		require.NoError(t, err, "initialize with explicit limits; diagnostics: %s", c.Diagnostics())
		require.NotNil(t, info.Limits, "explicit limits must be applied and reported verbatim: %+v", info.Limits)
		require.Equal(t, limits, *info.Limits, "explicit limits must be applied and reported verbatim: %+v", info.Limits)
		require.Equal(t, uint64(math.MaxUint64), info.NullSize, "null size sentinel: got %d, want %d", info.NullSize, uint64(math.MaxUint64))
		err = c.Close(ctx)
		require.NoError(t, err, "close; diagnostics: %s", c.Diagnostics())
		diagnostics, callbacks, operations := drainClosedEvents(t, c)
		require.Zero(t, diagnostics.LostEvents, "clean close must deliver a fault-free diagnostics snapshot: %+v", diagnostics)
		require.Nil(t, diagnostics.FirstFault, "clean close must deliver a fault-free diagnostics snapshot: %+v", diagnostics)
		require.Empty(t, operations, "no operation ran in this flow: %+v", operations)
		require.Len(t, callbacks, 1, "expected the sole Destroyed callback, got %+v", callbacks)
		require.Equal(t, int32(5), callbacks[0].MessageID, "expected the sole Destroyed callback, got %+v", callbacks)
		require.Equal(t, uint64(1), callbacks[0].CoreSequence, "expected the sole Destroyed callback, got %+v", callbacks)
	})
}

// isNativeRejected reports whether err is the worker's stable
// native_rejected protocol error.
func isNativeRejected(err error) bool {
	var remote *protocol.Error
	return errors.As(err, &remote) && remote.Code == protocol.NativeRejected
}

// assertQueryCapabilityMatchesBehavior proves the capability report and the
// command outcome agree: an optional interface the loaded library does not
// export must be rejected with the stable unsupported code before any native
// call, and a present one must execute with the normal zero-value "not found"
// result. Absence is recorded as absence, never faked into a success path.
func assertQueryCapabilityMatchesBehavior(t *testing.T, c *worker.Client, caps protocol.Capabilities) {
	t.Helper()
	ctx := context.Background()
	_, keyErr := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: ""})
	_, nameErr := c.ItemName(ctx, protocol.ItemNameRequest{ID: ""})
	if caps.MapLevelKey {
		require.NoError(t, keyErr, "reported-present MapLevelKey must execute, not-found is a normal result")
	}
	if !caps.MapLevelKey {
		require.True(t, isUnsupported(keyErr), "absent MapLevelKey must be stable unsupported: %v", keyErr)
	}
	if caps.ItemName {
		require.NoError(t, nameErr, "reported-present ItemName must execute, not-found is a normal result")
	}
	if !caps.ItemName {
		require.True(t, isUnsupported(nameErr), "absent ItemName must be stable unsupported: %v", nameErr)
	}
}

func isUnsupported(err error) bool {
	var remote *protocol.Error
	return errors.As(err, &remote) && remote.Code == protocol.Unsupported
}

// drainClosedEvents reads the buffered observation after a clean close: the
// terminal diagnostics snapshot, the native callbacks and the worker's own
// operation events. The delivery order between these kinds is not part of the
// frozen contract, so they are matched by name; the callback FIFO order itself
// stays observable.
func drainClosedEvents(t *testing.T, c *worker.Client) (protocol.InstanceDiagnostics, []protocol.Callback, []protocol.OperationResult) {
	t.Helper()
	var diagnostics *protocol.InstanceDiagnostics
	var callbacks []protocol.Callback
	var operations []protocol.OperationResult
	for event := range c.Events() {
		switch event.Name {
		case protocol.DiagnosticsEvent:
			require.Nil(t, diagnostics, "second diagnostics event: %+v", event)
			var snap protocol.InstanceDiagnostics
			require.NoError(t, protocol.Decode(event.Payload, &snap), "undecodable diagnostics payload: %+v", event)
			diagnostics = &snap
		case protocol.CoreEvent:
			var callback protocol.Callback
			require.NoError(t, protocol.Decode(event.Payload, &callback), "undecodable callback payload: %+v", event)
			callbacks = append(callbacks, callback)
		case protocol.OperationEvent:
			var result protocol.OperationResult
			require.NoError(t, protocol.Decode(event.Payload, &result), "undecodable operation payload: %+v", event)
			operations = append(operations, result)
		default:
			require.FailNow(t, fmt.Sprintf("unexpected event %q: %+v", event.Name, event))
		}
	}
	require.NotNil(t, diagnostics, "clean close must deliver a terminal diagnostics snapshot")
	return *diagnostics, callbacks, operations
}
