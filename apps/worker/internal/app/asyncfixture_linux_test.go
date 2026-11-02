//go:build linux

// This file is the cross-layer acceptance matrix of the full worker: the real
// worker binary loads a controlled native library (compiled at test time)
// whose four async submissions report real AsyncCallInfo completions through
// the native callback, and whose Log input carries deterministic fault and
// flood triggers. The supervision client observes completions, faults,
// diagnostics and floods across the whole chain — binding adaptation, worker
// session, wire and client — and every failure path asserts stable codes and
// a reaped process. Nothing touches a device: the fixture never connects,
// never screencaps a real screen and never starts a task.
package app

import (
	"bytes"
	"context"
	"encoding/json"
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
	asyncFixtureSrcRel = "testdata/asyncfixture/fixture.c"
	asyncFixtureSOName = "libasyncfixture.so"

	// asyncfxTriggerX mirrors fixture.c's trigger coordinate: a click at this
	// x completes the whole pending stack LIFO inside its own native call.
	asyncfxTriggerX int32 = 0x4658
	// asyncfxFailY marks the click whose completion reports details.ret=false.
	asyncfxFailY int32 = -1
	// asyncfxFirstID mirrors fixture.c's first async call ID.
	asyncfxFirstID int32 = 0x1F001
	// asyncfxUnknownID mirrors fixture.c's never-submitted completion ID.
	asyncfxUnknownID int32 = 0x7FFFFF00
	// asyncfxBig is the 2^53+1 integer every fixture completion carries; a
	// float64 round trip anywhere on the wire would corrupt it.
	asyncfxBig = "9007199254740993"
)

// asyncfxDetails is the exact details JSON fixture.c emits for ret=ok: the
// client must receive these bytes verbatim.
func asyncfxDetails(ok bool) []byte {
	if ok {
		return []byte(`{"ret":true,"cost":1,"big":` + asyncfxBig + `,"src":"asyncfx"}`)
	}
	return []byte(`{"ret":false,"cost":1,"big":` + asyncfxBig + `,"src":"asyncfx"}`)
}

// asyncfxBGR mirrors fixture.c's deterministic 16-byte BGR image:
// byte[i] = i*29 + 0xB5.
func asyncfxBGR() []byte {
	out := make([]byte, 16)
	for i := range out {
		out[i] = byte(i*29 + 0xB5)
	}
	return out
}

// buildAsyncFixture compiles the controlled native library and returns the
// .so absolute path. Missing cc/gcc is a test-environment limitation and is
// reported as a skip, never as a pass.
func buildAsyncFixture(t *testing.T) string {
	t.Helper()
	compiler := ""
	for _, name := range []string{"cc", "gcc"} {
		if path, err := exec.LookPath(name); err == nil {
			compiler = path
			break
		}
	}
	if compiler == "" {
		t.Skipf("async fixture blocked: no C compiler found; controlled native verification not executed, does not count as a pass")
	}
	src, err := filepath.Abs(asyncFixtureSrcRel)
	require.NoError(t, err, "resolve fixture source")
	outPath := filepath.Join(t.TempDir(), asyncFixtureSOName)
	args := []string{"-shared", "-fPIC", "-O2", "-o", outPath, src}
	t.Logf("async fixture build: compiler=%s platform=%s/%s command=%s", compiler, runtime.GOOS, runtime.GOARCH, compiler+" "+strings.Join(args, " "))
	output, err := exec.Command(compiler, args...).CombinedOutput()
	require.NoError(t, err, "failed to compile the async fixture:\n%s", output)
	return outPath
}

