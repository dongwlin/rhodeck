//go:build linux

// This file verifies the Runtime log and experiment query commands end to
// end: the real core-worker binary loads a controlled native library
// (compiled at test time) whose AsstLog records what actually reached the
// native boundary — read back through the fixture's UUID observation — and
// whose AsstGetMapLevelKey/AsstGetItemName serve counter-stamped borrowed
// strings, so field misalignment, stale views and calls that should never
// have happened are all visible. The capability-absence case loads the image
// fixture, which exports no experiment symbols, and proves the stable
// unsupported rejection matches the reported capability snapshot. Nothing
// touches a device: no connect, no screencap, no task start.
package main

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
)

const (
	queryFixtureSrcRel = "testdata/queryfixture/fixture.c"
	queryFixtureSOName = "libqueryfixture.so"
)

// buildQueryFixture compiles the controlled native library and returns the
// .so absolute path. Missing cc/gcc is a test-environment limitation and is
// reported as a skip, never as a pass.
func buildQueryFixture(t *testing.T) string {
	t.Helper()
	compiler := ""
	for _, name := range []string{"cc", "gcc"} {
		if path, err := exec.LookPath(name); err == nil {
			compiler = path
			break
		}
	}
	if compiler == "" {
		t.Skipf("query fixture blocked: no C compiler found; controlled native verification not executed, does not count as a pass")
	}
	src, err := filepath.Abs(queryFixtureSrcRel)
	if err != nil {
		t.Fatalf("resolve fixture source: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), queryFixtureSOName)
	args := []string{"-shared", "-fPIC", "-O2", "-o", outPath, src}
	t.Logf("query fixture build: compiler=%s platform=%s/%s command=%s", compiler, runtime.GOOS, runtime.GOARCH, compiler+" "+strings.Join(args, " "))
	output, err := exec.Command(compiler, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("failed to compile the query fixture: %v\n%s", err, output)
	}
	return outPath
}

// launchQueryFixtureWorker starts one real worker process against the
// controlled native library.
func launchQueryFixtureWorker(t *testing.T, library string) *worker.Client {
	t.Helper()
	c, err := worker.Launch(context.Background(), worker.Config{
		Executable:      workerBinary(t),
		StartupTimeout:  15 * time.Second,
		RequestTimeout:  60 * time.Second,
		ShutdownTimeout: 15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close(context.Background()) })
	return c
}

// initializeQueryFixtureWorker initializes the fixture instance and pins the
// capability snapshot: both experiment symbols are exported, so both
// capabilities must be reported; the image BGR and window symbols are not.
func initializeQueryFixtureWorker(t *testing.T, c *worker.Client, library string) protocol.RuntimeInfo {
	t.Helper()
	root := t.TempDir()
	info, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "query-fixture", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}})
	if err != nil {
		t.Fatalf("initialize: %v; diagnostics: %s", err, c.Diagnostics())
	}
	want := protocol.Capabilities{ImageBGR: false, AttachWindow: false, MapLevelKey: true, ItemName: true}
	if info.Capabilities != want {
		t.Fatalf("fixture capability snapshot = %+v, want %+v", info.Capabilities, want)
	}
	return info
}

// fixtureLogReadback reads the recorded log through the fixture's UUID
// observation: "<level>\x1f<message>\x1f<count>".
func fixtureLogReadback(t *testing.T, c *worker.Client) string {
	t.Helper()
	res, err := c.UUID(context.Background())
	if err != nil {
		t.Fatalf("log readback: %v; diagnostics: %s", err, c.Diagnostics())
	}
	return res.UUID
}

// requireRejectionCode runs call and requires a *protocol.Error with exactly
// the expected stable code.
func requireRejectionCode(t *testing.T, call func() error, code string) {
	t.Helper()
	err := call()
	if err == nil {
		t.Fatalf("expected %s rejection, got success", code)
	}
	var rejected *protocol.Error
	if !errors.As(err, &rejected) || rejected.Code != code {
		t.Fatalf("error = %v, want code %s", err, code)
	}
}

