//go:build linux

// This file is the cross-layer acceptance matrix of the full worker: the real
// core-worker binary loads a controlled native library (compiled at test time)
// whose four async submissions report real AsyncCallInfo completions through
// the native callback, and whose Log input carries deterministic fault and
// flood triggers. The supervision client observes completions, faults,
// diagnostics and floods across the whole chain — binding adaptation, worker
// session, wire and client — and every failure path asserts stable codes and
// a reaped process. Nothing touches a device: the fixture never connects,
// never screencaps a real screen and never starts a task.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	if err != nil {
		t.Fatalf("resolve fixture source: %v", err)
	}
	outPath := filepath.Join(t.TempDir(), asyncFixtureSOName)
	args := []string{"-shared", "-fPIC", "-O2", "-o", outPath, src}
	t.Logf("async fixture build: compiler=%s platform=%s/%s command=%s", compiler, runtime.GOOS, runtime.GOARCH, compiler+" "+strings.Join(args, " "))
	output, err := exec.Command(compiler, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("failed to compile the async fixture: %v\n%s", err, output)
	}
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
	if err != nil {
		t.Fatal(err)
	}
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
	if err != nil {
		t.Fatalf("initialize: %v; diagnostics: %s", err, c.Diagnostics())
	}
	want := protocol.Capabilities{ImageBGR: true, AttachWindow: false, MapLevelKey: true, ItemName: true}
	if info.Capabilities != want {
		t.Fatalf("fixture capability snapshot = %+v, want %+v", info.Capabilities, want)
	}
	if info.NullSize == 0 {
		t.Fatalf("fixture runtime snapshot: %+v", info)
	}
	return info
}

// asyncfxTriggerLog fires one fixture trigger through the runtime log command.
func asyncfxTriggerLog(t *testing.T, c *worker.Client, trigger string) {
	t.Helper()
	if err := c.Log(context.Background(), protocol.LogRequest{Level: "asyncfx", Message: trigger}); err != nil {
		t.Fatalf("trigger %s: %v; diagnostics: %s", trigger, err, c.Diagnostics())
	}
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
				t.Fatalf("event stream closed after %d/%d events: %v", len(got), want, c.Err())
			}
			got = append(got, event)
		case <-deadline:
			t.Fatalf("timed out after %d/%d events: %v", len(got), want, c.Err())
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
		t.Fatal("worker process was not reaped after the session failure")
	}
	err := c.Err()
	var failure *worker.Failure
	if !errors.As(err, &failure) || failure.Code != code {
		t.Fatalf("session failure = %v, want *Failure with code %s", err, code)
	}
	if failure.Cause == nil {
		t.Fatalf("failure %v carries no cause", failure)
	}
	return failure
}