// launchAsyncFixtureWorker starts one real worker process against the
// controlled native library.
func launchAsyncFixtureWorker(t *testing.T) *worker.Client {
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

// initializeAsyncFixtureWorker initializes the fixture instance and pins the
// capability snapshot: every all-platform optional symbol is exported, so
// ImageBGR and both experiment capabilities must be reported; the windows-only
// attach symbol is exported too, but a non-Windows platform must still report
// it absent — a capability claim can never outrun the platform gate.
func initializeAsyncFixtureWorker(t *testing.T, c *worker.Client, library string) protocol.RuntimeInfo {
	t.Helper()
	root := t.TempDir()
	info, err := c.Initialize(context.Background(), protocol.InitializeRequest{SnapshotID: "async-fixture", LibraryPath: library, UserDir: root, ResourceRoots: []string{root}})
	require.NoError(t, err, "initialize; diagnostics: %s", c.Diagnostics())
	want := protocol.Capabilities{ImageBGR: true, AttachWindow: false, MapLevelKey: true, ItemName: true}
	require.Equal(t, want, info.Capabilities, "fixture capability snapshot = %+v, want %+v", info.Capabilities, want)
	require.NotZero(t, info.NullSize, "fixture runtime snapshot: %+v", info)
	return info
}

// asyncfxTriggerLog fires one fixture trigger through the runtime log command.
func asyncfxTriggerLog(t *testing.T, c *worker.Client, trigger string) {
	t.Helper()
	err := c.Log(context.Background(), protocol.LogRequest{Level: "asyncfx", Message: trigger})
	require.NoError(t, err, "trigger %s; diagnostics: %s", trigger, c.Diagnostics())
}

// drainEvents reads exactly want events from the client event stream, failing
// on timeout or a closed stream. It deliberately does not select on Done: a
// closed Done leaves buffered events readable, and a drained stream closes the
// channel itself, which the read below reports with the session's error.
func drainEvents(t *testing.T, c *worker.Client, want int) []protocol.Message {
	t.Helper()
	var got []protocol.Message
	deadline := time.After(10 * time.Second)
	for len(got) < want {
		select {
		case event, ok := <-c.Events():
			if !ok {
				require.FailNow(t, fmt.Sprintf("event stream closed after %d/%d events: %v", len(got), want, c.Err()))
			}
			got = append(got, event)
		case <-deadline:
			require.FailNow(t, fmt.Sprintf("timed out after %d/%d events: %v", len(got), want, c.Err()))
		}
	}
	return got
}

// requireFailure waits for the process to exit and requires a *Failure with
// the given code and a truthful unknown flag.
func requireFailure(t *testing.T, c *worker.Client, code string) *worker.Failure {
	t.Helper()
	select {
	case <-c.Done():
	case <-time.After(15 * time.Second):
		require.FailNow(t, "worker process was not reaped after the session failure")
	}
	err := c.Err()
	var failure *worker.Failure
	require.ErrorAs(t, err, &failure, "session failure = %v, want *Failure with code %s", err, code)
	require.Equal(t, code, failure.Code, "session failure = %v, want *Failure with code %s", err, code)
	require.NotNil(t, failure.Cause, "failure %v carries no cause", failure)
	return failure
}

// requireProtocolErrorIn unwrains a *protocol.Error with the wanted code from
// an error chain.
func requireProtocolErrorIn(t *testing.T, err error, code string) *protocol.Error {
	t.Helper()
	var rejected *protocol.Error
	require.ErrorAs(t, err, &rejected, "error = %v (%T), want *protocol.Error with code %s", err, err, code)
	require.Equal(t, code, rejected.Code, "error = %v (%T), want *protocol.Error with code %s", err, err, code)
	return rejected
}

// requireOperationEvents filters the operation events out of a drained event
// slice and requires their count.
func requireOperationEvents(t *testing.T, events []protocol.Message, want int) []protocol.OperationResult {
	t.Helper()
	var results []protocol.OperationResult
	for _, event := range events {
		if event.Name != protocol.OperationEvent {
			continue
		}
		var result protocol.OperationResult
		require.NoError(t, protocol.Decode(event.Payload, &result), "decode operation event")
		results = append(results, result)
	}
	require.Len(t, results, want, "operation events = %d, want %d", len(results), want)
	return results
}

// TestControlledNativeAsyncCompletionMatrix proves the four async submissions
// through one real process and real native completions: an early result is
// joined before the acceptance response is even queued; completions may settle
// in any order across methods; a native false arrives as native_rejected with
// a cached, byte-exact result; and no path ever invents a success.
func TestControlledNativeAsyncCompletionMatrix(t *testing.T) {
	library := buildAsyncFixture(t)

	t.Run("first submission completes before its acceptance response", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)
		ctx := context.Background()

		op, err := c.Click(ctx, protocol.ClickRequest{X: asyncfxTriggerX, Y: 1})
		require.NoError(t, err, "trigger click; diagnostics: %s", c.Diagnostics())
		require.Equal(t, asyncfxFirstID, op.ID(), "async id = %d, want %d", op.ID(), asyncfxFirstID)
		result, err := op.Wait(ctx)
		require.NoError(t, err, "early completion did not settle the operation")
		require.Equal(t, asyncfxFirstID, result.AsyncID, "early result: %+v", result)
		require.Equal(t, "Click", result.What, "early result: %+v", result)
		require.True(t, result.OK, "early result: %+v", result)
		require.True(t, bytes.Equal(result.Details, asyncfxDetails(true)), "details were not preserved byte for byte: %q", result.Details)

		// The completion also crossed the event stream as one Core callback
		// carrying the same big integer verbatim; the acceptance also produced
		// its own operation event.
		events := drainEvents(t, c, 2)
		var callback protocol.Callback
		for _, event := range events {
			if event.Name != protocol.CoreEvent {
				continue
			}
			require.NoError(t, protocol.Decode(event.Payload, &callback))
		}
		require.Equal(t, uint64(1), callback.CoreSequence, "completion callback: %+v", callback)
		require.Equal(t, int32(4), callback.MessageID, "completion callback: %+v", callback)
		require.True(t, bytes.Contains(callback.JSON, []byte(asyncfxBig)), "completion callback: %+v", callback)
		requireOperationEvents(t, events, 1)

		// A repeated wait returns an independent copy with the same terminal
		// state, and the session stays healthy.
		again, err := op.Wait(ctx)
		require.NoError(t, err, "repeat wait = %+v", again)
		require.True(t, bytes.Equal(again.Details, asyncfxDetails(true)), "repeat wait = %+v, %v", again, err)
		err = c.Ping(ctx)
		require.NoError(t, err, "ping after early completion")
		err = c.Close(ctx)
		require.NoError(t, err, "close; diagnostics: %s", c.Diagnostics())
	})

	t.Run("completions settle in reverse acceptance order across methods", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)
		ctx := context.Background()

		click, err := c.Click(ctx, protocol.ClickRequest{X: 1, Y: 2})
		require.NoError(t, err)
		connect, err := c.Connect(ctx, protocol.ConnectRequest{ADBPath: "/asyncfx/none", Address: "127.0.0.1:0", Config: "{}"})
		require.NoError(t, err)
		screencap, err := c.Screencap(ctx)
		require.NoError(t, err)
		trigger, err := c.Click(ctx, protocol.ClickRequest{X: asyncfxTriggerX, Y: 3})
		require.NoError(t, err)
		ids := map[*worker.Operation]string{click: "Click", connect: "Connect", screencap: "Screencap", trigger: "Click"}
		for op, what := range ids {
			result, err := op.Wait(ctx)
			require.NoError(t, err, "%s wait = %+v", what, result)
			require.True(t, result.OK, "%s wait = %+v, %v", what, result, err)
			require.Equal(t, what, result.What, "%s wait = %+v, %v", what, result, err)
			require.True(t, bytes.Equal(result.Details, asyncfxDetails(true)), "%s details diverged: %q", what, result.Details)
		}

		// The fixture fired its stack LIFO inside the trigger call, so the
		// Core completion callbacks crossed the single dispatcher stream in
		// reverse acceptance order, with contiguous Core sequences; the
		// operation events themselves carry no order guarantee, because each
		// wait goroutine emits independently.
		events := drainEvents(t, c, 8)
		requireOperationEvents(t, events, 4)
		wantOrder := []int32{trigger.ID(), screencap.ID(), connect.ID(), click.ID()}
		coreIDs := []int32{}
		coreSequence := uint64(0)
		for _, event := range events {
			if event.Name != protocol.CoreEvent {
				continue
			}
			var callback protocol.Callback
			require.NoError(t, protocol.Decode(event.Payload, &callback))
			require.Equal(t, coreSequence+1, callback.CoreSequence, "core callback %d: %+v", len(coreIDs)+1, callback)
			require.Equal(t, int32(4), callback.MessageID, "core callback %d: %+v", len(coreIDs)+1, callback)
			coreSequence = callback.CoreSequence
			var completion struct {
				AsyncID int32 `json:"async_call_id"`
			}
			// The completion JSON is native-side content: decode only the
			// association field and leave every other field untouched.
			require.NoError(t, json.Unmarshal(callback.JSON, &completion))
			coreIDs = append(coreIDs, completion.AsyncID)
		}
		require.Len(t, coreIDs, len(wantOrder), "core completions = %v, want %v", coreIDs, wantOrder)
		for i, want := range wantOrder {
			require.Equal(t, want, coreIDs[i], "completion order = %v, want reverse acceptance order %v", coreIDs, wantOrder)
		}

		err = c.Ping(ctx)
		require.NoError(t, err, "ping after out-of-order completions")
		err = c.Close(ctx)
		require.NoError(t, err, "close")
	})

	t.Run("native failure settles with native_rejected and stays cached", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)
		ctx := context.Background()

		failed, err := c.Click(ctx, protocol.ClickRequest{X: 7, Y: asyncfxFailY})
		require.NoError(t, err)
		trigger, err := c.Click(ctx, protocol.ClickRequest{X: asyncfxTriggerX, Y: 8})
		require.NoError(t, err)
		result, err := failed.Wait(ctx)
		rejected := requireProtocolErrorIn(t, err, protocol.NativeRejected)
		require.False(t, result.OK, "failed result: %+v", result)
		require.Equal(t, failed.ID(), result.AsyncID, "failed result: %+v", result)
		require.Equal(t, "Click", result.What, "failed result: %+v", result)
		require.True(t, bytes.Equal(result.Details, asyncfxDetails(false)), "failed details diverged: %q", result.Details)
		// The failure is a stable code, not diagnostic text: the code alone
		// discriminates it, and the cached result survives repeated waits.
		rejected2 := requireProtocolErrorIn(t, func() error { _, err := failed.Wait(ctx); return err }(), protocol.NativeRejected)
		require.Equal(t, rejected.Message, rejected2.Message, "repeated wait rewrote the failure: %q vs %q", rejected2.Message, rejected.Message)
		result, err = trigger.Wait(ctx)
		require.NoError(t, err, "trigger wait = %+v", result)
		require.True(t, result.OK, "trigger wait = %+v, %v", result, err)
		err = c.Close(ctx)
		require.NoError(t, err, "close")
	})
}

