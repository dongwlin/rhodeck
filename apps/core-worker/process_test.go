package main

import (
	"context"
	"encoding/json"
	"errors"
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
// task, discovers devices, or uses an ADB executable. CI excludes it explicitly.
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
	c := launchWorker(t)
	ctx := context.Background()
	info, err := c.Initialize(ctx, protocol.InitializeRequest{SnapshotID: "explicit-no-device-test", LibraryPath: library, UserDir: t.TempDir(), ResourceRoots: []string{root}, ExpectedLibrarySHA256: digest, ExpectedCoreVersion: expectedVersion})
	if err != nil {
		t.Fatalf("initialize: %v; diagnostics: %s", err, c.Diagnostics())
	}
	if info.LibrarySHA256 != digest || info.CoreVersion != expectedVersion || len(info.Resources) != 1 || len(info.Resources[0].VersionSHA256) != 64 {
		t.Fatalf("runtime snapshot: %+v", info)
	}
	_, err = c.AppendTask(ctx, protocol.TaskRequest{Type: "RhodeckUnknownTaskForProtocolTest", Params: json.RawMessage(`{"future":9007199254740993}`)})
	var rejected *protocol.Error
	if !errors.As(err, &rejected) || rejected.Code != protocol.NativeRejected {
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
	if err := c.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("close: %v; diagnostics: %s", err, c.Diagnostics())
	}
	var callbacks []protocol.Callback
	for event := range c.Events() {
		var callback protocol.Callback
		if event.Name != protocol.CoreEvent || protocol.Decode(event.Payload, &callback) != nil {
			t.Fatalf("unexpected event: %+v", event)
		}
		callbacks = append(callbacks, callback)
	}
	if len(callbacks) != 1 || callbacks[0].MessageID != 5 || callbacks[0].CoreSequence != 1 {
		t.Fatalf("expected sole Destroyed callback, got %+v", callbacks)
	}
	t.Logf("platform=%s/%s Core=%s library_sha256=%s resource_version_sha256=%s; initialize/reject-unknown/append/update/status/destroy passed without device actions", runtime.GOOS, runtime.GOARCH, info.CoreVersion, info.LibrarySHA256, info.Resources[0].VersionSHA256)
}
