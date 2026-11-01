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
package app

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/dongwlin/rhodeck/packages/control/worker"
	"github.com/dongwlin/rhodeck/packages/control/worker/protocol"
	"github.com/stretchr/testify/require"
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
	require.NoError(t, err, "resolve fixture source")
	outPath := filepath.Join(t.TempDir(), queryFixtureSOName)
	args := []string{"-shared", "-fPIC", "-O2", "-o", outPath, src}
	t.Logf("query fixture build: compiler=%s platform=%s/%s command=%s", compiler, runtime.GOOS, runtime.GOARCH, compiler+" "+strings.Join(args, " "))
	output, err := exec.Command(compiler, args...).CombinedOutput()
	require.NoError(t, err, "failed to compile the query fixture:\n%s", output)
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
	require.NoError(t, err)
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
	require.NoError(t, err, "initialize; diagnostics: %s", c.Diagnostics())
	want := protocol.Capabilities{ImageBGR: false, AttachWindow: false, MapLevelKey: true, ItemName: true}
	require.Equal(t, want, info.Capabilities, "fixture capability snapshot = %+v, want %+v", info.Capabilities, want)
	return info
}

func TestQueryFixtureExplicitCoreVersion(t *testing.T) {
	library := buildQueryFixture(t)
	// With no caller expectation the actual version is reported, without a
	// repository-wide release gate. Each initialization uses a fresh process.
	baseline := launchQueryFixtureWorker(t, library)
	actual := initializeQueryFixtureWorker(t, baseline, library)
	require.NotEmpty(t, actual.CoreVersion, "missing actual Core version")
	require.NoError(t, baseline.Close(context.Background()))
	for _, matching := range []bool{true, false} {
		name, expected := "matching", actual.CoreVersion
		if !matching {
			name, expected = "mismatched", actual.CoreVersion+"-different"
		}
		t.Run(name, func(t *testing.T) {
			c := launchQueryFixtureWorker(t, library)
			root := t.TempDir()
			info, err := c.Initialize(context.Background(), protocol.InitializeRequest{
				SnapshotID: "explicit-version-fixture", LibraryPath: library, UserDir: root,
				ResourceRoots: []string{root}, ExpectedCoreVersion: expected,
			})
			if matching {
				require.NoError(t, err, "matching version: info=%+v", info)
				require.Equal(t, expected, info.CoreVersion, "matching version: info=%+v", info)
				return
			}
			var remote *protocol.Error
			require.ErrorAs(t, err, &remote, "version mismatch: got %v, want snapshot_mismatch", err)
			require.Equal(t, protocol.SnapshotMismatch, remote.Code, "version mismatch: got %v, want snapshot_mismatch", err)
		})
	}
}

// fixtureLogReadback reads the recorded log through the fixture's UUID
// observation: "<level>\x1f<message>\x1f<count>".
func fixtureLogReadback(t *testing.T, c *worker.Client) string {
	t.Helper()
	res, err := c.UUID(context.Background())
	require.NoError(t, err, "log readback; diagnostics: %s", c.Diagnostics())
	return res.UUID
}

