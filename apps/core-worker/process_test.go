package main

import (
	"context"
	"encoding/json"
	"errors"
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
		cmd := exec.CommandContext(ctx, "go", "build", "-o", processBinary.path, ".")
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
		out, err := cmd.CombinedOutput()
		if err != nil {
			processBinary.err = errors.New(string(out) + err.Error())
		}
	})
	if processBinary.err != nil {
		t.Fatal(processBinary.err)
	}
	return processBinary.path
}

func launchWorker(t *testing.T) *worker.Client {
	t.Helper()
	c, err := worker.Launch(context.Background(), worker.Config{Executable: workerBinary(t), StartupTimeout: 5 * time.Second, RequestTimeout: 30 * time.Second, ShutdownTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

func TestWorkerProcess(t *testing.T) {
	t.Run("handshake and uninitialized close", func(t *testing.T) {
		c := launchWorker(t)
		if g := c.Greeting(); g.WorkerVersion != "dev" || len(g.SessionID) != 32 || g.OS != runtime.GOOS || g.Arch != runtime.GOARCH {
			t.Fatalf("greeting: %+v", g)
		}
		if err := c.Ping(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("load failure is typed and terminal", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "missing-library", LibraryPath: filepath.Join(root, "missing-library"), UserDir: root, ResourceRoots: []string{root}})
		var remote *protocol.Error
		if !errors.As(err, &remote) || remote.Code != protocol.LoadFailed {
			t.Fatalf("load failure: %v", err)
		}
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("failed initializer did not exit")
		}
	})
	t.Run("snapshot hash mismatch precedes native loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		if err := os.WriteFile(library, []byte("not a dynamic library"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "mismatch", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}, ExpectedLibrarySHA256: strings.Repeat("0", 64)})
		var remote *protocol.Error
		if !errors.As(err, &remote) || remote.Code != protocol.SnapshotMismatch {
			t.Fatalf("snapshot check: %v", err)
		}
	})
	t.Run("binding rejects zero-capacity limits before loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		if err := os.WriteFile(library, []byte("not a dynamic library"), 0600); err != nil {
			t.Fatal(err)
		}
		limits := bindingDefaultLimits
		limits.ImagePNG.Initial = 0
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "zero-capacity", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}, Limits: &limits})
		var remote *protocol.Error
		if !errors.As(err, &remote) || remote.Code != protocol.InvalidInput || !strings.Contains(remote.Message, "Limits.ImagePNG.Initial") {
			t.Fatalf("zero capacity must be rejected by the binding with its field path: %v", err)
		}
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("rejected initializer did not exit")
		}
	})
	t.Run("binding rejects contradictory limits before loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		if err := os.WriteFile(library, []byte("not a dynamic library"), 0600); err != nil {
			t.Fatal(err)
		}
		limits := bindingDefaultLimits
		limits.UUID = protocol.BufferLimits{Initial: 4097, Max: 4096, GrowthRetries: 6}
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "contradiction", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}, Limits: &limits})
		var remote *protocol.Error
		if !errors.As(err, &remote) || remote.Code != protocol.InvalidInput || !strings.Contains(remote.Message, "Limits.UUID.Initial") {
			t.Fatalf("contradictory limits must be rejected by the binding with its field path: %v", err)
		}
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("rejected initializer did not exit")
		}
	})
	// A complete, binding-legal limit configuration must reach the native
	// factory; with a placeholder library the attempt fails as load_failed,
	// which proves validation passed and the mapping was applied.
	t.Run("complete explicit limits reach native loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		if err := os.WriteFile(library, []byte("not a dynamic library"), 0600); err != nil {
			t.Fatal(err)
		}
		limits := bindingDefaultLimits
		limits.ImageBGR = protocol.BufferLimits{Initial: 4 << 20, Max: 128 << 20, GrowthRetries: 5}
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "explicit-limits", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}, Limits: &limits})
		var remote *protocol.Error
		if !errors.As(err, &remote) || remote.Code != protocol.LoadFailed {
			t.Fatalf("legal explicit limits must reach native loading: %v", err)
		}
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("failed initializer did not exit")
		}
	})
	t.Run("omitted limits reach native loading", func(t *testing.T) {
		c := launchWorker(t)
		root := t.TempDir()
		library := filepath.Join(root, "placeholder")
		if err := os.WriteFile(library, []byte("not a dynamic library"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "default-limits", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}})
		var remote *protocol.Error
		if !errors.As(err, &remote) || remote.Code != protocol.LoadFailed {
			t.Fatalf("omitted limits must select the whole default set and load: %v", err)
		}
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("failed initializer did not exit")
		}
	})
}