// requireProtocolErrorIn unwrains a *protocol.Error with the wanted code from
// an error chain.
func requireProtocolErrorIn(t *testing.T, err error, code string) *protocol.Error {
	t.Helper()
	var rejected *protocol.Error
	if !errors.As(err, &rejected) || rejected.Code != code {
		t.Fatalf("error = %v (%T), want *protocol.Error with code %s", err, err, code)
	}
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
		if err := protocol.Decode(event.Payload, &result); err != nil {
			t.Fatalf("decode operation event: %v", err)
		}
		results = append(results, result)
	}
	if len(results) != want {
		t.Fatalf("operation events = %d, want %d", len(results), want)
	}
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
		if err != nil {
			t.Fatalf("trigger click: %v; diagnostics: %s", err, c.Diagnostics())
		}
		if op.ID() != asyncfxFirstID {
			t.Fatalf("async id = %d, want %d", op.ID(), asyncfxFirstID)
		}
		result, err := op.Wait(ctx)
		if err != nil {
			t.Fatalf("early completion did not settle the operation: %v", err)
		}
		if result.AsyncID != asyncfxFirstID || result.What != "Click" || !result.OK {
			t.Fatalf("early result: %+v", result)
		}
		if !bytes.Equal(result.Details, asyncfxDetails(true)) {
			t.Fatalf("details were not preserved byte for byte: %q", result.Details)
		}

		// The completion also crossed the event stream as one Core callback
		// carrying the same big integer verbatim; the acceptance also produced
		// its own operation event.
		events := drainEvents(t, c, 2)
		var callback protocol.Callback
		for _, event := range events {
			if event.Name != protocol.CoreEvent {
				continue
			}
			if err := protocol.Decode(event.Payload, &callback); err != nil {
				t.Fatal(err)
			}
		}
		if callback.CoreSequence != 1 || callback.MessageID != 4 || !bytes.Contains(callback.JSON, []byte(asyncfxBig)) {
			t.Fatalf("completion callback: %+v", callback)
		}
		requireOperationEvents(t, events, 1)

		// A repeated wait returns an independent copy with the same terminal
		// state, and the session stays healthy.
		again, err := op.Wait(ctx)
		if err != nil || !bytes.Equal(again.Details, asyncfxDetails(true)) {
			t.Fatalf("repeat wait = %+v, %v", again, err)
		}
		if err := c.Ping(ctx); err != nil {
			t.Fatalf("ping after early completion: %v", err)
		}
		if err := c.Close(ctx); err != nil {
			t.Fatalf("close: %v; diagnostics: %s", err, c.Diagnostics())
		}
	})

	t.Run("completions settle in reverse acceptance order across methods", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)
		ctx := context.Background()

		click, err := c.Click(ctx, protocol.ClickRequest{X: 1, Y: 2})
		if err != nil {
			t.Fatal(err)
		}
		connect, err := c.Connect(ctx, protocol.ConnectRequest{ADBPath: "/asyncfx/none", Address: "127.0.0.1:0", Config: "{}"})
		if err != nil {
			t.Fatal(err)
		}
		screencap, err := c.Screencap(ctx)
		if err != nil {
			t.Fatal(err)
		}
		trigger, err := c.Click(ctx, protocol.ClickRequest{X: asyncfxTriggerX, Y: 3})
		if err != nil {
			t.Fatal(err)
		}
		ids := map[*worker.Operation]string{click: "Click", connect: "Connect", screencap: "Screencap", trigger: "Click"}
		for op, what := range ids {
			result, err := op.Wait(ctx)
			if err != nil || !result.OK || result.What != what {
				t.Fatalf("%s wait = %+v, %v", what, result, err)
			}
			if !bytes.Equal(result.Details, asyncfxDetails(true)) {
				t.Fatalf("%s details diverged: %q", what, result.Details)
			}
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
			if err := protocol.Decode(event.Payload, &callback); err != nil {
				t.Fatal(err)
			}
			if callback.CoreSequence != coreSequence+1 || callback.MessageID != 4 {
				t.Fatalf("core callback %d: %+v", len(coreIDs)+1, callback)
			}
			coreSequence = callback.CoreSequence
			var completion struct {
				AsyncID int32 `json:"async_call_id"`
			}
			// The completion JSON is native-side content: decode only the
			// association field and leave every other field untouched.
			if err := json.Unmarshal(callback.JSON, &completion); err != nil {
				t.Fatal(err)
			}
			coreIDs = append(coreIDs, completion.AsyncID)
		}
		if len(coreIDs) != len(wantOrder) {
			t.Fatalf("core completions = %v, want %v", coreIDs, wantOrder)
		}
		for i, want := range wantOrder {
			if coreIDs[i] != want {
				t.Fatalf("completion order = %v, want reverse acceptance order %v", coreIDs, wantOrder)
			}
		}

		if err := c.Ping(ctx); err != nil {
			t.Fatalf("ping after out-of-order completions: %v", err)
		}
		if err := c.Close(ctx); err != nil {
			t.Fatalf("close: %v", err)
		}
	})

	t.Run("native failure settles with native_rejected and stays cached", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)
		ctx := context.Background()

		failed, err := c.Click(ctx, protocol.ClickRequest{X: 7, Y: asyncfxFailY})
		if err != nil {
			t.Fatal(err)
		}
		trigger, err := c.Click(ctx, protocol.ClickRequest{X: asyncfxTriggerX, Y: 8})
		if err != nil {
			t.Fatal(err)
		}
		result, err := failed.Wait(ctx)
		rejected := requireProtocolErrorIn(t, err, protocol.NativeRejected)
		if result.OK || result.AsyncID != failed.ID() || result.What != "Click" {
			t.Fatalf("failed result: %+v", result)
		}
		if !bytes.Equal(result.Details, asyncfxDetails(false)) {
			t.Fatalf("failed details diverged: %q", result.Details)
		}
		// The failure is a stable code, not diagnostic text: the code alone
		// discriminates it, and the cached result survives repeated waits.
		rejected2 := requireProtocolErrorIn(t, func() error { _, err := failed.Wait(ctx); return err }(), protocol.NativeRejected)
		if rejected2.Message != rejected.Message {
			t.Fatalf("repeated wait rewrote the failure: %q vs %q", rejected2.Message, rejected.Message)
		}
		if result, err := trigger.Wait(ctx); err != nil || !result.OK {
			t.Fatalf("trigger wait = %+v, %v", result, err)
		}
		if err := c.Close(ctx); err != nil {
			t.Fatalf("close: %v", err)
		}
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
			t.Fatal("fault notification never closed")
		}
		instanceErr := c.InstanceErr()
		if instanceErr == nil || instanceErr.Code != protocol.ProtocolError {
			t.Fatalf("instance error = %v, want protocol_error", instanceErr)
		}
		// A native instance fault is not a process failure: the worker keeps
		// serving, the observation is independent of the event reader, and
		// nothing was reconnected or replayed — the session identity is
		// unchanged and no new submission exists.
		if err := c.Ping(ctx); err != nil {
			t.Fatalf("session unusable after the instance fault: %v", err)
		}

		// A faulted session cannot acknowledge a clean close: the worker
		// answers the close with the classified fault and exits 1, and the
		// client reports a terminal failure whose result stays unknown. Which
		// terminal code wins (disconnected versus shutdown_failed) is a read
		// race at the process boundary; the stable code discrimination of the
		// close response itself is proven at the raw wire level in
		// TestWorkerRawProtocolMatrix.
		closeErr := c.Close(ctx)
		failure := &worker.Failure{}
		if !errors.As(closeErr, &failure) || !failure.Unknown {
			t.Fatalf("close after fault = %v, want a *Failure with unknown=true; diagnostics: %s", closeErr, c.Diagnostics())
		}
		// The stream carried exactly one fault report, one completion callback
		// and one terminal snapshot, with the snapshot last. The fault report
		// and the completion callback are emitted from independent watchers,
		// so their relative stream position is not frozen; the set and the
		// final position are. All three were buffered before the close
		// response, so they are read before the reaping assertions below.
		events := drainEvents(t, c, 3)
		names := []string{events[0].Name, events[1].Name, events[2].Name}
		if names[2] != protocol.DiagnosticsEvent ||
			!((names[0] == protocol.FaultEvent && names[1] == protocol.CoreEvent) ||
				(names[0] == protocol.CoreEvent && names[1] == protocol.FaultEvent)) {
			t.Fatalf("event order = %v, want one fault, one core completion and the final diagnostics", names)
		}
		var callback protocol.Callback
		coreIndex := 0
		if events[1].Name == protocol.CoreEvent {
			coreIndex = 1
		}
		if err := protocol.Decode(events[coreIndex].Payload, &callback); err != nil || callback.CoreSequence != 1 || callback.MessageID != 4 {
			t.Fatalf("completion callback: %+v, %v", callback, err)
		}
		if !strings.Contains(string(callback.JSON), fmt.Sprint(asyncfxUnknownID)) {
			t.Fatalf("callback does not name the unassociatable id: %s", callback.JSON)
		}
		requireOperationEvents(t, events, 0)
		select {
		case <-c.InstanceDone():
		case <-time.After(5 * time.Second):
			t.Fatal("instance terminal state never arrived")
		}
		snap, ok := c.InstanceDiagnostics()
		if !ok {
			t.Fatal("terminal diagnostics unavailable after a faulted close")
		}
		if snap.FirstFault == nil || snap.FirstFault.Code != protocol.ProtocolError {
			t.Fatalf("diagnostics first fault = %+v, want protocol_error", snap.FirstFault)
		}
		if snap.LostEvents != 0 || snap.RawPrefix != nil || snap.StopDiagnostic != nil {
			t.Fatalf("diagnostics snapshot: %+v", snap)
		}
		select {
		case <-c.Done():
		case <-time.After(5 * time.Second):
			t.Fatal("process was not reaped after the faulted close")
		}
	})

	t.Run("a completed result is never rewritten by a later duplicate", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)
		ctx := context.Background()

		op, err := c.Click(ctx, protocol.ClickRequest{X: asyncfxTriggerX, Y: 1})
		if err != nil {
			t.Fatal(err)
		}
		first, err := op.Wait(ctx)
		if err != nil || !first.OK {
			t.Fatalf("first wait = %+v, %v", first, err)
		}

		asyncfxTriggerLog(t, c, "asyncfx-duplicate")
		select {
		case <-c.Fault():
		case <-time.After(10 * time.Second):
			t.Fatal("duplicate completion did not fault the instance")
		}
		if instanceErr := c.InstanceErr(); instanceErr == nil || instanceErr.Code != protocol.ProtocolError {
			t.Fatalf("instance error = %v, want protocol_error", instanceErr)
		}
		// The confirmed result survives the later fault byte for byte.
		again, err := op.Wait(ctx)
		if err != nil || !again.OK || !bytes.Equal(again.Details, asyncfxDetails(true)) {
			t.Fatalf("confirmed result was rewritten: %+v, %v", again, err)
		}
		if err := c.Ping(ctx); err != nil {
			t.Fatalf("session unusable after the duplicate fault: %v", err)
		}

		// A faulted session cannot acknowledge a clean close: the worker
		// answers the close with the classified fault and exits 1, and the
		// client reports a terminal failure whose result stays unknown. Which
		// terminal code wins (disconnected versus shutdown_failed) is a read
		// race at the process boundary; the stable code discrimination of the
		// close response itself is proven at the raw wire level in
		// TestWorkerRawProtocolMatrix.
		closeErr := c.Close(ctx)
		failure := &worker.Failure{}
		if !errors.As(closeErr, &failure) || !failure.Unknown {
			t.Fatalf("close after fault = %v, want a *Failure with unknown=true; diagnostics: %s", closeErr, c.Diagnostics())
		}
		snap, ok := c.InstanceDiagnostics()
		if !ok || snap.FirstFault == nil || snap.FirstFault.Code != protocol.ProtocolError {
			t.Fatalf("diagnostics after duplicate fault: ok=%v snap=%+v", ok, snap)
		}
		if snap.LostEvents != 0 {
			t.Fatalf("delivered duplicate must not count as lost: %+v", snap)
		}
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
			if !errors.As(err, &failure) || !failure.Unknown {
				t.Fatalf("malformed callback trigger = %v, want acknowledgment or a *Failure with unknown=true; diagnostics: %s", err, c.Diagnostics())
			}
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
			t.Fatal("worker process was not reaped after the undeliverable callback")
		}
		failure := &worker.Failure{}
		if !errors.As(c.Err(), &failure) || !failure.Unknown {
			t.Fatalf("session failure = %v, want a *Failure with unknown=true", c.Err())
		}
		if instanceErr := c.InstanceErr(); instanceErr != nil && instanceErr.Code != protocol.ProtocolError {
			t.Fatalf("instance error = %v, want protocol_error", instanceErr)
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
			if event.Name != protocol.CoreEvent || event.Sequence != uint64(i+1) {
				t.Fatalf("event %d: name=%s sequence=%d", i, event.Name, event.Sequence)
			}
			var callback protocol.Callback
			if err := protocol.Decode(event.Payload, &callback); err != nil {
				t.Fatal(err)
			}
			if callback.MessageID != 777 || callback.CoreSequence != uint64(i+1) {
				t.Fatalf("flood event %d: %+v", i, callback)
			}
			if !bytes.Contains(callback.JSON, []byte(fmt.Sprintf(`"n":%d`, i+1))) || !bytes.Contains(callback.JSON, []byte(asyncfxBig)) {
				t.Fatalf("flood event %d lost its payload: %s", i, callback.JSON)
			}
		}
		if err := c.Ping(ctx); err != nil {
			t.Fatalf("ping after the flood: %v", err)
		}
		if err := c.Close(ctx); err != nil {
			t.Fatalf("close: %v", err)
		}
	})

	t.Run("an undrained flood aborts the session with event_overflow", func(t *testing.T) {
		c := launchAsyncFixtureWorker(t)
		initializeAsyncFixtureWorker(t, c, library)

		// Three small batches let the worker's bounded outbound queue drain
		// between batches, so the failure is deterministic at the client's own
		// bounded budget instead of a scheduling race inside the worker.
		for range 3 {
			asyncfxTriggerLog(t, c, "asyncfx-flood-25")
			time.Sleep(100 * time.Millisecond)
		}
		failure := requireFailure(t, c, protocol.EventOverflow)
		if !failure.Unknown {
			t.Fatalf("flood failure must report unknown results: %+v", failure)
		}
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
	if err == nil {
		t.Fatal("attach_window answered without the platform capability")
	}
	requireProtocolErrorIn(t, err, protocol.Unsupported)
	if op != nil {
		t.Fatalf("rejected submission returned an operation: %+v", op)
	}
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("session unusable after the unsupported rejection: %v", err)
	}
	if err := c.Close(ctx); err != nil {
		t.Fatalf("close: %v", err)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	click, err := c.Click(ctx, protocol.ClickRequest{X: 3, Y: 4})
	if err != nil {
		t.Fatal(err)
	}

	if res, err := c.UUID(ctx); err != nil || res.UUID != "asyncfx-uuid" {
		t.Fatalf("uuid = %+v, %v", res, err)
	}
	if status, err := c.Status(ctx); err != nil || status.Running || status.Connected || len(status.Tasks) != 0 {
		t.Fatalf("status = %+v, %v", status, err)
	}
	if accepted, err := c.AppendTask(ctx, protocol.TaskRequest{Type: "Click", Params: jsonRaw(`{"x":9007199254740993}`)}); err != nil || accepted.TaskID != 7 {
		t.Fatalf("append task = %+v, %v", accepted, err)
	}
	if err := c.SetTaskParams(ctx, protocol.TaskParamsRequest{TaskID: 7, Params: jsonRaw(`{"enable":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.BackToHome(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Log(ctx, protocol.LogRequest{Level: "asyncfx", Message: "combination"}); err != nil {
		t.Fatal(err)
	}
	if hit, err := c.MapLevelKey(ctx, protocol.MapLevelKeyRequest{Key: "asyncfx-hit"}); err != nil || hit.StageID != "asyncfx-stage#1" || hit.Name != "asyncfx-name#1" {
		t.Fatalf("map level key = %+v, %v", hit, err)
	}
	if name, err := c.ItemName(ctx, protocol.ItemNameRequest{ID: "asyncfx-item"}); err != nil || name.Name != "asyncfx-item#1" {
		t.Fatalf("item name = %+v, %v", name, err)
	}
	if png, err := c.GetImagePNG(ctx); err != nil || png == nil || len(png) != 0 {
		t.Fatalf("empty png = %d bytes, %v", len(png), err)
	}
	if bgr, err := c.GetImageBGR(ctx); err != nil || !bytes.Equal(bgr, asyncfxBGR()) {
		t.Fatalf("bgr snapshot diverged: %v", err)
	}

	// The clean close settles both pending operations with the stable
	// invalid_state code; the close response follows both operation events and
	// the terminal snapshot. The two operation events race the snapshot's
	// stream position (independent wait goroutines), so the drained set is
	// asserted, not the order.
	if err := c.Close(ctx); err != nil {
		t.Fatalf("close: %v; diagnostics: %s", err, c.Diagnostics())
	}
	results := requireOperationEvents(t, drainEvents(t, c, 3), 2)
	for _, result := range results {
		if result.OK || result.Error == nil || result.Error.Code != protocol.InvalidState {
			t.Fatalf("close settlement = %+v, want ok=false with invalid_state", result)
		}
	}
	for _, op := range []*worker.Operation{screencap, click} {
		_, err := op.Wait(ctx)
		requireProtocolErrorIn(t, err, protocol.InvalidState)
	}
	if snap, ok := c.InstanceDiagnostics(); !ok || snap.FirstFault != nil || snap.StopDiagnostic != nil || snap.LostEvents != 0 {
		t.Fatalf("clean close diagnostics: ok=%v snap=%+v", ok, snap)
	}
	select {
	case <-c.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("process was not reaped after the clean close")
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
	if err != nil {
		t.Fatal(err)
	}

	expired, cancel := context.WithCancel(ctx)
	cancel()
	closeErr := c.Close(expired)
	var failure *worker.Failure
	if !errors.As(closeErr, &failure) || !failure.Unknown {
		t.Fatalf("interrupted close = %v, want a *Failure with unknown=true", closeErr)
	}
	select {
	case <-c.Done():
	case <-time.After(10 * time.Second):
		t.Fatal("killed worker was not reaped within the bound")
	}
	_, err = op.Wait(ctx)
	var interrupted *worker.Failure
	if !errors.As(err, &interrupted) || !interrupted.Unknown {
		t.Fatalf("unconfirmed operation after interruption = %v, want unknown", err)
	}
}