// requireRejectionCode runs call and requires a *protocol.Error with exactly
// the expected stable code.
func requireRejectionCode(t *testing.T, call func() error, code string) {
	t.Helper()
	err := call()
	require.Error(t, err, "expected %s rejection, got success", code)
	var rejected *protocol.Error
	require.ErrorAs(t, err, &rejected, "error = %v, want code %s", err, code)
	require.Equal(t, code, rejected.Code, "error = %v, want code %s", err, code)
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
	err := c.Log(ctx, protocol.LogRequest{Level: "queryfx", Message: "hello 世界 — log fidelity ✔"})
	require.NoError(t, err, "log; diagnostics: %s", c.Diagnostics())
	got := fixtureLogReadback(t, c)
	require.Equal(t, "queryfx\x1fhello 世界 — log fidelity ✔\x1f1", got, "recorded log diverges from the input: %q", got)

	// Empty strings are valid log inputs; the C void return is answered as a
	// normal result that states only "the call returned".
	err = c.Log(ctx, protocol.LogRequest{})
	require.NoError(t, err, "empty log")
	emptyReadback := fixtureLogReadback(t, c)
	require.Equal(t, "\x1f\x1f2", emptyReadback, "empty log input was not recorded verbatim: %q", emptyReadback)

	// An input the binding rejects (embedded NUL) fails with invalid_input
	// and provably never reaches the native log: the recorded readback is
	// unchanged.
	err = c.Log(ctx, protocol.LogRequest{Level: "x", Message: "a\x00b"})
	require.Error(t, err, "embedded NUL accepted")
	rejected := classify(err)
	require.NotNil(t, rejected, "NUL log error = %v, want invalid_input", err)
	require.Equal(t, protocol.InvalidInput, rejected.Code, "NUL log error = %v, want invalid_input", err)
	got = fixtureLogReadback(t, c)
	require.Equal(t, emptyReadback, got, "rejected log input reached the native boundary: %q", got)

	// Experiment level lookup: a key the binding rejects (embedded NUL)
	// fails with invalid_input before any native call, so the first hit
	// afterwards still carries call number 1.
	requireRejectionCode(t, func() error {
		_, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "a\x00b"})
		return err
	}, protocol.InvalidInput)
	hit1, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "queryfx-hit"})
	require.NoError(t, err, "map_level_key hit")
	require.Equal(t, protocol.MapLevelKeyResult{StageID: "queryfx-stage#1", Code: "queryfx-code#1", LevelID: "queryfx-level#1", Name: "queryfx-name#1"}, hit1, "four-field result diverges or fields are misaligned: %+v", hit1)

	// Not found is the zero value: a normal result, not an error.
	miss, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "no-such-level"})
	require.NoError(t, err, "not-found result = %+v — the zero value must be a normal result", miss)
	require.Equal(t, protocol.MapLevelKeyResult{}, miss, "not-found result = %+v, %v — the zero value must be a normal result", miss, err)

	// The next hit rewrites the fixture's borrowed buffers. The earlier
	// result must stay its own snapshot: each response encoded an
	// independent copy of the borrowed data at its own call time.
	hit2, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "queryfx-hit"})
	require.NoError(t, err, "second hit = %+v", hit2)
	require.Equal(t, "queryfx-stage#2", hit2.StageID, "second hit = %+v, %v", hit2, err)
	require.Equal(t, "queryfx-stage#1", hit1.StageID, "earlier snapshot was rewritten by a later call: %+v", hit1)
	require.Equal(t, "queryfx-name#1", hit1.Name, "earlier snapshot was rewritten by a later call: %+v", hit1)

	// Item lookup: the empty id answers the native "Unknown", an unknown id
	// answers the empty string — both normal results — and a rejected id
	// never reaches the native side.
	name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: ""})
	require.NoError(t, err, "empty id = %+v", name)
	require.Equal(t, "Unknown", name.Name, "empty id = %+v, %v", name, err)
	requireRejectionCode(t, func() error {
		_, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "a\x00b"})
		return err
	}, protocol.InvalidInput)
	name, err = c.ItemName(ctx, protocol.ItemNameRequest{ID: "queryfx-item"})
	require.NoError(t, err, "item hit = %+v", name)
	require.Equal(t, "queryfx-item#1", name.Name, "item hit = %+v, %v", name, err)
	name, err = c.ItemName(ctx, protocol.ItemNameRequest{ID: "no-such-item"})
	require.NoError(t, err, "empty name = %+v — must be a normal result", name)
	require.Equal(t, "", name.Name, "empty name = %+v, %v — must be a normal result", name, err)
	name, err = c.ItemName(ctx, protocol.ItemNameRequest{ID: "queryfx-item"})
	require.NoError(t, err, "second item hit = %+v", name)
	require.Equal(t, "queryfx-item#2", name.Name, "second item hit = %+v, %v", name, err)

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
	err = c.Ping(ctx)
	require.NoError(t, err, "session unusable after contract breaches")
	got = fixtureLogReadback(t, c)
	require.Equal(t, "\x1f\x1f2", got, "query traffic disturbed the recorded log: %q", got)

	// The fixture never calls back: the query traffic must not have produced
	// any event.
	select {
	case event := <-c.Events():
		require.FailNow(t, fmt.Sprintf("query traffic produced events: %+v", event))
	default:
	}
	err = c.Close(ctx)
	require.NoError(t, err, "close; diagnostics: %s", c.Diagnostics())
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
	require.NoError(t, err, "initialize; diagnostics: %s", c.Diagnostics())
	want := protocol.Capabilities{ImageBGR: true, AttachWindow: false, MapLevelKey: false, ItemName: false}
	require.Equal(t, want, info.Capabilities, "capability snapshot = %+v, want %+v", info.Capabilities, want)
	ctx := context.Background()
	_, err = c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "queryfx-hit"})
	require.Error(t, err, "map_level_key answered without the optional symbol")
	mkRejected := classify(err)
	require.NotNil(t, mkRejected, "map_level_key error = %v, want unsupported", err)
	require.Equal(t, protocol.Unsupported, mkRejected.Code, "map_level_key error = %v, want unsupported", err)
	_, err = c.ItemName(ctx, protocol.ItemNameRequest{ID: "queryfx-item"})
	require.Error(t, err, "item_name answered without the optional symbol")
	inRejected := classify(err)
	require.NotNil(t, inRejected, "item_name error = %v, want unsupported", err)
	require.Equal(t, protocol.Unsupported, inRejected.Code, "item_name error = %v, want unsupported", err)
	err = c.Ping(ctx)
	require.NoError(t, err, "session unusable after unsupported rejections")
	err = c.Close(ctx)
	require.NoError(t, err, "close")
}