// TestControlledNativeFaultMatrix proves the cross-layer fault disciplines on
// a real process: a completion nobody submitted faults the instance without
// breaking the session; a duplicate of an already-settled completion faults
// without rewriting the confirmed result; invalid native callback bytes are
// rejected loudly at the client boundary with the process reaped; and every
// close still delivers the terminal diagnostics snapshot before the close
// response, with no automatic reconnect or replay anywhere.
func TestControlledNativeFaultMatrix(t *testing.T) {
	library := buildAsyncFixture(t)

	t.Run("unassociatable completion faults the instance without breaking the session", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)
		ctx := context.Background()

		asyncfxTriggerLog(t, c, "asyncfx-fire-unknown")
		select {
		case <-c.Fault():
		case <-time.After(10 * time.Second):
			require.FailNow(t, "fault notification never closed")
		}
		instanceErr := c.InstanceErr()
		require.NotNil(t, instanceErr, "instance error = %v, want protocol_error", instanceErr)
		require.Equal(t, protocol.ProtocolError, instanceErr.Code, "instance error = %v, want protocol_error", instanceErr)
		// A native instance fault is not a process failure: the worker keeps
		// serving, the observation is independent of the event reader, and
		// nothing was reconnected or replayed — the session identity is
		// unchanged and no new submission exists.
		err := c.Ping(ctx)
		require.NoError(t, err, "session unusable after the instance fault")

		// A faulted session cannot acknowledge a clean close: the worker
		// answers the close with the classified fault and exits 1, and the
		// client reports a terminal failure whose result stays unknown. Which
		// terminal code wins (disconnected versus shutdown_failed) is a read
		// race at the process boundary; the stable code discrimination of the
		// close response itself is proven at the raw wire level in
		// TestWorkerRawProtocolMatrix.
		closeErr := c.Close(ctx)
		failure := &worker.Failure{}
		require.ErrorAs(t, closeErr, &failure, "close after fault = %v, want a *Failure with unknown=true; diagnostics: %s", closeErr, c.Diagnostics())
		require.True(t, failure.Unknown, "close after fault = %v, want a *Failure with unknown=true; diagnostics: %s", closeErr, c.Diagnostics())
		// The stream carried exactly one fault report, one completion callback
		// and one terminal snapshot, with the snapshot last. The fault report
		// and the completion callback are emitted from independent watchers,
		// so their relative stream position is not frozen; the set and the
		// final position are. All three were buffered before the close
		// response, so they are read before the reaping assertions below.
		events := drainEvents(t, c, 3)
		names := []string{events[0].Name, events[1].Name, events[2].Name}
		require.True(t, names[2] == protocol.DiagnosticsEvent &&
			((names[0] == protocol.FaultEvent && names[1] == protocol.CoreEvent) ||
				(names[0] == protocol.CoreEvent && names[1] == protocol.FaultEvent)), "event order = %v, want one fault, one core completion and the final diagnostics", names)
		var callback protocol.Callback
		coreIndex := 0
		if events[1].Name == protocol.CoreEvent {
			coreIndex = 1
		}
		err = protocol.Decode(events[coreIndex].Payload, &callback)
		require.NoError(t, err, "completion callback: %+v", callback)
		require.Equal(t, uint64(1), callback.CoreSequence, "completion callback: %+v", callback)
		require.Equal(t, int32(4), callback.MessageID, "completion callback: %+v", callback)
		require.True(t, strings.Contains(string(callback.JSON), fmt.Sprint(asyncfxUnknownID)), "callback does not name the unassociatable id: %s", callback.JSON)
		requireOperationEvents(t, events, 0)
		select {
		case <-c.InstanceDone():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "instance terminal state never arrived")
		}
		snap, ok := c.InstanceDiagnostics()
		require.True(t, ok, "terminal diagnostics unavailable after a faulted close")
		require.NotNil(t, snap.FirstFault, "diagnostics first fault = %+v, want protocol_error", snap.FirstFault)
		require.Equal(t, protocol.ProtocolError, snap.FirstFault.Code, "diagnostics first fault = %+v, want protocol_error", snap.FirstFault)
		require.Zero(t, snap.LostEvents, "diagnostics snapshot: %+v", snap)
		require.Nil(t, snap.RawPrefix, "diagnostics snapshot: %+v", snap)
		require.Nil(t, snap.StopDiagnostic, "diagnostics snapshot: %+v", snap)
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			require.FailNow(t, "process was not reaped after the faulted close")
		}
	})

	t.Run("a completed result is never rewritten by a later duplicate", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)
		ctx := context.Background()

		op, err := c.Click(ctx, protocol.ClickRequest{X: asyncfxTriggerX, Y: 1})
		require.NoError(t, err)
		first, err := op.Wait(ctx)
		require.NoError(t, err, "first wait = %+v", first)
		require.True(t, first.OK, "first wait = %+v, %v", first, err)

		asyncfxTriggerLog(t, c, "asyncfx-duplicate")
		select {
		case <-c.Fault():
		case <-time.After(10 * time.Second):
			require.FailNow(t, "duplicate completion did not fault the instance")
		}
		instanceErr := c.InstanceErr()
		require.NotNil(t, instanceErr, "instance error = %v, want protocol_error", instanceErr)
		require.Equal(t, protocol.ProtocolError, instanceErr.Code, "instance error = %v, want protocol_error", instanceErr)
		// The confirmed result survives the later fault byte for byte.
		again, err := op.Wait(ctx)
		require.NoError(t, err, "confirmed result was rewritten: %+v", again)
		require.True(t, again.OK, "confirmed result was rewritten: %+v, %v", again, err)
		require.True(t, bytes.Equal(again.Details, asyncfxDetails(true)), "confirmed result was rewritten: %+v, %v", again, err)
		err = c.Ping(ctx)
		require.NoError(t, err, "session unusable after the duplicate fault")

		// A faulted session cannot acknowledge a clean close: the worker
		// answers the close with the classified fault and exits 1, and the
		// client reports a terminal failure whose result stays unknown. Which
		// terminal code wins (disconnected versus shutdown_failed) is a read
		// race at the process boundary; the stable code discrimination of the
		// close response itself is proven at the raw wire level in
		// TestWorkerRawProtocolMatrix.
		closeErr := c.Close(ctx)
		failure := &worker.Failure{}
		require.ErrorAs(t, closeErr, &failure, "close after fault = %v, want a *Failure with unknown=true; diagnostics: %s", closeErr, c.Diagnostics())
		require.True(t, failure.Unknown, "close after fault = %v, want a *Failure with unknown=true; diagnostics: %s", closeErr, c.Diagnostics())
		snap, ok := c.InstanceDiagnostics()
		require.True(t, ok, "diagnostics after duplicate fault: ok=%v snap=%+v", ok, snap)
		require.NotNil(t, snap.FirstFault, "diagnostics after duplicate fault: ok=%v snap=%+v", ok, snap)
		require.Equal(t, protocol.ProtocolError, snap.FirstFault.Code, "diagnostics after duplicate fault: ok=%v snap=%+v", ok, snap)
		require.Zero(t, snap.LostEvents, "delivered duplicate must not count as lost: %+v", snap)
	})

	t.Run("invalid native callback bytes fail loudly at the worker boundary", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)

		// The callback is produced inside the native Log call, but Observe
		// encodes it independently. The resulting session failure can win the
		// race against the Log response; both an acknowledgment and an unknown
		// process/transport failure are valid trigger outcomes.
		if err := c.Log(context.Background(), protocol.LogRequest{Level: "asyncfx", Message: "asyncfx-badjson"}); err != nil {
			failure := &worker.Failure{}
			require.ErrorAs(t, err, &failure, "malformed callback trigger = %v, want acknowledgment or a *Failure with unknown=true; diagnostics: %s", err, c.Diagnostics())
			require.True(t, failure.Unknown, "malformed callback trigger = %v, want acknowledgment or a *Failure with unknown=true; diagnostics: %s", err, c.Diagnostics())
		}
		// Invalid native callback bytes are not valid JSON and can never be
		// encoded onto the wire: the worker fails its own session instead of
		// delivering lossy output, the process exits and the client reports a
		// terminal failure with unknown results — never a fabricated success.
		// The binding's fault observation raced the delivery failure and may
		// or may not have crossed before the exit; when it did, it carries the
		// stable protocol_error code.
		select {
		case <-c.Done():
		case <-time.After(15 * time.Second):
			require.FailNow(t, "worker process was not reaped after the undeliverable callback")
		}
		failure := &worker.Failure{}
		require.ErrorAs(t, c.Err(), &failure, "session failure = %v, want a *Failure with unknown=true", c.Err())
		require.True(t, failure.Unknown, "session failure = %v, want a *Failure with unknown=true", c.Err())
		if instanceErr := c.InstanceErr(); instanceErr != nil {
			require.Equal(t, protocol.ProtocolError, instanceErr.Code, "instance error = %v, want protocol_error", instanceErr)
		}
	})
}