// Opt in with explicit, caller-owned paths. This test never connects, starts a
// task, discovers devices, or uses an ADB executable. The only asynchronous
// probe is an unconnected Screencap: Core rejects it locally with ret=false
// without contacting any device, the same failure probe the locked-Core
// binding acceptance uses. CI excludes this test explicitly.
func TestRealCoreWorkerNoDevice(t *testing.T) {
	library := os.Getenv("RHODECK_WORKER_CORE_LIBRARY")
	root := os.Getenv("RHODECK_WORKER_RESOURCE_ROOT")
	digest := os.Getenv("RHODECK_WORKER_LIBRARY_SHA256")
	expectedVersion := os.Getenv("RHODECK_WORKER_CORE_VERSION")
	if library == "" && root == "" && digest == "" && expectedVersion == "" {
		t.Skip("real Core worker unverified: explicit library, resource root, SHA256 and version were not provided")
	}
	if !filepath.IsAbs(library) || !filepath.IsAbs(root) || len(digest) != 64 || expectedVersion == "" {
		t.Fatal("all real Core inputs must be explicit, including SHA256 and version")
	}
	ctx := context.Background()

	t.Run("default limits, metadata, queries and rejections", func(t *testing.T) {
		c := launchWorker(t)
		info, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "explicit-no-device-test", LibraryPath: library, UserDir: t.TempDir(), ResourceRoots: []string{root}, ExpectedLibrarySHA256: digest, ExpectedCoreVersion: expectedVersion})
		if err != nil {
			t.Fatalf("initialize: %v; diagnostics: %s", err, c.Diagnostics())
		}
		// Metadata: the locked library and resource digests, the native
		// NullSize sentinel and the whole materialized default limit set must
		// come back in the immutable snapshot.
		if info.LibrarySHA256 != digest || info.CoreVersion != expectedVersion || len(info.Resources) != 1 || len(info.Resources[0].VersionSHA256) != 64 {
			t.Fatalf("runtime snapshot: %+v", info)
		}
		if info.NullSize != math.MaxUint64 {
			t.Fatalf("null size sentinel: got %d, want %d", info.NullSize, uint64(math.MaxUint64))
		}
		if info.Limits == nil || *info.Limits != bindingDefaultLimits {
			t.Fatalf("omitted limits must report the materialized default set: %+v", info.Limits)
		}
		if runtime.GOOS != "windows" && info.Capabilities.AttachWindow {
			t.Fatalf("async attach window is Windows-only and must be reported absent on %s", runtime.GOOS)
		}
		assertQueryCapabilityMatchesBehavior(t, c, info.Capabilities)
		// Queries: a log line and the device UUID are plain native calls with
		// no device on the other end; UUID without a connection must not
		// imply one (empty string is a normal result).
		if err := c.Log(ctx, protocol.LogRequest{Level: "Trace", Message: "worker no-device probe 9007199254740993"}); err != nil {
			t.Fatalf("log: %v; diagnostics: %s", err, c.Diagnostics())
		}
		uuid, err := c.UUID(ctx)
		if err != nil {
			t.Fatalf("uuid: %v; diagnostics: %s", err, c.Diagnostics())
		}
		if status, err := c.Status(ctx); err != nil || status.Connected {
			t.Fatalf("uuid read must not open an implicit connection: %+v, %v", status, err)
		}
		// Rejections: Core decides task-type acceptance, and a task id that
		// was never appended is a native false.
		if _, err := c.AppendTask(ctx, protocol.TaskRequest{Type: "RhodeckUnknownTaskForProtocolTest", Params: json.RawMessage(`{"future":9007199254740993}`)}); !isNativeRejected(err) {
			t.Fatalf("Core must decide unknown task acceptance: %v", err)
		}
		var ids []int32
		for range 2 {
			r, err := c.AppendTask(ctx, protocol.TaskRequest{Type: "Award", Params: json.RawMessage(`{"enable":false,"future":9007199254740993}`)})
			if err != nil || r.TaskID == 0 {
				t.Fatalf("append only: %+v, %v", r, err)
			}
			ids = append(ids, r.TaskID)
		}
		if err := c.SetTaskParams(ctx, protocol.TaskParamsRequest{TaskID: ids[0], Params: json.RawMessage(`{"enable":false}`)}); err != nil {
			t.Fatal(err)
		}
		if err := c.SetTaskParams(ctx, protocol.TaskParamsRequest{TaskID: 1 << 30, Params: json.RawMessage(`{"enable":false}`)}); !isNativeRejected(err) {
			t.Fatalf("params for a task id that was never appended must be a native rejection: %v", err)
		}
		status, err := c.Status(ctx)
		if err != nil || status.Running || status.Connected || len(status.Tasks) != 2 {
			t.Fatalf("unconnected status: %+v, %v", status, err)
		}
		for _, id := range ids {
			found := false
			for _, actual := range status.Tasks {
				found = found || id == actual
			}
			if !found {
				t.Fatalf("missing submitted task %d: %+v", id, status)
			}
		}
		// Stop on a never-started instance is a plain native observation, not
		// a device action; Core clears its pending task list and caches.
		if _, err := c.Stop(ctx); err != nil {
			t.Fatalf("stop on a never-started instance: %v; diagnostics: %s", err, c.Diagnostics())
		}
		// Unconnected Screencap failure probe: the submission is accepted, the
		// native completion arrives with ok=false and is reported as the
		// stable native_rejected code, never as a success.
		op, err := c.Screencap(ctx)
		if err != nil {
			t.Fatalf("screencap submission: %v", err)
		}
		if op.ID() == 0 {
			t.Fatal("screencap submission must return a nonzero async id")
		}
		if res, err := op.Wait(ctx); err != nil {
			var remote *protocol.Error
			if !errors.As(err, &remote) || remote.Code != protocol.NativeRejected {
				t.Fatalf("unconnected screencap must settle as native_rejected: %v", err)
			}
			if res.What != "Screencap" || res.OK || res.AsyncID != op.ID() {
				t.Fatalf("screencap completion must keep its identity: %+v", res)
			}
		} else {
			t.Fatalf("unconnected screencap must not report success: %+v", res)
		}
		// Cached image reads without any capture: an empty image is a legal,
		// complete transfer, not an error and not a fake success.
		png, err := c.GetImagePNG(ctx)
		if err != nil || len(png) != 0 {
			t.Fatalf("empty PNG cache must be an empty transfer: %d bytes, %v", len(png), err)
		}
		bgr, err := c.GetImageBGR(ctx)
		if err != nil || len(bgr) != 0 {
			t.Fatalf("empty BGR cache must be an empty transfer: %d bytes, %v", len(bgr), err)
		}
		if err := c.Ping(ctx); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(ctx); err != nil {
			t.Fatalf("close: %v; diagnostics: %s", err, c.Diagnostics())
		}
		diagnostics, callbacks, operations := drainClosedEvents(t, c)
		if diagnostics.LostEvents != 0 || diagnostics.FirstFault != nil {
			t.Fatalf("clean close must deliver a fault-free diagnostics snapshot: %+v", diagnostics)
		}
		// The unconnected Screencap completion (message 4) precedes Destroyed
		// (message 5) on the same FIFO observation stream; the buffered events
		// stay readable after the close response, and the worker's own
		// operation event carries the same terminal state.
		if len(callbacks) != 2 || callbacks[0].MessageID != 4 || callbacks[1].MessageID != 5 ||
			callbacks[0].CoreSequence != 1 || callbacks[1].CoreSequence != 2 {
			t.Fatalf("expected the screencap completion then Destroyed, got %+v", callbacks)
		}
		if len(operations) != 1 || operations[0].What != "Screencap" || operations[0].OK ||
			operations[0].AsyncID != op.ID() || operations[0].Error == nil || !isNativeRejected(operations[0].Error) {
			t.Fatalf("operation event must carry the screencap terminal state: %+v", operations)
		}
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
		if err != nil {
			t.Fatalf("initialize with explicit limits: %v; diagnostics: %s", err, c.Diagnostics())
		}
		if info.Limits == nil || *info.Limits != limits {
			t.Fatalf("explicit limits must be applied and reported verbatim: %+v", info.Limits)
		}
		if info.NullSize != math.MaxUint64 {
			t.Fatalf("null size sentinel: got %d, want %d", info.NullSize, uint64(math.MaxUint64))
		}
		if err := c.Close(ctx); err != nil {
			t.Fatalf("close: %v; diagnostics: %s", err, c.Diagnostics())
		}
		diagnostics, callbacks, operations := drainClosedEvents(t, c)
		if diagnostics.LostEvents != 0 || diagnostics.FirstFault != nil {
			t.Fatalf("clean close must deliver a fault-free diagnostics snapshot: %+v", diagnostics)
		}
		if len(operations) != 0 {
			t.Fatalf("no operation ran in this flow: %+v", operations)
		}
		if len(callbacks) != 1 || callbacks[0].MessageID != 5 || callbacks[0].CoreSequence != 1 {
			t.Fatalf("expected the sole Destroyed callback, got %+v", callbacks)
		}
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
	if caps.MapLevelKey && keyErr != nil {
		t.Fatalf("reported-present MapLevelKey must execute, not-found is a normal result: %v", keyErr)
	}
	if !caps.MapLevelKey && !isUnsupported(keyErr) {
		t.Fatalf("absent MapLevelKey must be stable unsupported: %v", keyErr)
	}
	if caps.ItemName && nameErr != nil {
		t.Fatalf("reported-present ItemName must execute, not-found is a normal result: %v", nameErr)
	}
	if !caps.ItemName && !isUnsupported(nameErr) {
		t.Fatalf("absent ItemName must be stable unsupported: %v", nameErr)
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
			if diagnostics != nil {
				t.Fatalf("second diagnostics event: %+v", event)
			}
			var snap protocol.InstanceDiagnostics
			if protocol.Decode(event.Payload, &snap) != nil {
				t.Fatalf("undecodable diagnostics payload: %+v", event)
			}
			diagnostics = &snap
		case protocol.CoreEvent:
			var callback protocol.Callback
			if protocol.Decode(event.Payload, &callback) != nil {
				t.Fatalf("undecodable callback payload: %+v", event)
			}
			callbacks = append(callbacks, callback)
		case protocol.OperationEvent:
			var result protocol.OperationResult
			if protocol.Decode(event.Payload, &result) != nil {
				t.Fatalf("undecodable operation payload: %+v", event)
			}
			operations = append(operations, result)
		default:
			t.Fatalf("unexpected event %q: %+v", event.Name, event)
		}
	}
	if diagnostics == nil {
		t.Fatal("clean close must deliver a terminal diagnostics snapshot")
	}
	return *diagnostics, callbacks, operations
}