// TestControlledNativeLogAndExperimentQueries proves the production chain of
// the log and the two experiment queries: the log input reaches the native
// AsstLog byte for byte (empty strings included) and a rejected input never
// reaches it; the four-field level result and the item name carry every
// field without loss, with zero values, empty names and the native
// "Unknown" as normal results; successive calls see fresh copies of the
// borrowed thread-local buffers, never a stale view; bad native strings and
// a null pointer arrive as protocol_error; and the session survives every
// classified rejection.
func TestControlledNativeLogAndExperimentQueries(t *testing.T) {
	library := buildQueryFixture(t)
	c := launchQueryFixtureWorker(t, library)
	initializeQueryFixtureWorker(t, c, library)
	ctx := context.Background()

	// The log input crosses verbatim, including non-ASCII bytes and spaces,
	// and the fixture records it as the first log call.
	if err := c.Log(ctx, protocol.LogRequest{Level: "queryfx", Message: "hello 世界 — log fidelity ✔"}); err != nil {
		t.Fatalf("log: %v; diagnostics: %s", err, c.Diagnostics())
	}
	if got := fixtureLogReadback(t, c); got != "queryfx\x1fhello 世界 — log fidelity ✔\x1f1" {
		t.Fatalf("recorded log diverges from the input: %q", got)
	}

	// Empty strings are valid log inputs; the C void return is answered as a
	// normal result that states only "the call returned".
	if err := c.Log(ctx, protocol.LogRequest{}); err != nil {
		t.Fatalf("empty log: %v", err)
	}
	emptyReadback := fixtureLogReadback(t, c)
	if emptyReadback != "\x1f\x1f2" {
		t.Fatalf("empty log input was not recorded verbatim: %q", emptyReadback)
	}

	// An input the binding rejects (embedded NUL) fails with invalid_input
	// and provably never reaches the native log: the recorded readback is
	// unchanged.
	if err := c.Log(ctx, protocol.LogRequest{Level: "x", Message: "a\x00b"}); err == nil {
		t.Fatal("embedded NUL accepted")
	} else if rejected := classify(err); rejected == nil || rejected.Code != protocol.InvalidInput {
		t.Fatalf("NUL log error = %v, want invalid_input", err)
	}
	if got := fixtureLogReadback(t, c); got != emptyReadback {
		t.Fatalf("rejected log input reached the native boundary: %q", got)
	}

	// Experiment level lookup: a key the binding rejects (embedded NUL)
	// fails with invalid_input before any native call, so the first hit
	// afterwards still carries call number 1.
	requireRejectionCode(t, func() error {
		_, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "a\x00b"})
		return err
	}, protocol.InvalidInput)
	hit1, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "queryfx-hit"})
	if err != nil {
		t.Fatalf("map_level_key hit: %v", err)
	}
	if hit1 != (protocol.MapLevelKeyResult{StageID: "queryfx-stage#1", Code: "queryfx-code#1", LevelID: "queryfx-level#1", Name: "queryfx-name#1"}) {
		t.Fatalf("four-field result diverges or fields are misaligned: %+v", hit1)
	}

	// Not found is the zero value: a normal result, not an error.
	miss, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "no-such-level"})
	if err != nil || miss != (protocol.MapLevelKeyResult{}) {
		t.Fatalf("not-found result = %+v, %v — the zero value must be a normal result", miss, err)
	}

	// The next hit rewrites the fixture's borrowed buffers. The earlier
	// result must stay its own snapshot: each response encoded an
	// independent copy of the borrowed data at its own call time.
	hit2, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "queryfx-hit"})
	if err != nil || hit2.StageID != "queryfx-stage#2" {
		t.Fatalf("second hit = %+v, %v", hit2, err)
	}
	if hit1.StageID != "queryfx-stage#1" || hit1.Name != "queryfx-name#1" {
		t.Fatalf("earlier snapshot was rewritten by a later call: %+v", hit1)
	}

	// Item lookup: the empty id answers the native "Unknown", an unknown id
	// answers the empty string — both normal results — and a rejected id
	// never reaches the native side.
	if name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: ""}); err != nil || name.Name != "Unknown" {
		t.Fatalf("empty id = %+v, %v", name, err)
	}
	requireRejectionCode(t, func() error {
		_, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "a\x00b"})
		return err
	}, protocol.InvalidInput)
	if name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "queryfx-item"}); err != nil || name.Name != "queryfx-item#1" {
		t.Fatalf("item hit = %+v, %v", name, err)
	}
	if name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "no-such-item"}); err != nil || name.Name != "" {
		t.Fatalf("empty name = %+v, %v — must be a normal result", name, err)
	}
	if name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "queryfx-item"}); err != nil || name.Name != "queryfx-item#2" {
		t.Fatalf("second item hit = %+v, %v", name, err)
	}

	// A bad native string (invalid UTF-8, no NUL within the scan window) and
	// a null return are native contract breaches: protocol_error, never a
	// fabricated success or empty result, and the session survives them all.
	requireRejectionCode(t, func() error {
		_, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "queryfx-badutf8"})
		return err
	}, protocol.ProtocolError)
	requireRejectionCode(t, func() error {
		_, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "queryfx-nonul"})
		return err
	}, protocol.ProtocolError)
	requireRejectionCode(t, func() error { _, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "queryfx-badutf8"}); return err }, protocol.ProtocolError)
	requireRejectionCode(t, func() error { _, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "queryfx-nil"}); return err }, protocol.ProtocolError)
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("session unusable after contract breaches: %v", err)
	}
	if got := fixtureLogReadback(t, c); got != "\x1f\x1f2" {
		t.Fatalf("query traffic disturbed the recorded log: %q", got)
	}

	// The fixture never calls back: the query traffic must not have produced
	// any event.
	select {
	case event := <-c.Events():
		t.Fatalf("query traffic produced events: %+v", event)
	default:
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("close: %v; diagnostics: %s", err, c.Diagnostics())
	}
}

// TestControlledNativeQueryCapabilityAbsence proves the capability gate end
// to end: the image fixture exports no experiment symbols, the initialize
// snapshot reports both capabilities false, and both query commands answer
// with the stable unsupported rejection — without any native call to fake,
// because there is no symbol to call — while the session survives.
func TestControlledNativeQueryCapabilityAbsence(t *testing.T) {
	library := buildImageFixture(t)
	c := launchImageFixtureWorker(t, library, 0, 0)
	root := t.TempDir()
	info, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "query-absence", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}})
	if err != nil {
		t.Fatalf("initialize: %v; diagnostics: %s", err, c.Diagnostics())
	}
	want := protocol.Capabilities{ImageBGR: true, AttachWindow: false, MapLevelKey: false, ItemName: false}
	if info.Capabilities != want {
		t.Fatalf("capability snapshot = %+v, want %+v", info.Capabilities, want)
	}
	ctx := context.Background()
	if _, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "queryfx-hit"}); err == nil {
		t.Fatal("map_level_key answered without the optional symbol")
	} else if rejected := classify(err); rejected == nil || rejected.Code != protocol.Unsupported {
		t.Fatalf("map_level_key error = %v, want unsupported", err)
	}
	if _, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "queryfx-item"}); err == nil {
		t.Fatal("item_name answered without the optional symbol")
	} else if rejected := classify(err); rejected == nil || rejected.Code != protocol.Unsupported {
		t.Fatalf("item_name error = %v, want unsupported", err)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("session unusable after unsupported rejections: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
}