// TestControlledNativeEventFloodIsBounded proves the bounded event budgets end
// to end: a drained flood is delivered verbatim with contiguous sequences, and
// an undrained flood aborts the client with the stable event_overflow code and
// a reaped process — never unbounded buffering and never silent loss.
func TestControlledNativeEventFloodIsBounded(t *testing.T) {
	library := buildAsyncFixture(t)

	t.Run("a drained flood delivers every event verbatim", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)
		ctx := context.Background()

		asyncfxTriggerLog(t, c, "asyncfx-flood-40")
		events := drainEvents(t, c, 40)
		for i, event := range events {
			require.Equal(t, protocol.CoreEvent, event.Name, "event %d: name=%s sequence=%d", i, event.Name, event.Sequence)
			require.Equal(t, uint64(i+1), event.Sequence, "event %d: name=%s sequence=%d", i, event.Name, event.Sequence)
			var callback protocol.Callback
			require.NoError(t, protocol.Decode(event.Payload, &callback))
			require.Equal(t, int32(777), callback.MessageID, "flood event %d: %+v", i, callback)
			require.Equal(t, uint64(i+1), callback.CoreSequence, "flood event %d: %+v", i, callback)
			require.True(t, bytes.Contains(callback.JSON, fmt.Appendf(nil, `"n":%d`, i+1)), "flood event %d lost its payload: %s", i, callback.JSON)
			require.True(t, bytes.Contains(callback.JSON, []byte(asyncfxBig)), "flood event %d lost its payload: %s", i, callback.JSON)
		}
		err := c.Ping(ctx)
		require.NoError(t, err, "ping after the flood")
		err = c.Close(ctx)
		require.NoError(t, err, "close")
	})

	t.Run("an undrained flood aborts the session with event_overflow", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)

		// Three small batches let the worker's bounded outbound queue drain
		// between batches, so the failure is deterministic at the client's own
		// bounded budget instead of a scheduling race inside the worker.
		for range 3 {
			err := c.Log(context.Background(), protocol.LogRequest{Level: "asyncfx", Message: "asyncfx-flood-25"})
			if err != nil {
				// The batch whose events cross the client's 64-slot budget
				// always has its own trigger still awaiting its response, and
				// the worker's dispatcher and responder enqueue independently,
				// so either call may observe the abort first. Whatever the
				// interleaving, the trigger may only fail with the stable
				// flood abort; requireFailure below owns the terminal claims.
				var failure *worker.Failure
				require.ErrorAs(t, err, &failure, "trigger failed without a session failure: %v", err)
				require.Equal(t, protocol.EventOverflow, failure.Code, "trigger failure = %v, want the flood abort", err)
				require.True(t, failure.Unknown, "flood failure must report unknown results: %+v", failure)
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		failure := requireFailure(t, c, protocol.EventOverflow)
		require.True(t, failure.Unknown, "flood failure must report unknown results: %+v", failure)
	})
}

// TestControlledNativePlatformCapabilityGates proves the platform gate end to
// end: the fixture exports AsstAsyncAttachWindow, but a non-Windows platform
// reports the capability absent and the submission answers the stable
// unsupported rejection without any native call or ADB fallback.
func TestControlledNativePlatformCapabilityGates(t *testing.T) {
	library := buildAsyncFixture(t)
	c := launchAsyncFixtureWorker(t)
	initializeAsyncFixtureWorker(t, c, library)
	ctx := context.Background()

	op, err := c.AttachWindow(ctx, protocol.AttachWindowRequest{Hwnd: 0x1234, ScreencapMethod: 1, MouseMethod: 2, KeyboardMethod: 4})
	require.Error(t, err, "attach_window answered without the platform capability")
	requireProtocolErrorIn(t, err, protocol.Unsupported)
	require.Nil(t, op, "rejected submission returned an operation: %+v", op)
	err = c.Ping(ctx)
	require.NoError(t, err, "session unusable after the unsupported rejection")
	err = c.Close(ctx)
	require.NoError(t, err, "close")
}

// TestControlledNativeMixedMethodsShareAssociationAndClose proves that
// different methods share one session's association, caches and close: reads,
// queries, tasks, log and image transfers coexist with pending async
// submissions, and a clean close settles every pending operation with the
// stable invalid_state code instead of inventing results or leaking the
// process.
func TestControlledNativeMixedMethodsShareAssociationAndClose(t *testing.T) {
	library := buildAsyncFixture(t)
	c := launchAsyncFixtureWorker(t)
	initializeAsyncFixtureWorker(t, c, library)
	ctx := context.Background()

	screencap, err := c.Screencap(ctx)
	require.NoError(t, err)
	click, err := c.Click(ctx, protocol.ClickRequest{X: 3, Y: 4})
	require.NoError(t, err)

	res, err := c.UUID(ctx)
	require.NoError(t, err, "uuid = %+v", res)
	require.Equal(t, "asyncfx-uuid", res.UUID, "uuid = %+v, %v", res, err)
	status, err := c.Status(ctx)
	require.NoError(t, err, "status = %+v", status)
	require.False(t, status.Running, "status = %+v, %v", status, err)
	require.False(t, status.Connected, "status = %+v, %v", status, err)
	require.Empty(t, status.Tasks, "status = %+v, %v", status, err)
	accepted, err := c.AppendTask(ctx, protocol.TaskRequest{Type: "Click", Params: jsonRaw(`{"x":9007199254740993}`)})
	require.NoError(t, err, "append task = %+v", accepted)
	require.Equal(t, int32(7), accepted.TaskID, "append task = %+v, %v", accepted, err)
	err = c.SetTaskParams(ctx, protocol.TaskParamsRequest{TaskID: 7, Params: jsonRaw(`{"enable":true}`)})
	require.NoError(t, err)
	err = c.Start(ctx)
	require.NoError(t, err)
	err = c.BackToHome(ctx)
	require.NoError(t, err)
	err = c.Log(ctx, protocol.LogRequest{Level: "asyncfx", Message: "combination"})
	require.NoError(t, err)
	hit, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "asyncfx-hit"})
	require.NoError(t, err, "map level key = %+v", hit)
	require.Equal(t, "asyncfx-stage#1", hit.StageID, "map level key = %+v, %v", hit, err)
	require.Equal(t, "asyncfx-name#1", hit.Name, "map level key = %+v, %v", hit, err)
	name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "asyncfx-item"})
	require.NoError(t, err, "item name = %+v", name)
	require.Equal(t, "asyncfx-item#1", name.Name, "item name = %+v, %v", name, err)
	png, err := c.GetImagePNG(ctx)
	require.NoError(t, err, "empty png = %d bytes, %v", len(png), err)
	require.NotNil(t, png, "empty png = %d bytes, %v", len(png), err)
	require.Empty(t, png, "empty png = %d bytes, %v", len(png), err)
	bgr, err := c.GetImageBGR(ctx)
	require.NoError(t, err, "bgr snapshot diverged")
	require.True(t, bytes.Equal(bgr, asyncfxBGR()), "bgr snapshot diverged: %v", err)

	// The clean close settles both pending operations with the stable
	// invalid_state code; the close response follows both operation events and
	// the terminal snapshot. The two operation events race the snapshot's
	// stream position (independent wait goroutines), so the drained set is
	// asserted, not the order.
	err = c.Close(ctx)
	require.NoError(t, err, "close; diagnostics: %s", c.Diagnostics())
	results := requireOperationEvents(t, drainEvents(t, c, 3), 2)
	for _, result := range results {
		require.False(t, result.OK, "close settlement = %+v, want ok=false with invalid_state", result)
		require.NotNil(t, result.Error, "close settlement = %+v, want ok=false with invalid_state", result)
		require.Equal(t, protocol.InvalidState, result.Error.Code, "close settlement = %+v, want ok=false with invalid_state", result)
	}
	for _, op := range []*worker.Operation{screencap, click} {
		_, err := op.Wait(ctx)
		requireProtocolErrorIn(t, err, protocol.InvalidState)
	}
	snap, ok := c.InstanceDiagnostics()
	require.True(t, ok, "clean close diagnostics: ok=%v snap=%+v", ok, snap)
	require.Nil(t, snap.FirstFault, "clean close diagnostics: ok=%v snap=%+v", ok, snap)
	require.Nil(t, snap.StopDiagnostic, "clean close diagnostics: ok=%v snap=%+v", ok, snap)
	require.Zero(t, snap.LostEvents, "clean close diagnostics: ok=%v snap=%+v", ok, snap)
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		require.FailNow(t, "process was not reaped after the clean close")
	}
}

// jsonRaw is a small helper for raw JSON payloads in this file.
func jsonRaw(s string) json.RawMessage { return json.RawMessage(s) }

// TestControlledNativeParentInterruptionIsBounded proves the supervision-side
// interruption discipline against the real worker: an expired close deadline
// kills the worker, reaps the process within the shutdown bound, and every
// unconfirmed operation stays unknown — never a fabricated result and never
// an automatic retry.
func TestControlledNativeParentInterruptionIsBounded(t *testing.T) {
	library := buildAsyncFixture(t)
	c := launchAsyncFixtureWorker(t)
	initializeAsyncFixtureWorker(t, c, library)
	ctx := context.Background()

	op, err := c.Screencap(ctx)
	require.NoError(t, err)

	expired, cancel := context.WithCancel(ctx)
	cancel()
	closeErr := c.Close(expired)
	var failure *worker.Failure
	require.ErrorAs(t, closeErr, &failure, "interrupted close = %v, want a *Failure with unknown=true", closeErr)
	require.True(t, failure.Unknown, "interrupted close = %v, want a *Failure with unknown=true", closeErr)
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		require.FailNow(t, "killed worker was not reaped within the bound")
	}
	_, err = op.Wait(ctx)
	var interrupted *worker.Failure
	require.ErrorAs(t, err, &interrupted, "unconfirmed operation after interruption = %v, want unknown", err)
	require.True(t, interrupted.Unknown, "unconfirmed operation after interruption = %v, want unknown", err)
}
